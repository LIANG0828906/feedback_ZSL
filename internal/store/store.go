package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	VersionPending   = "pending"
	VersionCommitted = "committed"
	VersionDeleting  = "deleting"
	VersionDeleted   = "deleted"

	SessionActive     = "active"
	SessionCompleting = "completing"
	SessionCompleted  = "completed"
	SessionAborting   = "aborting"
	SessionAborted    = "aborted"
	SessionReaping    = "reaping"
	SessionReaped     = "reaped"
)

type ObjectMetadata struct {
	ContentType        string            `json:"content_type,omitempty"`
	ContentEncoding    string            `json:"content_encoding,omitempty"`
	ContentDisposition string            `json:"content_disposition,omitempty"`
	CacheControl       string            `json:"cache_control,omitempty"`
	Expires            string            `json:"expires,omitempty"`
	User               map[string]string `json:"user,omitempty"`
}

func (m ObjectMetadata) Encode() string {
	b, _ := json.Marshal(m)
	return string(b)
}

func DecodeMetadata(s string) ObjectMetadata {
	var m ObjectMetadata
	_ = json.Unmarshal([]byte(s), &m)
	if m.User == nil {
		m.User = map[string]string{}
	}
	return m
}

type Version struct {
	ID           int64
	VersionID    string
	Bucket       string
	Key          string
	Kind         string
	State        string
	BackendKey   string
	ETag         string
	Size         int64
	ContentType  string
	StorageClass string
	ACL          string
	MetadataJSON string
	PartsJSON    string
	CreatedAt    int64
	CommittedAt  int64
	DeletedAt    int64
}

func (v Version) IsDeleteMarker() bool { return v.Kind == "delete" }

type Session struct {
	ID           int64
	UploadID     string
	Bucket       string
	Key          string
	Status       string
	ContentType  string
	StorageClass string
	ACL          string
	MetadataJSON string
	CreatedAt    int64
	UpdatedAt    int64
	CompletedAt  int64
}

type Part struct {
	SessionID  int64
	PartNumber int
	ETag       string
	Size       int64
	BackendKey string
	CreatedAt  int64
}

type ReclaimRecord struct {
	ID          int64  `json:"-"`
	SessionID   string `json:"upload_id"`
	Bytes       int64  `json:"bytes"`
	Reason      string `json:"reason"`
	Status      string `json:"status"`
	ReclaimedAt int64  `json:"reclaimed_at_unix_ms"`
}

type Counts struct {
	Current    int64 `json:"current"`
	Historical int64 `json:"historical"`
}

type MultipartStats struct {
	Sessions int64 `json:"sessions"`
	Bytes    int64 `json:"bytes"`
}

type Store struct {
	db   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("empty database path")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS buckets (
    name TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS versions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id TEXT NOT NULL UNIQUE,
    bucket TEXT NOT NULL,
    object_key TEXT NOT NULL,
    kind TEXT NOT NULL,
    state TEXT NOT NULL,
    backend_key TEXT NOT NULL DEFAULT '',
    etag TEXT NOT NULL DEFAULT '',
    size INTEGER NOT NULL DEFAULT 0,
    content_type TEXT NOT NULL DEFAULT '',
    storage_class TEXT NOT NULL DEFAULT 'STANDARD',
    acl TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    parts_json TEXT NOT NULL DEFAULT '[]',
    created_at INTEGER NOT NULL,
    committed_at INTEGER NOT NULL DEFAULT 0,
    deleted_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS versions_key_idx ON versions(bucket, object_key, state, id DESC);
CREATE INDEX IF NOT EXISTS versions_bucket_idx ON versions(bucket, state, object_key, id DESC);
CREATE TABLE IF NOT EXISTS multipart_sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    upload_id TEXT NOT NULL UNIQUE,
    bucket TEXT NOT NULL,
    object_key TEXT NOT NULL,
    status TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    storage_class TEXT NOT NULL DEFAULT 'STANDARD',
    acl TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS sessions_status_idx ON multipart_sessions(status, created_at);
CREATE TABLE IF NOT EXISTS multipart_parts (
    session_id INTEGER NOT NULL,
    part_number INTEGER NOT NULL,
    etag TEXT NOT NULL,
    size INTEGER NOT NULL,
    backend_key TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(session_id, part_number),
    FOREIGN KEY(session_id) REFERENCES multipart_sessions(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS reclaim_ledger (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    bucket TEXT NOT NULL,
    upload_id TEXT NOT NULL,
    bytes INTEGER NOT NULL,
    reason TEXT NOT NULL,
    status TEXT NOT NULL,
    reclaimed_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS reclaim_bucket_idx ON reclaim_ledger(bucket, reclaimed_at DESC);
`
	_, err := s.db.Exec(schema)
	return err
}

func (s *Store) EnsureBucket(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO buckets(name, created_at) VALUES(?, ?)`, name, time.Now().UnixNano())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) BucketExists(ctx context.Context, name string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM buckets WHERE name=?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) ListBuckets(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM buckets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func (s *Store) BucketHasData(ctx context.Context, name string) (bool, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM versions WHERE bucket=?`, name).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM multipart_sessions WHERE bucket=? AND status IN (?,?,?,?)`,
		name, SessionActive, SessionCompleting, SessionAborting, SessionReaping).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) DeleteBucket(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM buckets WHERE name=?`, name)
	return err
}

func formatVersionID(id int64) string {
	return fmt.Sprintf("v%020d", id)
}

func (s *Store) ReserveVersion(ctx context.Context, bucket, key, kind string, meta ObjectMetadata, storageClass, acl string) (*Version, error) {
	now := time.Now().UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
INSERT INTO versions(version_id, bucket, object_key, kind, state, content_type, storage_class, acl, metadata_json, created_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fmt.Sprintf("pending-%d", now), bucket, key, kind, VersionPending,
		meta.ContentType, storageClass, acl, meta.Encode(), now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	vid := formatVersionID(id)
	backendKey := ""
	if kind == "object" {
		backendKey = ObjectBackendKey(bucket, vid)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE versions SET version_id=?, backend_key=? WHERE id=?`, vid, backendKey, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Version{
		ID: id, VersionID: vid, Bucket: bucket, Key: key, Kind: kind, State: VersionPending,
		BackendKey: backendKey, ContentType: meta.ContentType, StorageClass: storageClass, ACL: acl,
		MetadataJSON: meta.Encode(), PartsJSON: "[]", CreatedAt: now,
	}, nil
}

func ObjectBackendKey(bucket, versionID string) string {
	return ".s3gw/v1/objects/" + bucket + "/" + versionID
}

func UploadPartBackendKey(uploadID string, partNumber int) string {
	return ".s3gw/v1/uploads/" + uploadID + "/parts/" + fmt.Sprintf("%05d", partNumber)
}

func (s *Store) CommitVersion(ctx context.Context, id int64, etag string, size int64, partsJSON string) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE versions SET state=?, etag=?, size=?, parts_json=?, committed_at=?
WHERE id=? AND state=?`, VersionCommitted, etag, size, partsJSON, time.Now().UnixNano(), id, VersionPending)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("version %d is no longer pending", id)
	}
	return nil
}

func (s *Store) FailVersion(ctx context.Context, id int64) {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM versions WHERE id=? AND state=?`, id, VersionPending)
}

func (s *Store) GetCurrentVersion(ctx context.Context, bucket, key string) (*Version, error) {
	return s.scanVersion(s.db.QueryRowContext(ctx, `
SELECT id, version_id, bucket, object_key, kind, state, backend_key, etag, size,
       content_type, storage_class, acl, metadata_json, parts_json, created_at, committed_at, deleted_at
FROM versions WHERE bucket=? AND object_key=? AND state=? ORDER BY id DESC LIMIT 1`,
		bucket, key, VersionCommitted))
}

func (s *Store) GetVersion(ctx context.Context, bucket, key, versionID string) (*Version, error) {
	return s.scanVersion(s.db.QueryRowContext(ctx, `
SELECT id, version_id, bucket, object_key, kind, state, backend_key, etag, size,
       content_type, storage_class, acl, metadata_json, parts_json, created_at, committed_at, deleted_at
FROM versions WHERE bucket=? AND object_key=? AND version_id=?`,
		bucket, key, versionID))
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanVersion(row rowScanner) (*Version, error) {
	var v Version
	err := row.Scan(&v.ID, &v.VersionID, &v.Bucket, &v.Key, &v.Kind, &v.State, &v.BackendKey,
		&v.ETag, &v.Size, &v.ContentType, &v.StorageClass, &v.ACL, &v.MetadataJSON,
		&v.PartsJSON, &v.CreatedAt, &v.CommittedAt, &v.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *Store) BeginDeleteVersion(ctx context.Context, bucket, key, versionID string) (*Version, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	v, err := s.scanVersion(tx.QueryRowContext(ctx, `
SELECT id, version_id, bucket, object_key, kind, state, backend_key, etag, size,
       content_type, storage_class, acl, metadata_json, parts_json, created_at, committed_at, deleted_at
FROM versions WHERE bucket=? AND object_key=? AND version_id=?`, bucket, key, versionID))
	if err != nil {
		return nil, false, err
	}
	if v == nil || v.State == VersionPending {
		return nil, false, nil
	}
	if v.State == VersionDeleted {
		return v, true, tx.Commit()
	}
	if v.State == VersionDeleting {
		return v, true, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE versions SET state=?, deleted_at=? WHERE id=? AND state=?`,
		VersionDeleting, time.Now().UnixNano(), v.ID, VersionCommitted); err != nil {
		return nil, false, err
	}
	v.State = VersionDeleting
	return v, false, tx.Commit()
}

func (s *Store) FinishDeleteVersion(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE versions SET state=? WHERE id=? AND state IN (?,?)`,
		VersionDeleted, id, VersionDeleting, VersionCommitted)
	return err
}

func (s *Store) CancelDeleteVersion(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE versions SET state=?, deleted_at=0 WHERE id=? AND state=?`,
		VersionCommitted, id, VersionDeleting)
	return err
}

func (s *Store) ListVersions(ctx context.Context, bucket, prefix, keyMarker, versionMarker string, max int) ([]Version, bool, error) {
	if max < 0 {
		max = 1000
	}
	var markerID int64
	if keyMarker != "" && versionMarker != "" {
		_ = s.db.QueryRowContext(ctx, `SELECT id FROM versions WHERE bucket=? AND object_key=? AND version_id=?`,
			bucket, keyMarker, versionMarker).Scan(&markerID)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, version_id, bucket, object_key, kind, state, backend_key, etag, size,
       content_type, storage_class, acl, metadata_json, parts_json, created_at, committed_at, deleted_at
FROM versions
WHERE bucket=? AND state=? AND (?='' OR object_key LIKE ? ESCAPE '\')
ORDER BY object_key ASC, id DESC`, bucket, VersionCommitted, prefix, escapeLike(prefix)+"%")
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var all []Version
	for rows.Next() {
		v, err := s.scanVersion(rows)
		if err != nil {
			return nil, false, err
		}
		if keyMarker != "" {
			if v.Key < keyMarker {
				continue
			}
			if v.Key == keyMarker {
				if versionMarker != "" {
					if markerID == 0 || v.ID >= markerID {
						continue
					}
				} else {
					continue
				}
			}
		}
		all = append(all, *v)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(all) > max
	if truncated {
		all = all[:max]
	}
	return all, truncated, nil
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

func (s *Store) LatestVersionIDs(ctx context.Context, bucket string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT object_key, version_id FROM versions v
WHERE bucket=? AND state=? AND id=(SELECT MAX(id) FROM versions x WHERE x.bucket=v.bucket AND x.object_key=v.object_key AND x.state=?)`,
		bucket, VersionCommitted, VersionCommitted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, version string
		if err := rows.Scan(&key, &version); err != nil {
			return nil, err
		}
		out[key] = version
	}
	return out, rows.Err()
}

type ObjectRow struct {
	Version
	IsLatest bool
}

func (s *Store) ListCurrentObjects(ctx context.Context, bucket, prefix, startAfter string, max int) ([]ObjectRow, bool, error) {
	if max < 0 {
		max = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, version_id, bucket, object_key, kind, state, backend_key, etag, size,
       content_type, storage_class, acl, metadata_json, parts_json, created_at, committed_at, deleted_at
FROM versions v
WHERE bucket=? AND state=? AND kind='object'
  AND id=(SELECT MAX(id) FROM versions x WHERE x.bucket=v.bucket AND x.object_key=v.object_key AND x.state=?)
  AND (?='' OR object_key LIKE ? ESCAPE '\')
  AND (?='' OR object_key > ?)
ORDER BY object_key ASC`, bucket, VersionCommitted, VersionCommitted, prefix, escapeLike(prefix)+"%", startAfter, startAfter)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []ObjectRow
	for rows.Next() {
		v, err := s.scanVersion(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, ObjectRow{Version: *v, IsLatest: true})
		if len(out) > max {
			return out[:max], true, nil
		}
	}
	return out, false, rows.Err()
}

func (s *Store) CreateSession(ctx context.Context, uploadID, bucket, key string, meta ObjectMetadata, storageClass, acl string) (*Session, error) {
	now := time.Now().UnixNano()
	return s.CreateSessionAt(ctx, uploadID, bucket, key, meta, storageClass, acl, now)
}

func (s *Store) CreateSessionAt(ctx context.Context, uploadID, bucket, key string, meta ObjectMetadata, storageClass, acl string, createdAt int64) (*Session, error) {
	now := time.Now().UnixNano()
	res, err := s.db.ExecContext(ctx, `
INSERT INTO multipart_sessions(upload_id, bucket, object_key, status, content_type, storage_class, acl, metadata_json, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uploadID, bucket, key, SessionActive, meta.ContentType, storageClass, acl, meta.Encode(), createdAt, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Session{ID: id, UploadID: uploadID, Bucket: bucket, Key: key, Status: SessionActive,
		ContentType: meta.ContentType, StorageClass: storageClass, ACL: acl, MetadataJSON: meta.Encode(),
		CreatedAt: createdAt, UpdatedAt: now}, nil
}

func (s *Store) GetSession(ctx context.Context, uploadID string) (*Session, error) {
	var x Session
	err := s.db.QueryRowContext(ctx, `
SELECT id, upload_id, bucket, object_key, status, content_type, storage_class, acl, metadata_json, created_at, updated_at, completed_at
FROM multipart_sessions WHERE upload_id=?`, uploadID).Scan(&x.ID, &x.UploadID, &x.Bucket, &x.Key, &x.Status,
		&x.ContentType, &x.StorageClass, &x.ACL, &x.MetadataJSON, &x.CreatedAt, &x.UpdatedAt, &x.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func (s *Store) ClaimSession(ctx context.Context, uploadID, from, to string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE multipart_sessions SET status=?, updated_at=? WHERE upload_id=? AND status=?`,
		to, time.Now().UnixNano(), uploadID, from)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) SetSessionStatus(ctx context.Context, uploadID, from, to string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE multipart_sessions SET status=?, updated_at=? WHERE upload_id=? AND status=?`,
		to, time.Now().UnixNano(), uploadID, from)
	return err
}

func (s *Store) PutPart(ctx context.Context, sessionID int64, partNumber int, etag string, size int64, backendKey string) error {
	return s.PutPartAt(ctx, sessionID, partNumber, etag, size, backendKey, time.Now().UnixNano())
}

func (s *Store) PutPartAt(ctx context.Context, sessionID int64, partNumber int, etag string, size int64, backendKey string, createdAt int64) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO multipart_parts(session_id, part_number, etag, size, backend_key, created_at)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id, part_number) DO UPDATE SET
  etag=excluded.etag, size=excluded.size, backend_key=excluded.backend_key, created_at=excluded.created_at`,
		sessionID, partNumber, etag, size, backendKey, createdAt)
	return err
}

func (s *Store) ListParts(ctx context.Context, sessionID int64) ([]Part, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT session_id, part_number, etag, size, backend_key, created_at
FROM multipart_parts WHERE session_id=? ORDER BY part_number ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Part
	for rows.Next() {
		var p Part
		if err := rows.Scan(&p.SessionID, &p.PartNumber, &p.ETag, &p.Size, &p.BackendKey, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetPart(ctx context.Context, sessionID int64, partNumber int) (*Part, error) {
	var p Part
	err := s.db.QueryRowContext(ctx, `
SELECT session_id, part_number, etag, size, backend_key, created_at
FROM multipart_parts WHERE session_id=? AND part_number=?`, sessionID, partNumber).
		Scan(&p.SessionID, &p.PartNumber, &p.ETag, &p.Size, &p.BackendKey, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) DeletePartRecords(ctx context.Context, sessionID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM multipart_parts WHERE session_id=?`, sessionID)
	return err
}

func (s *Store) FinalizeMultipart(ctx context.Context, versionID int64, etag string, size int64, partsJSON string, sessionID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
UPDATE versions SET state=?, etag=?, size=?, parts_json=?, committed_at=?
WHERE id=? AND state=?`, VersionCommitted, etag, size, partsJSON, time.Now().UnixNano(), versionID, VersionPending)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("version %d is no longer pending", versionID)
	}
	res, err = tx.ExecContext(ctx, `UPDATE multipart_sessions SET status=?, completed_at=?, updated_at=? WHERE id=? AND status=?`,
		SessionCompleted, time.Now().UnixNano(), time.Now().UnixNano(), sessionID, SessionCompleting)
	if err != nil {
		return err
	}
	n, _ = res.RowsAffected()
	if n != 1 {
		return errors.New("multipart session is no longer completing")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM multipart_parts WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishAbort(ctx context.Context, sessionID int64, uploadID string) (int64, error) {
	return s.finishCleanup(ctx, sessionID, uploadID, SessionAborting, SessionAborted, "abort", false)
}

func (s *Store) FinishReap(ctx context.Context, sessionID int64, uploadID string) (int64, error) {
	return s.finishCleanup(ctx, sessionID, uploadID, SessionReaping, SessionReaped, "expired", true)
}

func (s *Store) finishCleanup(ctx context.Context, sessionID int64, uploadID, from, to, reason string, recordLedger bool) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var bytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM multipart_parts WHERE session_id=?`, sessionID).Scan(&bytes); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE multipart_sessions SET status=?, updated_at=? WHERE id=? AND status=?`,
		to, time.Now().UnixNano(), sessionID, from)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return 0, fmt.Errorf("session %d is not in %s state", sessionID, from)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM multipart_parts WHERE session_id=?`, sessionID); err != nil {
		return 0, err
	}
	if recordLedger {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO reclaim_ledger(bucket, upload_id, bytes, reason, status, reclaimed_at)
SELECT bucket, upload_id, ?, ?, 'success', ? FROM multipart_sessions WHERE id=?`,
			bytes, reason, time.Now().UnixNano(), sessionID); err != nil {
			return 0, err
		}
	}
	return bytes, tx.Commit()
}

func (s *Store) ExpiredSessions(ctx context.Context, cutoff int64) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, upload_id, bucket, object_key, status, content_type, storage_class, acl, metadata_json, created_at, updated_at, completed_at
FROM multipart_sessions WHERE status=? AND created_at < ? ORDER BY created_at ASC`, SessionActive, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var x Session
		if err := rows.Scan(&x.ID, &x.UploadID, &x.Bucket, &x.Key, &x.Status, &x.ContentType,
			&x.StorageClass, &x.ACL, &x.MetadataJSON, &x.CreatedAt, &x.UpdatedAt, &x.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) VersionCounts(ctx context.Context, bucket string) (Counts, error) {
	var c Counts
	where := ""
	args := []any{VersionCommitted}
	if bucket != "" {
		where = " AND bucket=?"
		args = append(args, bucket)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM versions WHERE state=?`+where, args...).Scan(&c.Current); err != nil {
		return c, err
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM versions WHERE state=?`+where, args...).Scan(&total); err != nil {
		return c, err
	}
	currentQuery := `SELECT COUNT(*) FROM (SELECT object_key FROM versions WHERE state=?` + where + ` GROUP BY object_key)`
	if err := s.db.QueryRowContext(ctx, currentQuery, args...).Scan(&c.Current); err != nil {
		return c, err
	}
	c.Historical = total - c.Current
	if c.Historical < 0 {
		c.Historical = 0
	}
	return c, nil
}

func (s *Store) MultipartStats(ctx context.Context, bucket string) (MultipartStats, error) {
	var x MultipartStats
	where := " AND status IN ('active','completing','aborting','reaping')"
	args := []any{}
	if bucket != "" {
		where += " AND bucket=?"
		args = append(args, bucket)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM multipart_sessions WHERE 1=1`+where, args...).Scan(&x.Sessions); err != nil {
		return x, err
	}
	if err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(p.size),0) FROM multipart_parts p
JOIN multipart_sessions s ON s.id=p.session_id
WHERE s.status IN ('active','completing','aborting','reaping')`+
		func() string {
			if bucket != "" {
				return " AND s.bucket=?"
			}
			return ""
		}(), args...).Scan(&x.Bytes); err != nil {
		return x, err
	}
	return x, nil
}

func (s *Store) OldestIncompleteAge(ctx context.Context, bucket string) (time.Duration, bool, error) {
	q := `SELECT MIN(created_at) FROM multipart_sessions WHERE status IN ('active','completing','aborting','reaping')`
	args := []any{}
	if bucket != "" {
		q += ` AND bucket=?`
		args = append(args, bucket)
	}
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&oldest); err != nil {
		return 0, false, err
	}
	if !oldest.Valid {
		return 0, false, nil
	}
	age := time.Since(time.Unix(0, oldest.Int64))
	if age < 0 {
		age = 0
	}
	return age, true, nil
}

func (s *Store) ReclaimTotal(ctx context.Context, bucket string) (int64, error) {
	q := `SELECT COALESCE(SUM(bytes),0) FROM reclaim_ledger WHERE status='success' AND reason='expired'`
	args := []any{}
	if bucket != "" {
		q += ` AND bucket=?`
		args = append(args, bucket)
	}
	var total int64
	return total, s.db.QueryRowContext(ctx, q, args...).Scan(&total)
}

func (s *Store) RecentReclaims(ctx context.Context, bucket string, limit int) ([]ReclaimRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT id, upload_id, bytes, reason, status, reclaimed_at FROM reclaim_ledger WHERE reason='expired'`
	args := []any{}
	if bucket != "" {
		q += ` AND bucket=?`
		args = append(args, bucket)
	}
	q += ` ORDER BY reclaimed_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReclaimRecord
	for rows.Next() {
		var r ReclaimRecord
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Bytes, &r.Reason, &r.Status, &r.ReclaimedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) BucketsWithIncompleteSessions(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT bucket FROM multipart_sessions WHERE status IN ('active','completing','aborting','reaping') ORDER BY bucket`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) ListIncompleteSessions(ctx context.Context, bucket, prefix, keyMarker string, max int) ([]Session, bool, error) {
	if max < 0 {
		max = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, upload_id, bucket, object_key, status, content_type, storage_class, acl, metadata_json, created_at, updated_at, completed_at
FROM multipart_sessions
WHERE bucket=? AND status IN (?,?,?,?)
  AND (?='' OR object_key LIKE ? ESCAPE '\')
  AND (?='' OR object_key > ? OR (object_key = ? AND upload_id > ?))
ORDER BY object_key ASC, upload_id ASC`,
		bucket, SessionActive, SessionCompleting, SessionAborting, SessionReaping,
		prefix, escapeLike(prefix)+"%", keyMarker, keyMarker, keyMarker, keyMarker)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var x Session
		if err := rows.Scan(&x.ID, &x.UploadID, &x.Bucket, &x.Key, &x.Status, &x.ContentType,
			&x.StorageClass, &x.ACL, &x.MetadataJSON, &x.CreatedAt, &x.UpdatedAt, &x.CompletedAt); err != nil {
			return nil, false, err
		}
		out = append(out, x)
		if len(out) > max {
			return out[:max], true, nil
		}
	}
	return out, false, rows.Err()
}

func (s *Store) CleanupOldPending(ctx context.Context, olderThan time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM versions WHERE state=? AND created_at < ?`,
		VersionPending, olderThan.UnixNano())
	return err
}

func (s *Store) RepairDeleting(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE versions SET state=? WHERE state=?`, VersionCommitted, VersionDeleting)
	return err
}

func (s *Store) SnapshotVersionCounts(ctx context.Context) (map[string]Counts, error) {
	buckets, err := s.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Counts, len(buckets))
	for _, bucket := range buckets {
		c, err := s.VersionCounts(ctx, bucket)
		if err != nil {
			return nil, err
		}
		out[bucket] = c
	}
	return out, nil
}

func (s *Store) SnapshotMultipartStats(ctx context.Context) (map[string]MultipartStats, error) {
	buckets, err := s.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]MultipartStats, len(buckets))
	for _, bucket := range buckets {
		x, err := s.MultipartStats(ctx, bucket)
		if err != nil {
			return nil, err
		}
		out[bucket] = x
	}
	return out, nil
}

func (s *Store) SortedReclaims(records []ReclaimRecord) []ReclaimRecord {
	sort.Slice(records, func(i, j int) bool {
		if records[i].ReclaimedAt == records[j].ReclaimedAt {
			return records[i].ID > records[j].ID
		}
		return records[i].ReclaimedAt > records[j].ReclaimedAt
	})
	return records
}
