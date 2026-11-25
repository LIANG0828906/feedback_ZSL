package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"texturekernel"
)

type RunMeta struct {
	RunID            string
	SourceHash       string
	PlanFingerprint  string
	ParamVersion     string
	AlgorithmVersion string
	Width            int
	Height           int
	BitDepth         int
	Layout           string
	ChunkWidth       int
	ChunkHeight      int
}

func (m RunMeta) equal(other RunMeta) bool {
	return m.RunID == other.RunID &&
		m.SourceHash == other.SourceHash &&
		m.PlanFingerprint == other.PlanFingerprint &&
		m.ParamVersion == other.ParamVersion &&
		m.AlgorithmVersion == other.AlgorithmVersion &&
		m.Width == other.Width &&
		m.Height == other.Height &&
		m.BitDepth == other.BitDepth &&
		m.Layout == other.Layout &&
		m.ChunkWidth == other.ChunkWidth &&
		m.ChunkHeight == other.ChunkHeight
}

type StageOutput struct {
	EntryIndex        int            `json:"entry_index"`
	CacheKey          string         `json:"cache_key"`
	InputHash         string         `json:"input_hash"`
	OutputHash        string         `json:"output_hash"`
	PlanFingerprint   string         `json:"plan_fingerprint"`
	ParamVersion      string         `json:"param_version"`
	AlgorithmVersion  string         `json:"algorithm_version"`
	ParameterSnapshot string         `json:"parameter_snapshot"`
	Image             *texture.Image `json:"image"`
}

type ChunkCommit struct {
	ChunkX    int           `json:"chunk_x"`
	ChunkY    int           `json:"chunk_y"`
	CommitSeq int64         `json:"commit_seq"`
	Stages    []StageOutput `json:"stages"`
}

type Prepared struct {
	Existing bool
	Wiped    bool
}

type Store struct {
	db       *sql.DB
	path     string
	commitMu sync.Mutex
}

func Open(path string) (*Store, error) {
	dsn := sqliteDSN(path)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	dsn += sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, storeError("open", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, storeError("ping", err)
	}
	s := &Store{db: db, path: path}
	if err := s.createSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) createSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS runs (
			run_id TEXT PRIMARY KEY,
			source_hash TEXT NOT NULL,
			plan_fingerprint TEXT NOT NULL,
			param_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			bit_depth INTEGER NOT NULL,
			layout TEXT NOT NULL,
			chunk_width INTEGER NOT NULL,
			chunk_height INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS chunks (
			run_id TEXT NOT NULL,
			chunk_x INTEGER NOT NULL,
			chunk_y INTEGER NOT NULL,
			commit_seq INTEGER NOT NULL,
			PRIMARY KEY (run_id, chunk_x, chunk_y),
			UNIQUE (run_id, commit_seq)
		)`,
		`CREATE TABLE IF NOT EXISTS stage_outputs (
			run_id TEXT NOT NULL,
			chunk_x INTEGER NOT NULL,
			chunk_y INTEGER NOT NULL,
			entry_index INTEGER NOT NULL,
			cache_key TEXT NOT NULL,
			input_hash TEXT NOT NULL,
			output_hash TEXT NOT NULL,
			plan_fingerprint TEXT NOT NULL,
			param_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			parameter_snapshot TEXT NOT NULL,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			bit_depth INTEGER NOT NULL,
			layout TEXT NOT NULL,
			pixels BLOB NOT NULL,
			PRIMARY KEY (run_id, chunk_x, chunk_y, entry_index)
		)`,
		`CREATE INDEX IF NOT EXISTS stage_outputs_lookup
			ON stage_outputs (run_id, entry_index, cache_key)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return storeError("schema", err)
		}
	}
	return nil
}

// PrepareRun serializes run initialization. If any global version/hash field
// differs, the entire old snapshot is deleted before the new run is created.
func (s *Store) PrepareRun(ctx context.Context, meta RunMeta) (Prepared, error) {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Prepared{}, storeError("prepare begin", err)
	}
	defer tx.Rollback()
	existing, found, err := readRunMetaTx(ctx, tx, meta.RunID)
	if err != nil {
		return Prepared{}, err
	}
	if found {
		if existing.equal(meta) {
			if err := tx.Commit(); err != nil {
				return Prepared{}, storeError("prepare commit", err)
			}
			return Prepared{Existing: true}, nil
		}
		if err := deleteRunTx(ctx, tx, meta.RunID); err != nil {
			return Prepared{}, err
		}
		if err := insertRunTx(ctx, tx, meta); err != nil {
			return Prepared{}, err
		}
		if err := tx.Commit(); err != nil {
			return Prepared{}, storeError("prepare mismatch commit", err)
		}
		return Prepared{Wiped: true}, nil
	}
	if err := insertRunTx(ctx, tx, meta); err != nil {
		return Prepared{}, err
	}
	if err := tx.Commit(); err != nil {
		return Prepared{}, storeError("prepare insert commit", err)
	}
	return Prepared{}, nil
}

func (s *Store) WipeRun(ctx context.Context, runID string) error {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storeError("wipe begin", err)
	}
	defer tx.Rollback()
	if err := deleteRunTx(ctx, tx, runID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return storeError("wipe commit", err)
	}
	return nil
}

// CommitChunk writes one tile for every stage in one serialized transaction.
// The commit sequence is stable and supplied by the deterministic coordinator.
func (s *Store) CommitChunk(ctx context.Context, meta RunMeta, commit ChunkCommit) error {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	stages := append([]StageOutput(nil), commit.Stages...)
	sort.Slice(stages, func(i, j int) bool { return stages[i].EntryIndex < stages[j].EntryIndex })
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storeError("commit begin", err)
	}
	defer tx.Rollback()
	storedMeta, found, err := readRunMetaTx(ctx, tx, meta.RunID)
	if err != nil {
		return err
	}
	if !found {
		return storeError("commit", fmt.Errorf("run %q is not prepared", meta.RunID))
	}
	if !storedMeta.equal(meta) {
		return storeError("commit", fmt.Errorf("run metadata mismatch for %q", meta.RunID))
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO chunks (run_id, chunk_x, chunk_y, commit_seq) VALUES (?, ?, ?, ?)`,
		meta.RunID, commit.ChunkX, commit.ChunkY, commit.CommitSeq); err != nil {
		return storeError("insert chunk", err)
	}
	for _, stage := range stages {
		if stage.Image == nil {
			return storeError("insert stage", fmt.Errorf("entry %d has nil image", stage.EntryIndex))
		}
		if err := stage.Image.Validate(); err != nil {
			return err
		}
		if stage.OutputHash != "" && stage.OutputHash != stage.Image.Hash() {
			return storeError("insert stage", fmt.Errorf("entry %d output hash mismatch", stage.EntryIndex))
		}
		pixels := texture.EncodeSamples(stage.Image.Pix)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stage_outputs
			 (run_id, chunk_x, chunk_y, entry_index, cache_key, input_hash, output_hash,
			  plan_fingerprint, param_version, algorithm_version, parameter_snapshot,
			  width, height, bit_depth, layout, pixels)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			meta.RunID, commit.ChunkX, commit.ChunkY, stage.EntryIndex,
			stage.CacheKey, stage.InputHash, stage.OutputHash,
			stage.PlanFingerprint, stage.ParamVersion, stage.AlgorithmVersion,
			stage.ParameterSnapshot,
			stage.Image.Width, stage.Image.Height, stage.Image.BitDepth,
			texture.LayoutString(stage.Image.Layout), pixels); err != nil {
			return storeError("insert stage", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return storeError("commit transaction", err)
	}
	return nil
}

func (s *Store) LoadChunks(ctx context.Context, meta RunMeta) ([]ChunkCommit, error) {
	return s.loadChunks(ctx, meta, true)
}

// LoadChunksUnchecked decodes persisted outputs without rejecting a stale
// output_hash. The independent verifier uses this to recompose pixels and
// report the first violation coordinate instead of failing before inspection.
func (s *Store) LoadChunksUnchecked(ctx context.Context, meta RunMeta) ([]ChunkCommit, error) {
	return s.loadChunks(ctx, meta, false)
}

func (s *Store) loadChunks(ctx context.Context, meta RunMeta, validateHash bool) ([]ChunkCommit, error) {
	stored, found, err := s.ReadRunMeta(ctx, meta.RunID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, storeError("load", fmt.Errorf("run %q not found", meta.RunID))
	}
	if !stored.equal(meta) {
		return nil, storeError("load", fmt.Errorf("run metadata mismatch for %q", meta.RunID))
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT chunk_x, chunk_y, commit_seq FROM chunks WHERE run_id = ? ORDER BY commit_seq`,
		meta.RunID)
	if err != nil {
		return nil, storeError("load chunks", err)
	}
	defer rows.Close()
	var out []ChunkCommit
	for rows.Next() {
		var c ChunkCommit
		if err := rows.Scan(&c.ChunkX, &c.ChunkY, &c.CommitSeq); err != nil {
			return nil, storeError("scan chunk", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, storeError("iterate chunks", err)
	}
	for i := range out {
		stages, err := s.loadStages(ctx, meta.RunID, out[i].ChunkX, out[i].ChunkY, validateHash)
		if err != nil {
			return nil, err
		}
		out[i].Stages = stages
	}
	return out, nil
}

func (s *Store) loadStages(ctx context.Context, runID string, chunkX, chunkY int, validateHash bool) ([]StageOutput, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT entry_index, cache_key, input_hash, output_hash, plan_fingerprint,
		        param_version, algorithm_version, parameter_snapshot,
		        width, height, bit_depth, layout, pixels
		   FROM stage_outputs
		  WHERE run_id = ? AND chunk_x = ? AND chunk_y = ?
		  ORDER BY entry_index`,
		runID, chunkX, chunkY)
	if err != nil {
		return nil, storeError("load stages", err)
	}
	defer rows.Close()
	var out []StageOutput
	for rows.Next() {
		var stage StageOutput
		var width, height, bitDepth int
		var layout string
		var pixels []byte
		if err := rows.Scan(
			&stage.EntryIndex, &stage.CacheKey, &stage.InputHash, &stage.OutputHash,
			&stage.PlanFingerprint, &stage.ParamVersion, &stage.AlgorithmVersion,
			&stage.ParameterSnapshot,
			&width, &height, &bitDepth, &layout, &pixels,
		); err != nil {
			return nil, storeError("scan stage", err)
		}
		layoutChannels, err := texture.ParseLayout(layout)
		if err != nil {
			return nil, storeError("decode layout", err)
		}
		im, err := texture.NewImage(width, height, bitDepth, layoutChannels)
		if err != nil {
			return nil, err
		}
		decoded, err := texture.DecodeSamples(pixels)
		if err != nil {
			return nil, err
		}
		if len(decoded) != len(im.Pix) {
			return nil, storeError("decode stage", fmt.Errorf("pixel count mismatch for entry %d", stage.EntryIndex))
		}
		copy(im.Pix, decoded)
		if validateHash && stage.OutputHash != im.Hash() {
			return nil, storeError("load stage", fmt.Errorf("content hash mismatch for entry %d chunk (%d,%d)", stage.EntryIndex, chunkX, chunkY))
		}
		stage.Image = im
		out = append(out, stage)
	}
	if err := rows.Err(); err != nil {
		return nil, storeError("iterate stages", err)
	}
	return out, nil
}

func (s *Store) ReadRunMeta(ctx context.Context, runID string) (RunMeta, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT run_id, source_hash, plan_fingerprint, param_version, algorithm_version,
		        width, height, bit_depth, layout, chunk_width, chunk_height
		   FROM runs WHERE run_id = ?`, runID)
	var m RunMeta
	err := row.Scan(
		&m.RunID, &m.SourceHash, &m.PlanFingerprint, &m.ParamVersion, &m.AlgorithmVersion,
		&m.Width, &m.Height, &m.BitDepth, &m.Layout, &m.ChunkWidth, &m.ChunkHeight,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return RunMeta{}, false, nil
	}
	if err != nil {
		return RunMeta{}, false, storeError("read run", err)
	}
	return m, true, nil
}

type CacheEntry struct {
	EntryIndex int
	CacheKey   string
	Stage      StageOutput
}

func (s *Store) LoadCacheIndex(ctx context.Context, runID string) (map[string]StageOutput, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT entry_index, cache_key, input_hash, output_hash, plan_fingerprint,
		        param_version, algorithm_version, parameter_snapshot,
		        width, height, bit_depth, layout, pixels, chunk_x, chunk_y
		   FROM stage_outputs WHERE run_id = ? ORDER BY entry_index, chunk_y, chunk_x`,
		runID)
	if err != nil {
		return nil, storeError("load cache", err)
	}
	defer rows.Close()
	out := make(map[string]StageOutput)
	for rows.Next() {
		var stage StageOutput
		var width, height, bitDepth int
		var layout string
		var pixels []byte
		var chunkX, chunkY int
		if err := rows.Scan(
			&stage.EntryIndex, &stage.CacheKey, &stage.InputHash, &stage.OutputHash,
			&stage.PlanFingerprint, &stage.ParamVersion, &stage.AlgorithmVersion,
			&stage.ParameterSnapshot,
			&width, &height, &bitDepth, &layout, &pixels, &chunkX, &chunkY,
		); err != nil {
			return nil, storeError("scan cache", err)
		}
		layoutChannels, err := texture.ParseLayout(layout)
		if err != nil {
			return nil, storeError("decode cache layout", err)
		}
		im, err := texture.NewImage(width, height, bitDepth, layoutChannels)
		if err != nil {
			return nil, err
		}
		decoded, err := texture.DecodeSamples(pixels)
		if err != nil {
			return nil, err
		}
		if len(decoded) != len(im.Pix) {
			return nil, storeError("decode cache", fmt.Errorf("pixel count mismatch"))
		}
		copy(im.Pix, decoded)
		if stage.OutputHash != im.Hash() {
			return nil, storeError("load cache", fmt.Errorf("content hash mismatch for entry %d chunk (%d,%d)", stage.EntryIndex, chunkX, chunkY))
		}
		stage.Image = im
		key := cacheIndexKey(stage.EntryIndex, stage.CacheKey)
		if _, exists := out[key]; exists {
			return nil, storeError("load cache", fmt.Errorf("duplicate cache key for entry %d", stage.EntryIndex))
		}
		out[key] = stage
	}
	if err := rows.Err(); err != nil {
		return nil, storeError("iterate cache", err)
	}
	return out, nil
}

func CacheLookupKey(entryIndex int, cacheKey string) string {
	return cacheIndexKey(entryIndex, cacheKey)
}

func cacheIndexKey(entryIndex int, cacheKey string) string {
	return fmt.Sprintf("%d:%s", entryIndex, cacheKey)
}

func (s *Store) ChunkCount(ctx context.Context, runID string) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks WHERE run_id = ?`, runID)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, storeError("chunk count", err)
	}
	return count, nil
}

func readRunMetaTx(ctx context.Context, tx *sql.Tx, runID string) (RunMeta, bool, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT run_id, source_hash, plan_fingerprint, param_version, algorithm_version,
		        width, height, bit_depth, layout, chunk_width, chunk_height
		   FROM runs WHERE run_id = ?`, runID)
	var m RunMeta
	err := row.Scan(
		&m.RunID, &m.SourceHash, &m.PlanFingerprint, &m.ParamVersion, &m.AlgorithmVersion,
		&m.Width, &m.Height, &m.BitDepth, &m.Layout, &m.ChunkWidth, &m.ChunkHeight,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return RunMeta{}, false, nil
	}
	if err != nil {
		return RunMeta{}, false, storeError("read run tx", err)
	}
	return m, true, nil
}

func insertRunTx(ctx context.Context, tx *sql.Tx, meta RunMeta) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO runs
		 (run_id, source_hash, plan_fingerprint, param_version, algorithm_version,
		  width, height, bit_depth, layout, chunk_width, chunk_height)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		meta.RunID, meta.SourceHash, meta.PlanFingerprint, meta.ParamVersion, meta.AlgorithmVersion,
		meta.Width, meta.Height, meta.BitDepth, meta.Layout, meta.ChunkWidth, meta.ChunkHeight)
	if err != nil {
		return storeError("insert run", err)
	}
	return nil
}

func deleteRunTx(ctx context.Context, tx *sql.Tx, runID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM stage_outputs WHERE run_id = ?`, runID); err != nil {
		return storeError("delete stages", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE run_id = ?`, runID); err != nil {
		return storeError("delete chunks", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE run_id = ?`, runID); err != nil {
		return storeError("delete run", err)
	}
	return nil
}

func storeError(op string, err error) error {
	var ke *texture.KernelError
	if errors.As(err, &ke) {
		return err
	}
	return &texture.KernelError{Code: texture.CodeStore, Path: "$.store", Message: op, Err: err}
}

// DSNForPath is exported for diagnostics only.
func DSNForPath(path string) string {
	return sqliteDSN(path)
}

func sqliteDSN(path string) string {
	if strings.HasPrefix(path, "file:") {
		return path
	}
	slashed := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" {
		return "file:///" + slashed
	}
	return "file:" + slashed
}
