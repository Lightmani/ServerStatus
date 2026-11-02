// Package pinger probes a set of targets periodically (ICMP echo or TCP
// connect) and accumulates loss / latency between reports.
package pinger

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/Lightmani/ServerStatus/internal/protocol"
)

// Target describes how to probe one host.
type Target struct {
	Kind string // "icmp" or "tcp"
	Host string // hostname or IP (icmp), host:port (tcp)
}

// ParseTarget validates a target host string.
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Target{}, errors.New("empty host")
	}
	if strings.HasPrefix(s, "tcp://") {
		u, err := url.Parse(s)
		if err != nil || u.Hostname() == "" || u.Port() == "" {
			return Target{}, fmt.Errorf("invalid tcp target %q, want tcp://host:port", s)
		}
		return Target{Kind: "tcp", Host: net.JoinHostPort(u.Hostname(), u.Port())}, nil
	}
	if strings.Contains(s, "://") || strings.ContainsAny(s, "/ ") {
		return Target{}, fmt.Errorf("invalid host %q", s)
	}
	return Target{Kind: "icmp", Host: strings.Trim(s, "[]")}, nil
}

type acc struct {
	sent, recv    int
	sum, min, max float64
}

func (a *acc) add(rtt time.Duration, ok bool) {
	a.sent++
	if !ok {
		return
	}
	ms := float64(rtt.Microseconds()) / 1000
	if a.recv == 0 || ms < a.min {
		a.min = ms
	}
	if ms > a.max {
		a.max = ms
	}
	a.recv++
	a.sum += ms
}

type task struct {
	def    protocol.PingTarget
	cancel context.CancelFunc
}

// Pinger runs one probe loop per target.
type Pinger struct {
	mu       sync.Mutex
	interval time.Duration
	tasks    map[string]*task
	accs     map[string]*acc
}

// New creates an idle pinger; call Update to give it targets.
func New() *Pinger {
	return &Pinger{tasks: map[string]*task{}, accs: map[string]*acc{}}
}

// Update replaces the target set. Unchanged targets keep running.
func (p *Pinger) Update(targets []protocol.PingTarget, interval time.Duration) {
	interval = max(time.Second, min(interval, time.Minute))
	p.mu.Lock()
	defer p.mu.Unlock()
	restartAll := interval != p.interval
	p.interval = interval
	want := map[string]protocol.PingTarget{}
	for _, t := range targets {
		want[t.ID] = t
	}
	for id, t := range p.tasks {
		if w, ok := want[id]; !ok || w.Host != t.def.Host || restartAll {
			t.cancel()
			delete(p.tasks, id)
			if !ok {
				delete(p.accs, id)
			}
		}
	}
	for id, def := range want {
		if _, running := p.tasks[id]; running {
			continue
		}
		tgt, err := ParseTarget(def.Host)
		if err != nil {
			log.Printf("ping target %s: %v", id, err)
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		p.tasks[id] = &task{def: def, cancel: cancel}
		if p.accs[id] == nil {
			p.accs[id] = &acc{}
		}
		go p.loop(ctx, id, tgt, interval)
	}
}

// Stop cancels all probe loops.
func (p *Pinger) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, t := range p.tasks {
		t.cancel()
		delete(p.tasks, id)
	}
}

// Take returns and resets the accumulated results.
func (p *Pinger) Take() []protocol.PingResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []protocol.PingResult
	for id, a := range p.accs {
		if a.sent == 0 {
			continue
		}
		out = append(out, protocol.PingResult{ID: id, Sent: a.sent, Recv: a.recv, RTTSum: a.sum, RTTMin: a.min, RTTMax: a.max})
		p.accs[id] = &acc{}
	}
	return out
}

// Restore merges results back after a failed report so nothing is lost.
func (p *Pinger) Restore(rs []protocol.PingResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range rs {
		a := p.accs[r.ID]
		if a == nil {
			continue // target was removed meanwhile
		}
		if r.Recv > 0 && (a.recv == 0 || r.RTTMin < a.min) {
			a.min = r.RTTMin
		}
		a.max = max(a.max, r.RTTMax)
		a.sent += r.Sent
		a.recv += r.Recv
		a.sum += r.RTTSum
	}
}

func (p *Pinger) record(id string, rtt time.Duration, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.accs[id]; a != nil {
		a.add(rtt, ok)
	}
}

func (p *Pinger) loop(ctx context.Context, id string, tgt Target, interval time.Duration) {
	timeout := min(interval, 2*time.Second)
	var ip net.IP
	var resolved time.Time
	var seq uint16
	// spread targets over the interval
	b := make([]byte, 1)
	_, _ = rand.Read(b)
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Duration(b[0]) * interval / 256):
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		var rtt time.Duration
		var err error
		if tgt.Kind == "tcp" {
			rtt, err = tcpProbe(ctx, tgt.Host, timeout)
		} else {
			if ip == nil || time.Since(resolved) > 5*time.Minute {
				if r, rerr := resolve(ctx, tgt.Host); rerr == nil {
					ip, resolved = r, time.Now()
				} else if ip == nil {
					err = rerr
				}
			}
			if ip != nil {
				seq++
				rtt, err = icmpProbe(ip, seq, timeout)
			}
		}
		if ctx.Err() != nil {
			return
		}
		p.record(id, rtt, err == nil)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func resolve(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, a := range ips { // prefer IPv4
		if a.IP.To4() != nil {
			return a.IP, nil
		}
	}
	if len(ips) > 0 {
		return ips[0].IP, nil
	}
	return nil, fmt.Errorf("no address for %s", host)
}

func tcpProbe(ctx context.Context, addr string, timeout time.Duration) (time.Duration, error) {
	d := net.Dialer{Timeout: timeout}
	start := time.Now()
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return 0, err
	}
	rtt := time.Since(start)
	c.Close()
	return rtt, nil
}

// unprivileged ICMP ("ping sockets") need net.ipv4.ping_group_range to
// include our group; otherwise fall back to raw sockets (root/CAP_NET_RAW).
// The decision is made once per address family.
var (
	modeV4 atomic.Int32 // 0 unknown, 1 udp, 2 raw
	modeV6 atomic.Int32
	warned sync.Once
)

func listen(v6 bool) (*icmp.PacketConn, bool, error) {
	mode := &modeV4
	udpNet, rawNet, laddr := "udp4", "ip4:icmp", "0.0.0.0"
	if v6 {
		mode = &modeV6
		udpNet, rawNet, laddr = "udp6", "ip6:ipv6-icmp", "::"
	}
	if m := mode.Load(); m != 2 {
		c, err := icmp.ListenPacket(udpNet, laddr)
		if err == nil {
			mode.Store(1)
			return c, true, nil
		}
		if m == 1 {
			return nil, false, err
		}
	}
	c, err := icmp.ListenPacket(rawNet, laddr)
	if err != nil {
		warned.Do(func() {
			log.Printf("ICMP ping unavailable (%v): grant CAP_NET_RAW or set sysctl net.ipv4.ping_group_range", err)
		})
		return nil, false, err
	}
	mode.Store(2)
	return c, false, nil
}

var idCounter atomic.Uint32

func icmpProbe(ip net.IP, seq uint16, timeout time.Duration) (time.Duration, error) {
	v6 := ip.To4() == nil
	c, udp, err := listen(v6)
	if err != nil {
		return 0, err
	}
	defer c.Close()

	token := make([]byte, 16)
	_, _ = rand.Read(token)
	id := int(idCounter.Add(1)&0xffff) ^ int(token[0])<<8
	var typ icmp.Type = ipv4.ICMPTypeEcho
	proto := 1
	if v6 {
		typ, proto = ipv6.ICMPTypeEchoRequest, 58
	}
	msg := icmp.Message{Type: typ, Body: &icmp.Echo{ID: id & 0xffff, Seq: int(seq), Data: token}}
	wb, err := msg.Marshal(nil)
	if err != nil {
		return 0, err
	}
	var dst net.Addr = &net.IPAddr{IP: ip}
	if udp {
		dst = &net.UDPAddr{IP: ip}
	}
	deadline := time.Now().Add(timeout)
	_ = c.SetDeadline(deadline)
	start := time.Now()
	if _, err := c.WriteTo(wb, dst); err != nil {
		return 0, err
	}
	buf := make([]byte, 1500)
	for {
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			return 0, err
		}
		rtt := time.Since(start)
		rm, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil {
			continue
		}
		if rm.Type != ipv4.ICMPTypeEchoReply && rm.Type != ipv6.ICMPTypeEchoReply {
			continue
		}
		echo, ok := rm.Body.(*icmp.Echo)
		// the kernel rewrites the ID for ping sockets, so match on the
		// random payload (plus ID for raw sockets that see every reply)
		if !ok || !bytes.Equal(echo.Data, token) || (!udp && echo.ID != id&0xffff) {
			continue
		}
		return rtt, nil
	}
}
