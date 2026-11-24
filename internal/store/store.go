package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"mediaforge/internal/domain"
	"mediaforge/internal/errs"
	"mediaforge/internal/state"
)

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

type VariantRecord struct {
	Ordinal          int         `json:"ordinal"`
	ID               string      `json:"id"`
	State            state.State `json:"state"`
	ErrorCode        string      `json:"errorCode,omitempty"`
	ErrorMessage     string      `json:"errorMessage,omitempty"`
	Path             string      `json:"path,omitempty"`
	BlockRows        int         `json:"blockRows,omitempty"`
	DownsampleFactor int         `json:"downsampleFactor,omitempty"`
	DecisionJSON     string      `json:"decisionJson,omitempty"`
	RecipeJSON       string      `json:"recipeJson,omitempty"`
	RecipeVersion    string      `json:"recipeVersion,omitempty"`
	InputHash        string      `json:"inputHash,omitempty"`
	AlgorithmVersion string      `json:"algorithmVersion,omitempty"`
	OutputSHA256     string      `json:"outputSha256,omitempty"`
	OutputSize       int64       `json:"outputSize,omitempty"`
	MetadataFields   []string    `json:"metadataFields,omitempty"`
	SummaryJSON      string      `json:"summaryJson,omitempty"`
	TruthJSON        string      `json:"truthJson,omitempty"`
	CommitSeq        int64       `json:"commitSeq,omitempty"`
	CommittedAt      string      `json:"committedAt,omitempty"`
	OutputBlob       []byte      `json:"-"`
}

type CommitRecord struct {
	ID               string
	Path             string
	BlockRows        int
	DownsampleFactor int
	DecisionJSON     string
	RecipeJSON       string
	RecipeVersion    string
	InputHash        string
	AlgorithmVersion string
	OutputSHA256     string
	OutputSize       int64
	MetadataFields   []string
	SummaryJSON      string
	TruthJSON        string
	OutputBlob       []byte
}

type EnsureResult struct {
	Invalidated bool   `json:"invalidated"`
	Reset       bool   `json:"reset"`
	Reason      string `json:"reason,omitempty"`
	Total       int    `json:"total"`
	Committed   int    `json:"committed"`
	Pending     int    `json:"pending"`
	Failed      int    `json:"failed"`
}

type Progress struct {
	Total     int `json:"total"`
	Committed int `json:"committed"`
	Failed    int `json:"failed"`
	Active    int `json:"active"`
	Pending   int `json:"pending"`
}

type StallRecord struct {
	Variant string `json:"variant"`
	Reason  string `json:"reason"`
	Bytes   int64  `json:"bytes"`
	At      string `json:"at"`
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, errs.Wrap(errs.EIO, "create checkpoint directory", err)
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_busy_timeout=10000&_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, errs.Wrap(errs.EIO, "open sqlite", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
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
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS variants (
			variant_id TEXT PRIMARY KEY,
			ordinal INTEGER NOT NULL UNIQUE,
			state TEXT NOT NULL,
			error_code TEXT NOT NULL DEFAULT '',
			error_message TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL DEFAULT '',
			block_rows INTEGER NOT NULL DEFAULT 0,
			downsample_factor INTEGER NOT NULL DEFAULT 0,
			decision_json TEXT NOT NULL DEFAULT '',
			recipe_json TEXT NOT NULL DEFAULT '',
			recipe_version TEXT NOT NULL DEFAULT '',
			input_hash TEXT NOT NULL DEFAULT '',
			algorithm_version TEXT NOT NULL DEFAULT '',
			output_sha256 TEXT NOT NULL DEFAULT '',
			output_size INTEGER NOT NULL DEFAULT 0,
			metadata_fields_json TEXT NOT NULL DEFAULT '[]',
			summary_json TEXT NOT NULL DEFAULT '',
			truth_json TEXT NOT NULL DEFAULT '',
			output_blob BLOB,
			commit_seq INTEGER UNIQUE,
			committed_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS invalidations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			reason TEXT NOT NULL,
			at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS stalls (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			variant_id TEXT NOT NULL,
			reason TEXT NOT NULL,
			bytes INTEGER NOT NULL,
			at TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return errs.Wrap(errs.EIO, "sqlite migration", err)
		}
	}
	return nil
}

func (s *Store) Ensure(ctx context.Context, p *domain.Project, rp *domain.ResolvedProject,
	projectFingerprint string, reset bool) (EnsureResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out EnsureResult
	storedFP, _ := s.meta(ctx, "project_fingerprint")
	storedProject, _ := s.meta(ctx, "project_id")
	storedAlg, _ := s.meta(ctx, "algorithm_version")
	invalidReason := ""
	if reset {
		invalidReason = "explicit full reset"
	} else if storedFP != "" && (storedFP != projectFingerprint || storedProject != p.ID ||
		storedAlg != domain.AlgorithmVersion) {
		invalidReason = "project fingerprint, project ID or algorithm version changed"
	}
	if invalidReason != "" {
		if err := s.wipeLocked(ctx, invalidReason); err != nil {
			return out, err
		}
		out.Invalidated = storedFP != ""
		out.Reset = true
		out.Reason = invalidReason
	}
	if err := s.setMetaLocked(ctx, "project_fingerprint", projectFingerprint); err != nil {
		return out, err
	}
	if err := s.setMetaLocked(ctx, "project_id", p.ID); err != nil {
		return out, err
	}
	if err := s.setMetaLocked(ctx, "algorithm_version", domain.AlgorithmVersion); err != nil {
		return out, err
	}
	for i, v := range rp.Variants {
		recipeJSON, _ := json.Marshal(v.Recipe)
		var truth domain.TruthVariant
		for _, tv := range p.Truth.Variants {
			if tv.ID == v.ID {
				truth = tv
				break
			}
		}
		truthJSON, _ := json.Marshal(truth)
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO variants
				(variant_id, ordinal, state, recipe_json, recipe_version, input_hash, truth_json)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(variant_id) DO UPDATE SET
				ordinal=excluded.ordinal,
				recipe_json=CASE WHEN variants.state='committed' THEN variants.recipe_json ELSE excluded.recipe_json END,
				recipe_version=CASE WHEN variants.state='committed' THEN variants.recipe_version ELSE excluded.recipe_version END,
				input_hash=CASE WHEN variants.state='committed' THEN variants.input_hash ELSE excluded.input_hash END,
				truth_json=CASE WHEN variants.state='committed' THEN variants.truth_json ELSE excluded.truth_json END
		`, v.ID, i, string(state.Pending), string(recipeJSON), v.RecipeVersion, v.InputHash, string(truthJSON))
		if err != nil {
			return out, errs.Wrap(errs.EIO, "upsert variant checkpoint", err)
		}
		_ = res
	}
	if err := s.resetInFlightLocked(ctx); err != nil {
		return out, err
	}
	if err := s.validateCommittedLocked(ctx, rp); err != nil {
		if code := errs.CodeOf(err); code == errs.EState {
			if err2 := s.wipeLocked(ctx, err.Error()); err2 != nil {
				return out, err2
			}
			out.Invalidated = true
			out.Reset = true
			out.Reason = err.Error()
			if err2 := s.reinsertVariantsLocked(ctx, p, rp); err2 != nil {
				return out, err2
			}
		} else {
			return out, err
		}
	}
	progress, err := s.Progress(ctx)
	if err != nil {
		return out, err
	}
	out.Total, out.Committed, out.Pending, out.Failed = progress.Total, progress.Committed, progress.Pending, progress.Failed
	return out, nil
}

func (s *Store) reinsertVariantsLocked(ctx context.Context, p *domain.Project, rp *domain.ResolvedProject) error {
	if err := s.wipeLocked(ctx, ""); err != nil {
		return err
	}
	for i, v := range rp.Variants {
		recipeJSON, _ := json.Marshal(v.Recipe)
		var truth domain.TruthVariant
		for _, tv := range p.Truth.Variants {
			if tv.ID == v.ID {
				truth = tv
				break
			}
		}
		truthJSON, _ := json.Marshal(truth)
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO variants
				(variant_id, ordinal, state, recipe_json, recipe_version, input_hash, truth_json)
			VALUES (?, ?, 'pending', ?, ?, ?, ?)`,
			v.ID, i, string(recipeJSON), v.RecipeVersion, v.InputHash, string(truthJSON)); err != nil {
			return errs.Wrap(errs.EIO, "reinsert variant checkpoint", err)
		}
	}
	return nil
}

func (s *Store) validateCommittedLocked(ctx context.Context, rp *domain.ResolvedProject) error {
	want := map[string]domain.ResolvedVariant{}
	for _, v := range rp.Variants {
		want[v.ID] = v
	}
	rows, err := s.db.QueryContext(ctx, `SELECT variant_id, input_hash, recipe_version, algorithm_version
		FROM variants WHERE state='committed'`)
	if err != nil {
		return errs.Wrap(errs.EIO, "inspect committed snapshots", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, inputHash, recipeVersion, alg string
		if err := rows.Scan(&id, &inputHash, &recipeVersion, &alg); err != nil {
			return errs.Wrap(errs.EIO, "scan committed snapshot", err)
		}
		v, ok := want[id]
		if !ok || inputHash != v.InputHash || recipeVersion != v.RecipeVersion || alg != domain.AlgorithmVersion {
			return &errs.Error{
				Code: errs.EState, Variant: id,
				Message: "committed checkpoint does not match current input or versions",
			}
		}
	}
	return rows.Err()
}

func (s *Store) resetInFlightLocked(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE variants SET state='pending', error_code='', error_message=''
		WHERE state IN ('decoding','encoding')`)
	if err != nil {
		return errs.Wrap(errs.EIO, "reset interrupted variants", err)
	}
	return nil
}

func (s *Store) wipeLocked(ctx context.Context, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errs.Wrap(errs.EIO, "begin sqlite wipe", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM variants`); err != nil {
		return errs.Wrap(errs.EIO, "delete committed snapshots", err)
	}
	if reason != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO invalidations(reason, at) VALUES (?, ?)`,
			reason, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return errs.Wrap(errs.EIO, "record invalidation", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return errs.Wrap(errs.EIO, "commit sqlite wipe", err)
	}
	return nil
}

func (s *Store) Reset(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.wipeLocked(ctx, "full rerun"); err != nil {
		return err
	}
	for _, key := range []string{"project_fingerprint", "project_id", "algorithm_version"} {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE key=?`, key); err != nil {
			return errs.Wrap(errs.EIO, "reset metadata", err)
		}
	}
	return nil
}

func (s *Store) meta(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (s *Store) setMetaLocked(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return errs.Wrap(errs.EIO, "write metadata", err)
	}
	return nil
}

func (s *Store) Transition(ctx context.Context, id string, to state.State, code errs.Code, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errs.Wrap(errs.EIO, "begin state transition", err)
	}
	defer tx.Rollback()
	var current state.State
	if err := tx.QueryRowContext(ctx, `SELECT state FROM variants WHERE variant_id=?`, id).Scan(&current); err != nil {
		return errs.Wrap(errs.EInternal, "load variant state", err)
	}
	if err := state.Validate(id, current, to); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE variants SET state=?, error_code=?, error_message=?
		WHERE variant_id=? AND state=?`, string(to), string(code), message, id, string(current)); err != nil {
		return errs.Wrap(errs.EIO, "write state transition", err)
	}
	if err := tx.Commit(); err != nil {
		return errs.Wrap(errs.EIO, "commit state transition", err)
	}
	return nil
}

func (s *Store) Commit(ctx context.Context, rec CommitRecord) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, errs.Wrap(errs.EIO, "begin variant commit", err)
	}
	defer tx.Rollback()
	var current state.State
	if err := tx.QueryRowContext(ctx, `SELECT state FROM variants WHERE variant_id=?`, rec.ID).Scan(&current); err != nil {
		return 0, errs.Wrap(errs.EInternal, "load variant state", err)
	}
	if err := state.Validate(rec.ID, current, state.Committed); err != nil {
		return 0, err
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(commit_seq),0)+1 FROM variants`).Scan(&seq); err != nil {
		return 0, errs.Wrap(errs.EIO, "allocate commit sequence", err)
	}
	fieldsJSON, _ := json.Marshal(rec.MetadataFields)
	res, err := tx.ExecContext(ctx, `UPDATE variants SET
		state='committed', error_code='', error_message='',
		path=?, block_rows=?, downsample_factor=?, decision_json=?,
		recipe_json=?, recipe_version=?, input_hash=?, algorithm_version=?,
		output_sha256=?, output_size=?, metadata_fields_json=?, summary_json=?, truth_json=?,
		output_blob=?, commit_seq=?, committed_at=?
		WHERE variant_id=? AND state='encoding'`,
		rec.Path, rec.BlockRows, rec.DownsampleFactor, rec.DecisionJSON,
		rec.RecipeJSON, rec.RecipeVersion, rec.InputHash, rec.AlgorithmVersion,
		rec.OutputSHA256, rec.OutputSize, string(fieldsJSON), rec.SummaryJSON, rec.TruthJSON,
		rec.OutputBlob, seq, time.Now().UTC().Format(time.RFC3339Nano), rec.ID)
	if err != nil {
		return 0, errs.Wrap(errs.EIO, "write variant commit", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return 0, &errs.Error{Code: errs.EState, Message: "variant commit boundary changed", Variant: rec.ID}
	}
	if err := tx.Commit(); err != nil {
		return 0, errs.Wrap(errs.EIO, "commit variant checkpoint", err)
	}
	return seq, nil
}

func (s *Store) Fail(ctx context.Context, id string, from state.State, code errs.Code, message string) error {
	return s.Transition(ctx, id, state.Failed, code, message)
}

func (s *Store) Retry(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errs.Wrap(errs.EIO, "begin retry", err)
	}
	defer tx.Rollback()
	var current state.State
	if err := tx.QueryRowContext(ctx, `SELECT state FROM variants WHERE variant_id=?`, id).Scan(&current); err != nil {
		return errs.Wrap(errs.EInternal, "load variant state", err)
	}
	if current != state.Failed {
		return state.TransitionError(id, current, state.Pending)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE variants SET state='pending', error_code='', error_message=''
		WHERE variant_id=? AND state='failed'`, id); err != nil {
		return errs.Wrap(errs.EIO, "reset failed variant", err)
	}
	if err := tx.Commit(); err != nil {
		return errs.Wrap(errs.EIO, "commit retry", err)
	}
	return nil
}

func (s *Store) CancelActive(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `UPDATE variants SET state='pending'
		WHERE state IN ('decoding','encoding')`); err != nil {
		return errs.Wrap(errs.EIO, "reset canceled variants", err)
	}
	return nil
}

func (s *Store) SetDecision(ctx context.Context, id, pathName string, blockRows, downsample int, decisionJSON string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE variants SET path=?, block_rows=?, downsample_factor=?, decision_json=?
		WHERE variant_id=? AND state!='committed'`, pathName, blockRows, downsample, decisionJSON, id)
	if err != nil {
		return errs.Wrap(errs.EIO, "record scale decision", err)
	}
	return nil
}

func (s *Store) UpdatePending(ctx context.Context, id string, recipe domain.Recipe, recipeVersion,
	inputHash string, truth domain.TruthVariant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	recipeJSON, _ := json.Marshal(recipe)
	truthJSON, _ := json.Marshal(truth)
	res, err := s.db.ExecContext(ctx, `UPDATE variants SET
		state='pending', recipe_json=?, recipe_version=?, input_hash=?, truth_json=?,
		error_code='', error_message=''
		WHERE variant_id=? AND state!='committed'`,
		string(recipeJSON), recipeVersion, inputHash, string(truthJSON), id)
	if err != nil {
		return errs.Wrap(errs.EIO, "update pending recipe", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &errs.Error{Code: errs.EState, Message: "committed variant is frozen", Variant: id}
	}
	return nil
}

func (s *Store) Variants(ctx context.Context) ([]VariantRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT variant_id, ordinal, state, error_code, error_message,
		path, block_rows, downsample_factor, decision_json, recipe_json, recipe_version, input_hash,
		algorithm_version, output_sha256, output_size, metadata_fields_json, summary_json, truth_json,
		COALESCE(commit_seq,0), committed_at, COALESCE(output_blob, x'')
		FROM variants ORDER BY ordinal`)
	if err != nil {
		return nil, errs.Wrap(errs.EIO, "query variants", err)
	}
	defer rows.Close()
	var out []VariantRecord
	for rows.Next() {
		var r VariantRecord
		var st, fieldsJSON string
		if err := rows.Scan(&r.ID, &r.Ordinal, &st, &r.ErrorCode, &r.ErrorMessage,
			&r.Path, &r.BlockRows, &r.DownsampleFactor, &r.DecisionJSON, &r.RecipeJSON,
			&r.RecipeVersion, &r.InputHash, &r.AlgorithmVersion, &r.OutputSHA256, &r.OutputSize,
			&fieldsJSON, &r.SummaryJSON, &r.TruthJSON, &r.CommitSeq, &r.CommittedAt, &r.OutputBlob); err != nil {
			return nil, errs.Wrap(errs.EIO, "scan variant", err)
		}
		r.State = state.State(st)
		_ = json.Unmarshal([]byte(fieldsJSON), &r.MetadataFields)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Get(ctx context.Context, id string) (*VariantRecord, error) {
	vs, err := s.Variants(ctx)
	if err != nil {
		return nil, err
	}
	for i := range vs {
		if vs[i].ID == id {
			return &vs[i], nil
		}
	}
	return nil, &errs.Error{Code: errs.EDecode, Message: "variant not found", Variant: id}
}

func (s *Store) Progress(ctx context.Context) (Progress, error) {
	var p Progress
	err := s.db.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN state='committed' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN state='failed' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN state IN ('decoding','encoding') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN state='pending' THEN 1 ELSE 0 END),0)
		FROM variants`).Scan(&p.Total, &p.Committed, &p.Failed, &p.Active, &p.Pending)
	if err != nil {
		return p, errs.Wrap(errs.EIO, "query progress", err)
	}
	return p, nil
}

func (s *Store) RecordStall(ctx context.Context, rec StallRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.At == "" {
		rec.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO stalls(variant_id, reason, bytes, at) VALUES (?,?,?,?)`,
		rec.Variant, rec.Reason, rec.Bytes, rec.At)
	if err != nil {
		return errs.Wrap(errs.EIO, "record resource stall", err)
	}
	return nil
}

func (s *Store) Stalls(ctx context.Context, limit int) ([]StallRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT variant_id, reason, bytes, at FROM stalls ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, errs.Wrap(errs.EIO, "query stalls", err)
	}
	defer rows.Close()
	var out []StallRecord
	for rows.Next() {
		var r StallRecord
		if err := rows.Scan(&r.Variant, &r.Reason, &r.Bytes, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Invalidations(ctx context.Context) ([]map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT reason, at FROM invalidations ORDER BY id DESC LIMIT 50`)
	if err != nil {
		return nil, errs.Wrap(errs.EIO, "query invalidations", err)
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var reason, at string
		if err := rows.Scan(&reason, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]string{"reason": reason, "at": at})
	}
	return out, rows.Err()
}

func (s *Store) CountCommitted(ctx context.Context) (int, error) {
	p, err := s.Progress(ctx)
	return p.Committed, err
}

func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta(ctx, key)
}

func (s *Store) String() string { return fmt.Sprintf("Store(%s)", s.path) }
