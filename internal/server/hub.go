package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/Lightmani/ServerStatus/internal/protocol"
)

// liveNode is the in-memory state of one node.
type liveNode struct {
	meta     Node
	host     *protocol.HostInfo
	state    *protocol.State
	lastSeen time.Time

	cur   *Bucket               // current (unflushed) minute
	pings map[string]*pingTrack // target id -> live ping state

	haveCounters bool
	lastRx       uint64
	lastTx       uint64
	lastBoot     int64

	// traffic in the current billing cycle that is already persisted;
	// only touched by the writer goroutine (under Hub.mu).
	cycleStart int64
	cycleRx    uint64
	cycleTx    uint64
}

// Hub owns live node state and funnels finished minute buckets to the store.
type Hub struct {
	cfg   *Config
	store *Store

	mu      sync.RWMutex
	nodes   map[string]*liveNode
	targets []PingTargetDef

	writes chan writeMsg
	wg     sync.WaitGroup
}

// NewHub loads nodes from the store.
func NewHub(cfg *Config, store *Store) (*Hub, error) {
	h := &Hub{cfg: cfg, store: store, nodes: map[string]*liveNode{}, writes: make(chan writeMsg, 1024)}
	if err := h.reload(); err != nil {
		return nil, err
	}
	return h, nil
}

// reload syncs the node set with the database so that CLI edits made while
// the server is running take effect without a restart.
func (h *Hub) reload() error {
	nodes, err := h.store.ListNodes()
	if err != nil {
		return err
	}
	targets, err := h.store.ListPingTargets()
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.targets = targets
	seen := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		seen[n.ID] = true
		ln, ok := h.nodes[n.ID]
		if !ok {
			ln = &liveNode{
				pings:        map[string]*pingTrack{},
				haveCounters: n.LastBoot != 0,
				lastRx:       n.LastRx,
				lastTx:       n.LastTx,
				lastBoot:     n.LastBoot,
			}
			if n.HostJSON != "" {
				var hi protocol.HostInfo
				if json.Unmarshal([]byte(n.HostJSON), &hi) == nil {
					ln.host = &hi
				}
			}
			if n.LastSeen > 0 {
				ln.lastSeen = time.Unix(n.LastSeen, 0)
			}
			h.nodes[n.ID] = ln
		} else if ln.meta.ResetDay != n.ResetDay {
			ln.cycleStart = 0 // force recompute
		}
		ln.meta = n
		// forget state of targets that no longer apply to this node
		for tid := range ln.pings {
			if t := h.targetLocked(tid); t == nil || !t.AppliesTo(n.ID) {
				delete(ln.pings, tid)
			}
		}
	}
	for id := range h.nodes {
		if !seen[id] {
			delete(h.nodes, id)
		}
	}
	return nil
}

func (h *Hub) targetLocked(id string) *PingTargetDef {
	for i := range h.targets {
		if h.targets[i].ID == id {
			return &h.targets[i]
		}
	}
	return nil
}

// Secret returns the HMAC secret of a node.
func (h *Hub) Secret(id string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n, ok := h.nodes[id]
	if !ok {
		return "", false
	}
	return n.meta.Secret, true
}

// PingTargetsFor returns the targets a node should probe.
func (h *Hub) PingTargetsFor(id string) []protocol.PingTarget {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := []protocol.PingTarget{}
	for _, t := range h.targets {
		if t.AppliesTo(id) {
			out = append(out, protocol.PingTarget{ID: t.ID, Name: t.Name, Host: t.Host})
		}
	}
	return out
}

// targetDefsFor returns the target definitions applying to a node.
func (h *Hub) targetDefsFor(id string) []PingTargetDef {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []PingTargetDef
	for _, t := range h.targets {
		if t.AppliesTo(id) {
			out = append(out, t)
		}
	}
	return out
}

// trafficDelta works out how many bytes were transferred since the last
// accounted report, handling reboots (counters restart from zero).
func trafficDelta(have bool, lastBoot int64, lastRx, lastTx uint64, boot int64, rx, tx uint64) (uint64, uint64) {
	if !have {
		return 0, 0
	}
	if d := boot - lastBoot; d > 60 || d < -60 {
		// rebooted: everything counted since boot is new
		return rx, tx
	}
	var drx, dtx uint64
	// A decrease without a reboot (interface re-created, exclude list
	// changed...) is ambiguous; count nothing rather than risk a spike.
	if rx >= lastRx {
		drx = rx - lastRx
	}
	if tx >= lastTx {
		dtx = tx - lastTx
	}
	return drx, dtx
}

// takeFlush packages the current minute (metrics + pings) and resets it.
func (n *liveNode) takeFlush(id string) flushJob {
	j := n.flushJob(id)
	n.cur = nil
	for tid, t := range n.pings {
		if t.cur.Sent > 0 {
			if j.Pings == nil {
				j.Pings = map[string]PingBucket{}
			}
			j.Pings[tid] = t.cur
		}
		t.cur = PingBucket{}
	}
	return j
}

func (n *liveNode) flushJob(id string) flushJob {
	j := flushJob{
		NodeID:   id,
		Minute:   *n.cur,
		LastSeen: n.lastSeen.Unix(),
		LastRx:   n.lastRx,
		LastTx:   n.lastTx,
		LastBoot: n.lastBoot,
	}
	if n.host != nil {
		b, _ := json.Marshal(n.host)
		j.HostJSON = string(b)
	}
	return j
}

var errUnknownNode = errors.New("unknown node")

// Ingest applies an authenticated report.
func (h *Hub) Ingest(id string, rep *protocol.Report, now time.Time) error {
	minute := now.Unix() - now.Unix()%60
	var job []flushJob

	h.mu.Lock()
	n, ok := h.nodes[id]
	if !ok {
		h.mu.Unlock()
		return errUnknownNode
	}
	if n.cur != nil && n.cur.TS != minute {
		// counters persisted with the job match what is inside it
		job = append(job, n.takeFlush(id))
	}
	drx, dtx := trafficDelta(n.haveCounters, n.lastBoot, n.lastRx, n.lastTx,
		rep.Host.BootTime, rep.State.NetRx, rep.State.NetTx)
	if n.cur == nil {
		n.cur = &Bucket{TS: minute}
	}
	st := rep.State
	host := rep.Host
	n.cur.Add(&st, drx, dtx)
	n.state, n.host, n.lastSeen = &st, &host, now
	n.haveCounters, n.lastRx, n.lastTx, n.lastBoot = true, st.NetRx, st.NetTx, host.BootTime
	for i := range rep.Pings {
		r := &rep.Pings[i]
		t := h.targetLocked(r.ID)
		if t == nil || !t.AppliesTo(id) || !sanitizePing(r) {
			continue
		}
		tr := n.pings[r.ID]
		if tr == nil {
			tr = &pingTrack{}
			n.pings[r.ID] = tr
		}
		tr.add(r, minute)
	}
	h.mu.Unlock()

	if job != nil {
		h.writes <- writeMsg{jobs: job}
	}
	return nil
}

// flushStale flushes minute buckets that are complete. all=true flushes everything.
func (h *Hub) flushStale(now time.Time, all bool) []flushJob {
	minute := now.Unix() - now.Unix()%60
	var jobs []flushJob
	h.mu.Lock()
	for id, n := range h.nodes {
		if n.cur != nil && (all || n.cur.TS < minute) {
			jobs = append(jobs, n.takeFlush(id))
		}
	}
	h.mu.Unlock()
	return jobs
}

// writeMsg is processed sequentially by the writer goroutine so that cycle
// traffic refreshes are ordered with respect to metric flushes.
type writeMsg struct {
	jobs    []flushJob
	refresh bool // recompute cycle traffic
	full    bool // ...for all nodes, not only those whose cycle rolled over
}

// Run starts background loops; it returns when ctx is cancelled and all
// pending data has been written.
func (h *Hub) Run(ctx context.Context) {
	h.refreshCycles(time.Now(), true)
	h.cleanup(time.Now())
	h.wg.Add(1)
	go h.writer()

	flushT := time.NewTicker(5 * time.Second)
	reloadT := time.NewTicker(10 * time.Second)
	cycleT := time.NewTicker(30 * time.Second)
	cleanT := time.NewTicker(6 * time.Hour)
	defer flushT.Stop()
	defer reloadT.Stop()
	defer cycleT.Stop()
	defer cleanT.Stop()
	fullRefresh := time.Now()
	for {
		select {
		case <-ctx.Done():
			if jobs := h.flushStale(time.Now(), true); len(jobs) > 0 {
				h.writes <- writeMsg{jobs: jobs}
			}
			close(h.writes)
			h.wg.Wait()
			return
		case now := <-flushT.C:
			if jobs := h.flushStale(now, false); len(jobs) > 0 {
				h.writes <- writeMsg{jobs: jobs}
			}
		case <-reloadT.C:
			if err := h.reload(); err != nil {
				log.Printf("reload nodes: %v", err)
			}
		case now := <-cycleT.C:
			full := now.Sub(fullRefresh) > 10*time.Minute
			if full {
				fullRefresh = now
			}
			h.writes <- writeMsg{refresh: true, full: full}
		case now := <-cleanT.C:
			go h.cleanup(now)
		}
	}
}

func (h *Hub) writer() {
	defer h.wg.Done()
	var pending []flushJob
	for msg := range h.writes {
		pending = append(pending, msg.jobs...)
		if msg.refresh {
			h.save(pending)
			pending = nil
			h.refreshCycles(time.Now(), msg.full)
			continue
		}
		// coalesce whatever else is already queued into one transaction
		if len(h.writes) > 0 && len(pending) < 4096 {
			continue
		}
		h.save(pending)
		pending = nil
	}
	h.save(pending)
}

func (h *Hub) save(jobs []flushJob) {
	if len(jobs) == 0 {
		return
	}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = h.store.SaveFlush(jobs); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		log.Printf("save metrics (%d buckets dropped): %v", len(jobs), err)
		return
	}
	h.mu.Lock()
	for i := range jobs {
		if n, ok := h.nodes[jobs[i].NodeID]; ok && n.cycleStart != 0 && jobs[i].Minute.TS >= n.cycleStart {
			n.cycleRx += jobs[i].Minute.Rx
			n.cycleTx += jobs[i].Minute.Tx
		}
	}
	h.mu.Unlock()
}

// refreshCycles recomputes billing-cycle traffic for nodes whose cycle
// changed (or all nodes when full is set). Must run on the writer goroutine
// (or before it starts).
func (h *Hub) refreshCycles(now time.Time, full bool) {
	type todo struct {
		id    string
		start int64
	}
	var list []todo
	h.mu.RLock()
	for id, n := range h.nodes {
		cs := cycleStart(now.In(h.cfg.Location()), n.meta.ResetDay).Unix()
		if full || cs != n.cycleStart {
			list = append(list, todo{id, cs})
		}
	}
	h.mu.RUnlock()
	for _, t := range list {
		rx, tx, err := h.store.SumTraffic(t.id, t.start)
		if err != nil {
			log.Printf("traffic cycle %s: %v", t.id, err)
			continue
		}
		h.mu.Lock()
		if n, ok := h.nodes[t.id]; ok {
			n.cycleStart, n.cycleRx, n.cycleTx = t.start, rx, tx
		}
		h.mu.Unlock()
	}
}

func (h *Hub) cleanup(now time.Time) {
	minBefore := now.Add(-time.Duration(h.cfg.Retention.MinuteDays) * 24 * time.Hour).Unix()
	var hourBefore int64
	if h.cfg.Retention.HourDays > 0 {
		hourBefore = now.Add(-time.Duration(h.cfg.Retention.HourDays) * 24 * time.Hour).Unix()
	}
	if err := h.store.Cleanup(minBefore, hourBefore); err != nil {
		log.Printf("cleanup: %v", err)
	}
}

// CycleView is the traffic used in the current billing cycle.
type CycleView struct {
	Start    int64  `json:"start"`
	Rx       uint64 `json:"rx"`
	Tx       uint64 `json:"tx"`
	Limit    uint64 `json:"limit"`
	ResetDay int    `json:"reset_day"`
}

// NodeView is the public JSON representation of a node.
type NodeView struct {
	ID       string             `json:"id"`
	Name     string             `json:"name"`
	Group    string             `json:"group"`
	Location string             `json:"location"`
	Sort     int                `json:"sort"`
	Online   bool               `json:"online"`
	LastSeen int64              `json:"last_seen"`
	Host     *protocol.HostInfo `json:"host"`
	State    *protocol.State    `json:"state"`
	Cycle    CycleView          `json:"cycle"`
	Pings    []PingView         `json:"pings"`
}

// Snapshot returns all visible nodes in display order.
func (h *Hub) Snapshot(now time.Time, includeHidden bool) []NodeView {
	h.mu.RLock()
	out := make([]NodeView, 0, len(h.nodes))
	for id, n := range h.nodes {
		if n.meta.Hidden && !includeHidden {
			continue
		}
		v := NodeView{
			ID: id, Name: n.meta.Name, Group: n.meta.Group, Location: n.meta.Location, Sort: n.meta.Sort,
			Host: n.host, State: n.state,
			Online: !n.lastSeen.IsZero() && now.Sub(n.lastSeen) <= h.cfg.OfflineAfter,
			Cycle: CycleView{Start: n.cycleStart, Rx: n.cycleRx, Tx: n.cycleTx,
				Limit: n.meta.TrafficLimit, ResetDay: max(1, n.meta.ResetDay)},
		}
		if !n.lastSeen.IsZero() {
			v.LastSeen = n.lastSeen.Unix()
		}
		if n.cur != nil && n.cur.TS >= n.cycleStart {
			v.Cycle.Rx += n.cur.Rx
			v.Cycle.Tx += n.cur.Tx
		}
		v.Pings = []PingView{}
		for _, t := range h.targets {
			if !t.AppliesTo(id) {
				continue
			}
			pv := PingView{ID: t.ID, Name: t.Name, Host: t.Host}
			if tr := n.pings[t.ID]; tr != nil && v.Online {
				pv.Latency, pv.Loss, pv.Sent = tr.view(now)
			}
			v.Pings = append(v.Pings, pv)
		}
		out = append(out, v)
	}
	h.mu.RUnlock()
	sortViews(out)
	return out
}

// nodeMeta returns a node's metadata if it is visible.
func (h *Hub) nodeMeta(id string, includeHidden bool) (Node, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n, ok := h.nodes[id]
	if !ok || (n.meta.Hidden && !includeHidden) {
		return Node{}, false
	}
	return n.meta, true
}

// visibleMetas returns metadata of all visible nodes.
func (h *Hub) visibleMetas(includeHidden bool) []Node {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Node, 0, len(h.nodes))
	for _, n := range h.nodes {
		if !n.meta.Hidden || includeHidden {
			out = append(out, n.meta)
		}
	}
	return out
}

// alertInput is the per-node data the alerter needs.
type alertInput struct {
	ID, Name, Location string
	Hidden, NoAlert    bool
	LastSeen           time.Time
	Online             bool
	Targets            []targetAlertInput
}

type targetAlertInput struct {
	ID, Name, Host string
	AllLost        bool    // 100% loss in each of the last N minutes
	Recovered      bool    // replies seen in the current or previous minute
	Latency        float64 // ms, latest
}

func (h *Hub) alertInputs(now time.Time, lossMinutes int) []alertInput {
	minute := now.Unix() - now.Unix()%60
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]alertInput, 0, len(h.nodes))
	for id, n := range h.nodes {
		in := alertInput{ID: id, Name: n.meta.Name, Location: n.meta.Location, Hidden: n.meta.Hidden,
			NoAlert: n.meta.NoAlert, LastSeen: n.lastSeen,
			Online: !n.lastSeen.IsZero() && now.Sub(n.lastSeen) <= h.cfg.OfflineAfter}
		for _, t := range h.targets {
			if !t.AppliesTo(id) {
				continue
			}
			ti := targetAlertInput{ID: t.ID, Name: t.Name, Host: t.Host}
			if tr := n.pings[t.ID]; tr != nil {
				ti.AllLost = tr.allLost(minute, lossMinutes)
				w := tr.window(minute-60, minute+60)
				ti.Recovered = w.Recv > 0
				ti.Latency = tr.lastRTT
			}
			in.Targets = append(in.Targets, ti)
		}
		out = append(out, in)
	}
	return out
}
