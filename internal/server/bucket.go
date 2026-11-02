package server

import "github.com/Lightmani/ServerStatus/internal/protocol"

// Bucket aggregates samples over a time range. Averages are stored as sums so
// buckets can be merged losslessly (minute -> hour -> day/week/month).
type Bucket struct {
	TS        int64 // bucket start, unix seconds
	Samples   int64
	Minutes   int64 // number of distinct minutes that contained data
	CPUSum    float64
	CPUMax    float64
	MemSum    float64
	MemMax    uint64
	MemTotal  uint64
	SwapSum   float64
	DiskSum   float64
	DiskTotal uint64
	LoadSum   float64
	LoadMax   float64
	Rx        uint64 // bytes received in this bucket
	Tx        uint64 // bytes sent in this bucket
	RxRateMax uint64
	TxRateMax uint64
	TCPSum    float64
	ProcSum   float64
}

// Add folds one report sample (with its traffic delta) into the bucket.
func (b *Bucket) Add(s *protocol.State, drx, dtx uint64) {
	b.Samples++
	b.CPUSum += s.CPU
	b.CPUMax = max(b.CPUMax, s.CPU)
	b.MemSum += float64(s.MemUsed)
	b.MemMax = max(b.MemMax, s.MemUsed)
	b.MemTotal = s.MemTotal
	b.SwapSum += float64(s.SwapUsed)
	b.DiskSum += float64(s.DiskUsed)
	b.DiskTotal = s.DiskTotal
	b.LoadSum += s.Load1
	b.LoadMax = max(b.LoadMax, s.Load1)
	b.Rx += drx
	b.Tx += dtx
	b.RxRateMax = max(b.RxRateMax, s.RxRate)
	b.TxRateMax = max(b.TxRateMax, s.TxRate)
	b.TCPSum += float64(s.TCP)
	b.ProcSum += float64(s.Procs)
}

// Merge folds o into b. Totals take the most recent non-zero value.
func (b *Bucket) Merge(o *Bucket) {
	b.Samples += o.Samples
	b.Minutes += o.Minutes
	b.CPUSum += o.CPUSum
	b.CPUMax = max(b.CPUMax, o.CPUMax)
	b.MemSum += o.MemSum
	b.MemMax = max(b.MemMax, o.MemMax)
	b.SwapSum += o.SwapSum
	b.DiskSum += o.DiskSum
	b.LoadSum += o.LoadSum
	b.LoadMax = max(b.LoadMax, o.LoadMax)
	b.Rx += o.Rx
	b.Tx += o.Tx
	b.RxRateMax = max(b.RxRateMax, o.RxRateMax)
	b.TxRateMax = max(b.TxRateMax, o.TxRateMax)
	b.TCPSum += o.TCPSum
	b.ProcSum += o.ProcSum
	if o.MemTotal > 0 && (o.TS >= b.TS || b.MemTotal == 0) {
		b.MemTotal = o.MemTotal
	}
	if o.DiskTotal > 0 && (o.TS >= b.TS || b.DiskTotal == 0) {
		b.DiskTotal = o.DiskTotal
	}
}

func (b *Bucket) avg(sum float64) float64 {
	if b.Samples == 0 {
		return 0
	}
	return sum / float64(b.Samples)
}

func pct(used, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return used / total * 100
}

// Summary is the JSON-friendly view of an aggregated bucket.
type Summary struct {
	CPUAvg    float64 `json:"cpu_avg"`
	CPUMax    float64 `json:"cpu_max"`
	MemAvg    float64 `json:"mem_avg"` // bytes
	MemMax    uint64  `json:"mem_max"`
	MemTotal  uint64  `json:"mem_total"`
	MemPct    float64 `json:"mem_pct"`
	SwapAvg   float64 `json:"swap_avg"`
	DiskPct   float64 `json:"disk_pct"`
	DiskTotal uint64  `json:"disk_total"`
	LoadAvg   float64 `json:"load_avg"`
	LoadMax   float64 `json:"load_max"`
	Rx        uint64  `json:"rx"`
	Tx        uint64  `json:"tx"`
	RxAvgRate float64 `json:"rx_avg_rate"` // bytes/s while online
	TxAvgRate float64 `json:"tx_avg_rate"`
	RxRateMax uint64  `json:"rx_rate_max"`
	TxRateMax uint64  `json:"tx_rate_max"`
	TCPAvg    float64 `json:"tcp_avg"`
	ProcAvg   float64 `json:"proc_avg"`
	Samples   int64   `json:"samples"`
	Minutes   int64   `json:"online_minutes"`
}

func (b *Bucket) Summary() Summary {
	s := Summary{
		CPUAvg:    b.avg(b.CPUSum),
		CPUMax:    b.CPUMax,
		MemAvg:    b.avg(b.MemSum),
		MemMax:    b.MemMax,
		MemTotal:  b.MemTotal,
		SwapAvg:   b.avg(b.SwapSum),
		DiskTotal: b.DiskTotal,
		LoadAvg:   b.avg(b.LoadSum),
		LoadMax:   b.LoadMax,
		Rx:        b.Rx,
		Tx:        b.Tx,
		RxRateMax: b.RxRateMax,
		TxRateMax: b.TxRateMax,
		TCPAvg:    b.avg(b.TCPSum),
		ProcAvg:   b.avg(b.ProcSum),
		Samples:   b.Samples,
		Minutes:   b.Minutes,
	}
	s.MemPct = pct(s.MemAvg, float64(b.MemTotal))
	s.DiskPct = pct(b.avg(b.DiskSum), float64(b.DiskTotal))
	if b.Minutes > 0 {
		s.RxAvgRate = float64(b.Rx) / float64(b.Minutes*60)
		s.TxAvgRate = float64(b.Tx) / float64(b.Minutes*60)
	}
	return s
}
