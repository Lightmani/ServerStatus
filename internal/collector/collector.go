// Package collector gathers Linux system metrics directly from /proc and
// /sys without any external dependencies.
package collector

import (
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Lightmani/ServerStatus/internal/protocol"
)

// DefaultNICExclude skips loopback and common virtual interfaces so that
// container traffic is not double counted.
const DefaultNICExclude = `^(lo|docker\d*|veth.*|br-.*|virbr.*|vnet\d+|cni.*|flannel.*|cali.*|kube-.*|lxcbr.*|lxc.*|ifb.*|dummy.*)$`

// Collector keeps the previous sample needed to compute rates.
type Collector struct {
	Version    string
	nicExclude *regexp.Regexp

	mu       sync.Mutex
	prevCPU  cpuTimes
	prevRx   uint64
	prevTx   uint64
	prevTime time.Time
	static   protocol.HostInfo
}

// New creates a collector. nicExclude may be empty to use the default.
func New(version, nicExclude string) (*Collector, error) {
	if nicExclude == "" {
		nicExclude = DefaultNICExclude
	}
	re, err := regexp.Compile(nicExclude)
	if err != nil {
		return nil, err
	}
	c := &Collector{Version: version, nicExclude: re}
	c.static = c.collectStatic()
	// prime the rate counters so the first report has meaningful values
	stat, _ := os.ReadFile("/proc/stat")
	c.prevCPU, _ = parseProcStat(string(stat))
	nd, _ := os.ReadFile("/proc/net/dev")
	c.prevRx, c.prevTx = parseNetDev(string(nd), c.nicExclude)
	c.prevTime = time.Now()
	return c, nil
}

func readStr(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func (c *Collector) collectStatic() protocol.HostInfo {
	h := protocol.HostInfo{
		Arch:         runtime.GOARCH,
		CPUCores:     runtime.NumCPU(),
		AgentVersion: c.Version,
	}
	h.Hostname, _ = os.Hostname()
	h.OS = parseOSRelease(readStr("/etc/os-release"))
	if h.OS == "" {
		h.OS = parseOSRelease(readStr("/usr/lib/os-release"))
	}
	h.Kernel = strings.TrimSpace(readStr("/proc/sys/kernel/osrelease"))
	h.CPUModel = parseCPUInfo(readStr("/proc/cpuinfo"))
	var uts syscall.Utsname
	if syscall.Uname(&uts) == nil {
		h.Arch = utsString(uts.Machine[:])
	}
	h.Virtualization = detectVirt()
	return h
}

func utsString[T int8 | uint8](b []T) string {
	var sb strings.Builder
	for _, c := range b {
		if c == 0 {
			break
		}
		sb.WriteByte(byte(c))
	}
	return sb.String()
}

func detectVirt() string {
	if p, err := exec.LookPath("systemd-detect-virt"); err == nil {
		out, _ := exec.Command(p).Output()
		v := strings.TrimSpace(string(out))
		if v != "" && v != "none" {
			return v
		}
		if v == "none" {
			return "dedicated"
		}
	}
	if _, err := os.Stat("/proc/vz"); err == nil {
		if _, err := os.Stat("/proc/bc"); err != nil {
			return "openvz"
		}
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "docker"
	}
	prod := strings.ToLower(readStr("/sys/class/dmi/id/product_name") + readStr("/sys/class/dmi/id/sys_vendor"))
	for _, k := range []string{"kvm", "qemu", "vmware", "virtualbox", "xen", "hyper-v", "microsoft", "bochs"} {
		if strings.Contains(prod, k) {
			return k
		}
	}
	return ""
}

// Collect returns a full report for this instant.
func (c *Collector) Collect() protocol.Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	host := c.static
	var st protocol.State

	cur, btime := parseProcStat(readStr("/proc/stat"))
	st.CPU, st.CPUSteal, st.CPUIOWait = cpuPercent(c.prevCPU, cur)
	c.prevCPU = cur
	host.BootTime = btime

	st.Load1, st.Load5, st.Load15, st.Threads = parseLoadavg(readStr("/proc/loadavg"))

	m := parseMeminfo(readStr("/proc/meminfo"))
	st.MemTotal, st.MemUsed, st.SwapTotal, st.SwapUsed = m.Total, m.Used, m.SwapTotal, m.SwapUsed

	for _, mnt := range parseMounts(readStr("/proc/mounts")) {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(mnt.Target, &fs); err != nil || fs.Blocks == 0 {
			continue
		}
		bs := uint64(fs.Bsize)
		st.DiskTotal += fs.Blocks * bs
		st.DiskUsed += (fs.Blocks - fs.Bfree) * bs
	}

	st.NetRx, st.NetTx = parseNetDev(readStr("/proc/net/dev"), c.nicExclude)
	if dt := now.Sub(c.prevTime).Seconds(); dt > 0.2 {
		st.RxRate = uint64(float64(sub(st.NetRx, c.prevRx)) / dt)
		st.TxRate = uint64(float64(sub(st.NetTx, c.prevTx)) / dt)
	}
	c.prevRx, c.prevTx, c.prevTime = st.NetRx, st.NetTx, now

	t4, u4 := parseSockstat(readStr("/proc/net/sockstat"))
	t6, u6 := parseSockstat(readStr("/proc/net/sockstat6"))
	st.TCP, st.UDP = t4+t6, u4+u6

	st.Procs = countProcs()
	if f := strings.Fields(readStr("/proc/uptime")); len(f) > 0 {
		up, _ := strconv.ParseFloat(f[0], 64)
		st.Uptime = uint64(up)
	}
	return protocol.Report{Time: now.Unix(), Host: host, State: st}
}

func countProcs() int {
	d, err := os.Open("/proc")
	if err != nil {
		return 0
	}
	defer d.Close()
	names, _ := d.Readdirnames(-1)
	n := 0
	for _, name := range names {
		if name != "" && name[0] >= '0' && name[0] <= '9' {
			n++
		}
	}
	return n
}
