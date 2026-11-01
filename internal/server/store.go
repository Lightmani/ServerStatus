package server

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Resolutions stored in the metrics table (bucket width in seconds).
const (
	ResMinute = 60
	ResHour   = 3600
)

// Node is a registered VPS.
type Node struct {
	ID           string
	Name         string
	Secret       string
	Group        string
	Location     string
	Sort         int
	Hidden       bool
	ResetDay     int    // day of month the traffic cycle resets (1-31)
	TrafficLimit uint64 // bytes per cycle, 0 = unlimited
	CreatedAt    int64
	HostJSON     string
	LastSeen     int64
	LastRx       uint64
	LastTx       uint64
	LastBoot     int64
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS nodes (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL,
	secret        TEXT NOT NULL,
	grp           TEXT NOT NULL DEFAULT '',
	location      TEXT NOT NULL DEFAULT '',
	sort          INTEGER NOT NULL DEFAULT 0,
	hidden        INTEGER NOT NULL DEFAULT 0,
	reset_day     INTEGER NOT NULL DEFAULT 1,
	traffic_limit INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL,
	host_json     TEXT NOT NULL DEFAULT '',
	last_seen     INTEGER NOT NULL DEFAULT 0,
	last_rx       INTEGER NOT NULL DEFAULT 0,
	last_tx       INTEGER NOT NULL DEFAULT 0,
	last_boot     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS metrics (
	node_id     TEXT NOT NULL,
	res         INTEGER NOT NULL,
	ts          INTEGER NOT NULL,
	samples     INTEGER NOT NULL,
	minutes     INTEGER NOT NULL,
	cpu_sum     REAL NOT NULL,
	cpu_max     REAL NOT NULL,
	mem_sum     REAL NOT NULL,
	mem_max     INTEGER NOT NULL,
	mem_total   INTEGER NOT NULL,
	swap_sum    REAL NOT NULL,
	disk_sum    REAL NOT NULL,
	disk_total  INTEGER NOT NULL,
	load_sum    REAL NOT NULL,
	load_max    REAL NOT NULL,
	rx          INTEGER NOT NULL,
	tx          INTEGER NOT NULL,
	rx_rate_max INTEGER NOT NULL,
	tx_rate_max INTEGER NOT NULL,
	tcp_sum     REAL NOT NULL,
	proc_sum    REAL NOT NULL,
	PRIMARY KEY (node_id, res, ts)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS metrics_res_ts ON metrics(res, ts);
CREATE TABLE IF NOT EXISTS kv (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);
`

// OpenStore opens (and creates) the database at path.
func OpenStore(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const nodeCols = `id, name, secret, grp, location, sort, hidden, reset_day, traffic_limit, created_at, host_json, last_seen, last_rx, last_tx, last_boot`

type scanner interface{ Scan(...any) error }

func scanNode(r scanner) (Node, error) {
	var n Node
	var hidden int
	var limit, rx, tx int64
	err := r.Scan(&n.ID, &n.Name, &n.Secret, &n.Group, &n.Location, &n.Sort, &hidden,
		&n.ResetDay, &limit, &n.CreatedAt, &n.HostJSON, &n.LastSeen, &rx, &tx, &n.LastBoot)
	n.Hidden = hidden != 0
	n.TrafficLimit, n.LastRx, n.LastTx = uint64(limit), uint64(rx), uint64(tx)
	return n, err
}

// ListNodes returns all nodes ordered for display.
func (s *Store) ListNodes() ([]Node, error) {
	rows, err := s.db.Query(`SELECT ` + nodeCols + ` FROM nodes ORDER BY sort, grp, name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ErrNotFound is returned for unknown nodes.
var ErrNotFound = errors.New("node not found")

func (s *Store) GetNode(id string) (Node, error) {
	n, err := scanNode(s.db.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrNotFound
	}
	return n, err
}

func (s *Store) AddNode(n Node) error {
	_, err := s.db.Exec(`INSERT INTO nodes (id, name, secret, grp, location, sort, hidden, reset_day, traffic_limit, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.Name, n.Secret, n.Group, n.Location, n.Sort, boolInt(n.Hidden), n.ResetDay, int64(n.TrafficLimit), n.CreatedAt)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("node id %q already exists", n.ID)
	}
	return err
}

// UpdateNodeMeta updates the user-editable fields of a node.
func (s *Store) UpdateNodeMeta(n Node) error {
	res, err := s.db.Exec(`UPDATE nodes SET name=?, secret=?, grp=?, location=?, sort=?, hidden=?, reset_day=?, traffic_limit=? WHERE id=?`,
		n.Name, n.Secret, n.Group, n.Location, n.Sort, boolInt(n.Hidden), n.ResetDay, int64(n.TrafficLimit), n.ID)
	if err != nil {
		return err
	}
	if c, _ := res.RowsAffected(); c == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteNode removes a node and all its metrics.
func (s *Store) DeleteNode(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM nodes WHERE id=?`, id)
	if err != nil {
		return err
	}
	if c, _ := res.RowsAffected(); c == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM metrics WHERE node_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

const upsertMetric = `INSERT INTO metrics (node_id, res, ts, samples, minutes, cpu_sum, cpu_max, mem_sum, mem_max, mem_total,
	swap_sum, disk_sum, disk_total, load_sum, load_max, rx, tx, rx_rate_max, tx_rate_max, tcp_sum, proc_sum)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (node_id, res, ts) DO UPDATE SET
	samples = samples + excluded.samples,
	minutes = min(minutes + excluded.minutes, excluded.res / 60),
	cpu_sum = cpu_sum + excluded.cpu_sum,
	cpu_max = max(cpu_max, excluded.cpu_max),
	mem_sum = mem_sum + excluded.mem_sum,
	mem_max = max(mem_max, excluded.mem_max),
	mem_total = excluded.mem_total,
	swap_sum = swap_sum + excluded.swap_sum,
	disk_sum = disk_sum + excluded.disk_sum,
	disk_total = excluded.disk_total,
	load_sum = load_sum + excluded.load_sum,
	load_max = max(load_max, excluded.load_max),
	rx = rx + excluded.rx,
	tx = tx + excluded.tx,
	rx_rate_max = max(rx_rate_max, excluded.rx_rate_max),
	tx_rate_max = max(tx_rate_max, excluded.tx_rate_max),
	tcp_sum = tcp_sum + excluded.tcp_sum,
	proc_sum = proc_sum + excluded.proc_sum`

func metricArgs(nodeID string, res int64, b *Bucket) []any {
	return []any{nodeID, res, b.TS, b.Samples, b.Minutes, b.CPUSum, b.CPUMax, b.MemSum, int64(b.MemMax), int64(b.MemTotal),
		b.SwapSum, b.DiskSum, int64(b.DiskTotal), b.LoadSum, b.LoadMax, int64(b.Rx), int64(b.Tx),
		int64(b.RxRateMax), int64(b.TxRateMax), b.TCPSum, b.ProcSum}
}

// flushJob is one finished minute bucket plus the node's latest persistent state.
type flushJob struct {
	NodeID   string
	Minute   Bucket
	HostJSON string
	LastSeen int64
	LastRx   uint64
	LastTx   uint64
	LastBoot int64
}

// SaveFlush writes a minute bucket, folds it into the hour bucket and
// persists the node's counters, all in one transaction.
func (s *Store) SaveFlush(jobs []flushJob) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ins, err := tx.Prepare(upsertMetric)
	if err != nil {
		return err
	}
	defer ins.Close()
	upd, err := tx.Prepare(`UPDATE nodes SET host_json=?, last_seen=?, last_rx=?, last_tx=?, last_boot=? WHERE id=?`)
	if err != nil {
		return err
	}
	defer upd.Close()
	for i := range jobs {
		j := &jobs[i]
		m := j.Minute
		m.Minutes = 1
		if _, err := ins.Exec(metricArgs(j.NodeID, ResMinute, &m)...); err != nil {
			return err
		}
		h := m
		h.TS = m.TS - m.TS%ResHour
		if _, err := ins.Exec(metricArgs(j.NodeID, ResHour, &h)...); err != nil {
			return err
		}
		if _, err := upd.Exec(j.HostJSON, j.LastSeen, int64(j.LastRx), int64(j.LastTx), j.LastBoot, j.NodeID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BucketRow is a stored bucket with its node.
type BucketRow struct {
	NodeID string
	Bucket
}

// QueryBuckets returns buckets of resolution res in [from, to). nodeID "" = all.
func (s *Store) QueryBuckets(nodeID string, res, from, to int64) ([]BucketRow, error) {
	q := `SELECT node_id, ts, samples, minutes, cpu_sum, cpu_max, mem_sum, mem_max, mem_total, swap_sum, disk_sum, disk_total,
		load_sum, load_max, rx, tx, rx_rate_max, tx_rate_max, tcp_sum, proc_sum
		FROM metrics WHERE res = ? AND ts >= ? AND ts < ?`
	args := []any{res, from, to}
	if nodeID != "" {
		q += ` AND node_id = ?`
		args = append(args, nodeID)
	}
	q += ` ORDER BY ts`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BucketRow
	for rows.Next() {
		var r BucketRow
		var memMax, memTotal, diskTotal, rx, tx, rxm, txm int64
		if err := rows.Scan(&r.NodeID, &r.TS, &r.Samples, &r.Minutes, &r.CPUSum, &r.CPUMax, &r.MemSum, &memMax, &memTotal,
			&r.SwapSum, &r.DiskSum, &diskTotal, &r.LoadSum, &r.LoadMax, &rx, &tx, &rxm, &txm, &r.TCPSum, &r.ProcSum); err != nil {
			return nil, err
		}
		r.MemMax, r.MemTotal, r.DiskTotal = uint64(memMax), uint64(memTotal), uint64(diskTotal)
		r.Rx, r.Tx, r.RxRateMax, r.TxRateMax = uint64(rx), uint64(tx), uint64(rxm), uint64(txm)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SumTraffic returns the bytes transferred by a node since `from`.
func (s *Store) SumTraffic(nodeID string, from int64) (rx, tx uint64, err error) {
	var r, t int64
	err = s.db.QueryRow(`SELECT COALESCE(SUM(rx),0), COALESCE(SUM(tx),0) FROM metrics WHERE node_id=? AND res=? AND ts>=?`,
		nodeID, ResHour, from).Scan(&r, &t)
	return uint64(r), uint64(t), err
}

// Cleanup enforces retention and removes orphaned metrics.
func (s *Store) Cleanup(minuteBefore, hourBefore int64) error {
	if _, err := s.db.Exec(`DELETE FROM metrics WHERE res=? AND ts<?`, ResMinute, minuteBefore); err != nil {
		return err
	}
	if hourBefore > 0 {
		if _, err := s.db.Exec(`DELETE FROM metrics WHERE res=? AND ts<?`, ResHour, hourBefore); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`DELETE FROM metrics WHERE node_id NOT IN (SELECT id FROM nodes)`)
	return err
}

// GetKV returns a stored setting ("" if absent).
func (s *Store) GetKV(k string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM kv WHERE k=?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetKV stores a setting; an empty value deletes it.
func (s *Store) SetKV(k, v string) error {
	if v == "" {
		_, err := s.db.Exec(`DELETE FROM kv WHERE k=?`, k)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO kv (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
