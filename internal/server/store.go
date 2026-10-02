package server

import (
	"sync"
	"time"

	"github.com/lightmani/serverstatus/internal/protocol"
)

// historyLen is the number of samples kept per node for the sparklines
// (about 10 minutes at the default 3s agent interval).
const historyLen = 200

type Sample struct {
	T   int64   `json:"t"`
	CPU float64 `json:"cpu"`
	Mem float64 `json:"mem"`
	Rx  float64 `json:"rx"`
	Tx  float64 `json:"tx"`
}

type nodeState struct {
	Report   *protocol.Report
	LastSeen time.Time
	IP       string
	History  []Sample
}

// Store holds the latest state of every node in memory. State is rebuilt
// from incoming reports within seconds after a restart, so nothing is
// persisted.
type Store struct {
	mu    sync.RWMutex
	nodes map[string]*nodeState
}

func NewStore() *Store {
	return &Store{nodes: make(map[string]*nodeState)}
}

func (s *Store) Update(id string, r *protocol.Report, ip string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[id]
	if n == nil {
		n = &nodeState{}
		s.nodes[id] = n
	}
	n.Report = r
	n.LastSeen = now
	n.IP = ip
	n.History = append(n.History, Sample{
		T:   now.Unix(),
		CPU: r.CPU,
		Mem: r.Memory.Percent(),
		Rx:  r.Network.RxRate,
		Tx:  r.Network.TxRate,
	})
	if len(n.History) > historyLen {
		n.History = n.History[len(n.History)-historyLen:]
	}
}

// Get returns a copy of a node's state, or false if it never reported.
func (s *Store) Get(id string) (nodeState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[id]
	if !ok {
		return nodeState{}, false
	}
	c := *n
	c.History = append([]Sample(nil), n.History...)
	return c, true
}

// Retain drops nodes that are no longer configured.
func (s *Store) Retain(keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.nodes {
		if !keep[id] {
			delete(s.nodes, id)
		}
	}
}
