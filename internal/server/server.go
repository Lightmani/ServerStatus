package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Lightmani/ServerStatus/internal/protocol"
)

//go:embed web
var webFS embed.FS

const (
	sessionCookie = "ss_session"
	sessionTTL    = 30 * 24 * time.Hour
	maxReportBody = 64 << 10
)

// Server is the HTTP front end.
type Server struct {
	cfg       *Config
	store     *Store
	hub       *Hub
	alerter   *Alerter
	guard     *ReplayGuard
	reportLim *Limiter
	loginLim  *Limiter
	version   string

	authMu     sync.RWMutex
	pwHash     string
	sessionKey string
}

// Run starts the server and blocks until ctx is cancelled.
func Run(ctx context.Context, cfg *Config, version string) error {
	store, err := OpenStore(cfg.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	hub, err := NewHub(cfg, store)
	if err != nil {
		return err
	}
	var notifiers []Notifier
	tg, err := NewTelegram(cfg.Telegram)
	if err != nil {
		return err
	}
	if tg != nil {
		notifiers = append(notifiers, tg)
	}
	alerter, err := NewAlerter(cfg, hub, store, notifiers...)
	if err != nil {
		return err
	}
	s := &Server{
		cfg: cfg, store: store, hub: hub, alerter: alerter, version: version,
		guard:     NewReplayGuard(cfg.MaxClockSkew),
		reportLim: NewLimiter(30, 10*time.Minute, 10*time.Minute),
		loginLim:  NewLimiter(5, 15*time.Minute, 15*time.Minute),
	}
	if err := s.refreshAuth(); err != nil {
		return err
	}

	hubCtx, hubCancel := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() { hub.Run(hubCtx); close(hubDone) }()
	go s.maintenance(ctx)
	alertDone := make(chan struct{})
	go func() { alerter.Run(hubCtx); close(alertDone) }()
	tgCtx, tgCancel := context.WithCancel(context.Background())
	tgDone := make(chan struct{})
	if tg != nil {
		go func() { tg.Run(tgCtx); close(tgDone) }()
		log.Printf("telegram notifications enabled for %d chat(s)", len(cfg.Telegram.ChatIDs))
	} else {
		close(tgDone)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("ServerStatus %s listening on %s (tls=%v, db=%s, tz=%s)",
			version, cfg.Listen, cfg.TLSCert != "", cfg.Database, cfg.Location())
		if cfg.TLSCert != "" {
			errCh <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			errCh <- srv.ListenAndServe()
		}
	}()

	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	}
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shCtx)
	// HTTP is drained; now flush buffered metrics.
	hubCancel()
	<-hubDone
	<-alertDone
	tgCancel() // delivers queued notifications for up to 5s
	<-tgDone
	log.Printf("shutdown complete")
	return serveErr
}

func (s *Server) maintenance(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.guard.Sweep(now)
			s.reportLim.Sweep(now)
			s.loginLim.Sweep(now)
			if err := s.refreshAuth(); err != nil {
				log.Printf("refresh auth: %v", err)
			}
		}
	}
}

// refreshAuth picks up password changes made with the CLI.
func (s *Server) refreshAuth() error {
	pw, err := s.store.GetKV(kvPasswordHash)
	if err != nil {
		return err
	}
	key, err := s.store.GetKV(kvSessionKey)
	if err != nil {
		return err
	}
	if key == "" {
		key = NewSessionKey()
		if err := s.store.SetKV(kvSessionKey, key); err != nil {
			return err
		}
	}
	s.authMu.Lock()
	s.pwHash, s.sessionKey = pw, key
	s.authMu.Unlock()
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+protocol.ReportPath, s.handleReport)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/nodes", s.view(s.handleNodes))
	mux.HandleFunc("GET /api/nodes/{id}/history", s.view(s.handleHistory))
	mux.HandleFunc("GET /api/nodes/{id}/stats", s.view(s.handleNodeStats))
	mux.HandleFunc("GET /api/nodes/{id}/ping", s.view(s.handlePingHistory))
	mux.HandleFunc("GET /api/stats", s.view(s.handleStats))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not found")
	})

	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServerFS(sub)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	}))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// helpers

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.RealIPHeader != "" {
		if v := r.Header.Get(s.cfg.RealIPHeader); v != "" {
			first, _, _ := strings.Cut(v, ",")
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// authState returns (password required, request authenticated).
func (s *Server) authState(r *http.Request) (bool, bool) {
	s.authMu.RLock()
	pw, key := s.pwHash, s.sessionKey
	s.authMu.RUnlock()
	if pw == "" {
		return false, false
	}
	c, err := r.Cookie(sessionCookie)
	return true, err == nil && validSession(key, c.Value, time.Now())
}

type viewHandler func(w http.ResponseWriter, r *http.Request, includeHidden bool)

// view guards read endpoints: open when no password is set, otherwise a
// valid session is required (and hidden nodes become visible).
func (s *Server) view(h viewHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		required, ok := s.authState(r)
		if required && !ok {
			writeErr(w, http.StatusUnauthorized, "login required")
			return
		}
		h(w, r, required && ok)
	}
}

// ---------------------------------------------------------------------------
// agent endpoint

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	ip := s.clientIP(r)
	if s.reportLim.Blocked(ip, now) {
		writeErr(w, http.StatusTooManyRequests, "too many failed attempts")
		return
	}
	signed, err := protocol.ParseHeaders(r.Header)
	if err != nil {
		s.reportLim.Fail(ip, now)
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxReportBody))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	secret, ok := s.hub.Secret(signed.Node)
	if !ok {
		s.reportLim.Fail(ip, now)
		log.Printf("report from %s: unknown node %q", ip, signed.Node)
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch err := s.guard.Check(signed, secret, r.Method, protocol.ReportPath, body, now); err {
	case nil:
	case errSkew:
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "clock skew too large", "server_time": now.Unix()})
		return
	case errReplay:
		log.Printf("report from %s: replayed nonce for node %s", ip, signed.Node)
		writeErr(w, http.StatusUnauthorized, "replay detected")
		return
	default:
		s.reportLim.Fail(ip, now)
		log.Printf("report from %s: bad signature for node %s", ip, signed.Node)
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var rep protocol.Report
	if err := json.Unmarshal(body, &rep); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	sanitizeReport(&rep)
	if err := s.hub.Ingest(signed.Node, &rep, now); err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// The response carries the agent's configuration; it is signed and bound
	// to this request's nonce so it cannot be forged or replayed.
	out, _ := json.Marshal(protocol.ReportResponse{
		PingTargets:  s.hub.PingTargetsFor(signed.Node),
		PingInterval: int(s.cfg.Ping.Interval / time.Second),
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(protocol.HeaderSignature, protocol.SignResponse(secret, signed.Node, signed.Timestamp, signed.Nonce, out))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

func clampF(v, lo, hi float64) float64 {
	if v != v || v < lo { // NaN
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func sanitizeReport(r *protocol.Report) {
	h := &r.Host
	h.Hostname = trunc(h.Hostname, 128)
	h.OS = trunc(h.OS, 128)
	h.Kernel = trunc(h.Kernel, 128)
	h.Arch = trunc(h.Arch, 32)
	h.Virtualization = trunc(h.Virtualization, 32)
	h.CPUModel = trunc(h.CPUModel, 128)
	h.AgentVersion = trunc(h.AgentVersion, 32)
	st := &r.State
	st.CPU = clampF(st.CPU, 0, 100)
	st.CPUSteal = clampF(st.CPUSteal, 0, 100)
	st.CPUIOWait = clampF(st.CPUIOWait, 0, 100)
	st.Load1 = clampF(st.Load1, 0, 1e6)
	st.Load5 = clampF(st.Load5, 0, 1e6)
	st.Load15 = clampF(st.Load15, 0, 1e6)
}

// ---------------------------------------------------------------------------
// dashboard endpoints

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	required, ok := s.authState(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"title":         s.cfg.Title,
		"version":       s.version,
		"timezone":      s.cfg.Location().String(),
		"auth_required": required,
		"authed":        ok,
		"offline_after": int(s.cfg.OfflineAfter / time.Second),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	ip := s.clientIP(r)
	if s.loginLim.Blocked(ip, now) {
		writeErr(w, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	s.authMu.RLock()
	pw, key := s.pwHash, s.sessionKey
	s.authMu.RUnlock()
	if pw == "" {
		writeErr(w, http.StatusBadRequest, "no password configured")
		return
	}
	if !CheckPassword(pw, req.Password) {
		s.loginLim.Fail(ip, now)
		log.Printf("dashboard login failed from %s", ip)
		writeErr(w, http.StatusUnauthorized, "密码错误")
		return
	}
	exp := now.Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: makeSession(key, exp), Path: "/", Expires: exp,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func sortViews(v []NodeView) {
	sort.Slice(v, func(i, j int) bool {
		a, b := v[i], v[j]
		if a.Sort != b.Sort {
			return a.Sort < b.Sort
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request, hidden bool) {
	now := time.Now()
	alerts := s.alerter.Active(hidden)
	// mark targets with a firing alert (an outage outranks degradation)
	firing := map[string]string{}
	for _, al := range alerts {
		if k := al.NodeID + "\x00" + al.TargetID; al.TargetID != "" && firing[k] != AlertPingLoss {
			firing[k] = al.Kind
		}
	}
	nodes := s.hub.Snapshot(now, hidden)
	for i := range nodes {
		for j := range nodes[i].Pings {
			nodes[i].Pings[j].Alert = firing[nodes[i].ID+"\x00"+nodes[i].Pings[j].ID]
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"now":    now.Unix(),
		"nodes":  nodes,
		"alerts": alerts,
	})
}

func (s *Server) handlePingHistory(w http.ResponseWriter, r *http.Request, hidden bool) {
	id := r.PathValue("id")
	if _, ok := s.hub.nodeMeta(id, hidden); !ok {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	dur, res, step, ok := historyRange(r.URL.Query().Get("range"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "range must be one of 1h,6h,24h,7d,30d,90d,1y")
		return
	}
	now := time.Now()
	from := floorLocal(now.Add(-dur).Unix(), step, s.cfg.Location())
	rows, err := s.store.QueryPing(id, res, from, now.Unix()+1)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, buildPingHistory(rows, s.hub.targetDefsFor(id), step, s.cfg.Location()))
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request, hidden bool) {
	id := r.PathValue("id")
	if _, ok := s.hub.nodeMeta(id, hidden); !ok {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	dur, res, step, ok := historyRange(r.URL.Query().Get("range"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "range must be one of 1h,6h,24h,7d,30d,90d,1y")
		return
	}
	now := time.Now()
	from := floorLocal(now.Add(-dur).Unix(), step, s.cfg.Location())
	rows, err := s.store.QueryBuckets(id, res, from, now.Unix()+1)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, buildHistory(downsample(rows, step, s.cfg.Location()), step))
}

func parsePeriod(r *http.Request) (string, error) {
	p := r.URL.Query().Get("period")
	if p == "" {
		p = PeriodDay
	}
	if !validPeriod(p) {
		return "", errors.New("period must be day, week or month")
	}
	return p, nil
}

func queryInt(r *http.Request, key string, def, lo, hi int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	return max(lo, min(hi, v))
}

func (s *Server) handleNodeStats(w http.ResponseWriter, r *http.Request, hidden bool) {
	id := r.PathValue("id")
	meta, ok := s.hub.nodeMeta(id, hidden)
	if !ok {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	kind, err := parsePeriod(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	count := queryInt(r, "count", 14, 1, 400)
	now := time.Now()
	periods := buildPeriods(kind, now.In(s.cfg.Location()), count, 0)
	rows, err := s.store.QueryBuckets(id, ResHour, periods[0].start.Unix(), periods[len(periods)-1].end.Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"period": kind,
		"stats":  aggregatePeriods(rows, periods, meta.CreatedAt, now),
	})
}

type nodeStat struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Group    string     `json:"group"`
	Location string     `json:"location"`
	Stat     PeriodStat `json:"stat"`
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, hidden bool) {
	kind, err := parsePeriod(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	offset := queryInt(r, "offset", 0, 0, 5000)
	now := time.Now()
	periods := buildPeriods(kind, now.In(s.cfg.Location()), 1, offset)
	p := periods[0]
	rows, err := s.store.QueryBuckets("", ResHour, p.start.Unix(), p.end.Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	byNode := map[string][]BucketRow{}
	for _, row := range rows {
		byNode[row.NodeID] = append(byNode[row.NodeID], row)
	}
	metas := s.hub.visibleMetas(hidden)
	sort.Slice(metas, func(i, j int) bool {
		if metas[i].Sort != metas[j].Sort {
			return metas[i].Sort < metas[j].Sort
		}
		if metas[i].Group != metas[j].Group {
			return metas[i].Group < metas[j].Group
		}
		return metas[i].Name < metas[j].Name
	})
	out := make([]nodeStat, 0, len(metas))
	var total Bucket
	for _, m := range metas {
		for i := range byNode[m.ID] {
			b := byNode[m.ID][i].Bucket
			total.Rx += b.Rx
			total.Tx += b.Tx
		}
		out = append(out, nodeStat{ID: m.ID, Name: m.Name, Group: m.Group, Location: m.Location,
			Stat: aggregatePeriods(byNode[m.ID], periods, m.CreatedAt, now)[0]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"period": kind,
		"offset": offset,
		"start":  p.start.Unix(),
		"end":    p.end.Unix(),
		"label":  p.label,
		"nodes":  out,
		"total":  map[string]uint64{"rx": total.Rx, "tx": total.Tx},
	})
}
