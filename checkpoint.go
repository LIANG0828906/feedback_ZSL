package vectorcore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

type CheckpointStore struct {
	db *sql.DB
	mu sync.Mutex
}

func OpenCheckpoint(path string) (*CheckpointStore, error) {
	dsn := path
	if path != ":memory:" {
		dsn = "file:" + path +
			"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	} else {
		dsn = "file::memory:?cache=shared&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, &Error{Code: CodeIO, Op: "openCheckpoint", Field: path, Keyframe: -1, TimeIndex: -1, Err: err}
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &CheckpointStore{db: db}
	if err := store.ensureSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *CheckpointStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *CheckpointStore) ensureSchema(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return &Error{Code: CodeIO, Op: "checkpointSchema", Keyframe: -1, TimeIndex: -1, Err: err}
	}
	defer tx.Rollback()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoints (
			scene_id TEXT NOT NULL,
			time_index INTEGER NOT NULL,
			commit_seq INTEGER NOT NULL,
			time_ticks INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			payload_hash TEXT NOT NULL,
			scene_hash TEXT NOT NULL,
			config_hash TEXT NOT NULL,
			interpolation_version TEXT NOT NULL,
			tolerance_version TEXT NOT NULL,
			composite_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			payload BLOB NOT NULL,
			PRIMARY KEY(scene_id, time_index),
			UNIQUE(scene_id, commit_seq)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return &Error{Code: CodeIO, Op: "checkpointSchema", Keyframe: -1, TimeIndex: -1, Err: err}
		}
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&existing)
	if err == sql.ErrNoRows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('schema_version',?)`, CheckpointSchemaVersion); err != nil {
			return &Error{Code: CodeIO, Op: "checkpointSchema", Keyframe: -1, TimeIndex: -1, Err: err}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('algorithm_version',?)`, AlgorithmVersion); err != nil {
			return &Error{Code: CodeIO, Op: "checkpointSchema", Keyframe: -1, TimeIndex: -1, Err: err}
		}
	} else if err != nil {
		return &Error{Code: CodeDecode, Op: "checkpointSchema", Field: "meta.schema_version", Keyframe: -1, TimeIndex: -1, Err: err}
	} else if existing != CheckpointSchemaVersion {
		return &Error{Code: CodeVersion, Op: "checkpointSchema", Field: "meta.schema_version", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("checkpoint schema %q, expected %q", existing, CheckpointSchemaVersion)}
	}
	var algorithm string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='algorithm_version'`).Scan(&algorithm); err != nil {
		return &Error{Code: CodeDecode, Op: "checkpointSchema", Field: "meta.algorithm_version", Keyframe: -1, TimeIndex: -1, Err: err}
	}
	if algorithm != AlgorithmVersion {
		return &Error{Code: CodeVersion, Op: "checkpointSchema", Field: "meta.algorithm_version", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("algorithm %q, expected %q", algorithm, AlgorithmVersion)}
	}
	return tx.Commit()
}

func (e *Engine) configHash() string {
	return HashValue(struct {
		Interpolation    InterpMode
		Tolerance        int64
		Scale            Scale
		Grid             int
		FillRule         FillRule
		Margin           int
		InterpolationVer string
		ToleranceVer     string
		CompositeVer     string
		Algorithm        string
	}{e.cfg.Interpolation, e.cfg.Tolerance, e.cfg.Scale, e.cfg.Grid, e.cfg.FillRule, e.cfg.Margin,
		InterpolationVersion, ToleranceVersion, CompositeVersion, AlgorithmVersion})
}

func encodeTimeResult(result *TimeResult) ([]byte, string, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(result); err != nil {
		return nil, "", &Error{Code: CodeDecode, Op: "checkpointEncode", TimeIndex: result.TimeIndex, Keyframe: -1, Err: err}
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:]), nil
}

func decodeTimeResult(data []byte) (*TimeResult, error) {
	var result TimeResult
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&result); err != nil {
		return nil, &Error{Code: CodeDecode, Op: "checkpointDecode", Keyframe: -1, TimeIndex: -1, Err: err}
	}
	return &result, nil
}

// AppendTime atomically and serially appends one committed sample time. All
// columns and the payload become visible in a single transaction.
func (s *CheckpointStore) AppendTime(ctx context.Context, result *TimeResult, sceneHash string) error {
	if result == nil || result.Bitmap == nil || result.TimeIndex < 0 || result.CommitSeq <= 0 {
		return &Error{Code: CodeState, Op: "checkpointAppend", Field: "result", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("invalid time result")}
	}
	payload, payloadHash, err := encodeTimeResult(result)
	if err != nil {
		return err
	}
	configHash := (&Engine{cfg: DefaultConfig()}).configHash()
	// The engine normally supplies its own config hash through the result's
	// viewport/config fingerprint in Resume. Direct stores use the default
	// fingerprint; Engine.AttachCheckpoint overrides it with setConfigHash.
	return s.append(ctx, result, sceneHash, configHash, payload, payloadHash)
}

func (s *CheckpointStore) append(ctx context.Context, result *TimeResult, sceneHash, configHash string, payload []byte, payloadHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return &Error{Code: CodeIO, Op: "checkpointAppend", TimeIndex: result.TimeIndex, Keyframe: -1, Err: err}
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO checkpoints(
		scene_id,time_index,commit_seq,time_ticks,content_hash,payload_hash,scene_hash,config_hash,
		interpolation_version,tolerance_version,composite_version,algorithm_version,width,height,payload
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		result.SceneID, result.TimeIndex, result.CommitSeq, result.TimeTicks, result.ContentHash, payloadHash,
		sceneHash, configHash, InterpolationVersion, ToleranceVersion, CompositeVersion, AlgorithmVersion,
		result.Bitmap.Width, result.Bitmap.Height, payload)
	if err != nil {
		return &Error{Code: CodeState, Op: "checkpointAppend", PathID: result.SceneID, TimeIndex: result.TimeIndex, Keyframe: -1,
			Err: fmt.Errorf("insert checkpoint: %w", err)}
	}
	if err := tx.Commit(); err != nil {
		return &Error{Code: CodeIO, Op: "checkpointAppend", TimeIndex: result.TimeIndex, Keyframe: -1, Err: err}
	}
	return nil
}

func (s *CheckpointStore) appendWithConfig(ctx context.Context, result *TimeResult, sceneHash, configHash string) error {
	payload, payloadHash, err := encodeTimeResult(result)
	if err != nil {
		return err
	}
	return s.append(ctx, result, sceneHash, configHash, payload, payloadHash)
}

func (s *CheckpointStore) LoadAll(ctx context.Context, sceneID, sceneHash, configHash string) ([]*TimeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT time_index,commit_seq,time_ticks,content_hash,payload_hash,scene_hash,config_hash,
		interpolation_version,tolerance_version,composite_version,algorithm_version,width,height,payload
		FROM checkpoints WHERE scene_id=? ORDER BY commit_seq`, sceneID)
	if err != nil {
		return nil, &Error{Code: CodeIO, Op: "checkpointLoad", Keyframe: -1, TimeIndex: -1, Err: err}
	}
	defer rows.Close()
	var out []*TimeResult
	lastSeq := int64(0)
	for rows.Next() {
		var (
			timeIndex, commitSeq, timeTicks, width, height        int64
			contentHash, payloadHash, rowSceneHash, rowConfigHash string
			interpVer, toleranceVer, compositeVer, algorithmVer   string
			payload                                               []byte
		)
		if err := rows.Scan(&timeIndex, &commitSeq, &timeTicks, &contentHash, &payloadHash, &rowSceneHash, &rowConfigHash,
			&interpVer, &toleranceVer, &compositeVer, &algorithmVer, &width, &height, &payload); err != nil {
			return nil, &Error{Code: CodeDecode, Op: "checkpointLoad", Keyframe: -1, TimeIndex: -1, Err: err}
		}
		if rowSceneHash != sceneHash || rowConfigHash != configHash {
			return nil, &Error{Code: CodeVersion, Op: "checkpointLoad", Field: "scene/config hash", Keyframe: -1, TimeIndex: int(timeIndex),
				Err: fmt.Errorf("checkpoint is incompatible with current scene or parameters")}
		}
		if interpVer != InterpolationVersion || toleranceVer != ToleranceVersion || compositeVer != CompositeVersion || algorithmVer != AlgorithmVersion {
			return nil, &Error{Code: CodeVersion, Op: "checkpointLoad", Field: "pipeline versions", Keyframe: -1, TimeIndex: int(timeIndex)}
		}
		if commitSeq != lastSeq+1 {
			return nil, &Error{Code: CodeDecode, Op: "checkpointLoad", Field: "commit_seq", Keyframe: -1, TimeIndex: int(timeIndex),
				Err: fmt.Errorf("commit sequence is not contiguous")}
		}
		lastSeq = commitSeq
		sum := sha256.Sum256(payload)
		if hex.EncodeToString(sum[:]) != payloadHash {
			return nil, &Error{Code: CodeDecode, Op: "checkpointLoad", Field: "payload_hash", Keyframe: -1, TimeIndex: int(timeIndex),
				Err: fmt.Errorf("payload hash mismatch")}
		}
		result, err := decodeTimeResult(payload)
		if err != nil {
			return nil, err
		}
		if result.TimeIndex != int(timeIndex) || result.CommitSeq != commitSeq || result.TimeTicks != timeTicks ||
			result.ContentHash != contentHash || result.Bitmap == nil || result.Bitmap.Width != int(width) || result.Bitmap.Height != int(height) {
			return nil, &Error{Code: CodeDecode, Op: "checkpointLoad", Field: "payload", Keyframe: -1, TimeIndex: int(timeIndex),
				Err: fmt.Errorf("payload does not match indexed columns")}
		}
		out = append(out, result)
	}
	if err := rows.Err(); err != nil {
		return nil, &Error{Code: CodeDecode, Op: "checkpointLoad", Keyframe: -1, TimeIndex: -1, Err: err}
	}
	return out, nil
}

// AttachCheckpoint makes subsequent RenderAll commits durable.
func (e *Engine) AttachCheckpoint(store *CheckpointStore) {
	e.mu.Lock()
	e.store = store
	e.mu.Unlock()
}

// LoadCheckpoint restores committed sample times and freezes their states.
func (e *Engine) LoadCheckpoint(ctx context.Context, store *CheckpointStore) error {
	if store == nil {
		return &Error{Code: CodeParam, Op: "checkpointResume", Field: "store", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("nil checkpoint store")}
	}
	done := e.beginRender()
	defer done()
	results, err := store.LoadAll(ctx, e.scene.ID, e.sceneContentHash(), e.configHash())
	if err != nil {
		return err
	}
	e.resultsMu.Lock()
	defer e.resultsMu.Unlock()
	maxSeq := int64(0)
	for _, result := range results {
		if result.TimeIndex < 0 || result.TimeIndex >= len(e.scene.SampleTimes) {
			return &Error{Code: CodeDecode, Op: "checkpointResume", Field: "time_index", Keyframe: -1, TimeIndex: result.TimeIndex}
		}
		if result.TimeTicks != e.scene.SampleTimes[result.TimeIndex] {
			return &Error{Code: CodeDecode, Op: "checkpointResume", Field: "time_ticks", Keyframe: -1, TimeIndex: result.TimeIndex,
				Err: fmt.Errorf("checkpoint time does not match scene")}
		}
		e.results[result.TimeIndex] = result
		ids := make([]string, len(e.scene.Paths))
		for i := range e.scene.Paths {
			ids[i] = e.scene.Paths[i].ID
		}
		e.states.freezeTime(result.TimeIndex, ids)
		if result.CommitSeq > maxSeq {
			maxSeq = result.CommitSeq
		}
	}
	e.nextSeq = maxSeq
	e.store = store
	return nil
}
