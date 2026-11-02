package pinger

import (
	"net"
	"testing"
	"time"

	"github.com/Lightmani/ServerStatus/internal/protocol"
)

func TestParseTarget(t *testing.T) {
	ok := map[string]Target{
		"1.2.3.4":           {"icmp", "1.2.3.4"},
		"example.com":       {"icmp", "example.com"},
		"[2001:db8::1]":     {"icmp", "2001:db8::1"},
		"tcp://1.2.3.4:443": {"tcp", "1.2.3.4:443"},
		"tcp://[::1]:22":    {"tcp", "[::1]:22"},
		" tcp://a.com:80 ":  {"tcp", "a.com:80"},
	}
	for in, want := range ok {
		got, err := ParseTarget(in)
		if err != nil || got != want {
			t.Errorf("ParseTarget(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "tcp://1.2.3.4", "http://x", "a b", "x/y"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("ParseTarget(%q) should fail", bad)
		}
	}
}

func TestAccAndRestore(t *testing.T) {
	p := New()
	p.accs["a"] = &acc{}
	p.record("a", 10*time.Millisecond, true)
	p.record("a", 30*time.Millisecond, true)
	p.record("a", 0, false)
	r := p.Take()
	if len(r) != 1 || r[0].Sent != 3 || r[0].Recv != 2 || r[0].RTTSum != 40 || r[0].RTTMin != 10 || r[0].RTTMax != 30 {
		t.Fatalf("take: %+v", r)
	}
	if len(p.Take()) != 0 {
		t.Fatal("take did not reset")
	}
	p.record("a", 5*time.Millisecond, true)
	p.Restore(r)
	r2 := p.Take()
	if r2[0].Sent != 4 || r2[0].Recv != 3 || r2[0].RTTMin != 5 || r2[0].RTTMax != 30 {
		t.Fatalf("restore: %+v", r2)
	}
}

func TestProbesLocal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	p := New()
	p.Update([]protocol.PingTarget{
		{ID: "tcp", Host: "tcp://" + ln.Addr().String()},
		{ID: "dead", Host: "tcp://127.0.0.1:1"},
		{ID: "lo", Host: "127.0.0.1"},
	}, time.Second)
	defer p.Stop()
	time.Sleep(2500 * time.Millisecond)
	res := map[string]protocol.PingResult{}
	for _, r := range p.Take() {
		res[r.ID] = r
	}
	if r := res["tcp"]; r.Sent == 0 || r.Recv != r.Sent {
		t.Errorf("tcp target: %+v", r)
	}
	if r := res["dead"]; r.Sent == 0 || r.Recv != 0 {
		t.Errorf("dead target: %+v", r)
	}
	// ICMP needs ping sockets or CAP_NET_RAW; only assert when it works
	if r := res["lo"]; r.Recv == 0 {
		t.Logf("ICMP unavailable in this environment: %+v", r)
	} else if r.RTTMax > 100 {
		t.Errorf("loopback rtt too high: %+v", r)
	}
}
