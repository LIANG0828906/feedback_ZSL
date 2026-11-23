package forensic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Snapshot struct {
	ImageID          string
	ParamsVersion    string
	AlgorithmVersion string
	Width            int
	Height           int
	Channels         int
	BlockSize        int
	HeatmapKey       string
	SummaryDigest    string
	Complete         bool
	UpdatedAt        string
}

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, ParamError("dbPath", "SQLite database path is required")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, DecodeError("create checkpoint directory", err)
		}
	}
	dsn := path
	if path != ":memory:" {
		dsn = "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, DecodeError("open SQLite checkpoint", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, path: path}
	if err := s.init(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

func (s *Store) Path() string { return s.path }

func (s *Store) init(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS schema_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS snapshots (
			image_id TEXT PRIMARY KEY,
			params_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			channels INTEGER NOT NULL,
			block_size INTEGER NOT NULL,
			heatmap_key TEXT NOT NULL DEFAULT '',
			summary_digest TEXT NOT NULL DEFAULT '',
			complete INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS blocks (
			image_id TEXT NOT NULL,
			block_index INTEGER NOT NULL,
			x0 INTEGER NOT NULL,
			y0 INTEGER NOT NULL,
			x1 INTEGER NOT NULL,
			y1 INTEGER NOT NULL,
			content_digest TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			fingerprint TEXT NOT NULL,
			noise_variance REAL NOT NULL,
			noise_rms REAL NOT NULL,
			residual_coverage REAL NOT NULL,
			edge_consistency REAL NOT NULL,
			contrast REAL NOT NULL,
			tamper_score REAL NOT NULL,
			tampered INTEGER NOT NULL,
			threshold REAL NOT NULL,
			window_size INTEGER NOT NULL,
			hash_key TEXT NOT NULL,
			stats_key TEXT NOT NULL,
			verdict_key TEXT NOT NULL,
			summary_key TEXT NOT NULL,
			state_digest TEXT NOT NULL,
			params_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			commit_seq INTEGER NOT NULL,
			committed_at TEXT NOT NULL,
			PRIMARY KEY(image_id, block_index)
		)`,
		`CREATE TABLE IF NOT EXISTS commits (
			image_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			block_index INTEGER NOT NULL,
			stage TEXT NOT NULL,
			state_digest TEXT NOT NULL,
			params_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY(image_id, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_commits_image_block ON commits(image_id, block_index)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return DecodeError("initialize SQLite schema", err)
		}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO schema_meta(key,value) VALUES('schema_version',?)
		 ON CONFLICT(key) DO NOTHING`, SchemaVersion)
	if err != nil {
		return DecodeError("write SQLite schema version", err)
	}
	var version string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key='schema_version'`).Scan(&version); err != nil {
		return DecodeError("read SQLite schema version", err)
	}
	if version != SchemaVersion {
		return DecodeError("unsupported SQLite schema version "+version, nil)
	}
	return nil
}

func (s *Store) LoadSnapshot(ctx context.Context, imageID string) (*Snapshot, []BlockState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var snap Snapshot
	var complete int
	err := s.db.QueryRowContext(ctx, `SELECT image_id,params_version,algorithm_version,width,height,channels,
		block_size,heatmap_key,summary_digest,complete,updated_at FROM snapshots WHERE image_id=?`, imageID).
		Scan(&snap.ImageID, &snap.ParamsVersion, &snap.AlgorithmVersion,
			&snap.Width, &snap.Height, &snap.Channels, &snap.BlockSize,
			&snap.HeatmapKey, &snap.SummaryDigest, &complete, &snap.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, DecodeError("load snapshot metadata", err)
	}
	snap.Complete = complete != 0

	rows, err := s.db.QueryContext(ctx, `SELECT block_index,x0,y0,x1,y1,content_digest,content_hash,
		fingerprint,noise_variance,noise_rms,residual_coverage,edge_consistency,contrast,tamper_score,tampered,
		threshold,window_size,hash_key,stats_key,verdict_key,summary_key,state_digest,
		params_version,algorithm_version,commit_seq FROM blocks WHERE image_id=? ORDER BY block_index`, imageID)
	if err != nil {
		return nil, nil, DecodeError("load block checkpoints", err)
	}
	defer rows.Close()
	var states []BlockState
	for rows.Next() {
		var st BlockState
		var tampered int
		if err := rows.Scan(&st.BlockIndex, &st.X0, &st.Y0, &st.X1, &st.Y1,
			&st.ContentDigest, &st.ContentHash, &st.Fingerprint,
			&st.Stats.NoiseVariance, &st.Stats.NoiseRMS, &st.Stats.ResidualCoverage, &st.Stats.EdgeConsistency,
			&st.Stats.Contrast, &st.Stats.TamperScore, &tampered,
			&st.Threshold, &st.Window, &st.HashKey, &st.StatsKey, &st.VerdictKey,
			&st.SummaryKey, &st.StateDigest, &st.ParamsVersion, &st.AlgorithmVersion,
			&st.CommitSeq); err != nil {
			return nil, nil, DecodeError("scan block checkpoint", err)
		}
		st.Tampered = tampered != 0
		st.ImageID = imageID
		states = append(states, st)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, DecodeError("iterate block checkpoints", err)
	}
	return &snap, states, nil
}

func (s *Store) UpsertSnapshot(ctx context.Context, snap Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsertSnapshotTx(ctx, s.db, snap)
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) upsertSnapshotTx(ctx context.Context, db execer, snap Snapshot) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if snap.UpdatedAt == "" {
		snap.UpdatedAt = now
	}
	_, err := db.ExecContext(ctx, `INSERT INTO snapshots(
		image_id,params_version,algorithm_version,width,height,channels,block_size,
		heatmap_key,summary_digest,complete,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(image_id) DO UPDATE SET
		params_version=excluded.params_version,algorithm_version=excluded.algorithm_version,
		width=excluded.width,height=excluded.height,channels=excluded.channels,
		block_size=excluded.block_size,heatmap_key=excluded.heatmap_key,
		summary_digest=excluded.summary_digest,complete=excluded.complete,updated_at=excluded.updated_at`,
		snap.ImageID, snap.ParamsVersion, snap.AlgorithmVersion, snap.Width, snap.Height,
		snap.Channels, snap.BlockSize, snap.HeatmapKey, snap.SummaryDigest,
		boolInt(snap.Complete), snap.UpdatedAt)
	if err != nil {
		return DecodeError("upsert snapshot metadata", err)
	}
	return nil
}

func (s *Store) ResetImage(ctx context.Context, snap Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DecodeError("begin snapshot reset", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM blocks WHERE image_id=?`, snap.ImageID); err != nil {
		return DecodeError("delete block snapshot", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM commits WHERE image_id=?`, snap.ImageID); err != nil {
		return DecodeError("delete commit ledger", err)
	}
	if err := s.upsertSnapshotTx(ctx, tx, snap); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return DecodeError("commit snapshot reset", err)
	}
	return nil
}

// CommitBlock atomically writes one exact block state and appends a commit
// ledger entry. The store mutex and SQLite transaction guarantee that check-
// point writes never interleave.
func (s *Store) CommitBlock(ctx context.Context, state BlockState, stage string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, DecodeError("begin block checkpoint", err)
	}
	defer tx.Rollback()
	var existingDigest, existingStatsKey, existingVerdictKey, existingSummaryKey string
	existingErr := tx.QueryRowContext(ctx, `SELECT state_digest,stats_key,verdict_key,summary_key
		FROM blocks WHERE image_id=? AND block_index=?`, state.ImageID, state.BlockIndex).
		Scan(&existingDigest, &existingStatsKey, &existingVerdictKey, &existingSummaryKey)
	if existingErr != nil && !errors.Is(existingErr, sql.ErrNoRows) {
		return 0, DecodeError("read existing block checkpoint", existingErr)
	}
	if existingErr == nil && existingStatsKey == state.StatsKey && existingVerdictKey == state.VerdictKey &&
		existingSummaryKey == state.SummaryKey && existingDigest != state.StateDigest {
		return 0, &Error{
			Code: CodeDiverge, ImageID: state.ImageID, Block: state.BlockIndex, Stage: StageAnalysis,
			Message: "refusing to silently replace an exact committed block result",
		}
	}
	var nextSeq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0)+1 FROM commits WHERE image_id=?`, state.ImageIDForStore()).Scan(&nextSeq); err != nil {
		return 0, DecodeError("allocate commit sequence", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO blocks(
		image_id,block_index,x0,y0,x1,y1,content_digest,content_hash,fingerprint,
		noise_variance,noise_rms,residual_coverage,edge_consistency,contrast,tamper_score,tampered,
		threshold,window_size,hash_key,stats_key,verdict_key,summary_key,state_digest,
		params_version,algorithm_version,commit_seq,committed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(image_id,block_index) DO UPDATE SET
		x0=excluded.x0,y0=excluded.y0,x1=excluded.x1,y1=excluded.y1,
		content_digest=excluded.content_digest,content_hash=excluded.content_hash,
		fingerprint=excluded.fingerprint,noise_variance=excluded.noise_variance,
		noise_rms=excluded.noise_rms,residual_coverage=excluded.residual_coverage,
		edge_consistency=excluded.edge_consistency,
		contrast=excluded.contrast,tamper_score=excluded.tamper_score,
		tampered=excluded.tampered,threshold=excluded.threshold,
		window_size=excluded.window_size,hash_key=excluded.hash_key,
		stats_key=excluded.stats_key,verdict_key=excluded.verdict_key,
		summary_key=excluded.summary_key,state_digest=excluded.state_digest,
		params_version=excluded.params_version,algorithm_version=excluded.algorithm_version,
		commit_seq=excluded.commit_seq,committed_at=excluded.committed_at`,
		state.ImageID, state.BlockIndex, state.X0, state.Y0, state.X1, state.Y1,
		state.ContentDigest, state.ContentHash, state.Fingerprint,
		state.Stats.NoiseVariance, state.Stats.NoiseRMS, state.Stats.ResidualCoverage, state.Stats.EdgeConsistency,
		state.Stats.Contrast, state.Stats.TamperScore, boolInt(state.Tampered),
		state.Threshold, state.Window, state.HashKey, state.StatsKey, state.VerdictKey,
		state.SummaryKey, state.StateDigest, state.ParamsVersion, state.AlgorithmVersion,
		nextSeq, now)
	if err != nil {
		return 0, DecodeError("write block checkpoint", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO commits(
		image_id,seq,block_index,stage,state_digest,params_version,algorithm_version,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, state.ImageID, nextSeq, state.BlockIndex, stage,
		state.StateDigest, state.ParamsVersion, state.AlgorithmVersion, now)
	if err != nil {
		return 0, DecodeError("append commit ledger", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, DecodeError("commit block checkpoint", err)
	}
	return nextSeq, nil
}

// PromoteMetadata atomically advances the persisted parameter version for
// exact results that remain valid under a threshold-only edit.
func (s *Store) PromoteMetadata(ctx context.Context, imageID, paramsVersion string, threshold float64, window int, verdictKeys, summaryKeys map[int]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DecodeError("begin metadata promotion", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT block_index,state_digest FROM blocks WHERE image_id=? ORDER BY block_index`, imageID)
	if err != nil {
		return DecodeError("read blocks for promotion", err)
	}
	type rec struct {
		index  int
		digest string
	}
	var records []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.index, &r.digest); err != nil {
			rows.Close()
			return DecodeError("scan metadata promotion", err)
		}
		records = append(records, r)
	}
	if err := rows.Close(); err != nil {
		return DecodeError("close metadata promotion cursor", err)
	}
	for _, r := range records {
		key, ok := verdictKeys[r.index]
		if !ok {
			return fmt.Errorf("missing verdict key for block %d", r.index)
		}
		summaryKey, ok := summaryKeys[r.index]
		if !ok {
			return fmt.Errorf("missing summary key for block %d", r.index)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE blocks SET params_version=?,threshold=?,window_size=?,verdict_key=?,summary_key=? WHERE image_id=? AND block_index=?`,
			paramsVersion, threshold, window, key, summaryKey, imageID, r.index); err != nil {
			return DecodeError("promote block metadata", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET params_version=?,updated_at=? WHERE image_id=?`,
		paramsVersion, time.Now().UTC().Format(time.RFC3339Nano), imageID); err != nil {
		return DecodeError("promote snapshot metadata", err)
	}
	if err := tx.Commit(); err != nil {
		return DecodeError("commit metadata promotion", err)
	}
	return nil
}

func (s *Store) MarkComplete(ctx context.Context, imageID, heatmapKey, summaryDigest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE snapshots SET complete=1,heatmap_key=?,summary_digest=?,updated_at=? WHERE image_id=?`,
		heatmapKey, summaryDigest, time.Now().UTC().Format(time.RFC3339Nano), imageID)
	if err != nil {
		return DecodeError("mark snapshot complete", err)
	}
	return nil
}

func (s *Store) CommitOrder(ctx context.Context, imageID string) ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT block_index FROM commits WHERE image_id=? ORDER BY seq`, imageID)
	if err != nil {
		return nil, DecodeError("read commit ledger", err)
	}
	defer rows.Close()
	var order []int
	for rows.Next() {
		var index int
		if err := rows.Scan(&index); err != nil {
			return nil, DecodeError("scan commit ledger", err)
		}
		order = append(order, index)
	}
	return order, rows.Err()
}

func (s *Store) Checkpoint(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(FULL)`); err != nil {
		return DecodeError("checkpoint SQLite WAL", err)
	}
	return nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// ImageIDForStore keeps Store.CommitBlock callers from accidentally omitting
// the image identifier while allowing BlockState to stay persistence-neutral.
func (s BlockState) ImageIDForStore() string { return s.ImageID }
