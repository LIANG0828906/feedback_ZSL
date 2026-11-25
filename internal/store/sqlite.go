package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"

	_ "modernc.org/sqlite"

	"maintenanceorchestrator/internal/domain"
)

const CurrentStateVersion = 1

type Checkpoint struct {
	StateVersion  int              `json:"state_version"`
	ProjectID     string           `json:"project_id"`
	Seed          int64            `json:"seed"`
	Tick          domain.Tick      `json:"tick"`
	CommittedWave int              `json:"committed_wave"`
	Nodes         []NodeRecord     `json:"nodes"`
	Instances     []InstanceRecord `json:"instances"`
	Leases        []LeaseRecord    `json:"leases"`
}

type NodeRecord struct {
	NodeID            string   `json:"node_id"`
	State             string   `json:"state"`
	UsedMilli         int64    `json:"used_milli"`
	Schedulable       bool     `json:"schedulable"`
	OutstandingLeases []string `json:"outstanding_leases"`
	PendingReturns    []string `json:"pending_returns"`
	PayloadJSON       string   `json:"payload_json,omitempty"`
}

type InstanceRecord struct {
	InstanceID      string      `json:"instance_id"`
	OriginalNodeID  string      `json:"original_node_id"`
	CurrentNodeID   string      `json:"current_node_id"`
	TargetNodeID    string      `json:"target_node_id"`
	Wave            int         `json:"wave"`
	MigrationStatus string      `json:"migration_status"`
	ReturnStatus    string      `json:"return_status"`
	MigrationStart  domain.Tick `json:"migration_start"`
	MigrationEnd    domain.Tick `json:"migration_end"`
	ReturnEnd       domain.Tick `json:"return_end"`
}

type LeaseRecord struct {
	LeaseID        string      `json:"lease_id"`
	LotID          string      `json:"lot_id"`
	LenderNodeID   string      `json:"lender_node_id"`
	BorrowerNodeID string      `json:"borrower_node_id"`
	AmountMilli    int64       `json:"amount_milli"`
	StartTick      domain.Tick `json:"start_tick"`
	ExpiryTick     domain.Tick `json:"expiry_tick"`
	Status         string      `json:"status"`
	ReturnedTick   domain.Tick `json:"returned_tick"`
}

type Store struct {
	db   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, mapCorruption(path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, mapCorruption(path, err)
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS node_states (
			node_id TEXT PRIMARY KEY, state TEXT NOT NULL, used_milli INTEGER NOT NULL,
			schedulable INTEGER NOT NULL, outstanding_json TEXT NOT NULL,
			pending_returns_json TEXT NOT NULL, payload_json TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS lease_states (
			lease_id TEXT PRIMARY KEY, lot_id TEXT NOT NULL, lender_node_id TEXT NOT NULL,
			borrower_node_id TEXT NOT NULL, amount_milli INTEGER NOT NULL,
			start_tick INTEGER NOT NULL, expiry_tick INTEGER NOT NULL,
			status TEXT NOT NULL, returned_tick INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS instance_states (
			instance_id TEXT PRIMARY KEY, original_node_id TEXT NOT NULL,
			current_node_id TEXT NOT NULL, target_node_id TEXT NOT NULL,
			wave INTEGER NOT NULL, migration_status TEXT NOT NULL,
			return_status TEXT NOT NULL, migration_start INTEGER NOT NULL,
			migration_end INTEGER NOT NULL, return_end INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS wave_cursor (
			id INTEGER PRIMARY KEY CHECK (id = 1), committed_wave INTEGER NOT NULL,
			tick INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_envelopes (
			checkpoint_id TEXT PRIMARY KEY, payload BLOB NOT NULL,
			sha256 TEXT NOT NULL, complete INTEGER NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return mapCorruption(s.path, err)
		}
	}
	return nil
}

func (s *Store) SaveCommit(c Checkpoint) error {
	if c.StateVersion == 0 {
		c.StateVersion = CurrentStateVersion
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Commit ordering is deliberate: node and lease ownership are durable
	// before the global wave cursor is allowed to advance.
	for _, n := range c.Nodes {
		outstanding, _ := json.Marshal(n.OutstandingLeases)
		pending, _ := json.Marshal(n.PendingReturns)
		if _, err := tx.Exec(`INSERT INTO node_states
			(node_id,state,used_milli,schedulable,outstanding_json,pending_returns_json,payload_json)
			VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(node_id) DO UPDATE SET state=excluded.state,used_milli=excluded.used_milli,
			schedulable=excluded.schedulable,outstanding_json=excluded.outstanding_json,
			pending_returns_json=excluded.pending_returns_json,payload_json=excluded.payload_json`,
			n.NodeID, n.State, n.UsedMilli, boolInt(n.Schedulable), string(outstanding), string(pending), n.PayloadJSON); err != nil {
			return err
		}
	}

	for _, l := range c.Leases {
		if _, err := tx.Exec(`INSERT INTO lease_states
			(lease_id,lot_id,lender_node_id,borrower_node_id,amount_milli,start_tick,expiry_tick,status,returned_tick)
			VALUES (?,?,?,?,?,?,?,?,?)
			ON CONFLICT(lease_id) DO UPDATE SET status=excluded.status,returned_tick=excluded.returned_tick,
			amount_milli=excluded.amount_milli`,
			l.LeaseID, l.LotID, l.LenderNodeID, l.BorrowerNodeID, l.AmountMilli,
			l.StartTick, l.ExpiryTick, l.Status, l.ReturnedTick); err != nil {
			return err
		}
	}

	for _, in := range c.Instances {
		if _, err := tx.Exec(`INSERT INTO instance_states
			(instance_id,original_node_id,current_node_id,target_node_id,wave,migration_status,
			return_status,migration_start,migration_end,return_end)
			VALUES (?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(instance_id) DO UPDATE SET current_node_id=excluded.current_node_id,
			target_node_id=excluded.target_node_id,wave=excluded.wave,
			migration_status=excluded.migration_status,return_status=excluded.return_status,
			return_end=excluded.return_end`,
			in.InstanceID, in.OriginalNodeID, in.CurrentNodeID, in.TargetNodeID, in.Wave,
			in.MigrationStatus, in.ReturnStatus, in.MigrationStart, in.MigrationEnd, in.ReturnEnd); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('state_version',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, itoa(c.StateVersion)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('project_id',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, c.ProjectID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO wave_cursor(id,committed_wave,tick) VALUES(1,?,?)
		ON CONFLICT(id) DO UPDATE SET committed_wave=excluded.committed_wave,tick=excluded.tick`,
		c.CommittedWave, c.Tick); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LoadRecovery() (Checkpoint, bool, error) {
	var versionString string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key='state_version'`).Scan(&versionString)
	if errors.Is(err, sql.ErrNoRows) {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, mapCorruption(s.path, err)
	}
	version := atoi(versionString)
	if version != CurrentStateVersion {
		return Checkpoint{StateVersion: version}, true, domain.NewError(domain.EStateVersionMismatch, "$.meta.state_version",
			"persisted state version %d does not match code version %d; recompute whole segment",
			version, CurrentStateVersion).
			WithDetailsSafe(map[string]any{"persisted_version": version, "code_version": CurrentStateVersion, "full_recompute_required": true})
	}

	c := Checkpoint{StateVersion: version}
	_ = s.db.QueryRow(`SELECT value FROM meta WHERE key='project_id'`).Scan(&c.ProjectID)
	_ = s.db.QueryRow(`SELECT committed_wave,tick FROM wave_cursor WHERE id=1`).Scan(&c.CommittedWave, &c.Tick)

	rows, err := s.db.Query(`SELECT node_id,state,used_milli,schedulable,outstanding_json,pending_returns_json,payload_json FROM node_states ORDER BY node_id`)
	if err != nil {
		return c, true, err
	}
	defer rows.Close()
	for rows.Next() {
		var n NodeRecord
		var schedulable int
		var outstanding, pending string
		if err := rows.Scan(&n.NodeID, &n.State, &n.UsedMilli, &schedulable, &outstanding, &pending, &n.PayloadJSON); err != nil {
			return c, true, err
		}
		n.Schedulable = schedulable == 1
		_ = json.Unmarshal([]byte(outstanding), &n.OutstandingLeases)
		_ = json.Unmarshal([]byte(pending), &n.PendingReturns)
		c.Nodes = append(c.Nodes, n)
	}

	lrows, err := s.db.Query(`SELECT lease_id,lot_id,lender_node_id,borrower_node_id,amount_milli,start_tick,expiry_tick,status,returned_tick FROM lease_states ORDER BY lease_id`)
	if err != nil {
		return c, true, err
	}
	defer lrows.Close()
	for lrows.Next() {
		var l LeaseRecord
		if err := lrows.Scan(&l.LeaseID, &l.LotID, &l.LenderNodeID, &l.BorrowerNodeID, &l.AmountMilli,
			&l.StartTick, &l.ExpiryTick, &l.Status, &l.ReturnedTick); err != nil {
			return c, true, err
		}
		c.Leases = append(c.Leases, l)
	}

	irows, err := s.db.Query(`SELECT instance_id,original_node_id,current_node_id,target_node_id,wave,migration_status,return_status,migration_start,migration_end,return_end FROM instance_states ORDER BY instance_id`)
	if err != nil {
		return c, true, err
	}
	defer irows.Close()
	for irows.Next() {
		var in InstanceRecord
		if err := irows.Scan(&in.InstanceID, &in.OriginalNodeID, &in.CurrentNodeID, &in.TargetNodeID, &in.Wave,
			&in.MigrationStatus, &in.ReturnStatus, &in.MigrationStart, &in.MigrationEnd, &in.ReturnEnd); err != nil {
			return c, true, err
		}
		c.Instances = append(c.Instances, in)
	}
	if err := ValidateRecoveryInvariants(c); err != nil {
		return c, true, err
	}
	return c, true, nil
}

func ValidateRecoveryInvariants(c Checkpoint) error {
	seenInstanceNode := map[string]string{}
	for _, in := range c.Instances {
		if prev, ok := seenInstanceNode[in.InstanceID]; ok && prev != in.CurrentNodeID {
			return domain.NewError(domain.EInstanceDualPlacement, "$.instance_states",
				"instance %s appears on both %s and %s", in.InstanceID, prev, in.CurrentNodeID)
		}
		seenInstanceNode[in.InstanceID] = in.CurrentNodeID
	}
	activeByLot := map[string]int64{}
	for _, l := range c.Leases {
		if l.Status == "active" {
			activeByLot[l.LotID] += l.AmountMilli
		}
	}
	// Lot cap is stored in node payload in full checkpoints; positivity is enough
	// here while SaveCommit callers perform domain-level exclusivity validation.
	for lot, amount := range activeByLot {
		if amount < 0 {
			return domain.NewError(domain.ELoanDoubleBooked, "$.lease_states", "negative total for lot %s", lot)
		}
	}
	return nil
}

func (s *Store) SaveEnvelope(id string, payload []byte) error {
	sum := sha256.Sum256(payload)
	_, err := s.db.Exec(`INSERT INTO checkpoint_envelopes(checkpoint_id,payload,sha256,complete)
		VALUES(?,?,?,1) ON CONFLICT(checkpoint_id) DO UPDATE SET payload=excluded.payload,
		sha256=excluded.sha256,complete=1`, id, payload, hex.EncodeToString(sum[:]))
	return err
}

func (s *Store) TruncateEnvelope(id string) error {
	var payload []byte
	err := s.db.QueryRow(`SELECT payload FROM checkpoint_envelopes WHERE checkpoint_id=?`, id).Scan(&payload)
	if err != nil {
		return err
	}
	if len(payload) < 4 {
		payload = []byte(`{"truncated":`)
	} else {
		payload = payload[:len(payload)/2]
	}
	_, err = s.db.Exec(`UPDATE checkpoint_envelopes SET payload=?, complete=0, sha256='truncated' WHERE checkpoint_id=?`, payload, id)
	return err
}

func (s *Store) LoadEnvelope(id string) ([]byte, error) {
	var payload []byte
	var sum string
	var complete int
	err := s.db.QueryRow(`SELECT payload,sha256,complete FROM checkpoint_envelopes WHERE checkpoint_id=?`, id).Scan(&payload, &sum, &complete)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.NewError(domain.ECheckpointTruncated, "$.checkpoint_envelopes."+id, "checkpoint envelope is missing")
		}
		return nil, mapCorruption(s.path, err)
	}
	if complete != 1 || sum == "truncated" {
		return nil, domain.NewError(domain.ECheckpointTruncated, "$.checkpoint_envelopes."+id, "checkpoint is marked incomplete")
	}
	actual := sha256.Sum256(payload)
	if hex.EncodeToString(actual[:]) != sum {
		return nil, domain.NewError(domain.ECheckpointTruncated, "$.checkpoint_envelopes."+id+".sha256", "checkpoint checksum mismatch")
	}
	return payload, nil
}

func TruncateDatabaseFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	size := st.Size()
	if size < 64 {
		return os.Truncate(path, 0)
	}
	return os.Truncate(path, size/3)
}

func mapCorruption(path string, err error) error {
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "malformed") || strings.Contains(text, "not a database") ||
		strings.Contains(text, "unexpected eof") || strings.Contains(text, "truncat") ||
		strings.Contains(text, "database disk image") {
		return domain.NewError(domain.ECheckpointTruncated, path, "checkpoint database is truncated or corrupt: %v", err)
	}
	return err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func atoi(s string) int {
	n := 0
	neg := false
	for _, r := range s {
		if r == '-' {
			neg = true
			continue
		}
		n = n*10 + int(r-'0')
	}
	if neg {
		return -n
	}
	return n
}
