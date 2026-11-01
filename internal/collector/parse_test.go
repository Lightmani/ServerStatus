package collector

import (
	"math"
	"regexp"
	"testing"
)

func TestParseProcStatAndPercent(t *testing.T) {
	a, bt := parseProcStat("cpu  100 0 100 700 50 0 0 50 0 0\ncpu0 1 2 3\nbtime 1700000000\n")
	if bt != 1700000000 {
		t.Fatalf("btime=%d", bt)
	}
	if a.Total != 1000 || a.Idle != 700 || a.IOWait != 50 || a.Steal != 50 {
		t.Fatalf("%+v", a)
	}
	b, _ := parseProcStat("cpu  200 0 200 1300 100 0 0 200 0 0\n")
	busy, steal, iow := cpuPercent(a, b)
	// dt = 2000-1000 = 1000; idle+iowait delta = 600+50 = 650 => busy 35%
	if math.Abs(busy-35) > 1e-9 || math.Abs(steal-15) > 1e-9 || math.Abs(iow-5) > 1e-9 {
		t.Fatalf("busy=%v steal=%v iowait=%v", busy, steal, iow)
	}
}

func TestParseMeminfo(t *testing.T) {
	m := parseMeminfo("MemTotal: 1000 kB\nMemFree: 100 kB\nMemAvailable: 400 kB\nSwapTotal: 200 kB\nSwapFree: 50 kB\n")
	if m.Total != 1000*1024 || m.Used != 600*1024 || m.SwapTotal != 200*1024 || m.SwapUsed != 150*1024 {
		t.Fatalf("%+v", m)
	}
}

func TestParseNetDev(t *testing.T) {
	data := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9999 1 0 0 0 0 0 0 9999 1 0 0 0 0 0 0
  eth0: 1000 10 0 0 0 0 0 0 2000 20 0 0 0 0 0 0
docker0: 500 1 0 0 0 0 0 0 500 1 0 0 0 0 0 0
vethab12: 7 1 0 0 0 0 0 0 7 1 0 0 0 0 0 0
  ens3: 1 1 0 0 0 0 0 0 2 1 0 0 0 0 0 0
`
	rx, tx := parseNetDev(data, regexp.MustCompile(DefaultNICExclude))
	if rx != 1001 || tx != 2002 {
		t.Fatalf("rx=%d tx=%d", rx, tx)
	}
}

func TestParseSockstat(t *testing.T) {
	tcp, udp := parseSockstat("sockets: used 300\nTCP: inuse 12 orphan 0 tw 5 alloc 20 mem 3\nUDP: inuse 4 mem 1\nUDPLITE: inuse 0\n")
	if tcp != 12 || udp != 4 {
		t.Fatalf("tcp=%d udp=%d", tcp, udp)
	}
}

func TestParseLoadavg(t *testing.T) {
	l1, l5, l15, th := parseLoadavg("0.50 0.25 0.10 2/345 6789\n")
	if l1 != 0.5 || l5 != 0.25 || l15 != 0.1 || th != 345 {
		t.Fatal(l1, l5, l15, th)
	}
}

func TestParseMounts(t *testing.T) {
	data := `sysfs /sys sysfs rw 0 0
/dev/vda1 / ext4 rw 0 0
/dev/vda1 /var/lib/docker ext4 rw 0 0
tmpfs /run tmpfs rw 0 0
tank/data /data zfs rw 0 0
tank/logs /logs zfs rw 0 0
/dev/vdb /mnt/my\040disk xfs rw 0 0
overlay /var/lib/docker/overlay2/x/merged overlay rw 0 0
`
	ms := parseMounts(data)
	if len(ms) != 3 {
		t.Fatalf("got %d mounts: %+v", len(ms), ms)
	}
	if ms[2].Target != "/mnt/my disk" {
		t.Fatalf("unescape failed: %q", ms[2].Target)
	}
}

func TestParseOSReleaseAndCPU(t *testing.T) {
	if got := parseOSRelease("NAME=\"Debian GNU/Linux\"\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n"); got != "Debian GNU/Linux 12 (bookworm)" {
		t.Fatal(got)
	}
	if got := parseCPUInfo("processor : 0\nmodel name : AMD EPYC  7543 32-Core Processor\n"); got != "AMD EPYC 7543 32-Core Processor" {
		t.Fatal(got)
	}
}
