// Package sqlitedrv is the repository-built database/sql driver used for
// deterministic local tile snapshots. It implements the small SQLite-style
// SQL subset used by this project and persists atomically to one local file.
package sqlitedrv

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

func init() {
	d := &Driver{}
	sql.Register("sqlite", d)
	sql.Register("grid-sqlite", d)
}

type Driver struct{}

func (Driver) Open(dsn string) (driver.Conn, error) {
	path := strings.TrimPrefix(dsn, "file:")
	db, err := loadFileDB(path)
	if err != nil {
		return nil, err
	}
	return &conn{path: path, db: db}, nil
}

type storedValue struct {
	Type string `json:"t"`
	I    int64  `json:"i,omitempty"`
	S    string `json:"s,omitempty"`
	B    []byte `json:"b,omitempty"`
}

func newValue(v driver.Value) storedValue {
	switch x := v.(type) {
	case int64:
		return storedValue{Type: "i", I: x}
	case int:
		return storedValue{Type: "i", I: int64(x)}
	case []byte:
		return storedValue{Type: "b", B: append([]byte(nil), x...)}
	case string:
		return storedValue{Type: "s", S: x}
	case bool:
		if x {
			return storedValue{Type: "i", I: 1}
		}
		return storedValue{Type: "i", I: 0}
	case nil:
		return storedValue{Type: "s"}
	default:
		return storedValue{Type: "s", S: fmt.Sprint(x)}
	}
}

func (v storedValue) driverValue() driver.Value {
	switch v.Type {
	case "i":
		return v.I
	case "b":
		return v.B
	default:
		return v.S
	}
}

type fileDB struct {
	Tables map[string]map[string]map[string]storedValue `json:"tables"`
}

var fileMu sync.Mutex

func loadFileDB(path string) (*fileDB, error) {
	fileMu.Lock()
	defer fileMu.Unlock()
	db := &fileDB{Tables: map[string]map[string]map[string]storedValue{}}
	if path == "" || path == ":memory:" {
		return db, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return db, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return db, nil
	}
	if err := json.Unmarshal(b, db); err != nil {
		return nil, fmt.Errorf("builtin sqlite driver: corrupt database file %s: %w", path, err)
	}
	if db.Tables == nil {
		db.Tables = map[string]map[string]map[string]storedValue{}
	}
	return db, nil
}

func (db *fileDB) save(path string) error {
	if path == "" || path == ":memory:" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

type conn struct {
	path string
	db   *fileDB
	tx   *localTx
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return &stmt{c: c, query: query}, nil
}
func (c *conn) Close() error { return nil }
func (c *conn) Begin() (driver.Tx, error) {
	if c.tx != nil {
		return nil, fmt.Errorf("builtin sqlite driver: transaction already active")
	}
	c.tx = &localTx{c: c}
	return c.tx, nil
}

type localTx struct {
	c    *conn
	ops  []func()
	done bool
}

func (t *localTx) Commit() error {
	if t.done {
		return nil
	}
	for _, op := range t.ops {
		op()
	}
	t.done = true
	t.c.tx = nil
	return t.c.db.save(t.c.path)
}

func (t *localTx) Rollback() error {
	t.done = true
	t.c.tx = nil
	return nil
}

type stmt struct {
	c     *conn
	query string
}

func (s *stmt) Close() error  { return nil }
func (s *stmt) NumInput() int { return -1 }

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	mutate, table, err := buildMutation(s.query, args)
	if err != nil {
		return nil, err
	}
	if table != "" && s.c.db.Tables[table] == nil {
		s.c.db.Tables[table] = map[string]map[string]storedValue{}
	}
	if s.c.tx != nil {
		s.c.tx.ops = append(s.c.tx.ops, func() { mutate(s.c.db) })
	} else {
		mutate(s.c.db)
		if err := s.c.db.save(s.c.path); err != nil {
			return nil, err
		}
	}
	return driver.RowsAffected(1), nil
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return runQuery(s.c.db, s.query, args)
}

func buildMutation(query string, args []driver.Value) (func(*fileDB), string, error) {
	trimmed := strings.TrimSpace(query)
	upper := strings.ToUpper(trimmed)
	if strings.HasPrefix(upper, "CREATE TABLE") {
		table := parseCreateTable(trimmed)
		return func(*fileDB) {}, table, nil
	}
	if strings.HasPrefix(upper, "INSERT") {
		table, cols, err := parseInsert(trimmed)
		if err != nil {
			return nil, "", err
		}
		if len(cols) != len(args) {
			return nil, "", fmt.Errorf("builtin sqlite driver: %d columns but %d args", len(cols), len(args))
		}
		row := map[string]storedValue{}
		keyParts := []string{"0", "0"}
		for i, col := range cols {
			v := newValue(args[i])
			row[col] = v
			if col == "tile_col" {
				keyParts[0] = fmt.Sprint(v.I)
			}
			if col == "tile_row" {
				keyParts[1] = fmt.Sprint(v.I)
			}
		}
		key := keyParts[0] + "," + keyParts[1]
		return func(db *fileDB) {
			if db.Tables[table] == nil {
				db.Tables[table] = map[string]map[string]storedValue{}
			}
			db.Tables[table][key] = row
		}, table, nil
	}
	return nil, "", fmt.Errorf("builtin sqlite driver: unsupported Exec SQL: %.80s", query)
}

func parseCreateTable(query string) string {
	fields := strings.Fields(query)
	for i, f := range fields {
		if strings.EqualFold(f, "TABLE") && i+1 < len(fields) {
			name := fields[i+1]
			name = strings.TrimPrefix(name, "IF")
			name = strings.Trim(name, "();\"`[]")
			// Handle "IF NOT EXISTS tiles": find last token before '('.
			rest := strings.SplitN(query, "(", 2)[0]
			tokens := strings.Fields(rest)
			return strings.Trim(tokens[len(tokens)-1], "\"`[]")
		}
	}
	return "tiles"
}

func parseInsert(query string) (string, []string, error) {
	open := strings.Index(query, "(")
	close := strings.Index(query, ")")
	intoIdx := indexCaseInsensitive(query, "INTO")
	if open < 0 || close <= open || intoIdx < 0 {
		return "", nil, fmt.Errorf("builtin sqlite driver: cannot parse insert: %.120s", query)
	}
	beforeOpen := strings.TrimSpace(query[intoIdx+4 : open])
	table := strings.Trim(strings.Fields(beforeOpen)[0], "\"`[]")
	colPart := query[open+1 : close]
	rawCols := strings.Split(colPart, ",")
	cols := make([]string, 0, len(rawCols))
	for _, c := range rawCols {
		cols = append(cols, strings.Trim(strings.TrimSpace(c), "\"`[]"))
	}
	return table, cols, nil
}

func indexCaseInsensitive(s, substr string) int {
	return strings.Index(strings.ToUpper(s), substr)
}

var snapshotColumns = []string{
	"tile_col", "tile_row", "seq", "content_hash", "color_digest", "depth_digest",
	"color_blob", "depth_blob", "structure_version", "camera_version", "algorithm_version",
	"versions_json", "coverage_permille", "poly_pixels", "hit_stats_json",
}

type rows struct {
	cols [][]storedValue
	pos  int
}

func (r *rows) Columns() []string { return snapshotColumns }
func (r *rows) Close() error      { return nil }
func (r *rows) Next(dest []driver.Value) error {
	if r.pos >= len(r.cols) {
		return io.EOF
	}
	row := r.cols[r.pos]
	for i := range dest {
		if i < len(row) {
			dest[i] = row[i].driverValue()
		} else {
			dest[i] = nil
		}
	}
	r.pos++
	return nil
}

func runQuery(db *fileDB, query string, args []driver.Value) (driver.Rows, error) {
	upper := strings.ToUpper(strings.TrimSpace(query))
	if !strings.HasPrefix(upper, "SELECT") {
		return nil, fmt.Errorf("builtin sqlite driver: unsupported Query SQL: %.80s", query)
	}
	table := "tiles"
	if idx := indexCaseInsensitive(query, "FROM"); idx >= 0 {
		rest := strings.TrimSpace(query[idx+4:])
		fields := strings.Fields(rest)
		if len(fields) > 0 {
			table = strings.Trim(fields[0], "\"`[];")
		}
	}
	tableRows := db.Tables[table]
	type indexed struct {
		key string
		row map[string]storedValue
		seq int64
		col int64
		rwn int64
	}
	all := make([]indexed, 0, len(tableRows))
	for key, row := range tableRows {
		item := indexed{key: key, row: row}
		if v, ok := row["seq"]; ok {
			item.seq = v.I
		}
		if v, ok := row["tile_col"]; ok {
			item.col = v.I
		}
		if v, ok := row["tile_row"]; ok {
			item.rwn = v.I
		}
		if strings.Contains(upper, "WHERE TILE_COL") && len(args) >= 2 {
			if item.col != valueInt(args[0]) || item.rwn != valueInt(args[1]) {
				continue
			}
		}
		all = append(all, item)
	}
	if strings.Contains(upper, "ORDER BY SEQ") {
		sort.Slice(all, func(i, j int) bool {
			a, b := all[i], all[j]
			if a.seq != b.seq {
				return a.seq < b.seq
			}
			if a.rwn != b.rwn {
				return a.rwn < b.rwn
			}
			return a.col < b.col
		})
	}
	data := make([][]storedValue, 0, len(all))
	for _, item := range all {
		values := make([]storedValue, len(snapshotColumns))
		for i, col := range snapshotColumns {
			values[i] = item.row[col]
		}
		data = append(data, values)
	}
	return &rows{cols: data}, nil
}

func valueInt(v driver.Value) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case []byte:
		var n int64
		fmt.Sscanf(string(x), "%d", &n)
		return n
	case string:
		var n int64
		fmt.Sscanf(x, "%d", &n)
		return n
	}
	return 0
}
