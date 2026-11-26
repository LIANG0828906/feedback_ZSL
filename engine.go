package mirror

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type RunMode string

const (
	ModeRealtime RunMode = "realtime"
	ModeResume   RunMode = "resume"
	ModeReplay   RunMode = "replay"
)

type RunOptions struct {
	Mode       RunMode
	Checkpoint *CheckpointStore
	Resume     bool
	StopAfter  int
	Budget     BudgetConfig
}

func Run(ctx context.Context, session *Session, opts RunOptions) (*RunResult, error) {
	if session == nil {
		return nil, errf(CodeSchema, "session", "session is nil")
	}
	cp := cloneSession(session)
	if err := NormalizeSession(cp); err != nil {
		return nil, err
	}
	if opts.Mode == "" {
		opts.Mode = ModeRealtime
	}
	if opts.Budget.MaxConcurrency == 0 && opts.Budget.BufferPerWorker == 0 && opts.Budget.QueueBudget == 0 {
		opts.Budget = DefaultBudget()
	}
	if err := opts.Budget.Validate(); err != nil {
		return nil, err
	}
	if opts.Resume && opts.Checkpoint == nil {
		return nil, &Error{Code: CodeState, Path: "run.resume", Message: "resume requires a checkpoint store"}
	}
	sm := NewStateMachine(cp)
	if err := sm.TransitionSession(SessionNegotiating); err != nil {
		return nil, err
	}
	if err := sm.TransitionSession(SessionMirroring); err != nil {
		return nil, err
	}
	timeline := NewTimeline(cp)
	result := &RunResult{
		Mode: opts.Mode, SessionID: cp.ID, SourceVersion: cp.SourceVersion,
		AlgorithmVersion: cp.AlgorithmVersion,
	}
	screenLatest := map[string]ScreenCommit{}
	failedAt := map[string]int{}
	startWindow := 0
	if opts.Resume {
		state, rejected, err := opts.Checkpoint.RestoreLatest(ctx, cp.ID)
		if err != nil {
			return nil, err
		}
		result.Rejected = rejected
		if state != nil {
			if state.SourceVersion != cp.SourceVersion || state.AlgorithmVersion != cp.AlgorithmVersion {
				return nil, &Error{Code: CodeState, Path: "checkpoint.version",
					Message: "checkpoint source/algorithm version does not match loaded session"}
			}
			if err := timeline.RestoreWithGenerations(state.Endpoints, state.EventsConsumed, state.Generations); err != nil {
				return nil, err
			}
			result.Windows = append([]WindowCommit(nil), state.WindowHistory...)
			startWindow = len(result.Windows)
			for _, sc := range state.Screens {
				screenLatest[sc.ScreenID] = sc
				if sc.Failed {
					generation := 0
					for _, g := range state.Generations {
						if g.ScreenID == sc.ScreenID {
							generation = g.Generation
							break
						}
					}
					for _, ep := range state.Endpoints {
						if ep.ScreenID == sc.ScreenID {
							generation = ep.Generation
							break
						}
					}
					failedAt[sc.ScreenID] = generation
				}
			}
			sm, err = restoreStateMachine(cp, state)
			if err != nil {
				return nil, err
			}
			copyState := *state
			result.Checkpoint = &copyState
		}
	}
	processedThisRun := 0
	poolStats := &SchedulerStats{Config: opts.Budget}
	for i := startWindow; i < len(cp.Windows); i++ {
		if err := ctx.Err(); err != nil {
			return nil, &Error{Code: CodeState, Path: "run.context", Message: err.Error()}
		}
		if opts.StopAfter > 0 && processedThisRun >= opts.StopAfter {
			result.FinalState = sm.Snapshot()
			result.Normalize()
			attachSchedulerStats(&result.Scheduler, poolStats)
			return result, nil
		}
		window := cp.Windows[i]
		commit, stats, err := runWindow(ctx, cp, window, len(result.Windows), timeline, sm, failedAt, opts.Budget, screenLatest)
		if err != nil {
			return nil, err
		}
		mergeSchedulerStats(poolStats, stats)
		result.Windows = append(result.Windows, commit)
		screenLatest = updateLatestScreens(screenLatest, commit.Screens)
		processedThisRun++
		if opts.Checkpoint != nil {
			state := CheckpointState{
				SessionID: cp.ID, SourceVersion: cp.SourceVersion,
				AlgorithmVersion: cp.AlgorithmVersion,
				WindowSeq:        commit.Sequence, LastWindowID: commit.WindowID,
				EventsConsumed: commit.EventsConsumed,
				Endpoints:      commit.Endpoints,
				Generations:    timeline.Generations(),
				Screens:        latestScreenSlice(screenLatest),
				WindowHistory:  append([]WindowCommit(nil), result.Windows...),
			}
			state.ContentHash = ComputeCheckpointHash(state)
			if err := opts.Checkpoint.SaveWindow(ctx, state); err != nil {
				return nil, err
			}
			copyState := state
			result.Checkpoint = &copyState
		}
	}
	if err := sm.TransitionSession(SessionEnded); err != nil {
		return nil, err
	}
	result.FinalState = sm.Snapshot()
	result.ErrorReports = BuildWindowErrorReports(result.Windows)
	result.Arbitrations, result.Invalidations = flattenCommitDetails(result.Windows)
	result.Normalize()
	attachSchedulerStats(&result.Scheduler, poolStats)
	return result, nil
}

func runWindow(
	ctx context.Context,
	session *Session,
	window Window,
	sequence int,
	timeline *Timeline,
	sm *StateMachine,
	failedAt map[string]int,
	budget BudgetConfig,
	_ map[string]ScreenCommit,
) (WindowCommit, *SchedulerStats, error) {
	arbStart := len(timeline.Arbitrations())
	invStart := len(timeline.Invalidations())
	if err := timeline.AdvanceTo(window.StartTick); err != nil {
		return WindowCommit{}, nil, err
	}
	if err := sm.TransitionWindow(window.ID, WindowDistributing); err != nil {
		return WindowCommit{}, nil, err
	}
	eligible := timeline.EligibleScreens()
	filtered := make([]Screen, 0, len(eligible))
	active := map[string]Endpoint{}
	preSnapshot := timeline.Snapshot(window.ID, window.StartTick)
	for _, ep := range preSnapshot.Endpoints {
		active[ep.ScreenID] = ep
	}
	for _, sc := range eligible {
		if generation, failed := failedAt[sc.ID]; failed {
			if ep, ok := active[sc.ID]; ok && ep.Generation == generation {
				continue
			}
			delete(failedAt, sc.ID)
		}
		filtered = append(filtered, sc)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].ID < filtered[j].ID })
	resetPendingAndNegotiatingScreens(sm, preSnapshot)
	syncScreensForWork(sm, filtered)
	type taskResult struct {
		Screen  Screen
		Mapping Mapping
		Packets []Packet
	}
	tasks := make([]Task[taskResult], 0, len(filtered))
	for _, screen := range filtered {
		screen := screen
		tasks = append(tasks, Task[taskResult]{
			ID: fmt.Sprintf("window:%s/screen:%s", window.ID, screen.ID),
			Run: func(ctx context.Context) (taskResult, error) {
				mapping, err := MapScreen(screen, window)
				if err != nil {
					return taskResult{}, err
				}
				packets, err := DistributeScreen(screen, window, mapping)
				if err != nil {
					return taskResult{}, err
				}
				return taskResult{Screen: screen, Mapping: mapping, Packets: packets}, nil
			},
		})
	}
	results, taskErrs, stats, err := RunTasks(ctx, tasks, budget)
	if err != nil {
		return WindowCommit{}, nil, err
	}
	commits := make([]ScreenCommit, 0, len(results))
	successCount := 0
	for i, res := range results {
		if taskErrs[i] != nil {
			screenID := filtered[i].ID
			commits = append(commits, ScreenCommit{
				ScreenID: screenID, State: ScreenFailed, Failed: true,
				Failure: taskErrs[i].Error(), Packets: []Packet{},
			})
			failedAt[screenID] = active[screenID].Generation
			_ = sm.TransitionScreen(screenID, ScreenFailed)
			continue
		}
		successCount++
		commits = append(commits, ScreenCommit{
			ScreenID: res.Screen.ID, State: ScreenCommitted,
			Mapping: res.Mapping, Packets: res.Packets,
			MappingParams: mappingParams(res.Screen, window),
		})
	}
	if err := timeline.AdvanceTo(window.EndTick); err != nil {
		return WindowCommit{}, nil, err
	}
	postSnapshot := timeline.Snapshot(window.ID, window.EndTick)
	resetPendingAndNegotiatingScreens(sm, postSnapshot)
	for i := range commits {
		if inv, ok := timeline.InvalidationFor(commits[i].ScreenID, window.ID); ok {
			commits[i].Failed = true
			commits[i].State = ScreenFailed
			commits[i].Packets = []Packet{}
			commits[i].Failure = inv.Reason
			failedAt[commits[i].ScreenID] = active[commits[i].ScreenID].Generation
			_ = sm.TransitionScreen(commits[i].ScreenID, ScreenFailed)
		}
	}
	sort.Slice(commits, func(i, j int) bool { return commits[i].ScreenID < commits[j].ScreenID })
	successCount = 0
	for _, c := range commits {
		if !c.Failed {
			successCount++
		}
	}
	windowStatus := WindowCommitted
	if successCount <= 0 {
		windowStatus = WindowFailed
	}
	if err := sm.TransitionWindow(window.ID, windowStatus); err != nil {
		return WindowCommit{}, nil, err
	}
	allArb := timeline.Arbitrations()
	newArb := append([]ArbitrationRecord(nil), allArb[arbStart:]...)
	allInv := timeline.Invalidations()
	newInv := make([]WindowInvalidation, 0)
	for _, inv := range allInv[invStart:] {
		if inv.WindowID == window.ID {
			newInv = append(newInv, inv)
		}
	}
	commit := WindowCommit{
		Sequence: sequence, WindowIndex: window.Index, WindowID: window.ID,
		Status: windowStatus, StartTick: window.StartTick, EndTick: window.EndTick,
		Screens: commits, Endpoints: postSnapshot.Endpoints,
		Arbitrations: newArb, Invalidations: newInv,
		EventsConsumed: timeline.EventsConsumed(),
		SourceVersion:  session.SourceVersion, AlgorithmVersion: session.AlgorithmVersion,
		State: sm.Snapshot(),
	}
	commit.ContentHash = ComputeWindowCommitHash(commit)
	return commit, stats, nil
}

func mappingParams(screen Screen, window Window) string {
	ax, ay := window.Chroma.Align()
	return fmt.Sprintf("scale=%s;chroma=%s;origin=%d,%d;rounding=floor;chromaAlign=%dx%d",
		window.Scale, window.Chroma, screen.OriginX, screen.OriginY, ax, ay)
}

func syncScreensForWork(sm *StateMachine, screens []Screen) {
	for _, screen := range screens {
		state, ok := sm.ScreenState(screen.ID)
		if !ok {
			continue
		}
		if state == ScreenFailed {
			_ = sm.TransitionScreen(screen.ID, ScreenPending)
			state = ScreenPending
		}
		if state == ScreenPending {
			_ = sm.TransitionScreen(screen.ID, ScreenNegotiating)
			state = ScreenNegotiating
		}
		if state == ScreenNegotiating {
			_ = sm.TransitionScreen(screen.ID, ScreenCommitted)
		}
	}
}

func resetPendingAndNegotiatingScreens(sm *StateMachine, snapshot EndpointSnapshot) {
	for _, ep := range snapshot.Endpoints {
		state, ok := sm.ScreenState(ep.ScreenID)
		if !ok {
			continue
		}
		switch ep.Status {
		case ScreenPending:
			if state != ScreenPending {
				_ = sm.TransitionScreen(ep.ScreenID, ScreenPending)
			}
		case ScreenNegotiating:
			if state == ScreenPending {
				_ = sm.TransitionScreen(ep.ScreenID, ScreenNegotiating)
			}
		}
	}
}

func restoreStateMachine(session *Session, state *CheckpointState) (*StateMachine, error) {
	sm := NewStateMachine(session)
	if err := sm.TransitionSession(SessionNegotiating); err != nil {
		return nil, err
	}
	if err := sm.TransitionSession(SessionMirroring); err != nil {
		return nil, err
	}
	for _, sc := range state.Screens {
		switch sc.State {
		case ScreenCommitted:
			if err := sm.TransitionScreen(sc.ScreenID, ScreenNegotiating); err != nil {
				return nil, err
			}
			if err := sm.TransitionScreen(sc.ScreenID, ScreenCommitted); err != nil {
				return nil, err
			}
		case ScreenFailed:
			if err := sm.TransitionScreen(sc.ScreenID, ScreenFailed); err != nil {
				return nil, err
			}
		}
	}
	for _, wc := range state.WindowHistory {
		status := wc.Status
		if status == "" {
			status = WindowCommitted
		}
		if err := sm.TransitionWindow(wc.WindowID, WindowDistributing); err != nil {
			return nil, err
		}
		if err := sm.TransitionWindow(wc.WindowID, status); err != nil {
			return nil, err
		}
	}
	return sm, nil
}

func updateLatestScreens(current map[string]ScreenCommit, commits []ScreenCommit) map[string]ScreenCommit {
	for _, sc := range commits {
		current[sc.ScreenID] = sc
	}
	return current
}

func latestScreenSlice(current map[string]ScreenCommit) []ScreenCommit {
	ids := make([]string, 0, len(current))
	for id := range current {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ScreenCommit, 0, len(ids))
	for _, id := range ids {
		out = append(out, current[id])
	}
	return out
}

func flattenCommitDetails(windows []WindowCommit) ([]ArbitrationRecord, []WindowInvalidation) {
	arbs := []ArbitrationRecord{}
	invs := []WindowInvalidation{}
	sorted := append([]WindowCommit(nil), windows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Sequence < sorted[j].Sequence })
	for _, w := range sorted {
		arbs = append(arbs, w.Arbitrations...)
		invs = append(invs, w.Invalidations...)
	}
	return arbs, invs
}

func BuildWindowErrorReports(windows []WindowCommit) []WindowErrorReport {
	sorted := append([]WindowCommit(nil), windows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Sequence < sorted[j].Sequence })
	cumulative := map[string]ErrorBound{}
	reports := []WindowErrorReport{}
	for _, w := range sorted {
		screens := append([]ScreenCommit(nil), w.Screens...)
		sort.Slice(screens, func(i, j int) bool { return screens[i].ScreenID < screens[j].ScreenID })
		for _, sc := range screens {
			if sc.Failed || sc.Mapping.Output.Empty() {
				continue
			}
			bound := cumulative[sc.ScreenID]
			bound.Add(sc.Mapping.Errors)
			cumulative[sc.ScreenID] = bound
			reports = append(reports, WindowErrorReport{
				ScreenID: sc.ScreenID, WindowID: w.WindowID,
				Errors: sc.Mapping.Errors, Cumulative: bound,
			})
		}
	}
	sort.Slice(reports, func(i, j int) bool {
		if reports[i].WindowID != reports[j].WindowID {
			return reports[i].WindowID < reports[j].WindowID
		}
		return reports[i].ScreenID < reports[j].ScreenID
	})
	return reports
}

func mergeSchedulerStats(dst, src *SchedulerStats) {
	if src == nil {
		return
	}
	if src.MaxObservedWorkers > dst.MaxObservedWorkers {
		dst.MaxObservedWorkers = src.MaxObservedWorkers
	}
	if src.MaxObservedQueue > dst.MaxObservedQueue {
		dst.MaxObservedQueue = src.MaxObservedQueue
	}
	dst.QueueRecords = append(dst.QueueRecords, src.QueueRecords...)
}

func attachSchedulerStats(target **SchedulerStats, stats *SchedulerStats) {
	if stats == nil {
		return
	}
	cp := *stats
	*target = &cp
}

type ThreePathReport struct {
	SessionID string     `json:"sessionId"`
	Matched   bool       `json:"matched"`
	Realtime  *RunResult `json:"realtime,omitempty"`
	Resume    *RunResult `json:"resume,omitempty"`
	Replay    *RunResult `json:"replay,omitempty"`
	FirstDiff *Error     `json:"firstDiff,omitempty"`
}

// RunThreePaths executes realtime, interrupted+resume, and one-shot replay
// against the same session and compares the semantic result byte-for-byte.
func RunThreePaths(ctx context.Context, session *Session, dir string, budget BudgetConfig) (*ThreePathReport, error) {
	if dir == "" {
		return nil, errf(CodeState, "three_paths.dir", "working directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if budget.MaxConcurrency == 0 && budget.BufferPerWorker == 0 && budget.QueueBudget == 0 {
		budget = DefaultBudget()
	}
	report := &ThreePathReport{SessionID: session.ID}
	cleanDB := func(name string) string {
		path := filepath.Join(dir, name)
		_ = os.Remove(path)
		_ = os.Remove(path + "-wal")
		_ = os.Remove(path + "-shm")
		return path
	}
	livePath := cleanDB("realtime.db")
	liveStore, err := OpenCheckpoint(livePath)
	if err != nil {
		return nil, err
	}
	report.Realtime, err = Run(ctx, session, RunOptions{Mode: ModeRealtime, Checkpoint: liveStore, Budget: budget})
	_ = liveStore.Close()
	if err != nil {
		return report, err
	}
	partialN := len(session.Windows) / 2
	if partialN < 1 {
		partialN = 1
	}
	resumePath := cleanDB("resume.db")
	partialStore, err := OpenCheckpoint(resumePath)
	if err != nil {
		return nil, err
	}
	_, err = Run(ctx, session, RunOptions{Mode: ModeRealtime, Checkpoint: partialStore, StopAfter: partialN, Budget: budget})
	_ = partialStore.Close()
	if err != nil {
		return report, err
	}
	resumeStore, err := OpenCheckpoint(resumePath)
	if err != nil {
		return nil, err
	}
	report.Resume, err = Run(ctx, session, RunOptions{Mode: ModeResume, Checkpoint: resumeStore, Resume: true, Budget: budget})
	_ = resumeStore.Close()
	if err != nil {
		return report, err
	}
	report.Replay, err = Run(ctx, session, RunOptions{Mode: ModeReplay, Budget: budget})
	if err != nil {
		return report, err
	}
	if diff := CompareRunResults(report.Realtime, report.Resume); diff != nil {
		report.FirstDiff = diff.(*Error)
		return report, diff
	}
	if diff := CompareRunResults(report.Realtime, report.Replay); diff != nil {
		report.FirstDiff = diff.(*Error)
		return report, diff
	}
	report.Matched = true
	return report, nil
}
