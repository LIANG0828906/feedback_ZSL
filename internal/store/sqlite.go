package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"retargetlab/internal/model"
	"retargetlab/internal/solver"
)

const SchemaVersion = 1

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

type Expected struct {
	SkeletonHash     string
	ParamVersion     int
	AlgorithmVersion string
}

type Window struct {
	WindowID         int                  `json:"windowId"`
	StartFrame       int                  `json:"startFrame"`
	EndFrame         int                  `json:"endFrame"`
	SkeletonHash     string               `json:"skeletonHash"`
	MappingHash      string               `json:"mappingHash"`
	ParamVersion     int                  `json:"paramVersion"`
	AlgorithmVersion string               `json:"algorithmVersion"`
	ScaleValues      map[string]float64   `json:"scaleValues"`
	Mapping          model.Mapping        `json:"mapping"`
	Results          []solver.FrameResult `json:"results"`
	ContentHash      string               `json:"contentHash"`
	CommitSeq        int64                `json:"commitSeq"`
}

type windowPayload struct {
	StartFrame       int                  `json:"startFrame"`
	EndFrame         int                  `json:"endFrame"`
	SkeletonHash     string               `json:"skeletonHash"`
	MappingHash      string               `json:"mappingHash"`
	ParamVersion     int                  `json:"paramVersion"`
	AlgorithmVersion string               `json:"algorithmVersion"`
	ScaleValues      map[string]float64   `json:"scaleValues"`
	Mapping          model.Mapping        `json:"mapping"`
	Results          []solver.FrameResult `json:"results"`
}

type RecoveryReport struct {
	LoadedWindows  int    `json:"loadedWindows"`
	LoadedFrames   int    `json:"loadedFrames"`
	CheckedWindows int    `json:"checkedWindows"`
	Invalidated    bool   `json:"invalidated"`
	Reason         string `json:"reason,omitempty"`
}

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, model.Err(model.E_PARAM, "store_open", "database path is required")
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, &model.CodedError{Code: model.E_IO, Op: "store_open", Message: err.Error(), Cause: err}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, &model.CodedError{Code: model.E_IO, Op: "store_open", Message: err.Error(), Cause: err}
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, path: path}
	ctx := context.Background()
	for _, stmt := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA busy_timeout=5000`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return nil, &model.CodedError{Code: model.E_IO, Op: "store_pragma", Message: err.Error(), Cause: err}
		}
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS checkpoints (
			window_id INTEGER PRIMARY KEY,
			start_frame INTEGER NOT NULL,
			end_frame INTEGER NOT NULL,
			skeleton_hash TEXT NOT NULL,
			param_version INTEGER NOT NULL,
			algorithm_version TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			commit_seq INTEGER NOT NULL UNIQUE,
			payload BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS checkpoints_frames ON checkpoints(start_frame, end_frame)`,
		`CREATE TABLE IF NOT EXISTS metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return &model.CodedError{Code: model.E_IO, Op: "store_migrate", Message: err.Error(), Cause: err}
		}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO metadata(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprintf("%d", SchemaVersion))
	if err != nil {
		return &model.CodedError{Code: model.E_IO, Op: "store_migrate", Message: err.Error(), Cause: err}
	}
	return nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) SaveWindow(ctx context.Context, w Window) (Window, error) {
	if w.StartFrame < 0 || w.EndFrame < w.StartFrame {
		return Window{}, model.ErrFrame(model.E_PARAM, "save_window", w.StartFrame, "", "invalid window range")
	}
	if len(w.Results) != w.EndFrame-w.StartFrame+1 {
		return Window{}, model.ErrFrame(model.E_PARAM, "save_window", w.StartFrame, "", "window result count does not match range")
	}
	for i, r := range w.Results {
		if r.Frame != w.StartFrame+i {
			return Window{}, model.ErrFrame(model.E_FRAME, "save_window", r.Frame, "", "window results are not contiguous and ordered")
		}
		if r.Snapshot.SkeletonHash != w.SkeletonHash ||
			r.Snapshot.MappingHash != w.MappingHash ||
			r.Snapshot.ParamVersion != w.ParamVersion ||
			r.Snapshot.AlgorithmVersion != w.AlgorithmVersion {
			return Window{}, model.ErrFrame(model.E_STATE, "save_window", r.Frame, "", "frame snapshot does not match window snapshot")
		}
	}
	w.WindowID = w.StartFrame
	w.ContentHash = contentHash(w)

	s.mu.Lock()
	defer s.mu.Unlock()
	payload, err := json.Marshal(windowPayload{
		StartFrame: w.StartFrame, EndFrame: w.EndFrame, SkeletonHash: w.SkeletonHash,
		MappingHash: w.MappingHash, ParamVersion: w.ParamVersion,
		AlgorithmVersion: w.AlgorithmVersion, ScaleValues: w.ScaleValues,
		Mapping: w.Mapping, Results: w.Results,
	})
	if err != nil {
		return Window{}, &model.CodedError{Code: model.E_IO, Op: "save_window", Message: err.Error(), Cause: err}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Window{}, &model.CodedError{Code: model.E_IO, Op: "save_window", Message: err.Error(), Cause: err}
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var overlaps int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM checkpoints WHERE start_frame <= ? AND end_frame >= ?`,
		w.EndFrame, w.StartFrame).Scan(&overlaps); err != nil {
		return Window{}, &model.CodedError{Code: model.E_IO, Op: "save_window", Message: err.Error(), Cause: err}
	}
	if overlaps > 0 {
		return Window{}, model.ErrFrame(model.E_STATE, "save_window", w.StartFrame, "", "checkpoint window overlaps a frozen window")
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(commit_seq), 0) + 1 FROM checkpoints`).Scan(&seq); err != nil {
		return Window{}, &model.CodedError{Code: model.E_IO, Op: "save_window", Message: err.Error(), Cause: err}
	}
	w.CommitSeq = seq
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO checkpoints(window_id, start_frame, end_frame, skeleton_hash, param_version, algorithm_version, content_hash, commit_seq, payload)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.WindowID, w.StartFrame, w.EndFrame, w.SkeletonHash, w.ParamVersion,
		w.AlgorithmVersion, w.ContentHash, w.CommitSeq, payload,
	); err != nil {
		return Window{}, &model.CodedError{Code: model.E_IO, Op: "save_window", Message: err.Error(), Cause: err}
	}
	if err := tx.Commit(); err != nil {
		return Window{}, &model.CodedError{Code: model.E_IO, Op: "save_window", Message: err.Error(), Cause: err}
	}
	committed = true
	return w, nil
}

func (s *Store) LoadAll(ctx context.Context, expected Expected) ([]Window, RecoveryReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: false})
	if err != nil {
		return nil, RecoveryReport{}, &model.CodedError{Code: model.E_IO, Op: "load_windows", Message: err.Error(), Cause: err}
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	rows, err := tx.QueryContext(ctx,
		`SELECT window_id, start_frame, end_frame, skeleton_hash, param_version, algorithm_version, content_hash, commit_seq, payload
		 FROM checkpoints ORDER BY start_frame ASC, commit_seq ASC`)
	if err != nil {
		return nil, RecoveryReport{}, &model.CodedError{Code: model.E_IO, Op: "load_windows", Message: err.Error(), Cause: err}
	}
	type rowData struct {
		windowID int
		start    int
		end      int
		skel     string
		param    int
		algo     string
		content  string
		seq      int64
		payload  []byte
	}
	var rowsData []rowData
	for rows.Next() {
		var r rowData
		if err := rows.Scan(&r.windowID, &r.start, &r.end, &r.skel, &r.param, &r.algo, &r.content, &r.seq, &r.payload); err != nil {
			_ = rows.Close()
			return nil, RecoveryReport{}, &model.CodedError{Code: model.E_IO, Op: "load_windows", Message: err.Error(), Cause: err}
		}
		rowsData = append(rowsData, r)
	}
	if err := rows.Close(); err != nil {
		return nil, RecoveryReport{}, &model.CodedError{Code: model.E_IO, Op: "load_windows", Message: err.Error(), Cause: err}
	}
	if err := rows.Err(); err != nil {
		return nil, RecoveryReport{}, &model.CodedError{Code: model.E_IO, Op: "load_windows", Message: err.Error(), Cause: err}
	}

	report := RecoveryReport{CheckedWindows: len(rowsData)}
	invalidReason := ""
	var windows []Window
	lastEnd := -1
	for _, r := range rowsData {
		if invalidReason == "" {
			if r.skel != expected.SkeletonHash {
				invalidReason = fmt.Sprintf("skeleton hash mismatch in window %d", r.windowID)
			} else if r.param != expected.ParamVersion {
				invalidReason = fmt.Sprintf("parameter version mismatch in window %d: %d != %d", r.windowID, r.param, expected.ParamVersion)
			} else if r.algo != expected.AlgorithmVersion {
				invalidReason = fmt.Sprintf("algorithm version mismatch in window %d: %q != %q", r.windowID, r.algo, expected.AlgorithmVersion)
			} else if r.start <= lastEnd {
				invalidReason = fmt.Sprintf("overlapping or unordered checkpoint window %d", r.windowID)
			}
		}
		var p windowPayload
		if invalidReason == "" {
			if err := json.Unmarshal(r.payload, &p); err != nil {
				invalidReason = fmt.Sprintf("window %d payload is corrupt: %v", r.windowID, err)
			}
		}
		w := Window{
			WindowID: r.windowID, StartFrame: r.start, EndFrame: r.end,
			SkeletonHash: r.skel, ParamVersion: r.param, AlgorithmVersion: r.algo,
			ContentHash: r.content, CommitSeq: r.seq,
		}
		if invalidReason == "" {
			w.MappingHash = p.MappingHash
			w.ScaleValues = p.ScaleValues
			w.Mapping = p.Mapping
			w.Results = p.Results
			if p.StartFrame != r.start || p.EndFrame != r.end ||
				p.SkeletonHash != r.skel || p.ParamVersion != r.param ||
				p.AlgorithmVersion != r.algo {
				invalidReason = fmt.Sprintf("window %d payload header does not match row", r.windowID)
			} else if w.Mapping.Hash() != w.MappingHash {
				invalidReason = fmt.Sprintf("window %d mapping hash does not match snapshot", r.windowID)
			} else if contentHash(w) != r.content {
				invalidReason = fmt.Sprintf("window %d content hash mismatch", r.windowID)
			} else if len(w.Results) != r.end-r.start+1 {
				invalidReason = fmt.Sprintf("window %d result count mismatch", r.windowID)
			} else {
				for i, res := range w.Results {
					if res.Frame != r.start+i ||
						res.Snapshot.SkeletonHash != r.skel ||
						res.Snapshot.MappingHash != w.MappingHash ||
						res.Snapshot.ParamVersion != r.param ||
						res.Snapshot.AlgorithmVersion != r.algo ||
						res.Snapshot.Mapping.Hash() != w.MappingHash {
						invalidReason = fmt.Sprintf("window %d frame %d snapshot mismatch", r.windowID, res.Frame)
						break
					}
				}
			}
		}
		if invalidReason == "" {
			windows = append(windows, w)
			lastEnd = w.EndFrame
			report.LoadedWindows++
			report.LoadedFrames += len(w.Results)
		}
	}
	if invalidReason != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoints`); err != nil {
			return nil, RecoveryReport{}, &model.CodedError{Code: model.E_IO, Op: "invalidate_windows", Message: err.Error(), Cause: err}
		}
		report.Invalidated = true
		report.Reason = invalidReason
		report.LoadedWindows = 0
		report.LoadedFrames = 0
		windows = nil
	}
	if err := tx.Commit(); err != nil {
		return nil, RecoveryReport{}, &model.CodedError{Code: model.E_IO, Op: "load_windows", Message: err.Error(), Cause: err}
	}
	committed = true
	sort.Slice(windows, func(i, j int) bool { return windows[i].StartFrame < windows[j].StartFrame })
	return windows, report, nil
}

func (s *Store) WipeAll(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM checkpoints`); err != nil {
		return &model.CodedError{Code: model.E_IO, Op: "wipe_windows", Message: err.Error(), Cause: err}
	}
	return nil
}

func (s *Store) Count(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkpoints`).Scan(&n); err != nil {
		return 0, &model.CodedError{Code: model.E_IO, Op: "count_windows", Message: err.Error(), Cause: err}
	}
	return n, nil
}

func contentHash(w Window) string {
	return model.StableHash(windowPayload{
		StartFrame: w.StartFrame, EndFrame: w.EndFrame, SkeletonHash: w.SkeletonHash,
		MappingHash: w.MappingHash, ParamVersion: w.ParamVersion,
		AlgorithmVersion: w.AlgorithmVersion, ScaleValues: w.ScaleValues,
		Mapping: w.Mapping, Results: w.Results,
	})
}

func ComputeContentHash(w Window) string {
	return contentHash(w)
}
