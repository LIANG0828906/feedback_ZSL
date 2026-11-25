package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"channelpipe"

	_ "modernc.org/sqlite"
)

type Identity struct {
	PlanFingerprint  string `json:"plan_fingerprint"`
	ParamVersion     string `json:"param_version"`
	AlgorithmVersion string `json:"algorithm_version"`
}

type OpenInfo struct {
	Reset           bool     `json:"reset"`
	Reason          string   `json:"reason,omitempty"`
	Previous        Identity `json:"previous,omitempty"`
	Current         Identity `json:"current"`
	CommittedChunks int      `json:"committed_chunks,omitempty"`
}

type StageOutput struct {
	StageIndex int                   `json:"stage_index"`
	StageID    string                `json:"stage_id"`
	Buffer     *channelpipe.Buffer   `json:"-"`
	CacheKey   string                `json:"cache_key"`
	OutputHash string                `json:"output_hash"`
	Dropped    []channelpipe.Channel `json:"dropped,omitempty"`
}

type CommitChunk struct {
	ImageID    string
	Chunk      channelpipe.Chunk
	ChunkOrder int
	Outputs    []StageOutput
	WaitMS     int64
}

type ChunkRecord struct {
	ImageID    string               `json:"image_id"`
	ChunkIndex int                  `json:"chunk_index"`
	Rect       channelpipe.Rect     `json:"rect"`
	State      channelpipe.RunState `json:"state"`
	Attempts   int                  `json:"attempts"`
	LastError  string               `json:"last_error,omitempty"`
	WaitMS     int64                `json:"wait_ms"`
	UpdatedAt  time.Time            `json:"updated_at"`
}

type QueueEvent struct {
	ID         int64  `json:"id"`
	ImageID    string `json:"image_id"`
	ChunkIndex int    `json:"chunk_index"`
	Reason     string `json:"reason"`
	WaitMS     int64  `json:"wait_ms"`
	CreatedAt  string `json:"created_at"`
}

type Store struct {
	db       *sql.DB
	mu       sync.Mutex
	identity Identity
	path     string
}

func Open(path string, expected Identity) (*Store, *OpenInfo, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, nil, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, path, "cannot open sqlite")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		_ = db.Close()
		return nil, nil, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "pragma", "cannot set busy timeout")
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, nil, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "pragma", "cannot enable WAL")
	}
	store := &Store{db: db, identity: expected, path: path}
	if err := store.initSchema(); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	info, err := store.verifyOrReset(expected)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return store, info, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

func (s *Store) Identity() Identity { return s.identity }

func (s *Store) initSchema() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS chunks (
			image_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			x INTEGER NOT NULL, y INTEGER NOT NULL, width INTEGER NOT NULL, height INTEGER NOT NULL,
			state TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			wait_ms INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(image_id, chunk_index)
		)`,
		`CREATE TABLE IF NOT EXISTS stage_states (
			image_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			stage_index INTEGER NOT NULL,
			state TEXT NOT NULL,
			cache_key TEXT NOT NULL DEFAULT '',
			output_hash TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL,
			PRIMARY KEY(image_id, chunk_index, stage_index)
		)`,
		`CREATE TABLE IF NOT EXISTS chunk_outputs (
			image_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			stage_index INTEGER NOT NULL,
			stage_id TEXT NOT NULL,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			channels INTEGER NOT NULL,
			channel_layout TEXT NOT NULL DEFAULT '',
			depth INTEGER NOT NULL,
			blob BLOB NOT NULL,
			cache_key TEXT NOT NULL,
			output_hash TEXT NOT NULL,
			plan_fingerprint TEXT NOT NULL,
			param_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			dropped_json TEXT NOT NULL DEFAULT '[]',
			PRIMARY KEY(image_id, chunk_index, stage_index)
		)`,
		`CREATE TABLE IF NOT EXISTS commit_log (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			image_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			chunk_order INTEGER NOT NULL,
			stage_count INTEGER NOT NULL,
			plan_fingerprint TEXT NOT NULL,
			cache_keys_json TEXT NOT NULL,
			committed_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS failure_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			image_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			stage_index INTEGER NOT NULL DEFAULT -1,
			error_code TEXT NOT NULL DEFAULT '',
			message TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS queue_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			image_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			reason TEXT NOT NULL,
			wait_ms INTEGER NOT NULL,
			created_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "schema", "cannot initialize sqlite schema")
		}
	}
	return nil
}

func (s *Store) verifyOrReset(expected Identity) (*OpenInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	info := &OpenInfo{Current: expected}
	keys := []string{"plan_fingerprint", "param_version", "algorithm_version"}
	values := map[string]string{}
	for _, key := range keys {
		var value string
		err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "meta", "cannot read snapshot identity")
		}
		values[key] = value
	}
	if len(values) == 0 {
		if err := s.putIdentityLocked(expected); err != nil {
			return nil, err
		}
		return info, nil
	}
	previous := Identity{PlanFingerprint: values["plan_fingerprint"], ParamVersion: values["param_version"], AlgorithmVersion: values["algorithm_version"]}
	info.Previous = previous

	rowMismatch, err := s.hasRowIdentityMismatchLocked(expected)
	if err != nil {
		return nil, err
	}
	hashMismatch, err := s.hasOutputHashMismatchLocked()
	if err != nil {
		return nil, err
	}
	if previous != expected || rowMismatch || hashMismatch {
		reason := "snapshot identity mismatch"
		if rowMismatch {
			reason = "persisted row hash/version mismatch"
		}
		if hashMismatch {
			reason = "persisted output content hash mismatch"
		}
		if err := s.resetLocked(expected); err != nil {
			return nil, err
		}
		info.Reset = true
		info.Reason = reason
		return info, nil
	}

	var committed int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE state=?`, channelpipe.StateCommitted).Scan(&committed); err != nil {
		return nil, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "chunks", "cannot count committed chunks")
	}
	info.CommittedChunks = committed
	return info, nil
}

func (s *Store) hasRowIdentityMismatchLocked(expected Identity) (bool, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM chunk_outputs
		WHERE plan_fingerprint <> ? OR param_version <> ? OR algorithm_version <> ?`,
		expected.PlanFingerprint, expected.ParamVersion, expected.AlgorithmVersion,
	).Scan(&count)
	if err != nil {
		return false, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "chunk_outputs", "cannot verify row identity")
	}
	return count > 0, nil
}

func (s *Store) hasOutputHashMismatchLocked() (bool, error) {
	rows, err := s.db.Query(`SELECT width,height,channels,channel_layout,depth,blob,output_hash FROM chunk_outputs`)
	if err != nil {
		return false, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "chunk_outputs", "cannot read output hashes for verification")
	}
	defer rows.Close()
	for rows.Next() {
		var width, height, channels, depth int
		var layout, expectedHash string
		var blob []byte
		if err := rows.Scan(&width, &height, &channels, &layout, &depth, &blob, &expectedHash); err != nil {
			return false, err
		}
		buffer, err := channelpipe.DecodeBuffer(blob, width, height, channels, depth)
		if err != nil {
			return true, nil
		}
		for _, r := range layout {
			buffer.Channels = append(buffer.Channels, channelpipe.Channel(r))
		}
		actualHash, err := channelpipe.RegionHash(buffer)
		if err != nil {
			return false, err
		}
		if actualHash != expectedHash {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) putIdentityLocked(identity Identity) error {
	for key, value := range map[string]string{
		"plan_fingerprint":  identity.PlanFingerprint,
		"param_version":     identity.ParamVersion,
		"algorithm_version": identity.AlgorithmVersion,
	} {
		if _, err := s.db.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "meta."+key, "cannot write identity")
		}
	}
	return nil
}

func (s *Store) resetLocked(expected Identity) error {
	tx, err := s.db.Begin()
	if err != nil {
		return channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "reset", "cannot begin reset")
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"chunk_outputs", "stage_states", "commit_log", "failure_events", "queue_events", "chunks", "meta"} {
		if _, err := tx.Exec(`DELETE FROM ` + table); err != nil {
			return channelpipe.WrapError(channelpipe.CodeCheckpoint, err, table, "cannot clear stale snapshot")
		}
	}
	if _, err := tx.Exec(`DELETE FROM sqlite_sequence WHERE name IN ('commit_log','failure_events','queue_events')`); err != nil {
		return channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "sqlite_sequence", "cannot reset sequences")
	}
	if err := tx.Commit(); err != nil {
		return channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "reset", "cannot commit snapshot reset")
	}
	return s.putIdentityLocked(expected)
}

func (s *Store) ResetNow(expected Identity, reason string) (*OpenInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.identity
	if err := s.resetLocked(expected); err != nil {
		return nil, err
	}
	s.identity = expected
	return &OpenInfo{Reset: true, Reason: reason, Previous: previous, Current: expected}, nil
}

func (s *Store) BeginLiveFingerprint(fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.identity.PlanFingerprint = fingerprint
	_, err := s.db.Exec(`UPDATE meta SET value=? WHERE key='plan_fingerprint'`, fingerprint)
	if err != nil {
		return channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "meta.plan_fingerprint", "cannot update live fingerprint")
	}
	return nil
}

func (s *Store) EnsureChunk(imageID string, chunk channelpipe.Chunk, stageCount int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Exec(`INSERT OR IGNORE INTO chunks(image_id,chunk_index,x,y,width,height,state,attempts,updated_at)
		VALUES(?,?,?,?,?,?,?,0,?)`, imageID, chunk.Index, chunk.Rect.X, chunk.Rect.Y, chunk.Rect.Width, chunk.Rect.Height, channelpipe.StatePending, now)
	if err != nil {
		return channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "chunks", "cannot create chunk")
	}
	for stage := 0; stage < stageCount; stage++ {
		_, err = tx.Exec(`INSERT OR IGNORE INTO stage_states(image_id,chunk_index,stage_index,state,updated_at)
			VALUES(?,?,?,?,?)`, imageID, chunk.Index, stage, channelpipe.StatePending, now)
		if err != nil {
			return channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "stage_states", "cannot create stage state")
		}
	}
	return tx.Commit()
}

func (s *Store) RecoverStale(imageID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT chunk_index,state FROM chunks WHERE image_id=?`, imageID)
	if err != nil {
		return 0, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "chunks", "cannot query recovery states")
	}
	type chunkState struct {
		index int
		state channelpipe.RunState
	}
	var chunks []chunkState
	for rows.Next() {
		var index int
		var state string
		if err := rows.Scan(&index, &state); err != nil {
			_ = rows.Close()
			return 0, err
		}
		chunks = append(chunks, chunkState{index, channelpipe.RunState(state)})
	}
	_ = rows.Close()
	recovered := 0
	for _, chunk := range chunks {
		if chunk.state != channelpipe.StateProcessing {
			continue
		}
		if _, err := channelpipe.TransitionState(chunk.state, channelpipe.StatePending); err != nil {
			return recovered, err
		}
		if _, err := s.db.Exec(`UPDATE chunks SET state=?, last_error='recovered stale processing lease', updated_at=? WHERE image_id=? AND chunk_index=?`, channelpipe.StatePending, nowNano(), imageID, chunk.index); err != nil {
			return recovered, err
		}
		if _, err := s.db.Exec(`UPDATE stage_states SET state=?, updated_at=? WHERE image_id=? AND chunk_index=? AND state=?`, channelpipe.StatePending, nowNano(), imageID, chunk.index, channelpipe.StateProcessing); err != nil {
			return recovered, err
		}
		recovered++
	}
	return recovered, nil
}

func (s *Store) ChunkState(imageID string, chunkIndex int) (channelpipe.RunState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chunkStateLocked(imageID, chunkIndex)
}

func (s *Store) chunkStateLocked(imageID string, chunkIndex int) (channelpipe.RunState, error) {
	var state string
	err := s.db.QueryRow(`SELECT state FROM chunks WHERE image_id=? AND chunk_index=?`, imageID, chunkIndex).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", channelpipe.FieldError(channelpipe.CodeState, "chunk", "chunk %s#%d does not exist", imageID, chunkIndex)
	}
	if err != nil {
		return "", channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "chunks.state", "cannot read state")
	}
	return channelpipe.RunState(state), nil
}

func (s *Store) TransitionChunk(imageID string, chunkIndex int, target channelpipe.RunState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.chunkStateLocked(imageID, chunkIndex)
	if err != nil {
		return err
	}
	next, err := channelpipe.TransitionState(current, target)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE chunks SET state=?, updated_at=? WHERE image_id=? AND chunk_index=?`, next, nowNano(), imageID, chunkIndex)
	if err != nil {
		return channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "chunks.state", "cannot update chunk state")
	}
	return nil
}

func (s *Store) StartChunk(imageID string, chunkIndex int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.chunkStateLocked(imageID, chunkIndex)
	if err != nil {
		return err
	}
	next, err := channelpipe.TransitionState(current, channelpipe.StateProcessing)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE chunks SET state=?, attempts=attempts+1, updated_at=? WHERE image_id=? AND chunk_index=?`, next, nowNano(), imageID, chunkIndex)
	return err
}

func (s *Store) StartStage(imageID string, chunkIndex, stageIndex int) error {
	return s.transitionStage(imageID, chunkIndex, stageIndex, channelpipe.StateProcessing, nil)
}

func (s *Store) FailStage(imageID string, chunkIndex, stageIndex int, cause error) error {
	return s.transitionStage(imageID, chunkIndex, stageIndex, channelpipe.StateFailed, cause)
}

func (s *Store) transitionStage(imageID string, chunkIndex, stageIndex int, target channelpipe.RunState, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var state string
	err := s.db.QueryRow(`SELECT state FROM stage_states WHERE image_id=? AND chunk_index=? AND stage_index=?`, imageID, chunkIndex, stageIndex).Scan(&state)
	if err != nil {
		return channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "stage_states.state", "cannot read stage state")
	}
	next, err := channelpipe.TransitionState(channelpipe.RunState(state), target)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE stage_states SET state=?, updated_at=? WHERE image_id=? AND chunk_index=? AND stage_index=?`, next, nowNano(), imageID, chunkIndex, stageIndex)
	if target == channelpipe.StateFailed && cause != nil {
		_, _ = s.db.Exec(`INSERT INTO failure_events(image_id,chunk_index,stage_index,error_code,message,created_at) VALUES(?,?,?,?,?,?)`, imageID, chunkIndex, stageIndex, errorCode(cause), cause.Error(), nowNano())
		_, _ = s.db.Exec(`UPDATE chunks SET state=?, last_error=?, updated_at=? WHERE image_id=? AND chunk_index=?`, channelpipe.StateFailed, cause.Error(), nowNano(), imageID, chunkIndex)
	}
	return err
}

func (s *Store) FailChunk(imageID string, chunkIndex int, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.chunkStateLocked(imageID, chunkIndex)
	if err != nil {
		return err
	}
	next, err := channelpipe.TransitionState(current, channelpipe.StateFailed)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE chunks SET state=?, last_error=?, updated_at=? WHERE image_id=? AND chunk_index=?`, next, cause.Error(), nowNano(), imageID, chunkIndex)
	if err == nil {
		_, _ = s.db.Exec(`INSERT INTO failure_events(image_id,chunk_index,stage_index,error_code,message,created_at) VALUES(?,?,?,?,?,?)`, imageID, chunkIndex, -1, errorCode(cause), cause.Error(), nowNano())
	}
	return err
}

func (s *Store) RecordQueueEvent(event QueueEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO queue_events(image_id,chunk_index,reason,wait_ms,created_at) VALUES(?,?,?,?,?)`, event.ImageID, event.ChunkIndex, event.Reason, event.WaitMS, nowNano())
	return err
}

func (s *Store) CommitChunk(ctx context.Context, commit CommitChunk) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "commit", "cannot begin transaction")
	}
	defer func() { _ = tx.Rollback() }()

	var state string
	err = tx.QueryRow(`SELECT state FROM chunks WHERE image_id=? AND chunk_index=?`, commit.ImageID, commit.Chunk.Index).Scan(&state)
	if err != nil {
		return 0, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "chunks.state", "cannot lock chunk for commit")
	}
	if _, err := channelpipe.TransitionState(channelpipe.RunState(state), channelpipe.StateCommitted); err != nil {
		return 0, err
	}

	cacheKeys := make([]string, 0, len(commit.Outputs))
	for _, output := range commit.Outputs {
		if output.Buffer == nil {
			return 0, channelpipe.FieldError(channelpipe.CodeInvalid, "outputs", "missing buffer for stage %d", output.StageIndex)
		}
		blob, err := channelpipe.EncodeBuffer(output.Buffer)
		if err != nil {
			return 0, err
		}
		droppedJSON, err := json.Marshal(output.Dropped)
		if err != nil {
			return 0, err
		}
		_, err = tx.Exec(`INSERT INTO chunk_outputs(
			image_id,chunk_index,stage_index,stage_id,width,height,channels,channel_layout,depth,blob,
			cache_key,output_hash,plan_fingerprint,param_version,algorithm_version,dropped_json)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(image_id,chunk_index,stage_index) DO UPDATE SET
			stage_id=excluded.stage_id,width=excluded.width,height=excluded.height,channels=excluded.channels,
			depth=excluded.depth,blob=excluded.blob,cache_key=excluded.cache_key,output_hash=excluded.output_hash,
			plan_fingerprint=excluded.plan_fingerprint,param_version=excluded.param_version,
			algorithm_version=excluded.algorithm_version,dropped_json=excluded.dropped_json`,
			commit.ImageID, commit.Chunk.Index, output.StageIndex, output.StageID,
			output.Buffer.Width, output.Buffer.Height, output.Buffer.ChannelCount(), channelLayoutString(output.Buffer.Channels), output.Buffer.Depth, blob,
			output.CacheKey, output.OutputHash, s.identity.PlanFingerprint, s.identity.ParamVersion, s.identity.AlgorithmVersion, string(droppedJSON),
		)
		if err != nil {
			return 0, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "chunk_outputs", "cannot write stage output")
		}
		_, err = tx.Exec(`INSERT INTO stage_states(image_id,chunk_index,stage_index,state,cache_key,output_hash,updated_at)
			VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(image_id,chunk_index,stage_index) DO UPDATE SET state=excluded.state,cache_key=excluded.cache_key,output_hash=excluded.output_hash,updated_at=excluded.updated_at`,
			commit.ImageID, commit.Chunk.Index, output.StageIndex, channelpipe.StateCommitted, output.CacheKey, output.OutputHash, nowNano())
		if err != nil {
			return 0, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "stage_states", "cannot commit stage state")
		}
		cacheKeys = append(cacheKeys, output.CacheKey)
	}

	_, err = tx.Exec(`UPDATE chunks SET state=?, attempts=attempts, last_error='', wait_ms=?, updated_at=? WHERE image_id=? AND chunk_index=?`, channelpipe.StateCommitted, commit.WaitMS, nowNano(), commit.ImageID, commit.Chunk.Index)
	if err != nil {
		return 0, err
	}
	keysJSON, _ := json.Marshal(cacheKeys)
	result, err := tx.Exec(`INSERT INTO commit_log(image_id,chunk_index,chunk_order,stage_count,plan_fingerprint,cache_keys_json,committed_at)
		VALUES(?,?,?,?,?,?,?)`, commit.ImageID, commit.Chunk.Index, commit.ChunkOrder, len(commit.Outputs), s.identity.PlanFingerprint, string(keysJSON), nowNano())
	if err != nil {
		return 0, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "commit_log", "cannot write commit sequence")
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "commit", "cannot commit chunk atomically")
	}
	return seq, nil
}

func (s *Store) CommittedChunkMap(imageID string) (map[int]ChunkRecord, error) {
	rows, err := s.db.Query(`SELECT image_id,chunk_index,x,y,width,height,state,attempts,last_error,wait_ms,updated_at FROM chunks WHERE image_id=?`, imageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[int]ChunkRecord{}
	for rows.Next() {
		var record ChunkRecord
		var state string
		var updated string
		if err := rows.Scan(&record.ImageID, &record.ChunkIndex, &record.Rect.X, &record.Rect.Y, &record.Rect.Width, &record.Rect.Height, &state, &record.Attempts, &record.LastError, &record.WaitMS, &updated); err != nil {
			return nil, err
		}
		record.State = channelpipe.RunState(state)
		record.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		result[record.ChunkIndex] = record
	}
	return result, rows.Err()
}

func (s *Store) CommitSequence(imageID string) ([]int, error) {
	rows, err := s.db.Query(`SELECT chunk_index FROM commit_log WHERE image_id=? ORDER BY seq ASC`, imageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sequence []int
	for rows.Next() {
		var index int
		if err := rows.Scan(&index); err != nil {
			return nil, err
		}
		sequence = append(sequence, index)
	}
	return sequence, rows.Err()
}

func (s *Store) CommitSequenceForFingerprint(imageID, fingerprint string) ([]int, error) {
	rows, err := s.db.Query(`SELECT chunk_index FROM commit_log WHERE image_id=? AND plan_fingerprint=? ORDER BY seq ASC`, imageID, fingerprint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sequence []int
	for rows.Next() {
		var index int
		if err := rows.Scan(&index); err != nil {
			return nil, err
		}
		sequence = append(sequence, index)
	}
	return sequence, rows.Err()
}

type PersistedOutput struct {
	StageIndex int
	StageID    string
	Buffer     *channelpipe.Buffer
	CacheKey   string
	OutputHash string
	Dropped    []channelpipe.Channel
}

func (s *Store) LoadChunkOutputs(imageID string, chunkIndex int) ([]PersistedOutput, error) {
	rows, err := s.db.Query(`SELECT stage_index,stage_id,width,height,channels,channel_layout,depth,blob,cache_key,output_hash,dropped_json FROM chunk_outputs WHERE image_id=? AND chunk_index=? ORDER BY stage_index`, imageID, chunkIndex)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var outputs []PersistedOutput
	for rows.Next() {
		var output PersistedOutput
		var width, height, channels, depth int
		var channelLayout string
		var blob []byte
		var droppedJSON string
		if err := rows.Scan(&output.StageIndex, &output.StageID, &width, &height, &channels, &channelLayout, &depth, &blob, &output.CacheKey, &output.OutputHash, &droppedJSON); err != nil {
			return nil, err
		}
		buffer, err := channelpipe.DecodeBuffer(blob, width, height, channels, depth)
		if err != nil {
			return nil, err
		}
		output.Buffer = buffer
		for _, r := range channelLayout {
			output.Buffer.Channels = append(output.Buffer.Channels, channelpipe.Channel(r))
		}
		_ = json.Unmarshal([]byte(droppedJSON), &output.Dropped)
		outputs = append(outputs, output)
	}
	return outputs, rows.Err()
}

func (s *Store) QueueEvents() ([]QueueEvent, error) {
	rows, err := s.db.Query(`SELECT id,image_id,chunk_index,reason,wait_ms,created_at FROM queue_events ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []QueueEvent
	for rows.Next() {
		var event QueueEvent
		if err := rows.Scan(&event.ID, &event.ImageID, &event.ChunkIndex, &event.Reason, &event.WaitMS, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func nowNano() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func causeMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func errorCode(err error) string {
	if domainErr, ok := channelpipe.AsError(err); ok {
		return domainErr.Code
	}
	return "E_INTERNAL"
}

func channelLayoutString(channels []channelpipe.Channel) string {
	result := ""
	for _, channel := range channels {
		result += string(channel)
	}
	return result
}

var _ = fmt.Sprintf
