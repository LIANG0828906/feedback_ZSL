package cachebench

import (
	"database/sql"
	"errors"
	"os"
)

var ErrNoCheckpoint = errors.New("no committed checkpoint")

// CheckpointData is the atomically committed durable view. Nodes contains the
// state at CommittedTick; windows are the latest deterministic stats windows.
type CheckpointData struct {
	Version              string
	ProjectSchemaVersion string
	CommittedTick        int64
	Nodes                []NodeTick
	Windows              []WindowStat
	Meta                 map[string]string
}

type SQLiteCheckpointStore struct {
	db   *sql.DB
	path string
}

func OpenSQLiteCheckpoint(path string) (*SQLiteCheckpointStore, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, NewError(CodeInvalidArgument, "open sqlite checkpoint: %v", err).WithField("path")
	}
	db.SetMaxOpenConns(1)
	s := &SQLiteCheckpointStore{db: db, path: path}
	if err := s.InitSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteCheckpointStore) Close() error { return s.db.Close() }
func (s *SQLiteCheckpointStore) Path() string { return s.path }

func (s *SQLiteCheckpointStore) InitSchema() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS node_state (
			tick INTEGER NOT NULL, node_id TEXT NOT NULL, inflight INTEGER NOT NULL,
			circuit_open INTEGER NOT NULL, high_since INTEGER NOT NULL, open_since INTEGER NOT NULL, recovery_at INTEGER NOT NULL,
			accepted INTEGER NOT NULL, completed INTEGER NOT NULL, errors_completed INTEGER NOT NULL,
			latency_total_ms INTEGER NOT NULL, overflow INTEGER NOT NULL,
			PRIMARY KEY(tick, node_id))`,
		`CREATE TABLE IF NOT EXISTS window_stat (
			window_index INTEGER PRIMARY KEY, start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL,
			requests INTEGER NOT NULL, errors INTEGER NOT NULL, total_latency_ms INTEGER NOT NULL,
			throughput_q3 INTEGER NOT NULL, error_rate_ppm INTEGER NOT NULL, avg_latency_q3 INTEGER NOT NULL,
			within_tolerance INTEGER NOT NULL, converted_value INTEGER NOT NULL, saturated INTEGER NOT NULL)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			if de, ok := AsDomainError(err); ok && de != nil {
				return de
			}
			return NewError(CodeSQLiteCorrupt, "initialize checkpoint schema: %v", err)
		}
	}
	return nil
}

// SaveCommitted executes the durable commit order: node states first, windows
// second, and the global committed_tick metadata last. The transaction commit
// atomically replaces the SQLite file in the stdlib driver.
func (s *SQLiteCheckpointStore) SaveCommitted(data CheckpointData) error {
	if data.Version == "" {
		data.Version = CheckpointSchemaVersion
	}
	tx, err := s.db.Begin()
	if err != nil {
		return NewError(CodeSQLiteCorrupt, "begin checkpoint tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM node_state`); err != nil {
		return NewError(CodeSQLiteCorrupt, "clear node_state: %v", err)
	}
	for _, n := range data.Nodes {
		if _, err := tx.Exec(`INSERT INTO node_state(tick,node_id,inflight,circuit_open,high_since,open_since,recovery_at,accepted,completed,errors_completed,latency_total_ms,overflow) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			n.TickMS, n.NodeID, n.Inflight, boolToInt(n.CircuitOpen), n.HighSinceMS, n.OpenSinceMS, n.RecoveryAtMS,
			n.Accepted, n.Completed, n.ErrorsCompleted, n.LatencyTotalMS, n.Overflow); err != nil {
			return NewError(CodeSQLiteCorrupt, "insert node_state: %v", err).WithNode(n.NodeID).WithTick(n.TickMS)
		}
	}
	if _, err := tx.Exec(`DELETE FROM window_stat`); err != nil {
		return NewError(CodeSQLiteCorrupt, "clear window_stat: %v", err)
	}
	for _, w := range data.Windows {
		if _, err := tx.Exec(`INSERT INTO window_stat(window_index,start_ms,end_ms,requests,errors,total_latency_ms,throughput_q3,error_rate_ppm,avg_latency_q3,within_tolerance,converted_value,saturated) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			w.WindowIndex, w.StartMS, w.EndMS, w.Requests, w.Errors, w.TotalLatencyMS, w.ThroughputQ3,
			w.ErrorRatePPM, w.AvgLatencyQ3, boolToInt(w.WithinTolerance), w.ConvertedValue, boolToInt(w.Saturated)); err != nil {
			return NewError(CodeSQLiteCorrupt, "insert window_stat: %v", err).WithContext("window", itoa(w.WindowIndex))
		}
	}
	meta := map[string]string{
		"checkpoint_version": data.Version,
		"schema_version":     data.ProjectSchemaVersion,
	}
	for k, v := range data.Meta {
		meta[k] = v
	}
	// Write all metadata before the global tick pointer.
	for k, v := range meta {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO checkpoint_meta(key,value) VALUES(?,?)`, k, v); err != nil {
			return NewError(CodeSQLiteCorrupt, "insert checkpoint_meta: %v", err).WithContext("key", k)
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO checkpoint_meta(key,value) VALUES(?,?)`, "committed_tick", itoa64(data.CommittedTick)); err != nil {
		return NewError(CodeSQLiteCorrupt, "commit global tick: %v", err).WithTick(data.CommittedTick)
	}
	if err := tx.Commit(); err != nil {
		return NewError(CodeSQLiteCorrupt, "commit checkpoint tx: %v", err)
	}
	return nil
}

func (s *SQLiteCheckpointStore) LoadCommitted() (*CheckpointData, error) {
	rows, err := s.db.Query(`SELECT key,value FROM checkpoint_meta`)
	if err != nil {
		return nil, normalizeReadError(err)
	}
	data := &CheckpointData{Meta: map[string]string{}}
	metaCount := 0
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			_ = rows.Close()
			return nil, NewError(CodeCheckpointTruncated, "scan checkpoint_meta: %v", err)
		}
		data.Meta[k] = v
		metaCount++
	}
	_ = rows.Close()
	if metaCount == 0 {
		if _, statErr := os.Stat(s.path); os.IsNotExist(statErr) {
			return nil, ErrNoCheckpoint
		}
		return nil, NewError(CodeCheckpointTruncated, "checkpoint metadata is missing")
	}
	data.Version = data.Meta["checkpoint_version"]
	data.ProjectSchemaVersion = data.Meta["schema_version"]
	data.CommittedTick = atoi64(data.Meta["committed_tick"])
	if data.Version != CheckpointSchemaVersion {
		return nil, NewError(CodeCheckpointVersion, "checkpoint version %q does not match supported %q", data.Version, CheckpointSchemaVersion).
			WithField("checkpoint_version").
			WithContext("found", data.Version).
			WithContext("expected", CheckpointSchemaVersion)
	}

	nodeRows, err := s.db.Query(`SELECT tick,node_id,inflight,circuit_open,high_since,open_since,recovery_at,accepted,completed,errors_completed,latency_total_ms,overflow FROM node_state`)
	if err != nil {
		return nil, normalizeReadError(err)
	}
	defer nodeRows.Close()
	for nodeRows.Next() {
		var n NodeTick
		var open int
		if err := nodeRows.Scan(&n.TickMS, &n.NodeID, &n.Inflight, &open, &n.HighSinceMS, &n.OpenSinceMS, &n.RecoveryAtMS, &n.Accepted, &n.Completed, &n.ErrorsCompleted, &n.LatencyTotalMS, &n.Overflow); err != nil {
			return nil, NewError(CodeCheckpointTruncated, "scan node_state: %v", err)
		}
		n.CircuitOpen = open != 0
		data.Nodes = append(data.Nodes, n)
	}
	windowRows, err := s.db.Query(`SELECT window_index,start_ms,end_ms,requests,errors,total_latency_ms,throughput_q3,error_rate_ppm,avg_latency_q3,within_tolerance,converted_value,saturated FROM window_stat`)
	if err != nil {
		return nil, normalizeReadError(err)
	}
	defer windowRows.Close()
	for windowRows.Next() {
		var w WindowStat
		var tolerant, saturated int
		if err := windowRows.Scan(&w.WindowIndex, &w.StartMS, &w.EndMS, &w.Requests, &w.Errors, &w.TotalLatencyMS, &w.ThroughputQ3, &w.ErrorRatePPM, &w.AvgLatencyQ3, &tolerant, &w.ConvertedValue, &saturated); err != nil {
			return nil, NewError(CodeCheckpointTruncated, "scan window_stat: %v", err)
		}
		w.WithinTolerance = tolerant != 0
		w.Saturated = saturated != 0
		data.Windows = append(data.Windows, w)
	}
	return data, nil
}

func normalizeReadError(err error) error {
	if de, ok := AsDomainError(err); ok {
		return de
	}
	return NewError(CodeCheckpointTruncated, "read sqlite checkpoint failed: %v", err)
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func itoa64(v int64) string {
	return itoa(int(v))
}

func atoi64(s string) int64 {
	var v int64
	neg := false
	i := 0
	if len(s) > 0 && s[0] == '-' {
		neg = true
		i = 1
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			break
		}
		v = v*10 + int64(s[i]-'0')
	}
	if neg {
		v = -v
	}
	return v
}

type RecoveryResult struct {
	Checkpoint *CheckpointData
	Result     *SimulationResult
	Rebuilt    bool
	Reason     ErrorCode
}

// RecoverOrRebuild accepts only a matching checkpoint version. Version mismatch
// and truncation deliberately trigger a deterministic start-to-finish rebuild.
func RecoverOrRebuild(p Project, path string) (*RecoveryResult, error) {
	store, err := OpenSQLiteCheckpoint(path)
	if err != nil {
		return nil, err
	}
	data, loadErr := store.LoadCommitted()
	_ = store.Close()
	if loadErr != nil {
		code := CodeOf(loadErr)
		if code != CodeCheckpointVersion && code != CodeCheckpointTruncated && code != CodeSQLiteCorrupt {
			return nil, loadErr
		}
		full, simErr := Simulate(p)
		if simErr != nil {
			return nil, simErr
		}
		return &RecoveryResult{Result: full, Rebuilt: true, Reason: code}, nil
	}
	full, err := Simulate(p)
	if err != nil {
		return nil, err
	}
	return &RecoveryResult{Checkpoint: data, Result: full, Rebuilt: false}, nil
}

func TruncateCheckpointForInjection(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	size := info.Size()
	cut := int64(128)
	if size <= cut {
		cut = size / 2
	}
	if cut < 16 {
		cut = 16
	}
	return os.Truncate(path, cut)
}

func checkpointTables(d *CheckpointData) []SQLiteTable {
	metaRows := [][]any{}
	keys := make([]string, 0, len(d.Meta)+3)
	seen := map[string]bool{}
	for k := range d.Meta {
		keys = append(keys, k)
		seen[k] = true
	}
	for _, k := range []string{"checkpoint_version", "schema_version", "committed_tick"} {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		v := d.Meta[k]
		switch k {
		case "checkpoint_version":
			v = d.Version
		case "schema_version":
			v = d.ProjectSchemaVersion
		case "committed_tick":
			v = itoa64(d.CommittedTick)
		}
		metaRows = append(metaRows, []any{k, v})
	}
	nodeRows := make([][]any, 0, len(d.Nodes))
	for _, n := range d.Nodes {
		nodeRows = append(nodeRows, []any{n.TickMS, n.NodeID, int64(n.Inflight), int64(boolToInt(n.CircuitOpen)), n.HighSinceMS, n.OpenSinceMS, n.RecoveryAtMS, int64(n.Accepted), int64(n.Completed), int64(n.ErrorsCompleted), n.LatencyTotalMS, int64(n.Overflow)})
	}
	windowRows := make([][]any, 0, len(d.Windows))
	for _, w := range d.Windows {
		windowRows = append(windowRows, []any{int64(w.WindowIndex), w.StartMS, w.EndMS, w.Requests, w.Errors, w.TotalLatencyMS, w.ThroughputQ3, w.ErrorRatePPM, w.AvgLatencyQ3, int64(boolToInt(w.WithinTolerance)), w.ConvertedValue, int64(boolToInt(w.Saturated))})
	}
	return []SQLiteTable{
		{Name: "checkpoint_meta", Columns: []SQLiteColumn{{Name: "key", Type: "TEXT"}, {Name: "value", Type: "TEXT"}}, Rows: metaRows},
		{Name: "node_state", Columns: []SQLiteColumn{{Name: "tick", Type: "INTEGER"}, {Name: "node_id", Type: "TEXT"}, {Name: "inflight", Type: "INTEGER"}, {Name: "circuit_open", Type: "INTEGER"}, {Name: "high_since", Type: "INTEGER"}, {Name: "open_since", Type: "INTEGER"}, {Name: "recovery_at", Type: "INTEGER"}, {Name: "accepted", Type: "INTEGER"}, {Name: "completed", Type: "INTEGER"}, {Name: "errors_completed", Type: "INTEGER"}, {Name: "latency_total_ms", Type: "INTEGER"}, {Name: "overflow", Type: "INTEGER"}}, Rows: nodeRows},
		{Name: "window_stat", Columns: []SQLiteColumn{{Name: "window_index", Type: "INTEGER"}, {Name: "start_ms", Type: "INTEGER"}, {Name: "end_ms", Type: "INTEGER"}, {Name: "requests", Type: "INTEGER"}, {Name: "errors", Type: "INTEGER"}, {Name: "total_latency_ms", Type: "INTEGER"}, {Name: "throughput_q3", Type: "INTEGER"}, {Name: "error_rate_ppm", Type: "INTEGER"}, {Name: "avg_latency_q3", Type: "INTEGER"}, {Name: "within_tolerance", Type: "INTEGER"}, {Name: "converted_value", Type: "INTEGER"}, {Name: "saturated", Type: "INTEGER"}}, Rows: windowRows},
	}
}

func checkpointDataFromTables(tables map[string][][]any) CheckpointData {
	d := CheckpointData{Version: CheckpointSchemaVersion, Meta: map[string]string{}}
	for _, r := range tables["checkpoint_meta"] {
		if len(r) >= 2 {
			k, _ := r[0].(string)
			v, _ := r[1].(string)
			d.Meta[k] = v
		}
	}
	d.Version = d.Meta["checkpoint_version"]
	d.ProjectSchemaVersion = d.Meta["schema_version"]
	d.CommittedTick = atoi64(d.Meta["committed_tick"])
	for _, r := range tables["node_state"] {
		if len(r) < 12 {
			continue
		}
		d.Nodes = append(d.Nodes, NodeTick{
			TickMS: asInt64(r[0]), NodeID: asString(r[1]), Inflight: int(asInt64(r[2])), CircuitOpen: asInt64(r[3]) != 0, HighSinceMS: asInt64(r[4]),
			OpenSinceMS: asInt64(r[5]), RecoveryAtMS: asInt64(r[6]), Accepted: int(asInt64(r[7])), Completed: int(asInt64(r[8])),
			ErrorsCompleted: int(asInt64(r[9])), LatencyTotalMS: asInt64(r[10]), Overflow: int(asInt64(r[11])),
		})
	}
	for _, r := range tables["window_stat"] {
		if len(r) < 12 {
			continue
		}
		d.Windows = append(d.Windows, WindowStat{
			WindowIndex: int(asInt64(r[0])), StartMS: asInt64(r[1]), EndMS: asInt64(r[2]), Requests: asInt64(r[3]), Errors: asInt64(r[4]),
			TotalLatencyMS: asInt64(r[5]), ThroughputQ3: asInt64(r[6]), ErrorRatePPM: asInt64(r[7]), AvgLatencyQ3: asInt64(r[8]),
			WithinTolerance: asInt64(r[9]) != 0, ConvertedValue: asInt64(r[10]), Saturated: asInt64(r[11]) != 0,
		})
	}
	return d
}

func asInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	default:
		return 0
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
