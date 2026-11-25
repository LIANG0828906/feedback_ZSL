package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"

	_ "modernc.org/sqlite"
)

type FailpointFunc func(phase string, tick int64) error

type StoreOption func(*SQLiteStore)

func WithFailpoint(failpoint FailpointFunc) StoreOption {
	return func(store *SQLiteStore) {
		store.failpoint = failpoint
	}
}

type SQLiteStore struct {
	db        *sql.DB
	mu        sync.Mutex
	failpoint FailpointFunc
}

func OpenSQLiteStore(path string, options ...StoreOption) (*SQLiteStore, error) {
	if path == "" {
		return nil, newError(CodeCheckpoint, "$.sqlite.path", "database path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, newError(CodeCheckpoint, "$.sqlite", "open: %v", err)
	}
	db.SetMaxOpenConns(1)
	store := &SQLiteStore{db: db}
	for _, option := range options {
		option(store)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, newError(CodeCheckpoint, "$.sqlite", "busy_timeout: %v", err)
	}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) SetFailpoint(failpoint FailpointFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failpoint = failpoint
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *SQLiteStore) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			tick INTEGER NOT NULL,
			snapshot_id TEXT NOT NULL,
			revision TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_history (
			tick INTEGER PRIMARY KEY,
			snapshot_id TEXT NOT NULL,
			revision TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS node_state (
			node_id TEXT PRIMARY KEY,
			rack TEXT NOT NULL,
			zone INTEGER NOT NULL,
			status TEXT NOT NULL,
			faulty INTEGER NOT NULL,
			isolated INTEGER NOT NULL,
			domain_id INTEGER,
			generation INTEGER NOT NULL,
			pending_recovery_tick INTEGER,
			recovered INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS domain_state (
			id INTEGER PRIMARY KEY,
			root_nodes TEXT NOT NULL,
			nodes TEXT NOT NULL,
			affected_nodes TEXT NOT NULL,
			started_tick INTEGER NOT NULL,
			timeout_num INTEGER NOT NULL,
			timeout_den INTEGER NOT NULL,
			deadline_tick INTEGER NOT NULL,
			status TEXT NOT NULL,
			forced INTEGER NOT NULL,
			upgraded_tick INTEGER,
			upgrade_evidence TEXT NOT NULL,
			committed_tick INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS workload_state (
			workload_id TEXT PRIMARY KEY,
			node_id TEXT NOT NULL,
			budget_bytes INTEGER NOT NULL,
			used_bytes INTEGER NOT NULL,
			frozen INTEGER NOT NULL,
			freeze_sources TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS freeze_queue (
			id TEXT PRIMARY KEY,
			workload_id TEXT NOT NULL,
			node_id TEXT NOT NULL,
			amount_bytes INTEGER NOT NULL,
			arrived_tick INTEGER NOT NULL,
			seq INTEGER NOT NULL,
			freeze_sources TEXT NOT NULL,
			position INTEGER NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return newError(CodeCheckpoint, "$.sqlite.migrate", "%v", err)
		}
	}
	return nil
}

// CommitCheckpoint writes one complete logical tick. Node, domain, workload
// and queue rows are written before checkpoint_meta (the authoritative global
// tick). Everything is in one transaction, so a crash at any phase leaves the
// previous completed commit visible and intact.
func (s *SQLiteStore) CommitCheckpoint(ctx context.Context, checkpoint Checkpoint) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return newError(CodeCheckpoint, "$.sqlite.begin", "%v", err)
	}
	defer tx.Rollback()
	if err := s.phase("after_begin", checkpoint.Tick); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_state`); err != nil {
		return checkpointError("clear node_state", err)
	}
	for _, node := range checkpoint.Nodes {
		domainID := interface{}(nil)
		if node.DomainID != nil {
			domainID = *node.DomainID
		}
		pending := interface{}(nil)
		if node.PendingRecoveryTick != nil {
			pending = *node.PendingRecoveryTick
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO node_state
			(node_id, rack, zone, status, faulty, isolated, domain_id, generation, pending_recovery_tick, recovered)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			node.NodeID, node.Rack, node.Zone, string(node.Status), boolInt(node.Faulty),
			boolInt(node.Isolated), domainID, node.Generation, pending, boolInt(node.Recovered)); err != nil {
			return checkpointError("write node_state", err)
		}
	}
	if err := s.phase("after_node_state", checkpoint.Tick); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM domain_state`); err != nil {
		return checkpointError("clear domain_state", err)
	}
	for _, domain := range checkpoint.Domains {
		roots, _ := json.Marshal(domain.RootNodes)
		nodes, _ := json.Marshal(domain.Nodes)
		affected, _ := json.Marshal(domain.AffectedNodes)
		upgraded := nullableInt(domain.UpgradedTick)
		committed := nullableInt(domain.CommittedTick)
		if _, err := tx.ExecContext(ctx, `INSERT INTO domain_state
			(id, root_nodes, nodes, affected_nodes, started_tick, timeout_num, timeout_den,
			 deadline_tick, status, forced, upgraded_tick, upgrade_evidence, committed_tick)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			domain.ID, string(roots), string(nodes), string(affected), domain.StartedTick,
			domain.TimeoutTicks.Num, domain.TimeoutTicks.Den, domain.DeadlineTick,
			string(domain.Status), boolInt(domain.Forced), upgraded, domain.UpgradeEvidence, committed); err != nil {
			return checkpointError("write domain_state", err)
		}
	}
	if err := s.phase("after_domain_state", checkpoint.Tick); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workload_state`); err != nil {
		return checkpointError("clear workload_state", err)
	}
	for _, workload := range checkpoint.Workloads {
		sources, _ := json.Marshal(workload.FreezeSource)
		if _, err := tx.ExecContext(ctx, `INSERT INTO workload_state
			(workload_id, node_id, budget_bytes, used_bytes, frozen, freeze_sources)
			VALUES (?, ?, ?, ?, ?, ?)`,
			workload.WorkloadID, workload.NodeID, workload.BudgetBytes, workload.UsedBytes,
			boolInt(workload.Frozen), string(sources)); err != nil {
			return checkpointError("write workload_state", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM freeze_queue`); err != nil {
		return checkpointError("clear freeze_queue", err)
	}
	for position, wait := range checkpoint.Waiting {
		sources, _ := json.Marshal(wait.Sources)
		if _, err := tx.ExecContext(ctx, `INSERT INTO freeze_queue
			(id, workload_id, node_id, amount_bytes, arrived_tick, seq, freeze_sources, position)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			wait.ID, wait.WorkloadID, wait.NodeID, wait.AmountBytes, wait.ArrivedTick,
			wait.Seq, string(sources), position); err != nil {
			return checkpointError("write freeze_queue", err)
		}
	}
	if err := s.phase("after_freeze_queue", checkpoint.Tick); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_history (tick, snapshot_id, revision)
		VALUES (?, ?, ?)
		ON CONFLICT(tick) DO UPDATE SET snapshot_id=excluded.snapshot_id, revision=excluded.revision`,
		checkpoint.Tick, checkpoint.SnapshotID, checkpoint.Revision); err != nil {
		return checkpointError("write checkpoint_history", err)
	}
	if err := s.phase("before_global_tick", checkpoint.Tick); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_meta (id, tick, snapshot_id, revision)
		VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET tick=excluded.tick, snapshot_id=excluded.snapshot_id, revision=excluded.revision`,
		checkpoint.Tick, checkpoint.SnapshotID, checkpoint.Revision); err != nil {
		return checkpointError("update global tick", err)
	}
	if err := s.phase("after_global_tick", checkpoint.Tick); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return checkpointError("commit", err)
	}
	return nil
}

func (s *SQLiteStore) phase(phase string, tick int64) error {
	s.mu.Lock()
	failpoint := s.failpoint
	s.mu.Unlock()
	if failpoint == nil {
		return nil
	}
	if err := failpoint(phase, tick); err != nil {
		return newError(CodeCheckpoint, "$.sqlite."+phase, "injected failure at tick %d: %v", tick, err)
	}
	return nil
}

func (s *SQLiteStore) LatestCheckpoint(ctx context.Context) (Checkpoint, bool, error) {
	var checkpoint Checkpoint
	err := s.db.QueryRowContext(ctx, `SELECT tick, snapshot_id, revision FROM checkpoint_meta WHERE id=1`).
		Scan(&checkpoint.Tick, &checkpoint.SnapshotID, &checkpoint.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, checkpointError("read global tick", err)
	}
	checkpoint.Nodes, err = s.readNodes(ctx)
	if err != nil {
		return Checkpoint{}, false, err
	}
	checkpoint.Domains, err = s.readDomains(ctx)
	if err != nil {
		return Checkpoint{}, false, err
	}
	checkpoint.Workloads, err = s.readWorkloads(ctx)
	if err != nil {
		return Checkpoint{}, false, err
	}
	checkpoint.Waiting, err = s.readWaiting(ctx)
	if err != nil {
		return Checkpoint{}, false, err
	}
	return checkpoint, true, nil
}

func (s *SQLiteStore) readNodes(ctx context.Context) ([]CheckpointNode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, rack, zone, status, faulty, isolated,
		domain_id, generation, pending_recovery_tick, recovered FROM node_state ORDER BY node_id`)
	if err != nil {
		return nil, checkpointError("read node_state", err)
	}
	defer rows.Close()
	var result []CheckpointNode
	for rows.Next() {
		var (
			node      CheckpointNode
			status    string
			faulty    int
			isolated  int
			domainID  sql.NullInt64
			pending   sql.NullInt64
			recovered int
		)
		if err := rows.Scan(&node.NodeID, &node.Rack, &node.Zone, &status, &faulty, &isolated,
			&domainID, &node.Generation, &pending, &recovered); err != nil {
			return nil, checkpointError("scan node_state", err)
		}
		node.Status = NodeStatus(status)
		node.Faulty = faulty != 0
		node.Isolated = isolated != 0
		node.Recovered = recovered != 0
		if domainID.Valid {
			value := domainID.Int64
			node.DomainID = &value
		}
		if pending.Valid {
			value := pending.Int64
			node.PendingRecoveryTick = &value
		}
		result = append(result, node)
	}
	if err := rows.Err(); err != nil {
		return nil, checkpointError("iterate node_state", err)
	}
	return result, nil
}

func (s *SQLiteStore) readDomains(ctx context.Context) ([]CheckpointDomain, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, root_nodes, nodes, affected_nodes, started_tick,
		timeout_num, timeout_den, deadline_tick, status, forced, upgraded_tick, upgrade_evidence, committed_tick
		FROM domain_state ORDER BY id`)
	if err != nil {
		return nil, checkpointError("read domain_state", err)
	}
	defer rows.Close()
	var result []CheckpointDomain
	for rows.Next() {
		var (
			domain    CheckpointDomain
			roots     string
			nodes     string
			affected  string
			status    string
			forced    int
			upgraded  sql.NullInt64
			committed sql.NullInt64
		)
		if err := rows.Scan(&domain.ID, &roots, &nodes, &affected, &domain.StartedTick,
			&domain.TimeoutTicks.Num, &domain.TimeoutTicks.Den, &domain.DeadlineTick,
			&status, &forced, &upgraded, &domain.UpgradeEvidence, &committed); err != nil {
			return nil, checkpointError("scan domain_state", err)
		}
		domain.Status = DomainStatus(status)
		domain.Forced = forced != 0
		if upgraded.Valid {
			value := upgraded.Int64
			domain.UpgradedTick = &value
		}
		if committed.Valid {
			value := committed.Int64
			domain.CommittedTick = &value
		}
		if err := json.Unmarshal([]byte(roots), &domain.RootNodes); err != nil {
			return nil, checkpointError("decode domain roots", err)
		}
		if err := json.Unmarshal([]byte(nodes), &domain.Nodes); err != nil {
			return nil, checkpointError("decode domain nodes", err)
		}
		if err := json.Unmarshal([]byte(affected), &domain.AffectedNodes); err != nil {
			return nil, checkpointError("decode domain affected nodes", err)
		}
		result = append(result, domain)
	}
	if err := rows.Err(); err != nil {
		return nil, checkpointError("iterate domain_state", err)
	}
	return result, nil
}

func (s *SQLiteStore) readWorkloads(ctx context.Context) ([]CheckpointWorkload, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT workload_id, node_id, budget_bytes, used_bytes, frozen,
		freeze_sources FROM workload_state ORDER BY workload_id`)
	if err != nil {
		return nil, checkpointError("read workload_state", err)
	}
	defer rows.Close()
	var result []CheckpointWorkload
	for rows.Next() {
		var (
			workload CheckpointWorkload
			frozen   int
			sources  string
		)
		if err := rows.Scan(&workload.WorkloadID, &workload.NodeID, &workload.BudgetBytes,
			&workload.UsedBytes, &frozen, &sources); err != nil {
			return nil, checkpointError("scan workload_state", err)
		}
		workload.Frozen = frozen != 0
		if err := json.Unmarshal([]byte(sources), &workload.FreezeSource); err != nil {
			return nil, checkpointError("decode workload sources", err)
		}
		result = append(result, workload)
	}
	if err := rows.Err(); err != nil {
		return nil, checkpointError("iterate workload_state", err)
	}
	return result, nil
}

func (s *SQLiteStore) readWaiting(ctx context.Context) ([]WaitEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, workload_id, node_id, amount_bytes, arrived_tick, seq,
		freeze_sources FROM freeze_queue ORDER BY position`)
	if err != nil {
		return nil, checkpointError("read freeze_queue", err)
	}
	defer rows.Close()
	var result []WaitEntry
	for rows.Next() {
		var (
			wait    WaitEntry
			sources string
		)
		if err := rows.Scan(&wait.ID, &wait.WorkloadID, &wait.NodeID, &wait.AmountBytes,
			&wait.ArrivedTick, &wait.Seq, &sources); err != nil {
			return nil, checkpointError("scan freeze_queue", err)
		}
		if err := json.Unmarshal([]byte(sources), &wait.Sources); err != nil {
			return nil, checkpointError("decode queue sources", err)
		}
		result = append(result, wait)
	}
	if err := rows.Err(); err != nil {
		return nil, checkpointError("iterate freeze_queue", err)
	}
	return result, nil
}

func checkpointError(operation string, err error) error {
	return newError(CodeCheckpoint, "$.sqlite."+operation, "%v", err)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullableInt(value *int64) interface{} {
	if value == nil {
		return nil
	}
	return *value
}
