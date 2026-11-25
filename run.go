package lightshow

import (
	"context"
	"fmt"
	"sort"
)

type RunMode string

const (
	RunRealtime      RunMode = "realtime"
	RunResume        RunMode = "resume"
	RunFullRecompute RunMode = "full-recompute"
)

type RunResult struct {
	Mode          RunMode
	Frames        []FrameOutput
	Decisions     []Decision
	Consumption   []ChangeConsumption
	Windows       []WindowResult
	ReusedWindows int
	Committed     int
}

type RunOptions struct {
	Mode RunMode
	// CancelAfter is the number of windows to finish before returning
	// E_CANCELED. Use -1 (the default via Run) to disable.
	CancelAfter int
}

type Mismatch struct {
	Frame     int
	FixtureID string
	Kind      string
}

func (m *Mismatch) Error() string {
	if m == nil {
		return ""
	}
	return fmt.Sprintf("first mismatch at frame %d fixture %s (%s)", m.Frame, m.FixtureID, m.Kind)
}

// Run executes one of the three equivalent scheduling paths. Real-time and
// full-recompute start from a clean cache; resume reuses only committed windows
// whose project/algorithm/source/format metadata matches exactly.
func Run(ctx context.Context, show *Show, store *Store, mode RunMode) (*RunResult, error) {
	return RunWithOptions(ctx, show, store, RunOptions{Mode: mode, CancelAfter: -1})
}

func RunWithOptions(ctx context.Context, show *Show, store *Store, options RunOptions) (*RunResult, error) {
	if show == nil || store == nil {
		return nil, engineError(CodeVersion, "project", "show and store are required")
	}
	mode := options.Mode
	copy := show.Clone()
	if err := copy.Normalize(); err != nil {
		return nil, err
	}
	projectHash := ProjectHash(copy)
	algorithm := AlgorithmVersion
	sourceVersion := copy.SourceVersion.String()
	formatVersion := copy.SourceVersion.String()

	if mode != RunResume {
		if err := store.Reset(); err != nil {
			return nil, err
		}
	} else {
		if err := store.PrepareForProject(projectHash, algorithm, sourceVersion, formatVersion); err != nil {
			return nil, err
		}
		if err := store.RecoverInFlight(); err != nil {
			return nil, err
		}
	}
	plan, err := Schedule(copy)
	if err != nil {
		return nil, err
	}
	for _, window := range plan.Windows {
		if err := store.EnsureWindow(window); err != nil {
			return nil, err
		}
	}
	reused := make(map[int]WindowResult)
	if mode == RunResume {
		cached, err := store.ReadCommitted(projectHash, algorithm, sourceVersion, formatVersion)
		if err != nil {
			return nil, err
		}
		for _, entry := range cached {
			reused[entry.Result.Index] = entry.Result
		}
	}
	result := &RunResult{Mode: mode, ReusedWindows: len(reused)}
	newlyCommitted := 0
	if options.CancelAfter == 0 {
		partial, loadErr := loadRunResult(store, copy, mode)
		if loadErr != nil {
			return partial, loadErr
		}
		partial.ReusedWindows = len(reused)
		partial.Committed = newlyCommitted
		return partial, windowContextError(context.Canceled)
	}
	finished := 0
	for _, window := range plan.Windows {
		if cached, ok := reused[window.Index]; ok {
			result.Committed++
			_ = cached
			finished++
			if options.CancelAfter >= 0 && finished >= options.CancelAfter {
				partial, loadErr := loadRunResult(store, copy, mode)
				if loadErr != nil {
					return partial, loadErr
				}
				partial.ReusedWindows = len(reused)
				partial.Committed = newlyCommitted
				return partial, windowContextError(context.Canceled)
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			partial, loadErr := loadRunResult(store, copy, mode)
			if loadErr != nil {
				return partial, loadErr
			}
			partial.ReusedWindows = len(reused)
			partial.Committed = newlyCommitted
			return partial, windowContextError(err)
		}
		state, err := store.State(window.WindowID)
		if err != nil {
			return nil, err
		}
		if state == StatePending {
			if err := store.Transition(window.WindowID, StateScheduling); err != nil {
				return nil, err
			}
		} else if state == StateFailed {
			if err := store.Transition(window.WindowID, StateScheduling); err != nil {
				return nil, err
			}
		} else {
			return nil, engineError(CodeState, window.WindowID, "cannot schedule from state %s", state)
		}
		if err := ctx.Err(); err != nil {
			_ = store.Fail(window.WindowID)
			partial, loadErr := loadRunResult(store, copy, mode)
			if loadErr != nil {
				return partial, loadErr
			}
			partial.ReusedWindows = len(reused)
			partial.Committed = newlyCommitted
			return partial, windowContextError(err)
		}
		window.ContentHash = window.ComputeContentHash()
		if err := store.CommitWindow(ctx, window, projectHash, algorithm, sourceVersion, formatVersion); err != nil {
			_ = store.Fail(window.WindowID)
			return nil, err
		}
		result.Committed++
		newlyCommitted++
		finished++
		if options.CancelAfter >= 0 && finished >= options.CancelAfter {
			partial, loadErr := loadRunResult(store, copy, mode)
			if loadErr != nil {
				return partial, loadErr
			}
			partial.ReusedWindows = len(reused)
			partial.Committed = newlyCommitted
			return partial, windowContextError(context.Canceled)
		}
	}
	final, err := loadRunResult(store, copy, mode)
	if err != nil {
		return nil, err
	}
	final.ReusedWindows = len(reused)
	final.Committed = newlyCommitted
	return final, nil
}

func loadRunResult(store *Store, show *Show, mode RunMode) (*RunResult, error) {
	projectHash := ProjectHash(show)
	cached, err := store.ReadCommitted(projectHash, AlgorithmVersion, show.SourceVersion.String(), show.SourceVersion.String())
	if err != nil {
		return nil, err
	}
	windows := make([]WindowResult, 0, len(cached))
	for _, cache := range cached {
		windows = append(windows, cache.Result)
	}
	combined := combineWindows(windows)
	return &RunResult{
		Mode: mode, Frames: combined.Frames, Decisions: combined.Decisions,
		Consumption: combined.Consumption, Windows: combined.Windows,
	}, nil
}

// CompareRuns reports the first frame/fixture difference. Decision and
// consumption differences are also anchored to the first involved frame.
func CompareRuns(a, b *RunResult) *Mismatch {
	if a == nil || b == nil {
		return &Mismatch{Frame: -1, FixtureID: "", Kind: "missing run"}
	}
	frameCount := len(a.Frames)
	if len(b.Frames) < frameCount {
		frameCount = len(b.Frames)
	}
	for i := 0; i < frameCount; i++ {
		af, bf := a.Frames[i], b.Frames[i]
		if af.Frame != bf.Frame || af.AtUS != bf.AtUS || len(af.Lights) != len(bf.Lights) {
			return &Mismatch{Frame: af.Frame, Kind: "frame header"}
		}
		for j := range af.Lights {
			if af.Lights[j] != bf.Lights[j] {
				return &Mismatch{Frame: af.Frame, FixtureID: af.Lights[j].FixtureID, Kind: "output"}
			}
		}
	}
	if len(a.Frames) != len(b.Frames) {
		frame := 0
		if len(a.Frames) > 0 {
			frame = a.Frames[len(a.Frames)-1].Frame
		}
		return &Mismatch{Frame: frame, Kind: "frame count"}
	}
	decisionCount := len(a.Decisions)
	if len(b.Decisions) < decisionCount {
		decisionCount = len(b.Decisions)
	}
	for i := 0; i < decisionCount; i++ {
		if !decisionsEqual(a.Decisions[i], b.Decisions[i]) {
			return &Mismatch{Frame: a.Decisions[i].Frame, FixtureID: a.Decisions[i].FixtureID, Kind: "decision"}
		}
	}
	if len(a.Decisions) != len(b.Decisions) {
		frame := 0
		if len(a.Decisions) > 0 {
			frame = a.Decisions[len(a.Decisions)-1].Frame
		}
		return &Mismatch{Frame: frame, Kind: "decision count"}
	}
	if len(a.Consumption) != len(b.Consumption) {
		frame := 0
		if len(a.Consumption) > 0 {
			frame = int(a.Consumption[len(a.Consumption)-1].Frame)
		}
		return &Mismatch{Frame: frame, Kind: "consumption count"}
	}
	for i := range a.Consumption {
		if a.Consumption[i] != b.Consumption[i] {
			return &Mismatch{Frame: int(a.Consumption[i].Frame), Kind: "consumption"}
		}
	}
	return nil
}

func decisionsEqual(a, b Decision) bool {
	if a.Frame != b.Frame || a.AtUS != b.AtUS || a.FixtureID != b.FixtureID ||
		a.Parameter != b.Parameter || a.Basis != b.Basis || a.Winner != b.Winner ||
		len(a.Overridden) != len(b.Overridden) {
		return false
	}
	for i := range a.Overridden {
		if a.Overridden[i] != b.Overridden[i] {
			return false
		}
	}
	return true
}

func sortWindows(windows []WindowResult) {
	sort.Slice(windows, func(i, j int) bool { return windows[i].Index < windows[j].Index })
}
