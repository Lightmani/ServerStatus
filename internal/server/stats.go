package server

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Period kinds supported by the statistics API.
const (
	PeriodDay   = "day"
	PeriodWeek  = "week"
	PeriodMonth = "month"
)

func validPeriod(kind string) bool {
	return kind == PeriodDay || kind == PeriodWeek || kind == PeriodMonth
}

// periodStart returns the start of the day / ISO week (Monday) / month
// containing t, in t's location.
func periodStart(kind string, t time.Time) time.Time {
	y, m, d := t.Date()
	loc := t.Location()
	switch kind {
	case PeriodWeek:
		wd := (int(t.Weekday()) + 6) % 7 // Monday = 0
		return time.Date(y, m, d-wd, 0, 0, 0, 0, loc)
	case PeriodMonth:
		return time.Date(y, m, 1, 0, 0, 0, 0, loc)
	default:
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
}

// periodShift moves a period start by n periods (n may be negative).
func periodShift(kind string, start time.Time, n int) time.Time {
	switch kind {
	case PeriodWeek:
		return start.AddDate(0, 0, 7*n)
	case PeriodMonth:
		return start.AddDate(0, n, 0)
	default:
		return start.AddDate(0, 0, n)
	}
}

func periodLabel(kind string, start time.Time) string {
	switch kind {
	case PeriodWeek:
		y, w := start.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w)
	case PeriodMonth:
		return start.Format("2006-01")
	default:
		return start.Format("2006-01-02")
	}
}

// cycleStart returns the start of the traffic billing cycle containing now.
// resetDay is clamped to the length of each month (31 => last day).
func cycleStart(now time.Time, resetDay int) time.Time {
	if resetDay < 1 {
		resetDay = 1
	}
	y, m, d := now.Date()
	loc := now.Location()
	day := min(resetDay, daysIn(y, m))
	if d >= day {
		return time.Date(y, m, day, 0, 0, 0, 0, loc)
	}
	py, pm := y, m-1
	if pm < 1 {
		py, pm = y-1, 12
	}
	return time.Date(py, pm, min(resetDay, daysIn(py, pm)), 0, 0, 0, 0, loc)
}

func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// PeriodStat is the aggregate of one node over one period.
type PeriodStat struct {
	Start        int64    `json:"start"`
	End          int64    `json:"end"`
	Label        string   `json:"label"`
	Expected     int64    `json:"expected_minutes"`
	Availability *float64 `json:"availability"` // percent, nil when not applicable
	Summary
}

type period struct {
	start, end time.Time
	label      string
}

// buildPeriods returns `count` consecutive periods ending with the one that is
// `offset` periods before the current one, oldest first.
func buildPeriods(kind string, now time.Time, count, offset int) []period {
	cur := periodShift(kind, periodStart(kind, now), -offset)
	out := make([]period, count)
	for i := 0; i < count; i++ {
		s := periodShift(kind, cur, -(count - 1 - i))
		out[i] = period{start: s, end: periodShift(kind, s, 1), label: periodLabel(kind, s)}
	}
	return out
}

// aggregatePeriods folds hour buckets (sorted by ts) of a single node into periods.
func aggregatePeriods(rows []BucketRow, periods []period, createdAt int64, now time.Time) []PeriodStat {
	acc := make([]Bucket, len(periods))
	for _, r := range rows {
		i := sort.Search(len(periods), func(i int) bool { return periods[i].end.Unix() > r.TS })
		if i < len(periods) && r.TS >= periods[i].start.Unix() {
			acc[i].Merge(&r.Bucket)
		}
	}
	out := make([]PeriodStat, len(periods))
	for i, p := range periods {
		ps := PeriodStat{Start: p.start.Unix(), End: p.end.Unix(), Label: p.label, Summary: acc[i].Summary()}
		from := max(p.start.Unix(), createdAt)
		to := min(p.end.Unix(), now.Unix())
		if to > from {
			ps.Expected = (to - from) / 60
			if ps.Expected > 0 {
				a := math.Min(100, float64(acc[i].Minutes)/float64(ps.Expected)*100)
				a = math.Round(a*100) / 100
				ps.Availability = &a
			}
		}
		out[i] = ps
	}
	return out
}

// floorLocal aligns ts down to a multiple of step in loc (days align to local midnight).
func floorLocal(ts, step int64, loc *time.Location) int64 {
	t := time.Unix(ts, 0).In(loc)
	if step >= 86400 && step%86400 == 0 {
		y, m, d := t.Date()
		mid := time.Date(y, m, d, 0, 0, 0, 0, loc)
		days := int64(step / 86400)
		if days > 1 {
			// align multi-day steps on a fixed epoch day
			n := int64(mid.Sub(time.Date(1970, 1, 1, 0, 0, 0, 0, loc)).Hours() / 24)
			mid = mid.AddDate(0, 0, -int(n%days))
		}
		return mid.Unix()
	}
	_, off := t.Zone()
	o := int64(off)
	return (ts+o)/step*step - o
}

// downsample merges sorted buckets into step-wide buckets.
func downsample(rows []BucketRow, step int64, loc *time.Location) []Bucket {
	var out []Bucket
	for i := range rows {
		k := floorLocal(rows[i].TS, step, loc)
		if len(out) == 0 || out[len(out)-1].TS != k {
			out = append(out, Bucket{TS: k})
		}
		b := &out[len(out)-1]
		r := rows[i].Bucket
		r.TS = k
		b.Merge(&r)
	}
	return out
}

// historyRange maps a UI range to (duration, source resolution, output step).
func historyRange(r string) (dur time.Duration, res, step int64, ok bool) {
	switch r {
	case "1h":
		return time.Hour, ResMinute, 60, true
	case "6h":
		return 6 * time.Hour, ResMinute, 60, true
	case "24h":
		return 24 * time.Hour, ResMinute, 300, true
	case "7d":
		return 7 * 24 * time.Hour, ResHour, 3600, true
	case "30d":
		return 30 * 24 * time.Hour, ResHour, 4 * 3600, true
	case "90d":
		return 90 * 24 * time.Hour, ResHour, 12 * 3600, true
	case "1y":
		return 365 * 24 * time.Hour, ResHour, 86400, true
	}
	return 0, 0, 0, false
}

// History is a columnar time series for charts.
type History struct {
	Step   int64     `json:"step"`
	T      []int64   `json:"t"`
	CPU    []float64 `json:"cpu"`
	CPUMax []float64 `json:"cpu_max"`
	Mem    []float64 `json:"mem"`
	Swap   []float64 `json:"swap"`
	Disk   []float64 `json:"disk"`
	Load   []float64 `json:"load"`
	Rx     []float64 `json:"rx"`
	Tx     []float64 `json:"tx"`
	RxMax  []uint64  `json:"rx_max"`
	TxMax  []uint64  `json:"tx_max"`
	TCP    []float64 `json:"tcp"`
	Procs  []float64 `json:"procs"`
}

func buildHistory(buckets []Bucket, step int64) *History {
	h := &History{Step: step}
	for i := range buckets {
		b := &buckets[i]
		s := b.Summary()
		h.T = append(h.T, b.TS)
		h.CPU = append(h.CPU, r2(s.CPUAvg))
		h.CPUMax = append(h.CPUMax, r2(s.CPUMax))
		h.Mem = append(h.Mem, r2(s.MemPct))
		h.Swap = append(h.Swap, r2(s.SwapAvg))
		h.Disk = append(h.Disk, r2(s.DiskPct))
		h.Load = append(h.Load, r2(s.LoadAvg))
		h.Rx = append(h.Rx, math.Round(s.RxAvgRate))
		h.Tx = append(h.Tx, math.Round(s.TxAvgRate))
		h.RxMax = append(h.RxMax, s.RxRateMax)
		h.TxMax = append(h.TxMax, s.TxRateMax)
		h.TCP = append(h.TCP, r2(s.TCPAvg))
		h.Procs = append(h.Procs, r2(s.ProcAvg))
	}
	return h
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }
