package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"

	_ "modernc.org/sqlite"

	"sre-timeline/internal/bizerr"
)

const sqliteHeader = "SQLite format 3\x00"

type Store struct {
	db   *sql.DB
	path string
}

type Round struct {
	RoundNo            int64               `json:"round_no"`
	ProjectID          string              `json:"project_id"`
	ProjectVersion     string              `json:"project_version"`
	SchemaVariant      string              `json:"schema_variant"`
	CommittedAtMS      int64               `json:"committed_at_ms"`
	ServiceStates      []ServiceState      `json:"service_states"`
	ContributionCaches []ContributionCache `json:"contribution_caches"`
}

type ServiceState struct {
	ServiceID string `json:"service_id"`
	StateJSON string `json:"state_json"`
}

type ContributionCache struct {
	IntervalKey string `json:"interval_key"`
	IntervalID  string `json:"interval_id"`
	PayloadJSON string `json:"payload_json"`
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path != ":memory:" {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			if info.Size() > 0 && info.Size() < 100 {
				return nil, bizerr.New(bizerr.CodeCheckpointTrunc, path, "checkpoint file is truncated").
					With("path", path).With("size_bytes", info.Size())
			}
			header := make([]byte, len(sqliteHeader))
			f, err := os.Open(path)
			if err == nil {
				n, _ := f.Read(header)
				_ = f.Close()
				if n == len(sqliteHeader) && string(header) != sqliteHeader {
					return nil, bizerr.New(bizerr.CodeCheckpointCorrupt, path, "checkpoint is not a SQLite database").
						With("path", path)
				}
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, bizerr.Wrap(bizerr.CodeCheckpointCorrupt, path, err, "open sqlite checkpoint failed")
	}
	store := &Store{db: db, path: path}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, mapDBError(path, err)
	}
	if err := store.init(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS review_rounds (
			round_no INTEGER PRIMARY KEY,
			project_id TEXT NOT NULL,
			project_version TEXT NOT NULL,
			schema_variant TEXT NOT NULL,
			committed_at_ms INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS service_states (
			round_no INTEGER NOT NULL,
			service_id TEXT NOT NULL,
			state_json TEXT NOT NULL,
			PRIMARY KEY(round_no, service_id)
		)`,
		`CREATE TABLE IF NOT EXISTS contribution_cache (
			round_no INTEGER NOT NULL,
			interval_key TEXT NOT NULL,
			interval_id TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			PRIMARY KEY(round_no, interval_key)
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return mapDBError(s.path, err)
		}
	}
	return nil
}

// SaveRound commits service/interval state and contribution cache first. The
// global latest_complete_round pointer is updated last in the same transaction.
func (s *Store) SaveRound(ctx context.Context, round Round) error {
	if round.ProjectVersion == "" {
		return bizerr.New(bizerr.CodeSchemaField, "checkpoint.project_version", "project version is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mapDBError(s.path, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM service_states WHERE round_no=?`, round.RoundNo); err != nil {
		return mapDBError(s.path, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM contribution_cache WHERE round_no=?`, round.RoundNo); err != nil {
		return mapDBError(s.path, err)
	}

	for _, state := range round.ServiceStates {
		payload := state.StateJSON
		if payload == "" {
			encoded, _ := json.Marshal(struct{}{})
			payload = string(encoded)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO service_states(round_no, service_id, state_json) VALUES(?,?,?)`,
			round.RoundNo, state.ServiceID, payload); err != nil {
			return mapDBError(s.path+"#service_states."+state.ServiceID, err)
		}
	}
	for _, cache := range round.ContributionCaches {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO contribution_cache(round_no, interval_key, interval_id, payload_json) VALUES(?,?,?,?)`,
			round.RoundNo, cache.IntervalKey, cache.IntervalID, cache.PayloadJSON); err != nil {
			return mapDBError(s.path+"#contribution_cache."+cache.IntervalKey, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO review_rounds(round_no, project_id, project_version, schema_variant, committed_at_ms)
		 VALUES(?,?,?,?,?)`,
		round.RoundNo, round.ProjectID, round.ProjectVersion, round.SchemaVariant, round.CommittedAtMS); err != nil {
		return mapDBError(s.path+"#review_rounds", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO checkpoint_meta(key,value) VALUES('latest_complete_round',?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		itoa64(round.RoundNo)); err != nil {
		return mapDBError(s.path+"#checkpoint_meta.latest_complete_round", err)
	}
	if err := tx.Commit(); err != nil {
		return mapDBError(s.path, err)
	}
	return nil
}

func (s *Store) LoadLatest(ctx context.Context, expectedVersion string) (*Round, error) {
	var latestText string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM checkpoint_meta WHERE key='latest_complete_round'`).Scan(&latestText)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, bizerr.New("E_CHECKPOINT_EMPTY", s.path, "no complete checkpoint round")
	}
	if err != nil {
		return nil, mapDBError(s.path+"#latest_complete_round", err)
	}
	var roundNo int64
	if _, scanErr := scanInt64(latestText, &roundNo); scanErr != nil {
		return nil, bizerr.Wrap(bizerr.CodeCheckpointCorrupt, s.path+"#latest_complete_round", scanErr, "invalid round pointer")
	}

	round := &Round{RoundNo: roundNo}
	err = s.db.QueryRowContext(ctx,
		`SELECT project_id, project_version, schema_variant, committed_at_ms FROM review_rounds WHERE round_no=?`,
		roundNo).Scan(&round.ProjectID, &round.ProjectVersion, &round.SchemaVariant, &round.CommittedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, bizerr.New(bizerr.CodeCheckpointCorrupt, s.path+"#review_rounds", "latest pointer refers to missing round %d", roundNo)
	}
	if err != nil {
		return nil, mapDBError(s.path+"#review_rounds", err)
	}
	if expectedVersion != "" && round.ProjectVersion != expectedVersion {
		return nil, bizerr.New(bizerr.CodeCheckpointVersion, s.path+"#project_version", "checkpoint version does not match current snapshot").
			With("stored_version", round.ProjectVersion).With("expected_version", expectedVersion).
			With("round_no", roundNo)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT service_id, state_json FROM service_states WHERE round_no=? ORDER BY service_id`, roundNo)
	if err != nil {
		return nil, mapDBError(s.path+"#service_states", err)
	}
	for rows.Next() {
		var state ServiceState
		if err := rows.Scan(&state.ServiceID, &state.StateJSON); err != nil {
			_ = rows.Close()
			return nil, mapDBError(s.path+"#service_states", err)
		}
		round.ServiceStates = append(round.ServiceStates, state)
	}
	_ = rows.Close()

	cacheRows, err := s.db.QueryContext(ctx,
		`SELECT interval_key, interval_id, payload_json FROM contribution_cache WHERE round_no=? ORDER BY interval_key`, roundNo)
	if err != nil {
		return nil, mapDBError(s.path+"#contribution_cache", err)
	}
	for cacheRows.Next() {
		var cache ContributionCache
		if err := cacheRows.Scan(&cache.IntervalKey, &cache.IntervalID, &cache.PayloadJSON); err != nil {
			_ = cacheRows.Close()
			return nil, mapDBError(s.path+"#contribution_cache", err)
		}
		round.ContributionCaches = append(round.ContributionCaches, cache)
	}
	_ = cacheRows.Close()
	return round, nil
}

func mapDBError(path string, err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "truncated") || strings.Contains(message, "end of file") {
		return bizerr.Wrap(bizerr.CodeCheckpointTrunc, path, err, "checkpoint is truncated")
	}
	if strings.Contains(message, "version") && strings.Contains(message, "mismatch") {
		return bizerr.Wrap(bizerr.CodeCheckpointVersion, path, err, "checkpoint version mismatch")
	}
	return bizerr.Wrap(bizerr.CodeCheckpointCorrupt, path, err, "checkpoint database error")
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	pos := len(b)
	for v > 0 {
		pos--
		b[pos] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func scanInt64(s string, out *int64) (int, error) {
	var v int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, bizerr.New(bizerr.CodeCheckpointCorrupt, "checkpoint.meta", "invalid integer %q", s)
		}
		v = v*10 + int64(r-'0')
	}
	*out = v
	return len(s), nil
}
