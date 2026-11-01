package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Lightmani/ServerStatus/internal/protocol"
)

func TestTrafficDelta(t *testing.T) {
	cases := []struct {
		name           string
		have           bool
		lastBoot, boot int64
		lastRx, rx     uint64
		wantRx         uint64
	}{
		{"first report", false, 0, 1000, 0, 500, 0},
		{"normal", true, 1000, 1000, 100, 350, 250},
		{"btime jitter", true, 1000, 1001, 100, 350, 250},
		{"reboot", true, 1000, 5000, 9999, 300, 300},
		{"counter reset w/o reboot", true, 1000, 1000, 9999, 300, 0},
	}
	for _, c := range cases {
		rx, _ := trafficDelta(c.have, c.lastBoot, c.lastRx, 0, c.boot, c.rx, 0)
		if rx != c.wantRx {
			t.Errorf("%s: got %d want %d", c.name, rx, c.wantRx)
		}
	}
}

func TestCycleStart(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 12, 0, 0, 0, loc) }
	cases := []struct {
		now   time.Time
		reset int
		want  time.Time
	}{
		{d(2026, 3, 15), 1, time.Date(2026, 3, 1, 0, 0, 0, 0, loc)},
		{d(2026, 3, 15), 20, time.Date(2026, 2, 20, 0, 0, 0, 0, loc)},
		{d(2026, 3, 20), 20, time.Date(2026, 3, 20, 0, 0, 0, 0, loc)},
		{d(2026, 1, 5), 10, time.Date(2025, 12, 10, 0, 0, 0, 0, loc)},
		{d(2026, 3, 1), 31, time.Date(2026, 2, 28, 0, 0, 0, 0, loc)}, // clamp to Feb end
		{d(2026, 2, 28), 31, time.Date(2026, 2, 28, 0, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		if got := cycleStart(c.now, c.reset); !got.Equal(c.want) {
			t.Errorf("cycleStart(%s, %d) = %s want %s", c.now, c.reset, got, c.want)
		}
	}
}

func TestPeriods(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, loc) // Sunday
	w := buildPeriods(PeriodWeek, now, 2, 0)
	if !w[1].start.Equal(time.Date(2026, 10, 26, 0, 0, 0, 0, loc)) || w[1].label != "2026-W44" {
		t.Fatalf("week: %v %s", w[1].start, w[1].label)
	}
	if !w[0].end.Equal(w[1].start) {
		t.Fatal("weeks not contiguous")
	}
	m := buildPeriods(PeriodMonth, now, 3, 1)
	if m[2].label != "2026-10" || m[0].label != "2026-08" {
		t.Fatalf("months: %s %s", m[0].label, m[2].label)
	}
	dd := buildPeriods(PeriodDay, now, 1, 0)
	if dd[0].label != "2026-11-01" || dd[0].end.Sub(dd[0].start) != 24*time.Hour {
		t.Fatalf("day: %+v", dd[0])
	}
}

func TestAggregatePeriods(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 1, 3, 0, 0, 0, 0, loc)
	periods := buildPeriods(PeriodDay, now.Add(-time.Second), 2, 0) // Jan 1, Jan 2
	day1 := periods[0].start.Unix()
	rows := []BucketRow{
		{Bucket: Bucket{TS: day1, Samples: 20, Minutes: 60, CPUSum: 200, CPUMax: 50, Rx: 100, Tx: 10, MemSum: 20 * 512, MemTotal: 1024}},
		{Bucket: Bucket{TS: day1 + 3600, Samples: 20, Minutes: 60, CPUSum: 600, CPUMax: 90, Rx: 300, Tx: 30, MemSum: 20 * 512, MemTotal: 1024}},
		{Bucket: Bucket{TS: day1 + 86400, Samples: 1, Minutes: 1, CPUSum: 5, Rx: 7}},
	}
	st := aggregatePeriods(rows, periods, day1, now)
	if st[0].Rx != 400 || st[0].Tx != 40 || st[0].CPUAvg != 20 || st[0].CPUMax != 90 || st[0].MemPct != 50 {
		t.Fatalf("day1: %+v", st[0])
	}
	if st[0].Availability == nil || *st[0].Availability != r2(120.0/1440*100) {
		t.Fatalf("availability: %v", st[0].Availability)
	}
	if st[1].Rx != 7 {
		t.Fatalf("day2: %+v", st[1])
	}
}

func TestDownsampleAlignsToLocalDay(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	// 16:00 UTC == 00:00 CST next day
	base := time.Date(2026, 1, 1, 15, 0, 0, 0, time.UTC).Unix()
	rows := []BucketRow{
		{Bucket: Bucket{TS: base, Samples: 1, Rx: 1}},
		{Bucket: Bucket{TS: base + 3600, Samples: 1, Rx: 2}},
	}
	out := downsample(rows, 86400, loc)
	if len(out) != 2 || out[0].Rx != 1 || out[1].Rx != 2 {
		t.Fatalf("got %+v", out)
	}
}

func TestReplayGuard(t *testing.T) {
	g := NewReplayGuard(60 * time.Second)
	body := []byte("{}")
	now := time.Unix(1700000000, 0)
	mk := func(ts int64, nonce string) protocol.Signed {
		return protocol.Signed{Node: "n", Timestamp: ts, Nonce: nonce,
			Signature: protocol.Sign("k", "POST", protocol.ReportPath, "n", ts, nonce, body)}
	}
	s := mk(now.Unix(), "00112233445566778899aabbccddeeff")
	if err := g.Check(s, "k", "POST", protocol.ReportPath, body, now); err != nil {
		t.Fatal(err)
	}
	if err := g.Check(s, "k", "POST", protocol.ReportPath, body, now.Add(time.Second)); err != errReplay {
		t.Fatalf("replay not detected: %v", err)
	}
	old := mk(now.Unix()-120, "ffeeddccbbaa99887766554433221100")
	if err := g.Check(old, "k", "POST", protocol.ReportPath, body, now); err != errSkew {
		t.Fatalf("stale request accepted: %v", err)
	}
	bad := mk(now.Unix(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err := g.Check(bad, "other", "POST", protocol.ReportPath, body, now); err != errSig {
		t.Fatalf("bad signature accepted: %v", err)
	}
}

func TestPassword(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "correct horse") || CheckPassword(h, "wrong") {
		t.Fatal("password check failed")
	}
	key := NewSessionKey()
	v := makeSession(key, time.Now().Add(time.Hour))
	if !validSession(key, v, time.Now()) || validSession(key, v, time.Now().Add(2*time.Hour)) || validSession(NewSessionKey(), v, time.Now()) {
		t.Fatal("session validation failed")
	}
}

// End-to-end: signed report -> live snapshot -> flush -> stats/history.
func TestEndToEnd(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Database = filepath.Join(t.TempDir(), "t.db")
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	created := time.Now().Add(-time.Hour).Unix()
	if err := store.AddNode(Node{ID: "hk1", Name: "HK", Secret: "sekret", ResetDay: 1, CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	hub, err := NewHub(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: cfg, store: store, hub: hub, guard: NewReplayGuard(time.Minute),
		reportLim: NewLimiter(30, time.Minute, time.Minute), loginLim: NewLimiter(5, time.Minute, time.Minute)}
	if err := s.refreshAuth(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	send := func(secret string, rx uint64) int {
		rep := protocol.Report{Time: time.Now().Unix(),
			Host:  protocol.HostInfo{Hostname: "<script>x</script>", BootTime: 1000},
			State: protocol.State{CPU: 42, MemUsed: 512, MemTotal: 1024, NetRx: rx, NetTx: rx / 2}}
		body, _ := json.Marshal(rep)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+protocol.ReportPath, bytes.NewReader(body))
		protocol.SignRequest(req, protocol.ReportPath, "hk1", secret, body, time.Now())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := send("wrong", 1000); c != http.StatusUnauthorized {
		t.Fatalf("bad secret: %d", c)
	}
	if c := send("sekret", 1000); c != http.StatusNoContent {
		t.Fatalf("report: %d", c)
	}
	if c := send("sekret", 5000); c != http.StatusNoContent {
		t.Fatalf("report: %d", c)
	}

	var nodes struct{ Nodes []NodeView }
	getJSON(t, ts.URL+"/api/nodes", &nodes)
	if len(nodes.Nodes) != 1 || !nodes.Nodes[0].Online || nodes.Nodes[0].State.CPU != 42 {
		t.Fatalf("nodes: %+v", nodes)
	}
	if nodes.Nodes[0].Cycle.Rx != 4000 {
		t.Fatalf("live cycle rx = %d", nodes.Nodes[0].Cycle.Rx)
	}

	// flush synchronously and check persisted stats
	hub.refreshCycles(time.Now(), true)
	jobs := hub.flushStale(time.Now(), true)
	hub.save(jobs)

	var st struct{ Stats []PeriodStat }
	getJSON(t, ts.URL+"/api/nodes/hk1/stats?period=day&count=2", &st)
	last := st.Stats[len(st.Stats)-1]
	if len(st.Stats) != 2 || last.Rx != 4000 || last.Tx != 2000 || last.CPUAvg != 42 || last.Minutes != 1 {
		t.Fatalf("stats: %+v", st.Stats)
	}
	var all struct {
		Nodes []nodeStat
		Total map[string]uint64
	}
	getJSON(t, ts.URL+"/api/stats?period=month", &all)
	if len(all.Nodes) != 1 || all.Total["rx"] != 4000 {
		t.Fatalf("all stats: %+v", all)
	}
	var h History
	getJSON(t, ts.URL+"/api/nodes/hk1/history?range=1h", &h)
	if len(h.T) != 1 || h.CPU[0] != 42 || h.Mem[0] != 50 {
		t.Fatalf("history: %+v", h)
	}
	getJSON(t, ts.URL+"/api/nodes", &nodes)
	if nodes.Nodes[0].Cycle.Rx != 4000 {
		t.Fatalf("persisted cycle rx = %d", nodes.Nodes[0].Cycle.Rx)
	}

	// counters persisted: a fresh hub continues accounting across restarts
	hub2, _ := NewHub(cfg, store)
	n := hub2.nodes["hk1"]
	if !n.haveCounters || n.lastRx != 5000 || n.lastBoot != 1000 {
		t.Fatalf("counters not persisted: %+v", n)
	}

	// password protection
	hash, _ := HashPassword("password123")
	_ = store.SetKV(kvPasswordHash, hash)
	_ = s.refreshAuth()
	resp, _ := http.Get(ts.URL + "/api/nodes")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	resp, _ = http.Post(ts.URL+"/api/login", "application/json", bytes.NewReader([]byte(`{"password":"password123"}`)))
	resp.Body.Close()
	if resp.StatusCode != 200 || len(resp.Cookies()) == 0 {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/nodes", nil)
	req.AddCookie(resp.Cookies()[0])
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("authed request failed: %d", resp.StatusCode)
	}
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}
