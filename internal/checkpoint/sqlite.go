package checkpoint

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"quotaflow/internal/domain"
	"quotaflow/internal/engine"
)

const (
	PhaseTenants      = "tenants_written"
	PhasePlacements   = "placements_written"
	PhaseNodes        = "nodes_written"
	PhaseMigrations   = "migrations_written"
	PhasePauses       = "pauses_written"
	PhaseStatesReady  = "states_ready"
	PhaseBeforeGlobal = "before_global_tick"
	PhaseAfterGlobal  = "after_global_tick"
)

type Store struct {
	db      *sql.DB
	path    string
	version string
	hash    string
}

type CommitHook func(phase string) error

type Meta struct {
	Version     string `json:"version"`
	ProjectHash string `json:"project_hash"`
	Tick        int64  `json:"tick"`
}

func Remove(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return domain.NewBizError(domain.ECheckpointIO, candidate, "remove checkpoint: %v", err)
		}
	}
	return nil
}

// Open validates the database before initializing its schema. A version or
// project-hash mismatch clears the complete checkpoint and reports rebuilt.
func Open(path, version, projectHash string) (*Store, bool, error) {
	if version == "" {
		return nil, false, domain.NewBizError(domain.ECheckpointVersion, "$.version", "empty checkpoint version")
	}
	if projectHash == "" {
		return nil, false, domain.NewBizError(domain.ECheckpointVersion, "$.project_hash", "empty project hash")
	}
	info, statErr := os.Stat(path)
	existed := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, false, domain.NewBizError(domain.ECheckpointIO, path, "stat checkpoint: %v", statErr)
	}
	if existed && info.Size() == 0 {
		return nil, false, domain.NewBizError(domain.ECheckpointTruncated, path, "checkpoint file is empty")
	}
	if existed {
		if err := validate(path); err != nil {
			return nil, false, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, domain.NewBizError(domain.ECheckpointIO, path, "create checkpoint directory: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, false, mapSQLiteError(path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON;`); err != nil {
		db.Close()
		return nil, false, mapSQLiteError(path, err)
	}
	if existed {
		if err := checkRequiredTables(db, path); err != nil {
			db.Close()
			return nil, false, err
		}
	}
	if err := ensureSchema(db); err != nil {
		db.Close()
		return nil, false, mapSQLiteError(path, err)
	}
	store := &Store{db: db, path: path, version: version, hash: projectHash}
	storedVersion, hasVersion, err := readMeta(db, "engine_version")
	if err != nil {
		db.Close()
		return nil, false, err
	}
	storedHash, hasHash, err := readMeta(db, "project_hash")
	if err != nil {
		db.Close()
		return nil, false, err
	}
	rebuilt := false
	if !hasVersion || !hasHash || storedVersion != version || storedHash != projectHash {
		if err := store.resetUnlocked(); err != nil {
			db.Close()
			return nil, false, err
		}
		rebuilt = existed
	}
	return store, rebuilt, nil
}

func checkRequiredTables(db *sql.DB, path string) error {
	required := []string{
		"meta", "tenant_state", "placement", "node_state", "domain_state",
		"migration_state", "pause_state", "tick_commits",
	}
	found := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return mapSQLiteError(path, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return mapSQLiteError(path, err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		return mapSQLiteError(path, err)
	}
	var missing []string
	for _, table := range required {
		if !found[table] {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		return domain.NewBizError(domain.ECheckpointTruncated, path,
			"checkpoint is missing required tables: %s", strings.Join(missing, ","))
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Path() string { return s.path }

func ensureSchema(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS tenant_state (
			tenant_id TEXT PRIMARY KEY, weight INTEGER NOT NULL, quota INTEGER NOT NULL,
			used INTEGER NOT NULL, reserve INTEGER NOT NULL, encroached INTEGER NOT NULL,
			pending_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS placement (
			tenant_id TEXT NOT NULL, node_id TEXT NOT NULL, amount INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, node_id)
		)`,
		`CREATE TABLE IF NOT EXISTS node_state (
			node_id TEXT PRIMARY KEY, capacity INTEGER NOT NULL, used INTEGER NOT NULL,
			failed INTEGER NOT NULL, fault_domain TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS domain_state (
			domain_id TEXT PRIMARY KEY, parent_id TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS migration_state (
			migration_id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, from_node TEXT NOT NULL,
			to_node TEXT NOT NULL, amount INTEGER NOT NULL, start_tick INTEGER NOT NULL,
			end_tick INTEGER NOT NULL, status TEXT NOT NULL, constraints_json TEXT NOT NULL,
			enable_basis TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS pause_state (
			tenant_id TEXT PRIMARY KEY, from_node TEXT NOT NULL, to_node TEXT NOT NULL,
			start_tick INTEGER NOT NULL, expected_end_tick INTEGER NOT NULL,
			reserve_milli INTEGER NOT NULL, enable_basis TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tick_commits (
			tick INTEGER PRIMARY KEY, committed_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return ensureColumn(db, "migration_state", "enable_basis", "TEXT NOT NULL DEFAULT ''")
}

func ensureColumn(db *sql.DB, table, column, declaration string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + declaration)
	return err
}

func validate(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return mapSQLiteError(path, err)
	}
	if info.Size() == 0 {
		return domain.NewBizError(domain.ECheckpointTruncated, path, "checkpoint file is empty")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return mapSQLiteError(path, err)
	}
	defer db.Close()
	var result string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&result); err != nil {
		return mapSQLiteError(path, err)
	}
	if result != "ok" {
		return domain.NewBizError(domain.ECheckpointCorrupt, path, "quick_check: %s", result)
	}
	return nil
}

func (s *Store) resetUnlocked() error {
	tx, err := s.db.Begin()
	if err != nil {
		return mapSQLiteError(s.path, err)
	}
	defer tx.Rollback()
	for _, table := range []string{
		"tenant_state", "placement", "node_state", "domain_state", "migration_state",
		"pause_state", "tick_commits", "meta",
	} {
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			return mapSQLiteError(s.path, err)
		}
	}
	if err := setMeta(tx, "engine_version", s.version); err != nil {
		return err
	}
	if err := setMeta(tx, "project_hash", s.hash); err != nil {
		return err
	}
	if err := setMeta(tx, "current_tick", "0"); err != nil {
		return err
	}
	if err := setMeta(tx, "next_migration", "0"); err != nil {
		return err
	}
	if err := setMeta(tx, "applied_edits", "{}"); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO tick_commits(tick, committed_at) VALUES(0, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return mapSQLiteError(s.path, err)
	}
	return tx.Commit()
}

func (s *Store) Reset() error {
	return s.resetUnlocked()
}

func (s *Store) EnsureInitial(state *engine.State) error {
	if state == nil || state.Tick != 0 {
		return domain.NewBizError(domain.EInternalInvariant, "$.state", "initial checkpoint state must have tick 0")
	}
	tick, _, err := s.metaInt64("current_tick")
	if err != nil {
		return err
	}
	count := 0
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tenant_state`).Scan(&count); err != nil {
		return mapSQLiteError(s.path, err)
	}
	if tick == 0 && count == 0 {
		return s.CommitWithHook(0, state, nil)
	}
	return nil
}

func (s *Store) Commit(tick int64, state *engine.State) error {
	return s.CommitWithHook(tick, state, nil)
}

// CommitWithHook writes state rows in dependency order and updates the global
// tick last. Every hook phase is inside one SQLite transaction; a hook error
// rolls back the whole tick, so a restart can only observe the previous tick.
func (s *Store) CommitWithHook(tick int64, state *engine.State, hook CommitHook) error {
	if state == nil || state.Tick != tick {
		return domain.NewBizError(domain.ECheckpointCorrupt, "$.state.tick",
			"state tick %d does not match commit tick %d", stateTick(state), tick)
	}
	current, ok, err := s.metaInt64("current_tick")
	if err != nil {
		return err
	}
	if !ok {
		return domain.NewBizError(domain.ECheckpointCorrupt, "$.meta.current_tick", "missing current tick")
	}
	if tick < current || tick > current+1 {
		return domain.NewBizError(domain.ECheckpointCorrupt, "$.tick",
			"checkpoint commits must be sequential: current=%d requested=%d", current, tick)
	}
	if tick == current && tick > 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return mapSQLiteError(s.path, err)
	}
	defer tx.Rollback()
	if err := writeTenants(tx, state); err != nil {
		return err
	}
	if err := hookPhase(hook, PhaseTenants); err != nil {
		return err
	}
	if err := writePlacements(tx, state); err != nil {
		return err
	}
	if err := hookPhase(hook, PhasePlacements); err != nil {
		return err
	}
	if err := writeNodes(tx, state); err != nil {
		return err
	}
	if err := hookPhase(hook, PhaseNodes); err != nil {
		return err
	}
	if err := writeDomains(tx, state); err != nil {
		return err
	}
	if err := writeMigrations(tx, state); err != nil {
		return err
	}
	if err := hookPhase(hook, PhaseMigrations); err != nil {
		return err
	}
	if err := writePauses(tx, state); err != nil {
		return err
	}
	if err := hookPhase(hook, PhasePauses); err != nil {
		return err
	}
	if err := setMeta(tx, "next_migration", strconv.Itoa(state.NextMigration)); err != nil {
		return err
	}
	applied, err := json.Marshal(state.AppliedEdits)
	if err != nil {
		return domain.NewBizError(domain.ECheckpointIO, s.path, "encode applied edits: %v", err)
	}
	if err := setMeta(tx, "applied_edits", string(applied)); err != nil {
		return err
	}
	if err := hookPhase(hook, PhaseStatesReady); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO tick_commits(tick, committed_at) VALUES(?, ?)`,
		tick, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return mapSQLiteError(s.path, err)
	}
	if err := hookPhase(hook, PhaseBeforeGlobal); err != nil {
		return err
	}
	if err := setMeta(tx, "current_tick", strconv.FormatInt(tick, 10)); err != nil {
		return err
	}
	if err := hookPhase(hook, PhaseAfterGlobal); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return mapSQLiteError(s.path, err)
	}
	return nil
}

func hookPhase(hook CommitHook, phase string) error {
	if hook == nil {
		return nil
	}
	if err := hook(phase); err != nil {
		if _, ok := err.(*domain.BizError); ok {
			return err
		}
		return domain.NewBizError(domain.ECheckpointIO, "$.commit."+phase, "commit interrupted: %v", err)
	}
	return nil
}

func stateTick(state *engine.State) int64 {
	if state == nil {
		return -1
	}
	return state.Tick
}

func writeTenants(tx *sql.Tx, state *engine.State) error {
	if _, err := tx.Exec(`DELETE FROM tenant_state`); err != nil {
		return mapSQLiteError("tenant_state", err)
	}
	for _, id := range sortedTenantIDs(state) {
		tenant := state.Tenants[id]
		pending, err := json.Marshal(tenant.Pending)
		if err != nil {
			return domain.NewBizError(domain.ECheckpointIO, "$.tenants."+id+".pending", "encode pending: %v", err)
		}
		if _, err := tx.Exec(`INSERT INTO tenant_state(
			tenant_id, weight, quota, used, reserve, encroached, pending_json
		) VALUES(?, ?, ?, ?, ?, ?, ?)`,
			tenant.TenantID, tenant.Weight, tenant.Quota, tenant.Used, tenant.Reserve, tenant.Encroached, string(pending)); err != nil {
			return mapSQLiteError("tenant_state", err)
		}
	}
	return nil
}

func writePlacements(tx *sql.Tx, state *engine.State) error {
	if _, err := tx.Exec(`DELETE FROM placement`); err != nil {
		return mapSQLiteError("placement", err)
	}
	for _, tenantID := range sortedPlacementTenants(state) {
		for _, nodeID := range sortedPlacementNodes(state, tenantID) {
			amount := state.Placements[tenantID][nodeID]
			if amount <= 0 {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO placement(tenant_id, node_id, amount) VALUES(?, ?, ?)`,
				tenantID, nodeID, amount); err != nil {
				return mapSQLiteError("placement", err)
			}
		}
	}
	return nil
}

func writeNodes(tx *sql.Tx, state *engine.State) error {
	if _, err := tx.Exec(`DELETE FROM node_state`); err != nil {
		return mapSQLiteError("node_state", err)
	}
	for _, id := range sortedNodeIDs(state) {
		node := state.Nodes[id]
		failed := 0
		if node.Failed {
			failed = 1
		}
		if _, err := tx.Exec(`INSERT INTO node_state(node_id, capacity, used, failed, fault_domain)
			VALUES(?, ?, ?, ?, ?)`, node.NodeID, node.Capacity, node.Used, failed, node.FaultDomain); err != nil {
			return mapSQLiteError("node_state", err)
		}
	}
	return nil
}

func writeDomains(tx *sql.Tx, state *engine.State) error {
	if _, err := tx.Exec(`DELETE FROM domain_state`); err != nil {
		return mapSQLiteError("domain_state", err)
	}
	for _, id := range sortedDomainIDs(state) {
		fd := state.Domains[id]
		if _, err := tx.Exec(`INSERT INTO domain_state(domain_id, parent_id) VALUES(?, ?)`,
			fd.ID, fd.Parent); err != nil {
			return mapSQLiteError("domain_state", err)
		}
	}
	return nil
}

func writeMigrations(tx *sql.Tx, state *engine.State) error {
	if _, err := tx.Exec(`DELETE FROM migration_state`); err != nil {
		return mapSQLiteError("migration_state", err)
	}
	for _, id := range sortedMigrationIDs(state) {
		migration := state.Migrations[id]
		constraints, err := json.Marshal(migration.Constraints)
		if err != nil {
			return domain.NewBizError(domain.ECheckpointIO, "$.migrations."+id, "encode constraints: %v", err)
		}
		if _, err := tx.Exec(`INSERT INTO migration_state(
			migration_id, tenant_id, from_node, to_node, amount, start_tick, end_tick, status, constraints_json, enable_basis
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			migration.ID, migration.TenantID, migration.FromNode, migration.ToNode,
			migration.Amount, migration.StartTick, migration.EndTick, migration.Status, string(constraints),
			migration.EnableBasis); err != nil {
			return mapSQLiteError("migration_state", err)
		}
	}
	return nil
}

func writePauses(tx *sql.Tx, state *engine.State) error {
	if _, err := tx.Exec(`DELETE FROM pause_state`); err != nil {
		return mapSQLiteError("pause_state", err)
	}
	for _, id := range sortedPauseIDs(state) {
		pause := state.Pauses[id]
		if _, err := tx.Exec(`INSERT INTO pause_state(
			tenant_id, from_node, to_node, start_tick, expected_end_tick, reserve_milli, enable_basis
		) VALUES(?, ?, ?, ?, ?, ?, ?)`,
			pause.TenantID, pause.FromNode, pause.ToNode, pause.StartTick,
			pause.ExpectedEndTick, pause.ReserveMilli, pause.EnableBasis); err != nil {
			return mapSQLiteError("pause_state", err)
		}
	}
	return nil
}

func (s *Store) Load() (*engine.State, Meta, error) {
	version, ok, err := readMeta(s.db, "engine_version")
	if err != nil {
		return nil, Meta{}, err
	}
	if !ok || version != s.version {
		return nil, Meta{}, domain.NewBizError(domain.ECheckpointVersion, "$.meta.engine_version",
			"checkpoint version mismatch: stored=%q expected=%q", version, s.version)
	}
	hash, ok, err := readMeta(s.db, "project_hash")
	if err != nil {
		return nil, Meta{}, err
	}
	if !ok || hash != s.hash {
		return nil, Meta{}, domain.NewBizError(domain.ECheckpointVersion, "$.meta.project_hash",
			"project snapshot hash mismatch")
	}
	tick, ok, err := s.metaInt64("current_tick")
	if err != nil {
		return nil, Meta{}, err
	}
	if !ok {
		return nil, Meta{}, domain.NewBizError(domain.ECheckpointCorrupt, "$.meta.current_tick", "missing current tick")
	}
	var tickCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tick_commits WHERE tick = ?`, tick).Scan(&tickCount); err != nil {
		return nil, Meta{}, mapSQLiteError(s.path, err)
	}
	if tickCount != 1 {
		return nil, Meta{}, domain.NewBizError(domain.ECheckpointCorrupt, "$.tick_commits",
			"global tick %d has no matching commit marker", tick)
	}
	state, err := s.loadState(tick)
	if err != nil {
		return nil, Meta{}, err
	}
	return state, Meta{Version: version, ProjectHash: hash, Tick: tick}, nil
}

func (s *Store) loadState(tick int64) (*engine.State, error) {
	state := &engine.State{
		Tick: tick, Tenants: map[string]engine.TenantState{}, Nodes: map[string]engine.NodeState{},
		Placements: map[string]map[string]int64{}, Domains: map[string]domain.FaultDomain{},
		Migrations: map[string]engine.Migration{}, Pauses: map[string]engine.Pause{},
		AppliedEdits: map[string]bool{},
	}
	rows, err := s.db.Query(`SELECT tenant_id, weight, quota, used, reserve, encroached, pending_json FROM tenant_state`)
	if err != nil {
		return nil, mapSQLiteError(s.path, err)
	}
	for rows.Next() {
		var tenant engine.TenantState
		var pendingJSON string
		if err := rows.Scan(&tenant.TenantID, &tenant.Weight, &tenant.Quota, &tenant.Used,
			&tenant.Reserve, &tenant.Encroached, &pendingJSON); err != nil {
			rows.Close()
			return nil, mapSQLiteError(s.path, err)
		}
		if err := json.Unmarshal([]byte(pendingJSON), &tenant.Pending); err != nil {
			rows.Close()
			return nil, domain.NewBizError(domain.ECheckpointCorrupt, "$.tenants."+tenant.TenantID+".pending",
				"invalid pending JSON: %v", err)
		}
		state.Tenants[tenant.TenantID] = tenant
		state.Placements[tenant.TenantID] = map[string]int64{}
	}
	if err := rows.Close(); err != nil {
		return nil, mapSQLiteError(s.path, err)
	}
	rows, err = s.db.Query(`SELECT tenant_id, node_id, amount FROM placement`)
	if err != nil {
		return nil, mapSQLiteError(s.path, err)
	}
	for rows.Next() {
		var tenantID, nodeID string
		var amount int64
		if err := rows.Scan(&tenantID, &nodeID, &amount); err != nil {
			rows.Close()
			return nil, mapSQLiteError(s.path, err)
		}
		if state.Placements[tenantID] == nil {
			state.Placements[tenantID] = map[string]int64{}
		}
		state.Placements[tenantID][nodeID] = amount
	}
	if err := rows.Close(); err != nil {
		return nil, mapSQLiteError(s.path, err)
	}
	rows, err = s.db.Query(`SELECT node_id, capacity, used, failed, fault_domain FROM node_state`)
	if err != nil {
		return nil, mapSQLiteError(s.path, err)
	}
	for rows.Next() {
		var node engine.NodeState
		var failed int
		if err := rows.Scan(&node.NodeID, &node.Capacity, &node.Used, &failed, &node.FaultDomain); err != nil {
			rows.Close()
			return nil, mapSQLiteError(s.path, err)
		}
		node.Failed = failed != 0
		state.Nodes[node.NodeID] = node
	}
	if err := rows.Close(); err != nil {
		return nil, mapSQLiteError(s.path, err)
	}
	rows, err = s.db.Query(`SELECT domain_id, parent_id FROM domain_state`)
	if err != nil {
		return nil, mapSQLiteError(s.path, err)
	}
	for rows.Next() {
		var fd domain.FaultDomain
		if err := rows.Scan(&fd.ID, &fd.Parent); err != nil {
			rows.Close()
			return nil, mapSQLiteError(s.path, err)
		}
		state.Domains[fd.ID] = fd
	}
	if err := rows.Close(); err != nil {
		return nil, mapSQLiteError(s.path, err)
	}
	rows, err = s.db.Query(`SELECT migration_id, tenant_id, from_node, to_node, amount,
		start_tick, end_tick, status, constraints_json, enable_basis FROM migration_state`)
	if err != nil {
		return nil, mapSQLiteError(s.path, err)
	}
	for rows.Next() {
		var migration engine.Migration
		var constraintsJSON string
		if err := rows.Scan(&migration.ID, &migration.TenantID, &migration.FromNode, &migration.ToNode,
			&migration.Amount, &migration.StartTick, &migration.EndTick, &migration.Status,
			&constraintsJSON, &migration.EnableBasis); err != nil {
			rows.Close()
			return nil, mapSQLiteError(s.path, err)
		}
		if err := json.Unmarshal([]byte(constraintsJSON), &migration.Constraints); err != nil {
			rows.Close()
			return nil, domain.NewBizError(domain.ECheckpointCorrupt, "$.migrations."+migration.ID,
				"invalid constraints JSON: %v", err)
		}
		state.Migrations[migration.ID] = migration
	}
	if err := rows.Close(); err != nil {
		return nil, mapSQLiteError(s.path, err)
	}
	rows, err = s.db.Query(`SELECT tenant_id, from_node, to_node, start_tick,
		expected_end_tick, reserve_milli, enable_basis FROM pause_state`)
	if err != nil {
		return nil, mapSQLiteError(s.path, err)
	}
	for rows.Next() {
		var pause engine.Pause
		if err := rows.Scan(&pause.TenantID, &pause.FromNode, &pause.ToNode, &pause.StartTick,
			&pause.ExpectedEndTick, &pause.ReserveMilli, &pause.EnableBasis); err != nil {
			rows.Close()
			return nil, mapSQLiteError(s.path, err)
		}
		state.Pauses[pause.TenantID] = pause
	}
	if err := rows.Close(); err != nil {
		return nil, mapSQLiteError(s.path, err)
	}
	nextMigration, ok, err := s.metaInt64("next_migration")
	if err != nil {
		return nil, err
	}
	if ok {
		state.NextMigration = int(nextMigration)
	}
	appliedJSON, ok, err := readMeta(s.db, "applied_edits")
	if err != nil {
		return nil, err
	}
	if ok && appliedJSON != "" {
		if err := json.Unmarshal([]byte(appliedJSON), &state.AppliedEdits); err != nil {
			return nil, domain.NewBizError(domain.ECheckpointCorrupt, "$.meta.applied_edits",
				"invalid applied edits JSON: %v", err)
		}
	}
	return state, nil
}

func mapSQLiteError(path string, err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "not a database"),
		strings.Contains(message, "malformed"),
		strings.Contains(message, "disk image is malformed"),
		strings.Contains(message, "no such table"):
		return domain.NewBizError(domain.ECheckpointTruncated, path, "checkpoint is truncated or corrupt: %v", err)
	case strings.Contains(message, "locked"):
		return domain.NewBizError(domain.ECheckpointIO, path, "checkpoint is locked: %v", err)
	default:
		return domain.NewBizError(domain.ECheckpointIO, path, "sqlite: %v", err)
	}
}

func setMeta(tx *sql.Tx, key, value string) error {
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
		return mapSQLiteError("meta."+key, err)
	}
	return nil
}

func readMeta(db *sql.DB, key string) (string, bool, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, mapSQLiteError("meta."+key, err)
	}
	return value, true, nil
}

func (s *Store) metaInt64(key string) (int64, bool, error) {
	value, ok, err := readMeta(s.db, key)
	if err != nil || !ok {
		return 0, ok, err
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false, domain.NewBizError(domain.ECheckpointCorrupt, "$.meta."+key,
			"invalid integer %q", value)
	}
	return parsed, true, nil
}

func sortedTenantIDs(state *engine.State) []string {
	return sortedMapKeys(state.Tenants)
}

func sortedNodeIDs(state *engine.State) []string {
	return sortedMapKeys(state.Nodes)
}

func sortedDomainIDs(state *engine.State) []string {
	return sortedMapKeys(state.Domains)
}

func sortedMigrationIDs(state *engine.State) []string {
	return sortedMapKeys(state.Migrations)
}

func sortedPauseIDs(state *engine.State) []string {
	return sortedMapKeys(state.Pauses)
}

func sortedPlacementTenants(state *engine.State) []string {
	return sortedMapKeys(state.Placements)
}

func sortedPlacementNodes(state *engine.State, tenantID string) []string {
	return sortedMapKeys(state.Placements[tenantID])
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func Summary(meta Meta) string {
	return fmt.Sprintf("version=%s tick=%d hash=%s", meta.Version, meta.Tick, meta.ProjectHash)
}
