package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// GenerateKey creates a new Ed25519 key pair for an agent.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// EncodePublicKey returns the base64 form used in the server config.
func EncodePublicKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("public key must be 32 bytes, base64 encoded")
	}
	return ed25519.PublicKey(b), nil
}

// EncodePrivateKey serializes the 32-byte seed as base64.
func EncodePrivateKey(priv ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(priv.Seed())
}

func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, errors.New("private key must be a 32-byte seed, base64 encoded")
	}
	return ed25519.NewKeyFromSeed(b), nil
}

// WritePrivateKeyFile stores the key with mode 0600 and refuses to overwrite
// an existing file, so a stray keygen cannot silently orphan an agent.
func WritePrivateKeyFile(path string, priv ed25519.PrivateKey) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(EncodePrivateKey(priv) + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// LoadPrivateKeyFile reads a key written by WritePrivateKeyFile. It refuses
// keys readable by group or others.
func LoadPrivateKeyFile(path string) (ed25519.PrivateKey, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is accessible by other users (mode %o); run: chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParsePrivateKey(string(b))
}
