package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"lip-pipeline-prototype/internal/domain"
	"lip-pipeline-prototype/internal/hashx"
)

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

type CommitReceipt struct {
	CommitSeq   int64  `json:"commit_seq"`
	ShardID     string `json:"shard_id"`
	PackCount   int    `json:"pack_count"`
	FrameCount  int    `json:"frame_count"`
	PackDigest  string `json:"pack_digest"`
	CommittedAt int64  `json:"committed_at_unix_nano"`
}

type Recovery struct {
	ShardID     string              `json:"shard_id"`
	State       State               `json:"state"`
	Result      *domain.ShardResult `json:"result,omitempty"`
	Invalidated bool                `json:"invalidated"`
	Reasons     []string            `json:"reasons,omitempty"`
}

type CacheEntry struct {
	CacheKey      string `json:"cache_key"`
	NodeID        string `json:"node_id"`
	Kind          string `json:"kind"`
	ShardID       string `json:"shard_id"`
	SourceVersion string `json:"source_version"`
	PayloadJSON   string `json:"payload_json"`
	UpdatedAt     int64  `json:"updated_at"`
}

func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		dsn = path + "?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if err := store.Init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Init(ctx context.Context) error {
	ddl := []string{
		`CREATE TABLE IF NOT EXISTS shard_checkpoints (
			shard_id TEXT PRIMARY KEY,
			dialogue_id TEXT NOT NULL DEFAULT '',
			shard_index INTEGER NOT NULL DEFAULT 0,
			state TEXT NOT NULL,
			progress INTEGER NOT NULL,
			content_hash TEXT NOT NULL DEFAULT '',
			param_version TEXT NOT NULL DEFAULT '',
			algorithm_version TEXT NOT NULL DEFAULT '',
			source_version TEXT NOT NULL DEFAULT '',
			pack_digest TEXT NOT NULL DEFAULT '',
			transitions_json TEXT NOT NULL DEFAULT '[]',
			reason TEXT NOT NULL DEFAULT '',
			updated_at_ns INTEGER NOT NULL,
			commit_seq INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS packs (
			shard_id TEXT NOT NULL,
			pack_index INTEGER NOT NULL,
			cache_key TEXT NOT NULL,
			digest TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			PRIMARY KEY(shard_id, pack_index)
		)`,
		`CREATE TABLE IF NOT EXISTS frames (
			shard_id TEXT NOT NULL,
			frame_index INTEGER NOT NULL,
			pack_index INTEGER NOT NULL,
			payload_json TEXT NOT NULL,
			PRIMARY KEY(shard_id, frame_index)
		)`,
		`CREATE TABLE IF NOT EXISTS dispatch_records (
			shard_id TEXT NOT NULL,
			frame_index INTEGER NOT NULL,
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			pack_index INTEGER NOT NULL,
			channel_id TEXT NOT NULL,
			mouth TEXT NOT NULL,
			kind TEXT NOT NULL,
			cache_key TEXT NOT NULL DEFAULT '',
			payload_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS commit_journal (
			commit_seq INTEGER PRIMARY KEY AUTOINCREMENT,
			shard_id TEXT NOT NULL,
			pack_count INTEGER NOT NULL,
			frame_count INTEGER NOT NULL,
			pack_digest TEXT NOT NULL,
			committed_at_ns INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS cache_entries (
			cache_key TEXT PRIMARY KEY,
			node_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			shard_id TEXT NOT NULL,
			source_version TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			updated_at_ns INTEGER NOT NULL
		)`,
	}
	for _, stmt := range ddl {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("init checkpoint schema: %w", err)
		}
	}
	return nil
}

func (s *Store) SaveMachine(ctx context.Context, m Machine, dialogueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO shard_checkpoints(shard_id,dialogue_id,shard_index,state,progress,reason,updated_at_ns)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(shard_id) DO UPDATE SET state=excluded.state, progress=excluded.progress,
			reason=excluded.reason, updated_at_ns=excluded.updated_at_ns`,
		m.ShardID, dialogueID, m.ShardIndex, string(m.State), m.Progress, m.Reason, time.Now().UnixNano())
	return err
}

func (s *Store) LoadMachine(ctx context.Context, shardID string) (*Machine, error) {
	row := s.db.QueryRowContext(ctx, `SELECT shard_id, shard_index, state, progress, reason FROM shard_checkpoints WHERE shard_id=?`, shardID)
	m := &Machine{}
	var state string
	if err := row.Scan(&m.ShardID, &m.ShardIndex, &state, &m.Progress, &m.Reason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	m.State = State(state)
	return m, nil
}

func (s *Store) CommitShard(ctx context.Context, result *domain.ShardResult, sourceVersion domain.SchemaVersion) (*CommitReceipt, error) {
	if result == nil {
		return nil, errors.New("commit requires shard result")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UnixNano()
	type packSummary struct {
		Index       int      `json:"index"`
		PhonemeIDs  []string `json:"phoneme_ids"`
		StartSample int64    `json:"start_sample"`
		EndSample   int64    `json:"end_sample"`
		StartFrame  int64    `json:"start_frame"`
		EndFrame    int64    `json:"end_frame"`
		Digest      string   `json:"digest"`
	}
	summaries := make([]packSummary, 0, len(result.Packs))
	for _, pack := range result.Packs {
		summaries = append(summaries, packSummary{
			Index: pack.Index, PhonemeIDs: pack.PhonemeIDs,
			StartSample: pack.StartSample, EndSample: pack.EndSample,
			StartFrame: pack.StartFrame, EndFrame: pack.EndFrame, Digest: pack.Digest,
		})
	}
	packDigest := hashx.JSON(summaries)
	transitionsJSON, err := json.Marshal(result.Transitions)
	if err != nil {
		return nil, err
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO commit_journal(shard_id,pack_count,frame_count,pack_digest,committed_at_ns)
		VALUES(?,?,?,?,?)`, result.ShardID, len(result.Packs), len(result.Frames), packDigest, now)
	if err != nil {
		return nil, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO shard_checkpoints(shard_id,dialogue_id,shard_index,state,progress,content_hash,
			param_version,algorithm_version,source_version,pack_digest,transitions_json,reason,updated_at_ns,commit_seq)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(shard_id) DO UPDATE SET dialogue_id=excluded.dialogue_id, shard_index=excluded.shard_index,
			state='committed', progress=100, content_hash=excluded.content_hash,
			param_version=excluded.param_version, algorithm_version=excluded.algorithm_version,
			source_version=excluded.source_version, pack_digest=excluded.pack_digest,
			transitions_json=excluded.transitions_json, reason='', updated_at_ns=excluded.updated_at_ns,
			commit_seq=excluded.commit_seq`,
		result.ShardID, result.DialogueID, result.ShardIndex, string(StateCommitted), 100,
		result.ContentHash, result.ParamVersion, result.AlgorithmVersion, string(sourceVersion),
		packDigest, string(transitionsJSON), "", now, seq)
	if err != nil {
		return nil, err
	}

	for _, pack := range result.Packs {
		payload, merr := json.Marshal(pack)
		if merr != nil {
			return nil, merr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO packs(shard_id,pack_index,cache_key,digest,payload_json)
			VALUES(?,?,?,?,?) ON CONFLICT(shard_id,pack_index) DO UPDATE SET
			cache_key=excluded.cache_key,digest=excluded.digest,payload_json=excluded.payload_json`,
			result.ShardID, pack.Index, pack.CacheKey, pack.Digest, string(payload))
		if err != nil {
			return nil, err
		}
	}
	for _, frame := range result.Frames {
		payload, merr := json.Marshal(frame)
		if merr != nil {
			return nil, merr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO frames(shard_id,frame_index,pack_index,payload_json)
			VALUES(?,?,?,?) ON CONFLICT(shard_id,frame_index) DO UPDATE SET
			pack_index=excluded.pack_index,payload_json=excluded.payload_json`,
			result.ShardID, frame.Index, frame.PackIndex, string(payload))
		if err != nil {
			return nil, err
		}
		record := domain.DispatchRecord{
			DialogueID: result.DialogueID, ShardID: result.ShardID, FrameIndex: frame.Index,
			PackIndex: frame.PackIndex, ChannelID: result.ChannelID, Mouth: frame.Mouth, Kind: frame.Kind,
		}
		dispatchPayload, merr := json.Marshal(record)
		if merr != nil {
			return nil, merr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO dispatch_records(shard_id,frame_index,pack_index,channel_id,mouth,kind,payload_json)
			VALUES(?,?,?,?,?,?,?)`, result.ShardID, frame.Index, frame.PackIndex, result.ChannelID,
			string(frame.Mouth), string(frame.Kind), string(dispatchPayload))
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &CommitReceipt{CommitSeq: seq, ShardID: result.ShardID, PackCount: len(result.Packs), FrameCount: len(result.Frames), PackDigest: packDigest, CommittedAt: now}, nil
}

func (s *Store) Recover(ctx context.Context, shardID, expectedHash, expectedParam, expectedAlgo string, expectedSource domain.SchemaVersion) (*Recovery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRowContext(ctx, `SELECT shard_index,state,progress,content_hash,param_version,algorithm_version,source_version,transitions_json
		FROM shard_checkpoints WHERE shard_id=?`, shardID)
	var idx int
	var state, hash, param, algo, source, transitionsJSON string
	var progress int
	if err := row.Scan(&idx, &state, &progress, &hash, &param, &algo, &source, &transitionsJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	recovery := &Recovery{ShardID: shardID, State: State(state)}
	if state != string(StateCommitted) {
		return recovery, nil
	}
	if hash != expectedHash {
		recovery.Invalidated = true
		recovery.Reasons = append(recovery.Reasons, "content_hash mismatch: checkpoint="+hash+" current="+expectedHash)
	}
	if param != expectedParam {
		recovery.Invalidated = true
		recovery.Reasons = append(recovery.Reasons, "param_version mismatch: checkpoint="+param+" current="+expectedParam)
	}
	if algo != expectedAlgo {
		recovery.Invalidated = true
		recovery.Reasons = append(recovery.Reasons, "algorithm_version mismatch: checkpoint="+algo+" current="+expectedAlgo)
	}
	if domain.SchemaVersion(source) != expectedSource {
		recovery.Invalidated = true
		recovery.Reasons = append(recovery.Reasons, "source_version mismatch: checkpoint="+source+" current="+string(expectedSource))
	}
	if recovery.Invalidated {
		if err := s.invalidateLocked(ctx, shardID, "hash/version mismatch"); err != nil {
			return nil, err
		}
		recovery.State = StatePending
		return recovery, nil
	}

	result := &domain.ShardResult{ShardID: shardID, ShardIndex: idx, ContentHash: hash, ParamVersion: param, AlgorithmVersion: algo}
	if err := json.Unmarshal([]byte(transitionsJSON), &result.Transitions); err != nil {
		return nil, err
	}
	packRows, err := s.db.QueryContext(ctx, `SELECT payload_json FROM packs WHERE shard_id=? ORDER BY pack_index`, shardID)
	if err != nil {
		return nil, err
	}
	defer packRows.Close()
	for packRows.Next() {
		var payload string
		if err := packRows.Scan(&payload); err != nil {
			return nil, err
		}
		var pack domain.Pack
		if err := json.Unmarshal([]byte(payload), &pack); err != nil {
			return nil, err
		}
		result.Packs = append(result.Packs, pack)
	}
	frameRows, err := s.db.QueryContext(ctx, `SELECT payload_json FROM frames WHERE shard_id=? ORDER BY frame_index`, shardID)
	if err != nil {
		return nil, err
	}
	defer frameRows.Close()
	for frameRows.Next() {
		var payload string
		if err := frameRows.Scan(&payload); err != nil {
			return nil, err
		}
		var frame domain.Frame
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			return nil, err
		}
		result.Frames = append(result.Frames, frame)
	}
	recovery.Result = result
	return recovery, nil
}

func (s *Store) Invalidate(ctx context.Context, shardID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidateLocked(ctx, shardID, reason)
}

func (s *Store) invalidateLocked(ctx context.Context, shardID, reason string) error {
	for _, table := range []string{"packs", "frames", "dispatch_records", "shard_checkpoints"} {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+table+" WHERE shard_id=?", shardID); err != nil {
			return err
		}
	}
	m := NewMachine(shardID, 0)
	_ = m.Transition(StateQuantizing, reason)
	_, err := s.db.ExecContext(ctx, `INSERT INTO shard_checkpoints(shard_id,state,progress,reason,updated_at_ns)
		VALUES(?,?,?,?,?)`, shardID, string(StatePending), 0, reason, time.Now().UnixNano())
	return err
}

func (s *Store) CommitOrder(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT shard_id FROM commit_journal ORDER BY commit_seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) PutCache(ctx context.Context, entry CacheEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO cache_entries(cache_key,node_id,kind,shard_id,source_version,payload_json,updated_at_ns)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(cache_key) DO UPDATE SET payload_json=excluded.payload_json,
		source_version=excluded.source_version, updated_at_ns=excluded.updated_at_ns`,
		entry.CacheKey, entry.NodeID, entry.Kind, entry.ShardID, entry.SourceVersion, entry.PayloadJSON, time.Now().UnixNano())
	return err
}

func (s *Store) GetCache(ctx context.Context, key string) (*CacheEntry, error) {
	row := s.db.QueryRowContext(ctx, `SELECT cache_key,node_id,kind,shard_id,source_version,payload_json,updated_at_ns FROM cache_entries WHERE cache_key=?`, key)
	var e CacheEntry
	if err := row.Scan(&e.CacheKey, &e.NodeID, &e.Kind, &e.ShardID, &e.SourceVersion, &e.PayloadJSON, &e.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &e, nil
}

func (s *Store) DeleteCache(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM cache_entries WHERE cache_key=?`, key)
	return err
}

func (s *Store) ListCache(ctx context.Context, shardID string) ([]CacheEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT cache_key,node_id,kind,shard_id,source_version,payload_json,updated_at_ns
		FROM cache_entries WHERE (?='' OR shard_id=?) ORDER BY node_id`, shardID, shardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CacheEntry{}
	for rows.Next() {
		var e CacheEntry
		if err := rows.Scan(&e.CacheKey, &e.NodeID, &e.Kind, &e.ShardID, &e.SourceVersion, &e.PayloadJSON, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) CorruptVersionsForTest(ctx context.Context, shardID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE shard_checkpoints
		SET content_hash=content_hash || '-corrupt', param_version=param_version || '-corrupt'
		WHERE shard_id=?`, shardID)
	return err
}
