//go:build linux

// Package collector gathers host metrics from /proc and /sys.
package collector

import (
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/lightmani/serverstatus/internal/protocol"
)

// Collector keeps the previous CPU and network counters so it can report
// rates. It is not safe for concurrent use.
type Collector struct {
	host    protocol.HostInfo
	prevCPU cpuTimes
	prevNet netCounters
	prevAt  time.Time
}

// New primes the counters; the first Collect after New reports rates over
// the interval since New.
func New() *Collector {
	c := &Collector{host: hostInfo()}
	c.prevCPU, _ = parseCPUStat(read("/proc/stat"))
	c.prevNet = parseNetDev(read("/proc/net/dev"))
	c.prevAt = time.Now()
	return c
}

func (c *Collector) Collect() (*protocol.Report, error) {
	now := time.Now()
	r := &protocol.Report{Host: c.host}
	r.Host.Hostname, _ = os.Hostname()

	if cur, err := parseCPUStat(read("/proc/stat")); err == nil {
		r.CPU = cpuUsage(c.prevCPU, cur)
		c.prevCPU = cur
	}

	mem := parseMeminfo(read("/proc/meminfo"))
	r.Memory.Total, r.Memory.Used = memUsage(mem)
	r.Swap.Total = mem["SwapTotal"]
	if free := mem["SwapFree"]; free <= r.Swap.Total {
		r.Swap.Used = r.Swap.Total - free
	}

	r.Load = parseLoadavg(read("/proc/loadavg"))
	r.Uptime = parseUptime(read("/proc/uptime"))

	net := parseNetDev(read("/proc/net/dev"))
	secs := now.Sub(c.prevAt).Seconds()
	r.Network = protocol.Network{
		RxBytes: net.rx,
		TxBytes: net.tx,
		RxRate:  rate(c.prevNet.rx, net.rx, secs),
		TxRate:  rate(c.prevNet.tx, net.tx, secs),
	}
	c.prevNet, c.prevAt = net, now

	for _, m := range parseMounts(read("/proc/mounts")) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(m.point, &st); err != nil || st.Blocks == 0 {
			continue
		}
		bs := uint64(st.Bsize)
		d := protocol.Disk{Mount: m.point, FSType: m.fstype}
		d.Total = st.Blocks * bs
		d.Used = (st.Blocks - st.Bfree) * bs
		r.Disks = append(r.Disks, d)
		r.Disk.Total += d.Total
		r.Disk.Used += d.Used
	}

	r.TCP = countSockets(read("/proc/net/tcp"), true) + countSockets(read("/proc/net/tcp6"), true)
	r.UDP = countSockets(read("/proc/net/udp"), false) + countSockets(read("/proc/net/udp6"), false)
	r.Processes = countProcesses()
	return r, nil
}

func hostInfo() protocol.HostInfo {
	osName := parseOSRelease(read("/etc/os-release"))
	if osName == "" {
		osName = parseOSRelease(read("/usr/lib/os-release"))
	}
	return protocol.HostInfo{
		OS:             osName,
		Kernel:         strings.TrimSpace(read("/proc/sys/kernel/osrelease")),
		Arch:           runtime.GOARCH,
		Virtualization: virtualization(),
		CPUModel:       parseCPUModel(read("/proc/cpuinfo")),
		CPUCores:       runtime.NumCPU(),
	}
}

func virtualization() string {
	// Containers first: they see the host's DMI data.
	if exists("/proc/vz") && !exists("/proc/bc") {
		return "OpenVZ"
	}
	if c := strings.TrimSpace(read("/run/systemd/container")); c != "" {
		return strings.ToUpper(c[:1]) + c[1:]
	}
	if exists("/.dockerenv") {
		return "Docker"
	}
	if exists("/run/.containerenv") {
		return "Podman"
	}
	return detectVirt(
		read("/sys/class/dmi/id/sys_vendor"),
		read("/sys/class/dmi/id/product_name"),
	)
}

func countProcesses() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() && isDigits(e.Name()) {
			n++
		}
	}
	return n
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
