package dialoguecal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

const localDBMagic = "dialoguecal.localsqlite.v1"

var (
	localDBMu       sync.Mutex
	localDBRegistry = make(map[string]*localDisk)
	localDBSeq      atomic.Uint64
)

type localDisk struct {
	mu    sync.Mutex
	path  string
	state *localDatabase
}

type localDatabase struct {
	Magic      string                 `json:"magic"`
	Generation int64                  `json:"generation"`
	Tables     map[string]*localTable `json:"tables"`
}

type localTable struct {
	Columns []string                  `json:"columns"`
	Types   map[string]string         `json:"types"`
	Rows    []map[string]driver.Value `json:"rows"`
}

func newLocalDatabase() *localDatabase {
	return &localDatabase{Magic: localDBMagic, Tables: make(map[string]*localTable)}
}

func (db *localDatabase) clone() *localDatabase {
	raw, _ := json.Marshal(db)
	var out localDatabase
	_ = json.Unmarshal(raw, &out)
	if out.Tables == nil {
		out.Tables = make(map[string]*localTable)
	}
	normalizeLocalDatabase(&out)
	return &out
}

func normalizeLocalDatabase(db *localDatabase) {
	for _, tbl := range db.Tables {
		for _, row := range tbl.Rows {
			for col, v := range row {
				if tbl.Types[col] == "INTEGER" {
					switch n := v.(type) {
					case float64:
						row[col] = int64(n)
					case json.Number:
						x, _ := n.Int64()
						row[col] = x
					}
				}
			}
		}
	}
}

func getLocalDisk(path string) (*localDisk, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	localDBMu.Lock()
	defer localDBMu.Unlock()
	if d := localDBRegistry[abs]; d != nil {
		return d, nil
	}
	d := &localDisk{path: abs, state: newLocalDatabase()}
	data, err := os.ReadFile(abs)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(data) > 0 {
		var loaded localDatabase
		if err := json.Unmarshal(data, &loaded); err != nil {
			return nil, fmt.Errorf("open local sqlite file: %w", err)
		}
		if loaded.Magic != localDBMagic {
			return nil, fmt.Errorf("open local sqlite file: bad magic %q", loaded.Magic)
		}
		normalizeLocalDatabase(&loaded)
		if loaded.Tables == nil {
			loaded.Tables = make(map[string]*localTable)
		}
		d.state = &loaded
	}
	localDBRegistry[abs] = d
	return d, nil
}

func (d *localDisk) persist(state *localDatabase) error {
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return err
	}
	state.Magic = localDBMagic
	state.Generation++
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%d", d.path, os.Getpid(), localDBSeq.Add(1))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, d.path); err != nil {
		return err
	}
	ok = true
	if dir, err := os.Open(filepath.Dir(d.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

type localDriver struct{}

func (d *localDriver) Open(name string) (driver.Conn, error) {
	disk, err := getLocalDisk(name)
	if err != nil {
		return nil, err
	}
	return &localConn{disk: disk}, nil
}

func init() {
	sql.Register("dialoguecal-sqlite", &localDriver{})
}

type localConn struct {
	disk *localDisk
	tx   *localTx
}

func (c *localConn) Prepare(query string) (driver.Stmt, error) {
	return &localStmt{conn: c, query: query}, nil
}

func (c *localConn) Close() error {
	return nil
}

func (c *localConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *localConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.disk.mu.Lock()
	if c.tx != nil {
		c.disk.mu.Unlock()
		return nil, fmt.Errorf("nested local transaction")
	}
	c.tx = &localTx{conn: c, state: c.disk.state.clone()}
	return c.tx, nil
}

func (c *localConn) Ping(ctx context.Context) error { return nil }

func (c *localConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.tx != nil {
		return execLocal(c.tx.state, query, namedValues(args))
	}
	c.disk.mu.Lock()
	defer c.disk.mu.Unlock()
	result, err := execLocal(c.disk.state, query, namedValues(args))
	if err != nil {
		return nil, err
	}
	if err := c.disk.persist(c.disk.state); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *localConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	state, unlock, err := c.readState()
	if err != nil {
		return nil, err
	}
	if unlock != nil {
		defer unlock()
	}
	return queryLocal(state, query, namedValues(args))
}

func (c *localConn) readState() (*localDatabase, func(), error) {
	if c.tx != nil {
		return c.tx.state, nil, nil
	}
	c.disk.mu.Lock()
	return c.disk.state, c.disk.mu.Unlock, nil
}

type localTx struct {
	conn  *localConn
	state *localDatabase
	done  bool
}

func (tx *localTx) Commit() error {
	if tx.done {
		return driver.ErrBadConn
	}
	tx.done = true
	err := tx.conn.disk.persist(tx.state)
	if err == nil {
		tx.conn.disk.state = tx.state
	}
	tx.conn.tx = nil
	tx.conn.disk.mu.Unlock()
	return err
}

func (tx *localTx) Rollback() error {
	if tx.done {
		return driver.ErrBadConn
	}
	tx.done = true
	tx.conn.tx = nil
	tx.conn.disk.mu.Unlock()
	return nil
}

type localStmt struct {
	conn  *localConn
	query string
}

func (s *localStmt) Close() error  { return nil }
func (s *localStmt) NumInput() int { return -1 }

func (s *localStmt) Exec(args []driver.Value) (driver.Result, error) {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return s.conn.ExecContext(context.Background(), s.query, named)
}

func (s *localStmt) Query(args []driver.Value) (driver.Rows, error) {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return s.conn.QueryContext(context.Background(), s.query, named)
}

type localResult struct {
	affected int64
}

func (r localResult) LastInsertId() (int64, error) { return 0, nil }
func (r localResult) RowsAffected() (int64, error) { return r.affected, nil }

type localRows struct {
	cols []string
	rows [][]driver.Value
	idx  int
}

func (r *localRows) Columns() []string { return r.cols }
func (r *localRows) Close() error      { return nil }
func (r *localRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.idx])
	r.idx++
	return nil
}

type localValueConverter struct{}

func (localValueConverter) ConvertValue(v any) (driver.Value, error) {
	switch x := v.(type) {
	case nil, int64, float64, bool, []byte, string:
		return x, nil
	case int:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case uint:
		return int64(x), nil
	case uint64:
		if x > 1<<63-1 {
			return nil, fmt.Errorf("uint64 value overflows int64")
		}
		return int64(x), nil
	case json.Number:
		return x.Int64()
	default:
		return nil, fmt.Errorf("unsupported local sqlite value %T", v)
	}
}

func namedValues(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}

func execLocal(db *localDatabase, query string, args []driver.Value) (driver.Result, error) {
	stmt, err := parseLocalSQL(query, args)
	if err != nil {
		return nil, err
	}
	switch stmt.kind {
	case "create":
		if _, ok := db.Tables[stmt.table]; !ok || !stmt.ifNotExists {
			tbl := &localTable{Columns: stmt.columns, Types: stmt.types, Rows: []map[string]driver.Value{}}
			db.Tables[stmt.table] = tbl
		}
		return localResult{}, nil
	case "insert":
		tbl := db.Tables[stmt.table]
		if tbl == nil {
			return nil, fmt.Errorf("table %s does not exist", stmt.table)
		}
		row := make(map[string]driver.Value, len(stmt.columns))
		for i, col := range stmt.columns {
			row[col] = stmt.values[i]
		}
		tbl.Rows = append(tbl.Rows, row)
		return localResult{affected: 1}, nil
	case "delete":
		tbl := db.Tables[stmt.table]
		if tbl == nil {
			return localResult{}, nil
		}
		kept := tbl.Rows[:0]
		var affected int64
		for _, row := range tbl.Rows {
			if localWhereMatch(row, stmt.where) {
				affected++
				continue
			}
			kept = append(kept, row)
		}
		tbl.Rows = kept
		return localResult{affected: affected}, nil
	case "pragma":
		return localResult{}, nil
	default:
		return nil, fmt.Errorf("unsupported local sqlite exec statement")
	}
}

func queryLocal(db *localDatabase, query string, args []driver.Value) (driver.Rows, error) {
	stmt, err := parseLocalSQL(query, args)
	if err != nil {
		return nil, err
	}
	if stmt.kind != "select" {
		return nil, fmt.Errorf("not a select statement")
	}
	tbl := db.Tables[stmt.table]
	if tbl == nil {
		return nil, fmt.Errorf("table %s does not exist", stmt.table)
	}
	cols := stmt.columns
	if len(cols) == 0 {
		cols = append([]string(nil), tbl.Columns...)
	}
	rows := &localRows{cols: cols}
	for _, row := range tbl.Rows {
		if !localWhereMatch(row, stmt.where) {
			continue
		}
		values := make([]driver.Value, len(cols))
		for i, col := range cols {
			values[i] = row[col]
		}
		rows.rows = append(rows.rows, values)
	}
	return rows, nil
}

type localSQL struct {
	kind        string
	table       string
	ifNotExists bool
	columns     []string
	types       map[string]string
	values      []driver.Value
	where       []localWhere
}

type localWhere struct {
	column string
	value  driver.Value
}

func parseLocalSQL(query string, args []driver.Value) (*localSQL, error) {
	q := strings.TrimSpace(strings.TrimSuffix(query, ";"))
	upper := strings.ToUpper(q)
	switch {
	case strings.HasPrefix(upper, "CREATE"):
		return parseCreate(q)
	case strings.HasPrefix(upper, "INSERT"):
		return parseInsert(q, args)
	case strings.HasPrefix(upper, "DELETE"):
		return parseDelete(q, args)
	case strings.HasPrefix(upper, "SELECT"):
		return parseSelect(q, args)
	case strings.HasPrefix(upper, "PRAGMA"):
		return &localSQL{kind: "pragma"}, nil
	default:
		return nil, fmt.Errorf("unsupported local sqlite SQL: %s", q)
	}
}

func parseCreate(q string) (*localSQL, error) {
	open := strings.Index(q, "(")
	close := strings.LastIndex(q, ")")
	if open < 0 || close <= open {
		return nil, fmt.Errorf("invalid CREATE TABLE")
	}
	head := strings.Fields(q[:open])
	if len(head) < 3 {
		return nil, fmt.Errorf("invalid CREATE TABLE")
	}
	table := strings.Trim(head[len(head)-1], "`\"")
	ifNotExists := false
	for i := range head {
		if strings.EqualFold(head[i], "IF") {
			ifNotExists = true
		}
	}
	stmt := &localSQL{kind: "create", table: table, ifNotExists: ifNotExists, types: make(map[string]string)}
	for _, def := range splitTopLevel(q[open+1:close], ',') {
		fields := strings.Fields(strings.TrimSpace(def))
		if len(fields) == 0 {
			continue
		}
		if strings.EqualFold(fields[0], "PRIMARY") || strings.EqualFold(fields[0], "UNIQUE") {
			continue
		}
		col := strings.Trim(fields[0], "`\"")
		typ := "TEXT"
		if len(fields) > 1 {
			typ = strings.ToUpper(fields[1])
		}
		stmt.columns = append(stmt.columns, col)
		stmt.types[col] = typ
	}
	return stmt, nil
}

func parseInsert(q string, args []driver.Value) (*localSQL, error) {
	up := strings.ToUpper(q)
	into := strings.Index(up, "INTO")
	if into < 0 {
		return nil, fmt.Errorf("invalid INSERT")
	}
	rest := strings.TrimSpace(q[into+4:])
	open := strings.Index(rest, "(")
	if open < 0 {
		return nil, fmt.Errorf("invalid INSERT columns")
	}
	table := strings.TrimSpace(rest[:open])
	close := strings.Index(rest[open:], ")")
	if close < 0 {
		return nil, fmt.Errorf("invalid INSERT columns")
	}
	cols := []string{}
	for _, c := range strings.Split(rest[open+1:open+close], ",") {
		cols = append(cols, strings.Trim(strings.TrimSpace(c), "`\""))
	}
	valuesPos := strings.Index(strings.ToUpper(rest), "VALUES")
	if valuesPos < 0 {
		return nil, fmt.Errorf("invalid INSERT values")
	}
	valPart := strings.TrimSpace(rest[valuesPos+6:])
	if !strings.HasPrefix(valPart, "(") {
		return nil, fmt.Errorf("invalid INSERT values")
	}
	placeholders := strings.Count(valPart, "?")
	if placeholders != len(cols) || placeholders != len(args) {
		return nil, fmt.Errorf("INSERT placeholder mismatch")
	}
	return &localSQL{kind: "insert", table: table, columns: cols, values: append([]driver.Value(nil), args...)}, nil
}

func parseDelete(q string, args []driver.Value) (*localSQL, error) {
	up := strings.ToUpper(q)
	from := strings.Index(up, "FROM")
	if from < 0 {
		return nil, fmt.Errorf("invalid DELETE")
	}
	rest := strings.TrimSpace(q[from+4:])
	wherePos := strings.Index(strings.ToUpper(rest), " WHERE ")
	table := rest
	wherePart := ""
	if wherePos >= 0 {
		table = strings.TrimSpace(rest[:wherePos])
		wherePart = strings.TrimSpace(rest[wherePos+7:])
	}
	where, err := parseWhere(wherePart, args)
	if err != nil {
		return nil, err
	}
	return &localSQL{kind: "delete", table: strings.Trim(table, "`\""), where: where}, nil
}

func parseSelect(q string, args []driver.Value) (*localSQL, error) {
	up := strings.ToUpper(q)
	from := strings.Index(up, " FROM ")
	if from < 0 {
		return nil, fmt.Errorf("invalid SELECT")
	}
	fieldPart := strings.TrimSpace(q[6:from])
	rest := strings.TrimSpace(q[from+6:])
	wherePos := strings.Index(strings.ToUpper(rest), " WHERE ")
	table := rest
	wherePart := ""
	if wherePos >= 0 {
		table = strings.TrimSpace(rest[:wherePos])
		wherePart = strings.TrimSpace(rest[wherePos+7:])
	}
	var cols []string
	if fieldPart != "*" {
		for _, c := range strings.Split(fieldPart, ",") {
			cols = append(cols, strings.Trim(strings.TrimSpace(c), "`\""))
		}
	}
	where, err := parseWhere(wherePart, args)
	if err != nil {
		return nil, err
	}
	return &localSQL{kind: "select", table: strings.Trim(table, "`\""), columns: cols, where: where}, nil
}

func parseWhere(part string, args []driver.Value) ([]localWhere, error) {
	if strings.TrimSpace(part) == "" {
		if len(args) != 0 {
			return nil, fmt.Errorf("WHERE placeholder mismatch")
		}
		return nil, nil
	}
	clauses := strings.Split(part, " AND ")
	if len(clauses) != len(args) {
		return nil, fmt.Errorf("WHERE placeholder mismatch")
	}
	out := make([]localWhere, 0, len(clauses))
	for i, clause := range clauses {
		eq := strings.Index(clause, "=")
		if eq < 0 || !strings.Contains(clause[eq+1:], "?") {
			return nil, fmt.Errorf("only equality WHERE clauses are supported: %s", clause)
		}
		col := strings.Trim(strings.TrimSpace(clause[:eq]), "`\"")
		out = append(out, localWhere{column: col, value: args[i]})
	}
	return out, nil
}

func splitTopLevel(s string, sep rune) []string {
	var out []string
	start := 0
	depth := 0
	quote := rune(0)
	for i, r := range s {
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		switch r {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if r == sep && depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if start <= len(s) {
		out = append(out, s[start:])
	}
	return out
}

func localWhereMatch(row map[string]driver.Value, where []localWhere) bool {
	for _, w := range where {
		v, ok := row[w.column]
		if !ok || !localValuesEqual(v, w.value) {
			return false
		}
	}
	return true
}

func localValuesEqual(a, b driver.Value) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

var _ driver.Driver = (*localDriver)(nil)
var _ driver.Conn = (*localConn)(nil)
var _ driver.ConnBeginTx = (*localConn)(nil)
var _ driver.ExecerContext = (*localConn)(nil)
var _ driver.QueryerContext = (*localConn)(nil)
var _ driver.Pinger = (*localConn)(nil)
var _ driver.Stmt = (*localStmt)(nil)
var _ driver.ValueConverter = localValueConverter{}
