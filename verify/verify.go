package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"example.com/multiscreen-cast/kernel"
	"example.com/multiscreen-cast/protocol"
	"example.com/multiscreen-cast/store"
)

type Report struct {
	Sessions              int  `json:"sessions"`
	Windows               int  `json:"windows"`
	TruthMatched          bool `json:"truth_matched"`
	ThreePathsMatched     bool `json:"three_paths_matched"`
	RecoveryPointsChecked int  `json:"recovery_points_checked"`
	RealtimeWindows       int  `json:"realtime_windows"`
	ReplayWindows         int  `json:"replay_windows"`
	CheckpointRowsChecked int  `json:"checkpoint_rows_checked"`
	ForcedStopExitReport  bool `json:"forced_stop_exit_report"`
}

func VerifySample(ctx context.Context, set *protocol.SampleSet, workRoot string) (*Report, *protocol.KernelError) {
	report := &Report{Sessions: len(set.Sessions)}
	if workRoot == "" {
		workRoot = os.TempDir()
	}
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		return nil, protocol.Errorf(protocol.CodeStore, workRoot, "$", "cannot prepare verification directory: %v", err)
	}
	for i := range set.Sessions {
		session := &set.Sessions[i]
		if i >= len(set.Truths) {
			return nil, protocol.Errorf(protocol.CodeDiff, "verify", "$.truths", "missing truth for session %q", session.ID)
		}
		truth := &set.Truths[i]
		cfg := kernel.Config{Mapping: kernel.DefaultMappingConfig()}
		fullEngine, err := kernel.NewEngine(session, cfg)
		if err != nil {
			return nil, err
		}
		fullTrace, err := fullEngine.Run(ctx)
		if err != nil {
			return nil, err
		}
		if err := CompareTruth(session, truth, fullTrace); err != nil {
			return nil, err
		}
		report.TruthMatched = true
		report.Windows += len(session.Windows)

		realtimeDir, tempErr := os.MkdirTemp(workRoot, "realtime-*")
		if tempErr != nil {
			return nil, protocol.Errorf(protocol.CodeStore, workRoot, "$", "cannot create realtime workspace: %v", tempErr)
		}
		defer os.RemoveAll(realtimeDir)
		realtimeStore, openErr := store.Open(filepath.Join(realtimeDir, "checkpoints.sqlite"))
		if openErr != nil {
			return nil, protocol.AsKernelError(openErr)
		}
		realtimeCfg := kernel.Config{Mapping: kernel.DefaultMappingConfig(), Checkpoint: realtimeStore}
		realtimeEngine, err := kernel.NewEngine(session, realtimeCfg)
		if err != nil {
			_ = realtimeStore.Close()
			return nil, err
		}
		realtimeTrace, err := realtimeEngine.Run(ctx)
		if err != nil {
			_ = realtimeStore.Close()
			return nil, err
		}
		rows, rowErr := realtimeStore.Rows(session.ID)
		if rowErr != nil {
			_ = realtimeStore.Close()
			return nil, protocol.AsKernelError(rowErr)
		}
		for j, row := range rows {
			if row.WindowIndex != j+1 {
				_ = realtimeStore.Close()
				return nil, protocol.Errorf(protocol.CodeDiff, "verify", "$.checkpoints",
					"checkpoint order differs at row %d: got window %d", j, row.WindowIndex)
			}
		}
		report.CheckpointRowsChecked += len(rows)
		if err := realtimeStore.Close(); err != nil {
			return nil, protocol.Errorf(protocol.CodeStore, realtimeDir, "$", "cannot close realtime checkpoint: %v", err)
		}
		if diff := kernel.CompareTraces(*fullTrace, *realtimeTrace); diff != nil {
			return nil, diff
		}
		report.RealtimeWindows += len(realtimeTrace.Windows)
		report.ThreePathsMatched = true

		breakpoints := recoveryBreakpoints(len(session.Windows))
		for _, breakpoint := range breakpoints {
			recovered, recoveryRows, err := runRecovery(ctx, session, workRoot, breakpoint)
			if err != nil {
				return nil, err
			}
			report.RecoveryPointsChecked++
			report.ReplayWindows += len(recovered.Windows)
			report.CheckpointRowsChecked += recoveryRows
			if diff := kernel.CompareTraces(*fullTrace, recovered); diff != nil {
				if diff.Window == 0 && len(recovered.Windows) > 0 {
					diff.Window = len(recovered.Windows)
				}
				return nil, diff
			}
		}
	}
	report.ForcedStopExitReport = false
	return report, nil
}

func runRecovery(ctx context.Context, session *protocol.Session, workRoot string, breakpoint int) (kernel.Trace, int, *protocol.KernelError) {
	dir, err := os.MkdirTemp(workRoot, "recovery-*")
	if err != nil {
		return kernel.Trace{}, 0, protocol.Errorf(protocol.CodeStore, workRoot, "$", "cannot create recovery workspace: %v", err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "checkpoints.sqlite")
	firstStore, openErr := store.Open(dbPath)
	if openErr != nil {
		return kernel.Trace{}, 0, protocol.AsKernelError(openErr)
	}
	firstCfg := kernel.Config{Mapping: kernel.DefaultMappingConfig(), Checkpoint: firstStore}
	firstEngine, engineErr := kernel.NewEngine(session, firstCfg)
	if engineErr != nil {
		_ = firstStore.Close()
		return kernel.Trace{}, 0, engineErr
	}
	for firstEngine.Progress().Committed < breakpoint && !firstEngine.Done() {
		if _, advanceErr := firstEngine.Advance(ctx); advanceErr != nil {
			_ = firstStore.Close()
			return kernel.Trace{}, 0, advanceErr
		}
	}
	progress := firstEngine.Progress()
	if progress.Committed < breakpoint {
		_ = firstStore.Close()
		return kernel.Trace{}, 0, protocol.Errorf(protocol.CodeState, "verify", "$.recovery",
			"could not reach recovery breakpoint %d", breakpoint)
	}
	if err := firstStore.Close(); err != nil {
		return kernel.Trace{}, 0, protocol.Errorf(protocol.CodeStore, dbPath, "$", "cannot close first store: %v", err)
	}
	secondStore, openErr := store.Open(dbPath)
	if openErr != nil {
		return kernel.Trace{}, 0, protocol.AsKernelError(openErr)
	}
	defer secondStore.Close()
	snapshot, status, loadErr := secondStore.LoadLatest(session, protocol.CurrentAlgorithmVersion)
	if loadErr != nil {
		return kernel.Trace{}, 0, protocol.AsKernelError(loadErr)
	}
	if status.Invalidated {
		return kernel.Trace{}, 0, protocol.Errorf(protocol.CodeState, "verify", "$.recovery",
			"recovery checkpoint invalidated: %s", status.Reason)
	}
	if snapshot == nil {
		return kernel.Trace{}, 0, protocol.Errorf(protocol.CodeState, "verify", "$.recovery",
			"no recovery checkpoint found")
	}
	secondCfg := kernel.Config{Mapping: kernel.DefaultMappingConfig(), Checkpoint: secondStore}
	engine, engineErr := kernel.RestoreEngine(session, secondCfg, *snapshot)
	if engineErr != nil {
		return kernel.Trace{}, 0, engineErr
	}
	trace, runErr := engine.Run(ctx)
	if runErr != nil {
		return kernel.Trace{}, 0, runErr
	}
	rows, rowErr := secondStore.Rows(session.ID)
	if rowErr != nil {
		return kernel.Trace{}, 0, protocol.AsKernelError(rowErr)
	}
	return *trace, len(rows), nil
}

func recoveryBreakpoints(windows int) []int {
	if windows <= 1 {
		return []int{windows}
	}
	points := make([]int, 0, windows)
	for i := 1; i <= windows; i++ {
		points = append(points, i)
	}
	return points
}

func CompareTruth(session *protocol.Session, truth *protocol.Truth, trace *kernel.Trace) *protocol.KernelError {
	if truth.SessionID != session.ID {
		return truthDiff(0, "", "session_id", truth.SessionID, session.ID)
	}
	if len(truth.Windows) != len(trace.Windows) {
		return truthDiff(0, "", "windows", fmt.Sprint(len(truth.Windows)), fmt.Sprint(len(trace.Windows)))
	}
	for i := range truth.Windows {
		expected := truth.Windows[i]
		actual := trace.Windows[i]
		if expected.Window != actual.Window {
			return truthDiff(expected.Window, "", "window", fmt.Sprint(expected.Window), fmt.Sprint(actual.Window))
		}
		if expected.Time != actual.Time {
			return truthDiff(expected.Window, "", "time", fmt.Sprint(expected.Time), fmt.Sprint(actual.Time))
		}
		if !sameStrings(expected.Endpoints, actual.Endpoints) {
			return truthDiff(expected.Window, "", "endpoints", fmt.Sprint(expected.Endpoints), fmt.Sprint(actual.Endpoints))
		}
		if len(expected.Screens) != len(actual.Screens) {
			return truthDiff(expected.Window, "", "screens.count", fmt.Sprint(len(expected.Screens)), fmt.Sprint(len(actual.Screens)))
		}
		actualScreens := make(map[string]kernel.ScreenResult, len(actual.Screens))
		for _, screen := range actual.Screens {
			actualScreens[screen.DisplayID] = screen
		}
		for _, expectedScreen := range expected.Screens {
			actualScreen, ok := actualScreens[expectedScreen.DisplayID]
			if !ok {
				return truthDiff(expected.Window, expectedScreen.DisplayID, "screen.presence", "present", "missing")
			}
			if expectedScreen.SourceWindow != actualScreen.SourceWindow {
				return truthDiff(expected.Window, expectedScreen.DisplayID, "source_window",
					fmt.Sprint(expectedScreen.SourceWindow), fmt.Sprint(actualScreen.SourceWindow))
			}
			if err := compareTruthRect(expected.Window, expectedScreen.DisplayID, "pixel", expectedScreen.Pixel, actualScreen.Pixel); err != nil {
				return err
			}
			if err := compareTruthRect(expected.Window, expectedScreen.DisplayID, "world", expectedScreen.World, actualScreen.World); err != nil {
				return err
			}
			if err := compareTruthRect(expected.Window, expectedScreen.DisplayID, "screen_world", expectedScreen.ScreenWorld, actualScreen.ScreenWorld); err != nil {
				return err
			}
		}
	}
	if len(truth.EventEffects) != len(trace.EventEffects) {
		return truthDiff(0, "", "event_effects", fmt.Sprint(len(truth.EventEffects)), fmt.Sprint(len(trace.EventEffects)))
	}
	for i := range truth.EventEffects {
		expected := truth.EventEffects[i]
		actual := trace.EventEffects[i]
		window := expected.ReplayWindow
		if expected.Seq != actual.Seq {
			return truthDiff(window, expected.DisplayID, "event.seq", fmt.Sprint(expected.Seq), fmt.Sprint(actual.Seq))
		}
		if expected.Time != actual.Time || expected.EffectiveTime != actual.EffectiveTime {
			return truthDiff(window, expected.DisplayID, "event.effective_time",
				fmt.Sprintf("%d/%d", expected.Time, expected.EffectiveTime),
				fmt.Sprintf("%d/%d", actual.Time, actual.EffectiveTime))
		}
		if expected.CommittedWindow != actual.CommittedWindow {
			return truthDiff(window, expected.DisplayID, "event.committed_window",
				fmt.Sprint(expected.CommittedWindow), fmt.Sprint(actual.CommittedWindow))
		}
		if expected.Kind != actual.Kind {
			return truthDiff(window, expected.DisplayID, "event.kind", string(expected.Kind), string(actual.Kind))
		}
		if expected.DisplayID != actual.DisplayID {
			return truthDiff(window, expected.DisplayID, "event.display_id", expected.DisplayID, actual.DisplayID)
		}
		if !sameStrings(expected.EndpointsAfter, actual.EndpointsAfter) {
			return truthDiff(window, expected.DisplayID, "event.endpoints_after",
				fmt.Sprint(expected.EndpointsAfter), fmt.Sprint(actual.EndpointsAfter))
		}
		if expected.StateAfter != actual.StateAfter {
			return truthDiff(window, expected.DisplayID, "event.state_after", string(expected.StateAfter), string(actual.StateAfter))
		}
		if (expected.Replay == nil) != (actual.Replay == nil) {
			return truthDiff(window, expected.DisplayID, "event.replay", fmt.Sprint(expected.Replay != nil), fmt.Sprint(actual.Replay != nil))
		}
		if expected.Replay != nil {
			if expected.ReplayWindow != actual.ReplayWindow {
				return truthDiff(window, expected.DisplayID, "event.replay_window",
					fmt.Sprint(expected.ReplayWindow), fmt.Sprint(actual.ReplayWindow))
			}
			if err := compareTruthRect(window, expected.DisplayID, "replay.pixel",
				expected.Replay.Pixel, actual.Replay.Pixel); err != nil {
				return err
			}
			if err := compareTruthRect(window, expected.DisplayID, "replay.world",
				expected.Replay.World, actual.Replay.World); err != nil {
				return err
			}
			if err := compareTruthRect(window, expected.DisplayID, "replay.screen_world",
				expected.Replay.ScreenWorld, actual.Replay.ScreenWorld); err != nil {
				return err
			}
		}
	}
	if !sameStrings(truth.FinalEndpoints, trace.FinalEndpoints) {
		return truthDiff(len(trace.Windows), "", "final_endpoints",
			fmt.Sprint(truth.FinalEndpoints), fmt.Sprint(trace.FinalEndpoints))
	}
	if truth.FinalState != trace.FinalState {
		return truthDiff(len(trace.Windows), "", "final_state", string(truth.FinalState), string(trace.FinalState))
	}
	return nil
}

func compareTruthRect(window int, displayID, field string, expected, actual protocol.Rect) *protocol.KernelError {
	if expected.X != actual.X {
		return truthDiff(window, displayID, field+".x", fmt.Sprint(expected.X), fmt.Sprint(actual.X))
	}
	if expected.Y != actual.Y {
		return truthDiff(window, displayID, field+".y", fmt.Sprint(expected.Y), fmt.Sprint(actual.Y))
	}
	if expected.Width != actual.Width {
		return truthDiff(window, displayID, field+".width", fmt.Sprint(expected.Width), fmt.Sprint(actual.Width))
	}
	if expected.Height != actual.Height {
		return truthDiff(window, displayID, field+".height", fmt.Sprint(expected.Height), fmt.Sprint(actual.Height))
	}
	return nil
}

func truthDiff(window int, displayID, field, left, right string) *protocol.KernelError {
	err := protocol.Errorf(protocol.CodeDiff, "truth", field,
		"truth mismatch window=%d display=%s field=%s expected=%s actual=%s", window, displayID, field, left, right)
	err.Window = window
	err.DisplayID = displayID
	err.Field = field
	err.Cause = fmt.Sprintf("expected=%s actual=%s", left, right)
	return err
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func SortedStrings(values []string) []string {
	output := append([]string(nil), values...)
	sort.Strings(output)
	return output
}
