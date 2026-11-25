package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"lipsync/domain"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

type ExpectedShard struct {
	ShardIndex       int
	DialogueID       string
	ContentHash      string
	SourceVersion    string
	QuantVersion     string
	AlgorithmVersion string
	TruthFingerprint string
}

type Invalidated struct {
	ShardIndex int      `json:"shard_index"`
	DialogueID string   `json:"dialogue_id"`
	Fields     []string `json:"fields"`
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS commit_counter (id INTEGER PRIMARY KEY CHECK (id=1), n INTEGER NOT NULL)`,
		`INSERT OR IGNORE INTO commit_counter(id,n) VALUES(1,0)`,
		`CREATE TABLE IF NOT EXISTS shards (
			shard_index INTEGER PRIMARY KEY,
			dialogue_id TEXT NOT NULL UNIQUE,
			state TEXT NOT NULL,
			source_version TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			quant_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			truth_fingerprint TEXT NOT NULL,
			package_summary TEXT NOT NULL,
			packages_json BLOB NOT NULL,
			commit_seq INTEGER NOT NULL UNIQUE
		)`,
		`CREATE TABLE IF NOT EXISTS frames (
			shard_index INTEGER NOT NULL,
			frame INTEGER NOT NULL,
			package_ids_json TEXT NOT NULL,
			PRIMARY KEY (shard_index, frame),
			FOREIGN KEY (shard_index) REFERENCES shards(shard_index) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS dispatch_events (
			shard_index INTEGER NOT NULL,
			event_seq INTEGER NOT NULL,
			frame INTEGER NOT NULL,
			package_id TEXT NOT NULL,
			status TEXT NOT NULL,
			reason TEXT NOT NULL,
			retry_frame INTEGER NOT NULL,
			PRIMARY KEY (shard_index, event_seq),
			FOREIGN KEY (shard_index) REFERENCES shards(shard_index) ON DELETE CASCADE
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// CommitShard atomically writes package placement, every frame dispatch, all
// events, hashes, and version metadata under one SQLite transaction. A process
// mutex plus a single connection serializes writers so commit sequences never
// interleave.
func (s *Store) CommitShard(ctx context.Context, a domain.ShardArtifact) (domain.ShardArtifact, error) {
	if a.DialogueID == "" || a.SourceVersion == "" || a.ContentHash == "" ||
		a.QuantVersion == "" || a.AlgorithmVersion == "" || a.TruthFingerprint == "" {
		return a, &domain.Error{
			Code: domain.CodeStage, ShardIndex: a.ShardIndex, DialogueID: a.DialogueID,
			Field: "shard.metadata", Message: "commit requires all hashes and version identifiers",
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if a.PackageSummary == "" {
		canonical := append([]domain.ScheduledPackage(nil), a.Packages...)
		sort.Slice(canonical, func(i, j int) bool { return canonical[i].ID < canonical[j].ID })
		sum, err := domain.Hash(canonical)
		if err != nil {
			return a, err
		}
		a.PackageSummary = sum
	}
	packagesJSON, err := json.Marshal(a.Packages)
	if err != nil {
		return a, err
	}
	frames := append([]domain.FrameRecord(nil), a.Frames...)
	sort.Slice(frames, func(i, j int) bool { return frames[i].Frame < frames[j].Frame })

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()

	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT n FROM commit_counter WHERE id=1`).Scan(&seq); err != nil {
		return a, err
	}
	seq++
	if _, err := tx.ExecContext(ctx, `UPDATE commit_counter SET n=? WHERE id=1`, seq); err != nil {
		return a, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO shards(
		shard_index,dialogue_id,state,source_version,content_hash,quant_version,
		algorithm_version,truth_fingerprint,package_summary,packages_json,commit_seq
	) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		a.ShardIndex, a.DialogueID, string(stageCommitted), a.SourceVersion, a.ContentHash,
		a.QuantVersion, a.AlgorithmVersion, a.TruthFingerprint, a.PackageSummary,
		packagesJSON, seq)
	if err != nil {
		return a, &domain.Error{
			Code: domain.CodeState, ShardIndex: a.ShardIndex, DialogueID: a.DialogueID,
			Field: "shard_index", Message: "shard already committed or metadata conflicts",
			Cause: err,
		}
	}
	for _, fr := range frames {
		ids, err := json.Marshal(append([]string(nil), fr.PackageIDs...))
		if err != nil {
			return a, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO frames(shard_index,frame,package_ids_json) VALUES(?,?,?)`,
			a.ShardIndex, fr.Frame, ids); err != nil {
			return a, err
		}
	}
	for i, ev := range a.Events {
		if _, err := tx.ExecContext(ctx, `INSERT INTO dispatch_events(
			shard_index,event_seq,frame,package_id,status,reason,retry_frame
		) VALUES(?,?,?,?,?,?,?)`,
			a.ShardIndex, i, ev.Frame, ev.PackageID, ev.Status, ev.Reason, ev.RetryFrame); err != nil {
			return a, err
		}
	}
	if err := tx.Commit(); err != nil {
		return a, err
	}
	a.CommitSeq = seq
	a.State = string(stageCommitted)
	return a, nil
}

// stageCommitted mirrors stage.Committed while keeping store free of a runtime
// state-machine dependency.
const stageCommitted = "committed"

func (s *Store) LoadShard(ctx context.Context, shardIndex int) (domain.ShardArtifact, error) {
	var a domain.ShardArtifact
	var packagesJSON []byte
	err := s.db.QueryRowContext(ctx, `SELECT
		shard_index,dialogue_id,state,source_version,content_hash,quant_version,
		algorithm_version,truth_fingerprint,package_summary,packages_json,commit_seq
		FROM shards WHERE shard_index=?`, shardIndex).Scan(
		&a.ShardIndex, &a.DialogueID, &a.State, &a.SourceVersion, &a.ContentHash,
		&a.QuantVersion, &a.AlgorithmVersion, &a.TruthFingerprint,
		&a.PackageSummary, &packagesJSON, &a.CommitSeq)
	if err != nil {
		return a, err
	}
	if err := json.Unmarshal(packagesJSON, &a.Packages); err != nil {
		return a, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT frame,package_ids_json FROM frames WHERE shard_index=? ORDER BY frame`, shardIndex)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		var fr domain.FrameRecord
		var ids []byte
		if err := rows.Scan(&fr.Frame, &ids); err != nil {
			rows.Close()
			return a, err
		}
		if err := json.Unmarshal(ids, &fr.PackageIDs); err != nil {
			rows.Close()
			return a, err
		}
		a.Frames = append(a.Frames, fr)
	}
	if err := rows.Close(); err != nil {
		return a, err
	}
	evRows, err := s.db.QueryContext(ctx, `SELECT
		frame,package_id,status,reason,retry_frame
		FROM dispatch_events WHERE shard_index=? ORDER BY event_seq`, shardIndex)
	if err != nil {
		return a, err
	}
	for evRows.Next() {
		ev := domain.DispatchEvent{DialogueID: a.DialogueID, ShardIndex: a.ShardIndex}
		if err := evRows.Scan(&ev.Frame, &ev.PackageID, &ev.Status, &ev.Reason, &ev.RetryFrame); err != nil {
			evRows.Close()
			return a, err
		}
		a.Events = append(a.Events, ev)
	}
	return a, evRows.Close()
}

func (s *Store) LoadAllShards(ctx context.Context) ([]domain.ShardArtifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT shard_index FROM shards ORDER BY shard_index`)
	if err != nil {
		return nil, err
	}
	var indexes []int
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			rows.Close()
			return nil, err
		}
		indexes = append(indexes, idx)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]domain.ShardArtifact, 0, len(indexes))
	for _, idx := range indexes {
		a, err := s.LoadShard(ctx, idx)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// ValidateExpected invalidates entire shards whose persisted hash or any
// version marker differs. Invalidation and its child rows are removed in one
// transaction per shard.
func (s *Store) ValidateExpected(ctx context.Context, expected []ExpectedShard) ([]Invalidated, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sort.Slice(expected, func(i, j int) bool { return expected[i].ShardIndex < expected[j].ShardIndex })
	expectedSet := make(map[int]ExpectedShard, len(expected))
	for _, exp := range expected {
		expectedSet[exp.ShardIndex] = exp
	}
	var invalid []Invalidated
	for _, exp := range expected {
		a, err := s.LoadShard(ctx, exp.ShardIndex)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var fields []string
		if a.DialogueID != exp.DialogueID {
			fields = append(fields, "dialogue_id")
		}
		if a.ContentHash != exp.ContentHash {
			fields = append(fields, "content_hash")
		}
		if a.SourceVersion != exp.SourceVersion {
			fields = append(fields, "source_version")
		}
		if a.QuantVersion != exp.QuantVersion {
			fields = append(fields, "quantization_version")
		}
		if a.AlgorithmVersion != exp.AlgorithmVersion {
			fields = append(fields, "algorithm_version")
		}
		if a.TruthFingerprint != exp.TruthFingerprint {
			fields = append(fields, "truth_fingerprint")
		}
		if len(fields) == 0 {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dispatch_events WHERE shard_index=?`, exp.ShardIndex); err != nil {
			tx.Rollback()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM frames WHERE shard_index=?`, exp.ShardIndex); err != nil {
			tx.Rollback()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM shards WHERE shard_index=?`, exp.ShardIndex); err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		invalid = append(invalid, Invalidated{ShardIndex: exp.ShardIndex, DialogueID: exp.DialogueID, Fields: fields})
	}
	rows, err := s.db.QueryContext(ctx, `SELECT shard_index,dialogue_id FROM shards ORDER BY shard_index`)
	if err != nil {
		return nil, err
	}
	type extra struct {
		idx  int
		name string
	}
	var extras []extra
	for rows.Next() {
		var idx int
		var name string
		if err := rows.Scan(&idx, &name); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := expectedSet[idx]; !ok {
			extras = append(extras, extra{idx: idx, name: name})
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, x := range extras {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dispatch_events WHERE shard_index=?`, x.idx); err != nil {
			tx.Rollback()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM frames WHERE shard_index=?`, x.idx); err != nil {
			tx.Rollback()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM shards WHERE shard_index=?`, x.idx); err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		invalid = append(invalid, Invalidated{ShardIndex: x.idx, DialogueID: x.name, Fields: []string{"unexpected_shard"}})
	}
	return invalid, nil
}

func (s *Store) CommitSequence(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT commit_seq FROM shards ORDER BY commit_seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		seqs = append(seqs, n)
	}
	return seqs, rows.Err()
}

// DeleteShard removes one committed shard and all child rows atomically. A
// crash after this deletion is safe: the shard is simply missing and will be
// recomputed on recovery.
func (s *Store) DeleteShard(ctx context.Context, shardIndex int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM dispatch_events WHERE shard_index=?`, shardIndex); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM frames WHERE shard_index=?`, shardIndex); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM shards WHERE shard_index=?`, shardIndex); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM shards`).Scan(&n)
	return n, err
}

func (s *Store) DB() *sql.DB { return s.db }
