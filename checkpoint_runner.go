package macrokit

import "context"

type CheckpointRun struct {
	Result          *RunResult
	Stopped         bool
	StoppedAtWindow int64
}

// RunCheckpointed processes frame windows and atomically persists each one.
// A non-nil stopAfterWindow simulates process termination immediately after the
// requested window has committed; callers can reopen SQLite and invoke this
// function again to exercise breakpoint recovery.
func RunCheckpointed(
	ctx context.Context,
	store *SQLiteStore,
	project *Project,
	events EventStream,
	config ExecutorConfig,
	windowSize int64,
	faults *FaultInjector,
	stopAfterWindow *int64,
) (*CheckpointRun, error) {
	if windowSize <= 0 {
		windowSize = 8
	}

	var snapshot *CheckpointSnapshot
	if store != nil {
		var err error
		snapshot, err = store.LoadSnapshot(ctx, project.ID, project.FormatNo, project.FormatVersion)
		if err != nil {
			return nil, err
		}
	}

	exec, err := NewExecutor(project, config)
	if err != nil {
		return nil, err
	}
	if snapshot != nil && snapshot.State != nil {
		if err := exec.ImportState(snapshot.State); err != nil {
			return nil, err
		}
	}

	inputs, err := BuildFrameInputs(events, config.FrameCount)
	if err != nil {
		return nil, err
	}
	startFrame := exec.ExportState().NextFrame
	for frame := startFrame; frame < int64(len(inputs)); frame++ {
		input := inputs[frame]
		if _, err := exec.Step(input); err != nil {
			return &CheckpointRun{Result: exec.Result()}, err
		}

		lastFrame := int64(len(inputs) - 1)
		shouldCommit := (frame+1)%windowSize == 0 || frame == lastFrame
		if !shouldCommit || store == nil {
			continue
		}

		windowIndex := frame / windowSize
		windowStart := windowIndex * windowSize
		conditionSnapshots := map[int64]map[string]bool{}
		for _, trace := range exec.frames {
			if trace.Frame >= windowStart && trace.Frame <= frame {
				conditionSnapshots[trace.Frame] = trace.ConditionSnapshot
			}
		}
		state := exec.ExportState()
		checkpoint := &WindowCheckpoint{
			ProjectID: project.ID, WindowIndex: windowIndex, StartFrame: windowStart, EndFrame: frame,
			ConsumedOffset: exec.consumedOffset, FormatNo: project.FormatNo, FormatVersion: project.FormatVersion,
			BranchResults: exec.results, ConditionSnapshots: conditionSnapshots, State: state,
		}
		if err := store.CommitWindow(ctx, checkpoint, faults); err != nil {
			return &CheckpointRun{Result: exec.Result()}, err
		}
		if stopAfterWindow != nil && windowIndex == *stopAfterWindow {
			return &CheckpointRun{Result: exec.Result(), Stopped: true, StoppedAtWindow: windowIndex}, nil
		}
	}

	return &CheckpointRun{Result: exec.Result()}, nil
}
