package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/lightmani/serverstatus/internal/auth"
)

type Config struct {
	Listen  string `json:"listen"`
	TLSCert string `json:"tls_cert"`
	TLSKey  string `json:"tls_key"`

	// MaxClockSkewSeconds is how far an agent's clock may differ from the
	// server's before its reports are rejected.
	MaxClockSkewSeconds int `json:"max_clock_skew_seconds"`
	// OfflineAfterSeconds marks a node offline when it has not reported
	// for this long.
	OfflineAfterSeconds int `json:"offline_after_seconds"`

	// TrustProxyHeaders takes the client IP from the right-most
	// X-Forwarded-For entry. Only enable behind a reverse proxy you control.
	TrustProxyHeaders bool `json:"trust_proxy_headers"`
	// ShowIP exposes each node's reporting IP on the dashboard.
	ShowIP bool `json:"show_ip"`

	Dashboard DashboardAuth  `json:"dashboard"`
	Clients   []ClientConfig `json:"clients"`
}

// DashboardAuth enables HTTP basic auth on the web UI when Username is set.
type DashboardAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type ClientConfig struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Location  string `json:"location"`
	PublicKey string `json:"public_key"`
	Disabled  bool   `json:"disabled"`
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields() // surface typos instead of silently ignoring them
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) normalize() error {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.MaxClockSkewSeconds <= 0 {
		c.MaxClockSkewSeconds = 60
	}
	if c.OfflineAfterSeconds <= 0 {
		c.OfflineAfterSeconds = 15
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("tls_cert and tls_key must be set together")
	}
	if c.Dashboard.Username != "" && c.Dashboard.Password == "" {
		return errors.New("dashboard.password is required when dashboard.username is set")
	}
	seen := make(map[string]bool)
	for i := range c.Clients {
		cl := &c.Clients[i]
		if !auth.ValidClientID(cl.ID) {
			return fmt.Errorf("clients[%d]: invalid id %q (allowed: A-Z a-z 0-9 _ . -, max 64)", i, cl.ID)
		}
		if seen[cl.ID] {
			return fmt.Errorf("clients[%d]: duplicate id %q", i, cl.ID)
		}
		seen[cl.ID] = true
		if _, err := auth.ParsePublicKey(cl.PublicKey); err != nil {
			return fmt.Errorf("client %q: %w", cl.ID, err)
		}
		if cl.Name == "" {
			cl.Name = cl.ID
		}
	}
	return nil
}

// PublicKeys returns the keys of all enabled clients.
func (c *Config) PublicKeys() map[string]ed25519.PublicKey {
	keys := make(map[string]ed25519.PublicKey, len(c.Clients))
	for _, cl := range c.Clients {
		if cl.Disabled {
			continue
		}
		k, _ := auth.ParsePublicKey(cl.PublicKey) // validated in normalize
		keys[cl.ID] = k
	}
	return keys
}
