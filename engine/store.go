package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const snapshotSchemaVersion = "snapshot-v1"

type SnapshotStore struct {
	db       *sql.DB
	scene    *Scene
	sceneKey string
	mu       sync.Mutex
}

type SnapshotStatus struct {
	Valid       bool    `json:"valid"`
	FrameCount  int     `json:"frameCount"`
	ResumedFrom int     `json:"resumedFrom,omitempty"`
	Invalidated bool    `json:"invalidated"`
	Reason      string  `json:"reason,omitempty"`
	LoadedTimes []int64 `json:"loadedTimes,omitempty"`
	CommitSeq   []int   `json:"commitSequence,omitempty"`
}

type snapshotPayload struct {
	Frame RenderedFrame `json:"frame"`
	RGBA  []byte        `json:"rgba"`
	Blend []BlendMode   `json:"blend"`
}

func OpenSnapshotStore(path string, scene *Scene) (*SnapshotStore, error) {
	if scene == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "nil scene"}
	}
	if path != ":memory:" {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create snapshot directory: %w", err)
		}
		f, err := os.OpenFile(filepath.Join(dir, ".write-test"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return nil, fmt.Errorf("persistent directory is not writable: %w", err)
		}
		_ = f.Close()
		_ = os.Remove(filepath.Join(dir, ".write-test"))
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=10000; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;"); err != nil {
		_ = db.Close()
		return nil, err
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS frames (
			scene_id TEXT NOT NULL,
			time_us INTEGER NOT NULL,
			frame_index INTEGER NOT NULL,
			commit_seq INTEGER NOT NULL,
			payload TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			interp_precision INTEGER NOT NULL,
			tolerance INTEGER NOT NULL,
			params_version TEXT NOT NULL,
			blend_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			spec_version TEXT NOT NULL,
			PRIMARY KEY(scene_id, time_us)
		)`,
		`CREATE INDEX IF NOT EXISTS frames_commit_order ON frames(scene_id, commit_seq)`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	key, err := sceneFingerprint(scene)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SnapshotStore{db: db, scene: scene.Clone(), sceneKey: key}, nil
}

func sceneFingerprint(scene *Scene) (string, error) {
	b, err := json.Marshal(scene)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (s *SnapshotStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *SnapshotStore) SceneKey() string { return s.sceneKey }

func (s *SnapshotStore) Reset(sceneID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM frames WHERE scene_id = ?`, sceneID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM meta WHERE key LIKE ?`, "scene/"+sceneID+"/%"); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// SaveFrame commits exactly one time slice in a single SQLite transaction.
func (s *SnapshotStore) SaveFrame(frame *RenderedFrame) error {
	if frame == nil {
		return &Error{Code: CodeState, Path: "frame", Message: "nil frame"}
	}
	if frame.ContentHash == "" {
		return &Error{Code: CodeState, Path: "frame.contentHash", Message: "missing content hash"}
	}
	payload := snapshotPayload{
		Frame: *frame,
		RGBA:  append([]byte(nil), frame.RGBA...),
	}
	for _, p := range s.scene.Paths {
		payload.Blend = append(payload.Blend, p.Blend)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO frames
		(scene_id,time_us,frame_index,commit_seq,payload,content_hash,interp_precision,tolerance,params_version,blend_version,algorithm_version,spec_version)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(scene_id,time_us) DO UPDATE SET
			frame_index=excluded.frame_index,
			commit_seq=excluded.commit_seq,
			payload=excluded.payload,
			content_hash=excluded.content_hash,
			interp_precision=excluded.interp_precision,
			tolerance=excluded.tolerance,
			params_version=excluded.params_version,
			blend_version=excluded.blend_version,
			algorithm_version=excluded.algorithm_version,
			spec_version=excluded.spec_version`,
		s.scene.ID, frame.Time, frame.Index, frame.CommitSequence, string(data), frame.ContentHash,
		s.scene.Interpolation.PrecisionBits, s.scene.Tolerance, "params-v1",
		s.scene.BlendVersion, s.scene.AlgorithmVersion, snapshotSchemaVersion)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "scene/"+s.scene.ID+"/fingerprint", s.sceneKey); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Validate loads a complete or prefix snapshot. Any corrupt row invalidates all rows.
func (s *SnapshotStore) Validate() ([]*RenderedFrame, SnapshotStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var status SnapshotStatus
	var storedFingerprint string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, "scene/"+s.scene.ID+"/fingerprint").Scan(&storedFingerprint)
	if err != nil && err != sql.ErrNoRows {
		return nil, status, err
	}
	if storedFingerprint != "" && storedFingerprint != s.sceneKey {
		reason := "scene fingerprint mismatch"
		if err := s.invalidateLocked(reason); err != nil {
			return nil, status, err
		}
		status.Invalidated = true
		status.Reason = reason
		return nil, status, nil
	}
	rows, err := s.db.Query(`SELECT time_us,frame_index,commit_seq,payload,content_hash,interp_precision,tolerance,params_version,blend_version,algorithm_version,spec_version
		FROM frames WHERE scene_id=? ORDER BY commit_seq ASC`, s.scene.ID)
	if err != nil {
		return nil, status, err
	}
	defer rows.Close()
	type rowData struct {
		time, index, seq     int64
		payload, hash        string
		precision, tolerance int64
		paramsVer, blendVer  string
		algoVer, specVer     string
	}
	var stored []rowData
	for rows.Next() {
		var r rowData
		if err := rows.Scan(&r.time, &r.index, &r.seq, &r.payload, &r.hash, &r.precision, &r.tolerance, &r.paramsVer, &r.blendVer, &r.algoVer, &r.specVer); err != nil {
			return nil, status, err
		}
		stored = append(stored, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, status, err
	}
	if err := rows.Close(); err != nil {
		return nil, status, err
	}
	if len(stored) == 0 {
		status.Valid = true
		return nil, status, nil
	}
	bad := ""
	if stored[0].seq != 0 {
		bad = "commit sequence does not start at zero"
	}
	var prevTime int64
	for i, r := range stored {
		if bad != "" {
			break
		}
		if r.seq != int64(i) {
			bad = fmt.Sprintf("commit sequence gap at row %d", i)
			break
		}
		if i > 0 && r.time <= prevTime {
			bad = fmt.Sprintf("time order is not strictly increasing at commit %d", i)
			break
		}
		prevTime = r.time
		if r.precision != int64(s.scene.Interpolation.PrecisionBits) {
			bad = fmt.Sprintf("interpolation precision mismatch at time %d", r.time)
			break
		}
		if r.tolerance != s.scene.Tolerance {
			bad = fmt.Sprintf("tolerance mismatch at time %d", r.time)
			break
		}
		if r.paramsVer != "params-v1" || r.blendVer != s.scene.BlendVersion || r.algoVer != s.scene.AlgorithmVersion || r.specVer != snapshotSchemaVersion {
			bad = fmt.Sprintf("version mismatch at time %d", r.time)
			break
		}
	}
	if bad != "" {
		if err := s.invalidateLocked(bad); err != nil {
			return nil, status, err
		}
		status.Invalidated = true
		status.Reason = bad
		return nil, status, nil
	}
	frames := make([]*RenderedFrame, 0, len(stored))
	for _, r := range stored {
		var payload snapshotPayload
		if err := json.Unmarshal([]byte(r.payload), &payload); err != nil {
			bad = "payload decode failure"
			break
		}
		frame := payload.Frame
		frame.RGBA = payload.RGBA
		if frame.ContentHash != r.hash {
			bad = fmt.Sprintf("stored hash mismatch at time %d", r.time)
			break
		}
		recomputed := frameHash(&frame, s.scene)
		if recomputed != r.hash {
			bad = fmt.Sprintf("content hash mismatch at time %d", r.time)
			break
		}
		if frame.Time != r.time || int64(frame.Index) != r.index {
			bad = fmt.Sprintf("frame identity mismatch at time %d", r.time)
			break
		}
		frames = append(frames, &frame)
		status.LoadedTimes = append(status.LoadedTimes, frame.Time)
		status.CommitSeq = append(status.CommitSeq, frame.CommitSequence)
	}
	if bad != "" {
		if err := s.invalidateLocked(bad); err != nil {
			return nil, status, err
		}
		status.Invalidated = true
		status.Reason = bad
		return nil, status, nil
	}
	status.Valid = true
	status.FrameCount = len(frames)
	return frames, status, nil
}

func (s *SnapshotStore) invalidateLocked(reason string) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM frames WHERE scene_id=?`, s.scene.ID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM meta WHERE key=?`, "scene/"+s.scene.ID+"/fingerprint"); err != nil {
		_ = tx.Rollback()
		return err
	}
	_ = reason
	return tx.Commit()
}
