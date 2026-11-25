package checkpoint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

type ComponentRow struct {
	ChunkID        string
	Component      []byte
	ComponentKey   string
	CacheKey       string
	ContentHash    string
	ResidualDigest string
}

type StrokeCommit struct {
	ProjectID      string
	StrokeID       string
	StrokeNo       int
	ContentHash    string
	ResidualDigest string
	OrderVersion   string
	AlgoVersion    string
	ParamsVersion  string
	Components     []ComponentRow
}

type CommittedComponent struct {
	ChunkID        string
	Component      []byte
	CacheKey       string
	ComponentKey   string
	ContentHash    string
	ResidualDigest string
	CommitIndex    int
}

type ChunkOutput struct {
	ChunkID    string
	CacheKey   string
	InputHash  string
	OutputHash string
	Coverage   []byte
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, path: path}
	if err := s.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

func (s *Store) init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS commit_sequence (
	project_id TEXT NOT NULL,
	commit_index INTEGER NOT NULL,
	stroke_id TEXT NOT NULL,
	stroke_no INTEGER NOT NULL,
	content_hash TEXT NOT NULL,
	residual_digest TEXT NOT NULL,
	order_version TEXT NOT NULL,
	algo_version TEXT NOT NULL,
	params_version TEXT NOT NULL,
	PRIMARY KEY(project_id, commit_index)
);
CREATE TABLE IF NOT EXISTS stroke_components (
	project_id TEXT NOT NULL,
	stroke_id TEXT NOT NULL,
	chunk_id TEXT NOT NULL,
	commit_index INTEGER NOT NULL,
	component BLOB NOT NULL,
	component_key TEXT NOT NULL,
	cache_key TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	residual_digest TEXT NOT NULL,
	order_version TEXT NOT NULL,
	algo_version TEXT NOT NULL,
	params_version TEXT NOT NULL,
	PRIMARY KEY(project_id, stroke_id, chunk_id, order_version, algo_version, params_version)
);
CREATE TABLE IF NOT EXISTS chunk_outputs (
	project_id TEXT NOT NULL,
	chunk_id TEXT NOT NULL,
	cache_key TEXT NOT NULL,
	input_hash TEXT NOT NULL,
	output_hash TEXT NOT NULL,
	coverage BLOB NOT NULL,
	commit_index INTEGER NOT NULL,
	PRIMARY KEY(project_id, chunk_id)
);
CREATE TABLE IF NOT EXISTS topology_outputs (
	project_id TEXT PRIMARY KEY,
	cache_key TEXT NOT NULL,
	report_json BLOB NOT NULL,
	output_hash TEXT NOT NULL,
	commit_index INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_components_project_commit ON stroke_components(project_id, commit_index);
`)
	return err
}

// CommitStroke writes the sequence entry and all affected chunk components in one
// serializable transaction. The process-wide mutex and MaxOpenConns(1) guarantee
// that commit transactions never interleave within this process.
func (s *Store) CommitStroke(ctx context.Context, c StrokeCommit) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sort.Slice(c.Components, func(i, j int) bool { return c.Components[i].ChunkID < c.Components[j].ChunkID })

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var next int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(commit_index), 0) + 1 FROM commit_sequence WHERE project_id = ?`, c.ProjectID).Scan(&next); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO commit_sequence(project_id, commit_index, stroke_id, stroke_no, content_hash, residual_digest, order_version, algo_version, params_version)
VALUES(?,?,?,?,?,?,?,?,?)`,
		c.ProjectID, next, c.StrokeID, c.StrokeNo, c.ContentHash, c.ResidualDigest, c.OrderVersion, c.AlgoVersion, c.ParamsVersion); err != nil {
		return 0, err
	}
	for _, row := range c.Components {
		if len(row.Component) == 0 {
			return 0, fmt.Errorf("refusing empty component for stroke=%s chunk=%s", c.StrokeID, row.ChunkID)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO stroke_components(project_id, stroke_id, chunk_id, commit_index, component, component_key, cache_key, content_hash, residual_digest, order_version, algo_version, params_version)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(project_id, stroke_id, chunk_id, order_version, algo_version, params_version)
DO UPDATE SET commit_index=excluded.commit_index, component=excluded.component, component_key=excluded.component_key,
 cache_key=excluded.cache_key, content_hash=excluded.content_hash, residual_digest=excluded.residual_digest`,
			c.ProjectID, c.StrokeID, row.ChunkID, next, row.Component, row.ComponentKey, row.CacheKey,
			c.ContentHash, c.ResidualDigest, c.OrderVersion, c.AlgoVersion, c.ParamsVersion); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return next, nil
}

type CommittedState struct {
	Sequence   []string
	Commits    map[string]int
	Components map[string]map[string]CommittedComponent
}

func (s *Store) LoadCommittedState(ctx context.Context, projectID, orderVersion, algoVersion, paramsVersion string) (*CommittedState, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT commit_index, stroke_id FROM commit_sequence
WHERE project_id=? AND order_version=? AND algo_version=? AND params_version=?
ORDER BY commit_index ASC`, projectID, orderVersion, algoVersion, paramsVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	state := &CommittedState{Commits: map[string]int{}, Components: map[string]map[string]CommittedComponent{}}
	for rows.Next() {
		var idx int
		var strokeID string
		if err := rows.Scan(&idx, &strokeID); err != nil {
			return nil, err
		}
		state.Sequence = append(state.Sequence, strokeID)
		state.Commits[strokeID] = idx
		state.Components[strokeID] = map[string]CommittedComponent{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	crows, err := s.db.QueryContext(ctx, `
SELECT stroke_id, chunk_id, commit_index, component, component_key, cache_key, content_hash, residual_digest
FROM stroke_components
WHERE project_id=? AND order_version=? AND algo_version=? AND params_version=?
ORDER BY stroke_id, chunk_id`, projectID, orderVersion, algoVersion, paramsVersion)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var strokeID string
		var row CommittedComponent
		if err := crows.Scan(&strokeID, &row.ChunkID, &row.CommitIndex, &row.Component, &row.ComponentKey, &row.CacheKey, &row.ContentHash, &row.ResidualDigest); err != nil {
			return nil, err
		}
		if state.Components[strokeID] == nil {
			state.Components[strokeID] = map[string]CommittedComponent{}
		}
		state.Components[strokeID][row.ChunkID] = row
	}
	return state, crows.Err()
}

func (s *Store) SaveChunkOutput(ctx context.Context, projectID string, out ChunkOutput, commitIndex int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO chunk_outputs(project_id, chunk_id, cache_key, input_hash, output_hash, coverage, commit_index)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(project_id, chunk_id) DO UPDATE SET
 cache_key=excluded.cache_key, input_hash=excluded.input_hash, output_hash=excluded.output_hash,
 coverage=excluded.coverage, commit_index=excluded.commit_index`,
		projectID, out.ChunkID, out.CacheKey, out.InputHash, out.OutputHash, out.Coverage, commitIndex)
	return err
}

func (s *Store) LoadChunkOutput(ctx context.Context, projectID, chunkID string) (*ChunkOutput, error) {
	var out ChunkOutput
	err := s.db.QueryRowContext(ctx, `
SELECT chunk_id, cache_key, input_hash, output_hash, coverage
FROM chunk_outputs WHERE project_id=? AND chunk_id=?`, projectID, chunkID).
		Scan(&out.ChunkID, &out.CacheKey, &out.InputHash, &out.OutputHash, &out.Coverage)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) SaveTopology(ctx context.Context, projectID, cacheKey string, reportJSON []byte, outputHash string, commitIndex int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO topology_outputs(project_id, cache_key, report_json, output_hash, commit_index)
VALUES(?,?,?,?,?)
ON CONFLICT(project_id) DO UPDATE SET cache_key=excluded.cache_key, report_json=excluded.report_json,
 output_hash=excluded.output_hash, commit_index=excluded.commit_index`,
		projectID, cacheKey, reportJSON, outputHash, commitIndex)
	return err
}

func (s *Store) LoadTopology(ctx context.Context, projectID string) (cacheKey string, report []byte, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT cache_key, report_json FROM topology_outputs WHERE project_id=?`, projectID).Scan(&cacheKey, &report)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	return cacheKey, report, err
}

func (s *Store) CountCommits(ctx context.Context, projectID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commit_sequence WHERE project_id=?`, projectID).Scan(&n)
	return n, err
}

func (s *Store) SaveVerificationReport(ctx context.Context, projectID, reportJSON []byte) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS verification_reports(project_id TEXT PRIMARY KEY, report_json BLOB NOT NULL, updated_at TEXT DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO verification_reports(project_id, report_json) VALUES(?,?)
ON CONFLICT(project_id) DO UPDATE SET report_json=excluded.report_json, updated_at=CURRENT_TIMESTAMP`, projectID, reportJSON)
	return err
}
