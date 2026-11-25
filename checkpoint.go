package fogkernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrCrashInjected = errors.New("crash injection: process terminated at a durable commit point")

type CheckpointStore struct {
	DB           *sql.DB
	Path         string
	SceneVersion string
}

type RecoveryState struct {
	Path              string        `json:"path"`
	SceneVersion      string        `json:"scene_version"`
	CommittedRayCount int           `json:"committed_ray_count"`
	CommittedRays     []RayResult   `json:"-"`
	PartialRayIndex   int           `json:"partial_ray_index"`
	PartialBlocks     []BlockResult `json:"partial_blocks"`
	HasPartialRay     bool          `json:"has_partial_ray"`
}

type CheckpointRunOptions struct {
	Budget                 Budget
	CrashAfterBlockCommits int
	StopAfterCompleteRays  int
	BlockDelay             time.Duration
	OnBlockCommitted       func(commits int, rayIndex int, blockIndex int) error
}

type CheckpointRunResult struct {
	Report             *SolveReport   `json:"report"`
	Recovery           *RecoveryState `json:"recovery"`
	StopReason         string         `json:"stop_reason"`
	BlockCommits       int            `json:"block_commits"`
	CompleteRayCommits int            `json:"complete_ray_commits"`
	ResumeHint         string         `json:"resume_hint"`
	CrashInjected      bool           `json:"crash_injected,omitempty"`
}

func sqliteDSN(path string) string {
	if path == ":memory:" {
		return ":memory:?_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"
	}
	return path + "?_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_pragma=journal_mode(DELETE)"
}

func isCorruptSQLError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "malformed") ||
		strings.Contains(msg, "not a database") ||
		strings.Contains(msg, "truncated") ||
		strings.Contains(msg, "disk image") ||
		strings.Contains(msg, "database disk image") ||
		strings.Contains(msg, "file is not a database")
}

func mapOpenError(path string, err error) error {
	if isCorruptSQLError(err) {
		return NewBizError(CodeCheckpointTrunc, path, "checkpoint database is truncated or corrupt: %v", err)
	}
	return NewBizError(CodeCheckpointOpen, path, "cannot open SQLite checkpoint: %v", err)
}

func OpenCheckpointStore(path, sceneVersion string) (*CheckpointStore, *RecoveryState, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, nil, mapOpenError(path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, nil, mapOpenError(path, err)
	}
	store := &CheckpointStore{DB: db, Path: path, SceneVersion: sceneVersion}
	if err := store.initSchema(); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	state, err := store.loadState()
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	if state.SceneVersion != "" && state.SceneVersion != sceneVersion {
		_ = db.Close()
		biz := NewBizError(CodeCheckpointVersion, path, "checkpoint scene version %q does not match current %q", state.SceneVersion, sceneVersion).
			WithContext("checkpoint_version", state.SceneVersion, "current_version", sceneVersion,
				"recovery_policy", "discard_partial_checkpoint_and_recompute_all_rays")
		return nil, state, biz
	}
	if state.SceneVersion == "" {
		if _, err := db.Exec(`UPDATE commit_state SET scene_version=?, committed_ray_count=0, updated_ns=? WHERE id=1`, sceneVersion, time.Now().UnixNano()); err != nil {
			_ = db.Close()
			return nil, nil, NewBizError(CodeCheckpointWrite, path, "cannot initialize version: %v", err)
		}
		state.SceneVersion = sceneVersion
	}
	recovery, err := store.loadRecovery(state.CommittedRayCount)
	if err != nil {
		_ = db.Close()
		return nil, state, err
	}
	recovery.SceneVersion = sceneVersion
	return store, recovery, nil
}

func CreateFreshCheckpoint(path, sceneVersion string) (*CheckpointStore, error) {
	if path != ":memory:" {
		_ = os.Remove(path)
		_ = os.Remove(path + "-wal")
		_ = os.Remove(path + "-shm")
	}
	store, _, err := OpenCheckpointStore(path, sceneVersion)
	return store, err
}

func InspectCheckpoint(path, wantedVersion string) (*RecoveryState, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, mapOpenError(path, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := db.Ping(); err != nil {
		return nil, mapOpenError(path, err)
	}
	var version string
	var count int
	err = db.QueryRow(`SELECT scene_version, committed_ray_count FROM commit_state WHERE id=1`).Scan(&version, &count)
	if err != nil {
		if isCorruptSQLError(err) {
			return nil, NewBizError(CodeCheckpointTrunc, path, "checkpoint header/commit state is truncated or corrupt: %v", err)
		}
		return nil, NewBizError(CodeCheckpointTrunc, path, "checkpoint commit state is missing or unreadable: %v", err)
	}
	state := &RecoveryState{Path: path, SceneVersion: version, CommittedRayCount: count}
	if version != wantedVersion {
		return state, NewBizError(CodeCheckpointVersion, path, "checkpoint scene version %q does not match current %q", version, wantedVersion).
			WithContext("checkpoint_version", version, "current_version", wantedVersion)
	}
	return state, nil
}

func (s *CheckpointStore) initSchema() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commit_state(
			id INTEGER PRIMARY KEY CHECK(id=1),
			scene_version TEXT NOT NULL,
			committed_ray_count INTEGER NOT NULL,
			updated_ns INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS rays(
			ray_index INTEGER PRIMARY KEY,
			ray_id TEXT NOT NULL,
			status TEXT NOT NULL,
			completed_blocks INTEGER NOT NULL DEFAULT 0,
			hit INTEGER NOT NULL DEFAULT 0,
			transmittance REAL NOT NULL DEFAULT 1,
			terminal_block INTEGER NOT NULL DEFAULT -1,
			stop_reason TEXT NOT NULL DEFAULT '',
			duration_ns INTEGER NOT NULL DEFAULT 0,
			result_json TEXT NOT NULL DEFAULT '{}',
			updated_ns INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS blocks(
			ray_index INTEGER NOT NULL,
			block_index INTEGER NOT NULL,
			chunk_bin INTEGER NOT NULL,
			status TEXT NOT NULL,
			tau REAL NOT NULL,
			t_in REAL NOT NULL,
			t_out REAL NOT NULL,
			energy REAL NOT NULL,
			block_json TEXT NOT NULL,
			updated_ns INTEGER NOT NULL,
			PRIMARY KEY(ray_index,block_index))`,
	}
	for _, stmt := range stmts {
		if _, err := s.DB.Exec(stmt); err != nil {
			return mapOpenError(s.Path, err)
		}
	}
	if _, err := s.DB.Exec(`INSERT OR IGNORE INTO commit_state(id,scene_version,committed_ray_count,updated_ns) VALUES(1,'',0,?)`, time.Now().UnixNano()); err != nil {
		return NewBizError(CodeCheckpointOpen, s.Path, "cannot initialize commit state: %v", err)
	}
	return nil
}

func (s *CheckpointStore) loadState() (*RecoveryState, error) {
	state := &RecoveryState{Path: s.Path}
	err := s.DB.QueryRow(`SELECT scene_version, committed_ray_count FROM commit_state WHERE id=1`).Scan(&state.SceneVersion, &state.CommittedRayCount)
	if err != nil {
		return nil, NewBizError(CodeCheckpointTrunc, s.Path, "cannot read global ray sequence: %v", err)
	}
	return state, nil
}

func (s *CheckpointStore) loadRecovery(committedCount int) (*RecoveryState, error) {
	state := &RecoveryState{Path: s.Path, CommittedRayCount: committedCount}
	rows, err := s.DB.Query(`SELECT ray_index, ray_id, result_json FROM rays WHERE ray_index < ? AND status='committed' ORDER BY ray_index`, committedCount)
	if err != nil {
		return nil, NewBizError(CodeCheckpointTrunc, s.Path, "cannot read committed rays: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var idx int
		var id, raw string
		if err := rows.Scan(&idx, &id, &raw); err != nil {
			return nil, NewBizError(CodeCheckpointTrunc, s.Path, "cannot scan committed ray: %v", err)
		}
		var rr RayResult
		if err := json.Unmarshal([]byte(raw), &rr); err != nil {
			return nil, NewBizError(CodeCheckpointTrunc, s.Path, "committed ray %d JSON is truncated: %v", idx, err)
		}
		state.CommittedRays = append(state.CommittedRays, rr)
	}
	if err := rows.Err(); err != nil {
		return nil, NewBizError(CodeCheckpointTrunc, s.Path, "committed ray cursor failed: %v", err)
	}

	partialIndex := committedCount
	var status string
	err = s.DB.QueryRow(`SELECT status FROM rays WHERE ray_index=?`, partialIndex).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return nil, NewBizError(CodeCheckpointTrunc, s.Path, "cannot read partial ray %d: %v", partialIndex, err)
	}
	state.PartialRayIndex = partialIndex
	state.HasPartialRay = true
	blockRows, err := s.DB.Query(`SELECT block_json FROM blocks WHERE ray_index=? ORDER BY block_index`, partialIndex)
	if err != nil {
		return nil, NewBizError(CodeCheckpointTrunc, s.Path, "cannot read partial blocks: %v", err)
	}
	defer blockRows.Close()
	expected := 0
	for blockRows.Next() {
		var raw string
		if err := blockRows.Scan(&raw); err != nil {
			return nil, NewBizError(CodeCheckpointTrunc, s.Path, "cannot scan partial block: %v", err)
		}
		var br BlockResult
		if err := json.Unmarshal([]byte(raw), &br); err != nil {
			return nil, NewBizError(CodeCheckpointTrunc, s.Path, "partial block JSON is truncated: %v", err)
		}
		if br.BlockIndex != expected {
			break
		}
		state.PartialBlocks = append(state.PartialBlocks, br)
		expected++
	}
	return state, nil
}

func (s *CheckpointStore) BeginRay(index int, rayID string) error {
	_, err := s.DB.Exec(`INSERT INTO rays(ray_index,ray_id,status,completed_blocks,updated_ns,result_json)
		VALUES(?,?,'in_progress',0,?,'{}')
		ON CONFLICT(ray_index) DO UPDATE SET ray_id=excluded.ray_id,status='in_progress',updated_ns=excluded.updated_ns`,
		index, rayID, time.Now().UnixNano())
	return bizOr(err, CodeCheckpointWrite, "$.checkpoint.rays")
}

func (s *CheckpointStore) CommitBlock(rayIndex int, block BlockResult) error {
	raw, err := json.Marshal(block)
	if err != nil {
		return NewBizError(CodeCheckpointWrite, "$.checkpoint.block_json", "%v", err)
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return NewBizError(CodeCheckpointWrite, "$.checkpoint.tx", "%v", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixNano()
	if _, err := tx.Exec(`INSERT INTO blocks(ray_index,block_index,chunk_bin,status,tau,t_in,t_out,energy,block_json,updated_ns)
		VALUES(?,?,?, 'committed', ?,?,?,?,?,?)
		ON CONFLICT(ray_index,block_index) DO UPDATE SET chunk_bin=excluded.chunk_bin,status='committed',
		tau=excluded.tau,t_in=excluded.t_in,t_out=excluded.t_out,energy=excluded.energy,
		block_json=excluded.block_json,updated_ns=excluded.updated_ns`,
		rayIndex, block.BlockIndex, block.ChunkBin, block.Tau, block.TIn, block.TOut, block.Energy, string(raw), now); err != nil {
		return NewBizError(CodeCheckpointWrite, "$.checkpoint.blocks", "%v", err)
	}
	if _, err := tx.Exec(`UPDATE rays SET status='in_progress', completed_blocks=?, updated_ns=? WHERE ray_index=?`,
		block.BlockIndex+1, now, rayIndex); err != nil {
		return NewBizError(CodeCheckpointWrite, "$.checkpoint.rays.progress", "%v", err)
	}
	if err := tx.Commit(); err != nil {
		return NewBizError(CodeCheckpointWrite, "$.checkpoint.commit_block", "%v", err)
	}
	return nil
}

func (s *CheckpointStore) CommitRay(result RayResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return NewBizError(CodeCheckpointWrite, "$.checkpoint.ray_json", "%v", err)
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return NewBizError(CodeCheckpointWrite, "$.checkpoint.tx", "%v", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixNano()
	for _, block := range result.Blocks {
		blockRaw, err := json.Marshal(block)
		if err != nil {
			return NewBizError(CodeCheckpointWrite, "$.checkpoint.block_json", "%v", err)
		}
		if _, err := tx.Exec(`INSERT INTO blocks(ray_index,block_index,chunk_bin,status,tau,t_in,t_out,energy,block_json,updated_ns)
			VALUES(?,?,?, 'committed', ?,?,?,?,?,?)
			ON CONFLICT(ray_index,block_index) DO UPDATE SET chunk_bin=excluded.chunk_bin,status='committed',
			tau=excluded.tau,t_in=excluded.t_in,t_out=excluded.t_out,energy=excluded.energy,
			block_json=excluded.block_json,updated_ns=excluded.updated_ns`,
			result.RayIndex, block.BlockIndex, block.ChunkBin, block.Tau, block.TIn, block.TOut, block.Energy, string(blockRaw), now); err != nil {
			return NewBizError(CodeCheckpointWrite, "$.checkpoint.blocks", "%v", err)
		}
	}
	hit := 0
	if result.Hit {
		hit = 1
	}
	if _, err := tx.Exec(`INSERT INTO rays(ray_index,ray_id,status,completed_blocks,hit,transmittance,terminal_block,stop_reason,duration_ns,result_json,updated_ns)
		VALUES(?,?, 'committed', ?,?,?,?,?,?,?,?)
		ON CONFLICT(ray_index) DO UPDATE SET ray_id=excluded.ray_id,status='committed',
		completed_blocks=excluded.completed_blocks,hit=excluded.hit,transmittance=excluded.transmittance,
		terminal_block=excluded.terminal_block,stop_reason=excluded.stop_reason,duration_ns=excluded.duration_ns,
		result_json=excluded.result_json,updated_ns=excluded.updated_ns`,
		result.RayIndex, result.RayID, len(result.Blocks), hit, result.Transmittance, result.TerminalBlock,
		result.StopReason, result.DurationNS, string(raw), now); err != nil {
		return NewBizError(CodeCheckpointWrite, "$.checkpoint.ray", "%v", err)
	}
	if err := tx.Commit(); err != nil {
		return NewBizError(CodeCheckpointWrite, "$.checkpoint.commit_ray", "%v", err)
	}
	return nil
}

// AdvanceGlobalRayIndex is deliberately the last write in the commit order.
// Recovery only treats ray indices below this high-water mark as complete.
func (s *CheckpointStore) AdvanceGlobalRayIndex(nextIndex int) error {
	_, err := s.DB.Exec(`UPDATE commit_state SET committed_ray_count=?, updated_ns=? WHERE id=1`, nextIndex, time.Now().UnixNano())
	return bizOr(err, CodeCheckpointWrite, "$.checkpoint.global_ray_index")
}

func (s *CheckpointStore) Close() error { return s.DB.Close() }

func RunCheckpointed(ctx context.Context, scene *Scene, store *CheckpointStore, recovery *RecoveryState, opts CheckpointRunOptions) (*CheckpointRunResult, error) {
	budget, err := opts.Budget.WithSceneDefaults(scene)
	if err != nil {
		return nil, err
	}
	_ = budget
	out := &CheckpointRunResult{Recovery: recovery}
	results := make([]RayResult, len(scene.Rays))
	startRay := 0
	var partial []BlockResult
	if recovery != nil {
		startRay = recovery.CommittedRayCount
		for _, rr := range recovery.CommittedRays {
			if rr.RayIndex >= 0 && rr.RayIndex < len(results) {
				results[rr.RayIndex] = rr
			}
		}
		if recovery.HasPartialRay && recovery.PartialRayIndex == startRay {
			partial = recovery.PartialBlocks
		}
	}

	for ri := startRay; ri < len(scene.Rays); ri++ {
		if opts.StopAfterCompleteRays > 0 && out.CompleteRayCommits >= opts.StopAfterCompleteRays {
			out.StopReason = StopSignal
			break
		}
		if ctx != nil {
			select {
			case <-ctx.Done():
				out.StopReason = StopSignal
				goto done
			default:
			}
		}
		ray := scene.Rays[ri]
		if err := store.BeginRay(ri, ray.ID); err != nil {
			return out, err
		}
		result, commits, err := solveRayCheckpoint(ctx, scene, ray, partial, store, opts, out)
		out.BlockCommits += commits
		if errors.Is(err, ErrCrashInjected) {
			out.CrashInjected = true
			return out, err
		}
		if errors.Is(err, context.Canceled) {
			results[ri] = result
			out.StopReason = StopSignal
			goto done
		}
		if err != nil {
			return out, err
		}
		partial = nil
		result.RayIndex = ri
		if err := store.CommitRay(result); err != nil {
			return out, err
		}
		if err := store.AdvanceGlobalRayIndex(ri + 1); err != nil {
			return out, err
		}
		out.CompleteRayCommits++
		results[ri] = result
	}
	if out.StopReason == "" && riAllCommitted(results) {
		out.StopReason = StopCompleted
	}

done:
	out.Report = &SolveReport{OK: true, Path: PathExact, SceneVersion: scene.Version(), Rays: results, Order: "checkpoint-sequential"}
	out.ResumeHint = "rerun the same command with --db " + store.Path + " --resume"
	return out, nil
}

func riAllCommitted(results []RayResult) bool {
	for i := range results {
		if results[i].RayID == "" {
			return false
		}
	}
	return true
}

func solveRayCheckpoint(ctx context.Context, scene *Scene, ray Ray, initial []BlockResult,
	store *CheckpointStore, opts CheckpointRunOptions, out *CheckpointRunResult) (RayResult, int, error) {
	plan := PlanRay(scene, ray)
	result := RayResult{RayIndex: ray.Index, RayID: ray.ID, CameraIndex: ray.CameraIndex, Path: PathExact, TerminalBlock: -1}
	commits := 0
	if !plan.Hit || len(plan.Blocks) == 0 {
		result.Hit = false
		result.Transmittance = 1
		result.StopReason = StopNoIntersection
		return result, 0, nil
	}
	result.Hit = true
	result.PlannedBlocks = len(plan.Blocks)
	t := 1.0
	startBlock := 0
	if len(initial) > 0 {
		for _, br := range initial {
			if br.BlockIndex != startBlock || startBlock >= len(plan.Blocks) {
				break
			}
			result.Blocks = append(result.Blocks, br)
			t = br.TOut
			result.Transmittance = t
			result.TerminalBlock = br.BlockIndex
			startBlock++
			if br.Terminal {
				if br.StopReason != "" {
					result.StopReason = br.StopReason
				} else {
					result.StopReason = StopThreshold
				}
				return result, 0, nil
			}
		}
	}
	threshold := scene.Recipe.TerminationThreshold()
	limit := scene.Recipe.BlockSampleLimit
	for bi := startBlock; bi < len(plan.Blocks); bi++ {
		if len(result.Blocks) >= limit {
			result.StopReason = StopBlockLimit
			break
		}
		if opts.BlockDelay > 0 {
			if ctx != nil {
				timer := time.NewTimer(opts.BlockDelay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return result, commits, context.Canceled
				case <-timer.C:
				}
			} else {
				time.Sleep(opts.BlockDelay)
			}
		}
		if ctx != nil {
			select {
			case <-ctx.Done():
				return result, commits, context.Canceled
			default:
			}
		}
		bp := plan.Blocks[bi]
		ph := ComputeBlockPhysics(scene, ray, bp)
		tIn := t
		br := BlockResult{
			BlockIndex: bi, ChunkBin: bp.Bin, T0: bp.T0, T1: bp.T1,
			TauA: ph.TauA, TauS: ph.TauS, Tau: ph.Tau, Albedo: ph.Albedo, Phase: ph.Phase,
			TBlock: expNeg(ph.Tau), TIn: tIn, VolumeIDs: ph.VolumeIDs, CacheStatus: CacheRecomputed,
		}
		br.Energy = blockEnergy(t, ph)
		br.TOut = t * br.TBlock
		t = br.TOut
		result.Blocks = append(result.Blocks, br)
		result.Transmittance = t
		result.TerminalBlock = bi
		if err := store.CommitBlock(ray.Index, br); err != nil {
			return result, commits, err
		}
		commits++
		if opts.OnBlockCommitted != nil {
			if err := opts.OnBlockCommitted(out.BlockCommits+commits, ray.Index, bi); err != nil {
				return result, commits, err
			}
		}
		if opts.CrashAfterBlockCommits > 0 && out.BlockCommits+commits >= opts.CrashAfterBlockCommits {
			return result, commits, ErrCrashInjected
		}
		if t < threshold {
			result.StopReason = StopThreshold
			result.Blocks[len(result.Blocks)-1].Terminal = true
			result.Blocks[len(result.Blocks)-1].StopReason = StopThreshold
			break
		}
	}
	if result.StopReason == "" {
		if len(plan.Blocks) > limit && len(result.Blocks) == limit {
			result.StopReason = StopBlockLimit
		} else {
			result.StopReason = StopCompleted
		}
	}
	return result, commits, nil
}
