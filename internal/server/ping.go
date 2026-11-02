package server

import (
	"math"
	"sort"
	"time"

	"github.com/Lightmani/ServerStatus/internal/protocol"
)

// PingBucket aggregates probes to one target over a time range.
type PingBucket struct {
	TS     int64
	Sent   int64
	Recv   int64
	RTTSum float64 // ms
	RTTMin float64
	RTTMax float64
}

// Add folds an agent result into the bucket.
func (b *PingBucket) Add(r *protocol.PingResult) {
	b.Merge(&PingBucket{Sent: int64(r.Sent), Recv: int64(r.Recv), RTTSum: r.RTTSum, RTTMin: r.RTTMin, RTTMax: r.RTTMax})
}

// Merge folds o into b.
func (b *PingBucket) Merge(o *PingBucket) {
	if o.Recv > 0 && (b.Recv == 0 || o.RTTMin < b.RTTMin) {
		b.RTTMin = o.RTTMin
	}
	b.RTTMax = max(b.RTTMax, o.RTTMax)
	b.Sent += o.Sent
	b.Recv += o.Recv
	b.RTTSum += o.RTTSum
}

// Loss returns the loss percentage, or -1 when nothing was sent.
func (b *PingBucket) Loss() float64 {
	if b.Sent == 0 {
		return -1
	}
	return float64(b.Sent-b.Recv) / float64(b.Sent) * 100
}

// AvgRTT returns the mean round-trip time in ms, or -1 without replies.
func (b *PingBucket) AvgRTT() float64 {
	if b.Recv == 0 {
		return -1
	}
	return b.RTTSum / float64(b.Recv)
}

// sanitizePing clamps untrusted agent values.
func sanitizePing(r *protocol.PingResult) bool {
	if r.Sent <= 0 || r.Sent > 100000 || r.Recv < 0 {
		return false
	}
	r.Recv = min(r.Recv, r.Sent)
	fix := func(v float64) float64 {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return 0
		}
		return min(v, 600000)
	}
	r.RTTMin, r.RTTMax = fix(r.RTTMin), fix(r.RTTMax)
	r.RTTSum = min(fix(r.RTTSum), r.RTTMax*float64(r.Recv)+1)
	if r.Recv == 0 {
		r.RTTSum, r.RTTMin, r.RTTMax = 0, 0, 0
	}
	return true
}

// pingTrack is the live state of one (node, target) pair.
type pingTrack struct {
	cur     PingBucket   // current minute, flushed with the node's metrics
	recent  []PingBucket // per-minute buckets of the last pingKeepMinutes
	lastRTT float64      // average of the latest report with replies
}

const pingKeepMinutes = 61

func (t *pingTrack) add(r *protocol.PingResult, minute int64) {
	t.cur.Add(r)
	if n := len(t.recent); n == 0 || t.recent[n-1].TS != minute {
		t.recent = append(t.recent, PingBucket{TS: minute})
		cut := 0
		for cut < len(t.recent) && t.recent[cut].TS <= minute-pingKeepMinutes*60 {
			cut++
		}
		t.recent = t.recent[cut:]
	}
	t.recent[len(t.recent)-1].Add(r)
	if r.Recv > 0 {
		t.lastRTT = r.RTTSum / float64(r.Recv)
	}
}

// window merges recent buckets with ts in [from, to).
func (t *pingTrack) window(from, to int64) PingBucket {
	var b PingBucket
	for i := range t.recent {
		if t.recent[i].TS >= from && t.recent[i].TS < to {
			b.Merge(&t.recent[i])
		}
	}
	return b
}

// allLost reports whether each of the `minutes` complete minutes before
// `minute` had probes and no replies at all. A minute without data (agent
// offline) breaks the streak, so an offline node never raises loss alerts.
func (t *pingTrack) allLost(minute int64, minutes int) bool {
	if minutes <= 0 {
		return false
	}
	byTS := make(map[int64]*PingBucket, len(t.recent))
	for i := range t.recent {
		byTS[t.recent[i].TS] = &t.recent[i]
	}
	for m := minute - int64(minutes)*60; m < minute; m += 60 {
		b := byTS[m]
		if b == nil || b.Sent == 0 || b.Recv > 0 {
			return false
		}
	}
	// replies in the current, still incomplete minute mean it has recovered
	if b := byTS[minute]; b != nil && b.Recv > 0 {
		return false
	}
	return true
}

// PingView is the live status of one target as seen from one node.
type PingView struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Host     string   `json:"host"`
	Latency  *float64 `json:"latency"` // ms, latest report
	Loss     *float64 `json:"loss"`    // %, current + previous minute
	Sent     int64    `json:"sent"`
	Baseline *float64 `json:"baseline"`        // ms, typical latency
	Alert    string   `json:"alert,omitempty"` // "ping_loss" / "ping_degrade" when firing
}

func (t *pingTrack) view(now time.Time) (lat, loss *float64, sent int64) {
	minute := now.Unix() - now.Unix()%60
	w := t.window(minute-60, minute+60)
	if w.Sent == 0 {
		return nil, nil, 0
	}
	l := r2(w.Loss())
	loss = &l
	if w.Recv > 0 {
		v := r2(t.lastRTT)
		lat = &v
	}
	return lat, loss, w.Sent
}

// PingHistory is a columnar per-target time series for charts.
type PingHistory struct {
	Step    int64             `json:"step"`
	T       []int64           `json:"t"`
	Targets []PingHistoryLine `json:"targets"`
}

type PingHistoryLine struct {
	ID   string     `json:"id"`
	Name string     `json:"name"`
	Host string     `json:"host"`
	RTT  []*float64 `json:"rtt"`  // ms; null = no replies
	Loss []*float64 `json:"loss"` // %; null = no data
}

func buildPingHistory(rows []PingRow, targets []PingTargetDef, step int64, loc *time.Location) *PingHistory {
	type key struct {
		target string
		ts     int64
	}
	agg := map[key]*PingBucket{}
	tsSet := map[int64]bool{}
	for i := range rows {
		k := key{rows[i].TargetID, floorLocal(rows[i].TS, step, loc)}
		b := agg[k]
		if b == nil {
			b = &PingBucket{TS: k.ts}
			agg[k] = b
		}
		b.Merge(&rows[i].PingBucket)
		tsSet[k.ts] = true
	}
	h := &PingHistory{Step: step}
	for ts := range tsSet {
		h.T = append(h.T, ts)
	}
	sort.Slice(h.T, func(i, j int) bool { return h.T[i] < h.T[j] })
	for _, t := range targets {
		line := PingHistoryLine{ID: t.ID, Name: t.Name, Host: t.Host,
			RTT: make([]*float64, len(h.T)), Loss: make([]*float64, len(h.T))}
		for i, ts := range h.T {
			b := agg[key{t.ID, ts}]
			if b == nil || b.Sent == 0 {
				continue
			}
			l := r2(b.Loss())
			line.Loss[i] = &l
			if b.Recv > 0 {
				v := r2(b.AvgRTT())
				line.RTT[i] = &v
			}
		}
		h.Targets = append(h.Targets, line)
	}
	return h
}

// windowStats merges the `minutes` complete minutes before `minute`.
// covered is true when every one of them has probe data (the agent was
// online throughout); lastLost reports that the most recent complete minute
// had probes but no replies at all (a possible outage in progress).
func (t *pingTrack) windowStats(minute int64, minutes int) (w PingBucket, covered, lastLost bool) {
	byTS := make(map[int64]*PingBucket, len(t.recent))
	for i := range t.recent {
		byTS[t.recent[i].TS] = &t.recent[i]
	}
	covered = minutes > 0
	for m := minute - int64(minutes)*60; m < minute; m += 60 {
		b := byTS[m]
		if b == nil || b.Sent == 0 {
			covered = false
			continue
		}
		w.Merge(b)
	}
	if b := byTS[minute-60]; b != nil && b.Sent > 0 && b.Recv == 0 {
		lastLost = true
	}
	return w, covered, lastLost
}

func baselineKey(node, target string) string { return node + "\x00" + target }

// computeBaselines returns, per (node, target), the median of hourly average
// RTTs. Hours with more than 50% loss are ignored, and pairs with fewer than
// minHours usable hours get no baseline. The median keeps past incidents
// from inflating the baseline.
func computeBaselines(rows []PingRowNode, minHours int) map[string]float64 {
	samples := map[string][]float64{}
	for _, r := range rows {
		if r.Recv == 0 || r.Recv*2 < r.Sent {
			continue
		}
		k := baselineKey(r.NodeID, r.TargetID)
		samples[k] = append(samples[k], r.RTTSum/float64(r.Recv))
	}
	out := map[string]float64{}
	for k, v := range samples {
		if len(v) < minHours {
			continue
		}
		sort.Float64s(v)
		m := v[len(v)/2]
		if len(v)%2 == 0 {
			m = (v[len(v)/2-1] + v[len(v)/2]) / 2
		}
		out[k] = m
	}
	return out
}

// latencyThreshold returns the alert threshold in ms (0 = not evaluable).
// factor scales the thresholds (1 to fire, <1 for the recovery hysteresis).
func latencyThreshold(d *DegradeConfig, baseline, factor float64) float64 {
	th := 0.0
	if baseline > 0 {
		th = max(baseline*d.LatencyFactor, baseline+d.LatencyMinIncreaseMs)
		th = baseline + (th-baseline)*factor
	}
	if d.LatencyAbsMs > 0 {
		abs := d.LatencyAbsMs * factor
		if th == 0 || abs < th {
			th = abs
		}
	}
	return th
}

// PingRowNode is a stored ping bucket including its node.
type PingRowNode struct {
	NodeID string
	PingRow
}
