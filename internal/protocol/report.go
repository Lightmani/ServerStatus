// Package protocol defines the report an agent sends to the server.
package protocol

import (
	"math"
	"strings"
)

// MaxBodyBytes bounds the size of a report request body.
const MaxBodyBytes = 64 << 10

const maxDisks = 32

type Report struct {
	AgentVersion string     `json:"agent_version"`
	Host         HostInfo   `json:"host"`
	Uptime       uint64     `json:"uptime"` // seconds
	CPU          float64    `json:"cpu"`    // percent, 0-100
	Load         [3]float64 `json:"load"`
	Memory       Usage      `json:"memory"`
	Swap         Usage      `json:"swap"`
	Disk         Usage      `json:"disk"` // sum over Disks
	Disks        []Disk     `json:"disks"`
	Network      Network    `json:"network"`
	TCP          int        `json:"tcp"` // non-listening TCP sockets
	UDP          int        `json:"udp"`
	Processes    int        `json:"processes"`
}

type HostInfo struct {
	Hostname       string `json:"hostname"`
	OS             string `json:"os"`
	Kernel         string `json:"kernel"`
	Arch           string `json:"arch"`
	Virtualization string `json:"virtualization"`
	CPUModel       string `json:"cpu_model"`
	CPUCores       int    `json:"cpu_cores"`
}

// Usage is a total/used pair in bytes.
type Usage struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

type Disk struct {
	Mount  string `json:"mount"`
	FSType string `json:"fstype"`
	Usage
}

type Network struct {
	RxBytes uint64  `json:"rx_bytes"` // counters since boot
	TxBytes uint64  `json:"tx_bytes"`
	RxRate  float64 `json:"rx_rate"` // bytes per second
	TxRate  float64 `json:"tx_rate"`
}

// Sanitize bounds every field so that a misbehaving (or compromised but
// still authenticated) agent cannot bloat server memory or feed nonsense
// into the dashboard.
func (r *Report) Sanitize() {
	r.AgentVersion = clip(r.AgentVersion, 32)
	h := &r.Host
	h.Hostname = clip(h.Hostname, 255)
	h.OS = clip(h.OS, 128)
	h.Kernel = clip(h.Kernel, 128)
	h.Arch = clip(h.Arch, 32)
	h.Virtualization = clip(h.Virtualization, 64)
	h.CPUModel = clip(h.CPUModel, 128)
	h.CPUCores = nonNeg(h.CPUCores)

	r.CPU = clampFloat(r.CPU, 0, 100)
	for i := range r.Load {
		r.Load[i] = clampFloat(r.Load[i], 0, 1e6)
	}
	r.Memory.fix()
	r.Swap.fix()
	r.Disk.fix()
	if len(r.Disks) > maxDisks {
		r.Disks = r.Disks[:maxDisks]
	}
	for i := range r.Disks {
		d := &r.Disks[i]
		d.Mount = clip(d.Mount, 255)
		d.FSType = clip(d.FSType, 32)
		d.Usage.fix()
	}
	r.Network.RxRate = clampFloat(r.Network.RxRate, 0, 1e13)
	r.Network.TxRate = clampFloat(r.Network.TxRate, 0, 1e13)
	r.TCP = nonNeg(r.TCP)
	r.UDP = nonNeg(r.UDP)
	r.Processes = nonNeg(r.Processes)
}

func (u *Usage) fix() {
	if u.Used > u.Total {
		u.Used = u.Total
	}
}

// Percent returns used/total in percent, or 0 when total is 0.
func (u Usage) Percent() float64 {
	if u.Total == 0 {
		return 0
	}
	return float64(u.Used) / float64(u.Total) * 100
}

func clip(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.ToValidUTF8(s, "")
}

func nonNeg(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func clampFloat(f, lo, hi float64) float64 {
	if math.IsNaN(f) || f < lo {
		return lo
	}
	if f > hi {
		return hi
	}
	return f
}
