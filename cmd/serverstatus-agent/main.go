// Command serverstatus-agent collects system metrics and reports them to a
// ServerStatus server using HMAC-signed requests.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Lightmani/ServerStatus/internal/collector"
	"github.com/Lightmani/ServerStatus/internal/protocol"
)

var version = "dev"

const defaultConfig = "/etc/serverstatus/agent.yaml"

type config struct {
	Server             string        `yaml:"server"`
	NodeID             string        `yaml:"node_id"`
	Secret             string        `yaml:"secret"`
	Interval           time.Duration `yaml:"interval"`
	NICExclude         string        `yaml:"nic_exclude"`
	InsecureSkipVerify bool          `yaml:"insecure_skip_verify"`
}

func main() {
	cfgPath := flag.String("c", defaultConfig, "config file")
	srv := flag.String("server", "", "server URL, e.g. https://status.example.com (overrides config)")
	id := flag.String("id", "", "node id (overrides config)")
	secretFile := flag.String("secret-file", "", "read the secret from this file (overrides config)")
	interval := flag.Duration("interval", 0, "report interval (overrides config, default 3s)")
	once := flag.Bool("once", false, "collect once, print the report as JSON and exit")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if *once {
		col, err := collector.New(version, "")
		if err != nil {
			log.Fatal(err)
		}
		time.Sleep(time.Second)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(col.Collect())
		return
	}

	cfg := config{Interval: 3 * time.Second}
	explicit := false
	flag.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "c" })
	if b, err := os.ReadFile(*cfgPath); err == nil {
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			log.Fatalf("parse %s: %v", *cfgPath, err)
		}
	} else if explicit || !errors.Is(err, os.ErrNotExist) {
		log.Fatal(err)
	}
	if *srv != "" {
		cfg.Server = *srv
	}
	if *id != "" {
		cfg.NodeID = *id
	}
	if *secretFile != "" {
		b, err := os.ReadFile(*secretFile)
		if err != nil {
			log.Fatal(err)
		}
		cfg.Secret = strings.TrimSpace(string(b))
	}
	if v := os.Getenv("SERVERSTATUS_SECRET"); v != "" {
		cfg.Secret = v
	}
	if *interval > 0 {
		cfg.Interval = *interval
	}
	if cfg.Interval < time.Second {
		cfg.Interval = time.Second
	}
	cfg.Server = strings.TrimRight(cfg.Server, "/")
	if cfg.Server == "" || cfg.NodeID == "" || cfg.Secret == "" {
		log.Fatal("server, node_id and secret are required (config file, flags or SERVERSTATUS_SECRET)")
	}
	if !strings.HasPrefix(cfg.Server, "https://") {
		log.Printf("warning: %s is not HTTPS; reports are authenticated and replay-protected but not encrypted", cfg.Server)
	}

	col, err := collector.New(version, cfg.NICExclude)
	if err != nil {
		log.Fatalf("nic_exclude: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	run(ctx, cfg, col)
}

func run(ctx context.Context, cfg config, col *collector.Collector) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 2
	if cfg.InsecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	url := cfg.Server + protocol.ReportPath

	log.Printf("serverstatus-agent %s reporting to %s as %q every %s", version, cfg.Server, cfg.NodeID, cfg.Interval)
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	failures := 0
	var skew time.Duration
	for {
		err := report(ctx, client, url, cfg, col, skew)
		var se *skewError
		switch {
		case err == nil:
			if failures > 0 {
				log.Printf("reporting recovered after %d failures", failures)
			}
			failures = 0
		case errors.As(err, &se):
			// compensate for a wrong local clock and retry immediately
			skew = time.Until(time.Unix(se.serverTime, 0))
			log.Printf("local clock is off by %s; compensating (please enable NTP)", skew.Round(time.Second))
			continue
		default:
			failures++
			// log the first few failures, then roughly once a minute
			if failures <= 3 || failures%int(max(1, time.Minute/cfg.Interval)) == 0 {
				log.Printf("report failed (%d): %v", failures, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type skewError struct{ serverTime int64 }

func (e *skewError) Error() string { return "clock skew" }

func report(ctx context.Context, client *http.Client, url string, cfg config, col *collector.Collector, skew time.Duration) error {
	body, err := json.Marshal(col.Collect())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "serverstatus-agent/"+version)
	protocol.SignRequest(req, protocol.ReportPath, cfg.NodeID, cfg.Secret, body, time.Now().Add(skew))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	var e struct {
		Error      string `json:"error"`
		ServerTime int64  `json:"server_time"`
	}
	_ = json.Unmarshal(data, &e)
	if resp.StatusCode == http.StatusUnauthorized && e.ServerTime > 0 {
		if d := time.Until(time.Unix(e.ServerTime, 0)) - skew; d > 5*time.Second || d < -5*time.Second {
			return &skewError{serverTime: e.ServerTime}
		}
	}
	if e.Error == "" {
		e.Error = strings.TrimSpace(string(data))
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Error)
}
