package server

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the server configuration (YAML).
type Config struct {
	Listen       string        `yaml:"listen"`
	Database     string        `yaml:"database"`
	Timezone     string        `yaml:"timezone"`
	Title        string        `yaml:"title"`
	PublicURL    string        `yaml:"public_url"`
	TLSCert      string        `yaml:"tls_cert"`
	TLSKey       string        `yaml:"tls_key"`
	RealIPHeader string        `yaml:"real_ip_header"`
	OfflineAfter time.Duration `yaml:"offline_after"`
	MaxClockSkew time.Duration `yaml:"max_clock_skew"`
	Retention    struct {
		MinuteDays int `yaml:"minute_days"`
		HourDays   int `yaml:"hour_days"`
	} `yaml:"retention"`
	Ping struct {
		Interval time.Duration `yaml:"interval"`
	} `yaml:"ping"`
	Alert struct {
		Enabled         bool          `yaml:"enabled"`
		OfflineAfter    time.Duration `yaml:"offline_after"`
		PingLossMinutes int           `yaml:"ping_loss_minutes"`
		NotifyRecovery  bool          `yaml:"notify_recovery"`
	} `yaml:"alert"`
	Telegram TelegramConfig `yaml:"telegram"`

	loc *time.Location
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() *Config {
	c := &Config{
		Listen:       ":8080",
		Database:     "serverstatus.db",
		Timezone:     "Local", // resolved to the system zone
		Title:        "Server Status",
		OfflineAfter: 15 * time.Second,
		MaxClockSkew: 60 * time.Second,
	}
	c.Retention.MinuteDays = 7
	c.Retention.HourDays = 0
	c.Ping.Interval = 2 * time.Second
	c.Alert.Enabled = true
	c.Alert.OfflineAfter = 5 * time.Minute
	c.Alert.PingLossMinutes = 10
	c.Alert.NotifyRecovery = true
	c.Telegram.APIBase = "https://api.telegram.org"
	return c
}

// LoadConfig reads path. If the file does not exist and mustExist is false,
// defaults are returned.
func LoadConfig(path string, mustExist bool) (*Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && !mustExist:
	case err != nil:
		return nil, err
	default:
		if err := yaml.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	return c, c.validate()
}

// resolveLocalZone turns "Local" into an IANA name (needed by the browser
// to render dates in the server's time zone).
func resolveLocalZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok {
			if _, err := time.LoadLocation(name); err == nil {
				return name
			}
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		name := strings.TrimSpace(string(b))
		if _, err := time.LoadLocation(name); err == nil && name != "" {
			return name
		}
	}
	return "UTC"
}

func (c *Config) validate() error {
	if c.Timezone == "" || c.Timezone == "Local" {
		c.Timezone = resolveLocalZone()
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return fmt.Errorf("timezone %q: %w", c.Timezone, err)
	}
	c.loc = loc
	if c.OfflineAfter < 3*time.Second {
		c.OfflineAfter = 3 * time.Second
	}
	if c.MaxClockSkew < 5*time.Second {
		c.MaxClockSkew = 5 * time.Second
	}
	if c.Retention.MinuteDays < 1 {
		c.Retention.MinuteDays = 1
	}
	if c.Retention.HourDays < 0 {
		c.Retention.HourDays = 0
	}
	c.Ping.Interval = max(time.Second, min(c.Ping.Interval, time.Minute))
	if c.Alert.OfflineAfter < c.OfflineAfter {
		c.Alert.OfflineAfter = c.OfflineAfter
	}
	if c.Alert.PingLossMinutes < 1 || c.Alert.PingLossMinutes > pingKeepMinutes-1 {
		return fmt.Errorf("alert.ping_loss_minutes must be between 1 and %d", pingKeepMinutes-1)
	}
	if c.Telegram.APIBase == "" {
		c.Telegram.APIBase = "https://api.telegram.org"
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("tls_cert and tls_key must be set together")
	}
	if c.Database == "" {
		return errors.New("database path is empty")
	}
	return nil
}

// Location returns the configured time zone.
func (c *Config) Location() *time.Location { return c.loc }

// ExampleConfig is printed by `serverstatus-server config-example`.
const ExampleConfig = `# ServerStatus server configuration
listen: ":8080"                      # HTTP(S) listen address
database: "/var/lib/serverstatus/serverstatus.db"
timezone: "Asia/Shanghai"            # day / week / month boundaries; "Local" = system zone
title: "Server Status"
public_url: ""                       # e.g. https://status.example.com (only used in CLI hints)

# Built-in TLS. Leave empty when running behind nginx/caddy.
tls_cert: ""
tls_key: ""

# Header carrying the client IP when behind a reverse proxy, e.g. "X-Real-IP".
# Leave empty when clients connect directly, otherwise IP rate limiting can be spoofed.
real_ip_header: ""

offline_after: 15s                   # mark a node offline after this long without reports
max_clock_skew: 60s                  # max allowed agent clock drift (replay window)

retention:
  minute_days: 7                     # 1-minute resolution data
  hour_days: 0                       # 1-hour resolution data, 0 = keep forever

# Targeted monitoring: targets are managed with ` + "`serverstatus-server target add`" + `
# and pushed to agents automatically.
ping:
  interval: 2s                       # probe interval per target (1s-60s)

alert:
  enabled: true
  offline_after: 5m                  # alert when an agent has not reported for this long
  ping_loss_minutes: 10              # alert when a target lost 100% in each of the last N minutes
  notify_recovery: true              # also notify when the problem is resolved

telegram:
  bot_token: ""                      # from @BotFather; empty = notifications disabled
  chat_ids: []                       # e.g. [123456789, -1001234567890, "@mychannel"]
  api_base: "https://api.telegram.org"   # or your own reverse proxy of the Bot API
  proxy: ""                          # e.g. socks5://127.0.0.1:1080 or http://127.0.0.1:8080
`
