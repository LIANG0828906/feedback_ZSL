package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"

	"tlscheck/internal/bizerr"
	"tlscheck/internal/engine"
	"tlscheck/internal/model"
)

type Checkpoint struct {
	ProjectID        string                           `json:"project_id"`
	CanonicalVersion string                           `json:"canonical_version"`
	SnapshotHash     string                           `json:"snapshot_hash"`
	CommittedTimeMs  int64                            `json:"committed_time_ms"`
	GlobalSequence   int                              `json:"global_sequence"`
	States           map[string]*engine.EndpointState `json:"states"`
	Results          []*engine.RequestResult          `json:"results"`
}

type SaveOptions struct {
	FaultBeforeGlobalSequence bool
}

type Store struct {
	db   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		if info.Size() < 100 {
			return nil, bizerr.Newf(bizerr.ECheckpointTruncated,
				"SQLite checkpoint %s is only %d bytes; expected at least 100-byte header", path, info.Size()).
				WithContext("path", path).WithContext("size", info.Size())
		}
		header := make([]byte, 16)
		f, err := os.Open(path)
		if err == nil {
			_, _ = f.Read(header)
			_ = f.Close()
			if string(header) != "SQLite format 3\x00" {
				return nil, bizerr.New(bizerr.ECheckpointTruncated, "checkpoint file has invalid SQLite header").
					WithContext("path", path)
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, bizerr.New(bizerr.ESQLite, err.Error())
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, classifyCorrupt(err, path)
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		_ = db.Close()
		return nil, classifyCorrupt(err, path)
	}
	if integrity != "ok" {
		_ = db.Close()
		return nil, bizerr.Newf(bizerr.ECheckpointTruncated, "integrity_check returned %q", integrity).
			WithContext("path", path)
	}
	st := &Store{db: db, path: path}
	if err := st.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return st, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init(ctx context.Context) error {
	ddl := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS endpoint_state (
			endpoint_id TEXT PRIMARY KEY, committed_time_ms INTEGER NOT NULL, state_json TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS chain_state (
			endpoint_id TEXT NOT NULL, chain_id TEXT NOT NULL, role TEXT NOT NULL,
			valid INTEGER NOT NULL, committed_update_id TEXT, committed_at_ms INTEGER NOT NULL,
			state_json TEXT NOT NULL, PRIMARY KEY(endpoint_id, chain_id, role))`,
		`CREATE TABLE IF NOT EXISTS wait_queue (
			endpoint_id TEXT NOT NULL, endpoint_queue_seq INTEGER NOT NULL,
			request_id TEXT NOT NULL, event_sequence INTEGER NOT NULL,
			enqueued_at_ms INTEGER NOT NULL, admitted_at_ms INTEGER NOT NULL DEFAULT 0,
			has_admission INTEGER NOT NULL, status TEXT NOT NULL, primary_gate TEXT NOT NULL,
			violations_json TEXT NOT NULL, PRIMARY KEY(endpoint_id, endpoint_queue_seq))`,
		`CREATE TABLE IF NOT EXISTS request_result (
			request_id TEXT PRIMARY KEY, endpoint_id TEXT NOT NULL, event_sequence INTEGER NOT NULL,
			status TEXT NOT NULL, gate_category TEXT NOT NULL, result_json TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS step_state (
			request_id TEXT NOT NULL, chain_id TEXT NOT NULL, level_index INTEGER NOT NULL,
			code TEXT NOT NULL, passed INTEGER NOT NULL, reused INTEGER NOT NULL,
			level_json TEXT NOT NULL, PRIMARY KEY(request_id, chain_id, level_index))`,
	}
	for _, q := range ddl {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return bizerr.New(bizerr.ESQLite, err.Error())
		}
	}
	return nil
}

func (s *Store) Save(project model.Project, cp *Checkpoint, opts SaveOptions) error {
	if cp.States == nil {
		cp.States = map[string]*engine.EndpointState{}
	}
	cp.CanonicalVersion = model.CanonicalFormat
	hash, err := project.CanonicalHash()
	if err != nil {
		return err
	}
	cp.SnapshotHash = hash
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return bizerr.New(bizerr.ESQLite, err.Error())
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM endpoint_state`); err != nil {
		return sqlError(err)
	}
	if _, err := tx.Exec(`DELETE FROM chain_state`); err != nil {
		return sqlError(err)
	}
	if _, err := tx.Exec(`DELETE FROM wait_queue`); err != nil {
		return sqlError(err)
	}
	if _, err := tx.Exec(`DELETE FROM request_result`); err != nil {
		return sqlError(err)
	}
	if _, err := tx.Exec(`DELETE FROM step_state`); err != nil {
		return sqlError(err)
	}

	epIDs := make([]string, 0, len(cp.States))
	for id := range cp.States {
		epIDs = append(epIDs, id)
	}
	// Stable order is diagnostic; relational primary keys remain authoritative.
	for _, epID := range epIDs {
		st := cp.States[epID]
		if st == nil {
			continue
		}
		stateJSON, err := json.Marshal(st)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO endpoint_state(endpoint_id,committed_time_ms,state_json) VALUES(?,?,?)`,
			epID, cp.CommittedTimeMs, string(stateJSON)); err != nil {
			return sqlError(err)
		}
		if err := saveChainStates(tx, project, epID, st, cp.CommittedTimeMs); err != nil {
			return err
		}
		items := append(append([]*engine.QueueItem{}, st.History...), st.Queue...)
		for _, item := range items {
			if item == nil {
				continue
			}
			vj, _ := json.Marshal(item.ViolatedConstraints)
			admittedAt := int64(0)
			has := 0
			if item.HasAdmission {
				admittedAt = item.AdmittedAtMs
				has = 1
			}
			if _, err := tx.Exec(`INSERT INTO wait_queue VALUES(?,?,?,?,?,?,?,?,?,?)`,
				item.EndpointID, item.EndpointQueueSeq, item.RequestID, item.EventSequence,
				item.EnqueuedAtMs, admittedAt, has, item.Status, item.PrimaryGate, string(vj)); err != nil {
				return sqlError(err)
			}
		}
	}
	for _, rr := range cp.Results {
		if rr == nil {
			continue
		}
		rj, err := json.Marshal(rr)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO request_result VALUES(?,?,?,?,?,?)`,
			rr.RequestID, rr.EndpointID, rr.EventSequence, rr.Status, rr.GateCategory, string(rj)); err != nil {
			return sqlError(err)
		}
		for _, cr := range rr.ChainResults {
			for _, lvl := range cr.Levels {
				lj, _ := json.Marshal(lvl)
				passed, reused := 0, 0
				if lvl.Passed {
					passed = 1
				}
				if lvl.Reused {
					reused = 1
				}
				if _, err := tx.Exec(`INSERT OR REPLACE INTO step_state VALUES(?,?,?,?,?,?,?)`,
					rr.RequestID, cr.ChainID, lvl.Index, lvl.Code, passed, reused, string(lj)); err != nil {
					return sqlError(err)
				}
			}
		}
	}

	// Metadata except the global commit sequence can be written before the
	// final linearization point. global_sequence is deliberately last.
	meta := map[string]any{
		"project_id": cp.ProjectID, "canonical_version": cp.CanonicalVersion,
		"snapshot_hash": cp.SnapshotHash, "committed_time_ms": cp.CommittedTimeMs,
	}
	for k, v := range meta {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO checkpoint_meta(key,value) VALUES(?,?)`, k, fmt.Sprint(v)); err != nil {
			return sqlError(err)
		}
	}
	if opts.FaultBeforeGlobalSequence {
		return bizerr.New(bizerr.EInterrupt, "fault injected after endpoint/chain writes and before global sequence commit")
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO checkpoint_meta(key,value) VALUES('global_sequence',?)`, cp.GlobalSequence); err != nil {
		return sqlError(err)
	}
	if err := tx.Commit(); err != nil {
		return sqlError(err)
	}
	return nil
}

func saveChainStates(tx *sql.Tx, project model.Project, epID string, st *engine.EndpointState, atMs int64) error {
	ep, epOK := project.Endpoint(epID)
	rows := []struct {
		id   string
		role string
	}{{st.CurrentChainID, "CURRENT"}, {st.LastValidChainID, "LAST_VALID"}}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.id == "" || seen[row.role+":"+row.id] {
			continue
		}
		seen[row.role+":"+row.id] = true
		valid := false
		if ch, ok := project.Chain(row.id); ok && epOK {
			pin := ""
			if ep.Pinned {
				pin = ep.PinDigest
			} else if ch.Pinned {
				pin = ch.PinDigest
			}
			valid = engine.ValidateChain(ch, atMs, pin).Valid
		}
		stateJSON, _ := json.Marshal(map[string]any{
			"chain_id": row.id, "role": row.role, "valid": valid,
			"committed_update_id": st.LastCommittedUpdateID,
		})
		validInt := 0
		if valid {
			validInt = 1
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO chain_state VALUES(?,?,?,?,?,?,?)`,
			epID, row.id, row.role, validInt, st.LastCommittedUpdateID, st.LastCommitAtMs, string(stateJSON)); err != nil {
			return sqlError(err)
		}
	}
	return nil
}

func (s *Store) Load() (*Checkpoint, error) {
	rows, err := s.db.Query(`SELECT key,value FROM checkpoint_meta`)
	if err != nil {
		return nil, classifyCorrupt(err, s.path)
	}
	meta := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			_ = rows.Close()
			return nil, classifyCorrupt(err, s.path)
		}
		meta[k] = v
	}
	_ = rows.Close()
	if seq, ok := meta["global_sequence"]; !ok || seq == "" {
		var count int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM endpoint_state`).Scan(&count)
		if count > 0 {
			return nil, bizerr.New(bizerr.ECheckpointTruncated, "checkpoint has endpoint rows but no committed global_sequence").
				WithContext("path", s.path)
		}
		return nil, nil
	}
	cp := &Checkpoint{
		ProjectID: meta["project_id"], CanonicalVersion: meta["canonical_version"],
		SnapshotHash: meta["snapshot_hash"],
	}
	fmt.Sscanf(meta["committed_time_ms"], "%d", &cp.CommittedTimeMs)
	fmt.Sscanf(meta["global_sequence"], "%d", &cp.GlobalSequence)
	cp.States = map[string]*engine.EndpointState{}
	erows, err := s.db.Query(`SELECT endpoint_id,state_json FROM endpoint_state`)
	if err != nil {
		return nil, classifyCorrupt(err, s.path)
	}
	for erows.Next() {
		var id, js string
		if err := erows.Scan(&id, &js); err != nil {
			_ = erows.Close()
			return nil, classifyCorrupt(err, s.path)
		}
		st := &engine.EndpointState{}
		if err := json.Unmarshal([]byte(js), st); err != nil {
			_ = erows.Close()
			return nil, bizerr.New(bizerr.ECheckpointTruncated, "endpoint state JSON is truncated: "+err.Error())
		}
		cp.States[id] = st
	}
	_ = erows.Close()
	rrows, err := s.db.Query(`SELECT result_json FROM request_result`)
	if err != nil {
		return nil, classifyCorrupt(err, s.path)
	}
	for rrows.Next() {
		var js string
		if err := rrows.Scan(&js); err != nil {
			_ = rrows.Close()
			return nil, classifyCorrupt(err, s.path)
		}
		rr := &engine.RequestResult{}
		if err := json.Unmarshal([]byte(js), rr); err != nil {
			_ = rrows.Close()
			return nil, bizerr.New(bizerr.ECheckpointTruncated, "request result JSON is truncated: "+err.Error())
		}
		cp.Results = append(cp.Results, rr)
	}
	_ = rrows.Close()
	return cp, nil
}

func (s *Store) LoadForProject(project model.Project) (*Checkpoint, error) {
	cp, err := s.Load()
	if err != nil || cp == nil {
		return cp, err
	}
	if cp.CanonicalVersion != model.CanonicalFormat {
		return nil, bizerr.Newf(bizerr.ECheckpointVersion,
			"checkpoint canonical version %q does not match binary %q", cp.CanonicalVersion, model.CanonicalFormat).
			WithContext("checkpoint_version", cp.CanonicalVersion).
			WithContext("binary_version", model.CanonicalFormat)
	}
	hash, err := project.CanonicalHash()
	if err != nil {
		return nil, err
	}
	if cp.SnapshotHash != hash {
		return nil, bizerr.Newf(bizerr.ESnapshotMismatch,
			"checkpoint snapshot %s does not match project %s", short(cp.SnapshotHash), short(hash)).
			WithContext("checkpoint_hash", cp.SnapshotHash).WithContext("project_hash", hash)
	}
	return cp, nil
}

func (s *Store) Reset() error {
	tx, err := s.db.Begin()
	if err != nil {
		return sqlError(err)
	}
	for _, table := range []string{"checkpoint_meta", "endpoint_state", "chain_state", "wait_queue", "request_result", "step_state"} {
		if _, err := tx.Exec(`DELETE FROM ` + table); err != nil {
			_ = tx.Rollback()
			return sqlError(err)
		}
	}
	return tx.Commit()
}

func (s *Store) LoadSteps(requestID string) ([]engine.LevelResult, error) {
	rows, err := s.db.Query(`SELECT level_json FROM step_state WHERE request_id=? ORDER BY chain_id,level_index`, requestID)
	if err != nil {
		return nil, sqlError(err)
	}
	defer rows.Close()
	var out []engine.LevelResult
	for rows.Next() {
		var js string
		if err := rows.Scan(&js); err != nil {
			return nil, sqlError(err)
		}
		var lvl engine.LevelResult
		if err := json.Unmarshal([]byte(js), &lvl); err != nil {
			return nil, bizerr.New(bizerr.ECheckpointTruncated, err.Error())
		}
		out = append(out, lvl)
	}
	return out, nil
}

func (s *Store) SaveStep(requestID, chainID string, level engine.LevelResult) error {
	js, _ := json.Marshal(level)
	passed, reused := 0, 0
	if level.Passed {
		passed = 1
	}
	if level.Reused {
		reused = 1
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO step_state VALUES(?,?,?,?,?,?,?)`,
		requestID, chainID, level.Index, level.Code, passed, reused, string(js))
	return sqlError(err)
}

func FromReport(project model.Project, r *engine.Report, globalSequence int) *Checkpoint {
	cp := &Checkpoint{
		ProjectID: project.ProjectID, CanonicalVersion: model.CanonicalFormat,
		SnapshotHash: r.SnapshotHash, GlobalSequence: globalSequence,
		States: r.EndpointStates, Results: r.Results,
	}
	for _, st := range r.EndpointStates {
		if st != nil && st.CommittedTimeMs > cp.CommittedTimeMs {
			cp.CommittedTimeMs = st.CommittedTimeMs
		}
	}
	if len(r.Moments) > 0 {
		last := r.Moments[len(r.Moments)-1]
		cp.GlobalSequence = last.Sequence
		if last.TimeMs > cp.CommittedTimeMs {
			cp.CommittedTimeMs = last.TimeMs
		}
	}
	return cp
}

func classifyCorrupt(err error, path string) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "malformed") || strings.Contains(msg, "not a database") ||
		strings.Contains(msg, "truncated") || strings.Contains(msg, "disk image") ||
		strings.Contains(msg, "file is encrypted") {
		return bizerr.New(bizerr.ECheckpointTruncated, err.Error()).WithContext("path", path)
	}
	return bizerr.New(bizerr.ESQLite, err.Error()).WithContext("path", path)
}

func sqlError(err error) error {
	if err == nil {
		return nil
	}
	var be *bizerr.Error
	if errors.As(err, &be) {
		return err
	}
	return classifyCorrupt(err, "")
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
