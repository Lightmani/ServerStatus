package server

import (
	"context"
	"fmt"
	"html"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// Alert kinds.
const (
	AlertOffline     = "offline"
	AlertPingLoss    = "ping_loss"
	AlertPingDegrade = "ping_degrade"
)

// Alert is a currently firing alert.
type Alert struct {
	Key        string `json:"key"`
	Kind       string `json:"kind"`
	NodeID     string `json:"node_id"`
	NodeName   string `json:"node_name"`
	TargetID   string `json:"target_id,omitempty"`
	TargetName string `json:"target_name,omitempty"`
	Since      int64  `json:"since"`
	Detail     string `json:"detail,omitempty"` // live description, not persisted
	hidden     bool
}

func offlineKey(node string) string         { return "offline:" + node }
func pingKey(node, target string) string    { return "ping:" + node + ":" + target }
func degradeKey(node, target string) string { return "degrade:" + node + ":" + target }

// Alerter evaluates alert rules against live hub state. Firing alerts are
// persisted so a restart neither forgets an outage nor re-sends it.
type Alerter struct {
	cfg       *Config
	hub       *Hub
	store     *Store
	notifiers []Notifier
	started   time.Time

	mu     sync.RWMutex
	active map[string]*Alert

	// when each outage (ping_loss) alert last resolved: the degradation rule
	// stays quiet for one window afterwards, because that window still
	// contains the outage minutes and would otherwise raise a false alarm
	outageEnded  map[string]time.Time
	lastBaseline time.Time
}

// NewAlerter loads persisted alert state.
func NewAlerter(cfg *Config, hub *Hub, store *Store, notifiers ...Notifier) (*Alerter, error) {
	a := &Alerter{cfg: cfg, hub: hub, store: store, notifiers: notifiers, started: time.Now(),
		active: map[string]*Alert{}, outageEnded: map[string]time.Time{}}
	saved, err := store.LoadAlerts()
	if err != nil {
		return nil, err
	}
	for i := range saved {
		al := saved[i]
		a.active[al.Key] = &al
	}
	return a, nil
}

// Run evaluates rules periodically until ctx is cancelled.
func (a *Alerter) Run(ctx context.Context) {
	// Safe right after start: offline alerts honour a grace period from
	// server start and ping alerts need fresh data, so this only refreshes
	// names/visibility of persisted alerts.
	a.Evaluate(time.Now())
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			a.Evaluate(now)
		}
	}
}

// Active returns firing alerts, oldest first.
func (a *Alerter) Active(includeHidden bool) []Alert {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Alert, 0, len(a.active))
	for _, al := range a.active {
		if al.hidden && !includeHidden {
			continue
		}
		out = append(out, *al)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since != out[j].Since {
			return out[i].Since < out[j].Since
		}
		return out[i].Key < out[j].Key
	})
	return out
}

type decision int

const (
	keepIfActive decision = iota // condition undecided: keep current state
	fire
	resolve
)

type evalResult struct {
	d     decision
	alert Alert
	msg   func(firing bool, al *Alert) string
	// degradation alerts are subordinate to the outage alert of the same
	// node/target: suppressed (and silently cleared) while it is active
	outageKey string
}

// degradeEval holds the numbers behind a degradation decision.
type degradeEval struct {
	Loss, Avg, Baseline, Threshold float64
	LossBad, LatBad                bool
}

// evalDegrade decides the degradation state of one node/target pair.
// Fires above the thresholds, resolves only below relaxed ones (loss under
// half the threshold, latency under 80% of the way to the threshold) so a
// line hovering around a limit does not flap.
func evalDegrade(d *DegradeConfig, t *targetAlertInput) (decision, degradeEval) {
	ev := degradeEval{Loss: t.Win.Loss(), Avg: t.Win.AvgRTT(), Baseline: t.Baseline,
		Threshold: latencyThreshold(d, t.Baseline, 1)}
	if !t.Covered || t.Win.Sent == 0 || t.AllLost || t.LastLost {
		// incomplete data, or a full outage in progress (the outage rule's job)
		return keepIfActive, ev
	}
	ev.LossBad = ev.Loss > d.LossPercent
	ev.LatBad = ev.Threshold > 0 && ev.Avg > ev.Threshold
	if ev.LossBad || ev.LatBad {
		return fire, ev
	}
	lossOK := ev.Loss < d.LossPercent/2
	relaxed := latencyThreshold(d, t.Baseline, 0.8)
	latOK := relaxed == 0 || ev.Avg < 0 || ev.Avg <= relaxed
	if lossOK && latOK {
		return resolve, ev
	}
	return keepIfActive, ev
}

func (ev degradeEval) detail() string {
	s := fmt.Sprintf("丢包 %.1f%%", ev.Loss)
	if ev.Avg >= 0 {
		s += fmt.Sprintf(" · 延迟 %.1f ms", ev.Avg)
		if ev.Baseline > 0 {
			s += fmt.Sprintf("（基线 %.1f ms）", ev.Baseline)
		}
	}
	return s
}

// Evaluate runs all rules once.
func (a *Alerter) Evaluate(now time.Time) {
	if !a.cfg.Alert.Enabled {
		a.mu.Lock()
		for k := range a.active {
			a.drop(k)
		}
		a.mu.Unlock()
		return
	}
	deg := &a.cfg.Alert.Degrade
	if now.Sub(a.lastBaseline) >= 30*time.Minute {
		a.refreshBaselines(now)
	}
	results := map[string]evalResult{}
	for _, n := range a.hub.alertInputs(now, a.cfg.Alert.PingLossMinutes, deg.WindowMinutes) {
		if n.NoAlert {
			continue // existing alerts of muted nodes are dropped below
		}
		n := n
		// --- offline rule ---------------------------------------------
		ok := offlineKey(n.ID)
		r := evalResult{alert: Alert{Key: ok, Kind: AlertOffline, NodeID: n.ID, NodeName: n.Name, hidden: n.Hidden}}
		switch {
		case n.LastSeen.IsZero():
			// never reported: nothing to alert about
		case n.Online:
			r.d = resolve
		case now.Sub(laterOf(n.LastSeen, a.started)) >= a.cfg.Alert.OfflineAfter:
			// measured from server start too, so a server restart after
			// downtime does not raise alerts before agents can reconnect
			r.d = fire
			r.alert.Since = n.LastSeen.Unix()
		}
		r.msg = func(firing bool, al *Alert) string { return a.offlineMessage(n, al, firing, now) }
		results[ok] = r

		// --- ping loss rule -------------------------------------------
		for _, t := range n.Targets {
			t := t
			pk := pingKey(n.ID, t.ID)
			pr := evalResult{alert: Alert{Key: pk, Kind: AlertPingLoss, NodeID: n.ID, NodeName: n.Name,
				TargetID: t.ID, TargetName: t.Name, hidden: n.Hidden}}
			switch {
			case t.AllLost:
				pr.d = fire
				pr.alert.Since = now.Unix() - now.Unix()%60 - int64(a.cfg.Alert.PingLossMinutes)*60
			case t.Recovered:
				pr.d = resolve
			}
			pr.msg = func(firing bool, al *Alert) string { return a.pingMessage(n, t, al, firing, now) }
			results[pk] = pr

			// --- degradation rule ---------------------------------------
			if !deg.Enabled {
				continue
			}
			d, ev := evalDegrade(deg, &t)
			dk := degradeKey(n.ID, t.ID)
			dr := evalResult{d: d, outageKey: pk, alert: Alert{Key: dk, Kind: AlertPingDegrade, NodeID: n.ID, NodeName: n.Name,
				TargetID: t.ID, TargetName: t.Name, hidden: n.Hidden, Detail: ev.detail(),
				Since: now.Unix() - now.Unix()%60 - int64(deg.WindowMinutes)*60}}
			dr.msg = func(firing bool, al *Alert) string { return a.degradeMessage(n, t, ev, al, firing, now) }
			results[dk] = dr
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	// outage/offline rules first, so degradation can defer to their outcome
	for key, r := range results {
		if r.outageKey == "" {
			a.apply(key, r, now)
		}
	}
	window := time.Duration(deg.WindowMinutes) * time.Minute
	for key, r := range results {
		if r.outageKey == "" {
			continue
		}
		if _, outage := a.active[r.outageKey]; outage {
			if _, active := a.active[key]; active {
				a.drop(key) // superseded by the outage alert
			}
			continue
		}
		if r.d == fire && now.Sub(a.outageEnded[r.outageKey]) < window {
			r.d = keepIfActive // window still contains the outage minutes
		}
		a.apply(key, r, now)
	}
	// node or target removed, or alerts muted: clear silently
	for key := range a.active {
		if _, ok := results[key]; !ok {
			a.drop(key)
		}
	}
	for key, t := range a.outageEnded {
		if now.Sub(t) > window {
			delete(a.outageEnded, key)
		}
	}
}

// apply performs the state transition for one alert. Caller holds a.mu.
func (a *Alerter) apply(key string, r evalResult, now time.Time) {
	cur, active := a.active[key]
	switch {
	case r.d == fire && !active:
		al := r.alert
		a.active[key] = &al
		if err := a.store.SaveAlert(al); err != nil {
			log.Printf("alert: save %s: %v", key, err)
		}
		a.send(r.msg(true, &al))
	case r.d == resolve && active:
		delete(a.active, key)
		if cur.Kind == AlertPingLoss {
			a.outageEnded[key] = now
		}
		if err := a.store.DeleteAlert(key); err != nil {
			log.Printf("alert: delete %s: %v", key, err)
		}
		if a.cfg.Alert.NotifyRecovery {
			a.send(r.msg(false, cur))
		}
	case active:
		// refresh display names / visibility / live detail of a kept alert
		cur.NodeName, cur.TargetName, cur.hidden = r.alert.NodeName, r.alert.TargetName, r.alert.hidden
		if r.alert.Detail != "" {
			cur.Detail = r.alert.Detail
		}
	}
}

// refreshBaselines recomputes per node/target latency baselines from the
// hourly history.
func (a *Alerter) refreshBaselines(now time.Time) {
	a.lastBaseline = now
	d := &a.cfg.Alert.Degrade
	rows, err := a.store.QueryPingAll(ResHour, now.Add(-time.Duration(d.BaselineDays)*24*time.Hour).Unix(), now.Unix()+1)
	if err != nil {
		log.Printf("alert: baselines: %v", err)
		return
	}
	a.hub.setBaselines(computeBaselines(rows, d.BaselineMinHours))
}

func (a *Alerter) degradeMessage(n alertInput, t targetAlertInput, ev degradeEval, al *Alert, firing bool, now time.Time) string {
	target := "<b>" + html.EscapeString(t.Name) + "</b> (" + html.EscapeString(t.Host) + ")"
	d := &a.cfg.Alert.Degrade
	if !firing {
		return fmt.Sprintf("✅ <b>[恢复]</b> %s → %s\n线路质量恢复：%s，持续约 %s",
			nodeLabel(n), target, ev.detail(), humanDur(now.Sub(time.Unix(al.Since, 0))))
	}
	var why []string
	if ev.LossBad {
		why = append(why, fmt.Sprintf("丢包率 %.1f%% &gt; %g%%", ev.Loss, d.LossPercent))
	}
	if ev.LatBad {
		s := fmt.Sprintf("平均延迟 %.1f ms &gt; %.1f ms", ev.Avg, ev.Threshold)
		if ev.Baseline > 0 {
			s += fmt.Sprintf("（基线 %.1f ms）", ev.Baseline)
		}
		why = append(why, s)
	}
	return fmt.Sprintf("🟠 <b>[劣化]</b> %s → %s\n过去 %d 分钟：%s\n当前：%s",
		nodeLabel(n), target, d.WindowMinutes, strings.Join(why, "；"), ev.detail())
}

// drop removes an alert without notification. Caller holds a.mu.
func (a *Alerter) drop(key string) {
	delete(a.active, key)
	if err := a.store.DeleteAlert(key); err != nil {
		log.Printf("alert: delete %s: %v", key, err)
	}
}

func (a *Alerter) send(msg string) {
	log.Printf("alert: %s", strings.ReplaceAll(stripTags(msg), "\n", " | "))
	for _, n := range a.notifiers {
		n.Notify(msg)
	}
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (a *Alerter) fmtTime(t time.Time) string {
	return t.In(a.cfg.Location()).Format("2006-01-02 15:04:05")
}

func nodeLabel(n alertInput) string {
	s := "<b>" + html.EscapeString(n.Name) + "</b>"
	if n.Location != "" {
		s += " (" + html.EscapeString(n.Location) + ")"
	}
	return s
}

func (a *Alerter) offlineMessage(n alertInput, al *Alert, firing bool, now time.Time) string {
	if firing {
		return fmt.Sprintf("🔴 <b>[离线]</b> %s\n已失联 %s\n最后上报：%s",
			nodeLabel(n), humanDur(now.Sub(n.LastSeen)), a.fmtTime(n.LastSeen))
	}
	return fmt.Sprintf("✅ <b>[恢复]</b> %s\n已恢复上报，离线约 %s",
		nodeLabel(n), humanDur(n.LastSeen.Sub(time.Unix(al.Since, 0))))
}

func (a *Alerter) pingMessage(n alertInput, t targetAlertInput, al *Alert, firing bool, now time.Time) string {
	target := "<b>" + html.EscapeString(t.Name) + "</b> (" + html.EscapeString(t.Host) + ")"
	if firing {
		return fmt.Sprintf("🔴 <b>[丢包]</b> %s → %s\n过去 %d 分钟丢包率 100%%",
			nodeLabel(n), target, a.cfg.Alert.PingLossMinutes)
	}
	return fmt.Sprintf("✅ <b>[恢复]</b> %s → %s\n已恢复连通，当前延迟 %.1f ms，中断约 %s",
		nodeLabel(n), target, t.Latency, humanDur(now.Sub(time.Unix(al.Since, 0))))
}

func humanDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%d 天 %d 小时", days, h)
	case h > 0:
		return fmt.Sprintf("%d 小时 %d 分", h, m)
	case m > 0:
		return fmt.Sprintf("%d 分 %d 秒", m, s)
	default:
		return fmt.Sprintf("%d 秒", s)
	}
}

func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return html.UnescapeString(b.String())
}
