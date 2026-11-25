package session

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"

	"qkernel/internal/errs"
	"qkernel/internal/imagex"
)

type State string

const (
	StatePending    State = "pending"
	StateProcessing State = "processing"
	StateCommitted  State = "committed"
	StateFailed     State = "failed"
)

var legalTransitions = map[State]map[State]bool{
	StatePending:    {StateProcessing: true},
	StateProcessing: {StateCommitted: true, StateFailed: true},
	StateFailed:     {StateProcessing: true},
	StateCommitted:  {},
}

func Transition(current, next State) error {
	if current == "" {
		current = StatePending
	}
	if current == next {
		return &errs.Error{Code: errs.EState, Message: fmt.Sprintf("state %s cannot transition to itself", current), StepIndex: -1, BlockX: -1, BlockY: -1}
	}
	if !legalTransitions[current][next] {
		return &errs.Error{Code: errs.EState, Message: fmt.Sprintf("illegal transition %s -> %s", current, next), StepIndex: -1, BlockX: -1, BlockY: -1}
	}
	return nil
}

type Store struct {
	db   *sql.DB
	mu   sync.Mutex
	Path string
}

func OpenStore(root string) (*Store, error) {
	path := filepath.Join(root, "checkpoints.db")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errs.Wrap(errs.EIO, err, "open sqlite store")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, Path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func OpenStorePath(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errs.Wrap(errs.EIO, err, "open sqlite store")
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, Path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS param_versions (
	param_version INTEGER PRIMARY KEY,
	chain_fingerprint TEXT NOT NULL,
	chain_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS runs (
	run_id TEXT PRIMARY KEY,
	frame_id TEXT NOT NULL,
	param_version INTEGER NOT NULL,
	segment_size INTEGER NOT NULL,
	metric_block_size INTEGER NOT NULL,
	workers INTEGER NOT NULL,
	chain_fingerprint TEXT NOT NULL,
	algo_version TEXT NOT NULL,
	root TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS unit_states (
	run_id TEXT NOT NULL,
	segment_index INTEGER NOT NULL,
	step_index INTEGER NOT NULL,
	state TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	cache_key TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(run_id, segment_index, step_index)
);
CREATE TABLE IF NOT EXISTS cache_entries (
	cache_key TEXT PRIMARY KEY,
	algo_version TEXT NOT NULL,
	chain_fingerprint TEXT NOT NULL,
	param_version INTEGER NOT NULL,
	step_index INTEGER NOT NULL,
	frame_id TEXT NOT NULL,
	segment_index INTEGER NOT NULL,
	input_hash TEXT NOT NULL,
	output_hash TEXT NOT NULL,
	output_path TEXT NOT NULL,
	summary_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS commits (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL,
	commit_order INTEGER NOT NULL,
	frame_id TEXT NOT NULL,
	segment_index INTEGER NOT NULL,
	step_index INTEGER NOT NULL,
	cache_key TEXT NOT NULL,
	input_hash TEXT NOT NULL,
	output_hash TEXT NOT NULL,
	output_path TEXT NOT NULL,
	region_json TEXT NOT NULL DEFAULT '{}',
	upstream_keys_json TEXT NOT NULL DEFAULT '[]',
	summary_json TEXT NOT NULL,
	chain_fingerprint TEXT NOT NULL,
	param_version INTEGER NOT NULL,
	algo_version TEXT NOT NULL,
	UNIQUE(run_id, commit_order),
	UNIQUE(run_id, segment_index, step_index)
);
CREATE TABLE IF NOT EXISTS state_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL,
	segment_index INTEGER NOT NULL,
	step_index INTEGER NOT NULL,
	from_state TEXT NOT NULL,
	to_state TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT ''
);
`)
	if err != nil {
		return errs.Wrap(errs.EIO, err, "migrate sqlite schema")
	}
	return nil
}

type RunRecord struct {
	RunID            string `json:"run_id"`
	FrameID          string `json:"frame_id"`
	ParamVersion     int    `json:"param_version"`
	SegmentSize      int    `json:"segment_size"`
	MetricBlockSize  int    `json:"metric_block_size"`
	Workers          int    `json:"workers"`
	ChainFingerprint string `json:"chain_fingerprint"`
	AlgoVersion      string `json:"algo_version"`
	Root             string `json:"root,omitempty"`
}

func (s *Store) EnsureRun(r RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var existing RunRecord
	err := s.db.QueryRow(`SELECT run_id,frame_id,param_version,segment_size,metric_block_size,workers,chain_fingerprint,algo_version,COALESCE(root,'') FROM runs WHERE run_id=?`, r.RunID).
		Scan(&existing.RunID, &existing.FrameID, &existing.ParamVersion, &existing.SegmentSize, &existing.MetricBlockSize, &existing.Workers, &existing.ChainFingerprint, &existing.AlgoVersion, &existing.Root)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.db.Exec(`INSERT INTO runs(run_id,frame_id,param_version,segment_size,metric_block_size,workers,chain_fingerprint,algo_version,root) VALUES(?,?,?,?,?,?,?,?,?)`,
			r.RunID, r.FrameID, r.ParamVersion, r.SegmentSize, r.MetricBlockSize, r.Workers, r.ChainFingerprint, r.AlgoVersion, r.Root)
		if err != nil {
			return errs.Wrap(errs.EIO, err, "insert run")
		}
		return nil
	}
	if err != nil {
		return errs.Wrap(errs.EIO, err, "query run")
	}
	if existing.RunID != r.RunID ||
		existing.FrameID != r.FrameID ||
		existing.ParamVersion != r.ParamVersion ||
		existing.SegmentSize != r.SegmentSize ||
		existing.MetricBlockSize != r.MetricBlockSize ||
		existing.ChainFingerprint != r.ChainFingerprint ||
		existing.AlgoVersion != r.AlgoVersion {
		return errs.New(errs.EMeta, "run id %s already exists with version-mismatched parameters: existing=%+v requested=%+v", r.RunID, existing, r)
	}
	if existing.Workers != r.Workers {
		_, _ = s.db.Exec(`UPDATE runs SET workers=? WHERE run_id=?`, r.Workers, r.RunID)
	}
	return nil
}

func (s *Store) LatestCommittedRun(frameID, excludeRunID string) (*RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`
SELECT r.run_id,r.frame_id,r.param_version,r.segment_size,r.metric_block_size,r.workers,r.chain_fingerprint,r.algo_version,COALESCE(r.root,'')
FROM runs r
JOIN commits c ON c.run_id=r.run_id
WHERE r.frame_id=? AND r.run_id<>?
GROUP BY r.run_id
ORDER BY MAX(c.id) DESC
LIMIT 1`, frameID, excludeRunID)
	var r RunRecord
	err := row.Scan(&r.RunID, &r.FrameID, &r.ParamVersion, &r.SegmentSize, &r.MetricBlockSize, &r.Workers, &r.ChainFingerprint, &r.AlgoVersion, &r.Root)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errs.Wrap(errs.EIO, err, "query latest committed run")
	}
	return &r, nil
}

func (s *Store) GetRun(runID string) (*RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`SELECT run_id,frame_id,param_version,segment_size,metric_block_size,workers,chain_fingerprint,algo_version,COALESCE(root,'') FROM runs WHERE run_id=?`, runID)
	var r RunRecord
	err := row.Scan(&r.RunID, &r.FrameID, &r.ParamVersion, &r.SegmentSize, &r.MetricBlockSize, &r.Workers, &r.ChainFingerprint, &r.AlgoVersion, &r.Root)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errs.New(errs.ECache, "unknown run id %s", runID)
	}
	if err != nil {
		return nil, errs.Wrap(errs.EIO, err, "query run")
	}
	return &r, nil
}

func (s *Store) LatestRun() (*RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`SELECT run_id,frame_id,param_version,segment_size,metric_block_size,workers,chain_fingerprint,algo_version,COALESCE(root,'') FROM runs ORDER BY rowid DESC LIMIT 1`)
	var r RunRecord
	err := row.Scan(&r.RunID, &r.FrameID, &r.ParamVersion, &r.SegmentSize, &r.MetricBlockSize, &r.Workers, &r.ChainFingerprint, &r.AlgoVersion, &r.Root)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errs.New(errs.ECache, "no generation run found")
	}
	if err != nil {
		return nil, errs.Wrap(errs.EIO, err, "query latest run")
	}
	return &r, nil
}

func (s *Store) RegisterParamVersion(version int, chainJSON, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var fp, cj string
	err := s.db.QueryRow(`SELECT chain_fingerprint,chain_json FROM param_versions WHERE param_version=?`, version).Scan(&fp, &cj)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.db.Exec(`INSERT INTO param_versions(param_version,chain_fingerprint,chain_json) VALUES(?,?,?)`, version, fingerprint, chainJSON)
		if err != nil {
			return errs.Wrap(errs.EIO, err, "insert param version")
		}
		return nil
	}
	if err != nil {
		return errs.Wrap(errs.EIO, err, "query param version")
	}
	if fp != fingerprint || cj != chainJSON {
		return errs.New(errs.EMeta, "param version %d is already bound to a different chain", version)
	}
	return nil
}

func (s *Store) ChainForVersion(version int) (string, string, error) {
	var fp, cj string
	err := s.db.QueryRow(`SELECT chain_fingerprint,chain_json FROM param_versions WHERE param_version=?`, version).Scan(&fp, &cj)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errs.New(errs.ECache, "unknown param version %d", version)
	}
	if err != nil {
		return "", "", errs.Wrap(errs.EIO, err, "query param version")
	}
	return cj, fp, nil
}

func (s *Store) currentStateTx(tx *sql.Tx, runID string, seg, step int) (State, int, error) {
	var state string
	var attempts int
	err := tx.QueryRow(`SELECT state,attempts FROM unit_states WHERE run_id=? AND segment_index=? AND step_index=?`, runID, seg, step).Scan(&state, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return StatePending, 0, nil
	}
	if err != nil {
		return "", 0, errs.Wrap(errs.EIO, err, "query unit state")
	}
	return State(state), attempts, nil
}

func (s *Store) TransitionState(runID string, seg, step int, next State, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return errs.Wrap(errs.EIO, err, "begin state transition")
	}
	defer tx.Rollback()
	current, attempts, err := s.currentStateTx(tx, runID, seg, step)
	if err != nil {
		return err
	}
	if err := Transition(current, next); err != nil {
		return err
	}
	newAttempts := attempts
	if next == StateProcessing {
		newAttempts++
	}
	_, err = tx.Exec(`INSERT INTO unit_states(run_id,segment_index,step_index,state,attempts,last_error)
VALUES(?,?,?,?,?,?)
ON CONFLICT(run_id,segment_index,step_index) DO UPDATE SET state=excluded.state,attempts=excluded.attempts,last_error=excluded.last_error`,
		runID, seg, step, next, newAttempts, reason)
	if err != nil {
		return errs.Wrap(errs.EIO, err, "upsert unit state")
	}
	_, err = tx.Exec(`INSERT INTO state_events(run_id,segment_index,step_index,from_state,to_state,reason) VALUES(?,?,?,?,?,?)`,
		runID, seg, step, current, next, reason)
	if err != nil {
		return errs.Wrap(errs.EIO, err, "insert state event")
	}
	return tx.Commit()
}

func (s *Store) RecoverProcessing(runID, reason string) ([]UnitKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT segment_index,step_index FROM unit_states WHERE run_id=? AND state=? ORDER BY step_index,segment_index`, runID, StateProcessing)
	if err != nil {
		return nil, errs.Wrap(errs.EIO, err, "query processing units")
	}
	var keys []UnitKey
	for rows.Next() {
		var k UnitKey
		if err := rows.Scan(&k.SegmentIndex, &k.StepIndex); err != nil {
			rows.Close()
			return nil, errs.Wrap(errs.EIO, err, "scan processing unit")
		}
		keys = append(keys, k)
	}
	rows.Close()
	for _, k := range keys {
		if err := s.transitionLocked(k.RunIDOr(runID), k.SegmentIndex, k.StepIndex, StateFailed, reason); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func (s *Store) EnsurePendingStates(runID string, stepCount, segmentCount int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return errs.Wrap(errs.EIO, err, "begin pending states")
	}
	defer tx.Rollback()
	for stepNo := 1; stepNo <= stepCount; stepNo++ {
		for seg := 0; seg < segmentCount; seg++ {
			_, err := tx.Exec(`INSERT OR IGNORE INTO unit_states(run_id,segment_index,step_index,state,attempts) VALUES(?,?,?,'pending',0)`, runID, seg, stepNo)
			if err != nil {
				return errs.Wrap(errs.EIO, err, "insert pending state")
			}
		}
	}
	return tx.Commit()
}

type UnitKey struct {
	RunID        string `json:"run_id,omitempty"`
	SegmentIndex int    `json:"segment_index"`
	StepIndex    int    `json:"step_index"`
}

func (k UnitKey) RunIDOr(fallback string) string {
	if k.RunID != "" {
		return k.RunID
	}
	return fallback
}

func (s *Store) transitionLocked(runID string, seg, step int, next State, reason string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return errs.Wrap(errs.EIO, err, "begin locked transition")
	}
	defer tx.Rollback()
	current, attempts, err := s.currentStateTx(tx, runID, seg, step)
	if err != nil {
		return err
	}
	if err := Transition(current, next); err != nil {
		return err
	}
	if next == StateProcessing {
		attempts++
	}
	_, err = tx.Exec(`INSERT INTO unit_states(run_id,segment_index,step_index,state,attempts,last_error) VALUES(?,?,?,?,?,?)
ON CONFLICT(run_id,segment_index,step_index) DO UPDATE SET state=excluded.state,attempts=excluded.attempts,last_error=excluded.last_error`,
		runID, seg, step, next, attempts, reason)
	if err != nil {
		return errs.Wrap(errs.EIO, err, "upsert recovered state")
	}
	_, err = tx.Exec(`INSERT INTO state_events(run_id,segment_index,step_index,from_state,to_state,reason) VALUES(?,?,?,?,?,?)`, runID, seg, step, current, next, reason)
	if err != nil {
		return errs.Wrap(errs.EIO, err, "insert recovered event")
	}
	return tx.Commit()
}

type CommitRecord struct {
	RunID            string          `json:"run_id"`
	CommitOrder      int             `json:"commit_order"`
	FrameID          string          `json:"frame_id"`
	SegmentIndex     int             `json:"segment_index"`
	StepIndex        int             `json:"step_index"`
	CacheKey         string          `json:"cache_key"`
	InputHash        string          `json:"input_hash"`
	OutputHash       string          `json:"output_hash"`
	OutputPath       string          `json:"output_path"`
	Region           imagex.Rect     `json:"region"`
	UpstreamKeys     []string        `json:"upstream_keys"`
	Summary          json.RawMessage `json:"summary"`
	ChainFingerprint string          `json:"chain_fingerprint"`
	ParamVersion     int             `json:"param_version"`
	AlgoVersion      string          `json:"algo_version"`
}

func (r CommitRecord) summaryString() string {
	if len(r.Summary) == 0 {
		return "{}"
	}
	return string(r.Summary)
}

func (s *Store) CommitUnit(c CommitRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return errs.Wrap(errs.EIO, err, "begin commit")
	}
	defer tx.Rollback()

	current, attempts, err := s.currentStateTx(tx, c.RunID, c.SegmentIndex, c.StepIndex)
	if err != nil {
		return err
	}
	if err := Transition(current, StateCommitted); err != nil {
		return err
	}
	regionJSON, _ := json.Marshal(c.Region)
	upstreamKeysJSON, _ := json.Marshal(c.UpstreamKeys)

	_, err = tx.Exec(`INSERT OR IGNORE INTO cache_entries(cache_key,algo_version,chain_fingerprint,param_version,step_index,frame_id,segment_index,input_hash,output_hash,output_path,summary_json)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		c.CacheKey, c.AlgoVersion, c.ChainFingerprint, c.ParamVersion, c.StepIndex, c.FrameID, c.SegmentIndex,
		c.InputHash, c.OutputHash, c.OutputPath, c.summaryString())
	if err != nil {
		return errs.Wrap(errs.EIO, err, "insert cache entry")
	}
	_, err = tx.Exec(`INSERT INTO commits(run_id,commit_order,frame_id,segment_index,step_index,cache_key,input_hash,output_hash,output_path,region_json,upstream_keys_json,summary_json,chain_fingerprint,param_version,algo_version)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.RunID, c.CommitOrder, c.FrameID, c.SegmentIndex, c.StepIndex, c.CacheKey, c.InputHash, c.OutputHash, c.OutputPath, string(regionJSON), string(upstreamKeysJSON), c.summaryString(), c.ChainFingerprint, c.ParamVersion, c.AlgoVersion)
	if err != nil {
		return errs.Wrap(errs.EIO, err, "insert commit")
	}
	_, err = tx.Exec(`INSERT INTO unit_states(run_id,segment_index,step_index,state,attempts,cache_key,last_error)
VALUES(?,?,?,'committed',?,?,'')
ON CONFLICT(run_id,segment_index,step_index) DO UPDATE SET state='committed',attempts=MAX(unit_states.attempts, excluded.attempts),cache_key=excluded.cache_key,last_error=''`,
		c.RunID, c.SegmentIndex, c.StepIndex, currentAttemptsOrOne(current, attempts), c.CacheKey)
	if err != nil {
		return errs.Wrap(errs.EIO, err, "commit unit state")
	}
	_, err = tx.Exec(`INSERT INTO state_events(run_id,segment_index,step_index,from_state,to_state,reason) VALUES(?,?,?,?,?,'atomic commit')`,
		c.RunID, c.SegmentIndex, c.StepIndex, current, StateCommitted)
	if err != nil {
		return errs.Wrap(errs.EIO, err, "insert commit event")
	}
	return tx.Commit()
}

func currentAttemptsOrOne(current State, attempts int) int {
	if attempts <= 0 {
		return 1
	}
	return attempts
}

type CacheEntry struct {
	CacheKey         string
	AlgoVersion      string
	ChainFingerprint string
	ParamVersion     int
	StepIndex        int
	FrameID          string
	SegmentIndex     int
	InputHash        string
	OutputHash       string
	OutputPath       string
	SummaryJSON      string
}

func (s *Store) GetCache(key string) (*CacheEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`SELECT cache_key,algo_version,chain_fingerprint,param_version,step_index,frame_id,segment_index,input_hash,output_hash,output_path,summary_json FROM cache_entries WHERE cache_key=?`, key)
	var e CacheEntry
	err := row.Scan(&e.CacheKey, &e.AlgoVersion, &e.ChainFingerprint, &e.ParamVersion, &e.StepIndex, &e.FrameID, &e.SegmentIndex, &e.InputHash, &e.OutputHash, &e.OutputPath, &e.SummaryJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errs.Wrap(errs.EIO, err, "query cache")
	}
	return &e, nil
}

func (s *Store) ListCommits(runID string) ([]CommitRecord, error) {
	rows, err := s.db.Query(`SELECT run_id,commit_order,frame_id,segment_index,step_index,cache_key,input_hash,output_hash,output_path,region_json,upstream_keys_json,summary_json,chain_fingerprint,param_version,algo_version
FROM commits WHERE run_id=? ORDER BY commit_order`, runID)
	if err != nil {
		return nil, errs.Wrap(errs.EIO, err, "list commits")
	}
	defer rows.Close()
	var out []CommitRecord
	for rows.Next() {
		var c CommitRecord
		var summary, regionJSON, upstreamKeysJSON string
		if err := rows.Scan(&c.RunID, &c.CommitOrder, &c.FrameID, &c.SegmentIndex, &c.StepIndex, &c.CacheKey, &c.InputHash, &c.OutputHash, &c.OutputPath, &regionJSON, &upstreamKeysJSON, &summary, &c.ChainFingerprint, &c.ParamVersion, &c.AlgoVersion); err != nil {
			return nil, errs.Wrap(errs.EIO, err, "scan commit")
		}
		_ = json.Unmarshal([]byte(regionJSON), &c.Region)
		_ = json.Unmarshal([]byte(upstreamKeysJSON), &c.UpstreamKeys)
		c.Summary = json.RawMessage(summary)
		out = append(out, c)
	}
	return out, nil
}

func (s *Store) CommittedKeySet(runID string) (map[[2]int]CommitRecord, error) {
	commits, err := s.ListCommits(runID)
	if err != nil {
		return nil, err
	}
	out := make(map[[2]int]CommitRecord, len(commits))
	for _, c := range commits {
		out[[2]int{c.StepIndex, c.SegmentIndex}] = c
	}
	return out, nil
}

type UnitStateRecord struct {
	SegmentIndex int    `json:"segment_index"`
	StepIndex    int    `json:"step_index"`
	State        State  `json:"state"`
	Attempts     int    `json:"attempts"`
	CacheKey     string `json:"cache_key"`
	LastError    string `json:"last_error"`
}

func (s *Store) ListUnitStates(runID string) ([]UnitStateRecord, error) {
	rows, err := s.db.Query(`SELECT segment_index,step_index,state,attempts,cache_key,last_error FROM unit_states WHERE run_id=? ORDER BY step_index,segment_index`, runID)
	if err != nil {
		return nil, errs.Wrap(errs.EIO, err, "list unit states")
	}
	defer rows.Close()
	var out []UnitStateRecord
	for rows.Next() {
		var r UnitStateRecord
		var state string
		if err := rows.Scan(&r.SegmentIndex, &r.StepIndex, &state, &r.Attempts, &r.CacheKey, &r.LastError); err != nil {
			return nil, errs.Wrap(errs.EIO, err, "scan unit state")
		}
		r.State = State(state)
		out = append(out, r)
	}
	return out, nil
}
