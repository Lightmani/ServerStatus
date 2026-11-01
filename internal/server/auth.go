package server

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Lightmani/ServerStatus/internal/protocol"
)

// ---------------------------------------------------------------------------
// Agent report authentication: HMAC-SHA256 + timestamp window + nonce cache.

var (
	errSkew   = errors.New("timestamp outside allowed window")
	errReplay = errors.New("nonce already used")
	errSig    = errors.New("bad signature")
)

// ReplayGuard remembers nonces for the duration of the clock-skew window.
// Any request older than the window is rejected by timestamp; any request
// within the window is rejected if its nonce was seen, so a captured request
// can never be accepted twice.
type ReplayGuard struct {
	mu     sync.Mutex
	window time.Duration
	seen   map[string]int64 // node+nonce -> expiry (unix)
}

func NewReplayGuard(window time.Duration) *ReplayGuard {
	return &ReplayGuard{window: window, seen: map[string]int64{}}
}

// Check validates a signed request. secretFor looks up the node secret.
func (g *ReplayGuard) Check(s protocol.Signed, secret, method, path string, body []byte, now time.Time) error {
	skew := now.Unix() - s.Timestamp
	w := int64(g.window / time.Second)
	if skew > w || skew < -w {
		return errSkew
	}
	// verify the MAC before touching the nonce cache so that unauthenticated
	// requests cannot fill it
	if !s.Verify(secret, method, path, body) {
		return errSig
	}
	key := s.Node + "\x00" + strings.ToLower(s.Nonce)
	g.mu.Lock()
	defer g.mu.Unlock()
	if exp, ok := g.seen[key]; ok && exp >= now.Unix() {
		return errReplay
	}
	g.seen[key] = s.Timestamp + w + 1
	return nil
}

// Sweep drops expired nonces.
func (g *ReplayGuard) Sweep(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, exp := range g.seen {
		if exp < now.Unix() {
			delete(g.seen, k)
		}
	}
}

// ---------------------------------------------------------------------------
// Per-IP failure limiter used for both report auth and dashboard login.

type Limiter struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	block    time.Duration
	failures map[string]*failRec
}

type failRec struct {
	count        int
	first        time.Time
	blockedUntil time.Time
}

func NewLimiter(maxFails int, window, block time.Duration) *Limiter {
	return &Limiter{max: maxFails, window: window, block: block, failures: map[string]*failRec{}}
}

// Blocked reports whether ip is currently blocked.
func (l *Limiter) Blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.failures[ip]
	return ok && now.Before(r.blockedUntil)
}

// Fail records a failure for ip.
func (l *Limiter) Fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.failures[ip]
	if !ok || now.Sub(r.first) > l.window {
		r = &failRec{first: now}
		l.failures[ip] = r
	}
	r.count++
	if r.count >= l.max {
		r.blockedUntil = now.Add(l.block)
	}
}

// Sweep removes stale records.
func (l *Limiter) Sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, r := range l.failures {
		if now.Sub(r.first) > l.window && now.After(r.blockedUntil) {
			delete(l.failures, ip)
		}
	}
}

// ---------------------------------------------------------------------------
// Dashboard password (PBKDF2-SHA256) and signed session cookies.

const (
	kvPasswordHash = "dashboard_password"
	kvSessionKey   = "session_key"
	pbkdf2Iter     = 600000
)

// HashPassword returns "pbkdf2-sha256$iter$salt$hash".
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(dk)), nil
}

// CheckPassword verifies pw against a hash produced by HashPassword.
func CheckPassword(hash, pw string) bool {
	p := strings.Split(hash, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(p[2])
	want, err2 := base64.RawStdEncoding.DecodeString(p[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && hmac.Equal(got, want)
}

// NewSessionKey returns a random hex key used to sign session cookies.
func NewSessionKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// makeSession returns a cookie value "expiry.signature".
func makeSession(key string, exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte("session\n" + e))
	return e + "." + hex.EncodeToString(m.Sum(nil))
}

// validSession checks a cookie value produced by makeSession.
func validSession(key, v string, now time.Time) bool {
	if key == "" {
		return false
	}
	e, sig, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(e, 10, 64)
	if err != nil || now.Unix() > exp {
		return false
	}
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte("session\n" + e))
	return hmac.Equal([]byte(hex.EncodeToString(m.Sum(nil))), []byte(sig))
}
