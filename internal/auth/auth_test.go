package auth

import (
	"crypto/ed25519"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setup(t *testing.T) (*Signer, *Verifier, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSigner("node-1", priv)
	if err != nil {
		t.Fatal(err)
	}
	v := NewVerifier(map[string]ed25519.PublicKey{"node-1": pub}, time.Minute)
	return s, v, priv
}

func signed(t *testing.T, s *Signer, body []byte) http.Header {
	t.Helper()
	h := http.Header{}
	if err := s.Sign(h, PurposeReport, body); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestVerifyValid(t *testing.T) {
	s, v, _ := setup(t)
	body := []byte(`{"cpu":1}`)
	id, err := v.Verify(signed(t, s, body), PurposeReport, body)
	if err != nil || id != "node-1" {
		t.Fatalf("got %q, %v", id, err)
	}
}

func TestVerifyRejects(t *testing.T) {
	body := []byte(`{"cpu":1}`)
	cases := []struct {
		name   string
		mutate func(h http.Header, v *Verifier) ([]byte, string)
		want   error
	}{
		{"tampered body", func(h http.Header, _ *Verifier) ([]byte, string) {
			return []byte(`{"cpu":99}`), PurposeReport
		}, ErrBadSignature},
		{"other purpose", func(h http.Header, _ *Verifier) ([]byte, string) {
			return body, "other"
		}, ErrBadSignature},
		{"spoofed client id", func(h http.Header, v *Verifier) ([]byte, string) {
			pub2, _, _ := GenerateKey()
			v.SetKeys(map[string]ed25519.PublicKey{"node-1": v.keys["node-1"], "node-2": pub2})
			h.Set(HeaderClientID, "node-2")
			return body, PurposeReport
		}, ErrBadSignature},
		{"unknown client", func(h http.Header, _ *Verifier) ([]byte, string) {
			h.Set(HeaderClientID, "ghost")
			return body, PurposeReport
		}, ErrUnknownClient},
		{"changed timestamp", func(h http.Header, _ *Verifier) ([]byte, string) {
			h.Set(HeaderTimestamp, "1")
			return body, PurposeReport
		}, ErrBadSignature},
		{"non-numeric timestamp", func(h http.Header, _ *Verifier) ([]byte, string) {
			h.Set(HeaderTimestamp, "soon")
			return body, PurposeReport
		}, ErrBadTimestamp},
		{"changed nonce", func(h http.Header, _ *Verifier) ([]byte, string) {
			h.Set(HeaderNonce, "00000000000000000000000000000000")
			return body, PurposeReport
		}, ErrBadSignature},
		{"short nonce", func(h http.Header, _ *Verifier) ([]byte, string) {
			h.Set(HeaderNonce, "abcd")
			return body, PurposeReport
		}, ErrBadNonce},
		{"garbage signature", func(h http.Header, _ *Verifier) ([]byte, string) {
			h.Set(HeaderSignature, "!!")
			return body, PurposeReport
		}, ErrBadSignature},
		{"missing header", func(h http.Header, _ *Verifier) ([]byte, string) {
			h.Del(HeaderSignature)
			return body, PurposeReport
		}, ErrMissingHeaders},
		{"revoked key", func(h http.Header, v *Verifier) ([]byte, string) {
			v.SetKeys(map[string]ed25519.PublicKey{})
			return body, PurposeReport
		}, ErrUnknownClient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, v, _ := setup(t)
			h := signed(t, s, body)
			b, purpose := tc.mutate(h, v)
			if _, err := v.Verify(h, purpose, b); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyReplay(t *testing.T) {
	s, v, _ := setup(t)
	body := []byte(`{}`)
	h := signed(t, s, body)
	if _, err := v.Verify(h, PurposeReport, body); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(h, PurposeReport, body); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay accepted: %v", err)
	}
}

func TestVerifyClockSkew(t *testing.T) {
	s, v, _ := setup(t)
	body := []byte(`{}`)
	base := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		agentOffset time.Duration
		ok          bool
	}{
		{0, true}, {59 * time.Second, true}, {-59 * time.Second, true},
		{61 * time.Second, false}, {-61 * time.Second, false},
	} {
		s.now = func() time.Time { return base.Add(tc.agentOffset) }
		v.now = func() time.Time { return base }
		_, err := v.Verify(signed(t, s, body), PurposeReport, body)
		if (err == nil) != tc.ok {
			t.Errorf("offset %s: err=%v, want ok=%v", tc.agentOffset, err, tc.ok)
		}
	}
}

func TestNonceExpiry(t *testing.T) {
	c := newNonceCache(10 * time.Second)
	t0 := time.Unix(0, 0)
	if !c.add("a", t0) || c.add("a", t0.Add(9*time.Second)) {
		t.Fatal("nonce should be rejected within ttl")
	}
	if !c.add("b", t0.Add(30*time.Second)) {
		t.Fatal("fresh nonce rejected")
	}
	if _, ok := c.seen["a"]; ok {
		t.Fatal("expired nonce not pruned")
	}
}

func TestKeyFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.key")
	pub, priv, _ := GenerateKey()
	if err := WritePrivateKeyFile(path, priv); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivateKeyFile(path, priv); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrite allowed: %v", err)
	}
	got, err := LoadPrivateKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Public().(ed25519.PublicKey).Equal(pub) {
		t.Fatal("key mismatch")
	}
	parsed, err := ParsePublicKey(EncodePublicKey(pub))
	if err != nil || !parsed.Equal(pub) {
		t.Fatal("public key round trip failed")
	}
	os.Chmod(path, 0o644)
	if _, err := LoadPrivateKeyFile(path); err == nil {
		t.Fatal("world-readable key accepted")
	}
}
