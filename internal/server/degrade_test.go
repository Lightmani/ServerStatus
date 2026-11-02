package server

import (
	"strings"
	"testing"
	"time"

	"github.com/Lightmani/ServerStatus/internal/protocol"
)

func win(sent, recv int64, rtt float64) PingBucket {
	return PingBucket{Sent: sent, Recv: recv, RTTSum: rtt * float64(recv), RTTMin: rtt, RTTMax: rtt}
}

func TestLatencyThreshold(t *testing.T) {
	d := DefaultConfig().Alert.Degrade // factor 2, +50ms
	cases := []struct{ base, factor, want float64 }{
		{40, 1, 90},   // max(80, 90)
		{200, 1, 400}, // max(400, 250)
		{40, 0.8, 80}, // relaxed: 40 + 50*0.8
		{0, 1, 0},     // no baseline, no absolute limit: not evaluable
	}
	for _, c := range cases {
		if got := latencyThreshold(&d, c.base, c.factor); got != c.want {
			t.Errorf("latencyThreshold(%v, %v) = %v want %v", c.base, c.factor, got, c.want)
		}
	}
	d.LatencyAbsMs = 300
	if got := latencyThreshold(&d, 0, 1); got != 300 {
		t.Errorf("absolute only: %v", got)
	}
	if got := latencyThreshold(&d, 200, 1); got != 300 {
		t.Errorf("absolute lower than relative should win: %v", got)
	}
}

func TestEvalDegrade(t *testing.T) {
	d := DefaultConfig().Alert.Degrade
	cases := []struct {
		name string
		in   targetAlertInput
		want decision
	}{
		{"not covered", targetAlertInput{Win: win(100, 50, 40), Covered: false, Baseline: 40}, keepIfActive},
		{"loss 30%", targetAlertInput{Win: win(100, 70, 40), Covered: true, Baseline: 40}, fire},
		{"latency 100 > 90", targetAlertInput{Win: win(100, 100, 100), Covered: true, Baseline: 40}, fire},
		{"loss 15% hysteresis", targetAlertInput{Win: win(100, 85, 40), Covered: true, Baseline: 40}, keepIfActive},
		{"latency 85 hysteresis", targetAlertInput{Win: win(100, 100, 85), Covered: true, Baseline: 40}, keepIfActive},
		{"healthy", targetAlertInput{Win: win(100, 98, 45), Covered: true, Baseline: 40}, resolve},
		{"no baseline yet", targetAlertInput{Win: win(100, 100, 1000), Covered: true}, resolve},
		{"outage in progress", targetAlertInput{Win: win(100, 50, 40), Covered: true, LastLost: true, Baseline: 40}, keepIfActive},
	}
	for _, c := range cases {
		if got, _ := evalDegrade(&d, &c.in); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestComputeBaselines(t *testing.T) {
	var rows []PingRowNode
	add := func(node string, n int, sent, recv int64, rtt float64) {
		for i := 0; i < n; i++ {
			rows = append(rows, PingRowNode{NodeID: node, PingRow: PingRow{TargetID: "t", PingBucket: win(sent, recv, rtt)}})
		}
	}
	add("a", 30, 100, 100, 40) // typical
	add("a", 5, 100, 100, 300) // incident hours: must not move the median
	add("a", 10, 100, 20, 900) // >50% loss hours: ignored
	add("b", 10, 100, 100, 40) // not enough history
	b := computeBaselines(rows, 24)
	if b[baselineKey("a", "t")] != 40 {
		t.Fatalf("baseline a = %v", b[baselineKey("a", "t")])
	}
	if _, ok := b[baselineKey("b", "t")]; ok {
		t.Fatal("b should have no baseline yet")
	}
}

func feed(hub *Hub, at time.Time, sent, recv int, rtt float64) {
	_ = hub.Ingest("n1", &protocol.Report{
		Host:  protocol.HostInfo{BootTime: 1},
		Pings: []protocol.PingResult{{ID: "t1", Sent: sent, Recv: recv, RTTSum: rtt * float64(recv), RTTMin: rtt, RTTMax: rtt}},
	}, at)
}

func degradeFixture(t *testing.T) (*Alerter, *Hub, *fakeNotifier, time.Time) {
	cfg, st, hub := newAlertFixture(t)
	fn := &fakeNotifier{}
	a, _ := NewAlerter(cfg, hub, st, fn)
	a.started = time.Now().Add(-time.Hour)
	a.lastBaseline = time.Now().Add(24 * time.Hour) // keep the injected baseline
	hub.setBaselines(map[string]float64{baselineKey("n1", "t1"): 40})
	return a, hub, fn, time.Now().Truncate(time.Minute)
}

func TestAlerterDegradeLifecycle(t *testing.T) {
	a, hub, fn, base := degradeFixture(t)
	at := func(m int) time.Time { return base.Add(time.Duration(m)*time.Minute + time.Second) }

	// 10 minutes with 30% loss
	for m := 0; m < 10; m++ {
		feed(hub, at(m), 10, 7, 40)
	}
	a.Evaluate(at(10))
	msgs := fn.take()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "[劣化]") || !strings.Contains(msgs[0], "丢包率 30.0%") {
		t.Fatalf("degrade alert: %q", msgs)
	}
	if al := a.Active(true); len(al) != 1 || al[0].Kind != AlertPingDegrade || al[0].Detail == "" {
		t.Fatalf("active: %+v", al)
	}
	// 10 clean minutes -> recovery
	for m := 10; m < 20; m++ {
		feed(hub, at(m), 10, 10, 41)
	}
	a.Evaluate(at(20))
	if msgs = fn.take(); len(msgs) != 1 || !strings.Contains(msgs[0], "线路质量恢复") {
		t.Fatalf("degrade recovery: %q", msgs)
	}
	// latency triples without loss
	for m := 20; m < 30; m++ {
		feed(hub, at(m), 10, 10, 150)
	}
	a.Evaluate(at(30))
	if msgs = fn.take(); len(msgs) != 1 || !strings.Contains(msgs[0], "平均延迟 150.0 ms") || !strings.Contains(msgs[0], "基线 40.0 ms") {
		t.Fatalf("latency alert: %q", msgs)
	}
}

func TestDegradeDefersToOutage(t *testing.T) {
	a, hub, fn, base := degradeFixture(t)
	at := func(m int) time.Time { return base.Add(time.Duration(m)*time.Minute + time.Second) }

	for m := 0; m < 10; m++ {
		feed(hub, at(m), 10, 6, 40)
	}
	a.Evaluate(at(10))
	if len(fn.take()) != 1 {
		t.Fatal("expected degrade alert")
	}
	// the line dies completely: only the outage alert is sent, the
	// degradation alert is superseded silently
	for m := 10; m < 21; m++ {
		feed(hub, at(m), 10, 0, 0)
		a.Evaluate(at(m))
	}
	a.Evaluate(at(21))
	msgs := fn.take()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "[丢包]") {
		t.Fatalf("expected only the outage alert, got %q", msgs)
	}
	if al := a.Active(true); len(al) != 1 || al[0].Kind != AlertPingLoss {
		t.Fatalf("active: %+v", al)
	}
	// recovery: exactly one recovery message, and no false degradation
	// alert while the window still contains the outage minutes
	for m := 21; m < 35; m++ {
		feed(hub, at(m), 10, 10, 40)
		a.Evaluate(at(m))
	}
	msgs = fn.take()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "[恢复]") || strings.Contains(msgs[0], "劣化") {
		t.Fatalf("after recovery: %q", msgs)
	}
	if len(a.Active(true)) != 0 {
		t.Fatalf("active after recovery: %+v", a.Active(true))
	}
}

func TestDegradeQuietDuringFullOutageOnset(t *testing.T) {
	a, hub, fn, base := degradeFixture(t)
	at := func(m int) time.Time { return base.Add(time.Duration(m)*time.Minute + time.Second) }
	for m := 0; m < 10; m++ {
		feed(hub, at(m), 10, 10, 40)
	}
	a.Evaluate(at(10))
	// sudden total outage: window loss crosses 20% after a few minutes, but
	// the latest minute is fully lost, so degradation must stay quiet and let
	// the outage rule speak
	for m := 10; m < 19; m++ {
		feed(hub, at(m), 10, 0, 0)
		a.Evaluate(at(m + 1))
	}
	if msgs := fn.take(); len(msgs) != 0 {
		t.Fatalf("no alert expected before the outage rule fires: %q", msgs)
	}
}
