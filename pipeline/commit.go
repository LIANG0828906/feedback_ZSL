package pipeline

import (
	"context"
	"encoding/json"

	"iconflow/checkpoint"
	"iconflow/errs"
	"iconflow/graph"
	"iconflow/state"
)

func loadedCacheKey(inputHash string) string {
	return graph.SHA256Hex("stage", "loaded", "input", inputHash)
}

func (r *Runner) beginStage(machine *state.BlockMachine, stage state.Stage) *errs.Error {
	switch machine.Stages[stage] {
	case state.StatusPending:
		return machine.Transition(stage, state.StatusRunning)
	case state.StatusFailed:
		return machine.Retry(stage)
	case state.StatusRunning:
		return nil
	case state.StatusCommitted:
		return nil
	default:
		return errs.State("$.block.stage", "unknown status %s", machine.Stages[stage])
	}
}

func artifactForStage(stage state.Stage, result *BlockResult) Artifact {
	artifact := Artifact{InputHash: result.InputHash}
	switch stage {
	case state.StageLoaded:
	case state.StageGradient:
		artifact.Gradient = result.Gradient
	case state.StageThreshold:
		artifact.Gradient = result.Gradient
		artifact.Threshold = result.Threshold
	case state.StageMarked:
		artifact.Gradient = result.Gradient
		artifact.Threshold = result.Threshold
		artifact.Markers = result.Markers
	case state.StageCleaned:
		artifact.Gradient = result.Gradient
		artifact.Threshold = result.Threshold
		artifact.Markers = result.Markers
		artifact.Stats = result.Stats
	}
	return artifact
}

func summaryForStage(stage state.Stage, result *BlockResult) string {
	switch stage {
	case state.StageLoaded:
		return "loaded block pixels " + result.Coord.Key()
	case state.StageGradient:
		if result.Gradient == nil {
			return "gradient unavailable"
		}
		return "gradient magnitudes computed: " + result.Gradient.OperatorVersion
	case state.StageThreshold:
		if result.Threshold == nil {
			return "threshold unavailable"
		}
		return "integer quantile threshold selected"
	case state.StageMarked:
		count := 0
		for _, marked := range result.Markers {
			if marked {
				count++
			}
		}
		return "foreground markers: " + itoa(count)
	case state.StageCleaned:
		if result.Stats == nil {
			return "cleanup unavailable"
		}
		return "foreground=" + itoa(result.Stats.Foreground) + " regions=" + itoa(len(result.Stats.Components))
	default:
		return ""
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	negative := v < 0
	if negative {
		v = -v
	}
	var buffer [20]byte
	i := len(buffer)
	for v > 0 {
		i--
		buffer[i] = byte('0' + v%10)
		v /= 10
	}
	if negative {
		i--
		buffer[i] = '-'
	}
	return string(buffer[i:])
}

func (r *Runner) commitResult(ctx context.Context, result *BlockResult) *errs.Error {
	key := result.Coord.Key()
	machine := r.blocks[key]
	params := Effective(r.Project, key)
	inputHash := result.InputHash
	keys := result.Keys

	for _, stage := range state.Stages() {
		cacheKey := r.cacheKeyForStage(stage, keys, inputHash)
		if existing, found := r.findCommit(key, stage, cacheKey); found {
			if machine.Stages[stage] != state.StatusCommitted {
				if err := machine.Commit(stage); err != nil {
					return err
				}
			}
			result.StageSeqs[string(stage)] = existing.Seq
			result.ReusedStages = append(result.ReusedStages, string(stage))
			continue
		}
		if machine.Stages[stage] == state.StatusCommitted {
			continue
		}
		if err := r.beginStage(machine, stage); err != nil {
			return err
		}
		artifact := artifactForStage(stage, result)
		artifactJSON, merr := json.Marshal(artifact)
		if merr != nil {
			return errs.New(errs.EInternal, key+"."+string(stage), "marshal artifact: %v", merr)
		}
		threshold := 0
		if result.Threshold != nil {
			threshold = result.Threshold.Threshold
		}
		commit := checkpoint.StageCommit{
			ProjectID:    r.Project.ProjectID,
			BlockKey:     key,
			Stage:        string(stage),
			Status:       string(state.StatusCommitted),
			Summary:      summaryForStage(stage, result),
			Threshold:    threshold,
			CacheKey:     cacheKey,
			InputHash:    inputHash,
			ContentHash:  ContentHash(string(stage), result),
			ParamVersion: r.Project.ParamVersion,
			AlgoVersion:  params.OperatorVersion,
			ArtifactJSON: artifactJSON,
		}
		seq, err := r.Store.CommitStage(ctx, commit)
		if err != nil {
			_ = machine.Transition(stage, state.StatusFailed)
			if domainErr, ok := errs.AsError(err); ok {
				return domainErr
			}
			return errs.New(errs.ECheck, "$.checkpoint.stage", "%v", err)
		}
		commit.Seq = seq
		r.history[key] = append(r.history[key], commit)
		result.StageSeqs[string(stage)] = seq
		if err := machine.Transition(stage, state.StatusCommitted); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) flushWindows(ctx context.Context, report *RunReport) *errs.Error {
	validationByKey := map[string]*errs.Error{}
	for _, result := range report.Results {
		if result.ValidationError != nil {
			validationByKey[result.Coord.Key()] = result.ValidationError
		}
	}
	for _, panel := range r.Project.Panels {
		machine := r.windows[panel.ID]
		if machine == nil {
			machine = state.NewWindowMachine(panel.ID)
			r.windows[panel.ID] = machine
		}
		if machine.Status == state.WindowCommitted {
			continue
		}
		if machine.Status == state.WindowPending {
			if err := machine.Transition(state.WindowSegmenting); err != nil {
				return err
			}
		}
		if machine.Status == state.WindowFailed {
			if err := machine.Transition(state.WindowSegmenting); err != nil {
				return err
			}
		}
		allCommitted := true
		for _, blockKey := range panel.Blocks {
			blockMachine := r.blocks[blockKey]
			if blockMachine == nil || !blockMachine.Frozen() {
				allCommitted = false
				break
			}
			if validationErr := validationByKey[blockKey]; validationErr != nil && validationErr.Code != errs.ERegion && validationErr.Code != errs.ETop {
				allCommitted = false
				break
			}
		}
		status := state.WindowCommitted
		summary := "all blocks committed"
		if !allCommitted {
			status = state.WindowFailed
			summary = "one or more blocks failed or remain incomplete"
			if err := machine.Transition(state.WindowFailed); err != nil {
				return err
			}
		} else {
			if err := machine.Transition(state.WindowCommitted); err != nil {
				return err
			}
		}
		contentHash := graph.HashJSON(panel.Blocks)
		_, err := r.Store.CommitWindow(ctx, checkpoint.WindowCommit{
			ProjectID: r.Project.ProjectID, WindowID: panel.ID,
			Status: string(status), Summary: summary, ContentHash: contentHash,
			ParamVersion: r.Project.ParamVersion, AlgoVersion: r.Project.Rules.OperatorVersion,
		})
		if err != nil {
			return errs.New(errs.ECheck, "$.checkpoint.window", "%v", err)
		}
	}
	return nil
}
