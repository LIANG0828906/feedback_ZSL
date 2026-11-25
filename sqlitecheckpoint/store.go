//go:build cgo

// Package sqlitecheckpoint implements colortrace.CheckpointStore using SQLite.
package sqlitecheckpoint

/*
#cgo LDFLAGS: -L/usr/lib/x86_64-linux-gnu -L/usr/lib/aarch64-linux-gnu -l:libsqlite3.so.0
#include <stdlib.h>
#define SQLITE_OK 0

typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;

extern int sqlite3_open_v2(const char *filename, sqlite3 **ppDb, int flags, const char *zVfs);
extern int sqlite3_close_v2(sqlite3 *db);
extern int sqlite3_busy_timeout(sqlite3 *db, int ms);
extern int sqlite3_exec(sqlite3 *db, const char *sql, void *callback, void *arg, char **errmsg);
extern int sqlite3_prepare_v2(sqlite3 *db, const char *sql, int nbyte, sqlite3_stmt **ppStmt, const char **pzTail);
extern int sqlite3_step(sqlite3_stmt *stmt);
extern int sqlite3_finalize(sqlite3_stmt *stmt);
extern int sqlite3_bind_text(sqlite3_stmt *stmt, int idx, const char *value, int n, void *destructor);
extern int sqlite3_bind_blob(sqlite3_stmt *stmt, int idx, const void *value, int n, void *destructor);
extern int sqlite3_bind_int64(sqlite3_stmt *stmt, int idx, long long value);
extern const unsigned char *sqlite3_column_text(sqlite3_stmt *stmt, int idx);
extern const void *sqlite3_column_blob(sqlite3_stmt *stmt, int idx);
extern int sqlite3_column_bytes(sqlite3_stmt *stmt, int idx);
extern long long sqlite3_column_int64(sqlite3_stmt *stmt, int idx);
extern int sqlite3_column_type(sqlite3_stmt *stmt, int idx);
extern long long sqlite3_last_insert_rowid(sqlite3 *db);
extern char *sqlite3_errmsg(sqlite3 *db);

#define CT_ROW 100
#define CT_DONE 101
#define CT_NULL 5
#define CT_TRANSIENT ((void*)-1)
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"unsafe"

	ct "colortrace"
)

// Store is a mutex-serialized SQLite checkpoint database.
type Store struct {
	mu sync.Mutex
	db *C.sqlite3
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS units (
  project_id TEXT NOT NULL,
  stage_index INTEGER NOT NULL,
  chunk_id TEXT NOT NULL,
  chunk_x INTEGER NOT NULL DEFAULT 0,
  chunk_y INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL,
  output_digest TEXT NOT NULL DEFAULT '',
  truncation_digest TEXT NOT NULL DEFAULT '',
  truncation_count INTEGER NOT NULL DEFAULT 0,
  truncation_total INTEGER NOT NULL DEFAULT 0,
  input_content_hash TEXT NOT NULL DEFAULT '',
  content_hash TEXT NOT NULL DEFAULT '',
  cache_key TEXT NOT NULL DEFAULT '',
  sequence_fingerprint TEXT NOT NULL DEFAULT '',
  param_version TEXT NOT NULL DEFAULT '',
  algo_version TEXT NOT NULL DEFAULT '',
  output_json TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  attempt INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL DEFAULT '',
  commit_seq INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(project_id, stage_index, chunk_id)
);
CREATE TABLE IF NOT EXISTS commit_log (
  seq INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  stage_index INTEGER NOT NULL,
  chunk_id TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  committed_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS artifacts (
  name TEXT PRIMARY KEY,
  data TEXT NOT NULL
);
`

// Open opens or creates a snapshot. Any identity/version mismatch purges the
// old snapshot atomically before the new identity is installed.
func Open(path string, expected ct.SnapshotMeta) (*Store, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	var db *C.sqlite3
	flags := C.int(0x2 | 0x4 | 0x10000)
	if rc := C.sqlite3_open_v2(cPath, &db, flags, nil); rc != C.SQLITE_OK {
		return nil, fmt.Errorf("sqlite open rc=%d", rc)
	}
	store := &Store{db: db}
	if err := store.execNoLock("PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;"); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.execNoLock(schemaSQL); err != nil {
		_ = store.Close()
		return nil, err
	}
	stored, err := store.loadMetaNoLock()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if len(stored) == 0 {
		if err := store.writeMetaNoLock(expected); err != nil {
			_ = store.Close()
			return nil, err
		}
	} else if !metaMatches(stored, expected) || !store.rowsCompatibleNoLock(expected) {
		if err := store.wipeNoLock(); err != nil {
			_ = store.Close()
			return nil, err
		}
		if err := store.writeMetaNoLock(expected); err != nil {
			_ = store.Close()
			return nil, err
		}
	}
	return store, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	rc := C.sqlite3_close_v2(s.db)
	s.db = nil
	if rc != C.SQLITE_OK {
		return fmt.Errorf("sqlite close rc=%d", rc)
	}
	return nil
}

func (s *Store) InitSnapshot(meta ct.SnapshotMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, err := s.loadMetaNoLock()
	if err != nil {
		return err
	}
	if len(stored) == 0 {
		return s.writeMetaNoLock(meta)
	}
	if !metaMatches(stored, meta) || !s.rowsCompatibleNoLock(meta) {
		if err := s.wipeNoLock(); err != nil {
			return err
		}
		return s.writeMetaNoLock(meta)
	}
	return nil
}

func (s *Store) Meta() (ct.SnapshotMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metaNoLock()
}

func (s *Store) CommitUnit(record ct.UnitRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	if err := s.execNoLock("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = s.execNoLock("ROLLBACK")
		}
	}()
	nextSeq, err := s.nextCommitSeqNoLock()
	if err != nil {
		return err
	}
	record.CommitSeq = nextSeq
	if err := s.execArgsNoLock(
		`INSERT INTO commit_log(seq,project_id,stage_index,chunk_id,cache_key,committed_at) VALUES(?,?,?,?,?,?)`,
		nextSeq, record.ProjectID, record.Stage, record.ChunkID, record.CacheKey, record.UpdatedAt.Format(time.RFC3339Nano),
	); err != nil {
		return err
	}
	if err := s.upsertUnitNoLock(record); err != nil {
		return err
	}
	if err := s.execNoLock("COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *Store) SaveUnitState(record ct.UnitRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	return s.upsertUnitNoLock(record)
}

func (s *Store) Unit(stage int, chunkID string) (*ct.UnitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stmt, err := s.prepareNoLock(unitSelectSQL() + ` WHERE stage_index=? AND chunk_id=? LIMIT 1`)
	if err != nil {
		return nil, err
	}
	defer C.sqlite3_finalize(stmt)
	if err := bindArgs(stmt, []any{stage, chunkID}); err != nil {
		return nil, err
	}
	rc := C.sqlite3_step(stmt)
	if rc == C.CT_DONE {
		return nil, nil
	}
	if rc != C.CT_ROW {
		return nil, s.errorRC(rc)
	}
	record := scanUnit(stmt)
	return &record, nil
}

func (s *Store) Units(projectID string) ([]ct.UnitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unitsNoLock(projectID)
}

func (s *Store) unitsNoLock(projectID string) ([]ct.UnitRecord, error) {
	query := unitSelectSQL()
	args := []any{}
	if projectID != "" {
		query += " WHERE project_id=?"
		args = append(args, projectID)
	}
	query += " ORDER BY stage_index ASC, chunk_id ASC"
	stmt, err := s.prepareNoLock(query)
	if err != nil {
		return nil, err
	}
	defer C.sqlite3_finalize(stmt)
	if err := bindArgs(stmt, args); err != nil {
		return nil, err
	}
	out := []ct.UnitRecord{}
	for {
		rc := C.sqlite3_step(stmt)
		if rc == C.CT_DONE {
			break
		}
		if rc != C.CT_ROW {
			return nil, s.errorRC(rc)
		}
		out = append(out, scanUnit(stmt))
	}
	return out, nil
}

func (s *Store) CommitLog(projectID string) ([]ct.CommitEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	query := `SELECT seq,stage_index,chunk_id,cache_key FROM commit_log`
	args := []any{}
	if projectID != "" {
		query += " WHERE project_id=?"
		args = append(args, projectID)
	}
	query += " ORDER BY seq ASC"
	stmt, err := s.prepareNoLock(query)
	if err != nil {
		return nil, err
	}
	defer C.sqlite3_finalize(stmt)
	if err := bindArgs(stmt, args); err != nil {
		return nil, err
	}
	out := []ct.CommitEntry{}
	for {
		rc := C.sqlite3_step(stmt)
		if rc == C.CT_DONE {
			break
		}
		if rc != C.CT_ROW {
			return nil, s.errorRC(rc)
		}
		out = append(out, ct.CommitEntry{
			Seq:      int64(C.sqlite3_column_int64(stmt, 0)),
			Stage:    int(C.sqlite3_column_int64(stmt, 1)),
			ChunkID:  columnString(stmt, 2),
			CacheKey: columnString(stmt, 3),
		})
	}
	return out, nil
}

func (s *Store) PutArtifact(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execArgsNoLock(`INSERT OR REPLACE INTO artifacts(name,data) VALUES(?,?)`, name, string(data))
}

func (s *Store) GetArtifact(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stmt, err := s.prepareNoLock(`SELECT data FROM artifacts WHERE name=?`)
	if err != nil {
		return nil, err
	}
	defer C.sqlite3_finalize(stmt)
	if err := bindArgs(stmt, []any{name}); err != nil {
		return nil, err
	}
	if rc := C.sqlite3_step(stmt); rc != C.CT_ROW {
		return nil, nil
	}
	return columnBytes(stmt, 0), nil
}

func (s *Store) upsertUnitNoLock(r ct.UnitRecord) error {
	return s.execArgsNoLock(`
INSERT INTO units(project_id,stage_index,chunk_id,chunk_x,chunk_y,state,output_digest,
 truncation_digest,truncation_count,truncation_total,input_content_hash,content_hash,
 cache_key,sequence_fingerprint,param_version,algo_version,output_json,last_error,
 attempt,updated_at,commit_seq)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(project_id,stage_index,chunk_id) DO UPDATE SET
 chunk_x=excluded.chunk_x,chunk_y=excluded.chunk_y,state=excluded.state,
 output_digest=excluded.output_digest,truncation_digest=excluded.truncation_digest,
 truncation_count=excluded.truncation_count,truncation_total=excluded.truncation_total,
 input_content_hash=excluded.input_content_hash,content_hash=excluded.content_hash,
 cache_key=excluded.cache_key,sequence_fingerprint=excluded.sequence_fingerprint,
 param_version=excluded.param_version,algo_version=excluded.algo_version,
 output_json=excluded.output_json,last_error=excluded.last_error,
 attempt=excluded.attempt,updated_at=excluded.updated_at,commit_seq=excluded.commit_seq`,
		r.ProjectID, r.Stage, r.ChunkID, r.ChunkX, r.ChunkY, string(r.State),
		r.OutputDigest, r.TruncationDigest, r.TruncationCount, r.TruncationAbsoluteTotal,
		r.InputContentHash, r.ContentHash, r.CacheKey, r.StageSequenceFingerprint,
		r.ParamVersion, r.AlgorithmVersion, string(r.OutputJSON), r.LastError, r.Attempt,
		r.UpdatedAt.Format(time.RFC3339Nano), r.CommitSeq)
}

func unitSelectSQL() string {
	return `SELECT project_id,stage_index,chunk_id,chunk_x,chunk_y,state,output_digest,
 truncation_digest,truncation_count,truncation_total,input_content_hash,content_hash,
 cache_key,sequence_fingerprint,param_version,algo_version,output_json,last_error,
 attempt,updated_at,commit_seq FROM units`
}

func scanUnit(stmt *C.sqlite3_stmt) ct.UnitRecord {
	return ct.UnitRecord{
		ProjectID:                columnString(stmt, 0),
		Stage:                    int(C.sqlite3_column_int64(stmt, 1)),
		ChunkID:                  columnString(stmt, 2),
		ChunkX:                   int(C.sqlite3_column_int64(stmt, 3)),
		ChunkY:                   int(C.sqlite3_column_int64(stmt, 4)),
		State:                    ct.UnitState(columnString(stmt, 5)),
		OutputDigest:             columnString(stmt, 6),
		TruncationDigest:         columnString(stmt, 7),
		TruncationCount:          int(C.sqlite3_column_int64(stmt, 8)),
		TruncationAbsoluteTotal:  int64(C.sqlite3_column_int64(stmt, 9)),
		InputContentHash:         columnString(stmt, 10),
		ContentHash:              columnString(stmt, 11),
		CacheKey:                 columnString(stmt, 12),
		StageSequenceFingerprint: columnString(stmt, 13),
		ParamVersion:             columnString(stmt, 14),
		AlgorithmVersion:         columnString(stmt, 15),
		OutputJSON:               columnBytes(stmt, 16),
		LastError:                columnString(stmt, 17),
		Attempt:                  int(C.sqlite3_column_int64(stmt, 18)),
		UpdatedAt:                time.Now().UTC(),
		CommitSeq:                int64(C.sqlite3_column_int64(stmt, 20)),
	}
}

func (s *Store) nextCommitSeqNoLock() (int64, error) {
	stmt, err := s.prepareNoLock(`SELECT COALESCE(MAX(seq),0)+1 FROM commit_log`)
	if err != nil {
		return 0, err
	}
	defer C.sqlite3_finalize(stmt)
	if rc := C.sqlite3_step(stmt); rc != C.CT_ROW {
		return 0, s.errorRC(rc)
	}
	return int64(C.sqlite3_column_int64(stmt, 0)), nil
}

func (s *Store) prepareNoLock(query string) (*C.sqlite3_stmt, error) {
	cQuery := C.CString(query)
	defer C.free(unsafe.Pointer(cQuery))
	var stmt *C.sqlite3_stmt
	if rc := C.sqlite3_prepare_v2(s.db, cQuery, -1, &stmt, nil); rc != C.SQLITE_OK {
		return nil, s.errorRC(rc)
	}
	return stmt, nil
}

func (s *Store) execNoLock(query string) error {
	cQuery := C.CString(query)
	defer C.free(unsafe.Pointer(cQuery))
	if rc := C.sqlite3_exec(s.db, cQuery, nil, nil, nil); rc != C.SQLITE_OK {
		return s.errorRC(rc)
	}
	return nil
}

func (s *Store) execArgsNoLock(query string, args ...any) error {
	stmt, err := s.prepareNoLock(query)
	if err != nil {
		return err
	}
	defer C.sqlite3_finalize(stmt)
	if err := bindArgs(stmt, args); err != nil {
		return err
	}
	if rc := C.sqlite3_step(stmt); rc != C.CT_DONE && rc != C.CT_ROW {
		return s.errorRC(rc)
	}
	return nil
}

func bindArgs(stmt *C.sqlite3_stmt, args []any) error {
	for i, arg := range args {
		idx := C.int(i + 1)
		switch value := arg.(type) {
		case string:
			cValue := C.CString(value)
			rc := C.sqlite3_bind_text(stmt, idx, cValue, C.int(len(value)), C.CT_TRANSIENT)
			C.free(unsafe.Pointer(cValue))
			if rc != C.SQLITE_OK {
				return fmt.Errorf("sqlite bind text rc=%d", rc)
			}
		case []byte:
			cValue := C.CBytes(value)
			rc := C.sqlite3_bind_blob(stmt, idx, cValue, C.int(len(value)), C.CT_TRANSIENT)
			C.free(cValue)
			if rc != C.SQLITE_OK {
				return fmt.Errorf("sqlite bind blob rc=%d", rc)
			}
		case int:
			C.sqlite3_bind_int64(stmt, idx, C.longlong(int64(value)))
		case int64:
			C.sqlite3_bind_int64(stmt, idx, C.longlong(value))
		default:
			data, err := json.Marshal(value)
			if err != nil {
				return err
			}
			cValue := C.CString(string(data))
			C.sqlite3_bind_text(stmt, idx, cValue, C.int(len(data)), C.CT_TRANSIENT)
			C.free(unsafe.Pointer(cValue))
		}
	}
	return nil
}

func columnString(stmt *C.sqlite3_stmt, idx C.int) string {
	if C.sqlite3_column_type(stmt, idx) == C.CT_NULL {
		return ""
	}
	ptr := C.sqlite3_column_text(stmt, idx)
	n := C.sqlite3_column_bytes(stmt, idx)
	if ptr == nil {
		return ""
	}
	return C.GoStringN((*C.char)(unsafe.Pointer(ptr)), n)
}

func columnBytes(stmt *C.sqlite3_stmt, idx C.int) []byte {
	if C.sqlite3_column_type(stmt, idx) == C.CT_NULL {
		return nil
	}
	ptr := C.sqlite3_column_blob(stmt, idx)
	n := C.sqlite3_column_bytes(stmt, idx)
	if ptr == nil || n == 0 {
		return nil
	}
	return C.GoBytes(ptr, n)
}

func (s *Store) errorRC(rc C.int) error {
	message := ""
	if s.db != nil {
		message = C.GoString(C.sqlite3_errmsg(s.db))
	}
	return fmt.Errorf("sqlite error rc=%d: %s", rc, message)
}

func (s *Store) loadMetaNoLock() (map[string]string, error) {
	stmt, err := s.prepareNoLock(`SELECT key,value FROM meta`)
	if err != nil {
		return nil, err
	}
	defer C.sqlite3_finalize(stmt)
	out := map[string]string{}
	for {
		rc := C.sqlite3_step(stmt)
		if rc == C.CT_DONE {
			break
		}
		if rc != C.CT_ROW {
			return nil, s.errorRC(rc)
		}
		out[columnString(stmt, 0)] = columnString(stmt, 1)
	}
	return out, nil
}

func (s *Store) metaNoLock() (ct.SnapshotMeta, error) {
	stored, err := s.loadMetaNoLock()
	if err != nil {
		return ct.SnapshotMeta{}, err
	}
	meta := ct.SnapshotMeta{
		ProjectID: stored["project_id"], StageSequenceFingerprint: stored["sequence_fingerprint"],
		ParamVersionFingerprint: stored["param_fingerprint"], AlgorithmVersion: stored["algorithm_version"],
		TruthFingerprint: stored["truth_fingerprint"], ParamVersions: map[int]string{},
	}
	if raw := stored["param_versions_json"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &meta.ParamVersions)
	}
	return meta, nil
}

func (s *Store) writeMetaNoLock(meta ct.SnapshotMeta) error {
	paramVersions, _ := json.Marshal(meta.ParamVersions)
	rows := [][2]string{
		{"project_id", meta.ProjectID},
		{"sequence_fingerprint", meta.StageSequenceFingerprint},
		{"param_fingerprint", meta.ParamVersionFingerprint},
		{"algorithm_version", meta.AlgorithmVersion},
		{"truth_fingerprint", meta.TruthFingerprint},
		{"param_versions_json", string(paramVersions)},
	}
	if err := s.execNoLock(`DELETE FROM meta`); err != nil {
		return err
	}
	for _, row := range rows {
		if err := s.execArgsNoLock(`INSERT INTO meta(key,value) VALUES(?,?)`, row[0], row[1]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) wipeNoLock() error {
	for _, query := range []string{
		`DELETE FROM units`, `DELETE FROM commit_log`, `DELETE FROM artifacts`, `DELETE FROM meta`,
	} {
		if err := s.execNoLock(query); err != nil {
			return err
		}
	}
	return nil
}

func metaMatches(stored map[string]string, expected ct.SnapshotMeta) bool {
	return stored["project_id"] == expected.ProjectID &&
		stored["sequence_fingerprint"] == expected.StageSequenceFingerprint &&
		stored["param_fingerprint"] == expected.ParamVersionFingerprint &&
		stored["algorithm_version"] == expected.AlgorithmVersion &&
		stored["truth_fingerprint"] == expected.TruthFingerprint
}

func (s *Store) rowsCompatibleNoLock(expected ct.SnapshotMeta) bool {
	rows, err := s.unitsNoLock("")
	if err != nil {
		return false
	}
	for _, row := range rows {
		if row.StageSequenceFingerprint != expected.StageSequenceFingerprint ||
			row.AlgorithmVersion != expected.AlgorithmVersion {
			return false
		}
		if expected.ParamVersions != nil {
			if version, ok := expected.ParamVersions[row.Stage]; ok && row.ParamVersion != version {
				return false
			}
		}
		if row.State == ct.StateCommitted && (row.CacheKey == "" || row.OutputDigest == "" || row.ContentHash == "") {
			return false
		}
		if !ct.VerifyPersistedUnit(row) {
			return false
		}
	}
	return true
}

var _ = C.CT_NULL
