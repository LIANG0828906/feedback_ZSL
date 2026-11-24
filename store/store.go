package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"virtualstudio/compose"
	"virtualstudio/model"
)

var ErrNotFound = errors.New("not found")

type InvalidatedError struct {
	Reason string
}

func (e *InvalidatedError) Error() string {
	return "persistent snapshot invalidated: " + e.Reason
}

type Project struct {
	ID               string `json:"id"`
	AlgorithmVersion string `json:"algorithm_version"`
	SourceVersion    string `json:"source_version"`
	BaseHash         string `json:"base_hash"`
	WindowSize       int    `json:"window_size"`
	FrameCount       int    `json:"frame_count"`
	ProjectVersion   int64  `json:"project_version"`
	TargetFormat     string `json:"target_format"`
	SceneJSON        []byte `json:"scene_json"`
	CommandsJSON     []byte `json:"commands_json"`
	Complete         bool   `json:"complete"`
}

type Checkpoint struct {
	ProjectID        string                `json:"project_id"`
	WindowIndex      int                   `json:"window_index"`
	StartFrame       int                   `json:"start_frame"`
	EndFrame         int                   `json:"end_frame"`
	Sequence         int                   `json:"sequence"`
	ParamVersion     string                `json:"param_version"`
	ContentHash      string                `json:"content_hash"`
	FrameDigest      string                `json:"frame_digest"`
	SourceVersion    string                `json:"source_version"`
	AlgorithmVersion string                `json:"algorithm_version"`
	Snapshot         *model.WindowSnapshot `json:"snapshot"`
	Frames           []model.Frame         `json:"frames"`
}

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, model.NewError(model.CodeParam, "db_path", "must not be empty")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, model.WrapError(model.CodeIO, path, "create database directory", err)
		}
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, model.WrapError(model.CodeIO, path, "open sqlite", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, path: path}
	if err := s.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func sqliteDSN(path string) string {
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return path
	}
	return "file:" + filepath.ToSlash(path)
}

func (s *Store) Path() string { return s.path }

func (s *Store) init(ctx context.Context) error {
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS projects (
			id TEXT PRIMARY KEY,
			algorithm_version TEXT NOT NULL,
			source_version TEXT NOT NULL,
			base_hash TEXT NOT NULL,
			window_size INTEGER NOT NULL,
			frame_count INTEGER NOT NULL,
			project_version INTEGER NOT NULL,
			target_format TEXT NOT NULL,
			scene_json BLOB NOT NULL,
			commands_json BLOB NOT NULL,
			complete INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoints (
			project_id TEXT NOT NULL,
			window_index INTEGER NOT NULL,
			start_frame INTEGER NOT NULL,
			end_frame INTEGER NOT NULL,
			sequence INTEGER NOT NULL,
			param_version TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			frame_digest TEXT NOT NULL,
			source_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			snapshot_json BLOB NOT NULL,
			frames_json BLOB NOT NULL,
			PRIMARY KEY (project_id, window_index),
			UNIQUE (project_id, sequence)
		)`,
		`CREATE TABLE IF NOT EXISTS exports (
			project_id TEXT NOT NULL,
			export_name TEXT NOT NULL,
			valid INTEGER NOT NULL,
			export_version TEXT NOT NULL,
			PRIMARY KEY (project_id, export_name)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return model.WrapError(model.CodeIO, s.path, "initialize sqlite", err)
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) SaveProject(ctx context.Context, project Project) error {
	if project.ID == "" {
		return model.NewError(model.CodeParam, "project.id", "must not be empty")
	}
	if project.AlgorithmVersion == "" {
		project.AlgorithmVersion = model.AlgorithmVersion
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (id, algorithm_version, source_version, base_hash, window_size, frame_count,
			project_version, target_format, scene_json, commands_json, complete)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			algorithm_version=excluded.algorithm_version,
			source_version=excluded.source_version,
			base_hash=excluded.base_hash,
			window_size=excluded.window_size,
			frame_count=excluded.frame_count,
			project_version=excluded.project_version,
			target_format=excluded.target_format,
			scene_json=excluded.scene_json,
			commands_json=excluded.commands_json,
			complete=excluded.complete`,
		project.ID, project.AlgorithmVersion, project.SourceVersion, project.BaseHash, project.WindowSize,
		project.FrameCount, project.ProjectVersion, project.TargetFormat, project.SceneJSON, project.CommandsJSON,
		boolInt(project.Complete))
	if err != nil {
		return model.WrapError(model.CodeIO, "projects", "save project", err)
	}
	return nil
}

func (s *Store) LoadProject(ctx context.Context, id string) (*Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRowContext(ctx, `
		SELECT id, algorithm_version, source_version, base_hash, window_size, frame_count,
		       project_version, target_format, scene_json, commands_json, complete
		FROM projects WHERE id = ?`, id)
	var p Project
	var complete int
	err := row.Scan(&p.ID, &p.AlgorithmVersion, &p.SourceVersion, &p.BaseHash, &p.WindowSize, &p.FrameCount,
		&p.ProjectVersion, &p.TargetFormat, &p.SceneJSON, &p.CommandsJSON, &complete)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, model.WrapError(model.CodeIO, "projects", "load project", err)
	}
	p.Complete = complete != 0
	return &p, nil
}

func (s *Store) PutCheckpoint(ctx context.Context, cp Checkpoint) error {
	if cp.Snapshot == nil {
		return model.NewError(model.CodeState, "checkpoint.snapshot", "missing frozen snapshot")
	}
	if cp.WindowIndex == 0 {
		cp.WindowIndex = cp.Snapshot.WindowIndex
	}
	if cp.StartFrame == 0 {
		cp.StartFrame = cp.Snapshot.StartFrame
	}
	if cp.EndFrame == 0 {
		cp.EndFrame = cp.Snapshot.EndFrame
	}
	if cp.SourceVersion == "" {
		cp.SourceVersion = cp.Snapshot.SourceVersion
	}
	if cp.AlgorithmVersion == "" {
		cp.AlgorithmVersion = model.AlgorithmVersion
	}
	cp.Sequence = cp.WindowIndex
	if cp.Snapshot.ParamVersion == "" {
		cp.Snapshot.ParamVersion = model.ParamVersionFor(cp.Snapshot)
	}
	if cp.ParamVersion == "" {
		cp.ParamVersion = cp.Snapshot.ParamVersion
	}
	if cp.ContentHash == "" {
		cp.ContentHash = cp.Snapshot.ContentHash
	}
	if cp.ParamVersion != model.ParamVersionFor(cp.Snapshot) {
		return model.NewError(model.CodeState, "checkpoint.param_version", "snapshot parameter version mismatch")
	}
	if cp.ContentHash != model.SnapshotContentHash(cp.Snapshot) {
		return model.NewError(model.CodeState, "checkpoint.content_hash", "snapshot content hash mismatch")
	}
	if cp.FrameDigest == "" {
		cp.FrameDigest = compose.WindowDigest(cp.Frames)
	}
	if cp.FrameDigest != compose.WindowDigest(cp.Frames) {
		return model.NewError(model.CodeState, "checkpoint.frame_digest", "frame digest mismatch")
	}
	if err := validateWindowFrames(cp.Snapshot, cp.Frames); err != nil {
		return model.NewError(model.CodeState, "checkpoint.frames", err.Error())
	}
	snapshotJSON, err := json.Marshal(cp.Snapshot)
	if err != nil {
		return model.WrapError(model.CodeIO, "checkpoint.snapshot", "marshal snapshot", err)
	}
	framesJSON, err := json.Marshal(cp.Frames)
	if err != nil {
		return model.WrapError(model.CodeIO, "checkpoint.frames", "marshal frames", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.WrapError(model.CodeIO, "checkpoints", "begin transaction", err)
	}
	defer tx.Rollback()
	var algorithm, source string
	err = tx.QueryRowContext(ctx, `SELECT algorithm_version, source_version FROM projects WHERE id = ?`, cp.ProjectID).Scan(&algorithm, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return model.NewError(model.CodeState, "checkpoint.project", "project does not exist")
	}
	if err != nil {
		return model.WrapError(model.CodeIO, "checkpoints", "read project versions", err)
	}
	if algorithm != cp.AlgorithmVersion || source != cp.SourceVersion {
		return model.NewError(model.CodeState, "checkpoint.versions", "algorithm or source version mismatch")
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO checkpoints (project_id, window_index, start_frame, end_frame, sequence, param_version,
			content_hash, frame_digest, source_version, algorithm_version, snapshot_json, frames_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, window_index) DO UPDATE SET
			start_frame=excluded.start_frame,
			end_frame=excluded.end_frame,
			sequence=excluded.sequence,
			param_version=excluded.param_version,
			content_hash=excluded.content_hash,
			frame_digest=excluded.frame_digest,
			source_version=excluded.source_version,
			algorithm_version=excluded.algorithm_version,
			snapshot_json=excluded.snapshot_json,
			frames_json=excluded.frames_json`,
		cp.ProjectID, cp.WindowIndex, cp.StartFrame, cp.EndFrame, cp.Sequence, cp.ParamVersion,
		cp.ContentHash, cp.FrameDigest, cp.SourceVersion, cp.AlgorithmVersion, snapshotJSON, framesJSON); err != nil {
		return model.WrapError(model.CodeIO, "checkpoints", "write checkpoint", err)
	}
	if err := tx.Commit(); err != nil {
		return model.WrapError(model.CodeIO, "checkpoints", "commit checkpoint", err)
	}
	return nil
}

func (s *Store) LoadCheckpoints(ctx context.Context, projectID string) ([]Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var projectAlgorithm, projectSource string
	var windowSize, frameCount int
	err := s.db.QueryRowContext(ctx, `
		SELECT algorithm_version, source_version, window_size, frame_count
		FROM projects WHERE id = ?`, projectID).Scan(&projectAlgorithm, &projectSource, &windowSize, &frameCount)
	if errors.Is(err, sql.ErrNoRows) {
		_ = s.purgeLocked(ctx, projectID)
		return nil, &InvalidatedError{Reason: "project metadata is missing"}
	}
	if err != nil {
		return nil, model.WrapError(model.CodeIO, "projects", "read project metadata", err)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, window_index, start_frame, end_frame, sequence, param_version, content_hash,
		       frame_digest, source_version, algorithm_version, snapshot_json, frames_json
		FROM checkpoints WHERE project_id = ? ORDER BY window_index`, projectID)
	if err != nil {
		return nil, model.WrapError(model.CodeIO, "checkpoints", "query checkpoints", err)
	}
	defer rows.Close()
	var out []Checkpoint
	for rows.Next() {
		var cp Checkpoint
		var snapshotJSON, framesJSON []byte
		if err := rows.Scan(&cp.ProjectID, &cp.WindowIndex, &cp.StartFrame, &cp.EndFrame, &cp.Sequence,
			&cp.ParamVersion, &cp.ContentHash, &cp.FrameDigest, &cp.SourceVersion, &cp.AlgorithmVersion,
			&snapshotJSON, &framesJSON); err != nil {
			return nil, model.WrapError(model.CodeIO, "checkpoints", "scan checkpoint", err)
		}
		if err := json.Unmarshal(snapshotJSON, &cp.Snapshot); err != nil {
			_ = rows.Close()
			return nil, s.invalidateLocked(ctx, projectID, "snapshot JSON decode failed")
		}
		if err := json.Unmarshal(framesJSON, &cp.Frames); err != nil {
			_ = rows.Close()
			return nil, s.invalidateLocked(ctx, projectID, "frame JSON decode failed")
		}
		out = append(out, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, model.WrapError(model.CodeIO, "checkpoints", "iterate checkpoints", err)
	}
	if err := rows.Close(); err != nil {
		return nil, model.WrapError(model.CodeIO, "checkpoints", "close checkpoint rows", err)
	}
	if err := validateCheckpoints(out, projectAlgorithm, projectSource, windowSize, frameCount); err != nil {
		return nil, s.invalidateLocked(ctx, projectID, err.Error())
	}
	return out, nil
}

func validateCheckpoints(checkpoints []Checkpoint, projectAlgorithm, projectSource string, windowSize, frameCount int) error {
	if windowSize <= 0 || frameCount <= 0 {
		return fmt.Errorf("project window or frame count is invalid")
	}
	windowCount := (frameCount + windowSize - 1) / windowSize
	last := -1
	for _, cp := range checkpoints {
		if cp.WindowIndex <= last {
			return fmt.Errorf("window index order is not strictly increasing at %d", cp.WindowIndex)
		}
		last = cp.WindowIndex
		if cp.WindowIndex < 0 || cp.WindowIndex >= windowCount {
			return fmt.Errorf("window %d is outside project range", cp.WindowIndex)
		}
		expectedStart := cp.WindowIndex * windowSize
		expectedEnd := expectedStart + windowSize - 1
		if expectedEnd >= frameCount {
			expectedEnd = frameCount - 1
		}
		if cp.StartFrame != expectedStart || cp.EndFrame != expectedEnd {
			return fmt.Errorf("window %d frame range mismatch", cp.WindowIndex)
		}
		if cp.Snapshot == nil {
			return fmt.Errorf("window %d has no snapshot", cp.WindowIndex)
		}
		if cp.WindowIndex != cp.Snapshot.WindowIndex || cp.StartFrame != cp.Snapshot.StartFrame || cp.EndFrame != cp.Snapshot.EndFrame {
			return fmt.Errorf("window %d metadata does not match snapshot", cp.WindowIndex)
		}
		if cp.SourceVersion != cp.Snapshot.SourceVersion || cp.AlgorithmVersion != model.AlgorithmVersion ||
			cp.SourceVersion != projectSource || cp.AlgorithmVersion != projectAlgorithm {
			return fmt.Errorf("window %d source or algorithm version mismatch", cp.WindowIndex)
		}
		if cp.ParamVersion != model.ParamVersionFor(cp.Snapshot) {
			return fmt.Errorf("window %d parameter version mismatch", cp.WindowIndex)
		}
		if cp.ContentHash != model.SnapshotContentHash(cp.Snapshot) {
			return fmt.Errorf("window %d content hash mismatch", cp.WindowIndex)
		}
		if cp.FrameDigest != compose.WindowDigest(cp.Frames) {
			return fmt.Errorf("window %d frame digest mismatch", cp.WindowIndex)
		}
		if len(cp.Frames) != cp.EndFrame-cp.StartFrame+1 {
			return fmt.Errorf("window %d frame count mismatch", cp.WindowIndex)
		}
		if cp.Sequence != cp.WindowIndex {
			return fmt.Errorf("window %d unstable write sequence %d", cp.WindowIndex, cp.Sequence)
		}
		if err := validateWindowFrames(cp.Snapshot, cp.Frames); err != nil {
			return fmt.Errorf("window %d: %w", cp.WindowIndex, err)
		}
	}
	return nil
}

func validateWindowFrames(snapshot *model.WindowSnapshot, frames []model.Frame) error {
	if snapshot == nil {
		return fmt.Errorf("missing snapshot")
	}
	count := snapshot.EndFrame - snapshot.StartFrame + 1
	if len(frames) != count {
		return fmt.Errorf("frame count %d does not match snapshot window %d", len(frames), count)
	}
	for _, layer := range snapshot.Layers {
		if len(layer.FrameStates) != count {
			return fmt.Errorf("layer %s frame state count mismatch", layer.ID)
		}
	}
	for j, frame := range frames {
		if frame.Index != snapshot.StartFrame+j {
			return fmt.Errorf("frame order mismatch at %d", j)
		}
		if frame.Width != snapshot.Width || frame.Height != snapshot.Height || frame.Channels != snapshot.Channels {
			return fmt.Errorf("frame %d dimensions mismatch", frame.Index)
		}
		if len(frame.RGBA) != frame.Width*frame.Height*frame.Channels {
			return fmt.Errorf("frame %d pixel length mismatch", frame.Index)
		}
		if frame.TimeNS != snapshot.FrameDurationNS*int64(frame.Index) {
			return fmt.Errorf("frame %d timestamp mismatch", frame.Index)
		}
		if frame.Hash != compose.HashFrame(frame) {
			return fmt.Errorf("frame %d hash mismatch", frame.Index)
		}
		if frame.Stats.Frame != frame.Index || frame.Stats.TimeNS != frame.TimeNS || frame.Stats.ContentHash != frame.Hash {
			return fmt.Errorf("frame %d stats metadata mismatch", frame.Index)
		}
		var sum [4]float64
		var minLuma, maxLuma uint8
		for p := 0; p < frame.Width*frame.Height; p++ {
			off := p * 4
			luma := uint8(math.Round(0.2126*float64(frame.RGBA[off]) + 0.7152*float64(frame.RGBA[off+1]) + 0.0722*float64(frame.RGBA[off+2])))
			if p == 0 || luma < minLuma {
				minLuma = luma
			}
			if p == 0 || luma > maxLuma {
				maxLuma = luma
			}
			for c := 0; c < 4; c++ {
				sum[c] += float64(frame.RGBA[off+c])
			}
		}
		pixels := float64(frame.Width * frame.Height)
		for c := 0; c < 4; c++ {
			if frame.Stats.MeanRGBA[c] != sum[c]/pixels {
				return fmt.Errorf("frame %d mean statistics mismatch", frame.Index)
			}
		}
		if frame.Stats.MinLuma != minLuma || frame.Stats.MaxLuma != maxLuma {
			return fmt.Errorf("frame %d luma statistics mismatch", frame.Index)
		}
		expectedVisible := make([]string, 0, len(snapshot.Layers))
		for _, layer := range snapshot.Layers {
			state := layer.FrameStates[j]
			if state.Visible && state.Opacity > 0 {
				expectedVisible = append(expectedVisible, layer.ID)
			}
		}
		if !slices.Equal(frame.Stats.VisibleLayers, expectedVisible) {
			return fmt.Errorf("frame %d visible layer statistics mismatch", frame.Index)
		}
		for _, sample := range frame.Stats.Samples {
			if sample.X < 0 || sample.X >= frame.Width || sample.Y < 0 || sample.Y >= frame.Height {
				return fmt.Errorf("frame %d sample out of bounds", frame.Index)
			}
			off := (sample.Y*frame.Width + sample.X) * 4
			if !slices.Equal(sample.RGBA[:], frame.RGBA[off:off+4]) {
				return fmt.Errorf("frame %d sample pixel mismatch", frame.Index)
			}
		}
	}
	return nil
}

func (s *Store) invalidateLocked(ctx context.Context, projectID, reason string) error {
	if err := s.purgeLocked(ctx, projectID); err != nil {
		return fmt.Errorf("%w (purge failed: %v)", &InvalidatedError{Reason: reason}, err)
	}
	return &InvalidatedError{Reason: reason}
}

func (s *Store) Invalidate(ctx context.Context, projectID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = reason
	return s.purgeLocked(ctx, projectID)
}

func (s *Store) DeleteWindows(ctx context.Context, projectID string, windowIndexes []int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.WrapError(model.CodeIO, "checkpoints", "begin transaction", err)
	}
	defer tx.Rollback()
	for _, index := range windowIndexes {
		if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoints WHERE project_id = ? AND window_index = ?`, projectID, index); err != nil {
			return model.WrapError(model.CodeIO, "checkpoints", "delete invalid window", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET complete = 0 WHERE id = ?`, projectID); err != nil {
		return model.WrapError(model.CodeIO, "projects", "mark project incomplete", err)
	}
	if err := tx.Commit(); err != nil {
		return model.WrapError(model.CodeIO, "checkpoints", "commit invalidation", err)
	}
	return nil
}

func (s *Store) SetComplete(ctx context.Context, projectID string, complete bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `UPDATE projects SET complete = ? WHERE id = ?`, boolInt(complete), projectID); err != nil {
		return model.WrapError(model.CodeIO, "projects", "set completeness", err)
	}
	return nil
}

func (s *Store) Purge(ctx context.Context, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.purgeLocked(ctx, projectID)
}

func (s *Store) purgeLocked(ctx context.Context, projectID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.WrapError(model.CodeIO, "projects", "begin purge", err)
	}
	defer tx.Rollback()
	for _, table := range []string{"checkpoints", "exports"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE project_id = ?`, projectID); err != nil {
			return model.WrapError(model.CodeIO, table, "purge snapshot", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, projectID); err != nil {
		return model.WrapError(model.CodeIO, "projects", "purge project", err)
	}
	if err := tx.Commit(); err != nil {
		return model.WrapError(model.CodeIO, "projects", "commit purge", err)
	}
	return nil
}

func (s *Store) SetExportStatus(ctx context.Context, projectID, name string, valid bool, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO exports (project_id, export_name, valid, export_version) VALUES (?, ?, ?, ?)
		ON CONFLICT(project_id, export_name) DO UPDATE SET valid=excluded.valid, export_version=excluded.export_version`,
		projectID, name, boolInt(valid), version)
	if err != nil {
		return model.WrapError(model.CodeIO, "exports", "set export status", err)
	}
	return nil
}

func (s *Store) ExportStatus(ctx context.Context, projectID string) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT export_name, valid FROM exports WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, model.WrapError(model.CodeIO, "exports", "query export status", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		var valid int
		if err := rows.Scan(&name, &valid); err != nil {
			return nil, model.WrapError(model.CodeIO, "exports", "scan export status", err)
		}
		out[name] = valid != 0
	}
	return out, rows.Err()
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
