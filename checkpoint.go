package iconcut

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	StageMapping = "mapping"
	StageGrow    = "grow"
	StageClean   = "clean"
	StageStats   = "stats"

	StatePending   = "pending"
	StateRunning   = "running"
	StateCommitted = "committed"
	StateFailed    = "failed"
)

var StageOrder = []string{StageMapping, StageGrow, StageClean, StageStats}

func stageRank(stage string) int {
	for i, s := range StageOrder {
		if s == stage {
			return i
		}
	}
	return len(StageOrder)
}

type TileKey struct {
	TileIndex int
	TX        int
	TY        int
	Stage     string
}

func (k TileKey) String() string {
	return fmt.Sprintf("tile-%02d/%s", k.TileIndex, k.Stage)
}

type Snapshot struct {
	ImageID          string         `json:"image_id"`
	Manifest         *Manifest      `json:"manifest"`
	Tiles            []Tile         `json:"tiles"`
	Versions         Versions       `json:"versions"`
	Param            *SegmentParams `json:"param_snapshot,omitempty"`
	TruthFingerprint string         `json:"truth_fingerprint"`
}

type Versions struct {
	Algorithm string `json:"algorithm"`
	Manifest  string `json:"manifest"`
	Mapping   string `json:"mapping"`
	Scheme    string `json:"scheme"`
	Param     string `json:"param"`
}

type CommitRecord struct {
	CommitSeq        int64  `json:"commit_seq"`
	ImageID          string `json:"image_id"`
	TileIndex        int    `json:"tile_index"`
	TX               int    `json:"tx"`
	TY               int    `json:"ty"`
	Stage            string `json:"stage"`
	State            string `json:"state"`
	Digest           string `json:"digest"`
	MappingVersion   string `json:"mapping_version"`
	SchemeVersion    string `json:"scheme_version"`
	ParamVersion     string `json:"param_version"`
	ContentHash      string `json:"content_hash"`
	AlgorithmVersion string `json:"algorithm_version"`
	SummaryJSON      string `json:"summary_json"`
	Artifact         []byte `json:"artifact,omitempty"`
	CommittedNano    int64  `json:"committed_nano"`
}

type TileState struct {
	TileIndex    int     `json:"tile_index"`
	TX           int     `json:"tx"`
	TY           int     `json:"ty"`
	Stage        string  `json:"stage"`
	State        string  `json:"state"`
	Progress     float64 `json:"progress"`
	Attempt      int     `json:"attempt"`
	ErrorCode    string  `json:"error_code,omitempty"`
	ErrorField   string  `json:"error_field,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
	UpdatedNano  int64   `json:"updated_nano"`
}

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS assets(
 image_id TEXT NOT NULL,
 name TEXT NOT NULL,
 content BLOB NOT NULL,
 PRIMARY KEY(image_id,name)
);
CREATE TABLE IF NOT EXISTS commits(
 commit_seq INTEGER PRIMARY KEY AUTOINCREMENT,
 image_id TEXT NOT NULL,
 tile_index INTEGER NOT NULL,
 tx INTEGER NOT NULL,
 ty INTEGER NOT NULL,
 stage TEXT NOT NULL,
 state TEXT NOT NULL,
 digest TEXT NOT NULL,
 mapping_version TEXT NOT NULL,
 scheme_version TEXT NOT NULL,
 param_version TEXT NOT NULL,
 content_hash TEXT NOT NULL,
 algorithm_version TEXT NOT NULL,
 summary_json TEXT NOT NULL,
 artifact BLOB NOT NULL,
 committed_nano INTEGER NOT NULL,
 UNIQUE(image_id,tile_index,stage)
);
CREATE TABLE IF NOT EXISTS meta(
 key TEXT PRIMARY KEY,
 value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS snapshots(
 param_version TEXT PRIMARY KEY,
 image_id TEXT NOT NULL,
 payload_json TEXT NOT NULL,
 algorithm_version TEXT NOT NULL,
 created_nano INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tile_state(
 image_id TEXT NOT NULL,
 tile_index INTEGER NOT NULL,
 tx INTEGER NOT NULL,
 ty INTEGER NOT NULL,
 stage TEXT NOT NULL,
 state TEXT NOT NULL,
 progress REAL NOT NULL DEFAULT 0,
 attempt INTEGER NOT NULL DEFAULT 0,
 error_code TEXT,
 error_field TEXT,
 error_message TEXT,
 updated_nano INTEGER NOT NULL,
 PRIMARY KEY(image_id,tile_index,stage)
);
`

func OpenStore(path string) (*Store, error) {
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SaveAsset(ctx context.Context, imageID, name string, content []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO assets(image_id,name,content) VALUES(?,?,?) ON CONFLICT(image_id,name) DO UPDATE SET content=excluded.content`, imageID, name, content)
	return err
}

func (s *Store) LoadAsset(ctx context.Context, imageID, name string) ([]byte, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT content FROM assets WHERE image_id=? AND name=?`, imageID, name).Scan(&b)
	return b, err
}

func (s *Store) SaveSnapshot(ctx context.Context, snap *Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	_, err = tx.ExecContext(ctx, `INSERT INTO snapshots(param_version,image_id,payload_json,algorithm_version,created_nano)
		VALUES(?,?,?,?,?) ON CONFLICT(param_version) DO UPDATE SET payload_json=excluded.payload_json, algorithm_version=excluded.algorithm_version`,
		snap.Versions.Param, snap.ImageID, string(b), snap.Versions.Algorithm, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('snapshot_json',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(b))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('image_id',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, snap.ImageID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('truth_fingerprint',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, snap.TruthFingerprint)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LoadSnapshot(ctx context.Context) (*Snapshot, error) {
	payload, err := s.GetMeta(ctx, "snapshot_json")
	if err != nil {
		return nil, err
	}
	if payload == "" {
		return nil, nil
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(payload), &snap); err != nil {
		return nil, NewError(EMeta, "meta.snapshot_json", "invalid snapshot: %v", err)
	}
	return &snap, nil
}

func legalTransition(old, next string) bool {
	if old == "" {
		return next == StateRunning || next == StatePending || next == StateFailed
	}
	switch old + "->" + next {
	case StatePending + "->" + StateRunning,
		StateRunning + "->" + StateCommitted,
		StateRunning + "->" + StateFailed,
		StateFailed + "->" + StatePending,
		StatePending + "->" + StatePending:
		return true
	default:
		return false
	}
}

func (s *Store) transitionLocked(ctx context.Context, imageID string, k TileKey, next string, progress float64, attempt int, e *Error) (*TileState, error) {
	row := s.db.QueryRowContext(ctx, `SELECT state,progress,attempt FROM tile_state WHERE image_id=? AND tile_index=? AND stage=?`, imageID, k.TileIndex, k.Stage)
	var oldState string
	var oldProgress float64
	var oldAttempt int
	err := row.Scan(&oldState, &oldProgress, &oldAttempt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && !legalTransition(oldState, next) {
		return nil, WithDetails(NewError(EState, fmt.Sprintf("tile_state[%d.%s].state", k.TileIndex, k.Stage), "illegal transition %s -> %s", oldState, next), map[string]any{
			"tile_index": k.TileIndex, "stage": k.Stage, "old_state": oldState, "new_state": next, "progress": oldProgress,
		})
	}
	if next == StateRunning && oldState == StateFailed {
		if !legalTransition(StateFailed, StatePending) {
			// unreachable because map contains failed->pending
		}
		oldState = StatePending
	}
	if attempt == 0 {
		if next == StateRunning {
			attempt = oldAttempt + 1
		} else {
			attempt = oldAttempt
		}
	}
	if progress < 0 {
		progress = oldProgress
	}
	var code, field, msg any
	if e != nil {
		code = string(e.Code)
		field = e.Field
		msg = e.Message
	}
	now := time.Now().UnixNano()
	_, err = s.db.ExecContext(ctx, `INSERT INTO tile_state
	(image_id,tile_index,tx,ty,stage,state,progress,attempt,error_code,error_field,error_message,updated_nano)
	VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(image_id,tile_index,stage) DO UPDATE SET
	state=excluded.state,progress=excluded.progress,attempt=excluded.attempt,
	error_code=excluded.error_code,error_field=excluded.error_field,error_message=excluded.error_message,
	updated_nano=excluded.updated_nano`,
		imageID, k.TileIndex, k.TX, k.TY, k.Stage, next, progress, attempt, code, field, msg, now)
	if err != nil {
		return nil, err
	}
	return &TileState{TileIndex: k.TileIndex, TX: k.TX, TY: k.TY, Stage: k.Stage, State: next, Progress: progress, Attempt: attempt, UpdatedNano: now}, nil
}

func (s *Store) Transition(ctx context.Context, imageID string, k TileKey, next string, progress float64, e *Error) (*TileState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transitionLocked(ctx, imageID, k, next, progress, 0, e)
}

func (s *Store) Retry(ctx context.Context, imageID string, k TileKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.transitionLocked(ctx, imageID, k, StatePending, 0, 0, nil)
	return err
}

func (s *Store) MarkRunning(ctx context.Context, imageID string, t Tile, stage string) error {
	k := TileKey{TileIndex: t.Index, TX: t.TX, TY: t.TY, Stage: stage}
	_, err := s.Transition(ctx, imageID, k, StateRunning, 0, nil)
	return err
}

func (s *Store) MarkFailed(ctx context.Context, imageID string, t Tile, stage string, runErr error) error {
	e, ok := AsError(runErr)
	if !ok {
		e = NewError(EInternal, "", "%v", runErr)
	}
	k := TileKey{TileIndex: t.Index, TX: t.TX, TY: t.TY, Stage: stage}
	_, err := s.Transition(ctx, imageID, k, StateFailed, -1, e)
	return err
}

func (s *Store) CommitStage(ctx context.Context, rec *CommitRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	err = tx.QueryRowContext(ctx, `SELECT state FROM tile_state WHERE image_id=? AND tile_index=? AND stage=?`, rec.ImageID, rec.TileIndex, rec.Stage).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && old == StateCommitted {
		return WithDetails(NewError(EState, fmt.Sprintf("commits[%d.%s]", rec.TileIndex, rec.Stage), "stage is already committed"), map[string]any{
			"tile_index": rec.TileIndex, "stage": rec.Stage,
		})
	}
	now := time.Now().UnixNano()
	if rec.CommittedNano == 0 {
		rec.CommittedNano = now
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO commits
	(image_id,tile_index,tx,ty,stage,state,digest,mapping_version,scheme_version,param_version,content_hash,algorithm_version,summary_json,artifact,committed_nano)
	VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ImageID, rec.TileIndex, rec.TX, rec.TY, rec.Stage, StateCommitted, rec.Digest,
		rec.MappingVersion, rec.SchemeVersion, rec.ParamVersion, rec.ContentHash, rec.AlgorithmVersion,
		rec.SummaryJSON, rec.Artifact, rec.CommittedNano)
	if err != nil {
		return err
	}
	if seq, err := res.LastInsertId(); err == nil {
		rec.CommitSeq = seq
	}
	var oldState string
	_ = tx.QueryRowContext(ctx, `SELECT state FROM tile_state WHERE image_id=? AND tile_index=? AND stage=?`, rec.ImageID, rec.TileIndex, rec.Stage).Scan(&oldState)
	if oldState != "" && oldState != StateRunning && oldState != StateCommitted {
		return WithDetails(NewError(EState, fmt.Sprintf("tile_state[%d.%s].state", rec.TileIndex, rec.Stage), "cannot commit from state %s", oldState), map[string]any{
			"tile_index": rec.TileIndex, "stage": rec.Stage, "old_state": oldState,
		})
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tile_state
	(image_id,tile_index,tx,ty,stage,state,progress,attempt,error_code,error_field,error_message,updated_nano)
	VALUES(?,?,?,?,?,?,1,COALESCE((SELECT attempt FROM tile_state WHERE image_id=? AND tile_index=? AND stage=?),1),NULL,NULL,NULL,?)
	ON CONFLICT(image_id,tile_index,stage) DO UPDATE SET state=excluded.state,progress=1,error_code=NULL,error_field=NULL,error_message=NULL,updated_nano=excluded.updated_nano`,
		rec.ImageID, rec.TileIndex, rec.TX, rec.TY, rec.Stage, StateCommitted,
		rec.ImageID, rec.TileIndex, rec.Stage, time.Now().UnixNano())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LoadCommits(ctx context.Context, imageID string) ([]CommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT commit_seq,image_id,tile_index,tx,ty,stage,state,digest,mapping_version,scheme_version,param_version,content_hash,algorithm_version,summary_json,artifact,committed_nano
		FROM commits WHERE image_id=? ORDER BY commit_seq`, imageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommitRecord
	for rows.Next() {
		var r CommitRecord
		if err := rows.Scan(&r.CommitSeq, &r.ImageID, &r.TileIndex, &r.TX, &r.TY, &r.Stage, &r.State, &r.Digest,
			&r.MappingVersion, &r.SchemeVersion, &r.ParamVersion, &r.ContentHash, &r.AlgorithmVersion,
			&r.SummaryJSON, &r.Artifact, &r.CommittedNano); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) LoadCommitSet(ctx context.Context, imageID string) (map[string]CommitRecord, error) {
	rows, err := s.LoadCommits(ctx, imageID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]CommitRecord, len(rows))
	for _, r := range rows {
		out[commitKey(r.TileIndex, r.Stage)] = r
	}
	return out, nil
}

func commitKey(tileIndex int, stage string) string {
	return fmt.Sprintf("%d:%s", tileIndex, stage)
}

func (s *Store) ListTileStates(ctx context.Context, imageID string) ([]TileState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tile_index,tx,ty,stage,state,progress,attempt,COALESCE(error_code,''),COALESCE(error_field,''),COALESCE(error_message,''),updated_nano
		FROM tile_state WHERE image_id=? ORDER BY tile_index,stage`, imageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TileState
	for rows.Next() {
		var st TileState
		if err := rows.Scan(&st.TileIndex, &st.TX, &st.TY, &st.Stage, &st.State, &st.Progress, &st.Attempt, &st.ErrorCode, &st.ErrorField, &st.ErrorMessage, &st.UpdatedNano); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) RecoverInterrupted(ctx context.Context, imageID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE tile_state SET state=?, updated_nano=? WHERE image_id=? AND state=?`, StatePending, time.Now().UnixNano(), imageID, StateRunning)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) ResetImage(ctx context.Context, imageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range []string{
		`DELETE FROM commits WHERE image_id=?`,
		`DELETE FROM tile_state WHERE image_id=?`,
		`DELETE FROM assets WHERE image_id=?`,
		`DELETE FROM snapshots WHERE image_id=?`,
	} {
		if _, err := s.db.ExecContext(ctx, q, imageID); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE key IN ('snapshot_json','image_id','truth_fingerprint')`)
	return err
}

func ValidateCheckpoint(ctx context.Context, s *Store, snap *Snapshot) error {
	commits, err := s.LoadCommits(ctx, snap.ImageID)
	if err != nil {
		return err
	}
	for _, c := range commits {
		if got := SHA256Bytes(c.Artifact); got != c.ContentHash {
			return WithDetails(NewError(EMeta, fmt.Sprintf("commits[%d].content_hash", c.CommitSeq), "artifact hash mismatch"), map[string]any{
				"tile_index": c.TileIndex, "stage": c.Stage, "expected": c.ContentHash, "actual": got,
			})
		}
		if c.AlgorithmVersion != snap.Versions.Algorithm || c.MappingVersion != snap.Versions.Mapping ||
			c.SchemeVersion != snap.Versions.Scheme || c.ParamVersion != snap.Versions.Param {
			return WithDetails(NewError(EMeta, fmt.Sprintf("commits[%d].version", c.CommitSeq), "stage artifact version mismatch"), map[string]any{
				"tile_index": c.TileIndex, "stage": c.Stage,
				"record":   map[string]string{"algorithm": c.AlgorithmVersion, "mapping": c.MappingVersion, "scheme": c.SchemeVersion, "param": c.ParamVersion},
				"snapshot": snap.Versions,
			})
		}
	}
	return nil
}
