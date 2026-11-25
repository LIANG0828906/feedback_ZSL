package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"tunnelworkbench/internal/forward"

	_ "modernc.org/sqlite"
)

const sqliteHeader = "SQLite format 3\x00"

type Meta struct {
	ProjectID     string `json:"project_id"`
	SchemaVersion string `json:"schema_version"`
	ProjectHash   string `json:"project_hash"`
	LastEventSeq  int    `json:"last_event_seq"`
}

type CacheRecord struct {
	Key                string   `json:"key"`
	EventSeq           int      `json:"event_seq"`
	Kind               string   `json:"kind"`
	Payload            any      `json:"payload"`
	Deps               []string `json:"deps,omitempty"`
	InvalidationSource string   `json:"invalidation_source,omitempty"`
}

type Recovery struct {
	Meta       Meta                       `json:"meta"`
	States     []forward.TunnelStatus     `json:"states"`
	RouteCache map[string]json.RawMessage `json:"route_cache"`
	CacheMeta  []CacheRecord              `json:"cache_meta,omitempty"`
}

type Store struct {
	db   *sql.DB
	path string
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, forward.WrapBiz(forward.CodeCheckpointOpen, "checkpoint.path", err, "cannot create checkpoint directory")
		}
		if err := preflightFile(path); err != nil {
			return nil, err
		}
	}
	dsn := path
	if path != ":memory:" {
		dsn = path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, forward.WrapBiz(forward.CodeCheckpointOpen, "checkpoint.open", err, "cannot open sqlite checkpoint")
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, forward.WrapBiz(forward.CodeCheckpointOpen, "checkpoint.ping", err, "cannot ping sqlite checkpoint")
	}
	store := &Store{db: db, path: path}
	if path != ":memory:" {
		if biz := store.checkIntegrity(ctx); biz != nil {
			_ = db.Close()
			return nil, biz
		}
	}
	if err := store.init(ctx); err != nil {
		if path != ":memory:" {
			if biz := store.checkIntegrity(ctx); biz != nil {
				_ = db.Close()
				return nil, biz
			}
		}
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func preflightFile(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return forward.WrapBiz(forward.CodeCheckpointOpen, path, err, "cannot stat checkpoint file")
	}
	if info.IsDir() {
		return forward.NewBizError(forward.CodeCheckpointOpen, path, "checkpoint path is a directory")
	}
	if info.Size() < 100 {
		return forward.NewBizError(forward.CodeCheckpointTruncated, path,
			"checkpoint file size %d is shorter than a SQLite database header/page", info.Size()).
			WithContext("size_bytes", info.Size())
	}
	if info.Size() >= 16 {
		f, err := os.Open(path)
		if err != nil {
			return forward.WrapBiz(forward.CodeCheckpointOpen, path, err, "cannot read checkpoint header")
		}
		defer f.Close()
		header := make([]byte, 16)
		if _, err := f.Read(header); err != nil {
			return forward.NewBizError(forward.CodeCheckpointTruncated, path, "cannot read SQLite header: %v", err)
		}
		if string(header) != sqliteHeader {
			return forward.NewBizError(forward.CodeCheckpointTruncated, path, "invalid SQLite header or truncated snapshot").
				WithContext("header", fmt.Sprintf("% x", header))
		}
	}
	return nil
}

func (s *Store) init(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			id INTEGER PRIMARY KEY CHECK(id=1),
			project_id TEXT NOT NULL,
			schema_version TEXT NOT NULL,
			project_hash TEXT NOT NULL,
			last_event_seq INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tunnel_state_history (
			tunnel_id TEXT NOT NULL,
			event_seq INTEGER NOT NULL,
			state TEXT NOT NULL,
			effective_at_ms INTEGER NOT NULL,
			source_event_id TEXT NOT NULL DEFAULT '',
			peer_id TEXT NOT NULL DEFAULT '',
			watermark_enabled INTEGER NOT NULL DEFAULT 0,
			degradations_json TEXT NOT NULL DEFAULT '[]',
			PRIMARY KEY(tunnel_id, event_seq)
		)`,
		`CREATE TABLE IF NOT EXISTS route_cache (
			cache_key TEXT PRIMARY KEY,
			event_seq INTEGER NOT NULL,
			kind TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			deps_json TEXT NOT NULL DEFAULT '[]',
			invalidation_source TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return forward.WrapBiz(forward.CodeCheckpointOpen, "checkpoint.schema", err, "cannot initialize checkpoint schema")
		}
	}
	return nil
}

func (s *Store) checkIntegrity(ctx context.Context) *forward.BizError {
	var result string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return forward.NewBizError(forward.CodeCheckpointTruncated, "checkpoint.integrity",
			"integrity check failed; snapshot is truncated or corrupt: %v", err)
	}
	if result != "ok" {
		return forward.NewBizError(forward.CodeCheckpointTruncated, "checkpoint.integrity",
			"integrity_check returned %q", result)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// InitializeProject inserts checkpoint identity without advancing event sequence.
func (s *Store) InitializeProject(ctx context.Context, meta Meta) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO checkpoint_meta(id, project_id, schema_version, project_hash, last_event_seq, updated_at)
		VALUES(1, ?, ?, ?, 0, ?)`,
		meta.ProjectID, meta.SchemaVersion, meta.ProjectHash, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return forward.WrapBiz(forward.CodeCheckpointWrite, "checkpoint_meta", err, "cannot initialize checkpoint identity")
	}
	return nil
}

// SaveStateBundle writes tunnel states and route cache before the global sequence is advanced.
func (s *Store) SaveStateBundle(ctx context.Context, seq int, states []forward.TunnelStatus, cache []CacheRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return forward.WrapBiz(forward.CodeCheckpointWrite, "transaction", err, "cannot begin checkpoint transaction")
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, st := range states {
		degs, _ := json.Marshal(st.Degradations)
		wm := 0
		if st.WatermarkEnabled {
			wm = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO tunnel_state_history
			(tunnel_id, event_seq, state, effective_at_ms, source_event_id, peer_id, watermark_enabled, degradations_json)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
			st.TunnelID, seq, st.State, st.EffectiveAtMs, st.SourceEventID, st.PeerID, wm, string(degs)); err != nil {
			return forward.WrapBiz(forward.CodeCheckpointWrite, "tunnel_state_history", err, "cannot write tunnel state")
		}
	}
	for _, rec := range cache {
		payload, err := json.Marshal(rec.Payload)
		if err != nil {
			return forward.WrapBiz(forward.CodeCheckpointWrite, "route_cache.payload", err, "cannot marshal cache payload")
		}
		deps, _ := json.Marshal(rec.Deps)
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO route_cache(cache_key, event_seq, kind, payload_json, deps_json, invalidation_source, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?)`,
			rec.Key, rec.EventSeq, rec.Kind, string(payload), string(deps), rec.InvalidationSource, now); err != nil {
			return forward.WrapBiz(forward.CodeCheckpointWrite, "route_cache", err, "cannot write route cache")
		}
	}
	if err := tx.Commit(); err != nil {
		return forward.WrapBiz(forward.CodeCheckpointWrite, "transaction.commit", err, "cannot commit state bundle")
	}
	return nil
}

// CommitSequence is the last write in a checkpoint. A crash before this leaves last_event_seq behind.
func (s *Store) CommitSequence(ctx context.Context, meta Meta, seq int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE checkpoint_meta
		SET project_id=?, schema_version=?, project_hash=?, last_event_seq=?, updated_at=?
		WHERE id=1`,
		meta.ProjectID, meta.SchemaVersion, meta.ProjectHash, seq,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return forward.WrapBiz(forward.CodeCheckpointWrite, "checkpoint_meta.last_event_seq", err, "cannot commit event sequence")
	}
	return nil
}

func (s *Store) ReadMeta(ctx context.Context) (Meta, error) {
	var meta Meta
	var updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT project_id, schema_version, project_hash, last_event_seq, updated_at
		FROM checkpoint_meta WHERE id=1`).
		Scan(&meta.ProjectID, &meta.SchemaVersion, &meta.ProjectHash, &meta.LastEventSeq, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Meta{LastEventSeq: 0}, nil
	}
	if err != nil {
		return Meta{}, forward.WrapBiz(forward.CodeCheckpointRead, "checkpoint_meta", err, "cannot read checkpoint meta")
	}
	return meta, nil
}

func (s *Store) LoadCommitted(ctx context.Context, expectedSchema, expectedHash string) (*Recovery, *forward.BizError) {
	meta, err := s.ReadMeta(ctx)
	if err != nil {
		return nil, forward.AsBizErrorOrInternal(err)
	}
	if meta.SchemaVersion != "" && (meta.SchemaVersion != expectedSchema || meta.ProjectHash != expectedHash) {
		return nil, forward.NewBizError(forward.CodeCheckpointVersionMismatch, "checkpoint_meta",
			"checkpoint identity %s/%s does not match requested %s/%s; full replay required",
			meta.SchemaVersion, forward.ShortHash(meta.ProjectHash), expectedSchema, forward.ShortHash(expectedHash)).
			WithContext("checkpoint_schema", meta.SchemaVersion).
			WithContext("checkpoint_hash", meta.ProjectHash).
			WithContext("expected_schema", expectedSchema).
			WithContext("expected_hash", expectedHash).
			WithContext("committed_seq", meta.LastEventSeq)
	}
	recovery := &Recovery{Meta: meta, States: []forward.TunnelStatus{}, RouteCache: map[string]json.RawMessage{}}
	rows, err := s.db.QueryContext(ctx, `
		SELECT tunnel_id, event_seq, state, effective_at_ms, source_event_id, peer_id, watermark_enabled, degradations_json
		FROM tunnel_state_history
		WHERE event_seq <= ?
		ORDER BY tunnel_id, event_seq`, meta.LastEventSeq)
	if err != nil {
		return nil, forward.WrapBiz(forward.CodeCheckpointRead, "tunnel_state_history", err, "cannot read tunnel states")
	}
	defer rows.Close()
	latest := map[string]forward.TunnelStatus{}
	for rows.Next() {
		var tunnelID, state, source, peer, degsJSON string
		var seq int
		var effective int64
		var wm int
		if err := rows.Scan(&tunnelID, &seq, &state, &effective, &source, &peer, &wm, &degsJSON); err != nil {
			return nil, forward.WrapBiz(forward.CodeCheckpointRead, "tunnel_state_history.scan", err, "cannot scan state")
		}
		degs := []string{}
		_ = json.Unmarshal([]byte(degsJSON), &degs)
		latest[tunnelID] = forward.TunnelStatus{
			TunnelID: tunnelID, PeerID: peer, State: state, EffectiveAtMs: effective,
			SourceEventID: source, LastSeq: seq, WatermarkEnabled: wm == 1, Degradations: degs,
		}
	}
	for _, st := range latest {
		recovery.States = append(recovery.States, st)
	}

	cacheRows, err := s.db.QueryContext(ctx, `
		SELECT cache_key, event_seq, kind, payload_json, deps_json, invalidation_source
		FROM route_cache WHERE event_seq <= ? ORDER BY cache_key`, meta.LastEventSeq)
	if err != nil {
		return nil, forward.WrapBiz(forward.CodeCheckpointRead, "route_cache", err, "cannot read route cache")
	}
	defer cacheRows.Close()
	for cacheRows.Next() {
		var key, kind, payload, deps, invalidation string
		var seq int
		if err := cacheRows.Scan(&key, &seq, &kind, &payload, &deps, &invalidation); err != nil {
			return nil, forward.WrapBiz(forward.CodeCheckpointRead, "route_cache.scan", err, "cannot scan cache")
		}
		recovery.RouteCache[key] = append(json.RawMessage(nil), json.RawMessage(payload)...)
		recovery.CacheMeta = append(recovery.CacheMeta, CacheRecord{
			Key: key, EventSeq: seq, Kind: kind, Deps: unmarshalDeps(deps), InvalidationSource: invalidation,
		})
	}
	return recovery, nil
}

func unmarshalDeps(raw string) []string {
	out := []string{}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}
