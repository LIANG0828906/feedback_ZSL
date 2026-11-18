package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Mapping struct {
	GatewayID       string
	Protocol        string
	ExternalPort    int
	BoundIP         string
	BoundPort       int
	DeclaredNATType string
	MeasuredNATType string
	TypeMismatch    bool
	ProbeEvidence   string
	MappingToken    string
	CreatedAt       time.Time
	LastHeartbeat   time.Time
	LeaseExpires    time.Time
	Recovered       bool
}

type Recycle struct {
	ID            int64
	GatewayID     string
	Protocol      string
	ExternalPort  int
	Reason        string
	RecycledAt    time.Time
	CooldownUntil time.Time
}

type GatewayCounters struct {
	GatewayID            string
	SourceMismatchDrops  int64
	NATTypeMismatchCount int64
	RecycleCount         int64
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS mappings (
  gateway_id TEXT NOT NULL,
  protocol TEXT NOT NULL CHECK(protocol IN ('udp','tcp')),
  external_port INTEGER NOT NULL,
  bound_ip TEXT NOT NULL,
  bound_port INTEGER NOT NULL,
  declared_nat_type TEXT NOT NULL,
  measured_nat_type TEXT NOT NULL,
  type_mismatch INTEGER NOT NULL DEFAULT 0,
  probe_evidence TEXT NOT NULL DEFAULT '{}',
  mapping_token TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_heartbeat INTEGER NOT NULL,
  lease_expires INTEGER NOT NULL,
  recovered INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (gateway_id, protocol),
  UNIQUE (protocol, external_port)
);
CREATE TABLE IF NOT EXISTS recycles (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  gateway_id TEXT NOT NULL,
  protocol TEXT NOT NULL,
  external_port INTEGER NOT NULL,
  reason TEXT NOT NULL,
  recycled_at INTEGER NOT NULL,
  cooldown_until INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_recycles_gateway ON recycles(gateway_id, id DESC);
CREATE TABLE IF NOT EXISTS gateway_counters (
  gateway_id TEXT PRIMARY KEY,
  source_mismatch_drops INTEGER NOT NULL DEFAULT 0,
  nat_type_mismatch_count INTEGER NOT NULL DEFAULT 0,
  recycle_count INTEGER NOT NULL DEFAULT 0
);
`
	_, err := s.db.Exec(schema)
	return err
}

func unix(t time.Time) int64      { return t.UnixMilli() }
func fromUnix(ms int64) time.Time { return time.UnixMilli(ms) }

func (s *Store) UpsertMapping(ctx context.Context, m Mapping) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO mappings(gateway_id,protocol,external_port,bound_ip,bound_port,declared_nat_type,
 measured_nat_type,type_mismatch,probe_evidence,mapping_token,created_at,last_heartbeat,lease_expires,recovered)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(gateway_id,protocol) DO UPDATE SET
 external_port=excluded.external_port,
 bound_ip=excluded.bound_ip,
 bound_port=excluded.bound_port,
 declared_nat_type=excluded.declared_nat_type,
 measured_nat_type=excluded.measured_nat_type,
 type_mismatch=excluded.type_mismatch,
 probe_evidence=excluded.probe_evidence,
 last_heartbeat=excluded.last_heartbeat,
 lease_expires=excluded.lease_expires,
 recovered=excluded.recovered`,
		m.GatewayID, m.Protocol, m.ExternalPort, m.BoundIP, m.BoundPort, m.DeclaredNATType,
		m.MeasuredNATType, boolInt(m.TypeMismatch), m.ProbeEvidence, m.MappingToken,
		unix(m.CreatedAt), unix(m.LastHeartbeat), unix(m.LeaseExpires), boolInt(m.Recovered))
	return err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *Store) DeleteMapping(ctx context.Context, gatewayID, proto string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM mappings WHERE gateway_id=? AND protocol=?`, gatewayID, proto)
	return err
}

func (s *Store) LoadMappings(ctx context.Context) ([]Mapping, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT gateway_id,protocol,external_port,bound_ip,bound_port,declared_nat_type,
 measured_nat_type,type_mismatch,probe_evidence,mapping_token,created_at,last_heartbeat,lease_expires,recovered
FROM mappings ORDER BY gateway_id,protocol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mapping
	for rows.Next() {
		var m Mapping
		var mismatch, recovered int
		var created, heartbeat, expires int64
		if err := rows.Scan(&m.GatewayID, &m.Protocol, &m.ExternalPort, &m.BoundIP, &m.BoundPort,
			&m.DeclaredNATType, &m.MeasuredNATType, &mismatch, &m.ProbeEvidence, &m.MappingToken,
			&created, &heartbeat, &expires, &recovered); err != nil {
			return nil, err
		}
		m.TypeMismatch = mismatch != 0
		m.Recovered = recovered != 0
		m.CreatedAt, m.LastHeartbeat, m.LeaseExpires = fromUnix(created), fromUnix(heartbeat), fromUnix(expires)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) AddRecycle(ctx context.Context, r Recycle) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO recycles(gateway_id,protocol,external_port,reason,recycled_at,cooldown_until)
VALUES(?,?,?,?,?,?)`, r.GatewayID, r.Protocol, r.ExternalPort, r.Reason, unix(r.RecycledAt), unix(r.CooldownUntil)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO gateway_counters(gateway_id,recycle_count) VALUES(?,1)
ON CONFLICT(gateway_id) DO UPDATE SET recycle_count=recycle_count+1`, r.GatewayID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LastRecycle(ctx context.Context, gatewayID string) (Recycle, bool, error) {
	var r Recycle
	var at, until int64
	err := s.db.QueryRowContext(ctx, `
SELECT id,gateway_id,protocol,external_port,reason,recycled_at,cooldown_until
FROM recycles WHERE gateway_id=? ORDER BY id DESC LIMIT 1`, gatewayID).
		Scan(&r.ID, &r.GatewayID, &r.Protocol, &r.ExternalPort, &r.Reason, &at, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r.RecycledAt, r.CooldownUntil = fromUnix(at), fromUnix(until)
	return r, true, nil
}

func (s *Store) Cooldowns(ctx context.Context, now time.Time) (map[int]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT external_port,cooldown_until FROM recycles WHERE cooldown_until>?`, unix(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int]time.Time)
	for rows.Next() {
		var p int
		var until int64
		if err := rows.Scan(&p, &until); err != nil {
			return nil, err
		}
		out[p] = fromUnix(until)
	}
	return out, rows.Err()
}

func (s *Store) GetCounters(ctx context.Context, gatewayID string) (GatewayCounters, error) {
	c := GatewayCounters{GatewayID: gatewayID}
	err := s.db.QueryRowContext(ctx, `
SELECT source_mismatch_drops,nat_type_mismatch_count,recycle_count
FROM gateway_counters WHERE gateway_id=?`, gatewayID).
		Scan(&c.SourceMismatchDrops, &c.NATTypeMismatchCount, &c.RecycleCount)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	return c, err
}

func (s *Store) LoadCounters(ctx context.Context) (map[string]GatewayCounters, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT gateway_id,source_mismatch_drops,nat_type_mismatch_count,recycle_count FROM gateway_counters`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]GatewayCounters)
	for rows.Next() {
		var c GatewayCounters
		if err := rows.Scan(&c.GatewayID, &c.SourceMismatchDrops, &c.NATTypeMismatchCount, &c.RecycleCount); err != nil {
			return nil, err
		}
		out[c.GatewayID] = c
	}
	return out, rows.Err()
}

func (s *Store) IncrementSourceMismatch(ctx context.Context, gatewayID string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO gateway_counters(gateway_id,source_mismatch_drops) VALUES(?,1)
ON CONFLICT(gateway_id) DO UPDATE SET source_mismatch_drops=source_mismatch_drops+1`, gatewayID)
	return err
}

func (s *Store) IncrementNATMismatch(ctx context.Context, gatewayID string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO gateway_counters(gateway_id,nat_type_mismatch_count) VALUES(?,1)
ON CONFLICT(gateway_id) DO UPDATE SET nat_type_mismatch_count=nat_type_mismatch_count+1`, gatewayID)
	return err
}

func (s *Store) ListRecycles(ctx context.Context, gatewayID string) ([]Recycle, error) {
	query := `SELECT id,gateway_id,protocol,external_port,reason,recycled_at,cooldown_until FROM recycles`
	args := []any{}
	if gatewayID != "" {
		query += ` WHERE gateway_id=?`
		args = append(args, gatewayID)
	}
	query += ` ORDER BY gateway_id,id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recycle
	for rows.Next() {
		var r Recycle
		var at, until int64
		if err := rows.Scan(&r.ID, &r.GatewayID, &r.Protocol, &r.ExternalPort, &r.Reason, &at, &until); err != nil {
			return nil, err
		}
		r.RecycledAt, r.CooldownUntil = fromUnix(at), fromUnix(until)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Ping(ctx context.Context) error {
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return err
	}
	if one != 1 {
		return fmt.Errorf("sqlite ping returned %d", one)
	}
	return nil
}
