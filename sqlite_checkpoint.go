//go:build cgo

package leasekit

/*
#cgo LDFLAGS: -L/usr/lib/x86_64-linux-gnu -L/usr/lib/aarch64-linux-gnu -L/usr/lib64 -L/usr/lib -l:libsqlite3.so.0
#include <stdlib.h>

typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;

extern int sqlite3_open_v2(const char *filename, sqlite3 **ppDb, int flags, const char *zVfs);
extern int sqlite3_close_v2(sqlite3 *db);
extern int sqlite3_exec(sqlite3 *db, const char *sql, int (*callback)(void *, int, char **, char **), void *arg, char **errmsg);
extern int sqlite3_prepare_v2(sqlite3 *db, const char *zSql, int nByte, sqlite3_stmt **ppStmt, const char **pzTail);
extern int sqlite3_step(sqlite3_stmt *pStmt);
extern int sqlite3_finalize(sqlite3_stmt *pStmt);
extern int sqlite3_column_count(sqlite3_stmt *pStmt);
extern const unsigned char *sqlite3_column_text(sqlite3_stmt *pStmt, int iCol);
extern long long sqlite3_column_int64(sqlite3_stmt *pStmt, int iCol);
extern const char *sqlite3_errmsg(sqlite3 *db);
extern void sqlite3_free(void *ptr);
*/
import "C"

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"
)

const (
	sqliteOK   = 0
	sqliteRow  = 100
	sqliteDone = 101
)

type SQLiteCheckpointStore struct {
	Path string
}

func NewSQLiteCheckpointStore(path string) *SQLiteCheckpointStore {
	return &SQLiteCheckpointStore{Path: path}
}

func (s *SQLiteCheckpointStore) Location() string { return s.Path }

type sqliteDB struct {
	ptr *C.sqlite3
}

func openSQLite(path string) (*sqliteDB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var ptr *C.sqlite3
	rc := C.sqlite3_open_v2(cpath, &ptr, 6, nil) // READWRITE|CREATE
	if rc != sqliteOK {
		msg := "unknown sqlite open error"
		if ptr != nil {
			msg = C.GoString(C.sqlite3_errmsg(ptr))
			C.sqlite3_close_v2(ptr)
		}
		return nil, validationError(CodeCheckpointCorrupt, path, "sqlite open rc=%d: %s", int(rc), msg)
	}
	db := &sqliteDB{ptr: ptr}
	if err := db.exec(`
CREATE TABLE IF NOT EXISTS checkpoint_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS holder_checkpoint (
  holder_id TEXT PRIMARY KEY,
  resource_id TEXT NOT NULL,
  renew_count INTEGER NOT NULL,
  expires_at_ms INTEGER NOT NULL,
  last_seq INTEGER NOT NULL,
  state_seq INTEGER NOT NULL
);`); err != nil {
		_ = db.close()
		return nil, err
	}
	return db, nil
}

func (db *sqliteDB) close() error {
	if db.ptr == nil {
		return nil
	}
	rc := C.sqlite3_close_v2(db.ptr)
	db.ptr = nil
	if rc != sqliteOK {
		return validationError(CodeCheckpointCorrupt, "sqlite", "close rc=%d", int(rc))
	}
	return nil
}

func (db *sqliteDB) exec(query string) error {
	cquery := C.CString(query)
	defer C.free(unsafe.Pointer(cquery))
	var errMsg *C.char
	rc := C.sqlite3_exec(db.ptr, cquery, nil, nil, &errMsg)
	if rc != sqliteOK {
		msg := C.GoString(C.sqlite3_errmsg(db.ptr))
		if errMsg != nil {
			msg = C.GoString(errMsg)
			C.sqlite3_free(unsafe.Pointer(errMsg))
		}
		return classifySQLiteError(validationError(CodeCheckpointCorrupt, "sqlite", "exec rc=%d: %s; sql=%s", int(rc), msg, query))
	}
	return nil
}

func (db *sqliteDB) stringRows(query string) ([][]string, error) {
	cquery := C.CString(query)
	defer C.free(unsafe.Pointer(cquery))
	var stmt *C.sqlite3_stmt
	rc := C.sqlite3_prepare_v2(db.ptr, cquery, -1, &stmt, nil)
	if rc != sqliteOK {
		msg := C.GoString(C.sqlite3_errmsg(db.ptr))
		return nil, classifySQLiteError(validationError(CodeCheckpointCorrupt, "sqlite", "prepare rc=%d: %s", int(rc), msg))
	}
	defer C.sqlite3_finalize(stmt)

	var rows [][]string
	for {
		stepRC := C.sqlite3_step(stmt)
		if stepRC == sqliteDone {
			break
		}
		if stepRC != sqliteRow {
			msg := C.GoString(C.sqlite3_errmsg(db.ptr))
			return nil, classifySQLiteError(validationError(CodeCheckpointCorrupt, "sqlite", "step rc=%d: %s", int(stepRC), msg))
		}
		count := int(C.sqlite3_column_count(stmt))
		row := make([]string, count)
		for i := 0; i < count; i++ {
			text := C.sqlite3_column_text(stmt, C.int(i))
			if text != nil {
				row[i] = C.GoString((*C.char)(unsafe.Pointer(text)))
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func classifySQLiteError(err error) error {
	var biz *Error
	if !errors.As(err, &biz) {
		return err
	}
	lower := strings.ToLower(biz.Detail)
	if strings.Contains(lower, "malformed") || strings.Contains(lower, "not a database") || strings.Contains(lower, "truncated") {
		biz.Code = CodeCheckpointTruncated
	}
	return biz
}

func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func (s *SQLiteCheckpointStore) WriteHolderStates(cp Checkpoint) error {
	db, err := sql.Open("lease-sqlite3", s.Path)
	if err != nil {
		return err
	}
	defer db.Close()

	var b strings.Builder
	b.WriteString("BEGIN IMMEDIATE; DELETE FROM holder_checkpoint; ")
	for _, h := range cp.Holders {
		b.WriteString("INSERT INTO holder_checkpoint VALUES (")
		b.WriteString(sqlString(h.HolderID))
		b.WriteByte(',')
		b.WriteString(sqlString(h.ResourceID))
		b.WriteByte(',')
		b.WriteString(strconv.Itoa(h.RenewCount))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(int64(h.ExpiresAtMS), 10))
		b.WriteByte(',')
		b.WriteString(strconv.Itoa(h.LastSeq))
		b.WriteByte(',')
		b.WriteString(strconv.Itoa(cp.CommittedSeq))
		b.WriteString("); ")
	}
	b.WriteString("COMMIT;")
	_, err = db.Exec(b.String())
	return err
}

func (s *SQLiteCheckpointStore) WriteCommittedSeq(cp Checkpoint) error {
	db, err := sql.Open("lease-sqlite3", s.Path)
	if err != nil {
		return err
	}
	defer db.Close()

	upsert := func(key, value string) string {
		return "INSERT INTO checkpoint_meta(key,value) VALUES(" + sqlString(key) + "," + sqlString(value) +
			") ON CONFLICT(key) DO UPDATE SET value=excluded.value; "
	}
	var b strings.Builder
	b.WriteString(upsert("version", cp.Version))
	b.WriteString(upsert("schema_version", cp.SchemaVersion))
	b.WriteString(upsert("project_id", cp.ProjectID))
	b.WriteString(upsert("checksum", cp.Checksum))
	// Global commit sequence is intentionally the last durable write.
	b.WriteString(upsert("committed_seq", strconv.Itoa(cp.CommittedSeq)))
	_, err = db.Exec(b.String())
	return err
}

func (s *SQLiteCheckpointStore) LoadCheckpoint() (*Checkpoint, error) {
	info, statErr := os.Stat(s.Path)
	if errors.Is(statErr, os.ErrNotExist) {
		return nil, nil
	}
	if statErr != nil {
		return nil, statErr
	}
	if info.Size() < 100 {
		return nil, validationError(CodeCheckpointTruncated, s.Path, "sqlite file is only %d bytes; missing 100-byte header/page", info.Size())
	}
	db, err := sql.Open("lease-sqlite3", s.Path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	metaRows, err := db.Query("SELECT key,value FROM checkpoint_meta ORDER BY key")
	if err != nil {
		return nil, err
	}
	meta := map[string]string{}
	for metaRows.Next() {
		var key, value string
		if err := metaRows.Scan(&key, &value); err != nil {
			_ = metaRows.Close()
			return nil, err
		}
		meta[key] = value
	}
	_ = metaRows.Close()
	if _, ok := meta["committed_seq"]; !ok {
		return nil, nil
	}
	if meta["version"] != CheckpointVersion || meta["schema_version"] != CanonicalSchemaVersion {
		return nil, validationError(CodeSchemaVersionMismatch, s.Path, "checkpoint=%s/%s want=%s/%s", meta["version"], meta["schema_version"], CheckpointVersion, CanonicalSchemaVersion)
	}
	seq, err := strconv.Atoi(meta["committed_seq"])
	if err != nil {
		return nil, validationError(CodeCheckpointCorrupt, s.Path, "bad committed_seq %q", meta["committed_seq"])
	}

	rows, err := db.Query("SELECT holder_id,resource_id,renew_count,expires_at_ms,last_seq,state_seq FROM holder_checkpoint ORDER BY holder_id")
	if err != nil {
		return nil, err
	}
	cp := &Checkpoint{
		Version: meta["version"], SchemaVersion: meta["schema_version"], ProjectID: meta["project_id"],
		CommittedSeq: seq, Checksum: meta["checksum"],
	}
	maxStateSeq := 0
	for rows.Next() {
		var holderID, resourceID string
		var count, expiry, lastSeq, stateSeq int64
		if err := rows.Scan(&holderID, &resourceID, &count, &expiry, &lastSeq, &stateSeq); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if int(stateSeq) > maxStateSeq {
			maxStateSeq = int(stateSeq)
		}
		cp.Holders = append(cp.Holders, HolderCheckpoint{
			HolderID: holderID, ResourceID: resourceID, RenewCount: int(count), ExpiresAtMS: Milli(expiry), LastSeq: int(lastSeq),
		})
	}
	_ = rows.Close()
	if maxStateSeq > seq {
		return nil, validationError(CodeCheckpointIncomplete, s.Path, "holder state_seq=%d is ahead of committed_seq=%d", maxStateSeq, seq)
	}
	if maxStateSeq != seq {
		return nil, validationError(CodeCheckpointTruncated, s.Path, "holder state_seq=%d does not match committed_seq=%d", maxStateSeq, seq)
	}
	if err := cp.verifyChecksum(); err != nil {
		biz, _ := AsError(err)
		biz.Path = s.Path
		return nil, err
	}
	return cp, nil
}
