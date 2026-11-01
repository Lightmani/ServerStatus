package collector

import (
	"bufio"
	"regexp"
	"strconv"
	"strings"
)

// cpuTimes are the aggregate jiffies from the first line of /proc/stat.
type cpuTimes struct {
	Total, Idle, IOWait, Steal uint64
}

// parseProcStat returns aggregate CPU times and the boot time (btime).
func parseProcStat(data string) (cpuTimes, int64) {
	var ct cpuTimes
	var btime int64
	sc := bufio.NewScanner(strings.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "cpu "):
			f := strings.Fields(line)[1:]
			vals := make([]uint64, len(f))
			for i, s := range f {
				vals[i], _ = strconv.ParseUint(s, 10, 64)
			}
			// user nice system idle iowait irq softirq steal guest guest_nice
			// guest time is already included in user/nice, so stop at steal.
			for i := 0; i < len(vals) && i < 8; i++ {
				ct.Total += vals[i]
			}
			if len(vals) > 3 {
				ct.Idle = vals[3]
			}
			if len(vals) > 4 {
				ct.IOWait = vals[4]
			}
			if len(vals) > 7 {
				ct.Steal = vals[7]
			}
		case strings.HasPrefix(line, "btime "):
			btime, _ = strconv.ParseInt(strings.TrimSpace(line[6:]), 10, 64)
		}
	}
	return ct, btime
}

// cpuPercent computes busy/steal/iowait percentages between two samples.
func cpuPercent(prev, cur cpuTimes) (busy, steal, iowait float64) {
	if cur.Total <= prev.Total {
		return 0, 0, 0
	}
	dt := float64(cur.Total - prev.Total)
	idle := float64(sub(cur.Idle, prev.Idle) + sub(cur.IOWait, prev.IOWait))
	busy = clampPct((dt - idle) / dt * 100)
	steal = clampPct(float64(sub(cur.Steal, prev.Steal)) / dt * 100)
	iowait = clampPct(float64(sub(cur.IOWait, prev.IOWait)) / dt * 100)
	return
}

type memInfo struct {
	Total, Used, SwapTotal, SwapUsed uint64
}

// parseMeminfo parses /proc/meminfo (values are in kB).
func parseMeminfo(data string) memInfo {
	kv := map[string]uint64{}
	for _, line := range strings.Split(data, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		kv[k] = n * 1024
	}
	m := memInfo{Total: kv["MemTotal"], SwapTotal: kv["SwapTotal"]}
	avail, ok := kv["MemAvailable"]
	if !ok {
		avail = kv["MemFree"] + kv["Buffers"] + kv["Cached"] + kv["SReclaimable"] - kv["Shmem"]
	}
	m.Used = sub(m.Total, avail)
	m.SwapUsed = sub(m.SwapTotal, kv["SwapFree"])
	return m
}

// parseNetDev sums rx/tx bytes over interfaces not matching exclude.
func parseNetDev(data string, exclude *regexp.Regexp) (rx, tx uint64) {
	for _, line := range strings.Split(data, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || (exclude != nil && exclude.MatchString(name)) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, err1 := strconv.ParseUint(f[0], 10, 64)
		t, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rx += r
		tx += t
	}
	return
}

// parseSockstat returns the "inuse" counts for TCP and UDP from
// /proc/net/sockstat or /proc/net/sockstat6.
func parseSockstat(data string) (tcp, udp int) {
	for _, line := range strings.Split(data, "\n") {
		proto, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		inuse := 0
		for i := 0; i+1 < len(f); i += 2 {
			if f[i] == "inuse" {
				inuse, _ = strconv.Atoi(f[i+1])
				break
			}
		}
		switch proto {
		case "TCP", "TCP6":
			tcp += inuse
		case "UDP", "UDP6":
			udp += inuse
		}
	}
	return
}

// parseLoadavg returns load averages and the total number of tasks (threads).
func parseLoadavg(data string) (l1, l5, l15 float64, threads int) {
	f := strings.Fields(data)
	if len(f) < 4 {
		return
	}
	l1, _ = strconv.ParseFloat(f[0], 64)
	l5, _ = strconv.ParseFloat(f[1], 64)
	l15, _ = strconv.ParseFloat(f[2], 64)
	if _, total, ok := strings.Cut(f[3], "/"); ok {
		threads, _ = strconv.Atoi(total)
	}
	return
}

// parseOSRelease returns PRETTY_NAME (or NAME VERSION) from /etc/os-release.
func parseOSRelease(data string) string {
	kv := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		kv[k] = strings.Trim(v, `"'`)
	}
	if v := kv["PRETTY_NAME"]; v != "" {
		return v
	}
	return strings.TrimSpace(kv["NAME"] + " " + kv["VERSION"])
}

// parseCPUInfo returns the CPU model name from /proc/cpuinfo.
func parseCPUInfo(data string) string {
	keys := []string{"model name", "Model", "Hardware", "cpu model", "Processor", "uarch"}
	found := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if _, seen := found[k]; !seen && v != "" {
			found[k] = v
		}
	}
	for _, k := range keys {
		if v := found[k]; v != "" {
			return strings.Join(strings.Fields(v), " ")
		}
	}
	return ""
}

// mount is a line from /proc/mounts.
type mount struct {
	Source, Target, FSType string
}

var diskFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
	"zfs": true, "f2fs": true, "jfs": true, "reiserfs": true, "bcachefs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "fuseblk": true,
	"simfs": true, // OpenVZ containers
}

// parseMounts returns de-duplicated physical filesystems.
func parseMounts(data string) []mount {
	var out []mount
	seen := map[string]bool{}
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !diskFS[f[2]] {
			continue
		}
		m := mount{Source: f[0], Target: unescapeMount(f[1]), FSType: f[2]}
		key := m.Source
		if m.FSType == "zfs" {
			// datasets of one pool share capacity; count each pool once
			key, _, _ = strings.Cut(m.Source, "/")
			key = "zfs:" + key
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, m)
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
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func sub(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return 0
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
