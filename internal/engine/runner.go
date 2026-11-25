package engine

import (
	"context"
	"fmt"
	"sync"

	"maskpreview/internal/core"
	"maskpreview/internal/segment"
)

type RunMode string

const (
	ModeFull        RunMode = "full"
	ModeIncremental RunMode = "incremental"
	ModeResume      RunMode = "resume"
)

type RunConfig struct {
	BlockSize int    `json:"block_size"`
	Budget    Budget `json:"budget"`
}

func DefaultRunConfig() RunConfig {
	return RunConfig{
		BlockSize: 16,
		Budget: Budget{
			MaxParallel:      4,
			MaxPerBlockBytes: 1 << 20,
			GlobalBytes:      8 << 20,
		},
	}
}

func (c RunConfig) Validate() error {
	if c.BlockSize <= 0 {
		return core.ParamError("run.config", "block_size", c.BlockSize, 1, "unbounded")
	}
	_, err := NewScheduler(c.Budget)
	return err
}

type RunResult struct {
	Mode              RunMode              `json:"mode"`
	Exact             *segment.ExactResult `json:"-"`
	MaskHash          string               `json:"mask_hash"`
	RegionCount       int                  `json:"region_count"`
	Regions           []segment.RegionStat `json:"regions"`
	Blocks            []Block              `json:"blocks"`
	Records           []StageRecord        `json:"records"`
	BlockStates       []BlockStateReport   `json:"block_states"`
	Progress          Progress             `json:"progress"`
	CheckpointReused  bool                 `json:"checkpoint_reused"`
	InvalidatedReason string               `json:"invalidated_reason,omitempty"`
	Scheduler         SchedulerStats       `json:"scheduler"`
}

type BlockStateReport struct {
	BlockID       int                  `json:"block_id"`
	States        map[Stage]StageState `json:"states"`
	FrozenVersion string               `json:"frozen_version,omitempty"`
}

type commitGate struct {
	mu   sync.Mutex
	cond *sync.Cond
	next int
	err  error
}

func newCommitGate() *commitGate {
	g := &commitGate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *commitGate) do(index int, fn func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.next != index && g.err == nil {
		g.cond.Wait()
	}
	if g.err != nil {
		return g.err
	}
	if err := fn(); err != nil {
		g.err = err
		g.cond.Broadcast()
		return err
	}
	g.next++
	g.cond.Broadcast()
	return nil
}

func (g *commitGate) fail(err error) {
	g.mu.Lock()
	if g.err == nil {
		g.err = err
	}
	g.cond.Broadcast()
	g.mu.Unlock()
}

func Execute(ctx context.Context, img *core.Image, snap core.Snapshot, truthFingerprint string, mode RunMode, store *Store, cfg RunConfig) (*RunResult, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, core.ParamError("run.execute", "store", "nil", 1, "open checkpoint store")
	}
	if mode == ModeResume {
		cp, err := store.LoadCheckpoint(snap, truthFingerprint)
		if err == nil {
			regions, labels := segment.Analyze(cp.CleanMask, cp.Info.Width, cp.Info.Height)
			exact := &segment.ExactResult{
				GrowMask:       cp.GrowMask,
				Mask:           cp.CleanMask,
				LabelMask:      labels,
				Width:          cp.Info.Width,
				Height:         cp.Info.Height,
				Regions:        regions,
				Snapshot:       snap.Clone(),
				IterationsUsed: snap.Params.GrowthIterations,
			}
			states := make([]BlockStateReport, 0, len(cp.Blocks))
			for _, block := range cp.Blocks {
				stateMap := map[Stage]StageState{
					StageGrow:  StateCommitted,
					StageClean: StateCommitted,
					StageStats: StateCommitted,
				}
				states = append(states, BlockStateReport{BlockID: block.ID, States: stateMap, FrozenVersion: snap.Version})
			}
			return &RunResult{
				Mode:             ModeResume,
				Exact:            exact,
				MaskHash:         core.HashBytes(cp.CleanMask),
				RegionCount:      len(regions),
				Regions:          regions,
				Blocks:           cp.Blocks,
				Records:          cp.Records,
				BlockStates:      states,
				Progress:         cp.Progress,
				CheckpointReused: true,
			}, nil
		}
		if core.CodeOf(err) != core.CodeResume {
			return nil, err
		}
		result, runErr := runCompute(ctx, img, snap, truthFingerprint, ModeResume, store, cfg)
		if runErr != nil {
			return nil, runErr
		}
		result.InvalidatedReason = err.Error()
		return result, nil
	}
	if mode != ModeFull && mode != ModeIncremental {
		return nil, core.ParamError("run.execute", "mode", mode, "full|incremental|resume", "")
	}
	return runCompute(ctx, img, snap, truthFingerprint, mode, store, cfg)
}

func runCompute(ctx context.Context, img *core.Image, snap core.Snapshot, truthFingerprint string, mode RunMode, store *Store, cfg RunConfig) (*RunResult, error) {
	if err := CheckContext(ctx); err != nil {
		return nil, err
	}
	exact, err := segment.Exact(img, snap)
	if err != nil {
		return nil, err
	}
	blocks, err := SplitBlocks(img.Width, img.Height, cfg.BlockSize)
	if err != nil {
		return nil, err
	}
	if err := store.BeginSnapshot(snap, truthFingerprint, blocks, cfg.BlockSize, img.Width, img.Height); err != nil {
		return nil, err
	}
	scheduler, err := NewScheduler(cfg.Budget)
	if err != nil {
		return nil, err
	}
	gate := newCommitGate()
	fsms := make([]*BlockFSM, len(blocks))
	for i, block := range blocks {
		fsms[i] = NewBlockFSM(block.ID)
	}
	jobs := make([]Job, 0, len(blocks))
	for blockIndex, block := range blocks {
		blockIndex := blockIndex
		block := block
		fsm := fsms[blockIndex]
		jobs = append(jobs, Job{
			BlockID: block.ID,
			Run: func(ctx context.Context) error {
				for stageIndex, stage := range StageOrder {
					canonical := blockIndex*len(StageOrder) + stageIndex
					stageBytes := int64(block.Width*block.Height + 1024)
					release, acquireErr := scheduler.Acquire(ctx, block.ID, stageBytes, fmt.Sprintf("%d:%s", block.ID, stage))
					if acquireErr != nil {
						gate.fail(acquireErr)
						return acquireErr
					}
					if startErr := fsm.Start(stage, snap.Version); startErr != nil {
						release()
						gate.fail(startErr)
						return startErr
					}
					artifact, summary, buildErr := buildArtifact(stage, block, exact)
					var payload []byte
					if buildErr == nil {
						payload, buildErr = EncodeArtifact(artifact)
					}
					release()
					if buildErr != nil {
						_ = fsm.Fail(stage)
						gate.fail(buildErr)
						return buildErr
					}
					hash := ArtifactHash(payload)
					commitErr := gate.do(canonical, func() error {
						_, dbErr := store.CommitStage(StageCommit{
							SnapshotVersion:  snap.Version,
							CanonicalIndex:   canonical,
							BlockID:          block.ID,
							Stage:            stage,
							Summary:          summary,
							ContentHash:      hash,
							ParamVersion:     snap.ParamVersion,
							AlgorithmVersion: snap.AlgorithmVersion,
							Payload:          payload,
						})
						if dbErr != nil {
							return dbErr
						}
						return fsm.Commit(stage, snap.Version, hash)
					})
					if commitErr != nil {
						gate.fail(commitErr)
						return commitErr
					}
				}
				return nil
			},
		})
	}
	if err := scheduler.Run(ctx, jobs); err != nil {
		return nil, err
	}
	records, err := store.StageRecords(snap.Version)
	if err != nil {
		return nil, err
	}
	progress := Progress{Committed: 0, Total: len(blocks) * len(StageOrder)}
	blockStates := make([]BlockStateReport, 0, len(fsms))
	for i, fsm := range fsms {
		p := fsm.Progress()
		progress.Committed += p.Committed
		blockStates = append(blockStates, BlockStateReport{
			BlockID:       blocks[i].ID,
			States:        fsm.AllStates(),
			FrozenVersion: fsm.FrozenVersion(),
		})
	}
	return &RunResult{
		Mode:        mode,
		Exact:       exact,
		MaskHash:    core.HashBytes(exact.Mask),
		RegionCount: len(exact.Regions),
		Regions:     exact.Regions,
		Blocks:      blocks,
		Records:     records,
		BlockStates: blockStates,
		Progress:    progress,
		Scheduler:   scheduler.Stats(),
	}, nil
}

func buildArtifact(stage Stage, block Block, exact *segment.ExactResult) (Artifact, string, error) {
	artifact := Artifact{Stage: stage, Block: block}
	switch stage {
	case StageGrow:
		artifact.Mask = cropMask(exact.GrowMask, exact.Width, block)
		return artifact, fmt.Sprintf("grow block=%d foreground=%d", block.ID, countOnes(artifact.Mask)), nil
	case StageClean:
		artifact.Mask = cropMask(exact.Mask, exact.Width, block)
		return artifact, fmt.Sprintf("clean block=%d foreground=%d", block.ID, countOnes(artifact.Mask)), nil
	case StageStats:
		localMask := cropMask(exact.Mask, exact.Width, block)
		regions, _ := segment.Analyze(localMask, block.Width, block.Height)
		artifact.Regions = regions
		area := 0
		for _, region := range regions {
			area += region.Area
		}
		return artifact, fmt.Sprintf("stats block=%d regions=%d area=%d", block.ID, len(regions), area), nil
	default:
		return Artifact{}, "", &core.Error{Code: core.CodeState, Op: "run.build_artifact", Field: "stage", Got: stage, Want: "grow|clean|stats"}
	}
}

func countOnes(mask []uint8) int {
	n := 0
	for _, v := range mask {
		if v != 0 {
			n++
		}
	}
	return n
}
