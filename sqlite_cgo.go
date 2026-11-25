//go:build cgo && linux

package thermalcache

/*
#cgo linux LDFLAGS: -L/usr/lib/x86_64-linux-gnu -l:libsqlite3.so.0
#include <stdlib.h>
#include <stdint.h>

typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;
typedef void (*sqlite3_destructor_type)(void*);

int sqlite3_open_v2(const char*, sqlite3**, int, const char*);
int sqlite3_close_v2(sqlite3*);
const char* sqlite3_errmsg(sqlite3*);
int sqlite3_prepare_v2(sqlite3*, const char*, int, sqlite3_stmt**, const char**);
int sqlite3_bind_int64(sqlite3_stmt*, int, long long);
int sqlite3_bind_text(sqlite3_stmt*, int, const char*, int, sqlite3_destructor_type);
int sqlite3_bind_null(sqlite3_stmt*, int);
int sqlite3_step(sqlite3_stmt*);
int sqlite3_reset(sqlite3_stmt*);
int sqlite3_clear_bindings(sqlite3_stmt*);
int sqlite3_finalize(sqlite3_stmt*);
long long sqlite3_column_int64(sqlite3_stmt*, int);
const unsigned char* sqlite3_column_text(sqlite3_stmt*, int);
int sqlite3_column_type(sqlite3_stmt*, int);
int sqlite3_column_count(sqlite3_stmt*);

static int tc_bind_text_copy(sqlite3_stmt *s, int i, const char *v, int n) {
	return sqlite3_bind_text(s, i, v, n, (sqlite3_destructor_type)-1);
}
*/
import "C"

import (
	"fmt"
	"os"
	"strings"
	"unsafe"
)

const (
	sqliteOK     = 0
	sqliteRcRow  = 100
	sqliteRcDone = 101
)

type sqliteDB struct {
	db   *C.sqlite3
	path string
}

type sqliteRow []any

func openSQLiteDB(path string, readOnly bool) (*sqliteDB, error) {
	if readOnly {
		st, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, biz(CodeCheckpointNotFound, "checkpoint file does not exist", map[string]any{"path": path})
			}
			return nil, biz(CodeCheckpointInvalid, "cannot stat checkpoint", map[string]any{"path": path, "error": err.Error()})
		}
		if st.Size() < 100 {
			return nil, biz(CodeCheckpointTruncated, "checkpoint is too short to contain a SQLite header", map[string]any{
				"path": path, "size": st.Size(),
			})
		}
		head := make([]byte, 16)
		f, err := os.Open(path)
		if err != nil {
			return nil, biz(CodeCheckpointInvalid, "cannot open checkpoint header", map[string]any{"path": path, "error": err.Error()})
		}
		n, _ := f.Read(head)
		_ = f.Close()
		if n != 16 || string(head) != "SQLite format 3\x00" {
			return nil, biz(CodeCheckpointTruncated, "checkpoint has no valid SQLite header", map[string]any{"path": path})
		}
	}
	flags := C.int(0x00000002 | 0x00000004) // READWRITE | CREATE
	if readOnly {
		flags = C.int(0x00000001) // READONLY
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var raw *C.sqlite3
	rc := C.sqlite3_open_v2(cpath, &raw, flags, nil)
	if rc != sqliteOK {
		msg := "sqlite open failed"
		if raw != nil {
			msg = C.GoString(C.sqlite3_errmsg(raw))
			C.sqlite3_close_v2(raw)
		}
		if readOnly && (strings.Contains(strings.ToLower(msg), "not a database") || strings.Contains(strings.ToLower(msg), "malformed")) {
			return nil, biz(CodeCheckpointTruncated, "checkpoint database is malformed", map[string]any{"path": path, "error": msg})
		}
		return nil, biz(CodeCheckpointInvalid, "cannot open checkpoint database", map[string]any{"path": path, "error": msg})
	}
	return &sqliteDB{db: raw, path: path}, nil
}

func (d *sqliteDB) errmsg() string {
	if d == nil || d.db == nil {
		return "database is closed"
	}
	return C.GoString(C.sqlite3_errmsg(d.db))
}

func (d *sqliteDB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	rc := C.sqlite3_close_v2(d.db)
	d.db = nil
	if rc != sqliteOK {
		return fmt.Errorf("sqlite close failed: %d", rc)
	}
	return nil
}

func (d *sqliteDB) prepare(query string) (*C.sqlite3_stmt, error) {
	cquery := C.CString(query)
	defer C.free(unsafe.Pointer(cquery))
	var stmt *C.sqlite3_stmt
	rc := C.sqlite3_prepare_v2(d.db, cquery, -1, &stmt, nil)
	if rc != sqliteOK {
		return nil, fmt.Errorf("sqlite prepare: %s", d.errmsg())
	}
	return stmt, nil
}

func (d *sqliteDB) bind(stmt *C.sqlite3_stmt, args []any) error {
	for i, arg := range args {
		idx := C.int(i + 1)
		var rc C.int
		switch v := arg.(type) {
		case nil:
			rc = C.sqlite3_bind_null(stmt, idx)
		case int:
			rc = C.sqlite3_bind_int64(stmt, idx, C.longlong(v))
		case int64:
			rc = C.sqlite3_bind_int64(stmt, idx, C.longlong(v))
		case bool:
			n := int64(0)
			if v {
				n = 1
			}
			rc = C.sqlite3_bind_int64(stmt, idx, C.longlong(n))
		case string:
			cs := C.CString(v)
			rc = C.tc_bind_text_copy(stmt, idx, cs, C.int(len(v)))
			C.free(unsafe.Pointer(cs))
		default:
			return fmt.Errorf("unsupported sqlite bind type %T", arg)
		}
		if rc != sqliteOK {
			return fmt.Errorf("sqlite bind %d: %s", i+1, d.errmsg())
		}
	}
	return nil
}

func (d *sqliteDB) Exec(query string, args ...any) error {
	stmt, err := d.prepare(query)
	if err != nil {
		return err
	}
	defer C.sqlite3_finalize(stmt)
	if err := d.bind(stmt, args); err != nil {
		return err
	}
	for {
		rc := C.sqlite3_step(stmt)
		if rc == sqliteRcDone {
			break
		}
		if rc != sqliteRcRow {
			return fmt.Errorf("sqlite exec: %s", d.errmsg())
		}
	}
	return nil
}

func (d *sqliteDB) Query(query string, args ...any) ([]sqliteRow, error) {
	stmt, err := d.prepare(query)
	if err != nil {
		return nil, err
	}
	defer C.sqlite3_finalize(stmt)
	if err := d.bind(stmt, args); err != nil {
		return nil, err
	}
	var rows []sqliteRow
	for {
		rc := C.sqlite3_step(stmt)
		if rc == sqliteRcDone {
			break
		}
		if rc != sqliteRcRow {
			return nil, fmt.Errorf("sqlite query: %s", d.errmsg())
		}
		n := int(C.sqlite3_column_count(stmt))
		row := make(sqliteRow, n)
		for i := 0; i < n; i++ {
			idx := C.int(i)
			switch C.sqlite3_column_type(stmt, idx) {
			case 5, 0:
				row[i] = nil
			case 1:
				row[i] = int64(C.sqlite3_column_int64(stmt, idx))
			case 2:
				row[i] = float64(0)
			case 3:
				p := C.sqlite3_column_text(stmt, idx)
				if p == nil {
					row[i] = ""
				} else {
					row[i] = C.GoString((*C.char)(unsafe.Pointer(p)))
				}
			default:
				row[i] = nil
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
