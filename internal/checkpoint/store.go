package checkpoint

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"roomreverb/internal/errs"
	"roomreverb/internal/model"
	"roomreverb/internal/solver"
)

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

type RunMeta struct {
	RunID            string `json:"run_id"`
	LevelHash        string `json:"level_hash"`
	ParamsVersion    int    `json:"params_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	Precision        string `json:"precision"`
}

type RoomSnapshot struct {
	Seq              int               `json:"seq"`
	RoomID           string            `json:"room_id"`
	InputHash        string            `json:"input_hash"`
	ContentHash      string            `json:"content_hash"`
	ParamsVersion    int               `json:"params_version"`
	AlgorithmVersion string            `json:"algorithm_version"`
	Precision        string            `json:"precision"`
	Result           solver.RoomResult `json:"result"`
}

type PrepareResult struct {
	Snapshots   []RoomSnapshot
	Reset       bool   `json:"reset"`
	ResetReason string `json:"reset_reason,omitempty"`
}

type cachePayload struct {
	RoomID           string            `json:"room_id"`
	RoomInputHash    string            `json:"room_input_hash"`
	ParamsVersion    int               `json:"params_version"`
	AlgorithmVersion string            `json:"algorithm_version"`
	Precision        string            `json:"precision"`
	Result           solver.RoomResult `json:"result"`
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
			return nil, errs.Wrap(errs.ECheck, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, errs.Wrap(errs.ECheck, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, errs.Wrap(errs.ECheck, err)
	}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS runs (
			run_id TEXT PRIMARY KEY,
			level_hash TEXT NOT NULL,
			params_version INTEGER NOT NULL,
			algorithm_version TEXT NOT NULL,
			precision TEXT NOT NULL,
			completed INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS snapshots (
			run_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			room_id TEXT NOT NULL,
			input_hash TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			params_version INTEGER NOT NULL,
			algorithm_version TEXT NOT NULL,
			precision TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (run_id, seq),
			UNIQUE (run_id, room_id)
		)`,
		`CREATE TABLE IF NOT EXISTS room_cache (
			room_id TEXT NOT NULL,
			room_input_hash TEXT NOT NULL,
			params_version INTEGER NOT NULL,
			algorithm_version TEXT NOT NULL,
			precision TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (room_id, room_input_hash, params_version, algorithm_version, precision)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return errs.Wrap(errs.ECheck, err)
		}
	}
	return nil
}

func (s *Store) Prepare(ctx context.Context, meta RunMeta) (PrepareResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		levelHash, algo, precision string
		paramsVersion              int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT level_hash, params_version, algorithm_version, precision FROM runs WHERE run_id=?`,
		meta.RunID,
	).Scan(&levelHash, &paramsVersion, &algo, &precision)
	if err != nil && err != sql.ErrNoRows {
		return PrepareResult{}, errs.Wrap(errs.ECheck, err)
	}
	reason := ""
	if err == sql.ErrNoRows {
		if err := s.insertRunLocked(ctx, meta); err != nil {
			return PrepareResult{}, err
		}
		return PrepareResult{}, nil
	}
	if levelHash != meta.LevelHash {
		reason = "level content hash mismatch"
	} else if paramsVersion != meta.ParamsVersion {
		reason = "parameter version mismatch"
	} else if algo != meta.AlgorithmVersion {
		reason = "algorithm version mismatch"
	} else if precision != meta.Precision {
		reason = "precision mismatch"
	}
	if reason != "" {
		if err := s.discardRunLocked(ctx, meta.RunID); err != nil {
			return PrepareResult{}, err
		}
		if err := s.insertRunLocked(ctx, meta); err != nil {
			return PrepareResult{}, err
		}
		return PrepareResult{Reset: true, ResetReason: reason}, nil
	}
	snaps, err := s.loadSnapshotsLocked(ctx, meta)
	if err != nil {
		if errs.IsCode(err, errs.ECheck) {
			if discardErr := s.discardRunLocked(ctx, meta.RunID); discardErr != nil {
				return PrepareResult{}, discardErr
			}
			if err := s.insertRunLocked(ctx, meta); err != nil {
				return PrepareResult{}, err
			}
			return PrepareResult{Reset: true, ResetReason: "snapshot content hash or payload mismatch"}, nil
		}
		return PrepareResult{}, err
	}
	if reason := validateSnapshotSequence(snaps, meta); reason != "" {
		if err := s.discardRunLocked(ctx, meta.RunID); err != nil {
			return PrepareResult{}, err
		}
		if err := s.insertRunLocked(ctx, meta); err != nil {
			return PrepareResult{}, err
		}
		return PrepareResult{Reset: true, ResetReason: reason}, nil
	}
	return PrepareResult{Snapshots: snaps}, nil
}

func (s *Store) SaveRoom(ctx context.Context, meta RunMeta, result solver.RoomResult) (RoomSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkRunLocked(ctx, meta); err != nil {
		return RoomSnapshot{}, err
	}
	var count, maxSeq int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(seq),0) FROM snapshots WHERE run_id=?`, meta.RunID,
	).Scan(&count, &maxSeq); err != nil {
		return RoomSnapshot{}, errs.Wrap(errs.ECheck, err)
	}
	seq := maxSeq + 1
	if seq != count+1 {
		return RoomSnapshot{}, &errs.Error{Code: errs.ECheck, Room: result.RoomID, Field: "seq", Message: "snapshot commit sequence has a gap"}
	}
	payload, err := json.Marshal(cachePayload{
		RoomID: result.RoomID, RoomInputHash: meta.LevelHash,
		ParamsVersion: meta.ParamsVersion, AlgorithmVersion: meta.AlgorithmVersion,
		Precision: meta.Precision, Result: result,
	})
	if err != nil {
		return RoomSnapshot{}, errs.Wrap(errs.ECheck, err)
	}
	contentHash := sha256Hex(payload)
	snap := RoomSnapshot{
		Seq: seq, RoomID: result.RoomID, InputHash: meta.LevelHash,
		ContentHash: contentHash, ParamsVersion: meta.ParamsVersion,
		AlgorithmVersion: meta.AlgorithmVersion, Precision: meta.Precision,
		Result: result.Clone(),
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RoomSnapshot{}, errs.Wrap(errs.ECheck, err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO snapshots(run_id,seq,room_id,input_hash,content_hash,params_version,algorithm_version,precision,payload_json,created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		meta.RunID, seq, result.RoomID, meta.LevelHash, contentHash,
		meta.ParamsVersion, meta.AlgorithmVersion, meta.Precision, string(payload), now,
	); err != nil {
		_ = tx.Rollback()
		return RoomSnapshot{}, errs.Wrap(errs.ECheck, err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runs SET updated_at=? WHERE run_id=?`, now, meta.RunID); err != nil {
		_ = tx.Rollback()
		return RoomSnapshot{}, errs.Wrap(errs.ECheck, err)
	}
	if err := tx.Commit(); err != nil {
		return RoomSnapshot{}, errs.Wrap(errs.ECheck, err)
	}
	return snap, nil
}

func (s *Store) MarkComplete(ctx context.Context, meta RunMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET completed=1, updated_at=? WHERE run_id=?`,
		time.Now().UTC().Format(time.RFC3339Nano), meta.RunID,
	)
	return errs.Wrap(errs.ECheck, err)
}

func (s *Store) DiscardRun(ctx context.Context, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.discardRunLocked(ctx, runID)
}

func (s *Store) SaveCache(ctx context.Context, result solver.RoomResult, roomInputHash string, paramsVersion int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, err := json.Marshal(cachePayload{
		RoomID: result.RoomID, RoomInputHash: roomInputHash,
		ParamsVersion: paramsVersion, AlgorithmVersion: model.AlgorithmVersion,
		Precision: model.Precision, Result: result,
	})
	if err != nil {
		return errs.Wrap(errs.ECheck, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO room_cache(room_id,room_input_hash,params_version,algorithm_version,precision,content_hash,payload_json,created_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(room_id,room_input_hash,params_version,algorithm_version,precision)
		 DO UPDATE SET content_hash=excluded.content_hash, payload_json=excluded.payload_json, created_at=excluded.created_at`,
		result.RoomID, roomInputHash, paramsVersion, model.AlgorithmVersion, model.Precision,
		sha256Hex(payload), string(payload), time.Now().UTC().Format(time.RFC3339Nano),
	)
	return errs.Wrap(errs.ECheck, err)
}

func (s *Store) LoadCache(ctx context.Context, roomID, roomInputHash string, paramsVersion int) (solver.RoomResult, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var payloadJSON, contentHash string
	err := s.db.QueryRowContext(ctx,
		`SELECT payload_json, content_hash FROM room_cache
		 WHERE room_id=? AND room_input_hash=? AND params_version=? AND algorithm_version=? AND precision=?`,
		roomID, roomInputHash, paramsVersion, model.AlgorithmVersion, model.Precision,
	).Scan(&payloadJSON, &contentHash)
	if err == sql.ErrNoRows {
		return solver.RoomResult{}, false, nil
	}
	if err != nil {
		return solver.RoomResult{}, false, errs.Wrap(errs.ECheck, err)
	}
	if sha256Hex([]byte(payloadJSON)) != contentHash {
		return solver.RoomResult{}, false, &errs.Error{Code: errs.ECheck, Room: roomID, Field: "content_hash", Message: "room cache hash mismatch"}
	}
	var payload cachePayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return solver.RoomResult{}, false, errs.Wrap(errs.ECheck, err)
	}
	if payload.RoomInputHash != roomInputHash || payload.ParamsVersion != paramsVersion ||
		payload.AlgorithmVersion != model.AlgorithmVersion || payload.Precision != model.Precision {
		return solver.RoomResult{}, false, &errs.Error{Code: errs.ECheck, Room: roomID, Field: "versions", Message: "room cache version mismatch"}
	}
	return payload.Result.Clone(), true, nil
}

func (s *Store) CountSnapshots(ctx context.Context, runID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshots WHERE run_id=?`, runID).Scan(&n)
	return n, errs.Wrap(errs.ECheck, err)
}

func (s *Store) insertRunLocked(ctx context.Context, meta RunMeta) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs(run_id,level_hash,params_version,algorithm_version,precision,completed,updated_at)
		 VALUES(?,?,?,?,?,0,?)`,
		meta.RunID, meta.LevelHash, meta.ParamsVersion, meta.AlgorithmVersion, meta.Precision,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	return errs.Wrap(errs.ECheck, err)
}

func (s *Store) checkRunLocked(ctx context.Context, meta RunMeta) error {
	var hash, algo, precision string
	var version int
	err := s.db.QueryRowContext(ctx,
		`SELECT level_hash,params_version,algorithm_version,precision FROM runs WHERE run_id=?`, meta.RunID,
	).Scan(&hash, &version, &algo, &precision)
	if err != nil {
		return errs.Wrap(errs.ECheck, err)
	}
	if hash != meta.LevelHash || version != meta.ParamsVersion || algo != meta.AlgorithmVersion || precision != meta.Precision {
		return &errs.Error{Code: errs.ECheck, Field: "run_meta", Message: "run metadata changed after prepare"}
	}
	return nil
}

func (s *Store) discardRunLocked(ctx context.Context, runID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errs.Wrap(errs.ECheck, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE run_id=?`, runID); err != nil {
		_ = tx.Rollback()
		return errs.Wrap(errs.ECheck, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE run_id=?`, runID); err != nil {
		_ = tx.Rollback()
		return errs.Wrap(errs.ECheck, err)
	}
	return errs.Wrap(errs.ECheck, tx.Commit())
}

func (s *Store) loadSnapshotsLocked(ctx context.Context, meta RunMeta) ([]RoomSnapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq,room_id,input_hash,content_hash,params_version,algorithm_version,precision,payload_json
		 FROM snapshots WHERE run_id=? ORDER BY seq ASC`, meta.RunID)
	if err != nil {
		return nil, errs.Wrap(errs.ECheck, err)
	}
	defer rows.Close()
	var out []RoomSnapshot
	for rows.Next() {
		var snap RoomSnapshot
		var payloadJSON string
		if err := rows.Scan(&snap.Seq, &snap.RoomID, &snap.InputHash, &snap.ContentHash,
			&snap.ParamsVersion, &snap.AlgorithmVersion, &snap.Precision, &payloadJSON); err != nil {
			return nil, errs.Wrap(errs.ECheck, err)
		}
		if sha256Hex([]byte(payloadJSON)) != snap.ContentHash {
			return nil, &errs.Error{Code: errs.ECheck, Room: snap.RoomID, Field: "content_hash", Message: "snapshot content hash mismatch"}
		}
		var payload cachePayload
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			return nil, errs.Wrap(errs.ECheck, err)
		}
		snap.Result = payload.Result.Clone()
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(errs.ECheck, err)
	}
	return out, nil
}

func validateSnapshotSequence(snaps []RoomSnapshot, meta RunMeta) string {
	seen := map[string]bool{}
	for i, s := range snaps {
		if s.Seq != i+1 {
			return "snapshot sequence has a gap"
		}
		if seen[s.RoomID] {
			return "duplicate room snapshot"
		}
		seen[s.RoomID] = true
		if s.InputHash != meta.LevelHash {
			return "room content hash mismatch"
		}
		if s.ParamsVersion != meta.ParamsVersion {
			return "room parameter version mismatch"
		}
		if s.AlgorithmVersion != meta.AlgorithmVersion {
			return "room algorithm version mismatch"
		}
		if s.Precision != meta.Precision {
			return "room precision mismatch"
		}
		if s.Result.RoomID != s.RoomID {
			return "room identifier mismatch"
		}
	}
	return ""
}

func SnapshotMap(snaps []RoomSnapshot) map[string]RoomSnapshot {
	out := make(map[string]RoomSnapshot, len(snaps))
	for _, s := range snaps {
		out[s.RoomID] = s
	}
	return out
}

func SortedRoomIDs(level model.Level) []string {
	ids := make([]string, 0, len(level.Rooms))
	for _, r := range level.Rooms {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

func LevelHash(level model.Level) string { return sha256Hex(level.Canonical()) }

// RoomInputHash projects only the data that can influence a room's listener
// solutions. Room rectangles are excluded because paths use portal centres,
// endpoints and source/listener coordinates.
func RoomInputHash(level model.Level, roomID string) string {
	type roomProjection struct {
		ID     string           `json:"id"`
		Params model.RoomParams `json:"params"`
	}
	type openingProjection struct {
		ID     string      `json:"id"`
		WallID string      `json:"wall_id"`
		Center model.Point `json:"center"`
		Width  float64     `json:"width"`
		Active bool        `json:"active"`
	}
	type wallProjection struct {
		ID    string   `json:"id"`
		Rooms []string `json:"rooms"`
	}
	type listenerProjection struct {
		ID  string      `json:"id"`
		Pos model.Point `json:"pos"`
	}
	type projection struct {
		RoomID    string               `json:"room_id"`
		Rooms     []roomProjection     `json:"rooms"`
		Walls     []wallProjection     `json:"walls"`
		Openings  []openingProjection  `json:"openings"`
		Sources   []model.Source       `json:"sources"`
		Listeners []listenerProjection `json:"listeners"`
	}
	p := projection{RoomID: roomID}
	for _, r := range level.Rooms {
		p.Rooms = append(p.Rooms, roomProjection{ID: r.ID, Params: r.Params})
	}
	for _, w := range level.Walls {
		p.Walls = append(p.Walls, wallProjection{ID: w.ID, Rooms: append([]string(nil), w.Rooms...)})
	}
	for _, o := range level.Openings {
		p.Openings = append(p.Openings, openingProjection{ID: o.ID, WallID: o.WallID, Center: o.Center, Width: o.Width, Active: o.Active})
	}
	p.Sources = append(p.Sources, level.Sources...)
	for _, l := range level.Listeners {
		room, err := validateRoomAt(level, l.Pos)
		if err == nil && room == roomID {
			p.Listeners = append(p.Listeners, listenerProjection{ID: l.ID, Pos: l.Pos})
		}
	}
	sort.Slice(p.Rooms, func(i, j int) bool { return p.Rooms[i].ID < p.Rooms[j].ID })
	sort.Slice(p.Walls, func(i, j int) bool { return p.Walls[i].ID < p.Walls[j].ID })
	sort.Slice(p.Openings, func(i, j int) bool { return p.Openings[i].ID < p.Openings[j].ID })
	sort.Slice(p.Sources, func(i, j int) bool { return p.Sources[i].ID < p.Sources[j].ID })
	sort.Slice(p.Listeners, func(i, j int) bool { return p.Listeners[i].ID < p.Listeners[j].ID })
	b, _ := json.Marshal(p)
	return sha256Hex(b)
}

func validateRoomAt(level model.Level, p model.Point) (string, error) {
	var found string
	for _, r := range level.Rooms {
		if r.Rect.ContainsInclusive(p) {
			if found != "" {
				return "", fmt.Errorf("point in multiple rooms")
			}
			found = r.ID
		}
	}
	if found == "" {
		return "", fmt.Errorf("point outside rooms")
	}
	return found, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return "."
}
