package lightshow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

type WindowCache struct {
	Result         WindowResult
	ProjectHash    string
	Algorithm      string
	SourceVersion  string
	FormatVersion  string
	DependencyHash string
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, engineError(CodeDecode, "checkpoint.path", "checkpoint path is empty")
	}
	dsn := path
	if path == ":memory:" {
		dsn = "file:lightshow-memory?mode=memory&cache=shared"
	} else {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, engineError(CodeDecode, "checkpoint.path", "resolve path: %v", err)
		}
		dsn = abs
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, engineError(CodeDecode, "checkpoint.open", "open SQLite: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) init() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	statements := []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS windows (
			window_id TEXT PRIMARY KEY,
			window_index INTEGER NOT NULL UNIQUE,
			start_us INTEGER NOT NULL,
			end_us INTEGER NOT NULL,
			frame_start INTEGER NOT NULL,
			frame_end INTEGER NOT NULL,
			state TEXT NOT NULL,
			project_hash TEXT NOT NULL DEFAULT '',
			algorithm_version TEXT NOT NULL DEFAULT '',
			source_version TEXT NOT NULL DEFAULT '',
			format_version TEXT NOT NULL DEFAULT '',
			dependency_hash TEXT NOT NULL DEFAULT '',
			content_hash TEXT NOT NULL DEFAULT '',
			change_offset_start INTEGER NOT NULL DEFAULT 0,
			change_offset_end INTEGER NOT NULL DEFAULT 0,
			committed_at_ms INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS outputs (
			window_id TEXT NOT NULL,
			frame INTEGER NOT NULL,
			fixture_id TEXT NOT NULL,
			at_us INTEGER NOT NULL DEFAULT 0,
			light_index INTEGER NOT NULL DEFAULT 0,
			color_r INTEGER NOT NULL,
			color_g INTEGER NOT NULL,
			color_b INTEGER NOT NULL,
			intensity INTEGER NOT NULL,
			pos_x INTEGER NOT NULL,
			pos_y INTEGER NOT NULL,
			pos_z INTEGER NOT NULL,
			focus INTEGER NOT NULL,
			PRIMARY KEY (window_id, frame, fixture_id),
			FOREIGN KEY (window_id) REFERENCES windows(window_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS decisions (
			window_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			decision_json TEXT NOT NULL,
			PRIMARY KEY (window_id, seq),
			FOREIGN KEY (window_id) REFERENCES windows(window_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS change_consumptions (
			window_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			change_id TEXT NOT NULL,
			at_us INTEGER NOT NULL,
			frame INTEGER NOT NULL,
			PRIMARY KEY (window_id, ordinal),
			FOREIGN KEY (window_id) REFERENCES windows(window_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS group_snapshots (
			window_id TEXT PRIMARY KEY,
			snapshot_json TEXT NOT NULL,
			FOREIGN KEY (window_id) REFERENCES windows(window_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS dependencies (
			window_id TEXT NOT NULL,
			dep_kind TEXT NOT NULL,
			dep_key TEXT NOT NULL,
			PRIMARY KEY (window_id, dep_kind, dep_key),
			FOREIGN KEY (window_id) REFERENCES windows(window_id) ON DELETE CASCADE
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return engineError(CodeDecode, "checkpoint.schema", "initialize SQLite: %v", err)
		}
	}
	if err := s.ensureColumn("outputs", "at_us", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("outputs", "light_index", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureColumn(table, column, definition string) error {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return engineError(CodeDecode, "checkpoint.schema", "inspect %s: %v", table, err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return engineError(CodeDecode, "checkpoint.schema", "inspect %s: %v", table, err)
		}
		if name == column {
			found = true
		}
	}
	rows.Close()
	if found {
		return nil
	}
	if _, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)); err != nil {
		return engineError(CodeDecode, "checkpoint.schema", "add %s.%s: %v", table, column, err)
	}
	return nil
}

func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return engineError(CodeDecode, "checkpoint.reset", "begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM windows`); err != nil {
		return engineError(CodeDecode, "checkpoint.reset", "clear windows: %v", err)
	}
	if _, err := tx.Exec(`DELETE FROM meta`); err != nil {
		return engineError(CodeDecode, "checkpoint.reset", "clear meta: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return engineError(CodeDecode, "checkpoint.reset", "commit: %v", err)
	}
	return nil
}

func (s *Store) EnsureWindow(w WindowResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO windows
		(window_id, window_index, start_us, end_us, frame_start, frame_end, state)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(window_index) DO NOTHING`,
		w.WindowID, w.Index, w.StartUS, w.EndUS, w.FrameStart, w.FrameEnd, string(StatePending))
	if err != nil {
		return engineError(CodeDecode, "checkpoint.windows", "ensure window %s: %v", w.WindowID, err)
	}
	return nil
}

func (s *Store) State(windowID string) (WindowState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var state string
	err := s.db.QueryRow(`SELECT state FROM windows WHERE window_id = ?`, windowID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", engineError(CodeState, windowID, "window does not exist")
	}
	if err != nil {
		return "", engineError(CodeDecode, "checkpoint.state", "%v", err)
	}
	return WindowState(state), nil
}

func (s *Store) Transition(windowID string, to WindowState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transitionLocked(windowID, to)
}

func (s *Store) transitionLocked(windowID string, to WindowState) error {
	tx, err := s.db.Begin()
	if err != nil {
		return engineError(CodeDecode, "checkpoint.transition", "begin: %v", err)
	}
	defer tx.Rollback()
	var current string
	if err := tx.QueryRow(`SELECT state FROM windows WHERE window_id = ?`, windowID).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return engineError(CodeState, windowID, "window does not exist")
		}
		return engineError(CodeDecode, "checkpoint.transition", "%v", err)
	}
	if !legalTransition(WindowState(current), to) {
		return engineError(CodeState, windowID, "illegal window state transition %s -> %s", current, to)
	}
	result, err := tx.Exec(`UPDATE windows SET state = ? WHERE window_id = ? AND state = ?`, string(to), windowID, current)
	if err != nil {
		return engineError(CodeDecode, "checkpoint.transition", "%v", err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return engineError(CodeState, windowID, "window state changed concurrently")
	}
	if err := tx.Commit(); err != nil {
		return engineError(CodeDecode, "checkpoint.transition", "commit: %v", err)
	}
	return nil
}

func (s *Store) RecoverInFlight() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return engineError(CodeDecode, "checkpoint.recover", "begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE windows SET state = ? WHERE state = ?`, string(StateFailed), string(StateScheduling)); err != nil {
		return engineError(CodeDecode, "checkpoint.recover", "%v", err)
	}
	if err := tx.Commit(); err != nil {
		return engineError(CodeDecode, "checkpoint.recover", "commit: %v", err)
	}
	return nil
}

// PrepareForProject removes committed cache entries that do not match the
// requested project/algorithm/source/format tuple. Pending and failed windows
// remain retryable, but they are never counted as reusable output.
func (s *Store) PrepareForProject(projectHash, algorithm, sourceVersion, formatVersion string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM windows WHERE state = ? AND (
		project_hash <> ? OR algorithm_version <> ? OR source_version <> ? OR format_version <> ?)`,
		string(StateCommitted), projectHash, algorithm, sourceVersion, formatVersion)
	if err != nil {
		return engineError(CodeDecode, "checkpoint.prepare", "clear incompatible cache: %v", err)
	}
	return nil
}

func (s *Store) Fail(windowID string) error {
	state, err := s.State(windowID)
	if err != nil {
		return err
	}
	if state == StateFailed {
		return nil
	}
	return s.Transition(windowID, StateFailed)
}

func (s *Store) CommitWindow(ctx context.Context, w WindowResult, projectHash, algorithm, sourceVersion, formatVersion string) error {
	if err := ctx.Err(); err != nil {
		return engineError(CodeCanceled, "schedule", "cancelled before commit: %v", err)
	}
	if w.ContentHash == "" {
		w.ContentHash = w.ComputeContentHash()
	}
	depHash := hashJSON(w.Dependencies)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return engineError(CodeDecode, "checkpoint.commit", "begin: %v", err)
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRow(`SELECT state FROM windows WHERE window_id = ?`, w.WindowID).Scan(&state); err != nil {
		return engineError(CodeState, w.WindowID, "cannot commit unknown window")
	}
	if WindowState(state) != StateScheduling {
		return engineError(CodeState, w.WindowID, "the window must be scheduling before commit (current %s)", state)
	}
	for _, frame := range w.Outputs {
		for lightIndex, light := range frame.Lights {
			if _, err := tx.Exec(`INSERT INTO outputs
				(window_id, frame, fixture_id, at_us, light_index, color_r, color_g, color_b, intensity, pos_x, pos_y, pos_z, focus)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				w.WindowID, frame.Frame, light.FixtureID, frame.AtUS, lightIndex, light.Color.R, light.Color.G, light.Color.B,
				light.Intensity, light.Position[0], light.Position[1], light.Position[2], light.Focus); err != nil {
				return engineError(CodeDecode, "checkpoint.outputs", "insert output: %v", err)
			}
		}
	}
	for i, decision := range w.Decisions {
		data, err := json.Marshal(decision)
		if err != nil {
			return engineError(CodeDecode, "checkpoint.decisions", "encode decision: %v", err)
		}
		if _, err := tx.Exec(`INSERT INTO decisions (window_id, seq, decision_json) VALUES (?, ?, ?)`, w.WindowID, i, string(data)); err != nil {
			return engineError(CodeDecode, "checkpoint.decisions", "insert decision: %v", err)
		}
	}
	for _, consumption := range w.Consumption {
		if _, err := tx.Exec(`INSERT INTO change_consumptions
			(window_id, ordinal, change_id, at_us, frame) VALUES (?, ?, ?, ?, ?)`,
			w.WindowID, consumption.Ordinal, consumption.ChangeID, consumption.AtUS, consumption.Frame); err != nil {
			return engineError(CodeDecode, "checkpoint.consumption", "insert consumption: %v", err)
		}
	}
	snapshot, err := json.Marshal(w.GroupSnapshot)
	if err != nil {
		return engineError(CodeDecode, "checkpoint.group_snapshot", "encode snapshot: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO group_snapshots (window_id, snapshot_json) VALUES (?, ?)`, w.WindowID, string(snapshot)); err != nil {
		return engineError(CodeDecode, "checkpoint.group_snapshot", "insert snapshot: %v", err)
	}
	for _, dep := range w.Dependencies {
		if _, err := tx.Exec(`INSERT INTO dependencies (window_id, dep_kind, dep_key) VALUES (?, ?, ?)`,
			w.WindowID, dep.Kind, dep.Key); err != nil {
			return engineError(CodeDecode, "checkpoint.dependencies", "insert dependency: %v", err)
		}
	}
	result, err := tx.Exec(`UPDATE windows SET
		state = ?, project_hash = ?, algorithm_version = ?, source_version = ?, format_version = ?,
		dependency_hash = ?, content_hash = ?, change_offset_start = ?, change_offset_end = ?, committed_at_ms = ?
		WHERE window_id = ? AND state = ?`,
		string(StateCommitted), projectHash, algorithm, sourceVersion, formatVersion,
		depHash, w.ContentHash, w.ChangeOffsetStart, w.ChangeOffsetEnd, time.Now().UnixMilli(),
		w.WindowID, string(StateScheduling))
	if err != nil {
		return engineError(CodeDecode, "checkpoint.commit", "update window: %v", err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return engineError(CodeState, w.WindowID, "concurrent window state change")
	}
	if err := tx.Commit(); err != nil {
		return engineError(CodeDecode, "checkpoint.commit", "commit: %v", err)
	}
	return nil
}

func (s *Store) DeleteWindow(windowID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM windows WHERE window_id = ?`, windowID); err != nil {
		return engineError(CodeDecode, "checkpoint.delete", "evict %s: %v", windowID, err)
	}
	return nil
}

func (s *Store) AdoptWindow(windowID, projectHash, algorithm, sourceVersion, formatVersion string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(`UPDATE windows SET
		project_hash = ?, algorithm_version = ?, source_version = ?, format_version = ?
		WHERE window_id = ? AND state = ?`,
		projectHash, algorithm, sourceVersion, formatVersion, windowID, string(StateCommitted))
	if err != nil {
		return engineError(CodeDecode, "checkpoint.adopt", "adopt %s: %v", windowID, err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return engineError(CodeState, windowID, "only a committed window can be adopted")
	}
	return nil
}

type cachedWindowRow struct {
	id, state, projectHash, algorithm, sourceVersion, formatVersion, dependencyHash, contentHash string
	index, startUS, endUS, frameStart, frameEnd, offsetStart, offsetEnd                          int64
}

func (s *Store) queryWindows(where string, args ...any) ([]cachedWindowRow, error) {
	query := `SELECT window_id, window_index, start_us, end_us, frame_start, frame_end, state,
		project_hash, algorithm_version, source_version, format_version, dependency_hash, content_hash,
		change_offset_start, change_offset_end FROM windows`
	if where != "" {
		query += " WHERE " + where
	}
	query += ` ORDER BY window_index`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, engineError(CodeDecode, "checkpoint.windows", "query: %v", err)
	}
	defer rows.Close()
	var out []cachedWindowRow
	for rows.Next() {
		var r cachedWindowRow
		if err := rows.Scan(&r.id, &r.index, &r.startUS, &r.endUS, &r.frameStart, &r.frameEnd, &r.state,
			&r.projectHash, &r.algorithm, &r.sourceVersion, &r.formatVersion, &r.dependencyHash, &r.contentHash,
			&r.offsetStart, &r.offsetEnd); err != nil {
			return nil, engineError(CodeDecode, "checkpoint.windows", "scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, engineError(CodeDecode, "checkpoint.windows", "iterate: %v", err)
	}
	return out, nil
}

func (s *Store) readWindow(row cachedWindowRow) (*WindowCache, error) {
	w := WindowResult{
		WindowID: row.id, Index: int(row.index), StartUS: row.startUS, EndUS: row.endUS,
		FrameStart: row.frameStart, FrameEnd: row.frameEnd, ContentHash: row.contentHash,
		ChangeOffsetStart: int(row.offsetStart), ChangeOffsetEnd: int(row.offsetEnd),
	}
	rows, err := s.db.Query(`SELECT frame, fixture_id, at_us, color_r, color_g, color_b, intensity, pos_x, pos_y, pos_z, focus
		FROM outputs WHERE window_id = ? ORDER BY frame, light_index`, row.id)
	if err != nil {
		return nil, engineError(CodeDecode, "checkpoint.outputs", "%v", err)
	}
	byFrame := map[int]*FrameOutput{}
	for rows.Next() {
		var (
			frame, intensity, x, y, z, focus int
			atUS                             int64
			fixture                          string
			r, g, b                          int
		)
		if err := rows.Scan(&frame, &fixture, &atUS, &r, &g, &b, &intensity, &x, &y, &z, &focus); err != nil {
			rows.Close()
			return nil, engineError(CodeDecode, "checkpoint.outputs", "scan: %v", err)
		}
		f := byFrame[frame]
		if f == nil {
			f = &FrameOutput{Frame: frame, AtUS: atUS}
			byFrame[frame] = f
		}
		f.Lights = append(f.Lights, LightOutput{
			FixtureID: fixture, Color: RGB{R: uint16(r), G: uint16(g), B: uint16(b)},
			Intensity: intensity, Position: [3]int{x, y, z}, Focus: focus,
		})
	}
	rows.Close()
	frames := make([]int, 0, len(byFrame))
	for frame := range byFrame {
		frames = append(frames, frame)
	}
	sort.Ints(frames)
	for _, frame := range frames {
		w.Outputs = append(w.Outputs, *byFrame[frame])
	}

	drows, err := s.db.Query(`SELECT decision_json FROM decisions WHERE window_id = ? ORDER BY seq`, row.id)
	if err != nil {
		return nil, engineError(CodeDecode, "checkpoint.decisions", "%v", err)
	}
	for drows.Next() {
		var data []byte
		if err := drows.Scan(&data); err != nil {
			drows.Close()
			return nil, engineError(CodeDecode, "checkpoint.decisions", "scan: %v", err)
		}
		var d Decision
		if err := json.Unmarshal(data, &d); err != nil {
			drows.Close()
			return nil, engineError(CodeDecode, "checkpoint.decisions."+row.id, "decode: %v", err)
		}
		w.Decisions = append(w.Decisions, d)
	}
	drows.Close()

	crows, err := s.db.Query(`SELECT ordinal, change_id, at_us, frame FROM change_consumptions WHERE window_id = ? ORDER BY ordinal`, row.id)
	if err != nil {
		return nil, engineError(CodeDecode, "checkpoint.consumption", "%v", err)
	}
	for crows.Next() {
		var c ChangeConsumption
		if err := crows.Scan(&c.Ordinal, &c.ChangeID, &c.AtUS, &c.Frame); err != nil {
			crows.Close()
			return nil, engineError(CodeDecode, "checkpoint.consumption", "scan: %v", err)
		}
		w.Consumption = append(w.Consumption, c)
	}
	crows.Close()

	var snapshot []byte
	if err := s.db.QueryRow(`SELECT snapshot_json FROM group_snapshots WHERE window_id = ?`, row.id).Scan(&snapshot); err != nil {
		return nil, engineError(CodeDecode, "checkpoint.group_snapshot", "%v", err)
	}
	if err := json.Unmarshal(snapshot, &w.GroupSnapshot); err != nil {
		return nil, engineError(CodeDecode, "checkpoint.group_snapshot", "%v", err)
	}
	depRows, err := s.db.Query(`SELECT dep_kind, dep_key FROM dependencies WHERE window_id = ? ORDER BY dep_kind, dep_key`, row.id)
	if err != nil {
		return nil, engineError(CodeDecode, "checkpoint.dependencies", "%v", err)
	}
	for depRows.Next() {
		var dep Dependency
		if err := depRows.Scan(&dep.Kind, &dep.Key); err != nil {
			depRows.Close()
			return nil, engineError(CodeDecode, "checkpoint.dependencies", "scan: %v", err)
		}
		w.Dependencies = append(w.Dependencies, dep)
	}
	depRows.Close()
	actual := w.ComputeContentHash()
	if actual != row.contentHash {
		return nil, engineError(CodeDecode, "checkpoint.content_hash."+row.id, "content hash mismatch")
	}
	if hashJSON(w.Dependencies) != row.dependencyHash {
		return nil, engineError(CodeDecode, "checkpoint.dependency_hash."+row.id, "dependency hash mismatch")
	}
	return &WindowCache{
		Result: w, ProjectHash: row.projectHash, Algorithm: row.algorithm,
		SourceVersion: row.sourceVersion, FormatVersion: row.formatVersion, DependencyHash: row.dependencyHash,
	}, nil
}

func (s *Store) ReadCommitted(projectHash, algorithm, sourceVersion, formatVersion string) ([]WindowCache, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.queryWindows(`state = ? AND project_hash = ? AND algorithm_version = ? AND source_version = ? AND format_version = ?`,
		string(StateCommitted), projectHash, algorithm, sourceVersion, formatVersion)
	if err != nil {
		return nil, err
	}
	out := make([]WindowCache, 0, len(rows))
	for _, row := range rows {
		cache, err := s.readWindow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, *cache)
	}
	return out, nil
}

func (s *Store) ValidateCommitted() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.queryWindows(`state = ?`, string(StateCommitted))
	if err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := s.readWindow(row); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) AllCommitted(count int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM windows WHERE state = ?`, string(StateCommitted)).Scan(&n); err != nil {
		return false, engineError(CodeDecode, "checkpoint.windows", "%v", err)
	}
	return n == count, nil
}

func combineWindows(windows []WindowResult) *ScheduleResult {
	sort.Slice(windows, func(i, j int) bool { return windows[i].Index < windows[j].Index })
	result := &ScheduleResult{Windows: windows}
	consumption := make([]ChangeConsumption, 0)
	for _, w := range windows {
		result.Frames = append(result.Frames, w.Outputs...)
		result.Decisions = append(result.Decisions, w.Decisions...)
		for _, c := range w.Consumption {
			c.Ordinal = len(consumption) + 1
			consumption = append(consumption, c)
		}
	}
	sort.Slice(consumption, func(i, j int) bool {
		if consumption[i].AtUS != consumption[j].AtUS {
			return consumption[i].AtUS < consumption[j].AtUS
		}
		return consumption[i].Ordinal < consumption[j].Ordinal
	})
	result.Consumption = consumption
	return result
}

func windowContextError(err error) error {
	if err == nil {
		return nil
	}
	return engineError(CodeCanceled, "schedule", "cancelled: %v", err)
}

func (s *Store) String() string {
	return fmt.Sprintf("checkpoint(%p)", s)
}
