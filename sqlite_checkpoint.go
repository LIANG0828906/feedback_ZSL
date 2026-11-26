package quantforensics

/*
#cgo linux LDFLAGS: -L/usr/lib/x86_64-linux-gnu -L/usr/lib/aarch64-linux-gnu -l:libsqlite3.so.0
#include <stddef.h>
#include <stdlib.h>

typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;

extern int sqlite3_open_v2(const char *filename, sqlite3 **ppDb, int flags, const char *zVfs);
extern int sqlite3_close_v2(sqlite3 *db);
extern int sqlite3_exec(sqlite3 *db, const char *sql, void *callback, void *arg, char **errmsg);
extern int sqlite3_prepare_v2(sqlite3 *db, const char *zSql, int nByte, sqlite3_stmt **ppStmt, const char **pzTail);
extern int sqlite3_step(sqlite3_stmt *pStmt);
extern int sqlite3_finalize(sqlite3_stmt *pStmt);
extern long long sqlite3_last_insert_rowid(sqlite3 *db);
extern const unsigned char *sqlite3_column_text(sqlite3_stmt *pStmt, int iCol);
extern long long sqlite3_column_int64(sqlite3_stmt *pStmt, int iCol);
extern const char *sqlite3_errmsg(sqlite3 *db);

#define CP_SQLITE_OPEN_READWRITE 2
#define CP_SQLITE_OPEN_CREATE    4
*/
import "C"

import (
	"fmt"
	"sync"
	"time"
	"unsafe"
)

type SQLiteStore struct {
	mu   sync.Mutex
	path string
	db   *C.sqlite3
}

func OpenSQLiteStore(path string) (*SQLiteStore, error) {
	if path == "" {
		path = "quantforensics-checkpoint.sqlite"
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	s := &SQLiteStore{path: path}
	rc := C.sqlite3_open_v2(cpath, &s.db, C.CP_SQLITE_OPEN_READWRITE|C.CP_SQLITE_OPEN_CREATE, nil)
	if rc != 0 {
		msg := "unknown sqlite error"
		if s.db != nil {
			msg = C.GoString(C.sqlite3_errmsg(s.db))
			C.sqlite3_close_v2(s.db)
		}
		return nil, fmt.Errorf("E_SQLITE open %s: %s (rc=%d)", path, msg, int(rc))
	}
	if err := s.execLocked(`PRAGMA busy_timeout=5000;`); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.Init(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) Path() string { return s.path }

func (s *SQLiteStore) Init() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execLocked(`
CREATE TABLE IF NOT EXISTS stage_checkpoints (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id TEXT NOT NULL,
  block_id TEXT NOT NULL,
  stage TEXT NOT NULL,
  state TEXT NOT NULL,
  order_key INTEGER NOT NULL,
  output_summary TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  output_json TEXT NOT NULL,
  commit_nano INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_stage_project_order ON stage_checkpoints(project_id, seq);
CREATE INDEX IF NOT EXISTS idx_stage_block_stage ON stage_checkpoints(project_id, block_id, stage, seq);
`)
}

func (s *SQLiteStore) sqliteErrorLocked(context string, rc C.int) error {
	msg := "unknown"
	if s.db != nil {
		msg = C.GoString(C.sqlite3_errmsg(s.db))
	}
	return fmt.Errorf("E_SQLITE %s: %s (rc=%d)", context, msg, int(rc))
}

func (s *SQLiteStore) execLocked(sql string) error {
	csql := C.CString(sql)
	defer C.free(unsafe.Pointer(csql))
	rc := C.sqlite3_exec(s.db, csql, nil, nil, nil)
	if rc != 0 {
		return s.sqliteErrorLocked("exec", rc)
	}
	return nil
}

func sqlString(v string) string {
	out := make([]rune, 0, len(v)+2)
	out = append(out, '\'')
	for _, r := range v {
		if r == '\'' {
			out = append(out, '\'', '\'')
		} else if r == 0 {
			continue
		} else {
			out = append(out, r)
		}
	}
	out = append(out, '\'')
	return string(out)
}

func sqlBytes(v []byte) string { return sqlString(string(v)) }

// CommitStage writes one append-only stage checkpoint in one immediate
// transaction. The store mutex serializes writers inside this process.
func (s *SQLiteStore) CommitStage(r StageRecord) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ProjectID == "" || r.BlockID == "" || r.Stage == "" {
		return 0, schemaError("$.checkpoint", "projectId, blockId and stage are required")
	}
	if r.State == "" {
		r.State = string(StateCommitted)
	}
	if r.CommitNano == 0 {
		r.CommitNano = time.Now().UnixNano()
	}
	tx := false
	if err := s.execLocked("BEGIN IMMEDIATE;"); err != nil {
		return 0, err
	}
	tx = true
	commit := func() (int64, error) {
		sql := fmt.Sprintf(`INSERT INTO stage_checkpoints
(project_id,block_id,stage,state,order_key,output_summary,cache_key,content_hash,param_version,algo_version,output_json,commit_nano)
VALUES (%s,%s,%s,%s,%d,%s,%s,%s,%s,%s,%s,%d);`,
			sqlString(r.ProjectID), sqlString(r.BlockID), sqlString(string(r.Stage)), sqlString(r.State), r.OrderKey,
			sqlString(r.OutputSummary), sqlString(r.CacheKey), sqlString(r.ContentHash), sqlString(r.ParamVersion), sqlString(r.AlgoVersion),
			sqlBytes(r.OutputJSON), r.CommitNano)
		if err := s.execLocked(sql); err != nil {
			_ = s.execLocked("ROLLBACK;")
			return 0, err
		}
		seq := int64(C.sqlite3_last_insert_rowid(s.db))
		if err := s.execLocked("COMMIT;"); err != nil {
			_ = s.execLocked("ROLLBACK;")
			return 0, err
		}
		return seq, nil
	}
	seq, err := commit()
	if err != nil && tx {
		_ = s.execLocked("ROLLBACK;")
	}
	return seq, err
}

func (s *SQLiteStore) Records(projectID string) ([]StageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	query := `SELECT seq,project_id,block_id,stage,state,order_key,output_summary,cache_key,content_hash,param_version,algo_version,output_json,commit_nano FROM stage_checkpoints`
	if projectID != "" {
		query += " WHERE project_id=" + sqlString(projectID)
	}
	query += " ORDER BY seq;"
	cquery := C.CString(query)
	defer C.free(unsafe.Pointer(cquery))
	var stmt *C.sqlite3_stmt
	rc := C.sqlite3_prepare_v2(s.db, cquery, -1, &stmt, nil)
	if rc != 0 {
		return nil, s.sqliteErrorLocked("prepare records", rc)
	}
	defer C.sqlite3_finalize(stmt)

	records := []StageRecord{}
	for {
		rc = C.sqlite3_step(stmt)
		if rc == 101 { // SQLITE_DONE
			break
		}
		if rc != 100 { // SQLITE_ROW
			return nil, s.sqliteErrorLocked("step records", rc)
		}
		text := func(col int) string {
			p := C.sqlite3_column_text(stmt, C.int(col))
			if p == nil {
				return ""
			}
			return C.GoString((*C.char)(unsafe.Pointer(p)))
		}
		r := StageRecord{
			Seq:           int64(C.sqlite3_column_int64(stmt, 0)),
			ProjectID:     text(1),
			BlockID:       text(2),
			Stage:         BlockStage(text(3)),
			State:         text(4),
			OrderKey:      int(C.sqlite3_column_int64(stmt, 5)),
			OutputSummary: text(6),
			CacheKey:      text(7),
			ContentHash:   text(8),
			ParamVersion:  text(9),
			AlgoVersion:   text(10),
			OutputJSON:    []byte(text(11)),
			CommitNano:    int64(C.sqlite3_column_int64(stmt, 12)),
		}
		records = append(records, r)
	}
	return records, nil
}

func (s *SQLiteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	rc := C.sqlite3_close_v2(s.db)
	if rc != 0 {
		return s.sqliteErrorLocked("close", rc)
	}
	s.db = nil
	return nil
}
