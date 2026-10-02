// Command ss-agent collects host metrics and reports them to ss-server.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lightmani/serverstatus/internal/auth"
	"github.com/lightmani/serverstatus/internal/collector"
)

var version = "dev"

const defaultKeyPath = "/etc/serverstatus/agent.key"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("ss-agent: ")

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "pubkey":
		err = pubkey(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage:
  ss-agent keygen [-key PATH]   generate a key pair, print the public key
  ss-agent pubkey [-key PATH]   print the public key of an existing key
  ss-agent run -server URL -id ID [-key PATH] [-interval 3s]
  ss-agent version

Default key path: %s
`, defaultKeyPath)
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	keyPath := fs.String("key", defaultKeyPath, "where to write the private key")
	fs.Parse(args)

	pub, priv, err := auth.GenerateKey()
	if err != nil {
		return err
	}
	if err := auth.WritePrivateKeyFile(*keyPath, priv); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; use `ss-agent pubkey -key %s` to show its public key", *keyPath, *keyPath)
		}
		return err
	}
	fmt.Fprintf(os.Stderr, "private key written to %s (keep it secret)\n", *keyPath)
	fmt.Fprintln(os.Stderr, "add this public key to the server config:")
	fmt.Println(auth.EncodePublicKey(pub))
	return nil
}

func pubkey(args []string) error {
	fs := flag.NewFlagSet("pubkey", flag.ExitOnError)
	keyPath := fs.String("key", defaultKeyPath, "private key file")
	fs.Parse(args)

	priv, err := auth.LoadPrivateKeyFile(*keyPath)
	if err != nil {
		return err
	}
	fmt.Println(auth.EncodePublicKey(priv.Public().(ed25519.PublicKey)))
	return nil
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	server := fs.String("server", os.Getenv("SS_SERVER"), "server base URL, e.g. https://status.example.com (env SS_SERVER)")
	id := fs.String("id", os.Getenv("SS_ID"), "client id as configured on the server (env SS_ID)")
	keyPath := fs.String("key", defaultKeyPath, "private key file")
	interval := fs.Duration("interval", 3*time.Second, "report interval")
	fs.Parse(args)

	if *server == "" || *id == "" {
		return errors.New("-server and -id are required")
	}
	if *interval < time.Second {
		return errors.New("-interval must be at least 1s")
	}
	endpoint, err := reportURL(*server)
	if err != nil {
		return err
	}
	if strings.HasPrefix(endpoint, "http://") {
		log.Printf("warning: %s is plain HTTP; reports are signed but readable in transit. Prefer https.", endpoint)
	}
	priv, err := auth.LoadPrivateKeyFile(*keyPath)
	if err != nil {
		return err
	}
	signer, err := auth.NewSigner(*id, priv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := &agent{
		endpoint:  endpoint,
		signer:    signer,
		collector: collector.New(),
		client:    &http.Client{Timeout: 10 * time.Second},
	}
	log.Printf("reporting to %s as %q every %s", endpoint, *id, *interval)

	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Print("shutting down")
			return nil
		case <-t.C:
			a.tick(ctx)
		}
	}
}

func reportURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid -server URL %q", base)
	}
	return u.String() + "/api/report", nil
}

type agent struct {
	endpoint  string
	signer    *auth.Signer
	collector *collector.Collector
	client    *http.Client
	failures  int
}

func (a *agent) tick(ctx context.Context) {
	err := a.report(ctx)
	switch {
	case err == nil:
		if a.failures > 0 {
			log.Printf("recovered after %d failed reports", a.failures)
		}
		a.failures = 0
	case ctx.Err() != nil:
	default:
		a.failures++
		// Log the first failure and then periodically, not every tick.
		if a.failures == 1 || a.failures%20 == 0 {
			log.Printf("report failed (%d in a row): %v", a.failures, err)
		}
	}
}

func (a *agent) report(ctx context.Context) error {
	rep, err := a.collector.Collect()
	if err != nil {
		return err
	}
	rep.AgentVersion = version
	body, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ss-agent/"+version)
	if err := a.signer.Sign(req.Header, auth.PurposeReport, body); err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}
