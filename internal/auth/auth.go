// Package auth implements request signing between agents and the server.
//
// Every agent owns an Ed25519 key pair. The server only stores public keys,
// so a leaked server config cannot be used to forge reports. Each request is
// signed over a canonical string that binds:
//
//	scheme version | purpose | client id | unix timestamp | random nonce | sha256(body)
//
// The server rejects requests whose timestamp is outside the allowed clock
// skew, and remembers nonces for the length of that window, so a captured
// request can be neither modified nor replayed.
package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	HeaderClientID  = "X-Probe-Client"
	HeaderTimestamp = "X-Probe-Timestamp"
	HeaderNonce     = "X-Probe-Nonce"
	HeaderSignature = "X-Probe-Signature"

	// PurposeReport is the domain-separation label for metric reports.
	PurposeReport = "report"

	schemeVersion = "SSv1"
	nonceBytes    = 16
)

var (
	ErrMissingHeaders = errors.New("missing authentication headers")
	ErrUnknownClient  = errors.New("unknown client")
	ErrBadTimestamp   = errors.New("timestamp outside allowed window")
	ErrBadNonce       = errors.New("malformed nonce")
	ErrReplay         = errors.New("nonce already used")
	ErrBadSignature   = errors.New("invalid signature")
)

var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// ValidClientID reports whether id is an acceptable client identifier.
func ValidClientID(id string) bool { return clientIDPattern.MatchString(id) }

func canonical(purpose, clientID, ts, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{
		schemeVersion, purpose, clientID, ts, nonce, hex.EncodeToString(sum[:]),
	}, "\n"))
}

// Signer adds authentication headers to outgoing agent requests.
type Signer struct {
	clientID string
	key      ed25519.PrivateKey
	rand     io.Reader
	now      func() time.Time
}

func NewSigner(clientID string, key ed25519.PrivateKey) (*Signer, error) {
	if !ValidClientID(clientID) {
		return nil, fmt.Errorf("invalid client id %q (allowed: A-Z a-z 0-9 _ . -, max 64)", clientID)
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid private key")
	}
	return &Signer{clientID: clientID, key: key, rand: rand.Reader, now: time.Now}, nil
}

// Sign sets the authentication headers for a request carrying body.
func (s *Signer) Sign(h http.Header, purpose string, body []byte) error {
	n := make([]byte, nonceBytes)
	if _, err := io.ReadFull(s.rand, n); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	nonce := hex.EncodeToString(n)
	ts := strconv.FormatInt(s.now().Unix(), 10)
	sig := ed25519.Sign(s.key, canonical(purpose, s.clientID, ts, nonce, body))

	h.Set(HeaderClientID, s.clientID)
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderNonce, nonce)
	h.Set(HeaderSignature, base64.StdEncoding.EncodeToString(sig))
	return nil
}

// Verifier checks signed agent requests. It is safe for concurrent use and
// its key set can be replaced at runtime (e.g. on config reload).
type Verifier struct {
	mu      sync.RWMutex
	keys    map[string]ed25519.PublicKey
	maxSkew time.Duration
	now     func() time.Time
	nonces  *nonceCache
}

func NewVerifier(keys map[string]ed25519.PublicKey, maxSkew time.Duration) *Verifier {
	return &Verifier{
		keys:    keys,
		maxSkew: maxSkew,
		now:     time.Now,
		// A timestamp is accepted within ±maxSkew, so a given nonce can be
		// valid for at most 2*maxSkew of wall-clock time.
		nonces: newNonceCache(2*maxSkew + time.Second),
	}
}

// SetKeys atomically replaces the set of trusted client keys.
func (v *Verifier) SetKeys(keys map[string]ed25519.PublicKey) {
	v.mu.Lock()
	v.keys = keys
	v.mu.Unlock()
}

// Verify authenticates a request and returns the verified client id.
// Callers must only trust the identity returned here, never one from the body.
func (v *Verifier) Verify(h http.Header, purpose string, body []byte) (string, error) {
	id := h.Get(HeaderClientID)
	ts := h.Get(HeaderTimestamp)
	nonce := h.Get(HeaderNonce)
	sigB64 := h.Get(HeaderSignature)
	if id == "" || ts == "" || nonce == "" || sigB64 == "" {
		return "", ErrMissingHeaders
	}
	if !ValidClientID(id) {
		return "", ErrUnknownClient
	}

	v.mu.RLock()
	key, ok := v.keys[id]
	v.mu.RUnlock()
	if !ok {
		return "", ErrUnknownClient
	}

	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", ErrBadTimestamp
	}
	if len(nonce) < 2*nonceBytes || len(nonce) > 128 {
		return "", ErrBadNonce
	}
	if _, err := hex.DecodeString(nonce); err != nil {
		return "", ErrBadNonce
	}

	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", ErrBadSignature
	}
	if !ed25519.Verify(key, canonical(purpose, id, ts, nonce, body), sig) {
		return "", ErrBadSignature
	}

	// Checked after the signature so that only key holders learn about
	// clock problems (and nothing about which ids exist).
	now := v.now()
	skew := now.Sub(time.Unix(sec, 0))
	if skew > v.maxSkew || skew < -v.maxSkew {
		return "", ErrBadTimestamp
	}

	// Record the nonce only after the signature checks out, so unauthenticated
	// traffic cannot grow the cache.
	if !v.nonces.add(id+"|"+nonce, now) {
		return "", ErrReplay
	}
	return id, nil
}

type nonceCache struct {
	mu        sync.Mutex
	ttl       time.Duration
	seen      map[string]time.Time // nonce -> expiry
	lastPrune time.Time
}

func newNonceCache(ttl time.Duration) *nonceCache {
	return &nonceCache{ttl: ttl, seen: make(map[string]time.Time)}
}

// add records key and reports whether it was not seen before.
func (c *nonceCache) add(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(c.lastPrune) > c.ttl/2 {
		for k, exp := range c.seen {
			if !now.Before(exp) {
				delete(c.seen, k)
			}
		}
		c.lastPrune = now
	}
	if exp, ok := c.seen[key]; ok && now.Before(exp) {
		return false
	}
	c.seen[key] = now.Add(c.ttl)
	return true
}
