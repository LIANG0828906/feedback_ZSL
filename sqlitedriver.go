package cachebench

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"strings"
)

type stdlibSQLiteDriver struct{}

type sqliteConn struct {
	path string
	cp   CheckpointData
	tx   bool
}

type sqliteTx struct {
	conn     *sqliteConn
	snapshot CheckpointData
}

type sqliteStmt struct {
	conn  *sqliteConn
	query string
}

type sqliteRows struct {
	columns []string
	rows    [][]driver.Value
	pos     int
}

type sqliteResult struct{}

func init() {
	names := sql.Drivers()
	hasSQLite3 := false
	for _, n := range names {
		if n == "sqlite3" {
			hasSQLite3 = true
		}
	}
	if !hasSQLite3 {
		sql.Register("sqlite3", stdlibSQLiteDriver{})
	}
	sql.Register("cachebench-sqlite3", stdlibSQLiteDriver{})
}

func (stdlibSQLiteDriver) Open(dsn string) (driver.Conn, error) {
	path := dsn
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	c := &sqliteConn{path: path, cp: CheckpointData{Version: CheckpointSchemaVersion, Meta: map[string]string{}}}
	if path != "" && path != ":memory:" {
		if _, err := os.Stat(path); err == nil {
			tables, err := ReadSQLiteTables(path)
			if err != nil {
				return nil, err
			}
			c.cp = checkpointDataFromTables(tables)
			if c.cp.Meta == nil {
				c.cp.Meta = map[string]string{}
			}
		}
	}
	return c, nil
}

func (c *sqliteConn) Prepare(query string) (driver.Stmt, error) {
	return &sqliteStmt{conn: c, query: query}, nil
}

func (c *sqliteConn) Close() error { return nil }

func (c *sqliteConn) Begin() (driver.Tx, error) {
	if c.tx {
		return nil, errors.New("sqlite driver: transaction already active")
	}
	c.tx = true
	return &sqliteTx{conn: c, snapshot: cloneCheckpointData(c.cp)}, nil
}

func (t *sqliteTx) Commit() error {
	t.conn.tx = false
	return t.conn.persist()
}

func (t *sqliteTx) Rollback() error {
	t.conn.tx = false
	t.conn.cp = t.snapshot
	return nil
}

func (s *sqliteStmt) Close() error  { return nil }
func (s *sqliteStmt) NumInput() int { return -1 }

func (s *sqliteStmt) Exec(args []driver.Value) (driver.Result, error) {
	if err := s.conn.exec(s.query, args); err != nil {
		return nil, err
	}
	if !s.conn.tx {
		if err := s.conn.persist(); err != nil {
			return nil, err
		}
	}
	return sqliteResult{}, nil
}

func (s *sqliteStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.conn.query(s.query, args)
}

func (r sqliteResult) LastInsertId() (int64, error) { return 0, nil }
func (r sqliteResult) RowsAffected() (int64, error) { return 1, nil }

func (r *sqliteRows) Columns() []string { return r.columns }
func (r *sqliteRows) Close() error      { return nil }

func (r *sqliteRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	row := r.rows[r.pos]
	for i := range dest {
		if i < len(row) {
			dest[i] = row[i]
		} else {
			dest[i] = nil
		}
	}
	r.pos++
	return nil
}

func cloneCheckpointData(d CheckpointData) CheckpointData {
	out := d
	out.Meta = map[string]string{}
	for k, v := range d.Meta {
		out.Meta[k] = v
	}
	out.Nodes = append([]NodeTick(nil), d.Nodes...)
	out.Windows = append([]WindowStat(nil), d.Windows...)
	return out
}

func (c *sqliteConn) persist() error {
	if c.path == "" || c.path == ":memory:" {
		return nil
	}
	return WriteSQLiteFile(c.path, checkpointTables(&c.cp))
}

func argInt64(args []driver.Value, i int) int64 {
	if i >= len(args) {
		return 0
	}
	switch v := args[i].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case bool:
		if v {
			return 1
		}
	}
	return 0
}

func argString(args []driver.Value, i int) string {
	if i >= len(args) {
		return ""
	}
	switch v := args[i].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

func containsFold(q, token string) bool {
	return strings.Contains(strings.ToLower(q), strings.ToLower(token))
}

func (c *sqliteConn) exec(query string, args []driver.Value) error {
	q := strings.TrimSpace(query)
	switch {
	case containsFold(q, "CREATE TABLE"):
		return nil
	case containsFold(q, "DELETE FROM node_state"):
		c.cp.Nodes = nil
		return nil
	case containsFold(q, "DELETE FROM window_stat"):
		c.cp.Windows = nil
		return nil
	case containsFold(q, "INTO node_state"):
		if len(args) < 12 {
			return NewError(CodeInvalidArgument, "node_state insert requires 12 args")
		}
		c.cp.Nodes = append(c.cp.Nodes, NodeTick{
			TickMS: argInt64(args, 0), NodeID: argString(args, 1), Inflight: int(argInt64(args, 2)),
			CircuitOpen: argInt64(args, 3) != 0, HighSinceMS: argInt64(args, 4), OpenSinceMS: argInt64(args, 5), RecoveryAtMS: argInt64(args, 6),
			Accepted: int(argInt64(args, 7)), Completed: int(argInt64(args, 8)), ErrorsCompleted: int(argInt64(args, 9)),
			LatencyTotalMS: argInt64(args, 10), Overflow: int(argInt64(args, 11)),
		})
		return nil
	case containsFold(q, "INTO window_stat"):
		if len(args) < 12 {
			return NewError(CodeInvalidArgument, "window_stat insert requires 12 args")
		}
		c.cp.Windows = append(c.cp.Windows, WindowStat{
			WindowIndex: int(argInt64(args, 0)), StartMS: argInt64(args, 1), EndMS: argInt64(args, 2),
			Requests: argInt64(args, 3), Errors: argInt64(args, 4), TotalLatencyMS: argInt64(args, 5),
			ThroughputQ3: argInt64(args, 6), ErrorRatePPM: argInt64(args, 7), AvgLatencyQ3: argInt64(args, 8),
			WithinTolerance: argInt64(args, 9) != 0, ConvertedValue: argInt64(args, 10), Saturated: argInt64(args, 11) != 0,
		})
		return nil
	case containsFold(q, "INTO checkpoint_meta"):
		if len(args) < 2 {
			return NewError(CodeInvalidArgument, "meta insert requires key/value")
		}
		key, value := argString(args, 0), argString(args, 1)
		if c.cp.Meta == nil {
			c.cp.Meta = map[string]string{}
		}
		c.cp.Meta[key] = value
		switch key {
		case "checkpoint_version":
			c.cp.Version = value
		case "schema_version":
			c.cp.ProjectSchemaVersion = value
		case "committed_tick":
			c.cp.CommittedTick = atoi64(value)
		}
		return nil
	default:
		return NewError(CodeInvalidArgument, "stdlib sqlite driver received unsupported SQL: %.80s", q)
	}
}

func (c *sqliteConn) query(query string, _ []driver.Value) (driver.Rows, error) {
	q := strings.ToLower(query)
	switch {
	case strings.Contains(q, "from checkpoint_meta"):
		rows := make([][]driver.Value, 0, len(c.cp.Meta))
		for k, v := range c.cp.Meta {
			rows = append(rows, []driver.Value{k, v})
		}
		return &sqliteRows{columns: []string{"key", "value"}, rows: rows}, nil
	case strings.Contains(q, "from node_state"):
		rows := make([][]driver.Value, 0, len(c.cp.Nodes))
		for _, n := range c.cp.Nodes {
			rows = append(rows, []driver.Value{n.TickMS, n.NodeID, int64(n.Inflight), int64(boolToInt(n.CircuitOpen)), n.HighSinceMS, n.OpenSinceMS, n.RecoveryAtMS, int64(n.Accepted), int64(n.Completed), int64(n.ErrorsCompleted), n.LatencyTotalMS, int64(n.Overflow)})
		}
		return &sqliteRows{columns: []string{"tick", "node_id", "inflight", "circuit_open", "high_since", "open_since", "recovery_at", "accepted", "completed", "errors_completed", "latency_total_ms", "overflow"}, rows: rows}, nil
	case strings.Contains(q, "from window_stat"):
		rows := make([][]driver.Value, 0, len(c.cp.Windows))
		for _, w := range c.cp.Windows {
			rows = append(rows, []driver.Value{int64(w.WindowIndex), w.StartMS, w.EndMS, w.Requests, w.Errors, w.TotalLatencyMS, w.ThroughputQ3, w.ErrorRatePPM, w.AvgLatencyQ3, int64(boolToInt(w.WithinTolerance)), w.ConvertedValue, int64(boolToInt(w.Saturated))})
		}
		return &sqliteRows{columns: []string{"window_index", "start_ms", "end_ms", "requests", "errors", "total_latency_ms", "throughput_q3", "error_rate_ppm", "avg_latency_q3", "within_tolerance", "converted_value", "saturated"}, rows: rows}, nil
	default:
		return nil, NewError(CodeInvalidArgument, "stdlib sqlite driver cannot query unsupported SQL: %.80s", query)
	}
}
