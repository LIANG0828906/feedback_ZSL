package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"

	"warmcache/internal/domain"
	_ "warmcache/internal/sqlite"
)

const SnapshotVersion = "warmcache-checkpoint.v1"

type Store struct {
	db   *sql.DB
	path string
}

type Checkpoint struct {
	RunID           string                 `json:"run_id"`
	ProjectID       string                 `json:"project_id"`
	Schema          domain.SchemaVersion   `json:"schema"`
	SemanticHash    string                 `json:"semantic_hash"`
	Tick            int64                  `json:"tick"`
	StateHash       string                 `json:"state_hash"`
	BudgetUsed      int64                  `json:"budget_used_milli"`
	BudgetCapacity  int64                  `json:"budget_capacity_milli"`
	Shards          []domain.ShardState    `json:"shards"`
	Nodes           []domain.NodeTickState `json:"nodes"`
	ReplayQueue     map[string][]string    `json:"replay_queue"`
	Requests        []domain.GateResult    `json:"requests"`
	Queue           []domain.QueueEntry    `json:"queue"`
	SnapshotVersion string                 `json:"snapshot_version"`
}

func Open(path string) (*Store, error) {
	return OpenWithVersion(path, SnapshotVersion)
}

func OpenWithVersion(path, expectedSnapshotVersion string) (*Store, error) {
	if path == "" {
		return nil, domain.NewBizError(domain.CodeCheckpointCorrupt, "open_checkpoint", "checkpoint path is required", nil)
	}
	if err := inspectFile(path); err != nil {
		return nil, err
	}
	if directory := filepath.Dir(path); directory != "." {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return nil, domain.NewBizError(domain.CodeCheckpointCorrupt, "open_checkpoint", "cannot create checkpoint directory", map[string]any{"path": path, "error": err.Error()})
		}
	}
	db, err := sql.Open("warmcache-sqlite3", path)
	if err != nil {
		return nil, mapOpenError(path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, mapOpenError(path, err)
	}
	store := &Store{db: db, path: path}
	existingVersion, versionErr := store.Meta("snapshot_version")
	if versionErr == nil && existingVersion != "" && existingVersion != expectedSnapshotVersion {
		_ = db.Close()
		return nil, domain.NewBizError(domain.CodeSnapshotVersion, "open_checkpoint", "snapshot version does not match", map[string]any{
			"expected": expectedSnapshotVersion, "actual": existingVersion, "path": path,
		})
	}
	if versionErr != nil && versionErr != sql.ErrNoRows && !missingMetaTable(versionErr) {
		_ = db.Close()
		return nil, mapOpenError(path, versionErr)
	}
	if err := store.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if existingVersion == "" {
		if err := store.SetMeta("snapshot_version", expectedSnapshotVersion); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := store.integrityCheck(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func missingMetaTable(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such table")
}

func inspectFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return domain.NewBizError(domain.CodeCheckpointCorrupt, "inspect_checkpoint", "cannot stat checkpoint", map[string]any{"path": path, "error": err.Error()})
	}
	if info.IsDir() {
		return domain.NewBizError(domain.CodeCheckpointCorrupt, "inspect_checkpoint", "checkpoint path is a directory", map[string]any{"path": path})
	}
	if info.Size() == 0 {
		return nil
	}
	if info.Size() < 100 {
		return domain.NewBizError(domain.CodeCheckpointTruncated, "inspect_checkpoint", "checkpoint is truncated", map[string]any{"path": path, "bytes": info.Size()})
	}
	file, err := os.Open(path)
	if err != nil {
		return domain.NewBizError(domain.CodeCheckpointCorrupt, "inspect_checkpoint", "cannot read checkpoint", map[string]any{"path": path, "error": err.Error()})
	}
	defer file.Close()
	header := make([]byte, 16)
	if _, err := file.Read(header); err != nil {
		return domain.NewBizError(domain.CodeCheckpointTruncated, "inspect_checkpoint", "checkpoint header is truncated", map[string]any{"path": path, "error": err.Error()})
	}
	if string(header) != "SQLite format 3\x00" {
		return domain.NewBizError(domain.CodeCheckpointTruncated, "inspect_checkpoint", "checkpoint header is not a complete SQLite header", map[string]any{"path": path, "header": fmt.Sprintf("%q", header)})
	}
	return nil
}

func mapOpenError(path string, err error) error {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "not a database") || strings.Contains(message, "malformed") || strings.Contains(message, "truncated") {
		return domain.NewBizError(domain.CodeCheckpointTruncated, "open_checkpoint", "checkpoint cannot be opened as SQLite", map[string]any{"path": path, "error": err.Error()})
	}
	return domain.NewBizError(domain.CodeCheckpointCorrupt, "open_checkpoint", "cannot open checkpoint", map[string]any{"path": path, "error": err.Error()})
}

func (s *Store) initialize() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS runs (
			run_id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			schema TEXT NOT NULL,
			semantic_hash TEXT NOT NULL,
			last_tick INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tick_commit (
			run_id TEXT NOT NULL,
			tick INTEGER NOT NULL,
			budget_used INTEGER NOT NULL,
			budget_capacity INTEGER NOT NULL,
			state_hash TEXT NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (run_id, tick)
		)`,
		`CREATE TABLE IF NOT EXISTS shard_state (
			run_id TEXT NOT NULL,
			tick INTEGER NOT NULL,
			shard_id TEXT NOT NULL,
			node_id TEXT NOT NULL,
			target_version INTEGER NOT NULL,
			visible_version INTEGER NOT NULL,
			progress INTEGER NOT NULL,
			committed_progress INTEGER NOT NULL,
			preheat_ticks INTEGER NOT NULL,
			available INTEGER NOT NULL,
			lease_id TEXT NOT NULL,
			lease_node TEXT NOT NULL,
			lease_expires_tick INTEGER NOT NULL,
			generation INTEGER NOT NULL,
			pending_replay INTEGER NOT NULL,
			budget_capacity INTEGER NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (run_id, tick, shard_id)
		)`,
		`CREATE TABLE IF NOT EXISTS node_state (
			run_id TEXT NOT NULL,
			tick INTEGER NOT NULL,
			node_id TEXT NOT NULL,
			active INTEGER NOT NULL,
			used_bandwidth INTEGER NOT NULL,
			concurrency INTEGER NOT NULL,
			pending_buffer INTEGER NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (run_id, tick, node_id)
		)`,
		`CREATE TABLE IF NOT EXISTS replay_queue (
			run_id TEXT NOT NULL,
			tick INTEGER NOT NULL,
			shard_id TEXT NOT NULL,
			request_id TEXT NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (run_id, tick, shard_id, request_id)
		)`,
		`CREATE TABLE IF NOT EXISTS lease_history (
			run_id TEXT NOT NULL,
			lease_id TEXT NOT NULL,
			shard_id TEXT NOT NULL,
			node_id TEXT NOT NULL,
			acquired_tick INTEGER NOT NULL,
			expires_tick INTEGER NOT NULL,
			released_tick INTEGER NOT NULL,
			release_reason TEXT NOT NULL,
			generation INTEGER NOT NULL,
			PRIMARY KEY (run_id, lease_id)
		)`,
		`CREATE TABLE IF NOT EXISTS gate_result (
			run_id TEXT NOT NULL,
			tick INTEGER NOT NULL,
			request_id TEXT NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (run_id, tick, request_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_shard_tick ON shard_state(run_id, tick)`,
		`CREATE INDEX IF NOT EXISTS idx_node_tick ON node_state(run_id, tick)`,
		`CREATE INDEX IF NOT EXISTS idx_tick_commit ON tick_commit(run_id, tick)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return domain.NewBizError(domain.CodeCheckpointCorrupt, "init_checkpoint", "cannot initialize checkpoint schema", map[string]any{"error": err.Error()})
		}
	}
	return nil
}

func (s *Store) integrityCheck() error {
	var result string
	if err := s.db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return domain.NewBizError(domain.CodeCheckpointCorrupt, "integrity_check", "SQLite integrity check failed", map[string]any{"path": s.path, "error": err.Error()})
	}
	if result != "ok" {
		return domain.NewBizError(domain.CodeCheckpointCorrupt, "integrity_check", "SQLite reported corruption", map[string]any{"path": s.path, "result": result})
	}
	return nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return domain.NewBizError(domain.CodeCheckpointCorrupt, "set_meta", "cannot write checkpoint metadata", map[string]any{"key": key, "error": err.Error()})
	}
	return nil
}

func (s *Store) Meta(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&value)
	return value, err
}

func (s *Store) SaveResult(result domain.RunResult) error {
	if result.RunID == "" {
		return domain.NewBizError(domain.CodeCheckpointOrder, "save_result", "run id is required", nil)
	}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`INSERT INTO runs(run_id, project_id, schema, semantic_hash, last_tick, created_at)
		VALUES(?, ?, ?, ?, 0, ?)
		ON CONFLICT(run_id) DO UPDATE SET project_id=excluded.project_id, schema=excluded.schema, semantic_hash=excluded.semantic_hash`,
		result.RunID, result.Project.ID, string(result.Schema), result.SemanticHash, created)
	if err != nil {
		return domain.NewBizError(domain.CodeCheckpointCorrupt, "save_result", "cannot register run", map[string]any{"error": err.Error()})
	}
	payloads := map[string]string{}
	for _, request := range result.InputRequests {
		payloads[request.ID] = request.Payload
	}
	for _, timeline := range result.Timeline {
		if err := s.SaveTick(result, timeline, payloads); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SaveTick(result domain.RunResult, tick domain.TickState, payloads map[string]string) error {
	if tick.Tick <= 0 {
		return domain.NewBizError(domain.CodeCheckpointOrder, "save_tick", "tick must be positive", map[string]any{"tick": tick.Tick})
	}
	tickPayload, _ := json.Marshal(tick)
	stateHash := hashPayload(tickPayload)
	tx, err := s.db.Begin()
	if err != nil {
		return domain.NewBizError(domain.CodeCheckpointCorrupt, "begin_tick", "cannot start checkpoint transaction", map[string]any{"error": err.Error()})
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO runs(run_id, project_id, schema, semantic_hash, last_tick, created_at)
		VALUES(?, ?, ?, ?, 0, ?)
		ON CONFLICT(run_id) DO UPDATE SET project_id=excluded.project_id, schema=excluded.schema, semantic_hash=excluded.semantic_hash`,
		result.RunID, result.Project.ID, string(result.Schema), result.SemanticHash, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return domain.NewBizError(domain.CodeCheckpointCorrupt, "save_tick", "cannot register run", map[string]any{"error": err.Error()})
	}
	if _, err := tx.Exec(`DELETE FROM shard_state WHERE run_id=? AND tick=?`, result.RunID, tick.Tick); err != nil {
		return domain.NewBizError(domain.CodeCheckpointCorrupt, "save_tick", "cannot reset shard rows", map[string]any{"error": err.Error()})
	}
	if _, err := tx.Exec(`DELETE FROM node_state WHERE run_id=? AND tick=?`, result.RunID, tick.Tick); err != nil {
		return domain.NewBizError(domain.CodeCheckpointCorrupt, "save_tick", "cannot reset node rows", map[string]any{"error": err.Error()})
	}
	if _, err := tx.Exec(`DELETE FROM replay_queue WHERE run_id=? AND tick=?`, result.RunID, tick.Tick); err != nil {
		return domain.NewBizError(domain.CodeCheckpointCorrupt, "save_tick", "cannot reset replay rows", map[string]any{"error": err.Error()})
	}
	if _, err := tx.Exec(`DELETE FROM gate_result WHERE run_id=? AND tick=?`, result.RunID, tick.Tick); err != nil {
		return domain.NewBizError(domain.CodeCheckpointCorrupt, "save_tick", "cannot reset gate rows", map[string]any{"error": err.Error()})
	}
	for _, shard := range tick.Shards {
		shardPayload, _ := json.Marshal(shard)
		if _, err := tx.Exec(`INSERT OR REPLACE INTO shard_state(
			run_id, tick, shard_id, node_id, target_version, visible_version, progress,
			committed_progress, preheat_ticks, available, lease_id, lease_node,
			lease_expires_tick, generation, pending_replay, budget_capacity, payload)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			result.RunID, tick.Tick, shard.ShardID, shard.NodeID, shard.TargetVersion,
			shard.VisibleVersion, shard.Progress, shard.CommittedProgress, shard.PreheatTicks,
			boolInt(shard.Available), shard.LeaseID, shard.LeaseNode, shard.LeaseExpiresTick,
			shard.Generation, shard.PendingReplay, tick.Capacity, string(shardPayload)); err != nil {
			return domain.NewBizError(domain.CodeCheckpointCorrupt, "save_tick", "cannot write shard state", map[string]any{"tick": tick.Tick, "shard": shard.ShardID, "error": err.Error()})
		}
		for _, requestID := range shard.ReplayQueue {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO replay_queue(run_id, tick, shard_id, request_id, payload) VALUES(?, ?, ?, ?, ?)`,
				result.RunID, tick.Tick, shard.ShardID, requestID, payloads[requestID]); err != nil {
				return domain.NewBizError(domain.CodeCheckpointCorrupt, "save_tick", "cannot write replay queue", map[string]any{"tick": tick.Tick, "request": requestID, "error": err.Error()})
			}
		}
	}
	for _, node := range tick.Nodes {
		nodePayload, _ := json.Marshal(node)
		if _, err := tx.Exec(`INSERT OR REPLACE INTO node_state(
			run_id, tick, node_id, active, used_bandwidth, concurrency, pending_buffer, payload)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
			result.RunID, tick.Tick, node.NodeID, boolInt(node.Active), node.UsedBandwidth,
			node.Concurrency, node.PendingBuffer, string(nodePayload)); err != nil {
			return domain.NewBizError(domain.CodeCheckpointCorrupt, "save_tick", "cannot write node state", map[string]any{"tick": tick.Tick, "node": node.NodeID, "error": err.Error()})
		}
	}
	for _, request := range tick.Requests {
		requestPayload, _ := json.Marshal(request)
		if _, err := tx.Exec(`INSERT OR REPLACE INTO gate_result(run_id, tick, request_id, payload) VALUES(?, ?, ?, ?)`,
			result.RunID, tick.Tick, request.RequestID, string(requestPayload)); err != nil {
			return domain.NewBizError(domain.CodeCheckpointCorrupt, "save_tick", "cannot write gate result", map[string]any{"tick": tick.Tick, "request": request.RequestID, "error": err.Error()})
		}
	}
	for _, lease := range tick.LeasesAcquired {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO lease_history(
			run_id, lease_id, shard_id, node_id, acquired_tick, expires_tick, released_tick, release_reason, generation)
			VALUES(?, ?, ?, ?, ?, ?, 0, '', ?)`,
			result.RunID, lease.LeaseID, lease.ShardID, lease.NodeID, lease.AcquiredTick, lease.ExpiresTick, lease.Generation); err != nil {
			return domain.NewBizError(domain.CodeCheckpointCorrupt, "save_tick", "cannot write acquired lease", map[string]any{"lease": lease.LeaseID, "error": err.Error()})
		}
	}
	for _, lease := range tick.LeasesReleased {
		if _, err := tx.Exec(`UPDATE lease_history SET released_tick=?, release_reason=? WHERE run_id=? AND lease_id=?`,
			lease.ReleasedTick, lease.ReleaseReason, result.RunID, lease.LeaseID); err != nil {
			return domain.NewBizError(domain.CodeCheckpointCorrupt, "save_tick", "cannot update released lease", map[string]any{"lease": lease.LeaseID, "error": err.Error()})
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO tick_commit(
		run_id, tick, budget_used, budget_capacity, state_hash, payload) VALUES(?, ?, ?, ?, ?, ?)`,
		result.RunID, tick.Tick, tick.UsedBandwidth, tick.Capacity, stateHash, string(tickPayload)); err != nil {
		return domain.NewBizError(domain.CodeCheckpointOrder, "save_tick", "cannot commit global tick watermark", map[string]any{"tick": tick.Tick, "error": err.Error()})
	}
	if _, err := tx.Exec(`UPDATE runs SET last_tick=? WHERE run_id=?`, tick.Tick, result.RunID); err != nil {
		return domain.NewBizError(domain.CodeCheckpointOrder, "save_tick", "cannot publish run tick watermark", map[string]any{"tick": tick.Tick, "error": err.Error()})
	}
	if err := tx.Commit(); err != nil {
		return domain.NewBizError(domain.CodeCheckpointCorrupt, "commit_tick", "checkpoint tick commit failed", map[string]any{"tick": tick.Tick, "error": err.Error()})
	}
	return nil
}

func (s *Store) Latest() (Checkpoint, error) {
	var runID, projectID, schemaValue, semanticHash string
	var lastTick int64
	err := s.db.QueryRow(`SELECT run_id, project_id, schema, semantic_hash, last_tick
		FROM runs WHERE last_tick > 0 ORDER BY created_at DESC, run_id DESC LIMIT 1`).
		Scan(&runID, &projectID, &schemaValue, &semanticHash, &lastTick)
	if err == sql.ErrNoRows {
		return Checkpoint{}, domain.NewBizError(domain.CodeNotFound, "load_checkpoint", "no committed checkpoint exists", map[string]any{"path": s.path})
	}
	if err != nil {
		return Checkpoint{}, domain.NewBizError(domain.CodeCheckpointCorrupt, "load_checkpoint", "cannot read run metadata", map[string]any{"error": err.Error()})
	}
	var maxChildTick int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(tick), 0) FROM (
		SELECT tick FROM shard_state WHERE run_id=?
		UNION ALL
		SELECT tick FROM node_state WHERE run_id=?
	)`, runID, runID).Scan(&maxChildTick); err != nil {
		return Checkpoint{}, domain.NewBizError(domain.CodeCheckpointCorrupt, "load_checkpoint", "cannot inspect shard watermark", map[string]any{"error": err.Error()})
	}
	if maxChildTick > lastTick {
		return Checkpoint{}, domain.NewBizError(domain.CodeCheckpointOrder, "load_checkpoint", "shard state exists beyond the global tick watermark", map[string]any{
			"run": runID, "child_tick": maxChildTick, "global_tick": lastTick,
		})
	}
	tick, err := s.LoadTick(runID, lastTick)
	if err != nil {
		return Checkpoint{}, err
	}
	var stateHash string
	if err := s.db.QueryRow(`SELECT state_hash FROM tick_commit WHERE run_id=? AND tick=?`, runID, lastTick).Scan(&stateHash); err != nil {
		return Checkpoint{}, domain.NewBizError(domain.CodeCheckpointCorrupt, "load_checkpoint", "cannot read committed state hash", map[string]any{"error": err.Error()})
	}
	checkpoint := Checkpoint{
		RunID: runID, ProjectID: projectID, Schema: domain.SchemaVersion(schemaValue),
		SemanticHash: semanticHash, Tick: lastTick, BudgetUsed: tick.UsedBandwidth,
		StateHash: stateHash, BudgetCapacity: tick.Capacity, Shards: tick.Shards, Nodes: tick.Nodes,
		ReplayQueue: map[string][]string{}, Requests: tick.Requests, Queue: tick.Queue,
		SnapshotVersion: SnapshotVersion,
	}
	for _, shard := range tick.Shards {
		if len(shard.ReplayQueue) > 0 {
			checkpoint.ReplayQueue[shard.ShardID] = append([]string(nil), shard.ReplayQueue...)
		}
	}
	return checkpoint, nil
}

func (s *Store) LoadTick(runID string, tick int64) (domain.TickState, error) {
	var payload, expectedHash string
	err := s.db.QueryRow(`SELECT payload, state_hash FROM tick_commit WHERE run_id=? AND tick=?`, runID, tick).Scan(&payload, &expectedHash)
	if err == sql.ErrNoRows {
		return domain.TickState{}, domain.NewBizError(domain.CodeCheckpointOrder, "load_tick", "no global tick commit exists", map[string]any{"run": runID, "tick": tick})
	}
	if err != nil {
		return domain.TickState{}, domain.NewBizError(domain.CodeCheckpointCorrupt, "load_tick", "cannot read global tick commit", map[string]any{"run": runID, "tick": tick, "error": err.Error()})
	}
	var state domain.TickState
	if err := json.Unmarshal([]byte(payload), &state); err != nil {
		return domain.TickState{}, domain.NewBizError(domain.CodeCheckpointCorrupt, "load_tick", "cannot decode committed tick", map[string]any{"run": runID, "tick": tick, "error": err.Error()})
	}
	if actual := hashPayload([]byte(payload)); actual != expectedHash {
		return domain.TickState{}, domain.NewBizError(domain.CodeCheckpointCorrupt, "load_tick", "committed tick hash mismatch", map[string]any{
			"run": runID, "tick": tick, "expected": expectedHash, "actual": actual,
		})
	}
	var shardCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM shard_state WHERE run_id=? AND tick=?`, runID, tick).Scan(&shardCount); err != nil {
		return domain.TickState{}, domain.NewBizError(domain.CodeCheckpointCorrupt, "load_tick", "cannot count shard state", map[string]any{"error": err.Error()})
	}
	if shardCount != len(state.Shards) {
		return domain.TickState{}, domain.NewBizError(domain.CodeCheckpointOrder, "load_tick", "shard rows do not match the committed tick", map[string]any{
			"run": runID, "tick": tick, "rows": shardCount, "committed": len(state.Shards),
		})
	}
	var nodeCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM node_state WHERE run_id=? AND tick=?`, runID, tick).Scan(&nodeCount); err != nil {
		return domain.TickState{}, domain.NewBizError(domain.CodeCheckpointCorrupt, "load_tick", "cannot count node state", map[string]any{"error": err.Error()})
	}
	if nodeCount != len(state.Nodes) {
		return domain.TickState{}, domain.NewBizError(domain.CodeCheckpointOrder, "load_tick", "node rows do not match the committed tick", map[string]any{
			"run": runID, "tick": tick, "rows": nodeCount, "committed": len(state.Nodes),
		})
	}
	return state, nil
}

func TickHash(tick domain.TickState) string {
	payload, _ := json.Marshal(tick)
	return hashPayload(payload)
}

func hashPayload(payload []byte) string {
	hasher := fnv.New64a()
	_, _ = hasher.Write(payload)
	return fmt.Sprintf("%016x", hasher.Sum64())
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
