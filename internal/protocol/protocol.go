// Package protocol defines the wire format shared by the agent and the server,
// including the HMAC request-signing scheme used to authenticate reports.
package protocol

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ReportPath is the HTTP path agents POST their reports to.
const ReportPath = "/api/report"

// Header names used by the signing scheme.
const (
	HeaderNode      = "X-Probe-Node"
	HeaderTimestamp = "X-Probe-Timestamp"
	HeaderNonce     = "X-Probe-Nonce"
	HeaderSignature = "X-Probe-Signature"
)

const signScheme = "PROBE-HMAC-SHA256"

// HostInfo is the mostly-static description of a machine.
type HostInfo struct {
	Hostname       string `json:"hostname"`
	OS             string `json:"os"`
	Kernel         string `json:"kernel"`
	Arch           string `json:"arch"`
	Virtualization string `json:"virt"`
	CPUModel       string `json:"cpu_model"`
	CPUCores       int    `json:"cpu_cores"`
	BootTime       int64  `json:"boot_time"`
	AgentVersion   string `json:"agent_version"`
}

// State is a point-in-time snapshot of a machine's resource usage.
type State struct {
	CPU       float64 `json:"cpu"`    // busy percent, 0-100
	CPUSteal  float64 `json:"steal"`  // steal percent
	CPUIOWait float64 `json:"iowait"` // iowait percent
	Load1     float64 `json:"load1"`
	Load5     float64 `json:"load5"`
	Load15    float64 `json:"load15"`
	MemUsed   uint64  `json:"mem_used"`
	MemTotal  uint64  `json:"mem_total"`
	SwapUsed  uint64  `json:"swap_used"`
	SwapTotal uint64  `json:"swap_total"`
	DiskUsed  uint64  `json:"disk_used"`
	DiskTotal uint64  `json:"disk_total"`
	NetRx     uint64  `json:"net_rx"`  // cumulative bytes received since boot
	NetTx     uint64  `json:"net_tx"`  // cumulative bytes sent since boot
	RxRate    uint64  `json:"rx_rate"` // bytes per second
	TxRate    uint64  `json:"tx_rate"` // bytes per second
	TCP       int     `json:"tcp"`
	UDP       int     `json:"udp"`
	Procs     int     `json:"procs"`
	Threads   int     `json:"threads"`
	Uptime    uint64  `json:"uptime"` // seconds
}

// Report is the body an agent sends on every interval.
type Report struct {
	Time  int64        `json:"time"` // agent clock, unix seconds
	Host  HostInfo     `json:"host"`
	State State        `json:"state"`
	Pings []PingResult `json:"pings,omitempty"`
}

// PingTarget is a host the agent should probe, pushed by the server.
// Host is "1.2.3.4", "example.com", "[2001:db8::1]" (ICMP echo) or
// "tcp://host:port" (TCP connect time).
type PingTarget struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Host string `json:"host"`
}

// PingResult aggregates the probes to one target since the previous report.
type PingResult struct {
	ID     string  `json:"id"`
	Sent   int     `json:"sent"`
	Recv   int     `json:"recv"`
	RTTSum float64 `json:"rtt_sum"` // milliseconds, over received probes
	RTTMin float64 `json:"rtt_min"`
	RTTMax float64 `json:"rtt_max"`
}

// ReportResponse is returned by the server for every accepted report.
type ReportResponse struct {
	PingTargets  []PingTarget `json:"ping_targets"`
	PingInterval int          `json:"ping_interval"` // seconds
}

// NewSecret returns a random 256-bit secret, base64url encoded.
func NewSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// NewNonce returns a random 128-bit hex nonce.
func NewNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// canonical builds the exact byte string that is signed.
func canonical(method, path, node string, ts int64, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	var sb strings.Builder
	sb.WriteString(signScheme)
	sb.WriteByte('\n')
	sb.WriteString(strings.ToUpper(method))
	sb.WriteByte('\n')
	sb.WriteString(path)
	sb.WriteByte('\n')
	sb.WriteString(node)
	sb.WriteByte('\n')
	sb.WriteString(strconv.FormatInt(ts, 10))
	sb.WriteByte('\n')
	sb.WriteString(nonce)
	sb.WriteByte('\n')
	sb.WriteString(hex.EncodeToString(sum[:]))
	return []byte(sb.String())
}

// Sign computes the hex HMAC-SHA256 signature for a request.
func Sign(secret, method, path, node string, ts int64, nonce string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(canonical(method, path, node, ts, nonce, body))
	return hex.EncodeToString(m.Sum(nil))
}

// SignRequest stamps the auth headers onto req. body must be the exact bytes
// sent. path is the logical endpoint (e.g. ReportPath); it is signed instead
// of req.URL.Path so the server may sit behind a reverse proxy sub-path.
func SignRequest(req *http.Request, path, node, secret string, body []byte, now time.Time) (int64, string) {
	ts := now.Unix()
	nonce := NewNonce()
	req.Header.Set(HeaderNode, node)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, Sign(secret, req.Method, path, node, ts, nonce, body))
	return ts, nonce
}

// responseMethod domain-separates response signatures from request signatures.
const responseMethod = "RESPONSE"

// SignResponse signs a server response bound to the request's timestamp and
// nonce, so agents only accept configuration that really comes from the
// server (and not a replayed older response).
func SignResponse(secret, node string, ts int64, nonce string, body []byte) string {
	return Sign(secret, responseMethod, ReportPath, node, ts, nonce, body)
}

// VerifyResponse checks a response signature produced by SignResponse.
func VerifyResponse(secret, node string, ts int64, nonce string, body []byte, sig string) bool {
	want := SignResponse(secret, node, ts, nonce, body)
	return hmac.Equal([]byte(want), []byte(strings.ToLower(sig)))
}

// Signed holds the auth fields parsed from a request.
type Signed struct {
	Node      string
	Timestamp int64
	Nonce     string
	Signature string
}

// ErrMalformed is returned when auth headers are missing or invalid.
var ErrMalformed = errors.New("malformed auth headers")

// ParseHeaders extracts and syntactically validates auth headers.
func ParseHeaders(h http.Header) (Signed, error) {
	s := Signed{
		Node:      h.Get(HeaderNode),
		Nonce:     h.Get(HeaderNonce),
		Signature: h.Get(HeaderSignature),
	}
	ts, err := strconv.ParseInt(h.Get(HeaderTimestamp), 10, 64)
	if err != nil || s.Node == "" || len(s.Node) > 64 {
		return s, ErrMalformed
	}
	s.Timestamp = ts
	if len(s.Nonce) < 16 || len(s.Nonce) > 64 || !isHex(s.Nonce) {
		return s, ErrMalformed
	}
	if len(s.Signature) != 64 || !isHex(s.Signature) {
		return s, ErrMalformed
	}
	return s, nil
}

// Verify checks the signature in constant time.
func (s Signed) Verify(secret, method, path string, body []byte) bool {
	want := Sign(secret, method, path, s.Node, s.Timestamp, s.Nonce, body)
	return hmac.Equal([]byte(want), []byte(strings.ToLower(s.Signature)))
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
