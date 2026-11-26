//go:build cgo

package checkpoint

/*
#cgo LDFLAGS: -L/usr/lib/x86_64-linux-gnu -l:libsqlite3.so.0
#include <stdlib.h>

typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;

extern int sqlite3_open(const char *filename, sqlite3 **ppDb);
extern int sqlite3_close_v2(sqlite3 *db);
extern int sqlite3_exec(sqlite3 *db, const char *sql, void *callback, void *arg, char **errmsg);
extern int sqlite3_prepare_v2(sqlite3 *db, const char *zSql, int nByte, sqlite3_stmt **ppStmt, const char **pzTail);
extern int sqlite3_step(sqlite3_stmt *pStmt);
extern int sqlite3_finalize(sqlite3_stmt *pStmt);
extern const unsigned char *sqlite3_column_text(sqlite3_stmt *pStmt, int iCol);
extern char *sqlite3_errmsg(sqlite3 *db);
extern void sqlite3_free(void *ptr);
*/
import "C"

import (
	"encoding/json"
	"strconv"
	"strings"
	"unsafe"

	"quota-cluster/internal/bizerror"
	"quota-cluster/internal/engine"
	"quota-cluster/internal/model"
)

const (
	sqliteRow  = 100
	sqliteDone = 101
)

func openStore(path string) (*Store, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))

	var db *C.sqlite3
	if rc := C.sqlite3_open(cpath, &db); rc != 0 {
		message := "sqlite open failed"
		if db != nil {
			message = C.GoString(C.sqlite3_errmsg(db))
			C.sqlite3_close_v2(db)
		}
		return nil, bizerror.New(bizerror.CodeCheckpointCorrupt, path, "open SQLite checkpoint: %s (rc=%d)", message, int(rc))
	}
	s := &Store{path: path, db: unsafe.Pointer(db)}
	if err := s.exec(schemaSQL); err != nil {
		_ = s.close()
		return nil, err
	}
	return s, nil
}

func (s *Store) cDB() *C.sqlite3 { return (*C.sqlite3)(s.db) }

func (s *Store) close() error {
	if s.db == nil {
		return nil
	}
	C.sqlite3_close_v2(s.cDB())
	s.db = nil
	return nil
}

func (s *Store) exec(query string) error {
	cquery := C.CString(query)
	defer C.free(unsafe.Pointer(cquery))

	var errMsg *C.char
	rc := C.sqlite3_exec(s.cDB(), cquery, nil, nil, &errMsg)
	if rc != 0 {
		message := C.GoString(C.sqlite3_errmsg(s.cDB()))
		if errMsg != nil {
			message = C.GoString(errMsg)
			C.sqlite3_free(unsafe.Pointer(errMsg))
		}
		return bizerror.New(bizerror.CodeCheckpointCorrupt, s.path, "sqlite exec: %s (rc=%d)", message, int(rc))
	}
	return nil
}

func (s *Store) queryTextColumn(query string) ([]string, error) {
	cquery := C.CString(query)
	defer C.free(unsafe.Pointer(cquery))

	var stmt *C.sqlite3_stmt
	if rc := C.sqlite3_prepare_v2(s.cDB(), cquery, -1, &stmt, nil); rc != 0 {
		return nil, bizerror.New(bizerror.CodeCheckpointCorrupt, s.path, "sqlite prepare: %s", C.GoString(C.sqlite3_errmsg(s.cDB())))
	}
	defer C.sqlite3_finalize(stmt)

	var rows []string
	for {
		rc := C.sqlite3_step(stmt)
		if rc == sqliteDone {
			break
		}
		if rc != sqliteRow {
			return nil, bizerror.New(bizerror.CodeCheckpointCorrupt, s.path, "sqlite step: %s", C.GoString(C.sqlite3_errmsg(s.cDB())))
		}
		text := C.sqlite3_column_text(stmt, 0)
		if text == nil {
			rows = append(rows, "")
		} else {
			rows = append(rows, C.GoString((*C.char)(unsafe.Pointer(text))))
		}
	}
	return rows, nil
}

func (s *Store) Save(cp *Checkpoint, opts SaveOptions) error {
	if cp == nil {
		return bizerror.New(bizerror.CodeCheckpointCorrupt, "/checkpoint", "nil checkpoint")
	}
	if cp.FormatVersion == "" {
		cp.FormatVersion = model.FormatVersion
	}
	pre, globalTickSQL, err := buildSaveStatements(cp, opts.ProjectHash)
	if err != nil {
		return err
	}
	if err := s.exec("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = s.exec("ROLLBACK")
		}
	}()
	for _, statement := range pre {
		if err := s.exec(statement); err != nil {
			return err
		}
	}
	if opts.BeforeGlobalTick != nil {
		opts.BeforeGlobalTick()
	}
	if err := s.exec(globalTickSQL); err != nil {
		return err
	}
	if err := s.exec("COMMIT"); err != nil {
		return err
	}
	rollback = false
	return nil
}

func (s *Store) Load(expectedProjectHash string) (*Checkpoint, error) {
	versions, err := s.queryTextColumn("SELECT value FROM checkpoint_meta WHERE key='format_version'")
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, bizerror.New(bizerror.CodeCheckpointCorrupt, s.path, "checkpoint has no format_version")
	}
	if versions[0] != model.FormatVersion {
		return nil, bizerror.New(bizerror.CodeCheckpointVersion, s.path, "format version %q does not match supported %q", versions[0], model.FormatVersion)
	}

	summaries, err := s.queryTextColumn("SELECT tick || '|' || format_version || '|' || project_id || '|' || project_hash FROM committed_tick WHERE id=1")
	if err != nil {
		return nil, err
	}
	if len(summaries) != 1 {
		return nil, bizerror.New(bizerror.CodeCheckpointCorrupt, s.path, "committed_tick row is missing or duplicated")
	}
	parts := strings.SplitN(summaries[0], "|", 4)
	if len(parts) != 4 {
		return nil, bizerror.New(bizerror.CodeCheckpointCorrupt, s.path, "invalid committed_tick summary")
	}
	tick, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, bizerror.New(bizerror.CodeCheckpointCorrupt, s.path, "invalid committed tick %q", parts[0])
	}
	if parts[1] != model.FormatVersion {
		return nil, bizerror.New(bizerror.CodeCheckpointVersion, s.path, "committed format %q != %q", parts[1], model.FormatVersion)
	}
	if expectedProjectHash != "" && parts[3] != expectedProjectHash {
		return nil, bizerror.New(bizerror.CodeCheckpointVersion, s.path, "project hash %q does not match expected %q", parts[3], expectedProjectHash)
	}

	cp := &Checkpoint{FormatVersion: parts[1], ProjectID: parts[2], ProjectHash: parts[3], CommittedTick: tick}
	if err := loadJSONRows(s, "SELECT payload_json FROM tenant_state ORDER BY tenant_id", &cp.TenantStates); err != nil {
		return nil, err
	}
	if err := loadJSONRows(s, "SELECT payload_json FROM node_state ORDER BY node_id", &cp.NodeStates); err != nil {
		return nil, err
	}
	if err := loadJSONRows(s, "SELECT payload_json FROM queue_member ORDER BY tenant_id,position", &cp.Queue); err != nil {
		return nil, err
	}
	if err := loadJSONRows(s, "SELECT payload_json FROM decision_record ORDER BY tick,request_id", &cp.Decisions); err != nil {
		return nil, err
	}
	engineRows, err := s.queryTextColumn("SELECT payload_json FROM engine_state WHERE id=1")
	if err != nil {
		return nil, err
	}
	if len(engineRows) == 1 {
		var state engine.State
		if err := json.Unmarshal([]byte(engineRows[0]), &state); err != nil {
			return nil, bizerror.New(bizerror.CodeCheckpointCorrupt, s.path, "decode engine_state: %v", err)
		}
		cp.EngineState = &state
	}
	return cp, nil
}

func loadJSONRows[T any](s *Store, query string, dst *[]T) error {
	rows, err := s.queryTextColumn(query)
	if err != nil {
		return err
	}
	*dst = make([]T, 0, len(rows))
	for _, row := range rows {
		var value T
		if err := json.Unmarshal([]byte(row), &value); err != nil {
			return bizerror.New(bizerror.CodeCheckpointCorrupt, s.path, "decode payload: %v", err)
		}
		*dst = append(*dst, value)
	}
	return nil
}
