package checkpoint

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"storyworkbench/engine"
	"storyworkbench/story"
)

var ErrNoCheckpoints = errors.New("no checkpoints")

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

type Summary struct {
	ProjectID        string `json:"project_id"`
	ContentHash      string `json:"content_hash"`
	SchemaVersion    int    `json:"schema_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	LastShotSeq      int    `json:"last_shot_seq"`
	ShotCount        int    `json:"shot_count"`
	UpdatedAt        string `json:"updated_at"`
}

type checkpointRow struct {
	ShotSeq          int
	NodeID           string
	NodeShotIndex    int
	ChoiceOffset     int
	ContentHash      string
	SchemaVersion    int
	AlgorithmVersion string
	ShotJSON         string
	SaveJSON         string
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, story.NewError(story.CodeSave, "", "database", "checkpoint database path is required")
	}
	if path != ":memory:" {
		dir := filepath.Dir(path)
		if dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, story.NewError(story.CodeSave, "", "database", err.Error())
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, story.NewError(story.CodeSave, "", "database", err.Error())
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: path}
	if err := store.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *Store) initialize() error {
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = FULL`,
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			project_id TEXT PRIMARY KEY,
			content_hash TEXT NOT NULL,
			save_schema_version INTEGER NOT NULL,
			algorithm_version TEXT NOT NULL,
			last_shot_seq INTEGER NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS shot_checkpoints (
			project_id TEXT NOT NULL,
			shot_seq INTEGER NOT NULL,
			node_id TEXT NOT NULL,
			node_shot_index INTEGER NOT NULL,
			choice_offset INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			save_schema_version INTEGER NOT NULL,
			algorithm_version TEXT NOT NULL,
			shot_json TEXT NOT NULL,
			save_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (project_id, shot_seq),
			FOREIGN KEY (project_id) REFERENCES checkpoint_meta(project_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_shot_checkpoints_project_seq
			ON shot_checkpoints(project_id, shot_seq)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return story.NewError(story.CodeSave, "", "database", err.Error())
		}
	}
	return nil
}

// CommitBoundary serializes a shot-boundary save into SQLite. The store is
// deliberately single-writer: duplicate commits are idempotent, jumps are
// rejected, and every row is written in the same transaction as the meta row.
func (s *Store) CommitBoundary(project *story.Project, save engine.Save) error {
	if err := engine.ValidateSave(project, &save, project.Choices); err != nil {
		return err
	}
	if save.State.ShotProgress == 0 {
		return story.NewError(story.CodeSave, "", "state.shot_progress", "checkpoint requires at least one presented shot")
	}
	shotSeq := save.State.ShotProgress - 1
	shot := save.State.Shots[shotSeq]
	shotJSON, err := json.Marshal(shot)
	if err != nil {
		return story.NewError(story.CodeSave, shot.NodeID, "shot_json", err.Error())
	}
	saveJSON, err := json.Marshal(save)
	if err != nil {
		return story.NewError(story.CodeSave, shot.NodeID, "save_json", err.Error())
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return story.NewError(story.CodeSave, "", "database", "checkpoint store is closed")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return story.NewError(story.CodeSave, shot.NodeID, "database", err.Error())
	}
	defer func() { _ = tx.Rollback() }()

	var (
		contentHash   string
		schemaVersion int
		algorithm     string
		lastSeq       int
	)
	err = tx.QueryRow(`SELECT content_hash, save_schema_version, algorithm_version, last_shot_seq
		FROM checkpoint_meta WHERE project_id = ?`, project.ID).
		Scan(&contentHash, &schemaVersion, &algorithm, &lastSeq)
	if errors.Is(err, sql.ErrNoRows) {
		if shotSeq != 0 {
			return story.NewError(story.CodeSave, shot.NodeID, "shot_seq",
				fmt.Sprintf("first checkpoint must be shot 0, got %d", shotSeq))
		}
		if _, err := tx.Exec(`INSERT INTO checkpoint_meta
			(project_id, content_hash, save_schema_version, algorithm_version, last_shot_seq, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			project.ID, save.ContentHash, save.SchemaVersion, save.AlgorithmVersion, shotSeq, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return story.NewError(story.CodeSave, shot.NodeID, "database", err.Error())
		}
	} else if err != nil {
		return story.NewError(story.CodeSave, shot.NodeID, "database", err.Error())
	} else {
		if contentHash != save.ContentHash {
			_ = purgeTx(tx, project.ID)
			if commitErr := tx.Commit(); commitErr != nil {
				return story.NewError(story.CodeSave, shot.NodeID, "database", commitErr.Error())
			}
			return story.NewError(story.CodeSave, shot.NodeID, "content_hash", "existing checkpoints belong to different project content and were discarded")
		}
		if schemaVersion != save.SchemaVersion {
			_ = purgeTx(tx, project.ID)
			if commitErr := tx.Commit(); commitErr != nil {
				return story.NewError(story.CodeSave, shot.NodeID, "database", commitErr.Error())
			}
			return story.NewError(story.CodeSave, shot.NodeID, "schema_version", "existing checkpoint structure version is incompatible and was discarded")
		}
		if algorithm != save.AlgorithmVersion {
			_ = purgeTx(tx, project.ID)
			if commitErr := tx.Commit(); commitErr != nil {
				return story.NewError(story.CodeSave, shot.NodeID, "database", commitErr.Error())
			}
			return story.NewError(story.CodeSave, shot.NodeID, "algorithm_version", "existing checkpoint algorithm version is incompatible and was discarded")
		}
		if shotSeq <= lastSeq {
			var existingJSON string
			if err := tx.QueryRow(`SELECT save_json FROM shot_checkpoints WHERE project_id = ? AND shot_seq = ?`,
				project.ID, shotSeq).Scan(&existingJSON); err != nil {
				return story.NewError(story.CodeSave, shot.NodeID, "shot_seq", "existing duplicate checkpoint is unreadable")
			}
			var existing engine.Save
			if err := json.Unmarshal([]byte(existingJSON), &existing); err != nil {
				return story.NewError(story.CodeSave, shot.NodeID, "shot_seq", "existing duplicate checkpoint is invalid")
			}
			if existing.Fingerprint != save.Fingerprint {
				return story.NewError(story.CodeSave, shot.NodeID, "shot_seq",
					fmt.Sprintf("shot %d was already committed with different content", shotSeq))
			}
			return nil
		}
		if shotSeq != lastSeq+1 {
			return story.NewError(story.CodeSave, shot.NodeID, "shot_seq",
				fmt.Sprintf("checkpoint shots must be contiguous; expected %d, got %d", lastSeq+1, shotSeq))
		}
	}
	if _, err := tx.Exec(`INSERT INTO shot_checkpoints
		(project_id, shot_seq, node_id, node_shot_index, choice_offset, content_hash,
		 save_schema_version, algorithm_version, shot_json, save_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		project.ID, shotSeq, shot.NodeID, shot.NodeShotIndex, save.State.ChoiceOffset,
		save.ContentHash, save.SchemaVersion, save.AlgorithmVersion, string(shotJSON), string(saveJSON),
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return story.NewError(story.CodeSave, shot.NodeID, "database", err.Error())
	}
	if _, err := tx.Exec(`UPDATE checkpoint_meta SET last_shot_seq = ?, updated_at = ? WHERE project_id = ?`,
		shotSeq, time.Now().UTC().Format(time.RFC3339Nano), project.ID); err != nil {
		return story.NewError(story.CodeSave, shot.NodeID, "database", err.Error())
	}
	if err := tx.Commit(); err != nil {
		return story.NewError(story.CodeSave, shot.NodeID, "database", err.Error())
	}
	return nil
}

func (s *Store) LoadLatest(project *story.Project) (*engine.Save, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, story.NewError(story.CodeSave, "", "database", "checkpoint store is closed")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, story.NewError(story.CodeSave, "", "database", err.Error())
	}
	defer func() { _ = tx.Rollback() }()

	var contentHash string
	var schemaVersion int
	var algorithm string
	var lastSeq int
	err = tx.QueryRow(`SELECT content_hash, save_schema_version, algorithm_version, last_shot_seq
		FROM checkpoint_meta WHERE project_id = ?`, project.ID).
		Scan(&contentHash, &schemaVersion, &algorithm, &lastSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoCheckpoints
	}
	if err != nil {
		return nil, story.NewError(story.CodeSave, "", "database", err.Error())
	}
	expectedHash := story.ContentHash(project)
	if contentHash != expectedHash {
		return nil, s.discardAndFail(tx, project.ID, "", "content_hash", "checkpoint content hash does not match project; all checkpoints discarded")
	}
	if schemaVersion != engine.SaveSchemaVersion {
		return nil, s.discardAndFail(tx, project.ID, "", "schema_version", "checkpoint structure version mismatch; all checkpoints discarded")
	}
	if algorithm != engine.AlgorithmVersion {
		return nil, s.discardAndFail(tx, project.ID, "", "algorithm_version", "checkpoint algorithm version mismatch; all checkpoints discarded")
	}
	rows, err := tx.Query(`SELECT shot_seq, node_id, node_shot_index, choice_offset, content_hash,
		save_schema_version, algorithm_version, shot_json, save_json
		FROM shot_checkpoints WHERE project_id = ? ORDER BY shot_seq ASC`, project.ID)
	if err != nil {
		return nil, story.NewError(story.CodeSave, "", "database", err.Error())
	}
	var checkpoints []checkpointRow
	for rows.Next() {
		var row checkpointRow
		if err := rows.Scan(&row.ShotSeq, &row.NodeID, &row.NodeShotIndex, &row.ChoiceOffset,
			&row.ContentHash, &row.SchemaVersion, &row.AlgorithmVersion, &row.ShotJSON, &row.SaveJSON); err != nil {
			_ = rows.Close()
			return nil, s.discardAndFail(tx, project.ID, row.NodeID, "database", "unreadable checkpoint row; all checkpoints discarded")
		}
		checkpoints = append(checkpoints, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, s.discardAndFail(tx, project.ID, "", "database", "checkpoint query failed; all checkpoints discarded")
	}
	_ = rows.Close()
	if len(checkpoints) != lastSeq+1 {
		return nil, s.discardAndFail(tx, project.ID, "", "shot_seq", "checkpoint rows are not contiguous; all checkpoints discarded")
	}
	var latest *engine.Save
	for i, row := range checkpoints {
		if row.ShotSeq != i {
			return nil, s.discardAndFail(tx, project.ID, row.NodeID, "shot_seq", "checkpoint sequence gap; all checkpoints discarded")
		}
		if row.ContentHash != expectedHash {
			return nil, s.discardAndFail(tx, project.ID, row.NodeID, "content_hash", "checkpoint row content hash mismatch; all checkpoints discarded")
		}
		if row.SchemaVersion != engine.SaveSchemaVersion {
			return nil, s.discardAndFail(tx, project.ID, row.NodeID, "schema_version", "checkpoint row structure mismatch; all checkpoints discarded")
		}
		if row.AlgorithmVersion != engine.AlgorithmVersion {
			return nil, s.discardAndFail(tx, project.ID, row.NodeID, "algorithm_version", "checkpoint row algorithm mismatch; all checkpoints discarded")
		}
		save, err := engine.ParseSave([]byte(row.SaveJSON))
		if err != nil {
			return nil, s.discardAndFail(tx, project.ID, row.NodeID, saveErrorField(err), "checkpoint save payload is invalid; all checkpoints discarded")
		}
		if err := engine.ValidateSave(project, save, project.Choices); err != nil {
			return nil, s.discardAndFail(tx, project.ID, row.NodeID, saveErrorField(err), "checkpoint save does not match project; all checkpoints discarded")
		}
		if save.State.ShotProgress != row.ShotSeq+1 {
			return nil, s.discardAndFail(tx, project.ID, row.NodeID, "state.shot_progress", "checkpoint progress does not match sequence; all checkpoints discarded")
		}
		savedShot := save.State.Shots[row.ShotSeq]
		if row.NodeID != savedShot.NodeID {
			return nil, s.discardAndFail(tx, project.ID, row.NodeID, "node_id", "checkpoint node column disagrees with save payload; all checkpoints discarded")
		}
		if row.NodeShotIndex != savedShot.NodeShotIndex {
			return nil, s.discardAndFail(tx, project.ID, row.NodeID, "node_shot_index", "checkpoint shot index disagrees with save payload; all checkpoints discarded")
		}
		if row.ChoiceOffset != save.State.ChoiceOffset {
			return nil, s.discardAndFail(tx, project.ID, row.NodeID, "choice_offset", "checkpoint choice offset disagrees with save payload; all checkpoints discarded")
		}
		var storedShot engine.ShotPresentation
		if err := json.Unmarshal([]byte(row.ShotJSON), &storedShot); err != nil {
			return nil, s.discardAndFail(tx, project.ID, row.NodeID, "shot_json", "checkpoint shot payload is invalid; all checkpoints discarded")
		}
		expectedShot := save.State.Shots[row.ShotSeq]
		if !reflect.DeepEqual(storedShot, expectedShot) {
			return nil, s.discardAndFail(tx, project.ID, row.NodeID, "shot_json", "checkpoint shot payload disagrees with save; all checkpoints discarded")
		}
		latest = save
	}
	if latest == nil {
		return nil, ErrNoCheckpoints
	}
	if err := tx.Commit(); err != nil {
		return nil, story.NewError(story.CodeSave, "", "database", err.Error())
	}
	return latest, nil
}

func (s *Store) discardAndFail(tx *sql.Tx, projectID, nodeID, field, message string) error {
	_ = purgeTx(tx, projectID)
	if err := tx.Commit(); err != nil {
		return story.NewError(story.CodeSave, nodeID, "database", err.Error())
	}
	return story.NewError(story.CodeSave, nodeID, field, message)
}

func purgeTx(tx *sql.Tx, projectID string) error {
	if _, err := tx.Exec(`DELETE FROM shot_checkpoints WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM checkpoint_meta WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	return nil
}

func (s *Store) Reset(projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return story.NewError(story.CodeSave, "", "database", "checkpoint store is closed")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return story.NewError(story.CodeSave, "", "database", err.Error())
	}
	defer func() { _ = tx.Rollback() }()
	if err := purgeTx(tx, projectID); err != nil {
		return story.NewError(story.CodeSave, "", "database", err.Error())
	}
	if err := tx.Commit(); err != nil {
		return story.NewError(story.CodeSave, "", "database", err.Error())
	}
	return nil
}

func (s *Store) Summary(projectID string) (*Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, story.NewError(story.CodeSave, "", "database", "checkpoint store is closed")
	}
	var summary Summary
	err := s.db.QueryRow(`SELECT project_id, content_hash, save_schema_version, algorithm_version,
		last_shot_seq, updated_at FROM checkpoint_meta WHERE project_id = ?`, projectID).
		Scan(&summary.ProjectID, &summary.ContentHash, &summary.SchemaVersion, &summary.AlgorithmVersion,
			&summary.LastShotSeq, &summary.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoCheckpoints
	}
	if err != nil {
		return nil, story.NewError(story.CodeSave, "", "database", err.Error())
	}
	summary.ShotCount = summary.LastShotSeq + 1
	return &summary, nil
}

func (s *Store) AllSummaries() ([]Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, story.NewError(story.CodeSave, "", "database", "checkpoint store is closed")
	}
	rows, err := s.db.Query(`SELECT project_id, content_hash, save_schema_version, algorithm_version,
		last_shot_seq, updated_at FROM checkpoint_meta ORDER BY project_id`)
	if err != nil {
		return nil, story.NewError(story.CodeSave, "", "database", err.Error())
	}
	defer rows.Close()
	var result []Summary
	for rows.Next() {
		var summary Summary
		if err := rows.Scan(&summary.ProjectID, &summary.ContentHash, &summary.SchemaVersion,
			&summary.AlgorithmVersion, &summary.LastShotSeq, &summary.UpdatedAt); err != nil {
			return nil, story.NewError(story.CodeSave, "", "database", err.Error())
		}
		summary.ShotCount = summary.LastShotSeq + 1
		result = append(result, summary)
	}
	return result, rows.Err()
}

func (s *Store) Path() string { return s.path }

func saveErrorField(err error) string {
	if appErr, ok := story.AsError(err); ok && appErr.Field != "" {
		return appErr.Field
	}
	return "save"
}
