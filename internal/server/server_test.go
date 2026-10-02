package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lightmani/serverstatus/internal/auth"
	"github.com/lightmani/serverstatus/internal/protocol"
)

type fixture struct {
	srv    *Server
	ts     *httptest.Server
	signer *auth.Signer
}

func newFixture(t *testing.T, mutate func(*Config)) *fixture {
	t.Helper()
	pub, priv, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Clients: []ClientConfig{
		{ID: "hk-1", Name: "Hong Kong", Location: "HK", PublicKey: auth.EncodePublicKey(pub)},
	}}
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, log.New(io.Discard, "", 0))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	signer, _ := auth.NewSigner("hk-1", priv)
	return &fixture{srv: srv, ts: ts, signer: signer}
}

func (f *fixture) post(t *testing.T, body []byte, sign bool) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/api/report", bytes.NewReader(body))
	if sign {
		if err := f.signer.Sign(req.Header, auth.PurposeReport, body); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

type serversResp struct {
	Servers []nodeView `json:"servers"`
}

func (f *fixture) servers(t *testing.T) serversResp {
	t.Helper()
	resp, err := http.Get(f.ts.URL + "/api/servers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out serversResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReportFlow(t *testing.T) {
	f := newFixture(t, nil)

	got := f.servers(t)
	if len(got.Servers) != 1 || got.Servers[0].Online || got.Servers[0].Report != nil {
		t.Fatalf("before report: %+v", got)
	}

	body, _ := json.Marshal(protocol.Report{
		CPU:    150, // out of range, must be clamped
		Memory: protocol.Usage{Total: 100, Used: 40},
		Host:   protocol.HostInfo{Hostname: strings.Repeat("x", 1000)},
	})
	if resp := f.post(t, body, true); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("signed report: %s", resp.Status)
	}

	got = f.servers(t)
	n := got.Servers[0]
	if !n.Online || n.Report == nil || n.Report.CPU != 100 || len(n.Report.Host.Hostname) != 255 {
		t.Fatalf("after report: %+v", n)
	}
	if n.IP != "" {
		t.Fatal("IP exposed while show_ip is off")
	}
	if len(n.History) != 1 || n.History[0].Mem != 40 {
		t.Fatalf("history: %+v", n.History)
	}
}

func TestReportRejected(t *testing.T) {
	f := newFixture(t, nil)
	body := []byte(`{"cpu":1}`)
	if resp := f.post(t, body, false); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned: %s", resp.Status)
	}

	// Replay of a valid request.
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/api/report", bytes.NewReader(body))
	f.signer.Sign(req.Header, auth.PurposeReport, body)
	for i, want := range []int{http.StatusNoContent, http.StatusUnauthorized} {
		r2, _ := http.NewRequest(http.MethodPost, req.URL.String(), bytes.NewReader(body))
		r2.Header = req.Header.Clone()
		resp, err := http.DefaultClient.Do(r2)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("attempt %d: %s", i, resp.Status)
		}
	}

	big := bytes.Repeat([]byte("a"), protocol.MaxBodyBytes+1)
	if resp := f.post(t, big, true); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: %s", resp.Status)
	}
}

func TestOffline(t *testing.T) {
	f := newFixture(t, nil)
	body, _ := json.Marshal(protocol.Report{})
	f.post(t, body, true)
	f.srv.now = func() time.Time { return time.Now().Add(time.Minute) }
	if f.servers(t).Servers[0].Online {
		t.Fatal("stale node reported online")
	}
}

func TestDashboardAuth(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.Dashboard = DashboardAuth{Username: "admin", Password: "s3cret"}
	})
	for _, path := range []string{"/", "/api/servers"} {
		resp, _ := http.Get(f.ts.URL + path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without auth: %s", path, resp.Status)
		}
		req, _ := http.NewRequest(http.MethodGet, f.ts.URL+path, nil)
		req.SetBasicAuth("admin", "s3cret")
		resp, _ = http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s with auth: %s", path, resp.Status)
		}
	}
	// Agents authenticate by signature, not basic auth.
	body, _ := json.Marshal(protocol.Report{})
	if resp := f.post(t, body, true); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("report with dashboard auth on: %s", resp.Status)
	}
}

func TestReloadRevokes(t *testing.T) {
	f := newFixture(t, nil)
	body, _ := json.Marshal(protocol.Report{})
	f.post(t, body, true)

	cfg := *f.srv.config()
	cfg.Clients = []ClientConfig{{ID: "hk-1", PublicKey: cfg.Clients[0].PublicKey, Disabled: true}}
	f.srv.Reload(&cfg)

	if resp := f.post(t, body, true); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled client accepted: %s", resp.Status)
	}
	if n := len(f.servers(t).Servers); n != 0 {
		t.Fatalf("disabled client still listed: %d", n)
	}
}

func TestClientIP(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.TrustProxyHeaders = true })
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	if ip := f.srv.clientIP(r); ip != "203.0.113.9" {
		t.Fatalf("got %s", ip)
	}
}

func TestConfigValidation(t *testing.T) {
	pub, _, _ := auth.GenerateKey()
	key := auth.EncodePublicKey(pub)
	for name, c := range map[string]Config{
		"bad id":    {Clients: []ClientConfig{{ID: "a b", PublicKey: key}}},
		"dup id":    {Clients: []ClientConfig{{ID: "a", PublicKey: key}, {ID: "a", PublicKey: key}}},
		"bad key":   {Clients: []ClientConfig{{ID: "a", PublicKey: "nope"}}},
		"half tls":  {TLSCert: "x"},
		"no passwd": {Dashboard: DashboardAuth{Username: "u"}},
	} {
		if err := c.normalize(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	f := newFixture(t, nil)
	resp, err := http.Get(f.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatal("missing CSP")
	}
}
