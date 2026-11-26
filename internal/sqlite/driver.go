//go:build cgo && linux

package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"sync"
	"time"
	"unsafe"
)

/*
#cgo LDFLAGS: -L/usr/lib/x86_64-linux-gnu -L/lib/x86_64-linux-gnu -l:libsqlite3.so.0

#include <stdlib.h>

typedef long long sqlite3_int64;
typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;

int sqlite3_open_v2(const char*, sqlite3**, int, const char*);
int sqlite3_close_v2(sqlite3*);
const char* sqlite3_errmsg(sqlite3*);
int sqlite3_busy_timeout(sqlite3*, int);
int sqlite3_extended_result_codes(sqlite3*, int);
int sqlite3_prepare_v2(sqlite3*, const char*, int, sqlite3_stmt**, const char**);
int sqlite3_finalize(sqlite3_stmt*);
int sqlite3_reset(sqlite3_stmt*);
int sqlite3_step(sqlite3_stmt*);
int sqlite3_bind_null(sqlite3_stmt*, int);
int sqlite3_bind_int64(sqlite3_stmt*, int, sqlite3_int64);
int sqlite3_bind_double(sqlite3_stmt*, int, double);
int sqlite3_bind_text(sqlite3_stmt*, int, const char*, int, void (*)(void*));
int sqlite3_bind_blob(sqlite3_stmt*, int, const void*, int, void (*)(void*));
int sqlite3_column_count(sqlite3_stmt*);
const char* sqlite3_column_name(sqlite3_stmt*, int);
int sqlite3_column_type(sqlite3_stmt*, int);
sqlite3_int64 sqlite3_column_int64(sqlite3_stmt*, int);
double sqlite3_column_double(sqlite3_stmt*, int);
const unsigned char* sqlite3_column_text(sqlite3_stmt*, int);
const void* sqlite3_column_blob(sqlite3_stmt*, int);
int sqlite3_column_bytes(sqlite3_stmt*, int);
int sqlite3_changes(sqlite3*);
sqlite3_int64 sqlite3_last_insert_rowid(sqlite3*);

static int warmcache_bind_text(sqlite3_stmt* stmt, int index, const char* value, int size) {
	return sqlite3_bind_text(stmt, index, value, size, (void (*)(void*))-1);
}

static int warmcache_bind_blob(sqlite3_stmt* stmt, int index, const void* value, int size) {
	return sqlite3_bind_blob(stmt, index, value, size, (void (*)(void*))-1);
}
*/
import "C"

const (
	sqliteOK      = 0
	sqliteRow     = 100
	sqliteDone    = 101
	openReadWrite = 0x00000002
	openCreate    = 0x00000004
	openFullMutex = 0x00010000
)

func init() {
	sql.Register("sqlite3", &Driver{})
	sql.Register("warmcache-sqlite3", &Driver{})
}

type Driver struct{}

func (d *Driver) Open(name string) (driver.Conn, error) {
	if name == "" {
		name = ":memory:"
	}
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))
	var handle *C.sqlite3
	rc := C.sqlite3_open_v2(cName, &handle, C.int(openReadWrite|openCreate|openFullMutex), nil)
	if rc != sqliteOK {
		message := "unable to open SQLite database"
		if handle != nil {
			message = C.GoString(C.sqlite3_errmsg(handle))
			C.sqlite3_close_v2(handle)
		}
		return nil, fmt.Errorf("sqlite open: %s", message)
	}
	C.sqlite3_extended_result_codes(handle, 1)
	C.sqlite3_busy_timeout(handle, 5000)
	connection := &conn{db: handle}
	if _, err := connection.execRaw("PRAGMA journal_mode=WAL"); err != nil {
		_ = connection.Close()
		return nil, err
	}
	if _, err := connection.execRaw("PRAGMA synchronous=FULL"); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return connection, nil
}

type conn struct {
	db     *C.sqlite3
	closed bool
	mu     sync.Mutex
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prepare(query)
}

func (c *conn) prepare(query string) (*stmt, error) {
	if c.closed || c.db == nil {
		return nil, driver.ErrBadConn
	}
	cQuery := C.CString(query)
	defer C.free(unsafe.Pointer(cQuery))
	var statement *C.sqlite3_stmt
	rc := C.sqlite3_prepare_v2(c.db, cQuery, -1, &statement, nil)
	if rc != sqliteOK {
		return nil, c.error("prepare", rc)
	}
	return &stmt{conn: c, statement: statement}, nil
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.db == nil {
		return nil
	}
	rc := C.sqlite3_close_v2(c.db)
	c.db = nil
	if rc != sqliteOK {
		return fmt.Errorf("sqlite close: code %d", int(rc))
	}
	return nil
}

func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *conn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if opts.Isolation != driver.IsolationLevel(sql.LevelDefault) && opts.Isolation != driver.IsolationLevel(sql.LevelSerializable) {
		return nil, fmt.Errorf("sqlite: unsupported isolation level %d", opts.Isolation)
	}
	if _, err := c.execRaw("BEGIN IMMEDIATE"); err != nil {
		return nil, err
	}
	return &tx{conn: c}, nil
}

func (c *conn) Ping(context.Context) error {
	if c.closed || c.db == nil {
		return driver.ErrBadConn
	}
	return nil
}

func (c *conn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	statement, err := c.Prepare(query)
	if err != nil {
		return nil, err
	}
	defer statement.Close()
	values := make([]driver.Value, len(args))
	for index := range args {
		values[index] = args[index].Value
	}
	return statement.Exec(values)
}

func (c *conn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	statement, err := c.Prepare(query)
	if err != nil {
		return nil, err
	}
	values := make([]driver.Value, len(args))
	for index := range args {
		values[index] = args[index].Value
	}
	queryRows, err := statement.Query(values)
	if err != nil {
		_ = statement.Close()
		return nil, err
	}
	return queryRows, nil
}

func (c *conn) execRaw(query string) (int64, error) {
	statement, err := c.prepare(query)
	if err != nil {
		return 0, err
	}
	defer statement.Close()
	return statement.execValues(nil)
}

func (c *conn) error(op string, rc C.int) error {
	message := fmt.Sprintf("sqlite code %d", int(rc))
	if c.db != nil {
		message = C.GoString(C.sqlite3_errmsg(c.db))
	}
	return fmt.Errorf("sqlite %s: %s", op, message)
}

type tx struct {
	conn   *conn
	closed bool
}

func (t *tx) Commit() error {
	if t.closed {
		return driver.ErrBadConn
	}
	t.closed = true
	_, err := t.conn.execRaw("COMMIT")
	return err
}

func (t *tx) Rollback() error {
	if t.closed {
		return nil
	}
	t.closed = true
	_, err := t.conn.execRaw("ROLLBACK")
	return err
}

type stmt struct {
	conn      *conn
	statement *C.sqlite3_stmt
	closed    bool
	mu        sync.Mutex
}

func (s *stmt) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.statement != nil {
		C.sqlite3_finalize(s.statement)
		s.statement = nil
	}
	return nil
}

func (s *stmt) NumInput() int {
	return -1
}

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	affected, err := s.execValues(args)
	if err != nil {
		return nil, err
	}
	lastID := int64(C.sqlite3_last_insert_rowid(s.conn.db))
	return result{affected: affected, lastID: lastID}, nil
}

func (s *stmt) execValues(args []driver.Value) (int64, error) {
	if err := s.bind(args); err != nil {
		return 0, err
	}
	for {
		rc := C.sqlite3_step(s.statement)
		if rc == sqliteDone {
			break
		}
		if rc == sqliteRow {
			continue
		}
		err := s.conn.error("step", rc)
		C.sqlite3_reset(s.statement)
		return 0, err
	}
	affected := int64(C.sqlite3_changes(s.conn.db))
	C.sqlite3_reset(s.statement)
	return affected, nil
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.bind(args); err != nil {
		return nil, err
	}
	count := int(C.sqlite3_column_count(s.statement))
	columns := make([]string, count)
	for index := 0; index < count; index++ {
		columns[index] = C.GoString(C.sqlite3_column_name(s.statement, C.int(index)))
	}
	return &rows{statement: s.statement, conn: s.conn, columns: columns}, nil
}

func (s *stmt) bind(args []driver.Value) error {
	if s.closed || s.statement == nil {
		return driver.ErrBadConn
	}
	C.sqlite3_reset(s.statement)
	for index, value := range args {
		position := C.int(index + 1)
		var rc C.int
		switch typed := value.(type) {
		case nil:
			rc = C.sqlite3_bind_null(s.statement, position)
		case int64:
			rc = C.sqlite3_bind_int64(s.statement, position, C.sqlite3_int64(typed))
		case int:
			rc = C.sqlite3_bind_int64(s.statement, position, C.sqlite3_int64(typed))
		case int32:
			rc = C.sqlite3_bind_int64(s.statement, position, C.sqlite3_int64(typed))
		case float64:
			rc = C.sqlite3_bind_double(s.statement, position, C.double(typed))
		case bool:
			number := int64(0)
			if typed {
				number = 1
			}
			rc = C.sqlite3_bind_int64(s.statement, position, C.sqlite3_int64(number))
		case time.Time:
			text := typed.UTC().Format(time.RFC3339Nano)
			cText := C.CString(text)
			rc = C.warmcache_bind_text(s.statement, position, cText, C.int(len(text)))
			C.free(unsafe.Pointer(cText))
		case string:
			cText := C.CString(typed)
			rc = C.warmcache_bind_text(s.statement, position, cText, C.int(len(typed)))
			C.free(unsafe.Pointer(cText))
		case []byte:
			if len(typed) == 0 {
				rc = C.warmcache_bind_blob(s.statement, position, nil, 0)
			} else {
				rc = C.warmcache_bind_blob(s.statement, position, unsafe.Pointer(&typed[0]), C.int(len(typed)))
			}
		default:
			text := fmt.Sprint(typed)
			cText := C.CString(text)
			rc = C.warmcache_bind_text(s.statement, position, cText, C.int(len(text)))
			C.free(unsafe.Pointer(cText))
		}
		if rc != sqliteOK {
			return s.conn.error("bind", rc)
		}
	}
	return nil
}

type result struct {
	affected int64
	lastID   int64
}

func (r result) LastInsertId() (int64, error) { return r.lastID, nil }
func (r result) RowsAffected() (int64, error) { return r.affected, nil }

type rows struct {
	statement *C.sqlite3_stmt
	conn      *conn
	columns   []string
	closed    bool
}

func (r *rows) Columns() []string {
	return r.columns
}

func (r *rows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.statement != nil {
		C.sqlite3_finalize(r.statement)
		r.statement = nil
	}
	return nil
}

func (r *rows) Next(dest []driver.Value) error {
	if r.closed || r.statement == nil {
		return io.EOF
	}
	rc := C.sqlite3_step(r.statement)
	if rc == sqliteDone {
		return io.EOF
	}
	if rc != sqliteRow {
		return r.conn.error("rows", rc)
	}
	for index := range dest {
		column := C.int(index)
		switch C.sqlite3_column_type(r.statement, column) {
		case 1:
			dest[index] = int64(C.sqlite3_column_int64(r.statement, column))
		case 2:
			dest[index] = float64(C.sqlite3_column_double(r.statement, column))
		case 3:
			pointer := C.sqlite3_column_text(r.statement, column)
			size := C.sqlite3_column_bytes(r.statement, column)
			if pointer == nil {
				dest[index] = ""
			} else {
				dest[index] = C.GoStringN((*C.char)(unsafe.Pointer(pointer)), size)
			}
		case 4:
			pointer := C.sqlite3_column_blob(r.statement, column)
			size := C.sqlite3_column_bytes(r.statement, column)
			if pointer == nil || size == 0 {
				dest[index] = []byte{}
			} else {
				dest[index] = C.GoBytes(pointer, size)
			}
		default:
			dest[index] = nil
		}
	}
	return nil
}

var _ driver.Driver = (*Driver)(nil)
var _ driver.Conn = (*conn)(nil)
var _ driver.ConnBeginTx = (*conn)(nil)
var _ driver.ExecerContext = (*conn)(nil)
var _ driver.QueryerContext = (*conn)(nil)
var _ driver.Pinger = (*conn)(nil)
var _ driver.Stmt = (*stmt)(nil)
var _ driver.Rows = (*rows)(nil)
