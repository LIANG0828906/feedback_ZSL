// Package checkpoint persists committed batch state before the global
// operation sequence. A transaction makes a forced stop at any injected point
// leave the previous committed snapshot readable.
package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"example.com/columnar/domain"
	_ "modernc.org/sqlite"
)

const (
	metaSchemaVersion = "schema_version"
	metaEngineVersion = "engine_version"
	metaCommittedOp   = "committed_op"
	metaContentDigest = "content_digest"
)

type Store struct {
	path string
	db   *sql.DB
}

type Expectation struct {
	SchemaVersion string
	EngineVersion string
	ContentDigest uint64
}

type Recovered struct {
	Data      domain.CheckpointData
	Integrity string
}

// Open validates the SQLite header before opening, then initializes the
// checkpoint schema. A malformed/truncated existing file is never treated as an
// empty database.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, domain.WrapError(domain.CodeCheckpointWrite, "checkpoint.path", err)
	}
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		if info.Size() < 100 {
			return nil, domain.Errorf(domain.CodeCheckpointTruncated, "checkpoint.file", "file is only %d bytes", info.Size())
		}
		header := make([]byte, 16)
		f, err := os.Open(path)
		if err != nil {
			return nil, domain.WrapError(domain.CodeCheckpointCorrupt, "checkpoint.file", err)
		}
		_, readErr := f.Read(header)
		_ = f.Close()
		if readErr != nil {
			return nil, domain.WrapError(domain.CodeCheckpointTruncated, "checkpoint.file", readErr)
		}
		if string(header) != "SQLite format 3\x00" {
			return nil, domain.NewError(domain.CodeCheckpointTruncated, "checkpoint.file", "invalid SQLite header")
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, classifyDBError(err, "checkpoint.open")
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, classifyDBError(err, "checkpoint.open")
	}
	store := &Store{path: path, db: db}
	if err := store.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Path() string { return s.path }

func (s *Store) init(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS committed_batches (
			batch_id INTEGER PRIMARY KEY,
			family TEXT NOT NULL,
			column_name TEXT NOT NULL,
			row_start INTEGER NOT NULL,
			row_end INTEGER NOT NULL,
			rows INTEGER NOT NULL,
			encoding TEXT NOT NULL,
			compressed_bytes INTEGER NOT NULL,
			digest TEXT NOT NULL,
			decode_buffer_bytes INTEGER NOT NULL,
			status TEXT NOT NULL,
			invalidated INTEGER NOT NULL,
			depends_json TEXT NOT NULL,
			source_id INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS scan_cache_index (
			scan_id TEXT PRIMARY KEY,
			after_op INTEGER NOT NULL,
			read_batches_json TEXT NOT NULL,
			decoded_rows INTEGER NOT NULL,
			invalid_sources_json TEXT NOT NULL
		)`,
	}
	for _, stmt := range statements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return classifyDBError(err, "checkpoint.init")
		}
	}
	return nil
}

// Save writes batch rows first, scan index rows second, and only then updates
// the global committed-op/version metadata. The whole sequence is one SQLite
// transaction. Failpoint is evaluated at named stages for crash-injection tests.
func (s *Store) Save(ctx context.Context, data domain.CheckpointData) error {
	return s.SaveWithFailpoint(ctx, data, nil)
}

func (s *Store) SaveWithFailpoint(ctx context.Context, data domain.CheckpointData, failpoint func(stage string) error) error {
	if err := data.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyDBError(err, "checkpoint.begin")
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `DELETE FROM committed_batches`); err != nil {
		return classifyDBError(err, "checkpoint.clear_batches")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM scan_cache_index`); err != nil {
		return classifyDBError(err, "checkpoint.clear_scans")
	}
	for _, b := range data.Batches {
		deps, err := json.Marshal(domain.CanonicalIntSet(b.DependsOn))
		if err != nil {
			return domain.WrapError(domain.CodeCheckpointWrite, "checkpoint.batch.depends", err)
		}
		if b.Status == "" {
			b.Status = domain.BatchActive
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO committed_batches
			(batch_id,family,column_name,row_start,row_end,rows,encoding,compressed_bytes,digest,decode_buffer_bytes,status,invalidated,depends_json,source_id)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			b.ID, b.Family, b.Column, b.RowStart, b.RowEnd, b.Rows, b.Encoding,
			b.CompressedBytes, domain.FormatDigest(b.Digest), b.DecodeBufferBytes,
			string(b.Status), boolInt(b.Invalidated), string(deps), b.SourceID)
		if err != nil {
			return classifyDBError(err, "checkpoint.write_batch")
		}
	}
	for _, sc := range data.ScanCache {
		readBatches, err := json.Marshal(domain.CanonicalIntSet(sc.ReadBatches))
		if err != nil {
			return domain.WrapError(domain.CodeCheckpointWrite, "checkpoint.scan.read_batches", err)
		}
		invalid, err := json.Marshal(domain.CanonicalIntSet(sc.InvalidSources))
		if err != nil {
			return domain.WrapError(domain.CodeCheckpointWrite, "checkpoint.scan.invalid_sources", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO scan_cache_index
			(scan_id,after_op,read_batches_json,decoded_rows,invalid_sources_json)
			VALUES (?,?,?,?,?)`,
			sc.ScanID, sc.AfterOp, string(readBatches), sc.DecodedRows, string(invalid))
		if err != nil {
			return classifyDBError(err, "checkpoint.write_scan")
		}
	}
	if failpoint != nil {
		if err := failpoint("after_batches"); err != nil {
			return domain.WrapError(domain.CodeCheckpointWrite, "checkpoint.failpoint.after_batches", err)
		}
	}
	meta := map[string]string{
		metaSchemaVersion: data.SchemaVersion,
		metaEngineVersion: data.EngineVersion,
		metaCommittedOp:   fmt.Sprintf("%d", data.CommittedOp),
		metaContentDigest: fmt.Sprintf("%d", data.ContentDigest),
	}
	for _, key := range []string{metaSchemaVersion, metaEngineVersion, metaContentDigest, metaCommittedOp} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_meta(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, meta[key]); err != nil {
			return classifyDBError(err, "checkpoint.write_meta")
		}
	}
	if failpoint != nil {
		if err := failpoint("after_meta"); err != nil {
			return domain.WrapError(domain.CodeCheckpointWrite, "checkpoint.failpoint.after_meta", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return classifyDBError(err, "checkpoint.commit")
	}
	rollback = false
	return nil
}

// Load verifies integrity and accepts only a checkpoint matching the requested
// schema/engine/content version.
func (s *Store) Load(ctx context.Context, expected Expectation) (*Recovered, error) {
	var integrity string
	if err := s.db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&integrity); err != nil {
		return nil, classifyDBError(err, "checkpoint.integrity")
	}
	if integrity != "ok" {
		return nil, domain.Errorf(domain.CodeCheckpointTruncated, "checkpoint.integrity", "quick_check returned %q", integrity)
	}
	meta, err := s.readMeta(ctx)
	if err != nil {
		return nil, err
	}
	schemaVersion, hasSchema := meta[metaSchemaVersion]
	engineVersion, hasEngine := meta[metaEngineVersion]
	committedRaw, hasCommitted := meta[metaCommittedOp]
	if !hasSchema || !hasEngine || !hasCommitted {
		var count int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM committed_batches`).Scan(&count)
		if count > 0 {
			return nil, domain.NewError(domain.CodeCheckpointTruncated, "checkpoint.meta", "batch rows exist without complete commit metadata")
		}
		return nil, domain.NewError(domain.CodeCheckpointCorrupt, "checkpoint.meta", "no committed checkpoint")
	}
	if expected.EngineVersion != "" && engineVersion != expected.EngineVersion {
		return nil, domain.Errorf(domain.CodeCheckpointVersion, "checkpoint.engine_version", "expected %q got %q", expected.EngineVersion, engineVersion)
	}
	if expected.SchemaVersion != "" && schemaVersion != expected.SchemaVersion {
		return nil, domain.Errorf(domain.CodeCheckpointVersion, "checkpoint.schema_version", "expected %q got %q", expected.SchemaVersion, schemaVersion)
	}
	var committedOp int
	if _, err := fmt.Sscan(committedRaw, &committedOp); err != nil || committedOp < 0 {
		return nil, domain.Errorf(domain.CodeCheckpointCorrupt, "checkpoint.committed_op", "invalid committed op %q", committedRaw)
	}
	var contentDigest uint64
	if raw, ok := meta[metaContentDigest]; ok {
		if _, err := fmt.Sscan(raw, &contentDigest); err != nil {
			return nil, domain.Errorf(domain.CodeCheckpointCorrupt, "checkpoint.content_digest", "invalid digest %q", raw)
		}
	}
	if expected.ContentDigest != 0 && contentDigest != expected.ContentDigest {
		return nil, domain.Errorf(domain.CodeCheckpointVersion, "checkpoint.content_digest", "expected %d got %d", expected.ContentDigest, contentDigest)
	}
	batches, err := s.readBatches(ctx)
	if err != nil {
		return nil, err
	}
	scans, err := s.readScans(ctx)
	if err != nil {
		return nil, err
	}
	return &Recovered{
		Data: domain.CheckpointData{
			SchemaVersion: schemaVersion, EngineVersion: engineVersion,
			CommittedOp: committedOp, Batches: batches, ScanCache: scans,
			ContentDigest: contentDigest,
		},
		Integrity: integrity,
	}, nil
}

func (s *Store) readMeta(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM checkpoint_meta`)
	if err != nil {
		return nil, classifyDBError(err, "checkpoint.read_meta")
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, classifyDBError(err, "checkpoint.read_meta")
		}
		out[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, classifyDBError(err, "checkpoint.read_meta")
	}
	return out, nil
}

func (s *Store) readBatches(ctx context.Context) ([]domain.Batch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT batch_id,family,column_name,row_start,row_end,rows,encoding,
		compressed_bytes,digest,decode_buffer_bytes,status,invalidated,depends_json,source_id
		FROM committed_batches ORDER BY batch_id`)
	if err != nil {
		return nil, classifyDBError(err, "checkpoint.read_batches")
	}
	defer rows.Close()
	var out []domain.Batch
	for rows.Next() {
		var b domain.Batch
		var digest string
		var status string
		var invalidated int
		var depsJSON string
		if err := rows.Scan(&b.ID, &b.Family, &b.Column, &b.RowStart, &b.RowEnd, &b.Rows,
			&b.Encoding, &b.CompressedBytes, &digest, &b.DecodeBufferBytes, &status,
			&invalidated, &depsJSON, &b.SourceID); err != nil {
			return nil, classifyDBError(err, "checkpoint.read_batches")
		}
		d, err := domain.ParseDigestHex(digest, 64)
		if err != nil {
			return nil, domain.WrapError(domain.CodeCheckpointCorrupt, "checkpoint.batch.digest", err)
		}
		b.Digest = d
		b.Status = domain.BatchStatus(status)
		b.Invalidated = invalidated != 0
		if err := json.Unmarshal([]byte(depsJSON), &b.DependsOn); err != nil {
			return nil, domain.WrapError(domain.CodeCheckpointCorrupt, "checkpoint.batch.depends", err)
		}
		if len(b.DependsOn) == 0 {
			b.DependsOn = nil
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyDBError(err, "checkpoint.read_batches")
	}
	return out, nil
}

func (s *Store) readScans(ctx context.Context) ([]domain.ScanCacheEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scan_id,after_op,read_batches_json,decoded_rows,invalid_sources_json
		FROM scan_cache_index ORDER BY scan_id`)
	if err != nil {
		return nil, classifyDBError(err, "checkpoint.read_scans")
	}
	defer rows.Close()
	var out []domain.ScanCacheEntry
	for rows.Next() {
		var sc domain.ScanCacheEntry
		var readJSON, invalidJSON string
		if err := rows.Scan(&sc.ScanID, &sc.AfterOp, &readJSON, &sc.DecodedRows, &invalidJSON); err != nil {
			return nil, classifyDBError(err, "checkpoint.read_scans")
		}
		if err := json.Unmarshal([]byte(readJSON), &sc.ReadBatches); err != nil {
			return nil, domain.WrapError(domain.CodeCheckpointCorrupt, "checkpoint.scan.read_batches", err)
		}
		if err := json.Unmarshal([]byte(invalidJSON), &sc.InvalidSources); err != nil {
			return nil, domain.WrapError(domain.CodeCheckpointCorrupt, "checkpoint.scan.invalid_sources", err)
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyDBError(err, "checkpoint.read_scans")
	}
	return out, nil
}

func classifyDBError(err error, context string) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "malformed") || strings.Contains(msg, "not a database") ||
		strings.Contains(msg, "disk image") || strings.Contains(msg, "disk i/o") ||
		strings.Contains(msg, "database is locked") {
		return domain.WrapError(domain.CodeCheckpointTruncated, context, err)
	}
	return domain.WrapError(domain.CodeCheckpointCorrupt, context, err)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
