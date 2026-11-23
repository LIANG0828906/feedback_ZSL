package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"ocrworkbench/internal/domain"
)

type Store struct {
	db   *sql.DB
	mu   sync.Mutex
	Path string
}

type RunRecord struct {
	ID               int64
	SampleID         string
	ImageHash        string
	TemplateVersion  string
	ParamVersion     string
	AlgorithmVersion string
	BandWidth        int
	AvgStrokeWidth   float64
	Params           domain.Params
	TuningVersion    string
	Status           string
	Error            *domain.AppError
	CreatedAt        int64
	UpdatedAt        int64
}

type StoredTile struct {
	Tile        domain.Tile
	State       domain.State
	Candidates  []domain.Candidate
	Error       *domain.AppError
	CommitSeq   int64
	PayloadHash string
}

type StoredLine struct {
	Line        domain.DetectedLine
	State       domain.State
	Recognition domain.LineRecognition
	Snapshot    *domain.LineCommitSnapshot
	Error       *domain.AppError
	CommitSeq   int64
	PayloadHash string
}

type Snapshot struct {
	Run     RunRecord
	Tiles   []StoredTile
	Lines   []StoredLine
	Commits []domain.CommitEntry
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_busy_timeout=8000&_journal_mode=WAL&_synchronous=FULL&_txlock=immediate&_foreign_keys=on"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, Path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

func (s *Store) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sample_id TEXT NOT NULL,
			image_hash TEXT NOT NULL,
			template_version TEXT NOT NULL,
			param_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			band_width INTEGER NOT NULL,
			avg_stroke_width REAL NOT NULL,
			params_json TEXT NOT NULL,
			tuning_version TEXT NOT NULL,
			status TEXT NOT NULL,
			error_json TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_sample ON runs(sample_id, id DESC)`,
		`CREATE TABLE IF NOT EXISTS tiles (
			run_id INTEGER NOT NULL,
			tile_index INTEGER NOT NULL,
			state TEXT NOT NULL,
			tile_json TEXT NOT NULL,
			candidates_json TEXT NOT NULL DEFAULT '[]',
			candidate_count INTEGER NOT NULL DEFAULT 0,
			error_json TEXT NOT NULL DEFAULT '',
			commit_seq INTEGER NOT NULL DEFAULT 0,
			payload_hash TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(run_id, tile_index),
			FOREIGN KEY(run_id) REFERENCES runs(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS lines (
			run_id INTEGER NOT NULL,
			reading_index INTEGER NOT NULL,
			state TEXT NOT NULL,
			line_json TEXT NOT NULL,
			result_json TEXT NOT NULL DEFAULT '',
			snapshot_json TEXT NOT NULL DEFAULT '',
			error_json TEXT NOT NULL DEFAULT '',
			commit_seq INTEGER NOT NULL DEFAULT 0,
			payload_hash TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(run_id, reading_index),
			FOREIGN KEY(run_id) REFERENCES runs(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS commits (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			unit_key TEXT NOT NULL,
			payload_hash TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			FOREIGN KEY(run_id) REFERENCES runs(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_commits_run ON commits(run_id, seq)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) OpenOrCreateRun(identity domain.SnapshotIdentity, sampleID string, params domain.Params, avgStroke float64) (RunRecord, []string, *domain.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return RunRecord{}, nil, storeError("begin run transaction", err)
	}
	defer tx.Rollback()

	record, found, err := latestRunTx(tx, sampleID)
	if err != nil {
		return RunRecord{}, nil, storeError("load latest run", err)
	}
	invalidated := make([]string, 0)
	if found {
		checks := []struct {
			field    string
			expected string
			actual   string
		}{
			{"image_hash", identity.ImageHash, record.ImageHash},
			{"template_version", identity.TemplateVersion, record.TemplateVersion},
			{"param_version", identity.ParamVersion, record.ParamVersion},
			{"algorithm_version", identity.AlgorithmVersion, record.AlgorithmVersion},
			{"band_width", fmt.Sprint(identity.BandWidth), fmt.Sprint(record.BandWidth)},
		}
		for _, check := range checks {
			if check.expected != check.actual {
				invalidated = append(invalidated, check.field)
			}
		}
		if len(invalidated) > 0 {
			if _, err := tx.Exec(`DELETE FROM commits WHERE run_id=?`, record.ID); err != nil {
				return RunRecord{}, nil, storeError("invalidate commits", err)
			}
			if _, err := tx.Exec(`DELETE FROM lines WHERE run_id=?`, record.ID); err != nil {
				return RunRecord{}, nil, storeError("invalidate lines", err)
			}
			if _, err := tx.Exec(`DELETE FROM tiles WHERE run_id=?`, record.ID); err != nil {
				return RunRecord{}, nil, storeError("invalidate tiles", err)
			}
			if _, err := tx.Exec(`DELETE FROM runs WHERE id=?`, record.ID); err != nil {
				return RunRecord{}, nil, storeError("invalidate run", err)
			}
			found = false
		}
	}
	if !found {
		now := time.Now().UnixNano()
		paramsJSON := mustJSON(params)
		result, err := tx.Exec(`INSERT INTO runs
			(sample_id, image_hash, template_version, param_version, algorithm_version, band_width, avg_stroke_width, params_json, tuning_version, status, error_json, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'idle', '', ?, ?)`,
			sampleID, identity.ImageHash, identity.TemplateVersion, identity.ParamVersion, identity.AlgorithmVersion,
			identity.BandWidth, avgStroke, paramsJSON, params.TuningVersion(), now, now)
		if err != nil {
			return RunRecord{}, invalidated, storeError("create run", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return RunRecord{}, invalidated, storeError("read run id", err)
		}
		record = RunRecord{
			ID:               id,
			SampleID:         sampleID,
			ImageHash:        identity.ImageHash,
			TemplateVersion:  identity.TemplateVersion,
			ParamVersion:     identity.ParamVersion,
			AlgorithmVersion: identity.AlgorithmVersion,
			BandWidth:        identity.BandWidth,
			AvgStrokeWidth:   avgStroke,
			Params:           params,
			TuningVersion:    params.TuningVersion(),
			Status:           "idle",
			CreatedAt:        now,
			UpdatedAt:        now,
		}
	}
	if err := tx.Commit(); err != nil {
		return RunRecord{}, invalidated, storeError("commit run transaction", err)
	}
	return record, invalidated, nil
}

func latestRunTx(tx *sql.Tx, sampleID string) (RunRecord, bool, error) {
	row := tx.QueryRow(`SELECT id, sample_id, image_hash, template_version, param_version, algorithm_version,
		band_width, avg_stroke_width, params_json, tuning_version, status, error_json, created_at, updated_at
		FROM runs WHERE sample_id=? ORDER BY id DESC LIMIT 1`, sampleID)
	return scanRun(row)
}

func (s *Store) LatestRun(sampleID string) (RunRecord, bool, *domain.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`SELECT id, sample_id, image_hash, template_version, param_version, algorithm_version,
		band_width, avg_stroke_width, params_json, tuning_version, status, error_json, created_at, updated_at
		FROM runs WHERE sample_id=? ORDER BY id DESC LIMIT 1`, sampleID)
	record, found, err := scanRun(row)
	if err != nil {
		return RunRecord{}, false, storeError("load latest run", err)
	}
	return record, found, nil
}

func (s *Store) EnsureTiles(runID int64, tiles []domain.Tile) *domain.AppError {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return storeError("begin tile transaction", err)
	}
	defer tx.Rollback()
	for _, tile := range tiles {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO tiles
			(run_id, tile_index, state, tile_json, candidates_json, candidate_count, error_json, commit_seq, payload_hash)
			VALUES (?, ?, ?, ?, '[]', 0, '', 0, '')`,
			runID, tile.Index, string(domain.TilePending), mustJSON(tile)); err != nil {
			return storeError("insert pending tile", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return storeError("commit tile transaction", err)
	}
	return nil
}

func (s *Store) EnsureLines(runID int64, lines []domain.DetectedLine) *domain.AppError {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return storeError("begin line transaction", err)
	}
	defer tx.Rollback()
	for _, line := range lines {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO lines
			(run_id, reading_index, state, line_json, result_json, snapshot_json, error_json, commit_seq, payload_hash)
			VALUES (?, ?, ?, ?, '', '', '', 0, '')`,
			runID, line.ReadingIndex, string(domain.LinePending), mustJSON(line)); err != nil {
			return storeError("insert pending line", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return storeError("commit line transaction", err)
	}
	return nil
}

func (s *Store) TransitionTile(runID int64, tileIndex int, to domain.State) *domain.AppError {
	s.mu.Lock()
	defer s.mu.Unlock()
	from, appErr := tileState(s.db, runID, tileIndex)
	if appErr != nil {
		return appErr
	}
	if !domain.CanTileTransition(from, to) {
		return domain.StateError("tile", tileIndex, -1, from, to)
	}
	if _, err := s.db.Exec(`UPDATE tiles SET state=? WHERE run_id=? AND tile_index=?`, string(to), runID, tileIndex); err != nil {
		return storeError("update tile state", err)
	}
	return nil
}

func (s *Store) TransitionLine(runID int64, lineIndex int, to domain.State) *domain.AppError {
	s.mu.Lock()
	defer s.mu.Unlock()
	from, appErr := lineState(s.db, runID, lineIndex)
	if appErr != nil {
		return appErr
	}
	if !domain.CanLineTransition(from, to) {
		return domain.StateError("line", -1, lineIndex, from, to)
	}
	if _, err := s.db.Exec(`UPDATE lines SET state=? WHERE run_id=? AND reading_index=?`, string(to), runID, lineIndex); err != nil {
		return storeError("update line state", err)
	}
	return nil
}

func (s *Store) CommitTile(runID int64, payload domain.TileCommitPayload, payloadHash string) (int64, *domain.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, storeError("begin tile commit", err)
	}
	defer tx.Rollback()
	from, appErr := tileStateTx(tx, runID, payload.Tile.Index)
	if appErr != nil {
		return 0, appErr
	}
	if from != domain.TileDetecting {
		return 0, domain.StateError("tile", payload.Tile.Index, -1, from, domain.TileCommitted)
	}
	var pendingPrior int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tiles WHERE run_id=? AND tile_index<? AND state<>?`,
		runID, payload.Tile.Index, string(domain.TileCommitted)).Scan(&pendingPrior); err != nil {
		return 0, storeError("check tile commit order", err)
	}
	if pendingPrior > 0 {
		e := domain.Errorf(domain.CodeState, "tile commit sequence is not in scan order")
		e.Unit = "tile"
		e.TileIndex = payload.Tile.Index
		e.Field = "tile_index"
		return 0, e
	}
	result, err := tx.Exec(`INSERT INTO commits(run_id, kind, unit_key, payload_hash, payload_json, created_at)
		VALUES (?, 'tile', ?, ?, ?, ?)`,
		runID, fmt.Sprintf("tile:%06d", payload.Tile.Index), payloadHash, mustJSON(payload), payload.CommittedUnixNano)
	if err != nil {
		return 0, storeError("insert tile commit", err)
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return 0, storeError("read tile commit sequence", err)
	}
	if _, err := tx.Exec(`UPDATE tiles SET state=?, candidates_json=?, candidate_count=?, error_json='', commit_seq=?, payload_hash=?
		WHERE run_id=? AND tile_index=?`,
		string(domain.TileCommitted), mustJSON(payload.Candidates), len(payload.Candidates), seq, payloadHash,
		runID, payload.Tile.Index); err != nil {
		return 0, storeError("commit tile row", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, storeError("commit tile transaction", err)
	}
	return seq, nil
}

func (s *Store) CommitLine(runID int64, snapshot domain.LineCommitSnapshot, payloadHash string) (int64, *domain.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, storeError("begin line commit", err)
	}
	defer tx.Rollback()
	index := snapshot.Line.ReadingIndex
	from, appErr := lineStateTx(tx, runID, index)
	if appErr != nil {
		return 0, appErr
	}
	if from != domain.LineRecognizing {
		return 0, domain.StateError("line", -1, index, from, domain.LineCommitted)
	}
	var pendingTiles int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tiles WHERE run_id=? AND state<>?`, runID, string(domain.TileCommitted)).Scan(&pendingTiles); err != nil {
		return 0, storeError("check committed tiles", err)
	}
	if pendingTiles > 0 {
		return 0, domain.Errorf(domain.CodeState, "text line cannot commit before all tiles are committed")
	}
	var pendingPrior int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM lines WHERE run_id=? AND reading_index<? AND state<>?`,
		runID, index, string(domain.LineCommitted)).Scan(&pendingPrior); err != nil {
		return 0, storeError("check line reading order", err)
	}
	if pendingPrior > 0 {
		e := domain.Errorf(domain.CodeRead, "line commit sequence is not in reading order")
		e.Unit = "line"
		e.LineIndex = index
		e.Field = "reading_index"
		return 0, e
	}
	result, err := tx.Exec(`INSERT INTO commits(run_id, kind, unit_key, payload_hash, payload_json, created_at)
		VALUES (?, 'line', ?, ?, ?, ?)`,
		runID, fmt.Sprintf("line:%06d", index), payloadHash, mustJSON(snapshot), snapshot.CommittedUnixNano)
	if err != nil {
		return 0, storeError("insert line commit", err)
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return 0, storeError("read line commit sequence", err)
	}
	if _, err := tx.Exec(`UPDATE lines SET state=?, result_json=?, snapshot_json=?, error_json='', commit_seq=?, payload_hash=?
		WHERE run_id=? AND reading_index=?`,
		string(domain.LineCommitted), mustJSON(snapshot.Recognition), mustJSON(snapshot), seq, payloadHash,
		runID, index); err != nil {
		return 0, storeError("commit line row", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, storeError("commit line transaction", err)
	}
	return seq, nil
}

func (s *Store) FailTile(runID int64, tileIndex int, appErr *domain.AppError) *domain.AppError {
	s.mu.Lock()
	defer s.mu.Unlock()
	from, stateErr := tileState(s.db, runID, tileIndex)
	if stateErr != nil {
		return stateErr
	}
	if !domain.CanTileTransition(from, domain.TileFailed) {
		return domain.StateError("tile", tileIndex, -1, from, domain.TileFailed)
	}
	if _, err := s.db.Exec(`UPDATE tiles SET state=?, error_json=? WHERE run_id=? AND tile_index=?`,
		string(domain.TileFailed), mustJSON(appErr), runID, tileIndex); err != nil {
		return storeError("fail tile", err)
	}
	return nil
}

func (s *Store) FailLine(runID int64, lineIndex int, appErr *domain.AppError) *domain.AppError {
	s.mu.Lock()
	defer s.mu.Unlock()
	from, stateErr := lineState(s.db, runID, lineIndex)
	if stateErr != nil {
		return stateErr
	}
	if !domain.CanLineTransition(from, domain.LineFailed) {
		return domain.StateError("line", -1, lineIndex, from, domain.LineFailed)
	}
	if _, err := s.db.Exec(`UPDATE lines SET state=?, error_json=? WHERE run_id=? AND reading_index=?`,
		string(domain.LineFailed), mustJSON(appErr), runID, lineIndex); err != nil {
		return storeError("fail line", err)
	}
	return nil
}

func (s *Store) SetRunStatus(runID int64, status string, appErr *domain.AppError) *domain.AppError {
	s.mu.Lock()
	defer s.mu.Unlock()
	errorJSON := ""
	if appErr != nil {
		errorJSON = mustJSON(appErr)
	}
	if _, err := s.db.Exec(`UPDATE runs SET status=?, error_json=?, updated_at=? WHERE id=?`,
		status, errorJSON, time.Now().UnixNano(), runID); err != nil {
		return storeError("update run status", err)
	}
	return nil
}

func (s *Store) UpdateRunParams(runID int64, params domain.Params) *domain.AppError {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`UPDATE runs SET params_json=?, tuning_version=?, updated_at=? WHERE id=?`,
		mustJSON(params), params.TuningVersion(), time.Now().UnixNano(), runID); err != nil {
		return storeError("update run params", err)
	}
	return nil
}

func (s *Store) Snapshot(runID int64) (Snapshot, *domain.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`SELECT id, sample_id, image_hash, template_version, param_version, algorithm_version,
		band_width, avg_stroke_width, params_json, tuning_version, status, error_json, created_at, updated_at
		FROM runs WHERE id=?`, runID)
	run, found, err := scanRun(row)
	if err != nil {
		return Snapshot{}, storeError("load run", err)
	}
	if !found {
		e := domain.Errorf(domain.CodeStore, "run does not exist")
		e.Unit = "run"
		return Snapshot{}, e
	}
	tiles, appErr := loadTiles(s.db, runID)
	if appErr != nil {
		return Snapshot{}, appErr
	}
	lines, appErr := loadLines(s.db, runID)
	if appErr != nil {
		return Snapshot{}, appErr
	}
	commits, appErr := loadCommits(s.db, runID)
	if appErr != nil {
		return Snapshot{}, appErr
	}
	return Snapshot{Run: run, Tiles: tiles, Lines: lines, Commits: commits}, nil
}

func (s *Store) DeleteRun(runID int64) *domain.AppError {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return storeError("begin delete run", err)
	}
	defer tx.Rollback()
	for _, table := range []string{"commits", "lines", "tiles", "runs"} {
		query := "DELETE FROM " + table + " WHERE "
		if table == "runs" {
			query += "id=?"
		} else {
			query += "run_id=?"
		}
		if _, err := tx.Exec(query, runID); err != nil {
			return storeError("delete run data", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return storeError("commit delete run", err)
	}
	return nil
}

func (s *Store) DeleteSampleRuns(sampleID string) *domain.AppError {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return storeError("begin delete sample runs", err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM runs WHERE sample_id=?`, sampleID)
	if err != nil {
		return storeError("find sample runs", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return storeError("scan sample run", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		for _, table := range []string{"commits", "lines", "tiles"} {
			if _, err := tx.Exec("DELETE FROM "+table+" WHERE run_id=?", id); err != nil {
				return storeError("delete sample run data", err)
			}
		}
		if _, err := tx.Exec(`DELETE FROM runs WHERE id=?`, id); err != nil {
			return storeError("delete sample run", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return storeError("commit delete sample runs", err)
	}
	return nil
}

func tileState(db interface {
	QueryRow(string, ...any) *sql.Row
}, runID int64, tileIndex int) (domain.State, *domain.AppError) {
	var state string
	if err := db.QueryRow(`SELECT state FROM tiles WHERE run_id=? AND tile_index=?`, runID, tileIndex).Scan(&state); err != nil {
		if err == sql.ErrNoRows {
			e := domain.Errorf(domain.CodeState, "tile does not exist")
			e.TileIndex = tileIndex
			e.Unit = "tile"
			return "", e
		}
		return "", storeError("load tile state", err)
	}
	return domain.State(state), nil
}

func lineState(db interface {
	QueryRow(string, ...any) *sql.Row
}, runID int64, lineIndex int) (domain.State, *domain.AppError) {
	var state string
	if err := db.QueryRow(`SELECT state FROM lines WHERE run_id=? AND reading_index=?`, runID, lineIndex).Scan(&state); err != nil {
		if err == sql.ErrNoRows {
			e := domain.Errorf(domain.CodeState, "text line does not exist")
			e.LineIndex = lineIndex
			e.Unit = "line"
			return "", e
		}
		return "", storeError("load line state", err)
	}
	return domain.State(state), nil
}

func tileStateTx(tx *sql.Tx, runID int64, tileIndex int) (domain.State, *domain.AppError) {
	return tileState(tx, runID, tileIndex)
}

func lineStateTx(tx *sql.Tx, runID int64, lineIndex int) (domain.State, *domain.AppError) {
	return lineState(tx, runID, lineIndex)
}

func loadTiles(db interface {
	Query(string, ...any) (*sql.Rows, error)
}, runID int64) ([]StoredTile, *domain.AppError) {
	rows, err := db.Query(`SELECT tile_json, state, candidates_json, error_json, commit_seq, payload_hash
		FROM tiles WHERE run_id=? ORDER BY tile_index`, runID)
	if err != nil {
		return nil, storeError("query tiles", err)
	}
	defer rows.Close()
	out := make([]StoredTile, 0)
	for rows.Next() {
		var tileJSON, state, candidatesJSON, errorJSON, payloadHash string
		var commitSeq int64
		if err := rows.Scan(&tileJSON, &state, &candidatesJSON, &errorJSON, &commitSeq, &payloadHash); err != nil {
			return nil, storeError("scan tile", err)
		}
		var tile domain.Tile
		if err := json.Unmarshal([]byte(tileJSON), &tile); err != nil {
			return nil, storeError("decode tile", err)
		}
		var candidates []domain.Candidate
		if candidatesJSON != "" {
			if err := json.Unmarshal([]byte(candidatesJSON), &candidates); err != nil {
				return nil, storeError("decode candidates", err)
			}
		}
		var appErr *domain.AppError
		if errorJSON != "" {
			appErr = &domain.AppError{}
			if err := json.Unmarshal([]byte(errorJSON), appErr); err != nil {
				return nil, storeError("decode tile error", err)
			}
		}
		out = append(out, StoredTile{
			Tile:        tile,
			State:       domain.State(state),
			Candidates:  candidates,
			Error:       appErr,
			CommitSeq:   commitSeq,
			PayloadHash: payloadHash,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, storeError("iterate tiles", err)
	}
	return out, nil
}

func loadLines(db interface {
	Query(string, ...any) (*sql.Rows, error)
}, runID int64) ([]StoredLine, *domain.AppError) {
	rows, err := db.Query(`SELECT line_json, state, result_json, snapshot_json, error_json, commit_seq, payload_hash
		FROM lines WHERE run_id=? ORDER BY reading_index`, runID)
	if err != nil {
		return nil, storeError("query lines", err)
	}
	defer rows.Close()
	out := make([]StoredLine, 0)
	for rows.Next() {
		var lineJSON, state, resultJSON, snapshotJSON, errorJSON, payloadHash string
		var commitSeq int64
		if err := rows.Scan(&lineJSON, &state, &resultJSON, &snapshotJSON, &errorJSON, &commitSeq, &payloadHash); err != nil {
			return nil, storeError("scan line", err)
		}
		var line domain.DetectedLine
		if err := json.Unmarshal([]byte(lineJSON), &line); err != nil {
			return nil, storeError("decode line", err)
		}
		var recognition domain.LineRecognition
		if resultJSON != "" {
			if err := json.Unmarshal([]byte(resultJSON), &recognition); err != nil {
				return nil, storeError("decode recognition", err)
			}
		}
		var snapshot *domain.LineCommitSnapshot
		if snapshotJSON != "" {
			snapshot = &domain.LineCommitSnapshot{}
			if err := json.Unmarshal([]byte(snapshotJSON), snapshot); err != nil {
				return nil, storeError("decode line snapshot", err)
			}
		}
		var appErr *domain.AppError
		if errorJSON != "" {
			appErr = &domain.AppError{}
			if err := json.Unmarshal([]byte(errorJSON), appErr); err != nil {
				return nil, storeError("decode line error", err)
			}
		}
		out = append(out, StoredLine{
			Line:        line,
			State:       domain.State(state),
			Recognition: recognition,
			Snapshot:    snapshot,
			Error:       appErr,
			CommitSeq:   commitSeq,
			PayloadHash: payloadHash,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, storeError("iterate lines", err)
	}
	return out, nil
}

func loadCommits(db interface {
	Query(string, ...any) (*sql.Rows, error)
}, runID int64) ([]domain.CommitEntry, *domain.AppError) {
	rows, err := db.Query(`SELECT seq, kind, unit_key, payload_hash, payload_json FROM commits WHERE run_id=? ORDER BY seq`, runID)
	if err != nil {
		return nil, storeError("query commits", err)
	}
	defer rows.Close()
	out := make([]domain.CommitEntry, 0)
	for rows.Next() {
		var entry domain.CommitEntry
		if err := rows.Scan(&entry.Seq, &entry.Kind, &entry.UnitKey, &entry.PayloadHash, &entry.PayloadJSON); err != nil {
			return nil, storeError("scan commit", err)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, storeError("iterate commits", err)
	}
	return out, nil
}

func scanRun(scanner interface{ Scan(...any) error }) (RunRecord, bool, error) {
	var record RunRecord
	var paramsJSON, tuningVersion, status, errorJSON string
	err := scanner.Scan(
		&record.ID, &record.SampleID, &record.ImageHash, &record.TemplateVersion,
		&record.ParamVersion, &record.AlgorithmVersion, &record.BandWidth,
		&record.AvgStrokeWidth, &paramsJSON, &tuningVersion, &status, &errorJSON,
		&record.CreatedAt, &record.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return RunRecord{}, false, nil
	}
	if err != nil {
		return RunRecord{}, false, err
	}
	if err := json.Unmarshal([]byte(paramsJSON), &record.Params); err != nil {
		return RunRecord{}, false, err
	}
	record.TuningVersion = tuningVersion
	record.Status = status
	if strings.TrimSpace(errorJSON) != "" {
		record.Error = &domain.AppError{}
		if err := json.Unmarshal([]byte(errorJSON), record.Error); err != nil {
			return RunRecord{}, false, err
		}
	}
	return record, true, nil
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func storeError(message string, err error) *domain.AppError {
	e := domain.Errorf(domain.CodeStore, message)
	if err != nil {
		e.Cause = err.Error()
	}
	return e
}
