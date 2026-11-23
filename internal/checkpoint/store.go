package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"maskpipeline/internal/apperr"
	"maskpipeline/internal/model"
)

type OpenMode string

const (
	ModeIncremental OpenMode = "incremental"
	ModeResume      OpenMode = "resume"
	ModeFull        OpenMode = "full"
)

type Meta struct {
	SessionID        string `json:"session_id"`
	SampleID         string `json:"sample_id"`
	ImageSHA256      string `json:"image_sha256"`
	TruthFingerprint string `json:"truth_fingerprint"`
	ParamsVersion    uint64 `json:"params_version"`
	ParamsHash       string `json:"params_hash"`
	ParamsJSON       string `json:"params_json"`
	AlgorithmVersion string `json:"algorithm_version"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
	TileSize         int    `json:"tile_size"`
	TilesX           int    `json:"tiles_x"`
	TilesY           int    `json:"tiles_y"`
}

func (m Meta) Hash() string {
	return model.HashJSON(m)
}

func (m Meta) Validate() error {
	if m.SessionID == "" {
		return apperr.New(apperr.CodeMeta, "checkpoint_meta", "session id is required").WithField("session_id")
	}
	if m.SampleID == "" || m.ImageSHA256 == "" || m.TruthFingerprint == "" || m.ParamsHash == "" || m.ParamsJSON == "" || m.AlgorithmVersion == "" {
		return apperr.New(apperr.CodeMeta, "checkpoint_meta", "session metadata is incomplete")
	}
	var params model.Params
	if err := json.Unmarshal([]byte(m.ParamsJSON), &params); err != nil {
		return apperr.New(apperr.CodeMeta, "checkpoint_meta", "parameter snapshot is not valid JSON").
			WithField("params_json")
	}
	if model.HashJSON(params) != m.ParamsHash {
		return apperr.New(apperr.CodeMeta, "checkpoint_meta", "parameter snapshot hash mismatch").
			WithField("params_hash")
	}
	if m.Width <= 0 || m.Height <= 0 || m.TileSize <= 0 {
		return apperr.New(apperr.CodeMeta, "checkpoint_meta", "invalid image dimensions or tile size")
	}
	if m.TilesX <= 0 || m.TilesY <= 0 {
		return apperr.New(apperr.CodeMeta, "checkpoint_meta", "invalid tile grid")
	}
	return nil
}

type EnsureResult struct {
	SessionID   string `json:"session_id"`
	Created     bool   `json:"created"`
	Reset       bool   `json:"reset"`
	ResetReason string `json:"reset_reason,omitempty"`
	Interrupted int    `json:"interrupted_stages"`
}

type StageView struct {
	SessionID        string      `json:"session_id"`
	TileIndex        int         `json:"tile_index"`
	Stage            model.Stage `json:"stage"`
	State            model.State `json:"state"`
	Output           []byte      `json:"-"`
	ContentHash      string      `json:"content_hash"`
	ParamVersion     uint64      `json:"param_version"`
	AlgorithmVersion string      `json:"algorithm_version"`
	ErrorCode        string      `json:"error_code,omitempty"`
	ErrorMessage     string      `json:"error_message,omitempty"`
	UpdatedAt        string      `json:"updated_at"`
}

type CommitRecord struct {
	Seq              int         `json:"seq"`
	TileIndex        int         `json:"tile_index"`
	Stage            model.Stage `json:"stage"`
	ContentHash      string      `json:"content_hash"`
	ParamVersion     uint64      `json:"param_version"`
	AlgorithmVersion string      `json:"algorithm_version"`
	CommittedAt      string      `json:"committed_at"`
}

type CommitInput struct {
	SessionID        string
	TileIndex        int
	Stage            model.Stage
	Output           []byte
	ContentHash      string
	ParamVersion     uint64
	AlgorithmVersion string
}

type Store struct {
	mu sync.Mutex
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, apperr.New(apperr.CodeIO, "open_checkpoint", "database path is required").WithField("path")
	}
	if path != ":memory:" {
		dir := filepath.Dir(path)
		if dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, apperr.Wrap(apperr.CodeIO, "open_checkpoint", "create database directory", err)
			}
		}
	}
	dsn := path
	if path != ":memory:" {
		dsn = "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeIO, "open_checkpoint", "open sqlite database", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if path == ":memory:" {
		if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
			db.Close()
			return nil, apperr.Wrap(apperr.CodeIO, "open_checkpoint", "configure sqlite", err)
		}
	}
	s := &Store{db: db}
	if err := s.createSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) createSchema() error {
	const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	session_id TEXT PRIMARY KEY,
	sample_id TEXT NOT NULL,
	image_sha256 TEXT NOT NULL,
	truth_fingerprint TEXT NOT NULL,
	param_version INTEGER NOT NULL,
	params_hash TEXT NOT NULL,
	algorithm_version TEXT NOT NULL,
	params_json TEXT NOT NULL DEFAULT '',
	width INTEGER NOT NULL,
	height INTEGER NOT NULL,
	tile_size INTEGER NOT NULL,
	tiles_x INTEGER NOT NULL,
	tiles_y INTEGER NOT NULL,
	meta_hash TEXT NOT NULL,
	status TEXT NOT NULL,
	final_mask_hash TEXT NOT NULL DEFAULT '',
	final_stats_hash TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tile_stages (
	session_id TEXT NOT NULL,
	tile_index INTEGER NOT NULL,
	stage_name TEXT NOT NULL,
	state TEXT NOT NULL,
	output BLOB,
	content_hash TEXT NOT NULL DEFAULT '',
	param_version INTEGER NOT NULL,
	algorithm_version TEXT NOT NULL,
	error_code TEXT NOT NULL DEFAULT '',
	error_message TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL,
	PRIMARY KEY (session_id, tile_index, stage_name),
	FOREIGN KEY (session_id) REFERENCES sessions(session_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS commits (
	session_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	tile_index INTEGER NOT NULL,
	stage_name TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	param_version INTEGER NOT NULL,
	algorithm_version TEXT NOT NULL,
	committed_at TEXT NOT NULL,
	PRIMARY KEY (session_id, seq),
	UNIQUE (session_id, tile_index, stage_name),
	FOREIGN KEY (session_id) REFERENCES sessions(session_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_stages_tile ON tile_stages(session_id, tile_index, stage_name);
`
	if _, err := s.db.Exec(schema); err != nil {
		return apperr.Wrap(apperr.CodeIO, "create_schema", "initialize sqlite schema", err)
	}
	if err := s.ensureColumn("sessions", "params_json", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureColumn(table, column, declaration string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return apperr.Wrap(apperr.CodeIO, "migrate_schema", "inspect table", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return apperr.Wrap(apperr.CodeIO, "migrate_schema", "inspect column", err)
		}
		if name == column {
			found = true
		}
	}
	if found {
		return nil
	}
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + declaration)
	if err != nil {
		return apperr.Wrap(apperr.CodeIO, "migrate_schema", "add parameter snapshot column", err)
	}
	return nil
}

func (s *Store) EnsureSession(ctx context.Context, meta Meta, mode OpenMode) (EnsureResult, error) {
	if err := meta.Validate(); err != nil {
		return EnsureResult{}, err
	}
	if mode != ModeIncremental && mode != ModeResume && mode != ModeFull {
		return EnsureResult{}, apperr.New(apperr.CodePara, "ensure_session", "unsupported checkpoint mode").
			WithField("mode").WithDetail("value", mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EnsureResult{}, wrapSQL("ensure_session", err)
	}
	defer tx.Rollback()

	result := EnsureResult{SessionID: meta.SessionID}
	existing, found, err := readMetaTx(ctx, tx, meta.SessionID)
	if err != nil {
		return EnsureResult{}, err
	}
	resetReason := ""
	if mode == ModeFull {
		if found {
			resetReason = "full recompute requested"
		}
	} else if found {
		reason, err := validateSessionTx(ctx, tx, existing)
		if err != nil {
			return EnsureResult{}, err
		}
		if reason != "" {
			resetReason = reason
		} else if existing.Hash() != meta.Hash() || !sameMeta(existing, meta) {
			resetReason = "session metadata or parameter version mismatch"
		}
	}
	if resetReason != "" && found {
		if err := deleteSessionTx(ctx, tx, meta.SessionID); err != nil {
			return EnsureResult{}, err
		}
		found = false
		result.Reset = true
		result.ResetReason = resetReason
	}
	if !found {
		if err := insertSessionTx(ctx, tx, meta); err != nil {
			return EnsureResult{}, err
		}
		if err := insertPendingStagesTx(ctx, tx, meta); err != nil {
			return EnsureResult{}, err
		}
		result.Created = true
	} else {
		n, err := resetInterruptedTx(ctx, tx, meta.SessionID)
		if err != nil {
			return EnsureResult{}, err
		}
		result.Interrupted = n
	}
	if err := tx.Commit(); err != nil {
		return EnsureResult{}, wrapSQL("ensure_session", err)
	}
	return result, nil
}

func sameMeta(a, b Meta) bool {
	return a.SessionID == b.SessionID &&
		a.SampleID == b.SampleID &&
		a.ImageSHA256 == b.ImageSHA256 &&
		a.TruthFingerprint == b.TruthFingerprint &&
		a.ParamsVersion == b.ParamsVersion &&
		a.ParamsHash == b.ParamsHash &&
		a.ParamsJSON == b.ParamsJSON &&
		a.AlgorithmVersion == b.AlgorithmVersion &&
		a.Width == b.Width &&
		a.Height == b.Height &&
		a.TileSize == b.TileSize &&
		a.TilesX == b.TilesX &&
		a.TilesY == b.TilesY
}

func validateSessionTx(ctx context.Context, tx *sql.Tx, meta Meta) (string, error) {
	expectedStages := meta.TilesX * meta.TilesY * len(model.Stages)
	type key struct {
		tile  int
		stage model.Stage
	}
	rows, err := tx.QueryContext(ctx, `SELECT tile_index, stage_name, state, output, content_hash, param_version, algorithm_version FROM tile_stages WHERE session_id=?`, meta.SessionID)
	if err != nil {
		return "", wrapSQL("validate_session", err)
	}
	defer rows.Close()
	seen := make(map[key]model.State, expectedStages)
	for rows.Next() {
		var tile int
		var stageName, state, contentHash, algo string
		var output []byte
		var version uint64
		if err := rows.Scan(&tile, &stageName, &state, &output, &contentHash, &version, &algo); err != nil {
			return "", wrapSQL("validate_session", err)
		}
		stage := model.Stage(stageName)
		st := model.State(state)
		if !stage.Valid() || !st.Valid() || tile < 0 || tile >= meta.TilesX*meta.TilesY {
			return "invalid persisted stage row", nil
		}
		if version != meta.ParamsVersion || algo != meta.AlgorithmVersion {
			return fmt.Sprintf("stage v%d/%s does not match session v%d/%s", version, algo, meta.ParamsVersion, meta.AlgorithmVersion), nil
		}
		if st == model.StateCommitted {
			if contentHash == "" || model.HashBytes(output) != contentHash {
				return fmt.Sprintf("committed stage %d/%s content hash mismatch", tile, stage), nil
			}
		}
		seen[key{tile: tile, stage: stage}] = st
	}
	if err := rows.Err(); err != nil {
		return "", wrapSQL("validate_session", err)
	}
	if len(seen) != expectedStages {
		return "persisted stage rows are incomplete", nil
	}
	for _, stage := range model.Stages {
		for tile := 0; tile < meta.TilesX*meta.TilesY; tile++ {
			if _, ok := seen[key{tile: tile, stage: stage}]; !ok {
				return "persisted stage rows are incomplete", nil
			}
		}
	}
	commitRows, err := tx.QueryContext(ctx, `SELECT seq, tile_index, stage_name, content_hash, param_version, algorithm_version FROM commits WHERE session_id=? ORDER BY seq`, meta.SessionID)
	if err != nil {
		return "", wrapSQL("validate_session", err)
	}
	defer commitRows.Close()
	wantSeq := 0
	committed := make(map[key]bool)
	for commitRows.Next() {
		var seq, tile int
		var stageName, contentHash, algo string
		var version uint64
		if err := commitRows.Scan(&seq, &tile, &stageName, &contentHash, &version, &algo); err != nil {
			return "", wrapSQL("validate_session", err)
		}
		stage := model.Stage(stageName)
		if seq != canonicalSeq(tile, stage) {
			return "commit sequence number differs from its canonical tile/stage slot", nil
		}
		wantSeq++
		if version != meta.ParamsVersion || algo != meta.AlgorithmVersion {
			return "commit parameter or algorithm version mismatch", nil
		}
		if seen[key{tile: tile, stage: stage}] != model.StateCommitted {
			return "commit references a non-committed stage", nil
		}
		if contentHash == "" {
			return "commit content hash is empty", nil
		}
		committed[key{tile: tile, stage: stage}] = true
	}
	if wantSeq > expectedStages {
		return "checkpoint has too many commit records", nil
	}
	if err := commitRows.Err(); err != nil {
		return "", wrapSQL("validate_session", err)
	}
	for k, state := range seen {
		if state == model.StateCommitted && !committed[k] {
			return "committed stage is missing its commit record", nil
		}
	}
	return "", nil
}

func insertSessionTx(ctx context.Context, tx *sql.Tx, meta Meta) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO sessions
		(session_id, sample_id, image_sha256, truth_fingerprint, param_version, params_hash, params_json,
		 algorithm_version, width, height, tile_size, tiles_x, tiles_y, meta_hash, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'running', ?)`,
		meta.SessionID, meta.SampleID, meta.ImageSHA256, meta.TruthFingerprint, meta.ParamsVersion,
		meta.ParamsHash, meta.ParamsJSON, meta.AlgorithmVersion, meta.Width, meta.Height, meta.TileSize,
		meta.TilesX, meta.TilesY, meta.Hash(), now())
	if err != nil {
		return wrapSQL("insert_session", err)
	}
	return nil
}

func insertPendingStagesTx(ctx context.Context, tx *sql.Tx, meta Meta) error {
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO tile_stages
		(session_id, tile_index, stage_name, state, param_version, algorithm_version, updated_at)
		VALUES (?, ?, ?, 'pending', ?, ?, ?)`)
	if err != nil {
		return wrapSQL("insert_pending_stages", err)
	}
	defer stmt.Close()
	for tile := 0; tile < meta.TilesX*meta.TilesY; tile++ {
		for _, stage := range model.Stages {
			if _, err := stmt.ExecContext(ctx, meta.SessionID, tile, string(stage), meta.ParamsVersion, meta.AlgorithmVersion, now()); err != nil {
				return wrapSQL("insert_pending_stages", err)
			}
		}
	}
	return nil
}

func deleteSessionTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM commits WHERE session_id=?`, sessionID); err != nil {
		return wrapSQL("delete_session", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tile_stages WHERE session_id=?`, sessionID); err != nil {
		return wrapSQL("delete_session", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE session_id=?`, sessionID); err != nil {
		return wrapSQL("delete_session", err)
	}
	return nil
}

func resetInterruptedTx(ctx context.Context, tx *sql.Tx, sessionID string) (int, error) {
	res, err := tx.ExecContext(ctx, `UPDATE tile_stages SET state='failed', error_code='E_INTERRUPTED',
		error_message='process restarted while stage was in progress', updated_at=? WHERE session_id=? AND state='in_progress'`,
		now(), sessionID)
	if err != nil {
		return 0, wrapSQL("reset_interrupted", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func readMetaTx(ctx context.Context, tx *sql.Tx, sessionID string) (Meta, bool, error) {
	var m Meta
	err := tx.QueryRowContext(ctx, `SELECT session_id, sample_id, image_sha256, truth_fingerprint,
		param_version, params_hash, params_json, algorithm_version, width, height, tile_size, tiles_x, tiles_y
		FROM sessions WHERE session_id=?`, sessionID).Scan(
		&m.SessionID, &m.SampleID, &m.ImageSHA256, &m.TruthFingerprint, &m.ParamsVersion,
		&m.ParamsHash, &m.ParamsJSON, &m.AlgorithmVersion, &m.Width, &m.Height, &m.TileSize, &m.TilesX, &m.TilesY,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Meta{}, false, nil
	}
	if err != nil {
		return Meta{}, false, wrapSQL("read_session", err)
	}
	return m, true, nil
}

func (s *Store) SessionMeta(ctx context.Context, sessionID string) (Meta, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Meta{}, false, wrapSQL("read_session", err)
	}
	defer tx.Rollback()
	m, ok, err := readMetaTx(ctx, tx, sessionID)
	return m, ok, err
}

func (s *Store) Transition(ctx context.Context, sessionID string, tile int, stage model.Stage, to model.State, paramVersion uint64, algorithmVersion string) error {
	if !stage.Valid() {
		return apperr.New(apperr.CodeState, "transition", "unknown stage").WithDetail("stage", stage)
	}
	if !to.Valid() || to == model.StateCommitted {
		return apperr.New(apperr.CodeState, "transition", "target state is not valid for a bare transition").WithDetail("to", to)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrapSQL("transition", err)
	}
	defer tx.Rollback()
	meta, found, err := readMetaTx(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	if !found {
		return apperr.New(apperr.CodeState, "transition", "checkpoint session does not exist").WithDetail("session_id", sessionID)
	}
	if paramVersion != meta.ParamsVersion || algorithmVersion != meta.AlgorithmVersion {
		return apperr.New(apperr.CodeVersion, "transition", "operation version differs from the frozen session").
			WithDetail("left_version", meta.ParamsVersion).
			WithDetail("right_version", paramVersion).
			WithDetail("left_algorithm", meta.AlgorithmVersion).
			WithDetail("right_algorithm", algorithmVersion)
	}
	if tile < 0 || tile >= meta.TilesX*meta.TilesY {
		return apperr.New(apperr.CodeState, "transition", "tile index is outside the session grid").
			WithDetail("tile", tile)
	}
	var currentName string
	var rowVersion uint64
	var rowAlgo string
	err = tx.QueryRowContext(ctx, `SELECT state, param_version, algorithm_version FROM tile_stages
		WHERE session_id=? AND tile_index=? AND stage_name=?`, sessionID, tile, string(stage)).Scan(&currentName, &rowVersion, &rowAlgo)
	if errors.Is(err, sql.ErrNoRows) {
		return apperr.New(apperr.CodeState, "transition", "stage row does not exist").
			WithDetail("tile", tile).WithDetail("stage", stage)
	}
	if err != nil {
		return wrapSQL("transition", err)
	}
	if rowVersion != meta.ParamsVersion || rowAlgo != meta.AlgorithmVersion {
		return apperr.New(apperr.CodeVersion, "transition", "stage version differs from the frozen session").
			WithDetail("left_version", meta.ParamsVersion).
			WithDetail("right_version", rowVersion)
	}
	current := model.State(currentName)
	if !model.LegalTransition(current, to) {
		return apperr.New(apperr.CodeState, "transition", "illegal stage state transition").
			WithDetail("tile", tile).
			WithDetail("stage", stage).
			WithDetail("from", current).
			WithDetail("to", to)
	}
	_, err = tx.ExecContext(ctx, `UPDATE tile_stages SET state=?, error_code='', error_message='', updated_at=?
		WHERE session_id=? AND tile_index=? AND stage_name=?`, string(to), now(), sessionID, tile, string(stage))
	if err != nil {
		return wrapSQL("transition", err)
	}
	if err := tx.Commit(); err != nil {
		return wrapSQL("transition", err)
	}
	return nil
}

func (s *Store) Commit(ctx context.Context, in CommitInput) (CommitRecord, error) {
	if !in.Stage.Valid() {
		return CommitRecord{}, apperr.New(apperr.CodeState, "commit_stage", "unknown stage").WithDetail("stage", in.Stage)
	}
	if in.ContentHash == "" {
		in.ContentHash = model.HashBytes(in.Output)
	} else if in.ContentHash != model.HashBytes(in.Output) {
		return CommitRecord{}, apperr.New(apperr.CodeMeta, "commit_stage", "declared content hash does not match output").
			WithField("content_hash")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CommitRecord{}, wrapSQL("commit_stage", err)
	}
	defer tx.Rollback()
	meta, found, err := readMetaTx(ctx, tx, in.SessionID)
	if err != nil {
		return CommitRecord{}, err
	}
	if !found {
		return CommitRecord{}, apperr.New(apperr.CodeState, "commit_stage", "checkpoint session does not exist").
			WithDetail("session_id", in.SessionID)
	}
	if in.ParamVersion != meta.ParamsVersion || in.AlgorithmVersion != meta.AlgorithmVersion {
		return CommitRecord{}, apperr.New(apperr.CodeVersion, "commit_stage", "commit version differs from the frozen session").
			WithDetail("left_version", meta.ParamsVersion).
			WithDetail("right_version", in.ParamVersion).
			WithDetail("left_algorithm", meta.AlgorithmVersion).
			WithDetail("right_algorithm", in.AlgorithmVersion)
	}
	if in.TileIndex < 0 || in.TileIndex >= meta.TilesX*meta.TilesY {
		return CommitRecord{}, apperr.New(apperr.CodeState, "commit_stage", "tile index is outside the session grid").
			WithDetail("tile", in.TileIndex)
	}
	var stateName string
	var rowVersion uint64
	var rowAlgo string
	var oldHash string
	err = tx.QueryRowContext(ctx, `SELECT state, param_version, algorithm_version, content_hash FROM tile_stages
		WHERE session_id=? AND tile_index=? AND stage_name=?`, in.SessionID, in.TileIndex, string(in.Stage)).
		Scan(&stateName, &rowVersion, &rowAlgo, &oldHash)
	if errors.Is(err, sql.ErrNoRows) {
		return CommitRecord{}, apperr.New(apperr.CodeState, "commit_stage", "stage row does not exist").
			WithDetail("tile", in.TileIndex).WithDetail("stage", in.Stage)
	}
	if err != nil {
		return CommitRecord{}, wrapSQL("commit_stage", err)
	}
	if rowVersion != meta.ParamsVersion || rowAlgo != meta.AlgorithmVersion {
		return CommitRecord{}, apperr.New(apperr.CodeVersion, "commit_stage", "stage row version differs from session").
			WithDetail("left_version", meta.ParamsVersion).
			WithDetail("right_version", rowVersion)
	}
	state := model.State(stateName)
	if state == model.StateCommitted {
		if oldHash != in.ContentHash {
			return CommitRecord{}, apperr.New(apperr.CodeState, "commit_stage", "committed stage is frozen with a different output").
				WithDetail("tile", in.TileIndex).WithDetail("stage", in.Stage).
				WithDetail("from", state).WithDetail("to", model.StateCommitted)
		}
		rec, err := readCommitTx(ctx, tx, in.SessionID, in.TileIndex, in.Stage)
		if err != nil {
			return CommitRecord{}, err
		}
		return rec, nil
	}
	if state != model.StateInProgress {
		return CommitRecord{}, apperr.New(apperr.CodeState, "commit_stage", "stage must be in progress before commit").
			WithDetail("tile", in.TileIndex).
			WithDetail("stage", in.Stage).
			WithDetail("from", state).
			WithDetail("to", model.StateCommitted)
	}
	seq := canonicalSeq(in.TileIndex, in.Stage)
	committedAt := now()
	_, err = tx.ExecContext(ctx, `UPDATE tile_stages SET state='committed', output=?, content_hash=?,
		error_code='', error_message='', updated_at=? WHERE session_id=? AND tile_index=? AND stage_name=?`,
		in.Output, in.ContentHash, committedAt, in.SessionID, in.TileIndex, string(in.Stage))
	if err != nil {
		return CommitRecord{}, wrapSQL("commit_stage", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO commits
		(session_id, seq, tile_index, stage_name, content_hash, param_version, algorithm_version, committed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, in.SessionID, seq, in.TileIndex, string(in.Stage),
		in.ContentHash, in.ParamVersion, in.AlgorithmVersion, committedAt)
	if err != nil {
		return CommitRecord{}, wrapSQL("commit_stage", err)
	}
	if err := tx.Commit(); err != nil {
		return CommitRecord{}, wrapSQL("commit_stage", err)
	}
	return CommitRecord{
		Seq: seq, TileIndex: in.TileIndex, Stage: in.Stage, ContentHash: in.ContentHash,
		ParamVersion: in.ParamVersion, AlgorithmVersion: in.AlgorithmVersion, CommittedAt: committedAt,
	}, nil
}

func readCommitTx(ctx context.Context, tx *sql.Tx, sessionID string, tile int, stage model.Stage) (CommitRecord, error) {
	var rec CommitRecord
	var stageName string
	err := tx.QueryRowContext(ctx, `SELECT seq, tile_index, stage_name, content_hash, param_version, algorithm_version, committed_at
		FROM commits WHERE session_id=? AND tile_index=? AND stage_name=?`, sessionID, tile, string(stage)).
		Scan(&rec.Seq, &rec.TileIndex, &stageName, &rec.ContentHash, &rec.ParamVersion, &rec.AlgorithmVersion, &rec.CommittedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CommitRecord{}, apperr.New(apperr.CodeState, "read_commit", "committed stage has no commit record")
	}
	if err != nil {
		return CommitRecord{}, wrapSQL("read_commit", err)
	}
	rec.Stage = model.Stage(stageName)
	return rec, nil
}

func (s *Store) FailStage(ctx context.Context, sessionID string, tile int, stage model.Stage, paramVersion uint64, algorithmVersion, code, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrapSQL("fail_stage", err)
	}
	defer tx.Rollback()
	meta, found, err := readMetaTx(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	if !found {
		return apperr.New(apperr.CodeState, "fail_stage", "checkpoint session does not exist")
	}
	if paramVersion != meta.ParamsVersion || algorithmVersion != meta.AlgorithmVersion {
		return apperr.New(apperr.CodeVersion, "fail_stage", "failure report version differs from session").
			WithDetail("left_version", meta.ParamsVersion).
			WithDetail("right_version", paramVersion)
	}
	if tile < 0 || tile >= meta.TilesX*meta.TilesY {
		return apperr.New(apperr.CodeState, "fail_stage", "tile index is outside the session grid").
			WithDetail("tile", tile)
	}
	var currentName string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tile_stages WHERE session_id=? AND tile_index=? AND stage_name=?`,
		sessionID, tile, string(stage)).Scan(&currentName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeState, "fail_stage", "stage row does not exist")
		}
		return wrapSQL("fail_stage", err)
	}
	current := model.State(currentName)
	if !model.LegalTransition(current, model.StateFailed) {
		return apperr.New(apperr.CodeState, "fail_stage", "illegal stage failure transition").
			WithDetail("tile", tile).WithDetail("stage", stage).
			WithDetail("from", current).WithDetail("to", model.StateFailed)
	}
	if code == "" {
		code = apperr.CodeBlock
	}
	_, err = tx.ExecContext(ctx, `UPDATE tile_stages SET state='failed', error_code=?, error_message=?, updated_at=?
		WHERE session_id=? AND tile_index=? AND stage_name=?`, code, message, now(), sessionID, tile, string(stage))
	if err != nil {
		return wrapSQL("fail_stage", err)
	}
	return tx.Commit()
}

func (s *Store) GetStage(ctx context.Context, sessionID string, tile int, stage model.Stage) (StageView, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var v StageView
	var stageName, state string
	err := s.db.QueryRowContext(ctx, `SELECT session_id, tile_index, stage_name, state, output, content_hash,
		param_version, algorithm_version, error_code, error_message, updated_at
		FROM tile_stages WHERE session_id=? AND tile_index=? AND stage_name=?`, sessionID, tile, string(stage)).
		Scan(&v.SessionID, &v.TileIndex, &stageName, &state, &v.Output, &v.ContentHash,
			&v.ParamVersion, &v.AlgorithmVersion, &v.ErrorCode, &v.ErrorMessage, &v.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return StageView{}, false, nil
	}
	if err != nil {
		return StageView{}, false, wrapSQL("get_stage", err)
	}
	v.Stage = model.Stage(stageName)
	v.State = model.State(state)
	return v, true, nil
}

func (s *Store) AllStages(ctx context.Context, sessionID string) ([]StageView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, tile_index, stage_name, state, output, content_hash,
		param_version, algorithm_version, error_code, error_message, updated_at
		FROM tile_stages WHERE session_id=?`, sessionID)
	if err != nil {
		return nil, wrapSQL("list_stages", err)
	}
	defer rows.Close()
	out := make([]StageView, 0)
	for rows.Next() {
		var v StageView
		var stageName, state string
		if err := rows.Scan(&v.SessionID, &v.TileIndex, &stageName, &state, &v.Output, &v.ContentHash,
			&v.ParamVersion, &v.AlgorithmVersion, &v.ErrorCode, &v.ErrorMessage, &v.UpdatedAt); err != nil {
			return nil, wrapSQL("list_stages", err)
		}
		v.Stage = model.Stage(stageName)
		v.State = model.State(state)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapSQL("list_stages", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TileIndex != out[j].TileIndex {
			return out[i].TileIndex < out[j].TileIndex
		}
		return stageOrder(out[i].Stage) < stageOrder(out[j].Stage)
	})
	return out, nil
}

func stageOrder(stage model.Stage) int {
	for i, s := range model.Stages {
		if s == stage {
			return i
		}
	}
	return 99
}

func (s *Store) Commits(ctx context.Context, sessionID string) ([]CommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT seq, tile_index, stage_name, content_hash, param_version, algorithm_version, committed_at
		FROM commits WHERE session_id=? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, wrapSQL("list_commits", err)
	}
	defer rows.Close()
	out := make([]CommitRecord, 0)
	for rows.Next() {
		var rec CommitRecord
		var stageName string
		if err := rows.Scan(&rec.Seq, &rec.TileIndex, &stageName, &rec.ContentHash, &rec.ParamVersion, &rec.AlgorithmVersion, &rec.CommittedAt); err != nil {
			return nil, wrapSQL("list_commits", err)
		}
		rec.Stage = model.Stage(stageName)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapSQL("list_commits", err)
	}
	return out, nil
}

func (s *Store) Finalize(ctx context.Context, sessionID string, maskHash, statsHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrapSQL("finalize_session", err)
	}
	defer tx.Rollback()
	meta, found, err := readMetaTx(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	if !found {
		return apperr.New(apperr.CodeState, "finalize_session", "checkpoint session does not exist")
	}
	var committed int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tile_stages WHERE session_id=? AND state='committed'`, sessionID).Scan(&committed); err != nil {
		return wrapSQL("finalize_session", err)
	}
	if committed != meta.TilesX*meta.TilesY*len(model.Stages) {
		return apperr.New(apperr.CodeState, "finalize_session", "cannot finalize with uncommitted stages").
			WithDetail("committed", committed).
			WithDetail("expected", meta.TilesX*meta.TilesY*len(model.Stages))
	}
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET status='completed', final_mask_hash=?, final_stats_hash=?
		WHERE session_id=?`, maskHash, statsHash, sessionID)
	if err != nil {
		return wrapSQL("finalize_session", err)
	}
	return tx.Commit()
}

type SessionSummary struct {
	Meta           Meta
	Status         string
	FinalMaskHash  string
	FinalStatsHash string
}

func (s *Store) Summary(ctx context.Context, sessionID string) (SessionSummary, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out SessionSummary
	err := s.db.QueryRowContext(ctx, `SELECT session_id, sample_id, image_sha256, truth_fingerprint,
		param_version, params_hash, params_json, algorithm_version, width, height, tile_size, tiles_x, tiles_y,
		status, final_mask_hash, final_stats_hash FROM sessions WHERE session_id=?`, sessionID).Scan(
		&out.Meta.SessionID, &out.Meta.SampleID, &out.Meta.ImageSHA256, &out.Meta.TruthFingerprint,
		&out.Meta.ParamsVersion, &out.Meta.ParamsHash, &out.Meta.ParamsJSON, &out.Meta.AlgorithmVersion,
		&out.Meta.Width, &out.Meta.Height, &out.Meta.TileSize, &out.Meta.TilesX, &out.Meta.TilesY,
		&out.Status, &out.FinalMaskHash, &out.FinalStatsHash,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionSummary{}, false, nil
	}
	if err != nil {
		return SessionSummary{}, false, wrapSQL("session_summary", err)
	}
	return out, true, nil
}

func (s *Store) ResetSession(ctx context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrapSQL("reset_session", err)
	}
	defer tx.Rollback()
	if err := deleteSessionTx(ctx, tx, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func wrapSQL(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	e, ok := apperr.As(err)
	if ok {
		return e
	}
	return apperr.Wrap(apperr.CodeIO, op, "sqlite operation failed", err)
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func StageSequence(tiles int) []string {
	out := make([]string, 0, tiles*len(model.Stages))
	for tile := 0; tile < tiles; tile++ {
		for _, stage := range model.Stages {
			out = append(out, fmt.Sprintf("%d/%s", tile, stage))
		}
	}
	return out
}

func canonicalSeq(tile int, stage model.Stage) int {
	return tile*len(model.Stages) + stageIndex(stage)
}

func stageIndex(stage model.Stage) int {
	for i, s := range model.Stages {
		if s == stage {
			return i
		}
	}
	return -1
}

func ParseStageSequence(items []CommitRecord) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprintf("%d/%s", item.TileIndex, item.Stage))
	}
	return out
}

func StringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func SummaryString(s SessionSummary) string {
	return strings.Join([]string{s.Meta.SessionID, s.Status, s.FinalMaskHash, s.FinalStatsHash}, "/")
}
