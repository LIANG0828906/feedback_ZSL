package quota

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type CheckpointMeta struct {
	CheckpointVersion  string `json:"checkpoint_version"`
	ProjectFingerprint string `json:"project_fingerprint"`
	SchemaVersion      string `json:"schema_version"`
	CommittedTimeMilli int64  `json:"committed_time_millis"`
}

type CheckpointSnapshot struct {
	Meta   CheckpointMeta `json:"meta"`
	State  State          `json:"state"`
	Ledger []TenantLedger `json:"ledger"`
	Nodes  []NodeSnapshot `json:"nodes"`
}

type CheckpointStore struct {
	db   *sql.DB
	path string
}

func OpenCheckpoint(path string) (*CheckpointStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, NewError(CodeCheckpointCorrupt, "open_checkpoint", "path", "empty path")
	}
	if st, err := os.Stat(path); err == nil && st.Size() == 0 {
		return nil, &BusinessError{Code: CodeCheckpointTruncated, Op: "open_checkpoint", Path: path, Detail: "zero-length checkpoint"}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, checkpointOpenError(path, err)
	}
	db.SetMaxOpenConns(1)
	store := &CheckpointStore{db: db, path: path}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, checkpointOpenError(path, err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout=5000; PRAGMA synchronous=FULL;`); err != nil {
		db.Close()
		return nil, checkpointOpenError(path, err)
	}
	if err := store.ensureSchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func checkpointOpenError(path string, err error) error {
	msg := strings.ToLower(err.Error())
	code := CodeCheckpointCorrupt
	if strings.Contains(msg, "not a database") || strings.Contains(msg, "malformed") ||
		strings.Contains(msg, "unexpected eof") || strings.Contains(msg, "truncated") {
		code = CodeCheckpointTruncated
	}
	return &BusinessError{Code: code, Op: "open_checkpoint", Path: path, Detail: "checkpoint cannot be opened", Err: err}
}

func (s *CheckpointStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *CheckpointStore) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS checkpoint_meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  checkpoint_version TEXT NOT NULL,
  project_fingerprint TEXT NOT NULL,
  schema_version TEXT NOT NULL,
  committed_time_millis INTEGER NOT NULL,
  committed INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tenant_ledger (
  tenant_id TEXT PRIMARY KEY,
  available_millicores INTEGER NOT NULL,
  used_millicores INTEGER NOT NULL,
  guarantee_remaining_millicores INTEGER NOT NULL,
  quota_millicores INTEGER NOT NULL,
  burst_millicores INTEGER NOT NULL,
  weight_permille INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS node_occupancy (
  node_id TEXT NOT NULL,
  tenant_id TEXT NOT NULL,
  used_millicores INTEGER NOT NULL,
  PRIMARY KEY (node_id, tenant_id)
);
CREATE TABLE IF NOT EXISTS wait_queue (
  ordinal INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id TEXT NOT NULL,
  node_id TEXT NOT NULL,
  amount_millicores INTEGER NOT NULL,
  constraint_code TEXT NOT NULL,
  order_basis TEXT NOT NULL,
  event_seq INTEGER NOT NULL,
  enqueued_at_millis INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS commit_audit (
  ordinal INTEGER PRIMARY KEY AUTOINCREMENT,
  phase TEXT NOT NULL,
  committed_time_millis INTEGER NOT NULL
);
`
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return &BusinessError{Code: CodeCheckpointCorrupt, Op: "ensure_schema", Path: s.path, Detail: "cannot initialize checkpoint tables", Err: err}
	}
	return nil
}

// Save is one SQLite transaction. It deliberately writes tenant ledger and
// node occupancy rows before the singleton meta row that advances the global
// committed logical time.
func (s *CheckpointStore) Save(ctx context.Context, p *Project, state State) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := validateState(p, state); err != nil {
		return err
	}
	fp, err := p.Fingerprint()
	if err != nil {
		return err
	}
	ledger, _, err := ledgerSnapshot(context.Background(), p, state, 1)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return &BusinessError{Code: CodeCheckpointCorrupt, Op: "save_checkpoint", Path: s.path, Detail: "begin transaction", Err: err}
	}
	defer tx.Rollback()
	for _, table := range []string{"tenant_ledger", "node_occupancy", "wait_queue"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return checkpointWriteError(s.path, "clear "+table, err)
		}
	}
	for _, row := range ledger {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tenant_ledger
			(tenant_id, available_millicores, used_millicores, guarantee_remaining_millicores,
			 quota_millicores, burst_millicores, weight_permille)
			VALUES (?,?,?,?,?,?,?)`,
			row.TenantID, row.AvailableMilli, row.UsedMilli, row.GuaranteeRemainingMilli,
			row.QuotaMilli, row.BurstMilli, row.WeightPermille); err != nil {
			return checkpointWriteError(s.path, "tenant_ledger", err)
		}
	}
	for node, usage := range state.Nodes {
		for tenant, amount := range usage {
			if amount == 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO node_occupancy
				(node_id, tenant_id, used_millicores) VALUES (?,?,?)`, node, tenant, amount); err != nil {
				return checkpointWriteError(s.path, "node_occupancy", err)
			}
		}
	}
	for _, q := range state.Queue {
		if _, err := tx.ExecContext(ctx, `INSERT INTO wait_queue
			(tenant_id, node_id, amount_millicores, constraint_code, order_basis, event_seq, enqueued_at_millis)
			VALUES (?,?,?,?,?,?,?)`,
			q.TenantID, q.NodeID, q.AmountMilli, q.Constraint, q.OrderBasis, q.Seq, q.EnqueuedAt); err != nil {
			return checkpointWriteError(s.path, "wait_queue", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO commit_audit (phase, committed_time_millis)
		VALUES ('tenant_node_queue_committed', ?)`, state.CommittedTimeMilli); err != nil {
		return checkpointWriteError(s.path, "commit_audit", err)
	}
	committed := 0
	if state.Committed {
		committed = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_meta
		(id, checkpoint_version, project_fingerprint, schema_version, committed_time_millis, committed)
		VALUES (1,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		 checkpoint_version=excluded.checkpoint_version,
		 project_fingerprint=excluded.project_fingerprint,
		 schema_version=excluded.schema_version,
		 committed_time_millis=excluded.committed_time_millis,
		 committed=excluded.committed`,
		CheckpointVersion, fp, p.SchemaVersion, state.CommittedTimeMilli, committed); err != nil {
		return checkpointWriteError(s.path, "checkpoint_meta (global time last)", err)
	}
	if err := tx.Commit(); err != nil {
		return &BusinessError{Code: CodeCheckpointCorrupt, Op: "save_checkpoint", Path: s.path, Detail: "commit transaction", Err: err}
	}
	return nil
}

func checkpointWriteError(path, table string, err error) error {
	return &BusinessError{Code: CodeCheckpointCorrupt, Op: "save_checkpoint", Path: path, Detail: table, Err: err}
}

func (s *CheckpointStore) Load(ctx context.Context, p *Project) (*CheckpointSnapshot, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	var meta CheckpointMeta
	var committed int
	err := s.db.QueryRowContext(ctx, `SELECT checkpoint_version, project_fingerprint, schema_version,
		committed_time_millis, committed FROM checkpoint_meta WHERE id=1`).
		Scan(&meta.CheckpointVersion, &meta.ProjectFingerprint, &meta.SchemaVersion, &meta.CommittedTimeMilli, &committed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &BusinessError{Code: CodeCheckpointMissing, Op: "load_checkpoint", Path: s.path, Detail: "no committed checkpoint"}
	}
	if err != nil {
		return nil, checkpointReadError(s.path, "checkpoint_meta", err)
	}
	if meta.CheckpointVersion != CheckpointVersion {
		return nil, &BusinessError{Code: CodeCheckpointVersion, Op: "load_checkpoint", Path: s.path,
			Detail: fmt.Sprintf("checkpoint version got %s want %s", meta.CheckpointVersion, CheckpointVersion)}
	}
	fp, err := p.Fingerprint()
	if err != nil {
		return nil, err
	}
	if meta.SchemaVersion != p.SchemaVersion || meta.ProjectFingerprint != fp {
		return nil, &BusinessError{Code: CodeVersionMismatch, Op: "load_checkpoint", Path: s.path,
			Detail: fmt.Sprintf("checkpoint schema=%s project=%s does not match schema=%s project=%s",
				meta.SchemaVersion, meta.ProjectFingerprint, p.SchemaVersion, fp)}
	}

	state := State{
		Committed: committed != 0, CommittedTimeMilli: meta.CommittedTimeMilli,
		Weights: map[string]int64{}, Nodes: map[string]map[string]int64{},
	}
	for _, n := range p.Nodes {
		state.Nodes[n.ID] = map[string]int64{}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT tenant_id, weight_permille FROM tenant_ledger ORDER BY tenant_id`)
	if err != nil {
		return nil, checkpointReadError(s.path, "tenant_ledger", err)
	}
	for rows.Next() {
		var tenant string
		var weight int64
		if err := rows.Scan(&tenant, &weight); err != nil {
			rows.Close()
			return nil, checkpointReadError(s.path, "tenant_ledger row", err)
		}
		state.Weights[tenant] = weight
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, checkpointReadError(s.path, "tenant_ledger iteration", err)
	}
	if err := rows.Close(); err != nil {
		return nil, checkpointReadError(s.path, "tenant_ledger close", err)
	}
	nodeRows, err := s.db.QueryContext(ctx, `SELECT node_id, tenant_id, used_millicores FROM node_occupancy ORDER BY node_id, tenant_id`)
	if err != nil {
		return nil, checkpointReadError(s.path, "node_occupancy", err)
	}
	for nodeRows.Next() {
		var node, tenant string
		var amount int64
		if err := nodeRows.Scan(&node, &tenant, &amount); err != nil {
			nodeRows.Close()
			return nil, checkpointReadError(s.path, "node_occupancy row", err)
		}
		if state.Nodes[node] == nil {
			nodeRows.Close()
			return nil, &BusinessError{Code: CodeCheckpointInvariant, Op: "load_checkpoint", NodeID: node, Detail: "unknown node row"}
		}
		if _, exists := state.Nodes[node][tenant]; exists {
			nodeRows.Close()
			return nil, &BusinessError{Code: CodeCheckpointInvariant, Op: "load_checkpoint", NodeID: node, TenantID: tenant,
				Detail: "duplicate tenant occupancy row would double count usage"}
		}
		state.Nodes[node][tenant] = amount
	}
	if err := nodeRows.Err(); err != nil {
		nodeRows.Close()
		return nil, checkpointReadError(s.path, "node_occupancy iteration", err)
	}
	if err := nodeRows.Close(); err != nil {
		return nil, checkpointReadError(s.path, "node_occupancy close", err)
	}
	queueRows, err := s.db.QueryContext(ctx, `SELECT tenant_id, node_id, amount_millicores, constraint_code,
		order_basis, event_seq, enqueued_at_millis FROM wait_queue ORDER BY ordinal`)
	if err != nil {
		return nil, checkpointReadError(s.path, "wait_queue", err)
	}
	for queueRows.Next() {
		var q QueueEntry
		if err := queueRows.Scan(&q.TenantID, &q.NodeID, &q.AmountMilli, &q.Constraint, &q.OrderBasis, &q.Seq, &q.EnqueuedAt); err != nil {
			queueRows.Close()
			return nil, checkpointReadError(s.path, "wait_queue row", err)
		}
		state.Queue = append(state.Queue, q)
	}
	if err := queueRows.Err(); err != nil {
		queueRows.Close()
		return nil, checkpointReadError(s.path, "wait_queue iteration", err)
	}
	if err := queueRows.Close(); err != nil {
		return nil, checkpointReadError(s.path, "wait_queue close", err)
	}
	if err := validateState(p, state); err != nil {
		return nil, err
	}
	ledger, _, err := ledgerSnapshot(context.Background(), p, state, 1)
	if err != nil {
		return nil, err
	}
	// The persisted ledger rows are read back for integrity checks. Runtime
	// ledger is derived from node occupancy so a stale cached number cannot
	// reintroduce double accounting.
	persisted, err := s.readPersistedLedger(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ledger {
		want, ok := persisted[ledger[i].TenantID]
		if !ok || !tenantLedgerEqual(ledger[i], want) {
			return nil, &BusinessError{Code: CodeCheckpointCorrupt, Op: "load_checkpoint", TenantID: ledger[i].TenantID,
				Detail: "persisted tenant ledger does not reconcile with node occupancy and weights"}
		}
	}
	if len(persisted) != len(ledger) {
		return nil, &BusinessError{Code: CodeCheckpointCorrupt, Op: "load_checkpoint", Path: s.path,
			Detail: "persisted tenant ledger row count does not match project tenants"}
	}
	nodes := nodeSnapshot(p, state)
	return &CheckpointSnapshot{Meta: meta, State: state, Ledger: ledger, Nodes: nodes}, nil
}

func (s *CheckpointStore) readPersistedLedger(ctx context.Context) (map[string]TenantLedger, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tenant_id, available_millicores, used_millicores,
		guarantee_remaining_millicores, quota_millicores, burst_millicores, weight_permille
		FROM tenant_ledger`)
	if err != nil {
		return nil, checkpointReadError(s.path, "tenant_ledger", err)
	}
	defer rows.Close()
	out := make(map[string]TenantLedger)
	for rows.Next() {
		var l TenantLedger
		if err := rows.Scan(&l.TenantID, &l.AvailableMilli, &l.UsedMilli, &l.GuaranteeRemainingMilli,
			&l.QuotaMilli, &l.BurstMilli, &l.WeightPermille); err != nil {
			return nil, checkpointReadError(s.path, "tenant_ledger row", err)
		}
		out[l.TenantID] = l
	}
	return out, rows.Err()
}

func checkpointReadError(path, table string, err error) error {
	return &BusinessError{Code: CodeCheckpointCorrupt, Op: "load_checkpoint", Path: path, Detail: table, Err: err}
}

func (s *CheckpointStore) Reset(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"tenant_ledger", "node_occupancy", "wait_queue", "checkpoint_meta", "commit_audit"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *CheckpointStore) Path() string { return s.path }

// SetCheckpointVersionForDiagnostics is only for the local fault-injection
// demonstration. Normal writes always use CheckpointVersion.
func (s *CheckpointStore) SetCheckpointVersionForDiagnostics(ctx context.Context, version string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE checkpoint_meta SET checkpoint_version=? WHERE id=1`, version)
	if err != nil {
		return checkpointWriteError(s.path, "checkpoint_meta diagnostic rewrite", err)
	}
	return nil
}
