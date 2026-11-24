package pipeline

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

type CheckpointStore struct {
	mu   sync.Mutex
	db   *sql.DB
	path string
}

type RecoveryResult struct {
	Restored []WindowSnapshot `json:"restored"`
	Purged   bool             `json:"purged"`
	Reason   string           `json:"reason,omitempty"`
	Checked  int              `json:"checked"`
}

type RecoveryExpectation struct {
	AlgorithmVersion string
	SourceHash       string
	Windows          []Window
	FrameStore       FrameStore
}

func OpenCheckpoint(path string) (*CheckpointStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, paraError(-1, "checkpoint path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, errCode(CodeDecode, "create checkpoint directory: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, errCode(CodeDecode, "open sqlite checkpoint: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &CheckpointStore{db: db, path: path}
	if err := store.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *CheckpointStore) initialize() error {
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	}
	for _, pragma := range pragmas {
		if _, err := s.db.Exec(pragma); err != nil {
			return errCode(CodeDecode, "sqlite %s: %v", pragma, err)
		}
	}
	schema := `
CREATE TABLE IF NOT EXISTS checkpoint_meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  algorithm_version TEXT NOT NULL,
  source_hash TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS window_snapshots (
  window_index INTEGER PRIMARY KEY,
  start_frame INTEGER NOT NULL,
  end_frame INTEGER NOT NULL,
  frame_hash TEXT NOT NULL,
  summary_json TEXT NOT NULL,
  frame_summaries_json TEXT NOT NULL,
  layers_json TEXT NOT NULL,
  source_json TEXT NOT NULL,
  model_hash TEXT NOT NULL,
  source_hash TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  frame_path TEXT NOT NULL,
  committed_sequence INTEGER NOT NULL,
  committed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_window_snapshots_sequence ON window_snapshots(committed_sequence);
`
	if _, err := s.db.Exec(schema); err != nil {
		return errCode(CodeDecode, "create checkpoint schema: %v", err)
	}
	return nil
}

func (s *CheckpointStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// SaveSnapshot commits exactly one window in one transaction. The mutex and a
// single SQLite connection serialize all checkpoint writes.
func (s *CheckpointStore) SaveSnapshot(snapshot WindowSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return errCode(CodeDecode, "checkpoint store is closed")
	}
	if snapshot.AlgorithmVersion == "" || snapshot.SourceHash == "" || snapshot.FrameHash == "" {
		return paraError(snapshot.WindowIndex, "snapshot is missing required checkpoint fields")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return errCode(CodeDecode, "begin checkpoint transaction: %v", err)
	}
	defer tx.Rollback()

	var alg, sourceHash string
	err = tx.QueryRow(`SELECT algorithm_version, source_hash FROM checkpoint_meta WHERE id = 1`).Scan(&alg, &sourceHash)
	switch {
	case err == sql.ErrNoRows:
		if _, err := tx.Exec(`INSERT INTO checkpoint_meta(id, algorithm_version, source_hash) VALUES(1,?,?)`, snapshot.AlgorithmVersion, snapshot.SourceHash); err != nil {
			return errCode(CodeDecode, "insert checkpoint metadata: %v", err)
		}
	case err != nil:
		return errCode(CodeDecode, "read checkpoint metadata: %v", err)
	default:
		if alg != snapshot.AlgorithmVersion || sourceHash != snapshot.SourceHash {
			return errCode(CodeVersion, "checkpoint metadata mismatch: algorithm %q/%q source %q/%q", alg, snapshot.AlgorithmVersion, sourceHash, snapshot.SourceHash)
		}
	}

	var nextSeq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(committed_sequence),0)+1 FROM window_snapshots`).Scan(&nextSeq); err != nil {
		return errCode(CodeDecode, "allocate checkpoint sequence: %v", err)
	}
	if snapshot.CommittedSequence <= 0 {
		snapshot.CommittedSequence = nextSeq
	}
	summaryJSON, err := json.Marshal(snapshot.Summary)
	if err != nil {
		return errCode(CodeDecode, "encode frame summary: %v", err)
	}
	frameSummariesJSON, err := json.Marshal(snapshot.FrameSummaries)
	if err != nil {
		return errCode(CodeDecode, "encode per-frame summaries: %v", err)
	}
	layersJSON, err := json.Marshal(snapshot.Layers)
	if err != nil {
		return errCode(CodeDecode, "encode layer snapshot: %v", err)
	}
	sourceJSON, err := json.Marshal(snapshot.Source)
	if err != nil {
		return errCode(CodeDecode, "encode source snapshot: %v", err)
	}
	_, err = tx.Exec(`
INSERT INTO window_snapshots(
  window_index,start_frame,end_frame,frame_hash,summary_json,frame_summaries_json,layers_json,source_json,
  model_hash,source_hash,algorithm_version,frame_path,committed_sequence,committed_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(window_index) DO UPDATE SET
  start_frame=excluded.start_frame,end_frame=excluded.end_frame,frame_hash=excluded.frame_hash,
  summary_json=excluded.summary_json,frame_summaries_json=excluded.frame_summaries_json,
  layers_json=excluded.layers_json,source_json=excluded.source_json,
  model_hash=excluded.model_hash,source_hash=excluded.source_hash,algorithm_version=excluded.algorithm_version,
  frame_path=excluded.frame_path,committed_sequence=excluded.committed_sequence,committed_at=excluded.committed_at`,
		snapshot.WindowIndex, snapshot.StartFrame, snapshot.EndFrame, snapshot.FrameHash, string(summaryJSON), string(frameSummariesJSON),
		string(layersJSON), string(sourceJSON), snapshot.ModelHash, snapshot.SourceHash,
		snapshot.AlgorithmVersion, snapshot.FramePath, snapshot.CommittedSequence, snapshot.CommittedAt)
	if err != nil {
		return errCode(CodeDecode, "write window checkpoint: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return errCode(CodeDecode, "commit checkpoint transaction: %v", err)
	}
	return nil
}

func (s *CheckpointStore) DeleteSnapshots(indices []int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return errCode(CodeDecode, "checkpoint store is closed")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return errCode(CodeDecode, "begin checkpoint delete: %v", err)
	}
	defer tx.Rollback()
	for _, index := range indices {
		if _, err := tx.Exec(`DELETE FROM window_snapshots WHERE window_index=?`, index); err != nil {
			return errCode(CodeDecode, "delete window checkpoint %d: %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return errCode(CodeDecode, "commit checkpoint delete: %v", err)
	}
	return nil
}

func (s *CheckpointStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return errCode(CodeDecode, "checkpoint store is closed")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return errCode(CodeDecode, "begin checkpoint purge: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM window_snapshots`); err != nil {
		return errCode(CodeDecode, "purge window snapshots: %v", err)
	}
	if _, err := tx.Exec(`DELETE FROM checkpoint_meta`); err != nil {
		return errCode(CodeDecode, "purge checkpoint metadata: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return errCode(CodeDecode, "commit checkpoint purge: %v", err)
	}
	return nil
}

func (s *CheckpointStore) LoadVerified(exp RecoveryExpectation) (RecoveryResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return RecoveryResult{}, errCode(CodeDecode, "checkpoint store is closed")
	}
	rows, err := s.db.Query(`SELECT window_index,start_frame,end_frame,frame_hash,summary_json,frame_summaries_json,layers_json,source_json,
model_hash,source_hash,algorithm_version,frame_path,committed_sequence,committed_at
FROM window_snapshots ORDER BY window_index ASC`)
	if err != nil {
		return RecoveryResult{}, errCode(CodeDecode, "query checkpoints: %v", err)
	}
	var snapshots []WindowSnapshot
	for rows.Next() {
		var snap WindowSnapshot
		var summaryJSON, frameSummariesJSON, layersJSON, sourceJSON string
		if err := rows.Scan(&snap.WindowIndex, &snap.StartFrame, &snap.EndFrame, &snap.FrameHash,
			&summaryJSON, &frameSummariesJSON, &layersJSON, &sourceJSON, &snap.ModelHash, &snap.SourceHash,
			&snap.AlgorithmVersion, &snap.FramePath, &snap.CommittedSequence, &snap.CommittedAt); err != nil {
			rows.Close()
			return RecoveryResult{}, errCode(CodeDecode, "scan checkpoint: %v", err)
		}
		if err := json.Unmarshal([]byte(summaryJSON), &snap.Summary); err != nil {
			rows.Close()
			return RecoveryResult{}, errCode(CodeDecode, "decode checkpoint summary: %v", err)
		}
		if err := json.Unmarshal([]byte(frameSummariesJSON), &snap.FrameSummaries); err != nil {
			rows.Close()
			return RecoveryResult{}, errCode(CodeDecode, "decode per-frame summaries: %v", err)
		}
		if err := json.Unmarshal([]byte(layersJSON), &snap.Layers); err != nil {
			rows.Close()
			return RecoveryResult{}, errCode(CodeDecode, "decode checkpoint layers: %v", err)
		}
		if err := json.Unmarshal([]byte(sourceJSON), &snap.Source); err != nil {
			rows.Close()
			return RecoveryResult{}, errCode(CodeDecode, "decode checkpoint source: %v", err)
		}
		snapshots = append(snapshots, snap)
	}
	if err := rows.Close(); err != nil {
		return RecoveryResult{}, errCode(CodeDecode, "close checkpoint rows: %v", err)
	}
	if err := rows.Err(); err != nil {
		return RecoveryResult{}, errCode(CodeDecode, "read checkpoints: %v", err)
	}
	result := RecoveryResult{Checked: len(snapshots)}
	if len(snapshots) == 0 {
		return result, nil
	}
	var metaAlg, metaSource string
	if err := s.db.QueryRow(`SELECT algorithm_version, source_hash FROM checkpoint_meta WHERE id=1`).Scan(&metaAlg, &metaSource); err != nil {
		if err := s.clearLocked(); err != nil {
			return RecoveryResult{}, err
		}
		result.Purged = true
		result.Reason = "checkpoint metadata is missing or unreadable"
		return result, nil
	}
	if metaAlg != exp.AlgorithmVersion || metaSource != exp.SourceHash {
		if err := s.clearLocked(); err != nil {
			return RecoveryResult{}, err
		}
		result.Purged = true
		result.Reason = "checkpoint metadata algorithm/source version mismatch"
		return result, nil
	}
	reason := verifySnapshots(snapshots, exp)
	if reason != "" {
		if err := s.clearLocked(); err != nil {
			return RecoveryResult{}, err
		}
		result.Purged = true
		result.Reason = reason
		return result, nil
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].WindowIndex == snapshots[j].WindowIndex {
			return snapshots[i].CommittedSequence < snapshots[j].CommittedSequence
		}
		return snapshots[i].WindowIndex < snapshots[j].WindowIndex
	})
	result.Restored = snapshots
	return result, nil
}

func verifySnapshots(snapshots []WindowSnapshot, exp RecoveryExpectation) string {
	byIndex := map[int]Window{}
	for _, window := range exp.Windows {
		byIndex[window.Index] = window
	}
	lastSeq := int64(0)
	lastIndex := -1
	for _, snapshot := range snapshots {
		if snapshot.WindowIndex <= lastIndex {
			return fmt.Sprintf("window order is not strictly ascending at %d", snapshot.WindowIndex)
		}
		lastIndex = snapshot.WindowIndex
		window, ok := byIndex[snapshot.WindowIndex]
		if !ok || window.StartFrame != snapshot.StartFrame || window.EndFrame != snapshot.EndFrame {
			return fmt.Sprintf("window %d bounds do not match the current project", snapshot.WindowIndex)
		}
		if snapshot.AlgorithmVersion != exp.AlgorithmVersion {
			return fmt.Sprintf("window %d algorithm version %q does not match %q", snapshot.WindowIndex, snapshot.AlgorithmVersion, exp.AlgorithmVersion)
		}
		if snapshot.SourceHash != exp.SourceHash {
			return fmt.Sprintf("window %d source version hash does not match the current project", snapshot.WindowIndex)
		}
		if snapshot.CommittedSequence <= lastSeq {
			return fmt.Sprintf("window %d commit sequence %d is not strictly ordered", snapshot.WindowIndex, snapshot.CommittedSequence)
		}
		lastSeq = snapshot.CommittedSequence
		raw, err := os.ReadFile(exp.FrameStore.WindowPath(snapshot.WindowIndex))
		if err != nil {
			return fmt.Sprintf("window %d cached frame is missing: %v", snapshot.WindowIndex, err)
		}
		fileSum := sha256.Sum256(raw)
		if hex.EncodeToString(fileSum[:]) != snapshot.FrameHash {
			return fmt.Sprintf("window %d persisted file hash mismatch", snapshot.WindowIndex)
		}
		packet, err := exp.FrameStore.ReadWindow(snapshot.WindowIndex)
		if err != nil {
			return fmt.Sprintf("window %d cached frame is missing or corrupt: %v", snapshot.WindowIndex, err)
		}
		if packet.ContentHash != snapshot.Summary.ContentHash {
			return fmt.Sprintf("window %d content hash mismatch", snapshot.WindowIndex)
		}
	}
	return ""
}

func (s *CheckpointStore) clearLocked() error {
	tx, err := s.db.Begin()
	if err != nil {
		return errCode(CodeDecode, "begin checkpoint purge: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM window_snapshots`); err != nil {
		return errCode(CodeDecode, "purge window snapshots: %v", err)
	}
	if _, err := tx.Exec(`DELETE FROM checkpoint_meta`); err != nil {
		return errCode(CodeDecode, "purge checkpoint metadata: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return errCode(CodeDecode, "commit checkpoint purge: %v", err)
	}
	return nil
}

// SourceFingerprint identifies immutable source declarations, not tunable
// layer values. It changes for version, canvas or time-base changes, forcing a
// whole-cache invalidation.
func SourceFingerprint(model *Model) (string, error) {
	if model == nil {
		return "", paraError(-1, "model is nil")
	}
	type sourceIdentity struct {
		Versions   []int       `json:"versions"`
		Canvas     Canvas      `json:"canvas"`
		TimeBase   TimeBase    `json:"time_base"`
		SourceRefs []SourceRef `json:"source_refs"`
	}
	versions := append([]int(nil), model.SourceVersions...)
	sort.Ints(versions)
	refs := append([]SourceRef(nil), model.Sources...)
	for i := range refs {
		refs[i].Path = ""
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Version != refs[j].Version {
			return refs[i].Version < refs[j].Version
		}
		return refs[i].Format < refs[j].Format
	})
	raw, err := json.Marshal(sourceIdentity{
		Versions: versions, Canvas: model.Canvas, TimeBase: model.TimeBase, SourceRefs: refs,
	})
	if err != nil {
		return "", errCode(CodeDecode, "encode source fingerprint: %v", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
