package checkpoint

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"graphlab/internal/engine"
	"graphlab/internal/model"
)

const FormatVersion = "1"

const (
	StageShardIndexWritten = "shard_index_written"
	StageIndexRefsWritten  = "index_refs_written"
	StageCacheWritten      = "cache_written"
	StageCommitLogWritten  = "commit_log_written"
	StageMetaWritten       = "meta_written"
)

type CommitOptions struct {
	Failpoint func(stage string) error
}

type Store struct {
	path string
	db   *sql.DB
}

type CommitReport struct {
	Path           string    `json:"path"`
	CommitSeq      int       `json:"commit_seq"`
	ProjectID      string    `json:"project_id"`
	ProjectVersion int       `json:"project_version"`
	CoreDigest     string    `json:"core_digest"`
	CommittedAt    time.Time `json:"committed_at"`
	ShardRows      int       `json:"shard_rows"`
	IndexRows      int       `json:"index_rows"`
	CacheRows      int       `json:"cache_rows"`
}

type Header struct {
	FormatVersion  string `json:"format_version"`
	ProjectID      string `json:"project_id"`
	ProjectVersion int    `json:"project_version"`
	CommitSeq      int    `json:"commit_seq"`
	CoreDigest     string `json:"core_digest"`
}

func dsn(path string) string {
	return "file:" + path + "?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on"
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, model.Err(model.ECheckpointIO, "checkpoint path is empty", map[string]any{"path": path})
	}
	if info, err := os.Stat(path); err == nil && info.Size() == 0 {
		return nil, model.Err(model.ECheckpointTruncated, "checkpoint file is empty",
			map[string]any{"path": path, "size": info.Size()})
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, model.Wrap(model.ECheckpointIO, "cannot create checkpoint directory", err,
			map[string]any{"path": path})
	}
	db, err := sql.Open("sqlite3", dsn(path))
	if err != nil {
		return nil, model.Wrap(model.ECheckpointIO, "cannot open SQLite checkpoint", err,
			map[string]any{"path": path})
	}
	store := &Store{path: path, db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) init() error {
	if err := s.db.Ping(); err != nil {
		return mapSQLiteError(err, s.path, "ping")
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS commit_log (
			seq INTEGER PRIMARY KEY,
			project_version INTEGER NOT NULL,
			core_digest TEXT NOT NULL,
			committed_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS shard_index (
			shard_id TEXT PRIMARY KEY,
			shard_version INTEGER NOT NULL,
			vertex_order TEXT NOT NULL,
			edge_order TEXT NOT NULL,
			contributions TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS index_key (
			key TEXT PRIMARY KEY,
			version INTEGER NOT NULL,
			ref_shards TEXT NOT NULL,
			entities TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS cache_entry (
			cache_id TEXT PRIMARY KEY,
			query_key TEXT NOT NULL,
			query_id TEXT NOT NULL,
			result TEXT NOT NULL,
			dependencies TEXT NOT NULL,
			built_project_version INTEGER NOT NULL,
			hits INTEGER NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return mapSQLiteError(err, s.path, "create schema")
		}
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Path() string { return s.path }

func (s *Store) Commit(rt *engine.Runtime, opts CommitOptions) (CommitReport, error) {
	if rt == nil || rt.Project == nil {
		return CommitReport{}, model.Err(model.ECheckpointIO, "runtime is nil", map[string]any{"path": s.path})
	}
	projectJSON, err := json.Marshal(rt.Project)
	if err != nil {
		return CommitReport{}, model.Wrap(model.ECheckpointIO, "cannot encode project snapshot", err,
			map[string]any{"path": s.path})
	}
	coreDigest := model.CoreDigest(rt.Project)
	now := time.Now().UTC()
	report := CommitReport{
		Path: s.path, CommitSeq: rt.CommitSeq, ProjectID: rt.Project.ID,
		ProjectVersion: rt.Project.Version, CoreDigest: coreDigest, CommittedAt: now,
		ShardRows: len(rt.Index.Shards), IndexRows: len(rt.Index.Keys),
		CacheRows: len(rt.Cache.Entries),
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CommitReport{}, mapSQLiteError(err, s.path, "begin commit")
	}
	defer func() { _ = tx.Rollback() }()
	callStage := func(stage string) error {
		if opts.Failpoint == nil {
			return nil
		}
		if err := opts.Failpoint(stage); err != nil {
			return model.Wrap(model.ECheckpointIO, "checkpoint failpoint interrupted commit", err,
				map[string]any{"path": s.path, "stage": stage})
		}
		return nil
	}

	if _, err := tx.Exec(`DELETE FROM shard_index`); err != nil {
		return CommitReport{}, mapSQLiteError(err, s.path, "clear shard indexes")
	}
	shardIDs := make([]string, 0, len(rt.Index.Shards))
	for id := range rt.Index.Shards {
		shardIDs = append(shardIDs, id)
	}
	sort.Strings(shardIDs)
	for _, id := range shardIDs {
		si := rt.Index.Shards[id]
		vertexOrder, _ := json.Marshal(si.VertexOrder)
		edgeOrder, _ := json.Marshal(si.EdgeOrder)
		contribs, _ := json.Marshal(si.Contributions)
		if _, err := tx.Exec(`INSERT INTO shard_index
			(shard_id, shard_version, vertex_order, edge_order, contributions) VALUES (?,?,?,?,?)`,
			id, si.Version, string(vertexOrder), string(edgeOrder), string(contribs)); err != nil {
			return CommitReport{}, mapSQLiteError(err, s.path, "write shard index")
		}
	}
	if err := callStage(StageShardIndexWritten); err != nil {
		return CommitReport{}, err
	}

	if _, err := tx.Exec(`DELETE FROM index_key`); err != nil {
		return CommitReport{}, mapSQLiteError(err, s.path, "clear index keys")
	}
	keys := rt.Index.KeyViews()
	for _, key := range keys {
		refs, _ := json.Marshal(key.RefShards)
		entities, _ := json.Marshal(key.Entities)
		if _, err := tx.Exec(`INSERT INTO index_key (key, version, ref_shards, entities)
			VALUES (?,?,?,?)`, key.Key, key.Version, string(refs), string(entities)); err != nil {
			return CommitReport{}, mapSQLiteError(err, s.path, "write index key")
		}
	}
	if err := callStage(StageIndexRefsWritten); err != nil {
		return CommitReport{}, err
	}

	if _, err := tx.Exec(`DELETE FROM cache_entry`); err != nil {
		return CommitReport{}, mapSQLiteError(err, s.path, "clear cache entries")
	}
	for _, entry := range rt.Cache.EntriesSorted() {
		result, _ := json.Marshal(entry.Result)
		deps, _ := json.Marshal(entry.Dependencies)
		if _, err := tx.Exec(`INSERT INTO cache_entry
			(cache_id, query_key, query_id, result, dependencies, built_project_version, hits)
			VALUES (?,?,?,?,?,?,?)`,
			entry.CacheID, entry.QueryKey, entry.QueryID, string(result), string(deps),
			entry.BuiltProjectVersion, entry.Hits); err != nil {
			return CommitReport{}, mapSQLiteError(err, s.path, "write cache entry")
		}
	}
	if err := callStage(StageCacheWritten); err != nil {
		return CommitReport{}, err
	}

	if _, err := tx.Exec(`INSERT INTO commit_log (seq, project_version, core_digest, committed_at)
		VALUES (?,?,?,?)
		ON CONFLICT(seq) DO UPDATE SET project_version=excluded.project_version,
			core_digest=excluded.core_digest, committed_at=excluded.committed_at`,
		rt.CommitSeq, rt.Project.Version, coreDigest, now.Format(time.RFC3339Nano)); err != nil {
		return CommitReport{}, mapSQLiteError(err, s.path, "write commit log")
	}
	if err := callStage(StageCommitLogWritten); err != nil {
		return CommitReport{}, err
	}

	metaRows := []struct {
		key   string
		value string
	}{
		{"format_version", FormatVersion},
		{"project_id", rt.Project.ID},
		{"project_version", fmt.Sprintf("%d", rt.Project.Version)},
		{"core_digest", coreDigest},
		{"project_json", string(projectJSON)},
	}
	for _, row := range metaRows {
		if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, row.key, row.value); err != nil {
			return CommitReport{}, mapSQLiteError(err, s.path, "write commit meta")
		}
	}
	// The global commit sequence is deliberately the final metadata write.
	// Together with the surrounding transaction this makes the commit marker
	// the last durable fact of a checkpoint.
	if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('commit_seq', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprintf("%d", rt.CommitSeq)); err != nil {
		return CommitReport{}, mapSQLiteError(err, s.path, "write global commit sequence")
	}
	if err := callStage(StageMetaWritten); err != nil {
		return CommitReport{}, err
	}
	if err := tx.Commit(); err != nil {
		return CommitReport{}, mapSQLiteError(err, s.path, "commit checkpoint transaction")
	}
	return report, nil
}

func (s *Store) Header() (Header, error) {
	if err := s.quickCheck(); err != nil {
		return Header{}, err
	}
	rows, err := s.db.Query(`SELECT key, value FROM meta WHERE key IN
		('format_version','project_id','project_version','commit_seq','core_digest')`)
	if err != nil {
		return Header{}, mapSQLiteError(err, s.path, "read checkpoint header")
	}
	defer rows.Close()
	values := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return Header{}, model.Wrap(model.ECheckpointCorrupt, "cannot scan checkpoint header", err,
				map[string]any{"path": s.path})
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return Header{}, mapSQLiteError(err, s.path, "iterate checkpoint header")
	}
	if len(values) == 0 {
		return Header{}, model.Err(model.ENoCommit, "checkpoint contains no committed state", map[string]any{"path": s.path})
	}
	for _, key := range []string{"format_version", "project_id", "project_version", "commit_seq", "core_digest"} {
		if _, ok := values[key]; !ok {
			return Header{}, model.Err(model.ECheckpointCorrupt, "checkpoint header is incomplete",
				map[string]any{"path": s.path, "missing": key})
		}
	}
	var h Header
	h.FormatVersion = values["format_version"]
	h.ProjectID = values["project_id"]
	h.CoreDigest = values["core_digest"]
	if _, err := fmt.Sscan(values["project_version"], &h.ProjectVersion); err != nil {
		return Header{}, model.Err(model.ECheckpointCorrupt, "project version header is not an integer",
			map[string]any{"path": s.path, "value": values["project_version"]})
	}
	if _, err := fmt.Sscan(values["commit_seq"], &h.CommitSeq); err != nil {
		return Header{}, model.Err(model.ECheckpointCorrupt, "commit sequence header is not an integer",
			map[string]any{"path": s.path, "value": values["commit_seq"]})
	}
	return h, nil
}

func (s *Store) quickCheck() error {
	var result string
	if err := s.db.QueryRow(`PRAGMA quick_check`).Scan(&result); err != nil {
		return mapSQLiteError(err, s.path, "quick check")
	}
	if result != "ok" {
		return model.Err(model.ECheckpointCorrupt, "SQLite quick check failed",
			map[string]any{"path": s.path, "result": result})
	}
	return nil
}

func (s *Store) Recover(expectedProjectID string, expectedVersion int, opts engine.BuildOptions) (*engine.Runtime, Header, error) {
	if err := opts.Validate(); err != nil {
		return nil, Header{}, err
	}
	h, err := s.Header()
	if err != nil {
		return nil, Header{}, err
	}
	if h.FormatVersion != FormatVersion {
		return nil, h, model.Err(model.ECheckpointVersionMismatch, "checkpoint format version mismatch",
			map[string]any{"path": s.path, "expected": FormatVersion, "actual": h.FormatVersion})
	}
	if expectedProjectID == "" || h.ProjectID != expectedProjectID {
		return nil, h, model.Err(model.ECheckpointVersionMismatch, "checkpoint project id mismatch",
			map[string]any{"path": s.path, "expected": expectedProjectID, "actual": h.ProjectID})
	}
	if expectedVersion <= 0 || h.ProjectVersion != expectedVersion {
		return nil, h, model.Err(model.ECheckpointVersionMismatch, "checkpoint project version mismatch",
			map[string]any{"path": s.path, "expected": expectedVersion, "actual": h.ProjectVersion})
	}
	var projectJSON string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key='project_json'`).Scan(&projectJSON); err != nil {
		return nil, h, mapSQLiteError(err, s.path, "read project snapshot")
	}
	var project model.Project
	if err := json.Unmarshal([]byte(projectJSON), &project); err != nil {
		return nil, h, model.Wrap(model.ECheckpointCorrupt, "project snapshot JSON is corrupt", err,
			map[string]any{"path": s.path})
	}
	if project.ID != h.ProjectID || project.Version != h.ProjectVersion {
		return nil, h, model.Err(model.ECheckpointCorrupt, "project snapshot disagrees with checkpoint header",
			map[string]any{"path": s.path, "header_project": h.ProjectID, "snapshot_project": project.ID})
	}
	if model.CoreDigest(&project) != h.CoreDigest {
		return nil, h, model.Err(model.ECheckpointCorrupt, "project snapshot digest does not match header",
			map[string]any{"path": s.path})
	}
	if err := validateCommitLog(s, h); err != nil {
		return nil, h, err
	}
	indexStore, err := loadIndexStore(s)
	if err != nil {
		return nil, h, err
	}
	cacheStore, err := loadCacheStore(s)
	if err != nil {
		return nil, h, err
	}
	rt := &engine.Runtime{
		Project: &project, Options: opts, Index: indexStore, Cache: cacheStore,
		CommitSeq: h.CommitSeq,
	}
	return rt, h, nil
}

func validateCommitLog(s *Store, h Header) error {
	var row struct {
		seq            int
		projectVersion int
		coreDigest     string
	}
	err := s.db.QueryRow(`SELECT seq, project_version, core_digest FROM commit_log WHERE seq=?`, h.CommitSeq).
		Scan(&row.seq, &row.projectVersion, &row.coreDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Err(model.ECheckpointCorrupt, "global commit sequence has no commit log marker",
			map[string]any{"path": s.path, "commit_seq": h.CommitSeq})
	}
	if err != nil {
		return mapSQLiteError(err, s.path, "read commit log marker")
	}
	if row.projectVersion != h.ProjectVersion || row.coreDigest != h.CoreDigest {
		return model.Err(model.ECheckpointCorrupt, "commit log marker disagrees with header",
			map[string]any{"path": s.path, "commit_seq": h.CommitSeq})
	}
	return nil
}

func loadIndexStore(s *Store) (*engine.IndexStore, error) {
	store := &engine.IndexStore{
		Shards: make(map[string]*engine.ShardIndex),
		Keys:   make(map[string]*engine.IndexKey),
	}
	rows, err := s.db.Query(`SELECT shard_id, shard_version, vertex_order, edge_order, contributions FROM shard_index`)
	if err != nil {
		return nil, mapSQLiteError(err, s.path, "read shard indexes")
	}
	defer rows.Close()
	for rows.Next() {
		var id, vertexJSON, edgeJSON, contribJSON string
		var version int
		if err := rows.Scan(&id, &version, &vertexJSON, &edgeJSON, &contribJSON); err != nil {
			return nil, model.Wrap(model.ECheckpointCorrupt, "cannot scan shard index", err,
				map[string]any{"path": s.path, "shard_id": id})
		}
		si := &engine.ShardIndex{ID: id, Version: version}
		if err := json.Unmarshal([]byte(vertexJSON), &si.VertexOrder); err != nil {
			return nil, model.Wrap(model.ECheckpointCorrupt, "shard vertex order JSON is corrupt", err,
				map[string]any{"path": s.path, "shard_id": id})
		}
		if err := json.Unmarshal([]byte(edgeJSON), &si.EdgeOrder); err != nil {
			return nil, model.Wrap(model.ECheckpointCorrupt, "shard edge order JSON is corrupt", err,
				map[string]any{"path": s.path, "shard_id": id})
		}
		if err := json.Unmarshal([]byte(contribJSON), &si.Contributions); err != nil {
			return nil, model.Wrap(model.ECheckpointCorrupt, "shard contributions JSON is corrupt", err,
				map[string]any{"path": s.path, "shard_id": id})
		}
		if si.Contributions == nil {
			si.Contributions = make(map[string][]string)
		}
		store.Shards[id] = si
	}
	if err := rows.Err(); err != nil {
		return nil, mapSQLiteError(err, s.path, "iterate shard indexes")
	}
	keyRows, err := s.db.Query(`SELECT key, version, ref_shards, entities FROM index_key`)
	if err != nil {
		return nil, mapSQLiteError(err, s.path, "read index keys")
	}
	defer keyRows.Close()
	for keyRows.Next() {
		var key, refsJSON, entitiesJSON string
		var version int
		if err := keyRows.Scan(&key, &version, &refsJSON, &entitiesJSON); err != nil {
			return nil, model.Wrap(model.ECheckpointCorrupt, "cannot scan index key", err,
				map[string]any{"path": s.path, "key": key})
		}
		ik := &engine.IndexKey{Key: key, Version: version}
		if err := json.Unmarshal([]byte(refsJSON), &ik.RefShards); err != nil {
			return nil, model.Wrap(model.ECheckpointCorrupt, "index key ref shards JSON is corrupt", err,
				map[string]any{"path": s.path, "key": key})
		}
		if err := json.Unmarshal([]byte(entitiesJSON), &ik.Entities); err != nil {
			return nil, model.Wrap(model.ECheckpointCorrupt, "index key entities JSON is corrupt", err,
				map[string]any{"path": s.path, "key": key})
		}
		store.Keys[key] = ik
	}
	if err := keyRows.Err(); err != nil {
		return nil, mapSQLiteError(err, s.path, "iterate index keys")
	}
	for _, si := range store.Shards {
		for key := range si.Contributions {
			if store.Keys[key] == nil {
				return nil, model.Err(model.ECheckpointCorrupt, "shard contribution references a missing index key",
					map[string]any{"path": s.path, "shard_id": si.ID, "key": key})
			}
		}
	}
	return store, nil
}

func loadCacheStore(s *Store) (*engine.CacheStore, error) {
	store := engine.NewCacheStore()
	rows, err := s.db.Query(`SELECT cache_id, query_key, query_id, result, dependencies, built_project_version, hits
		FROM cache_entry`)
	if err != nil {
		return nil, mapSQLiteError(err, s.path, "read cache entries")
	}
	defer rows.Close()
	for rows.Next() {
		var cacheID, queryKey, queryID, resultJSON, depsJSON string
		var builtVersion, hits int
		if err := rows.Scan(&cacheID, &queryKey, &queryID, &resultJSON, &depsJSON, &builtVersion, &hits); err != nil {
			return nil, model.Wrap(model.ECheckpointCorrupt, "cannot scan cache entry", err,
				map[string]any{"path": s.path, "cache_id": cacheID})
		}
		entry := &engine.CacheEntry{
			CacheID: cacheID, QueryKey: queryKey, QueryID: queryID,
			BuiltProjectVersion: builtVersion, Hits: hits,
		}
		if err := json.Unmarshal([]byte(resultJSON), &entry.Result); err != nil {
			return nil, model.Wrap(model.ECheckpointCorrupt, "cache result JSON is corrupt", err,
				map[string]any{"path": s.path, "cache_id": cacheID})
		}
		if err := json.Unmarshal([]byte(depsJSON), &entry.Dependencies); err != nil {
			return nil, model.Wrap(model.ECheckpointCorrupt, "cache dependency JSON is corrupt", err,
				map[string]any{"path": s.path, "cache_id": cacheID})
		}
		store.Entries[cacheID] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, mapSQLiteError(err, s.path, "iterate cache entries")
	}
	return store, nil
}

func mapSQLiteError(err error, path, stage string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	code := model.ECheckpointIO
	switch {
	case errors.Is(err, os.ErrNotExist):
		code = model.ECheckpointTruncated
	case containsAny(msg, "not a database", "malformed", "database disk image is malformed", "file is encrypted"):
		code = model.ECheckpointTruncated
	case containsAny(msg, "no such table", "no such column"):
		code = model.ECheckpointCorrupt
	}
	return model.Wrap(code, "SQLite checkpoint operation failed", err,
		map[string]any{"path": path, "stage": stage})
}

func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if needle != "" && len(needle) <= len(s) && (s == needle || len(s) > len(needle) && indexOfFold(s, needle) >= 0) {
			return true
		}
	}
	return false
}

func indexOfFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a, b := s[i+j], sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
