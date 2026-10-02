// Package server receives signed agent reports and serves the dashboard.
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lightmani/serverstatus/internal/auth"
	"github.com/lightmani/serverstatus/internal/protocol"
	"github.com/lightmani/serverstatus/internal/web"
)

type Server struct {
	mu       sync.RWMutex
	cfg      *Config
	verifier *auth.Verifier
	store    *Store
	now      func() time.Time
	logger   *log.Logger

	rejectMu   sync.Mutex
	rejectSeen map[string]time.Time // throttles rejection logs
}

func New(cfg *Config, logger *log.Logger) *Server {
	return &Server{
		cfg:        cfg,
		verifier:   auth.NewVerifier(cfg.PublicKeys(), time.Duration(cfg.MaxClockSkewSeconds)*time.Second),
		store:      NewStore(),
		now:        time.Now,
		logger:     logger,
		rejectSeen: make(map[string]time.Time),
	}
}

// Reload applies a new config: client list, keys, dashboard auth and display
// options take effect immediately. Listen address, TLS and clock skew
// require a restart.
func (s *Server) Reload(cfg *Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	s.verifier.SetKeys(cfg.PublicKeys())
	keep := make(map[string]bool, len(cfg.Clients))
	for _, c := range cfg.Clients {
		keep[c.ID] = true
	}
	s.store.Retain(keep)
}

func (s *Server) config() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/report", s.handleReport)
	mux.Handle("GET /api/servers", s.dashboardAuth(http.HandlerFunc(s.handleServers)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.Handle("GET /", s.dashboardAuth(http.FileServerFS(web.Static())))
	return securityHeaders(mux)
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, protocol.MaxBodyBytes))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	ip := s.clientIP(r)

	id, err := s.verifier.Verify(r.Header, auth.PurposeReport, body)
	if err != nil {
		s.logReject(ip, r.Header.Get(auth.HeaderClientID), err)
		msg := "unauthorized"
		if errors.Is(err, auth.ErrBadTimestamp) {
			// Not secret, and the most common misconfiguration: tell the
			// agent so the operator can fix NTP.
			msg = "timestamp outside allowed window; check the agent's clock"
		}
		http.Error(w, msg, http.StatusUnauthorized)
		return
	}

	var rep protocol.Report
	if err := json.Unmarshal(body, &rep); err != nil {
		http.Error(w, "invalid report", http.StatusBadRequest)
		return
	}
	rep.Sanitize()
	s.store.Update(id, &rep, ip, s.now())
	w.WriteHeader(http.StatusNoContent)
}

// logReject logs a rejected report at most once per minute per source,
// client id and reason, so a misconfigured agent or a scanner cannot flood
// the log.
func (s *Server) logReject(ip, clientID string, err error) {
	now := s.now()
	key := ip + "|" + clientID + "|" + err.Error()
	s.rejectMu.Lock()
	if now.Sub(s.rejectSeen[key]) < time.Minute {
		s.rejectMu.Unlock()
		return
	}
	if len(s.rejectSeen) > 4096 {
		clear(s.rejectSeen)
	}
	s.rejectSeen[key] = now
	s.rejectMu.Unlock()
	if len(clientID) > 64 {
		clientID = clientID[:64]
	}
	s.logger.Printf("rejected report from %s (client %q): %v", ip, clientID, err)
}

type nodeView struct {
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	Location string           `json:"location"`
	Online   bool             `json:"online"`
	LastSeen int64            `json:"last_seen"` // unix seconds, 0 = never
	IP       string           `json:"ip,omitempty"`
	Report   *protocol.Report `json:"report"`
	History  []Sample         `json:"history"`
}

func (s *Server) handleServers(w http.ResponseWriter, _ *http.Request) {
	cfg := s.config()
	now := s.now()
	offlineAfter := time.Duration(cfg.OfflineAfterSeconds) * time.Second

	views := make([]nodeView, 0, len(cfg.Clients))
	for _, c := range cfg.Clients {
		if c.Disabled {
			continue
		}
		v := nodeView{ID: c.ID, Name: c.Name, Location: c.Location, History: []Sample{}}
		if st, ok := s.store.Get(c.ID); ok {
			v.LastSeen = st.LastSeen.Unix()
			v.Online = now.Sub(st.LastSeen) <= offlineAfter
			v.Report = st.Report
			v.History = st.History
			if cfg.ShowIP {
				v.IP = st.IP
			}
		}
		views = append(views, v)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"now":     now.Unix(),
		"servers": views,
	})
}

func (s *Server) dashboardAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := s.config().Dashboard
		if d.Username == "" {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || !constantTimeEqual(user, d.Username) || !constantTimeEqual(pass, d.Password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="ServerStatus", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// constantTimeEqual compares hashes so neither content nor length leaks
// through timing.
func constantTimeEqual(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func (s *Server) clientIP(r *http.Request) string {
	if s.config().TrustProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			// The right-most entry is the one appended by our own proxy;
			// earlier entries are client-controlled.
			if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
