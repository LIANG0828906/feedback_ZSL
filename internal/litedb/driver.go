package litedb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"screenrelay/internal/model"
)

const DriverName = "sqlitefile"

func init() {
	sql.Register(DriverName, &Driver{})
}

type Driver struct{}

func (d *Driver) Open(dsn string) (driver.Conn, error) {
	if dsn == "" {
		return nil, model.NewError(model.EInternal, "litedb: empty DSN/path")
	}
	store, err := openStore(dsn)
	if err != nil {
		return nil, err
	}
	return &Conn{store: store}, nil
}

type Table struct {
	Name    string              `json:"name"`
	Columns []string            `json:"columns"`
	Rows    []map[string]string `json:"rows"`
}

type fileData struct {
	Tables map[string]*Table `json:"tables"`
}

type Store struct {
	path string
	mu   sync.Mutex
	data fileData
}

var (
	storesMu sync.Mutex
)

func openStore(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	storesMu.Lock()
	defer storesMu.Unlock()
	store := &Store{path: abs, data: fileData{Tables: map[string]*Table{}}}
	if raw, err := os.ReadFile(abs); err == nil {
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &store.data); err != nil {
				return nil, model.NewError(model.EDecode, "litedb corrupt file %s: %v", abs, err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if store.data.Tables == nil {
		store.data.Tables = map[string]*Table{}
	}
	return store, nil
}

func (s *Store) persistLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}

type Conn struct {
	store *Store
	tx    *fileTx
}

type fileTx struct {
	conn     *Conn
	snapshot fileData
	done     bool
}

func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	return &Stmt{conn: c, query: query}, nil
}

func (c *Conn) Close() error { return nil }

func (c *Conn) Begin() (driver.Tx, error) {
	c.store.mu.Lock()
	raw, err := json.Marshal(c.store.data)
	if err != nil {
		c.store.mu.Unlock()
		return nil, err
	}
	var snapshot fileData
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		c.store.mu.Unlock()
		return nil, err
	}
	if snapshot.Tables == nil {
		snapshot.Tables = map[string]*Table{}
	}
	c.tx = &fileTx{conn: c, snapshot: snapshot}
	return c.tx, nil
}

func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.Begin()
}

func (t *fileTx) Commit() error {
	if t.done {
		return sql.ErrTxDone
	}
	t.done = true
	defer func() {
		t.conn.tx = nil
		t.conn.store.mu.Unlock()
	}()
	return t.conn.store.persistLocked()
}

func (t *fileTx) Rollback() error {
	if t.done {
		return sql.ErrTxDone
	}
	t.done = true
	t.conn.store.data = t.snapshot
	t.conn.tx = nil
	t.conn.store.mu.Unlock()
	return nil
}

type Stmt struct {
	conn  *Conn
	query string
}

func (s *Stmt) Close() error  { return nil }
func (s *Stmt) NumInput() int { return strings.Count(s.query, "?") }

func (s *Stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.conn.ExecContext(context.Background(), s.query, valuesToNamed(args))
}

func (s *Stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.conn.QueryContext(context.Background(), s.query, valuesToNamed(args))
}

func valuesToNamed(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, arg := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: arg}
	}
	return out
}

func (c *Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	values := namedValues(args)
	if c.tx == nil {
		c.store.mu.Lock()
		defer c.store.mu.Unlock()
	}
	return execLocked(&c.store.data, query, values)
}

func (c *Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	values := namedValues(args)
	if c.tx == nil {
		c.store.mu.Lock()
		defer c.store.mu.Unlock()
	}
	return queryLocked(&c.store.data, query, values)
}

func namedValues(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, arg := range args {
		out[i] = arg.Value
	}
	return out
}

func execLocked(data *fileData, query string, args []driver.Value) (driver.Result, error) {
	q := normalize(query)
	upper := strings.ToUpper(q)
	switch {
	case strings.HasPrefix(upper, "CREATE TABLE"):
		return createTable(data, q)
	case strings.HasPrefix(upper, "INSERT "):
		return insert(data, q, args)
	default:
		return nil, fmt.Errorf("sqlitefile: unsupported exec statement: %s", q)
	}
}

func normalize(query string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(query)), " ")
}

func createTable(data *fileData, query string) (driver.Result, error) {
	upper := strings.ToUpper(query)
	name := ""
	if idx := strings.Index(upper, "TABLE IF NOT EXISTS"); idx >= 0 {
		rest := strings.TrimSpace(query[idx+len("TABLE IF NOT EXISTS"):])
		name = firstToken(rest)
	} else if idx := strings.Index(upper, "TABLE"); idx >= 0 {
		rest := strings.TrimSpace(query[idx+len("TABLE"):])
		name = firstToken(rest)
	}
	if name == "" {
		return nil, fmt.Errorf("sqlitefile: cannot parse table name: %s", query)
	}
	start := strings.Index(query, "(")
	end := strings.LastIndex(query, ")")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("sqlitefile: cannot parse columns: %s", query)
	}
	parts := splitTopLevel(query[start+1 : end])
	columns := []string{}
	for _, part := range parts {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		col := strings.Trim(fields[0], "`\"[]")
		if strings.EqualFold(col, "PRIMARY") || strings.HasPrefix(strings.ToUpper(strings.TrimSpace(part)), "PRIMARY KEY") {
			continue
		}
		columns = append(columns, col)
	}
	if _, ok := data.Tables[name]; !ok {
		data.Tables[name] = &Table{Name: name, Columns: columns, Rows: []map[string]string{}}
	}
	return driver.RowsAffected(0), nil
}

func firstToken(s string) string {
	s = strings.TrimSpace(s)
	end := strings.IndexAny(s, " (")
	if end < 0 {
		return strings.Trim(s, "`\"")
	}
	return strings.Trim(s[:end], "`\"")
}

func splitTopLevel(s string) []string {
	parts := []string{}
	depth := 0
	var current strings.Builder
	for _, r := range s {
		switch r {
		case '(':
			depth++
			current.WriteRune(r)
		case ')':
			depth--
			current.WriteRune(r)
		case ',':
			if depth == 0 {
				parts = append(parts, current.String())
				current.Reset()
			} else {
				current.WriteRune(r)
			}
		default:
			current.WriteRune(r)
		}
	}
	if strings.TrimSpace(current.String()) != "" {
		parts = append(parts, current.String())
	}
	return parts
}

func insert(data *fileData, query string, args []driver.Value) (driver.Result, error) {
	upper := strings.ToUpper(query)
	replace := strings.HasPrefix(upper, "INSERT OR REPLACE INTO")
	intoIdx := strings.Index(upper, "INTO")
	if intoIdx < 0 {
		return nil, fmt.Errorf("sqlitefile: bad insert: %s", query)
	}
	rest := strings.TrimSpace(query[intoIdx+len("INTO"):])
	name := firstToken(rest)
	table, ok := data.Tables[name]
	if !ok {
		return nil, fmt.Errorf("sqlitefile: unknown table %s", name)
	}
	colStart := strings.Index(query, "(")
	colEnd := strings.Index(query, ")")
	if colStart < 0 || colEnd <= colStart {
		return nil, fmt.Errorf("sqlitefile: missing insert columns: %s", query)
	}
	columns := splitTopLevel(query[colStart+1 : colEnd])
	for i := range columns {
		columns[i] = strings.TrimSpace(strings.Trim(columns[i], "`\""))
	}
	valuesIdx := strings.Index(upper, "VALUES")
	if valuesIdx < 0 {
		return nil, fmt.Errorf("sqlitefile: missing values: %s", query)
	}
	valuesPart := query[valuesIdx+len("VALUES"):]
	vStart := strings.Index(valuesPart, "(")
	vEnd := strings.Index(valuesPart, ")")
	if vStart < 0 || vEnd <= vStart {
		return nil, fmt.Errorf("sqlitefile: bad values: %s", query)
	}
	placeholders := splitTopLevel(valuesPart[vStart+1 : vEnd])
	if len(placeholders) != len(columns) {
		return nil, fmt.Errorf("sqlitefile: %d columns but %d values", len(columns), len(placeholders))
	}
	if len(args) != len(columns) {
		return nil, fmt.Errorf("sqlitefile: %d columns but %d args", len(columns), len(args))
	}
	row := map[string]string{}
	for i, col := range columns {
		row[col] = valueToString(args[i])
	}
	if replace && len(table.Columns) > 0 {
		key := table.Columns[0]
		keyValue := row[key]
		kept := table.Rows[:0]
		for _, existing := range table.Rows {
			if existing[key] != keyValue {
				kept = append(kept, existing)
			}
		}
		table.Rows = kept
	}
	table.Rows = append(table.Rows, row)
	return driver.RowsAffected(1), nil
}

func valueToString(v driver.Value) string {
	switch x := v.(type) {
	case nil:
		return ""
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case []byte:
		return string(x)
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

func queryLocked(data *fileData, query string, args []driver.Value) (driver.Rows, error) {
	q := normalize(query)
	upper := strings.ToUpper(q)
	if !strings.HasPrefix(upper, "SELECT") {
		return nil, fmt.Errorf("sqlitefile: unsupported query: %s", q)
	}
	fromIdx := strings.Index(upper, "FROM")
	if fromIdx < 0 {
		return nil, fmt.Errorf("sqlitefile: missing FROM: %s", q)
	}
	selectPart := strings.TrimSpace(q[len("SELECT"):fromIdx])
	rest := strings.TrimSpace(q[fromIdx+len("FROM"):])
	name := firstToken(rest)
	table, ok := data.Tables[name]
	if !ok {
		return nil, fmt.Errorf("sqlitefile: unknown table %s", name)
	}
	columns := table.Columns
	if selectPart != "*" {
		columns = splitTopLevel(selectPart)
		for i := range columns {
			columns[i] = strings.TrimSpace(strings.Trim(columns[i], "`\""))
		}
	}

	whereCol, whereArgIndex := parseWhere(q, upper)
	orderCol, descending := parseOrder(q, upper)
	filtered := []map[string]string{}
	for _, row := range table.Rows {
		if whereCol != "" {
			if whereArgIndex >= len(args) || row[whereCol] != valueToString(args[whereArgIndex]) {
				continue
			}
		}
		filtered = append(filtered, row)
	}
	if orderCol != "" {
		sortRows(filtered, orderCol, descending)
	}
	values := make([][]driver.Value, 0, len(filtered))
	for _, row := range filtered {
		line := make([]driver.Value, len(columns))
		for i, col := range columns {
			line[i] = row[col]
		}
		values = append(values, line)
	}
	return &Rows{columns: columns, values: values}, nil
}

func parseWhere(q, upper string) (column string, argIndex int) {
	idx := strings.Index(upper, "WHERE")
	if idx < 0 {
		return "", -1
	}
	part := q[idx+len("WHERE"):]
	if orderIdx := strings.Index(strings.ToUpper(part), "ORDER BY"); orderIdx >= 0 {
		part = part[:orderIdx]
	}
	part = strings.TrimSpace(part)
	eq := strings.Index(part, "=")
	if eq < 0 {
		return "", -1
	}
	column = strings.TrimSpace(part[:eq])
	right := strings.TrimSpace(part[eq+1:])
	if right == "?" {
		return column, 0
	}
	return column, -1
}

func parseOrder(q, upper string) (column string, descending bool) {
	idx := strings.Index(upper, "ORDER BY")
	if idx < 0 {
		return "", false
	}
	part := strings.TrimSpace(q[idx+len("ORDER BY"):])
	fields := strings.Fields(part)
	if len(fields) == 0 {
		return "", false
	}
	column = fields[0]
	return column, len(fields) > 1 && strings.EqualFold(fields[1], "DESC")
}

func sortRows(rows []map[string]string, col string, descending bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i][col], rows[j][col]
		na, ea := strconv.ParseInt(a, 10, 64)
		nb, eb := strconv.ParseInt(b, 10, 64)
		less := a < b
		if ea == nil && eb == nil {
			less = na < nb
		}
		if descending {
			return !less && a != b
		}
		return less
	})
}

type Rows struct {
	columns []string
	values  [][]driver.Value
	pos     int
}

func (r *Rows) Columns() []string { return r.columns }
func (r *Rows) Close() error      { return nil }

func (r *Rows) Next(dest []driver.Value) error {
	if r.pos >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.pos])
	r.pos++
	return nil
}
