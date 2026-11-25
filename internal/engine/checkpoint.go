package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"maskpreview/internal/core"
	"maskpreview/internal/segment"
)

type StageCommit struct {
	SnapshotVersion  string
	CanonicalIndex   int
	BlockID          int
	Stage            Stage
	Summary          string
	ContentHash      string
	ParamVersion     string
	AlgorithmVersion string
	Payload          []byte
}

type StageRecord struct {
	DBSeq            int64  `json:"db_seq"`
	CanonicalIndex   int    `json:"canonical_index"`
	BlockID          int    `json:"block_id"`
	Stage            Stage  `json:"stage"`
	Summary          string `json:"summary"`
	ContentHash      string `json:"content_hash"`
	ParamVersion     string `json:"param_version"`
	AlgorithmVersion string `json:"algorithm_version"`
}

type SnapshotInfo struct {
	Version          string `json:"version"`
	ImageID          string `json:"image_id"`
	ImageHash        string `json:"image_hash"`
	TruthFingerprint string `json:"truth_fingerprint"`
	AlgorithmVersion string `json:"algorithm_version"`
	ParamVersion     string `json:"param_version"`
	Status           string `json:"status"`
	Reason           string `json:"reason"`
	CommittedStages  int    `json:"committed_stages"`
	TotalStages      int    `json:"total_stages"`
	BlockSize        int    `json:"block_size"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
}

type Checkpoint struct {
	Info      SnapshotInfo  `json:"info"`
	Records   []StageRecord `json:"records"`
	Artifacts []Artifact    `json:"-"`
	GrowMask  []uint8       `json:"-"`
	CleanMask []uint8       `json:"-"`
	Blocks    []Block       `json:"blocks"`
	Progress  Progress      `json:"progress"`
}

type Store struct {
	mu sync.Mutex
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, core.ParamError("checkpoint.open", "path", path, 1, "non-empty path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create checkpoint directory: %w", err)
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) initSchema() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS snapshots (
			version TEXT PRIMARY KEY,
			image_id TEXT NOT NULL,
			image_hash TEXT NOT NULL,
			truth_fingerprint TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			param_version TEXT NOT NULL,
			status TEXT NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			committed_stages INTEGER NOT NULL DEFAULT 0,
			total_stages INTEGER NOT NULL,
			block_size INTEGER NOT NULL,
			image_width INTEGER NOT NULL,
			image_height INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS stages (
			db_seq INTEGER PRIMARY KEY AUTOINCREMENT,
			snapshot_version TEXT NOT NULL,
			canonical_index INTEGER NOT NULL,
			block_id INTEGER NOT NULL,
			stage TEXT NOT NULL,
			summary TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			param_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			payload BLOB NOT NULL,
			UNIQUE(snapshot_version, canonical_index)
		)`,
		`CREATE INDEX IF NOT EXISTS stages_snapshot_order ON stages(snapshot_version, canonical_index)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`INSERT OR IGNORE INTO checkpoint_meta(key, value) VALUES ('schema_version', '` + core.CheckpointVersion + `')`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("initialize checkpoint schema: %w", err)
		}
	}
	return nil
}

func (s *Store) BeginSnapshot(snap core.Snapshot, truthFingerprint string, blocks []Block, blockSize, width, height int) error {
	if err := snap.Validate(); err != nil {
		return err
	}
	total := len(blocks) * len(StageOrder)
	if total == 0 {
		return core.ParamError("checkpoint.begin", "blocks", 0, 1, "unbounded")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin snapshot transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM stages WHERE snapshot_version = ?`, snap.Version); err != nil {
		return fmt.Errorf("clear old stages: %w", err)
	}
	_, err = tx.Exec(`
		INSERT INTO snapshots (
			version, image_id, image_hash, truth_fingerprint, algorithm_version,
			param_version, status, reason, committed_stages, total_stages,
			block_size, image_width, image_height
		) VALUES (?, ?, ?, ?, ?, ?, 'building', '', 0, ?, ?, ?, ?)
		ON CONFLICT(version) DO UPDATE SET
			image_id=excluded.image_id,
			image_hash=excluded.image_hash,
			truth_fingerprint=excluded.truth_fingerprint,
			algorithm_version=excluded.algorithm_version,
			param_version=excluded.param_version,
			status='building',
			reason='',
			committed_stages=0,
			total_stages=excluded.total_stages,
			block_size=excluded.block_size,
			image_width=excluded.image_width,
			image_height=excluded.image_height`,
		snap.Version, snap.ImageID, snap.ImageHash, truthFingerprint, snap.AlgorithmVersion,
		snap.ParamVersion, total, blockSize, width, height,
	)
	if err != nil {
		return fmt.Errorf("insert snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit snapshot transaction: %w", err)
	}
	return nil
}

func (s *Store) CommitStage(c StageCommit) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin stage transaction: %w", err)
	}
	defer tx.Rollback()
	var status string
	var committed, total int
	var paramVersion, algorithmVersion string
	err = tx.QueryRow(`SELECT status, committed_stages, total_stages, param_version, algorithm_version FROM snapshots WHERE version = ?`, c.SnapshotVersion).
		Scan(&status, &committed, &total, &paramVersion, &algorithmVersion)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, core.NewError(core.CodeResume, "checkpoint.commit", "snapshot not initialized")
		}
		return 0, fmt.Errorf("read snapshot: %w", err)
	}
	if status != "building" {
		return 0, &core.Error{Code: core.CodeState, Op: "checkpoint.commit", Field: "snapshot_status", Got: status, Want: "building"}
	}
	if c.ParamVersion != paramVersion {
		return 0, core.VersionError("checkpoint.commit.param", paramVersion, c.ParamVersion)
	}
	if c.AlgorithmVersion != algorithmVersion {
		return 0, &core.Error{Code: core.CodeVersion, Op: "checkpoint.commit.algorithm", Left: algorithmVersion, Right: c.AlgorithmVersion}
	}
	if c.CanonicalIndex != committed {
		return 0, &core.Error{
			Code:    core.CodeState,
			Op:      "checkpoint.commit",
			Field:   "canonical_index",
			Got:     c.CanonicalIndex,
			Want:    committed,
			Message: "stage commit arrived out of canonical order",
		}
	}
	res, err := tx.Exec(`
		INSERT INTO stages (
			snapshot_version, canonical_index, block_id, stage, summary,
			content_hash, param_version, algorithm_version, payload
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.SnapshotVersion, c.CanonicalIndex, c.BlockID, string(c.Stage), c.Summary,
		c.ContentHash, c.ParamVersion, c.AlgorithmVersion, c.Payload,
	)
	if err != nil {
		return 0, fmt.Errorf("insert stage: %w", err)
	}
	dbSeq, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read stage sequence: %w", err)
	}
	next := committed + 1
	nextStatus := "building"
	if next == total {
		nextStatus = "complete"
	}
	if _, err := tx.Exec(`UPDATE snapshots SET committed_stages = ?, status = ? WHERE version = ?`, next, nextStatus, c.SnapshotVersion); err != nil {
		return 0, fmt.Errorf("update snapshot progress: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit stage transaction: %w", err)
	}
	return dbSeq, nil
}

func (s *Store) LoadCheckpoint(snap core.Snapshot, truthFingerprint string) (*Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := snap.Validate(); err != nil {
		return nil, err
	}
	info, err := s.snapshotInfoLocked(snap.Version)
	if err != nil {
		if core.CodeOf(err) == core.CodeResume {
			return nil, err
		}
		return nil, err
	}
	fail := func(reason string) (*Checkpoint, error) {
		_ = s.invalidateLocked(snap.Version, reason)
		return nil, &core.Error{
			Code:    core.CodeResume,
			Op:      "checkpoint.load",
			Field:   "snapshot",
			Got:     reason,
			Want:    "complete, hash-valid, version-matched checkpoint",
			Message: "checkpoint invalidated; full recomputation required",
			Data:    map[string]any{"snapshot_version": snap.Version, "reason": reason},
		}
	}
	var schemaVersion string
	if err := s.db.QueryRow(`SELECT value FROM checkpoint_meta WHERE key = 'schema_version'`).Scan(&schemaVersion); err != nil {
		return fail(fmt.Sprintf("checkpoint schema version unavailable: %v", err))
	}
	if schemaVersion != core.CheckpointVersion {
		return fail(fmt.Sprintf("checkpoint schema version mismatch: db=%s expected=%s", schemaVersion, core.CheckpointVersion))
	}
	if info.ImageID != snap.ImageID || info.ImageHash != snap.ImageHash {
		return fail(fmt.Sprintf("image identity mismatch: db=(%s,%s) expected=(%s,%s)", info.ImageID, info.ImageHash, snap.ImageID, snap.ImageHash))
	}
	if info.ParamVersion != snap.ParamVersion {
		return fail(fmt.Sprintf("snapshot param version mismatch: db=%s expected=%s", info.ParamVersion, snap.ParamVersion))
	}
	if info.TruthFingerprint != truthFingerprint {
		return fail(fmt.Sprintf("truth fingerprint mismatch: db=%s request=%s", info.TruthFingerprint, truthFingerprint))
	}
	if info.AlgorithmVersion != core.AlgorithmVersion {
		return fail(fmt.Sprintf("algorithm version mismatch: db=%s expected=%s", info.AlgorithmVersion, core.AlgorithmVersion))
	}
	if info.Status != "complete" {
		return fail(fmt.Sprintf("snapshot status is %q, not complete", info.Status))
	}
	if info.CommittedStages != info.TotalStages {
		return fail(fmt.Sprintf("committed stages %d != total %d", info.CommittedStages, info.TotalStages))
	}
	rows, err := s.db.Query(`
		SELECT db_seq, canonical_index, block_id, stage, summary, content_hash,
		       param_version, algorithm_version, payload
		FROM stages WHERE snapshot_version = ? ORDER BY canonical_index`, snap.Version)
	if err != nil {
		return nil, fmt.Errorf("query stages: %w", err)
	}
	type loadedRow struct {
		rec     StageRecord
		payload []byte
	}
	var loaded []loadedRow
	for rows.Next() {
		var rec StageRecord
		var stage, payloadRaw []byte
		if err := rows.Scan(&rec.DBSeq, &rec.CanonicalIndex, &rec.BlockID, &stage, &rec.Summary,
			&rec.ContentHash, &rec.ParamVersion, &rec.AlgorithmVersion, &payloadRaw); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan stage: %w", err)
		}
		rec.Stage = Stage(stage)
		loaded = append(loaded, loadedRow{rec: rec, payload: payloadRaw})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate stages: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close stage rows: %w", err)
	}
	records := make([]StageRecord, 0, len(loaded))
	artifacts := make([]Artifact, 0, len(loaded))
	for _, row := range loaded {
		rec, payloadRaw := row.rec, row.payload
		records = append(records, rec)
		actual := core.HashBytes(payloadRaw)
		if actual != rec.ContentHash {
			return fail(fmt.Sprintf("content hash mismatch at canonical index %d: stored=%s actual=%s", rec.CanonicalIndex, rec.ContentHash, actual))
		}
		if rec.ParamVersion != info.ParamVersion {
			return fail(fmt.Sprintf("param version mismatch at canonical index %d: stored=%s snapshot=%s", rec.CanonicalIndex, rec.ParamVersion, info.ParamVersion))
		}
		if rec.AlgorithmVersion != info.AlgorithmVersion {
			return fail(fmt.Sprintf("algorithm version mismatch at canonical index %d: stored=%s snapshot=%s", rec.CanonicalIndex, rec.AlgorithmVersion, info.AlgorithmVersion))
		}
		artifact, err := DecodeArtifact(payloadRaw)
		if err != nil {
			return fail(fmt.Sprintf("artifact decode failed at canonical index %d: %v", rec.CanonicalIndex, err))
		}
		if artifact.Stage != rec.Stage || artifact.Block.ID != rec.BlockID {
			return fail(fmt.Sprintf("artifact metadata mismatch at canonical index %d", rec.CanonicalIndex))
		}
		artifacts = append(artifacts, artifact)
	}
	blocks, err := SplitBlocks(info.Width, info.Height, info.BlockSize)
	if err != nil {
		return fail(err.Error())
	}
	expectedStages := len(blocks) * len(StageOrder)
	if len(records) != expectedStages || info.TotalStages != expectedStages {
		return fail(fmt.Sprintf("stage count mismatch: records=%d expected=%d", len(records), expectedStages))
	}
	growMask := make([]uint8, info.Width*info.Height)
	cleanMask := make([]uint8, info.Width*info.Height)
	growCoverage := make([]uint8, len(growMask))
	cleanCoverage := make([]uint8, len(cleanMask))
	for i := range blocks {
		for stageIdx, stage := range StageOrder {
			canonical := i*len(StageOrder) + stageIdx
			if records[canonical].CanonicalIndex != canonical {
				return fail(fmt.Sprintf("non-contiguous canonical index: got=%d want=%d", records[canonical].CanonicalIndex, canonical))
			}
			if records[canonical].Stage != stage {
				return fail(fmt.Sprintf("stage order mismatch at canonical index %d: got=%s want=%s", canonical, records[canonical].Stage, stage))
			}
			block := blocks[i]
			artifact := artifacts[canonical]
			if artifact.Block.ID != block.ID || artifact.Block.X != block.X || artifact.Block.Y != block.Y ||
				artifact.Block.Width != block.Width || artifact.Block.Height != block.Height {
				return fail(fmt.Sprintf("block geometry mismatch at canonical index %d", canonical))
			}
			switch stage {
			case StageGrow:
				if err := placeMaskChecked(growMask, growCoverage, info.Width, block, artifact.Mask); err != nil {
					return fail(err.Error())
				}
			case StageClean:
				if err := placeMaskChecked(cleanMask, cleanCoverage, info.Width, block, artifact.Mask); err != nil {
					return fail(err.Error())
				}
			case StageStats:
				// Stats payload is persisted for the independent verifier and
				// consistency checks; the mask itself comes from clean stages.
			default:
				return fail(fmt.Sprintf("unknown stage %q", stage))
			}
		}
	}
	for i, c := range growCoverage {
		if c != 1 || cleanCoverage[i] != 1 {
			return fail(fmt.Sprintf("incomplete/overlapping block coverage at pixel %d", i))
		}
	}
	return &Checkpoint{
		Info:      info,
		Records:   records,
		Artifacts: artifacts,
		GrowMask:  growMask,
		CleanMask: cleanMask,
		Blocks:    blocks,
		Progress:  Progress{Committed: info.CommittedStages, Total: info.TotalStages},
	}, nil
}

func (s *Store) StageRecords(version string) ([]StageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
		SELECT db_seq, canonical_index, block_id, stage, summary, content_hash,
		       param_version, algorithm_version
		FROM stages WHERE snapshot_version = ? ORDER BY canonical_index`, version)
	if err != nil {
		return nil, fmt.Errorf("query stage records: %w", err)
	}
	defer rows.Close()
	var out []StageRecord
	for rows.Next() {
		var rec StageRecord
		var stage string
		if err := rows.Scan(&rec.DBSeq, &rec.CanonicalIndex, &rec.BlockID, &stage, &rec.Summary,
			&rec.ContentHash, &rec.ParamVersion, &rec.AlgorithmVersion); err != nil {
			return nil, fmt.Errorf("scan stage record: %w", err)
		}
		rec.Stage = Stage(stage)
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) SnapshotInfo(version string) (SnapshotInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotInfoLocked(version)
}

func (s *Store) snapshotInfoLocked(version string) (SnapshotInfo, error) {
	var info SnapshotInfo
	err := s.db.QueryRow(`
		SELECT version, image_id, image_hash, truth_fingerprint, algorithm_version,
		       param_version, status, reason, committed_stages, total_stages,
		       block_size, image_width, image_height
		FROM snapshots WHERE version = ?`, version).
		Scan(&info.Version, &info.ImageID, &info.ImageHash, &info.TruthFingerprint,
			&info.AlgorithmVersion, &info.ParamVersion, &info.Status, &info.Reason,
			&info.CommittedStages, &info.TotalStages, &info.BlockSize, &info.Width, &info.Height)
	if err == sql.ErrNoRows {
		return SnapshotInfo{}, core.NewError(core.CodeResume, "checkpoint.snapshot", "snapshot not found")
	}
	if err != nil {
		return SnapshotInfo{}, fmt.Errorf("read snapshot info: %w", err)
	}
	return info, nil
}

func (s *Store) invalidateLocked(version, reason string) error {
	_, err := s.db.Exec(`UPDATE snapshots SET status = 'invalid', reason = ? WHERE version = ?`, reason, version)
	if err != nil {
		return fmt.Errorf("invalidate snapshot: %w", err)
	}
	return nil
}

func placeMaskChecked(dst, coverage []uint8, imageWidth int, block Block, mask []uint8) error {
	if len(mask) != block.Width*block.Height {
		return fmt.Errorf("block %d mask length %d, want %d", block.ID, len(mask), block.Width*block.Height)
	}
	for y := 0; y < block.Height; y++ {
		for x := 0; x < block.Width; x++ {
			idx := (block.Y+y)*imageWidth + block.X + x
			if coverage[idx] != 0 {
				return fmt.Errorf("block %d overlaps committed block at (%d,%d)", block.ID, block.X+x, block.Y+y)
			}
			coverage[idx] = 1
			dst[idx] = mask[y*block.Width+x]
		}
	}
	return nil
}

func Sequence(records []StageRecord) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, fmt.Sprintf("%d:%s:%s", r.BlockID, r.Stage, r.ContentHash))
	}
	return out
}

func SequenceEqual(a, b []StageRecord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].BlockID != b[i].BlockID || a[i].Stage != b[i].Stage || a[i].ContentHash != b[i].ContentHash ||
			a[i].ParamVersion != b[i].ParamVersion || a[i].AlgorithmVersion != b[i].AlgorithmVersion {
			return false
		}
	}
	return true
}

func ReassembleFromArtifacts(artifacts []Artifact, blocks []Block, width, height int, stage Stage) ([]uint8, error) {
	out := make([]uint8, width*height)
	coverage := make([]uint8, width*height)
	for _, artifact := range artifacts {
		if artifact.Stage != stage {
			continue
		}
		if err := placeMaskChecked(out, coverage, width, artifact.Block, artifact.Mask); err != nil {
			return nil, err
		}
	}
	for i, c := range coverage {
		if c != 1 {
			return nil, fmt.Errorf("artifact coverage incomplete at pixel %d", i)
		}
	}
	return out, nil
}

func AnalyzeCleanMask(mask []uint8, width, height int) ([]segment.RegionStat, []int) {
	return segment.Analyze(mask, width, height)
}

func FormatSnapshots(infos []SnapshotInfo) string {
	var b strings.Builder
	for _, info := range infos {
		fmt.Fprintf(&b, "%s status=%s progress=%d/%d\n", info.Version, info.Status, info.CommittedStages, info.TotalStages)
	}
	return b.String()
}

func CheckContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
