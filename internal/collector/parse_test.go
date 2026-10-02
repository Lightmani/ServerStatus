package collector

import (
	"math"
	"testing"
)

func TestCPU(t *testing.T) {
	a, err := parseCPUStat("cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 2 3 4\n")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := parseCPUStat("cpu  150 0 150 750 150 0 0 0 0 0\n")
	// delta total 200, delta idle(idle+iowait) 100 -> 50% busy
	if got := cpuUsage(a, b); math.Abs(got-50) > 1e-9 {
		t.Fatalf("got %v", got)
	}
	if got := cpuUsage(b, a); got != 0 {
		t.Fatalf("counter reset: got %v", got)
	}
	if _, err := parseCPUStat("intr 1 2 3"); err == nil {
		t.Fatal("expected error")
	}
}

func TestMeminfo(t *testing.T) {
	m := parseMeminfo("MemTotal:  1000 kB\nMemFree: 100 kB\nMemAvailable:  400 kB\nSwapTotal: 0 kB\nHugePages_Total: 0\n")
	total, used := memUsage(m)
	if total != 1000*1024 || used != 600*1024 {
		t.Fatalf("total=%d used=%d", total, used)
	}
	old := parseMeminfo("MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 50 kB\nCached: 250 kB\n")
	if _, used := memUsage(old); used != 600*1024 {
		t.Fatalf("legacy used=%d", used)
	}
}

func TestNetDev(t *testing.T) {
	const s = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9999 1 0 0 0 0 0 0 9999 1 0 0 0 0 0 0
  eth0: 1000 10 0 0 0 0 0 0 2000 20 0 0 0 0 0 0
  ens4:500 5 0 0 0 0 0 0 700 7 0 0 0 0 0 0
docker0: 123 1 0 0 0 0 0 0 456 1 0 0 0 0 0 0
vethabc: 123 1 0 0 0 0 0 0 456 1 0 0 0 0 0 0
   wg0: 300 1 0 0 0 0 0 0 300 1 0 0 0 0 0 0
`
	c := parseNetDev(s)
	if c.rx != 1500 || c.tx != 2700 {
		t.Fatalf("got %+v", c)
	}
	if r := rate(100, 400, 3); r != 100 {
		t.Fatalf("rate %v", r)
	}
	if r := rate(400, 100, 3); r != 0 {
		t.Fatalf("reset rate %v", r)
	}
}

func TestSockets(t *testing.T) {
	const tcp = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1
   1: 0100007F:0016 0100007F:A000 01 00000000:00000000 00:00000000 00000000     0        0 2
   2: 0100007F:0016 0100007F:A001 06 00000000:00000000 00:00000000 00000000     0        0 3
`
	if n := countSockets(tcp, true); n != 2 {
		t.Fatalf("tcp=%d", n)
	}
	if n := countSockets(tcp, false); n != 3 {
		t.Fatalf("all=%d", n)
	}
}

func TestMounts(t *testing.T) {
	const s = `sysfs /sys sysfs rw 0 0
proc /proc proc rw 0 0
/dev/vda1 / ext4 rw,relatime 0 0
tmpfs /run tmpfs rw 0 0
/dev/vda1 /var/lib/docker ext4 rw 0 0
/dev/vdb /mnt/my\040data xfs rw 0 0
overlay /var/lib/docker/overlay2/x/merged overlay rw 0 0
`
	m := parseMounts(s)
	if len(m) != 2 || m[0].point != "/" || m[1].point != "/mnt/my data" || m[1].fstype != "xfs" {
		t.Fatalf("got %+v", m)
	}
}

func TestHostParsers(t *testing.T) {
	if got := parseOSRelease("NAME=\"Debian GNU/Linux\"\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n"); got != "Debian GNU/Linux 12 (bookworm)" {
		t.Fatalf("os: %q", got)
	}
	if got := parseOSRelease("NAME=Alpine\nVERSION_ID=3.20\n"); got != "Alpine 3.20" {
		t.Fatalf("os fallback: %q", got)
	}
	if got := parseCPUModel("processor\t: 0\nmodel name\t: AMD EPYC 7B13\n"); got != "AMD EPYC 7B13" {
		t.Fatalf("cpu: %q", got)
	}
	if got := parseCPUModel("processor\t: 0\nModel\t\t: Raspberry Pi 4\n"); got != "Raspberry Pi 4" {
		t.Fatalf("arm cpu: %q", got)
	}
	if l := parseLoadavg("0.10 0.20 0.30 1/100 1234\n"); l != [3]float64{0.1, 0.2, 0.3} {
		t.Fatalf("load %v", l)
	}
	if u := parseUptime("12345.67 999.00\n"); u != 12345 {
		t.Fatalf("uptime %d", u)
	}
	for in, want := range map[[2]string]string{
		{"QEMU", "Standard PC (i440FX + PIIX, 1996)"}: "KVM",
		{"Xen", "HVM domU"}:                           "Xen",
		{"Microsoft Corporation", "Virtual Machine"}:  "Hyper-V",
		{"Dell Inc.", "PowerEdge R640"}:               "Dedicated",
		{"", ""}:                                      "",
	} {
		if got := detectVirt(in[0], in[1]); got != want {
			t.Errorf("%v: got %q want %q", in, got, want)
		}
	}
}
