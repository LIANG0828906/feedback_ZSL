//go:build cgo && linux

// Package sqlite registers a minimal database/sql driver named "vpf_sqlite".
// It exists so the repository can verify its driver-agnostic SQL checkpoint
// store with only the Go standard library and a system SQLite shared library.
package sqlite

/*
#cgo LDFLAGS: -ldl
#include <stdlib.h>
#include <stddef.h>
#include <stdint.h>
#include <dlfcn.h>

typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;
typedef long long vpf_int64;

static void *vpf_lib = NULL;
static int (*vpf_sqlite3_open_v2_fn)(const char*, sqlite3**, int, const char*) = NULL;
static int (*vpf_sqlite3_close_v2_fn)(sqlite3*) = NULL;
static int (*vpf_sqlite3_exec_fn)(sqlite3*, const char*, void*, void*, char**) = NULL;
static int (*vpf_sqlite3_prepare_v2_fn)(sqlite3*, const char*, int, sqlite3_stmt**, const char**) = NULL;
static int (*vpf_sqlite3_step_fn)(sqlite3_stmt*) = NULL;
static int (*vpf_sqlite3_finalize_fn)(sqlite3_stmt*) = NULL;
static int (*vpf_sqlite3_bind_int64_fn)(sqlite3_stmt*, int, vpf_int64) = NULL;
static int (*vpf_sqlite3_bind_text_fn)(sqlite3_stmt*, int, const char*, int, void(*)(void*)) = NULL;
static int (*vpf_sqlite3_bind_blob_fn)(sqlite3_stmt*, int, const void*, int, void(*)(void*)) = NULL;
static int (*vpf_sqlite3_bind_null_fn)(sqlite3_stmt*, int) = NULL;
static int (*vpf_sqlite3_column_count_fn)(sqlite3_stmt*) = NULL;
static int (*vpf_sqlite3_column_type_fn)(sqlite3_stmt*, int) = NULL;
static vpf_int64 (*vpf_sqlite3_column_int64_fn)(sqlite3_stmt*, int) = NULL;
static const unsigned char *(*vpf_sqlite3_column_text_fn)(sqlite3_stmt*, int) = NULL;
static const void *(*vpf_sqlite3_column_blob_fn)(sqlite3_stmt*, int) = NULL;
static int (*vpf_sqlite3_column_bytes_fn)(sqlite3_stmt*, int) = NULL;
static const char *(*vpf_sqlite3_column_name_fn)(sqlite3_stmt*, int) = NULL;
static const char *(*vpf_sqlite3_errmsg_fn)(sqlite3*) = NULL;
static vpf_int64 (*vpf_sqlite3_changes_fn)(sqlite3*) = NULL;

#define VPF_TRANSIENT ((void(*)(void*))-1)

static int vpf_sqlite_load(void) {
	if (vpf_lib != NULL) return 0;
	vpf_lib = dlopen("libsqlite3.so.0", RTLD_NOW | RTLD_LOCAL);
	if (vpf_lib == NULL) return -1;
	#define VPF_LOAD(sym) do { \
		*(void **)(&vpf_##sym##_fn) = dlsym(vpf_lib, #sym); \
		if (vpf_##sym##_fn == NULL) return -2; \
	} while (0)
	VPF_LOAD(sqlite3_open_v2);
	VPF_LOAD(sqlite3_close_v2);
	VPF_LOAD(sqlite3_exec);
	VPF_LOAD(sqlite3_prepare_v2);
	VPF_LOAD(sqlite3_step);
	VPF_LOAD(sqlite3_finalize);
	VPF_LOAD(sqlite3_bind_int64);
	VPF_LOAD(sqlite3_bind_text);
	VPF_LOAD(sqlite3_bind_blob);
	VPF_LOAD(sqlite3_bind_null);
	VPF_LOAD(sqlite3_column_count);
	VPF_LOAD(sqlite3_column_type);
	VPF_LOAD(sqlite3_column_int64);
	VPF_LOAD(sqlite3_column_text);
	VPF_LOAD(sqlite3_column_blob);
	VPF_LOAD(sqlite3_column_bytes);
	VPF_LOAD(sqlite3_column_name);
	VPF_LOAD(sqlite3_errmsg);
	VPF_LOAD(sqlite3_changes);
	return 0;
}

static int vpf_open(const char *path, sqlite3 **db) {
	return vpf_sqlite3_open_v2_fn(path, db, 6, NULL);
}
static int vpf_close(sqlite3 *db) { return vpf_sqlite3_close_v2_fn(db); }
static int vpf_exec(sqlite3 *db, const char *sql) {
	return vpf_sqlite3_exec_fn(db, sql, NULL, NULL, NULL);
}
static int vpf_prepare(sqlite3 *db, const char *sql, sqlite3_stmt **stmt) {
	return vpf_sqlite3_prepare_v2_fn(db, sql, -1, stmt, NULL);
}
static int vpf_step(sqlite3_stmt *stmt) { return vpf_sqlite3_step_fn(stmt); }
static int vpf_finalize(sqlite3_stmt *stmt) { return vpf_sqlite3_finalize_fn(stmt); }
static int vpf_bind_int64(sqlite3_stmt *stmt, int i, long long v) { return vpf_sqlite3_bind_int64_fn(stmt, i, v); }
static int vpf_bind_text(sqlite3_stmt *stmt, int i, const char *v, int n) {
	return vpf_sqlite3_bind_text_fn(stmt, i, v, n, VPF_TRANSIENT);
}
static int vpf_bind_blob(sqlite3_stmt *stmt, int i, const void *v, int n) {
	return vpf_sqlite3_bind_blob_fn(stmt, i, v, n, VPF_TRANSIENT);
}
static int vpf_bind_null(sqlite3_stmt *stmt, int i) { return vpf_sqlite3_bind_null_fn(stmt, i); }
static int vpf_column_count(sqlite3_stmt *stmt) { return vpf_sqlite3_column_count_fn(stmt); }
static int vpf_column_type(sqlite3_stmt *stmt, int i) { return vpf_sqlite3_column_type_fn(stmt, i); }
static long long vpf_column_int64(sqlite3_stmt *stmt, int i) { return vpf_sqlite3_column_int64_fn(stmt, i); }
static const unsigned char *vpf_column_text(sqlite3_stmt *stmt, int i) { return vpf_sqlite3_column_text_fn(stmt, i); }
static const void *vpf_column_blob(sqlite3_stmt *stmt, int i) { return vpf_sqlite3_column_blob_fn(stmt, i); }
static int vpf_column_bytes(sqlite3_stmt *stmt, int i) { return vpf_sqlite3_column_bytes_fn(stmt, i); }
static const char *vpf_column_name(sqlite3_stmt *stmt, int i) { return vpf_sqlite3_column_name_fn(stmt, i); }
static const char *vpf_errmsg(sqlite3 *db) { return vpf_sqlite3_errmsg_fn(db); }
static long long vpf_changes(sqlite3 *db) { return vpf_sqlite3_changes_fn(db); }
*/
import "C"

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"unsafe"
)

const DriverName = "vpf_sqlite"

func init() {
	if C.vpf_sqlite_load() == 0 {
		sql.Register(DriverName, sqliteDriver{})
	}
}

type sqliteDriver struct{}

func (sqliteDriver) Open(dsn string) (driver.Conn, error) {
	if C.vpf_sqlite_load() != 0 {
		return nil, errors.New("vpf_sqlite: libsqlite3.so.0 was not found")
	}
	path := C.CString(dsn)
	defer C.free(unsafe.Pointer(path))
	var db *C.sqlite3
	if rc := C.vpf_open(path, &db); rc != 0 {
		message := "open failed"
		if db != nil {
			message = C.GoString(C.vpf_errmsg(db))
			C.vpf_close(db)
		}
		return nil, fmt.Errorf("vpf_sqlite: open rc=%d: %s", int(rc), message)
	}
	return &sqliteConn{db: db}, nil
}

type sqliteConn struct {
	db *C.sqlite3
}

func (c *sqliteConn) Prepare(query string) (driver.Stmt, error) {
	return &sqliteStmt{conn: c, query: query}, nil
}

func (c *sqliteConn) Close() error {
	if c.db != nil {
		C.vpf_close(c.db)
		c.db = nil
	}
	return nil
}

func (c *sqliteConn) Begin() (driver.Tx, error) {
	if _, err := c.ExecContext(context.Background(), "BEGIN", nil); err != nil {
		return nil, err
	}
	return sqliteTx{conn: c}, nil
}

func (c *sqliteConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	return c.Begin()
}

func (c *sqliteConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	stmt, err := c.prepareAndBind(query, values)
	if err != nil {
		return nil, err
	}
	defer C.vpf_finalize(stmt)
	rc := C.vpf_step(stmt)
	if rc != 101 && rc != 100 {
		return nil, c.errorf("exec step", rc)
	}
	return sqliteResult{changes: int64(C.vpf_changes(c.db))}, nil
}

func (c *sqliteConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	stmt, err := c.prepareAndBind(query, values)
	if err != nil {
		return nil, err
	}
	count := int(C.vpf_column_count(stmt))
	columns := make([]string, count)
	for i := range columns {
		columns[i] = C.GoString(C.vpf_column_name(stmt, C.int(i)))
	}
	return &sqliteRows{conn: c, stmt: stmt, columns: columns}, nil
}

func (c *sqliteConn) prepareAndBind(query string, args []driver.Value) (*C.sqlite3_stmt, error) {
	cQuery := C.CString(query)
	defer C.free(unsafe.Pointer(cQuery))
	var stmt *C.sqlite3_stmt
	if rc := C.vpf_prepare(c.db, cQuery, &stmt); rc != 0 {
		return nil, c.errorf("prepare", rc)
	}
	for i, arg := range args {
		index := C.int(i + 1)
		var rc C.int
		switch value := arg.(type) {
		case nil:
			rc = C.vpf_bind_null(stmt, index)
		case int64:
			rc = C.vpf_bind_int64(stmt, index, C.longlong(value))
		case int:
			rc = C.vpf_bind_int64(stmt, index, C.longlong(value))
		case bool:
			n := int64(0)
			if value {
				n = 1
			}
			rc = C.vpf_bind_int64(stmt, index, C.longlong(n))
		case string:
			ptr := C.CString(value)
			rc = C.vpf_bind_text(stmt, index, ptr, C.int(len(value)))
			C.free(unsafe.Pointer(ptr))
		case []byte:
			if value == nil {
				rc = C.vpf_bind_null(stmt, index)
				break
			}
			ptr := C.CBytes(value)
			rc = C.vpf_bind_blob(stmt, index, ptr, C.int(len(value)))
			C.free(ptr)
		default:
			C.vpf_finalize(stmt)
			return nil, fmt.Errorf("vpf_sqlite: unsupported driver value type %T", arg)
		}
		if rc != 0 {
			C.vpf_finalize(stmt)
			return nil, c.errorf("bind", rc)
		}
	}
	return stmt, nil
}

func (c *sqliteConn) errorf(op string, rc C.int) error {
	message := ""
	if c.db != nil {
		message = C.GoString(C.vpf_errmsg(c.db))
	}
	return fmt.Errorf("vpf_sqlite: %s rc=%d: %s", op, int(rc), message)
}

type sqliteStmt struct {
	conn   *sqliteConn
	query  string
	closed bool
}

func (s *sqliteStmt) Close() error  { s.closed = true; return nil }
func (s *sqliteStmt) NumInput() int { return -1 }

func (s *sqliteStmt) Exec(args []driver.Value) (driver.Result, error) {
	named := make([]driver.NamedValue, len(args))
	for i, arg := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: arg}
	}
	return s.conn.ExecContext(context.Background(), s.query, named)
}

func (s *sqliteStmt) Query(args []driver.Value) (driver.Rows, error) {
	named := make([]driver.NamedValue, len(args))
	for i, arg := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: arg}
	}
	return s.conn.QueryContext(context.Background(), s.query, named)
}

type sqliteTx struct {
	conn *sqliteConn
	done bool
}

func (t sqliteTx) Commit() error {
	if t.done {
		return errors.New("vpf_sqlite: transaction already finished")
	}
	_, err := t.conn.ExecContext(context.Background(), "COMMIT", nil)
	return err
}

func (t sqliteTx) Rollback() error {
	if t.done {
		return errors.New("vpf_sqlite: transaction already finished")
	}
	_, err := t.conn.ExecContext(context.Background(), "ROLLBACK", nil)
	return err
}

type sqliteResult struct {
	changes int64
}

func (r sqliteResult) LastInsertId() (int64, error) { return 0, nil }
func (r sqliteResult) RowsAffected() (int64, error) { return r.changes, nil }

type sqliteRows struct {
	conn    *sqliteConn
	stmt    *C.sqlite3_stmt
	columns []string
	closed  bool
}

func (r *sqliteRows) Columns() []string { return r.columns }

func (r *sqliteRows) Close() error {
	if !r.closed && r.stmt != nil {
		C.vpf_finalize(r.stmt)
	}
	r.closed = true
	return nil
}

func (r *sqliteRows) Next(dest []driver.Value) error {
	rc := C.vpf_step(r.stmt)
	if rc == 101 {
		return io.EOF
	}
	if rc != 100 {
		return r.conn.errorf("row step", rc)
	}
	for i := range r.columns {
		switch C.vpf_column_type(r.stmt, C.int(i)) {
		case 1:
			dest[i] = int64(C.vpf_column_int64(r.stmt, C.int(i)))
		case 2:
			// The checkpoint schema has no floating-point columns; retain a
			// deterministic zero if an unexpected REAL column appears.
			dest[i] = float64(0)
		case 3:
			ptr := C.vpf_column_text(r.stmt, C.int(i))
			if ptr == nil {
				dest[i] = ""
			} else {
				dest[i] = C.GoString((*C.char)(unsafe.Pointer(ptr)))
			}
		case 4:
			bytes := C.vpf_column_blob(r.stmt, C.int(i))
			n := int(C.vpf_column_bytes(r.stmt, C.int(i)))
			if bytes == nil || n == 0 {
				dest[i] = []byte{}
			} else {
				dest[i] = C.GoBytes(bytes, C.int(n))
			}
		case 5:
			dest[i] = nil
		default:
			dest[i] = nil
		}
	}
	return nil
}
