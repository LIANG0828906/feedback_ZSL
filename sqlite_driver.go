//go:build cgo

package leasekit

/*
#cgo LDFLAGS: -L/usr/lib/x86_64-linux-gnu -L/usr/lib/aarch64-linux-gnu -L/usr/lib64 -L/usr/lib -l:libsqlite3.so.0
#include <stdlib.h>

typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;

extern int sqlite3_prepare_v2(sqlite3 *db, const char *zSql, int nByte, sqlite3_stmt **ppStmt, const char **pzTail);
extern int sqlite3_step(sqlite3_stmt *pStmt);
extern int sqlite3_finalize(sqlite3_stmt *pStmt);
extern int sqlite3_column_count(sqlite3_stmt *pStmt);
extern const char *sqlite3_column_name(sqlite3_stmt *pStmt, int N);
extern const unsigned char *sqlite3_column_text(sqlite3_stmt *pStmt, int iCol);
extern const char *sqlite3_errmsg(sqlite3 *db);
*/
import "C"

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"unsafe"
)

func init() {
	sql.Register("lease-sqlite3", &leaseSQLiteDriver{})
}

type leaseSQLiteDriver struct{}

func (d *leaseSQLiteDriver) Open(name string) (driver.Conn, error) {
	db, err := openSQLite(name)
	if err != nil {
		return nil, err
	}
	return &leaseSQLiteConn{db: db}, nil
}

type leaseSQLiteConn struct {
	db *sqliteDB
}

func (c *leaseSQLiteConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("lease-sqlite3: prepared statements are not used by checkpoint store")
}

func (c *leaseSQLiteConn) Close() error {
	if c.db == nil {
		return nil
	}
	return c.db.close()
}

func (c *leaseSQLiteConn) Begin() (driver.Tx, error) {
	return nil, errors.New("lease-sqlite3: explicit transactions are encoded in checkpoint SQL")
}

func (c *leaseSQLiteConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if len(args) != 0 {
		return nil, errors.New("lease-sqlite3: checkpoint exec does not bind arguments")
	}
	if err := c.db.exec(query); err != nil {
		return nil, err
	}
	return leaseExecResult{}, nil
}

func (c *leaseSQLiteConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if len(args) != 0 {
		return nil, errors.New("lease-sqlite3: checkpoint query does not bind arguments")
	}
	cols, rows, err := c.db.queryValues(query)
	if err != nil {
		return nil, err
	}
	return &leaseRows{columns: cols, rows: rows}, nil
}

type leaseExecResult struct{}

func (leaseExecResult) LastInsertId() (int64, error) { return 0, nil }
func (leaseExecResult) RowsAffected() (int64, error) { return 0, nil }

type leaseRows struct {
	columns []string
	rows    [][]driver.Value
	pos     int
}

func (r *leaseRows) Columns() []string { return r.columns }
func (r *leaseRows) Close() error      { return nil }

func (r *leaseRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	for i := range dest {
		if i < len(r.rows[r.pos]) {
			dest[i] = r.rows[r.pos][i]
		}
	}
	r.pos++
	return nil
}

func (db *sqliteDB) queryValues(query string) ([]string, [][]driver.Value, error) {
	cquery := C.CString(query)
	defer C.free(unsafe.Pointer(cquery))
	var stmt *C.sqlite3_stmt
	rc := C.sqlite3_prepare_v2(db.ptr, cquery, -1, &stmt, nil)
	if rc != sqliteOK {
		msg := C.GoString(C.sqlite3_errmsg(db.ptr))
		return nil, nil, classifySQLiteError(validationError(CodeCheckpointCorrupt, "sqlite", "prepare rc=%d: %s", int(rc), msg))
	}
	defer C.sqlite3_finalize(stmt)

	count := int(C.sqlite3_column_count(stmt))
	columns := make([]string, count)
	for i := 0; i < count; i++ {
		name := C.sqlite3_column_name(stmt, C.int(i))
		columns[i] = C.GoString(name)
	}

	var rows [][]driver.Value
	for {
		stepRC := C.sqlite3_step(stmt)
		if stepRC == sqliteDone {
			break
		}
		if stepRC != sqliteRow {
			msg := C.GoString(C.sqlite3_errmsg(db.ptr))
			return nil, nil, classifySQLiteError(validationError(CodeCheckpointCorrupt, "sqlite", "step rc=%d: %s", int(stepRC), msg))
		}
		row := make([]driver.Value, count)
		for i := 0; i < count; i++ {
			text := C.sqlite3_column_text(stmt, C.int(i))
			if text != nil {
				row[i] = C.GoString((*C.char)(unsafe.Pointer(text)))
			} else {
				row[i] = nil
			}
		}
		rows = append(rows, row)
	}
	return columns, rows, nil
}

var _ driver.Driver = (*leaseSQLiteDriver)(nil)
var _ driver.Conn = (*leaseSQLiteConn)(nil)
var _ driver.ExecerContext = (*leaseSQLiteConn)(nil)
var _ driver.QueryerContext = (*leaseSQLiteConn)(nil)
var _ driver.Rows = (*leaseRows)(nil)
