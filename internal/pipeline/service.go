package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"meshdoctor/internal/errs"
	"meshdoctor/internal/mesh"
	"meshdoctor/internal/sample"
	"meshdoctor/internal/store"
)

type Orchestrator struct{}

func NewOrchestrator() *Orchestrator { return &Orchestrator{} }

type RunOptions struct {
	Path          string
	StopAfter     int
	BeforeExecute func(stage string)
}

func (o *Orchestrator) RunOrResume(
	ctx context.Context,
	sampleID string,
	input *mesh.Mesh,
	manifest *sample.Manifest,
	params Params,
	st *store.Store,
	opts RunOptions,
) (*PathResult, error) {
	params = params.WithDefaults()
	pathName := opts.Path
	if pathName == "" {
		pathName = "segmented"
	}
	result := &PathResult{Path: pathName, SampleID: sampleID, ParamVersion: params.Version()}
	engine := NewEngine(sampleID, manifest, params)

	current := input.Clone()
	var seam []int
	startStage := 0

	if st != nil {
		snapshots, _, err := st.LoadRecoveryChain(ctx, sampleID, params.Version(), AlgoVersion)
		if err != nil && !errors.Is(err, store.ErrInvalidSnapshot) {
			return result, err
		}
		if errors.Is(err, store.ErrInvalidSnapshot) {
			result.StopReason = "snapshot_invalidated_full_recompute"
			snapshots = nil
		}
		for _, raw := range snapshots {
			var snap StageSnapshot
			if err := json.Unmarshal(raw, &snap); err != nil {
				_ = st.InvalidateSample(ctx, sampleID, "snapshot JSON decode failure")
				return nil, errs.New(errs.EVER, "stage snapshot version/payload cannot be decoded: %v", err)
			}
			if err := CheckVersion(snap.Stage, snap.ParamVersion, snap.AlgoVersion, params); err != nil {
				return nil, err
			}
			if snap.Mesh == nil || snap.Mesh.Hash() != snap.MeshHash {
				_ = st.InvalidateSample(ctx, sampleID, "snapshot mesh hash mismatch")
				return nil, errs.New(errs.EVER, "content hash mismatch at stage %s; snapshot invalidated", snap.Stage)
			}
			result.Stages = append(result.Stages, snap)
			result.CommittedSequence = append(result.CommittedSequence, snap.Stage)
			current = snap.Mesh
			if snap.Stage == StageFill && snap.Fill != nil {
				seam = snap.Fill.SeamVertices
			}
			startStage = snap.StageIndex + 1
		}
		result.CacheHits = len(snapshots)
	}

	for i := startStage; i < len(Stages); i++ {
		stage := Stages[i]
		if opts.StopAfter > 0 && len(result.CommittedSequence) >= opts.StopAfter {
			result.StopReason = "stop_after_committed_boundary"
			return result, nil
		}
		if err := ctx.Err(); err != nil {
			result.StopReason = "canceled_before_stage"
			return result, err
		}

		if st != nil {
			rec, err := st.GetState(ctx, sampleID, stage, params.Version(), AlgoVersion)
			if err != nil {
				return result, err
			}
			state := State(rec.State)
			if state == StateRunning {
				_ = Transition(StateRunning, StateFailed)
				_ = st.SaveState(ctx, store.StateRecord{
					SampleID: sampleID, Stage: stage, ParamVersion: params.Version(), AlgoVersion: AlgoVersion,
					State: string(StateFailed), ErrorCode: "E_KILL_RECOVERY", ErrorMessage: "stale running state after termination",
				})
				state = StateFailed
			}
			if state == StateCommitted {
				return result, errs.New(errs.ESTATE, "stage %s is already committed but absent from recovery chain", stage)
			}
			if err := Transition(state, StateRunning); err != nil {
				return result, errs.WithDetail(errs.InStage(err, stage), "stage", stage)
			}
			if err := st.SaveState(ctx, store.StateRecord{
				SampleID: sampleID, Stage: stage, ParamVersion: params.Version(), AlgoVersion: AlgoVersion,
				State: string(StateRunning),
			}); err != nil {
				return result, err
			}
		}

		if opts.BeforeExecute != nil {
			opts.BeforeExecute(stage)
		}

		snap, next, nextSeam, execErr := engine.RunStage(stage, current, seam)
		if execErr != nil {
			if st != nil {
				_ = st.SaveState(ctx, store.StateRecord{
					SampleID: sampleID, Stage: stage, ParamVersion: params.Version(), AlgoVersion: AlgoVersion,
					State: string(StateFailed), ErrorCode: snap.ErrorCode, ErrorMessage: snap.ErrorMessage,
				})
			}
			return result, errs.InStage(execErr, stage)
		}

		now := time.Now().UTC()
		snap.CommittedAt = now
		if st != nil {
			blob := snap.Mesh.CanonicalBytes()
			raw, err := json.Marshal(snap)
			if err != nil {
				return result, err
			}
			err = st.CommitCheckpoint(ctx, store.Checkpoint{
				SampleID: sampleID, Stage: stage, StageIndex: i, State: string(StateCommitted),
				ParamVersion: params.Version(), AlgoVersion: AlgoVersion, ParamsJSON: params.SnapshotJSON(),
				MeshBlob: blob, MeshHash: snap.MeshHash, SnapshotJSON: raw, UpdatedAt: now.Format(time.RFC3339Nano),
			})
			if err != nil {
				return result, err
			}
			st.CacheMisses.Add(1)
			result.CacheMisses++
		}

		result.Stages = append(result.Stages, *snap)
		result.CommittedSequence = append(result.CommittedSequence, stage)
		current = next
		seam = nextSeam
	}

	if len(result.Stages) > 0 {
		last := result.Stages[len(result.Stages)-1]
		result.FinalHash = last.MeshHash
		result.FinalStats = last.Stats
	}
	if opts.StopAfter > 0 && len(result.CommittedSequence) >= opts.StopAfter {
		result.StopReason = "stop_after_committed_boundary"
	}
	return result, nil
}

func (o *Orchestrator) RunOneShot(sampleID string, input *mesh.Mesh, man *sample.Manifest, params Params) (*PathResult, error) {
	res, err := NewEngine(sampleID, man, params).RunAll(input)
	if res != nil {
		res.Path = "one_shot"
	}
	return res, err
}
