package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lightmani/ServerStatus/internal/protocol"
)

type fakeNotifier struct {
	mu   sync.Mutex
	msgs []string
}

func (f *fakeNotifier) Notify(s string) { f.mu.Lock(); f.msgs = append(f.msgs, s); f.mu.Unlock() }
func (f *fakeNotifier) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.msgs
	f.msgs = nil
	return m
}

func TestPingAllLost(t *testing.T) {
	tr := &pingTrack{}
	base := int64(1_800_000_000) - int64(1_800_000_000)%60
	lost := &protocol.PingResult{Sent: 30, Recv: 0}
	for m := 0; m < 10; m++ {
		tr.add(lost, base+int64(m)*60)
	}
	now := base + 600 // minutes base..base+540 are complete
	if !tr.allLost(now, 10) {
		t.Fatal("10 lost minutes should alert")
	}
	if tr.allLost(now, 11) {
		t.Fatal("only 10 minutes of data")
	}
	// a single reply in the window breaks it
	tr2 := &pingTrack{}
	for m := 0; m < 10; m++ {
		r := lost
		if m == 4 {
			r = &protocol.PingResult{Sent: 30, Recv: 1, RTTSum: 50, RTTMin: 50, RTTMax: 50}
		}
		tr2.add(r, base+int64(m)*60)
	}
	if tr2.allLost(now, 10) {
		t.Fatal("reply inside window must not alert")
	}
	// a gap (agent offline) breaks it
	tr3 := &pingTrack{}
	for m := 0; m < 10; m++ {
		if m != 6 {
			tr3.add(lost, base+int64(m)*60)
		}
	}
	if tr3.allLost(now, 10) {
		t.Fatal("minute without data must not alert")
	}
}

func newAlertFixture(t *testing.T) (*Config, *Store, *Hub) {
	cfg := DefaultConfig()
	cfg.Database = filepath.Join(t.TempDir(), "a.db")
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_ = st.AddNode(Node{ID: "n1", Name: "HK<1>", Secret: "s", ResetDay: 1, CreatedAt: 1})
	_ = st.AddPingTarget(PingTargetDef{ID: "t1", Name: "上海电信", Host: "1.2.3.4", CreatedAt: 1})
	hub, err := NewHub(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, st, hub
}

func report(hub *Hub, at time.Time, recv int) {
	_ = hub.Ingest("n1", &protocol.Report{
		Host:  protocol.HostInfo{BootTime: 1},
		Pings: []protocol.PingResult{{ID: "t1", Sent: 3, Recv: recv, RTTSum: float64(recv) * 20, RTTMin: 20, RTTMax: 20}},
	}, at)
}

func TestAlerterOffline(t *testing.T) {
	cfg, st, hub := newAlertFixture(t)
	fn := &fakeNotifier{}
	a, _ := NewAlerter(cfg, hub, st, fn)
	t0 := time.Now()
	a.started = t0.Add(-time.Hour)
	report(hub, t0, 3)

	a.Evaluate(t0.Add(4 * time.Minute))
	if len(fn.take()) != 0 {
		t.Fatal("alerted before 5 minutes")
	}
	a.Evaluate(t0.Add(5*time.Minute + time.Second))
	msgs := fn.take()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "[离线]") || !strings.Contains(msgs[0], "HK&lt;1&gt;") {
		t.Fatalf("offline alert: %q", msgs)
	}
	a.Evaluate(t0.Add(10 * time.Minute))
	if len(fn.take()) != 0 {
		t.Fatal("alert repeated")
	}
	// restart: persisted state, no duplicate
	a2, _ := NewAlerter(cfg, hub, st, fn)
	a2.started = t0.Add(-time.Hour)
	a2.Evaluate(t0.Add(11 * time.Minute))
	if len(fn.take()) != 0 || len(a2.Active(true)) != 1 {
		t.Fatal("alert state not persisted")
	}
	// node comes back
	back := t0.Add(12 * time.Minute)
	report(hub, back, 3)
	a2.Evaluate(back.Add(time.Second))
	msgs = fn.take()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "[恢复]") || len(a2.Active(true)) != 0 {
		t.Fatalf("recovery: %q", msgs)
	}
}

func TestAlerterStartupGrace(t *testing.T) {
	cfg, st, hub := newAlertFixture(t)
	fn := &fakeNotifier{}
	t0 := time.Now()
	report(hub, t0.Add(-time.Hour), 3) // last seen an hour ago (e.g. server was down)
	a, _ := NewAlerter(cfg, hub, st, fn)
	a.started = t0
	a.Evaluate(t0.Add(time.Minute))
	if len(fn.take()) != 0 {
		t.Fatal("alerted during startup grace")
	}
	a.Evaluate(t0.Add(5*time.Minute + time.Second))
	if len(fn.take()) != 1 {
		t.Fatal("no alert after grace")
	}
}

func TestAlerterPingLossAndMute(t *testing.T) {
	cfg, st, hub := newAlertFixture(t)
	fn := &fakeNotifier{}
	a, _ := NewAlerter(cfg, hub, st, fn)
	a.started = time.Now().Add(-time.Hour)
	base := time.Now().Truncate(time.Minute)
	for m := 0; m <= 10; m++ {
		report(hub, base.Add(time.Duration(m)*time.Minute+time.Second), 0)
	}
	now := base.Add(10*time.Minute + 2*time.Second)
	a.Evaluate(now)
	msgs := fn.take()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "[丢包]") || !strings.Contains(msgs[0], "上海电信") {
		t.Fatalf("ping alert: %q", msgs)
	}
	report(hub, now.Add(time.Second), 2)
	a.Evaluate(now.Add(2 * time.Second))
	if msgs = fn.take(); len(msgs) != 1 || !strings.Contains(msgs[0], "[恢复]") {
		t.Fatalf("ping recovery: %q", msgs)
	}

	// mute node while an offline alert is firing: dropped silently
	a.Evaluate(now.Add(10 * time.Minute))
	if len(fn.take()) != 1 {
		t.Fatal("expected offline alert")
	}
	n, _ := st.GetNode("n1")
	n.NoAlert = true
	_ = st.UpdateNodeMeta(n)
	_ = hub.reload()
	a.Evaluate(now.Add(11 * time.Minute))
	if len(fn.take()) != 0 || len(a.Active(true)) != 0 {
		t.Fatal("muted node should be cleared silently")
	}
}

func TestTelegram(t *testing.T) {
	var calls atomic.Int32
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTOKEN123/sendMessage" {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":1}}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()
	tg, err := NewTelegram(TelegramConfig{BotToken: "TOKEN123", ChatIDs: []string{"42"}, APIBase: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tg.Run(ctx); close(done) }()
	tg.Notify("<b>hi</b>")
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	if calls.Load() != 2 || got["chat_id"] != "42" || got["text"] != "<b>hi</b>" || got["parse_mode"] != "HTML" {
		t.Fatalf("calls=%d payload=%v", calls.Load(), got)
	}

	// errors must never contain the bot token
	bad, _ := NewTelegram(TelegramConfig{BotToken: "SECRETTOKEN", ChatIDs: []string{"1"}, APIBase: "http://127.0.0.1:1"})
	err = bad.Send(context.Background(), "1", "x")
	if err == nil || strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Fatalf("token leaked or no error: %v", err)
	}
}

func TestMigrationAddsNoAlert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// simulate a database created before alerts existed
	if _, err := st.db.Exec(`ALTER TABLE nodes DROP COLUMN no_alert`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AddNode(Node{ID: "x", Name: "x", Secret: "s", ResetDay: 1, NoAlert: true}); err != nil {
		t.Fatal(err)
	}
	n, err := st.GetNode("x")
	if err != nil || !n.NoAlert {
		t.Fatalf("migration failed: %+v %v", n, err)
	}
}
