package collector

// Pure parsers for /proc formats, kept platform-independent so they can be
// unit tested anywhere.

import (
	"bufio"
	"errors"
	"strconv"
	"strings"
)

type cpuTimes struct {
	total, idle uint64
}

// parseCPUStat reads the aggregate "cpu" line of /proc/stat.
func parseCPUStat(s string) (cpuTimes, error) {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var t cpuTimes
		// user nice system idle iowait irq softirq steal; guest time is
		// already included in user/nice.
		for i := 1; i < len(f) && i <= 8; i++ {
			v, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil {
				return cpuTimes{}, err
			}
			t.total += v
			if i == 4 || i == 5 { // idle, iowait
				t.idle += v
			}
		}
		return t, nil
	}
	return cpuTimes{}, errors.New("no cpu line in /proc/stat")
}

// cpuUsage returns busy percent between two samples.
func cpuUsage(prev, cur cpuTimes) float64 {
	if cur.total <= prev.total || cur.idle < prev.idle {
		return 0
	}
	dt := cur.total - prev.total
	di := cur.idle - prev.idle
	if di > dt {
		return 0
	}
	return float64(dt-di) / float64(dt) * 100
}

// parseMeminfo returns /proc/meminfo values in bytes.
func parseMeminfo(s string) map[string]uint64 {
	m := make(map[string]uint64)
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 1 && f[1] == "kB" {
			v *= 1024
		}
		m[key] = v
	}
	return m
}

// memUsage derives used memory the way `free` does: total minus available.
func memUsage(m map[string]uint64) (total, used uint64) {
	total = m["MemTotal"]
	avail, ok := m["MemAvailable"]
	if !ok { // kernels before 3.14
		avail = m["MemFree"] + m["Buffers"] + m["Cached"]
	}
	if avail > total {
		avail = total
	}
	return total, total - avail
}

func parseLoadavg(s string) [3]float64 {
	var l [3]float64
	f := strings.Fields(s)
	for i := 0; i < 3 && i < len(f); i++ {
		l[i], _ = strconv.ParseFloat(f[i], 64)
	}
	return l
}

func parseUptime(s string) uint64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return uint64(v)
}

type netCounters struct {
	rx, tx uint64
}

// Interfaces whose traffic is either local or already counted on the
// physical uplink (bridges, container veths, VPN tunnels).
var virtualIfacePrefixes = []string{
	"lo", "docker", "veth", "br-", "virbr", "vnet", "cni", "flannel", "cali",
	"kube", "tun", "tap", "wg", "tailscale", "zt", "dummy", "ifb", "lxc",
}

func isVirtualIface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// parseNetDev sums rx/tx bytes over physical interfaces in /proc/net/dev.
func parseNetDev(s string) netCounters {
	var c netCounters
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if isVirtualIface(name) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(f[0], 10, 64)
		tx, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		c.rx += rx
		c.tx += tx
	}
	return c
}

// rate returns bytes/s, or 0 when the counter went backwards (reset).
func rate(prev, cur uint64, seconds float64) float64 {
	if cur < prev || seconds <= 0 {
		return 0
	}
	return float64(cur-prev) / seconds
}

// countSockets counts entries in /proc/net/{tcp,udp}[6]. When skipListen is
// set, TCP sockets in LISTEN state (0A) are excluded.
func countSockets(s string, skipListen bool) int {
	n := 0
	sc := bufio.NewScanner(strings.NewReader(s))
	first := true
	for sc.Scan() {
		if first { // header
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		if skipListen && f[3] == "0A" {
			continue
		}
		n++
	}
	return n
}

type mount struct {
	device, point, fstype string
}

var realFilesystems = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
	"zfs": true, "f2fs": true, "jfs": true, "reiserfs": true, "bcachefs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true,
}

// parseMounts returns real, block-backed filesystems from /proc/mounts,
// one entry per device (bind mounts and btrfs subvolumes are deduplicated).
func parseMounts(s string) []mount {
	var out []mount
	seen := make(map[string]bool)
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || !realFilesystems[f[2]] || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		out = append(out, mount{device: f[0], point: unescapeMount(f[1]), fstype: f[2]})
	}
	return out
}

// unescapeMount decodes the octal escapes (\040 etc.) used in /proc/mounts.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func parseOSRelease(s string) string {
	vals := make(map[string]string)
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		vals[k] = strings.Trim(v, `"'`)
	}
	if v := vals["PRETTY_NAME"]; v != "" {
		return v
	}
	return strings.TrimSpace(vals["NAME"] + " " + vals["VERSION_ID"])
}

func parseCPUModel(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	var fallback string
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "model name": // x86
			return v
		case "Model", "Hardware", "cpu model", "uarch": // arm, mips, riscv
			if fallback == "" {
				fallback = v
			}
		}
	}
	return fallback
}

// detectVirt classifies the DMI vendor/product strings.
func detectVirt(vendor, product string) string {
	s := strings.ToLower(vendor + " " + product)
	switch {
	case strings.Contains(s, "kvm"), strings.Contains(s, "qemu"):
		return "KVM"
	case strings.Contains(s, "vmware"):
		return "VMware"
	case strings.Contains(s, "virtualbox"):
		return "VirtualBox"
	case strings.Contains(s, "xen"):
		return "Xen"
	case strings.Contains(s, "microsoft"):
		return "Hyper-V"
	case strings.Contains(s, "amazon ec2"):
		return "AWS Nitro"
	case strings.Contains(s, "google compute"):
		return "GCE"
	case strings.Contains(s, "bochs"):
		return "Bochs"
	case strings.Contains(s, "parallels"):
		return "Parallels"
	case strings.TrimSpace(s) == "":
		return ""
	}
	return "Dedicated"
}
