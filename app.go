package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"stageworkbench/pkg/generator"
	"stageworkbench/pkg/stage"
)

type App struct {
	mu         sync.Mutex
	ctx        context.Context
	project    *stage.Project
	params     stage.Params
	evaluator  *stage.Evaluator
	dbPath     string
	workDir    string
	running    bool
	runCancel  context.CancelFunc
	lastRun    *stage.RunResult
	lastVerify *stage.ConsistencyReport
	truthDirty bool
	lastNotice string
	initErr    string
}

type AppRunSummary struct {
	Mode                  stage.RunMode        `json:"mode"`
	Decision              stage.ModeDecision   `json:"decision"`
	Progress              stage.Progress       `json:"progress"`
	Partial               bool                 `json:"partial"`
	CommitOrder           []int                `json:"commit_order"`
	BudgetLog             []stage.BudgetRecord `json:"budget_log"`
	Statuses              []stage.WindowStatus `json:"statuses"`
	CheckpointInvalidated bool                 `json:"checkpoint_invalidated"`
	InvalidationReason    string               `json:"invalidation_reason,omitempty"`
	StartedAt             time.Time            `json:"started_at"`
	FinishedAt            time.Time            `json:"finished_at"`
}

type AppState struct {
	Initialized    bool                     `json:"initialized"`
	Project        *stage.Project           `json:"project,omitempty"`
	Params         stage.Params             `json:"params"`
	Decision       stage.ModeDecision       `json:"decision"`
	Progress       stage.Progress           `json:"progress"`
	Windows        []stage.WindowStatus     `json:"windows"`
	BudgetLog      []stage.BudgetRecord     `json:"budget_log"`
	Arbitrations   []stage.Arbitration      `json:"arbitrations"`
	MergeSpans     []stage.MergeSpan        `json:"merge_spans"`
	LastRun        *AppRunSummary           `json:"last_run,omitempty"`
	LastVerify     *stage.ConsistencyReport `json:"last_verify,omitempty"`
	CheckpointPath string                   `json:"checkpoint_path"`
	TruthDirty     bool                     `json:"truth_dirty"`
	Running        bool                     `json:"running"`
	LastNotice     string                   `json:"last_notice,omitempty"`
	Error          string                   `json:"error,omitempty"`
}

type CuePatch struct {
	TimeMS       *int64           `json:"time_ms,omitempty"`
	LightID      *string          `json:"light_id,omitempty"`
	GroupID      *string          `json:"group_id,omitempty"`
	Values       *stage.CueValues `json:"values,omitempty"`
	TransitionMS *int64           `json:"transition_ms,omitempty"`
	Easing       *stage.Easing    `json:"easing,omitempty"`
	CurvePower   *float64         `json:"curve_power,omitempty"`
	Priority     *int             `json:"priority,omitempty"`
}

type GroupPatch struct {
	Members  []string `json:"members,omitempty"`
	ParentID *string  `json:"parent_id,omitempty"`
	Priority *int     `json:"priority,omitempty"`
}

type CueView struct {
	ID           string          `json:"id"`
	Label        string          `json:"label"`
	TimeMS       int64           `json:"time_ms"`
	LightID      string          `json:"light_id,omitempty"`
	GroupID      string          `json:"group_id,omitempty"`
	Kind         stage.CueKind   `json:"kind"`
	Values       stage.CueValues `json:"values"`
	TransitionMS int64           `json:"transition_ms"`
	Easing       stage.Easing    `json:"easing"`
	Priority     int             `json:"priority"`
}

func NewApp() *App {
	params := stage.DefaultParams()
	workDir := "."
	if dir, err := os.UserConfigDir(); err == nil {
		workDir = filepath.Join(dir, "StageCue")
	}
	_ = os.MkdirAll(workDir, 0o755)
	return &App{
		params:  params,
		workDir: workDir,
		dbPath:  filepath.Join(workDir, "workbench.db"),
	}
}

func (a *App) startup(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()
	if err := a.initializeLocked(); err != nil {
		a.mu.Lock()
		a.initErr = err.Error()
		a.mu.Unlock()
	}
}

func (a *App) initialize() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.initializeLocked()
}

func (a *App) initializeLocked() error {
	if a.project != nil {
		return nil
	}
	project, err := generator.Generate(generator.Options{Preset: "small", Seed: 42})
	if err != nil {
		return err
	}
	evaluator, err := stage.NewEvaluator(project, a.params)
	if err != nil {
		return err
	}
	a.project = project
	a.params = a.params.WithDefaults(project)
	a.evaluator = evaluator
	a.truthDirty = false
	a.lastNotice = ""
	return nil
}

func (a *App) Initialize() (AppState, error) {
	if err := a.initialize(); err != nil {
		return AppState{}, err
	}
	return a.GetState()
}

func (a *App) Generate(preset string, seed int64) (AppState, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return AppState{}, newAppError(stage.CodeState, "cannot generate while scheduling")
	}
	project, err := generator.Generate(generator.Options{Preset: preset, Seed: seed})
	if err != nil {
		a.mu.Unlock()
		return AppState{}, err
	}
	params := stage.DefaultParams()
	params.WindowMS = project.WindowMS
	evaluator, err := stage.NewEvaluator(project, params)
	if err != nil {
		a.mu.Unlock()
		return AppState{}, err
	}
	a.project = project
	a.params = params
	a.evaluator = evaluator
	a.lastRun = nil
	a.lastVerify = nil
	a.truthDirty = false
	a.lastNotice = ""
	err = a.resetCheckpointLocked()
	a.mu.Unlock()
	if err != nil {
		return AppState{}, err
	}
	return a.GetState()
}

func (a *App) LoadProject(path string) (AppState, error) {
	project, err := generator.LoadProject(path)
	if err != nil {
		return AppState{}, err
	}
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return AppState{}, newAppError(stage.CodeState, "cannot load while scheduling")
	}
	params := a.params.WithDefaults(project)
	params.WindowMS = project.WindowMS
	evaluator, err := stage.NewEvaluator(project, params)
	if err != nil {
		a.mu.Unlock()
		return AppState{}, err
	}
	a.project = project
	a.params = params
	a.evaluator = evaluator
	a.lastRun = nil
	a.lastVerify = nil
	a.truthDirty = false
	a.lastNotice = ""
	err = a.resetCheckpointLocked()
	a.mu.Unlock()
	if err != nil {
		return AppState{}, err
	}
	return a.GetState()
}

func (a *App) SaveProject(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.project == nil {
		return newAppError(stage.CodeState, "no project loaded")
	}
	return generator.SaveProject(path, a.project)
}

func (a *App) GetState() (AppState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stateLocked()
}

func (a *App) stateLocked() (AppState, error) {
	state := AppState{
		Initialized:    a.project != nil,
		Params:         a.params,
		CheckpointPath: a.dbPath,
		TruthDirty:     a.truthDirty,
		Running:        a.running,
		LastNotice:     a.lastNotice,
		Error:          a.initErr,
	}
	if a.project == nil {
		return state, nil
	}
	state.Project = clone(a.project)
	state.Decision = stage.SelectMode(a.project, a.params)
	specs := stage.BuildWindowSpecs(a.project, a.params)
	if a.lastRun != nil {
		state.Progress = a.lastRun.Progress
		state.BudgetLog = append([]stage.BudgetRecord(nil), a.lastRun.BudgetLog...)
		state.Windows = append([]stage.WindowStatus(nil), a.lastRun.Statuses...)
		state.LastRun = summarizeRun(*a.lastRun)
		for _, window := range a.lastRun.Windows {
			state.Arbitrations = append(state.Arbitrations, window.Decisions...)
			if window.Merge != nil {
				state.MergeSpans = append(state.MergeSpans, window.Merge.Spans...)
			}
		}
		if len(state.Arbitrations) > 300 {
			state.Arbitrations = state.Arbitrations[:300]
		}
		if len(state.MergeSpans) > 300 {
			state.MergeSpans = state.MergeSpans[:300]
		}
	} else {
		state.Windows = make([]stage.WindowStatus, len(specs))
		for index, spec := range specs {
			state.Windows[index] = stage.WindowStatus{
				Index: index, StartUS: spec.StartUS, EndUS: spec.EndUS, State: stage.WindowPending,
				CueStart: spec.CueStart, CueOffset: spec.CueOffset, ConsumedCues: spec.CueOffset - spec.CueStart,
			}
		}
	}
	if a.lastVerify != nil {
		verify := *a.lastVerify
		state.LastVerify = &verify
	}
	return state, nil
}

func (a *App) UpdateParams(params stage.Params) (AppState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return AppState{}, newAppError(stage.CodeState, "cannot change parameters while scheduling")
	}
	params = params.WithDefaults(a.project)
	if err := params.Validate(); err != nil {
		return AppState{}, err
	}
	a.params = params
	if a.project != nil {
		evaluator, err := stage.NewEvaluator(a.project, params)
		if err != nil {
			return AppState{}, err
		}
		a.evaluator = evaluator
	}
	a.lastRun = nil
	a.lastVerify = nil
	a.lastNotice = ""
	return a.stateLocked()
}

func (a *App) Plan() (stage.ModeDecision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.project == nil {
		return stage.ModeDecision{}, newAppError(stage.CodeState, "no project loaded")
	}
	return stage.SelectMode(a.project, a.params), nil
}

func (a *App) Run(mode string, stopAfter int) (AppState, error) {
	return a.run(stage.RunMode(mode), stopAfter)
}

func (a *App) Resume() (AppState, error) {
	return a.run(stage.RunResume, 0)
}

func (a *App) FullRecompute() (AppState, error) {
	return a.run(stage.RunFull, 0)
}

func (a *App) run(mode stage.RunMode, stopAfter int) (AppState, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return AppState{}, newAppError(stage.CodeState, "scheduling is already running")
	}
	if a.project == nil {
		if err := a.initializeLocked(); err != nil {
			a.mu.Unlock()
			return AppState{}, err
		}
	}
	runner, err := stage.NewRunner(a.project, a.params)
	if err != nil {
		a.mu.Unlock()
		return AppState{}, err
	}
	parent := a.ctx
	if parent == nil {
		parent = context.Background()
	}
	runCtx, cancel := context.WithCancel(parent)
	a.running = true
	a.runCancel = cancel
	project := a.project
	params := a.params
	dbPath := a.dbPath
	skipTruth := a.truthDirty
	a.mu.Unlock()

	runResult, runErr := runner.Run(runCtx, stage.RunOptions{
		Mode:           mode,
		CheckpointPath: dbPath,
		StopAfter:      stopAfter,
		SkipTruthCheck: skipTruth,
	})

	a.mu.Lock()
	a.running = false
	a.runCancel = nil
	a.lastRun = &runResult
	evaluator, evalErr := stage.NewEvaluator(project, params)
	if evalErr == nil {
		a.evaluator = evaluator
	}
	a.mu.Unlock()
	if runErr != nil {
		state, stateErr := a.GetState()
		if stateErr != nil {
			return state, stateErr
		}
		return state, runErr
	}
	return a.GetState()
}

func (a *App) Pause() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.runCancel == nil {
		return newAppError(stage.CodeState, "no scheduling run is active")
	}
	a.runCancel()
	return nil
}

func (a *App) Verify() (stage.ConsistencyReport, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return stage.ConsistencyReport{}, newAppError(stage.CodeState, "cannot verify while scheduling")
	}
	if a.project == nil {
		a.mu.Unlock()
		return stage.ConsistencyReport{}, newAppError(stage.CodeState, "no project loaded")
	}
	project := clone(a.project)
	params := a.params
	skipTruth := a.truthDirty
	workDir := filepath.Join(a.workDir, fmt.Sprintf("verify-%d", time.Now().UnixNano()))
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Unlock()
	report, err := stage.VerifyThreePathsWithOptions(ctx, project, params, workDir, skipTruth)
	a.mu.Lock()
	a.lastVerify = &report
	a.mu.Unlock()
	if err != nil {
		return report, err
	}
	return report, nil
}

func (a *App) FrameAt(frame int) (stage.FrameOutput, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.project == nil || a.evaluator == nil {
		return stage.FrameOutput{}, newAppError(stage.CodeState, "no project loaded")
	}
	if frame < 0 || frame >= stage.FrameCount(a.project.DurationMS, a.project.FPS) {
		return stage.FrameOutput{}, newAppError(stage.CodeParameter, "frame is outside the project")
	}
	return a.evaluator.Frame(frame), nil
}

func (a *App) Timeline(offset, limit int) ([]CueView, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.project == nil {
		return nil, newAppError(stage.CodeState, "no project loaded")
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	end := offset + limit
	if end > len(a.project.Cues) {
		end = len(a.project.Cues)
	}
	if offset > len(a.project.Cues) {
		return []CueView{}, nil
	}
	out := make([]CueView, 0, end-offset)
	for _, cue := range a.project.Cues[offset:end] {
		out = append(out, CueView{
			ID: cue.ID, Label: cue.Label, TimeMS: cue.TimeMS,
			LightID: cue.LightID, GroupID: cue.GroupID, Kind: cue.Kind,
			Values: cue.Values, TransitionMS: cue.TransitionMS,
			Easing: cue.Easing, Priority: cue.Priority,
		})
	}
	return out, nil
}

func (a *App) WindowDetails(windowIndex int) (stage.WindowResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastRun == nil {
		return stage.WindowResult{}, newAppError(stage.CodeState, "no committed run is available")
	}
	for _, window := range a.lastRun.Windows {
		if window.Index == windowIndex {
			return window, nil
		}
	}
	return stage.WindowResult{}, newAppError(stage.CodeState, "window is not committed")
}

func (a *App) EditCue(cueID string, patch CuePatch) (AppState, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return AppState{}, newAppError(stage.CodeState, "cannot edit while scheduling")
	}
	if a.project == nil {
		a.mu.Unlock()
		return AppState{}, newAppError(stage.CodeState, "no project loaded")
	}
	committedEnd := committedEndMS(a.lastRun)
	oldProject := a.project
	oldEvaluator := a.evaluator
	oldTruthDirty := a.truthDirty
	oldLastRun := a.lastRun
	oldLastVerify := a.lastVerify
	project := clone(a.project)
	oldHash := stage.ContentHash(a.project)
	oldParamVersion := stage.ParamVersion(a.params)
	index := -1
	for i := range project.Cues {
		if project.Cues[i].ID == cueID {
			index = i
			break
		}
	}
	if index < 0 {
		a.mu.Unlock()
		return AppState{}, newAppError(stage.CodeReference, "cue does not exist")
	}
	cue := &project.Cues[index]
	if cue.TimeMS*1000 < committedEnd {
		a.mu.Unlock()
		return AppState{}, newAppError(stage.CodeCommitted, "cue belongs to a committed window; reset the project before editing it")
	}
	if patch.TimeMS != nil {
		if *patch.TimeMS*1000 < committedEnd {
			a.mu.Unlock()
			return AppState{}, newAppError(stage.CodeCommitted, "edited cue would enter a committed window")
		}
		cue.TimeMS = *patch.TimeMS
	}
	if patch.LightID != nil {
		cue.LightID = *patch.LightID
		cue.GroupID = ""
	}
	if patch.GroupID != nil {
		cue.GroupID = *patch.GroupID
		cue.LightID = ""
	}
	if patch.Values != nil {
		cue.Values = clone(*patch.Values)
	}
	if patch.TransitionMS != nil {
		cue.TransitionMS = *patch.TransitionMS
	}
	if patch.Easing != nil {
		cue.Easing = *patch.Easing
	}
	if patch.CurvePower != nil {
		cue.CurvePower = *patch.CurvePower
	}
	if patch.Priority != nil {
		cue.Priority = *patch.Priority
	}
	sortCues(project.Cues)
	if err := stage.ValidateProject(project, stage.ValidateOptions{CheckTruth: false, CheckConflict: true}); err != nil {
		a.mu.Unlock()
		return AppState{}, err
	}
	evaluator, err := stage.NewEvaluator(project, a.params)
	if err != nil {
		a.mu.Unlock()
		return AppState{}, err
	}
	a.project = project
	a.evaluator = evaluator
	a.truthDirty = true
	a.lastVerify = nil
	rebased := 0
	if committedEnd > 0 {
		rebased, err = a.rebaseCheckpointLocked(project, oldHash, oldParamVersion)
		if err == nil {
			a.lastNotice = fmt.Sprintf("已保留 %d 个已提交窗口并原子重定基；下一次断点续跑从后续窗口开始。", rebased)
		} else {
			a.project = oldProject
			a.evaluator = oldEvaluator
			a.truthDirty = oldTruthDirty
			a.lastRun = oldLastRun
			a.lastVerify = oldLastVerify
		}
	} else {
		a.lastRun = nil
		err = a.resetCheckpointLocked()
		if err == nil {
			a.lastNotice = "工程内容已变更；旧检查点已显式作废。"
		}
	}
	a.mu.Unlock()
	if err != nil {
		return AppState{}, err
	}
	return a.GetState()
}

func (a *App) EditGroup(groupID string, patch GroupPatch) (AppState, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return AppState{}, newAppError(stage.CodeState, "cannot edit while scheduling")
	}
	if a.project == nil {
		a.mu.Unlock()
		return AppState{}, newAppError(stage.CodeState, "no project loaded")
	}
	committedEnd := committedEndMS(a.lastRun)
	oldProject := a.project
	oldEvaluator := a.evaluator
	oldTruthDirty := a.truthDirty
	oldLastRun := a.lastRun
	oldLastVerify := a.lastVerify
	project := clone(a.project)
	oldHash := stage.ContentHash(a.project)
	oldParamVersion := stage.ParamVersion(a.params)
	index := -1
	for i := range project.Groups {
		if project.Groups[i].ID == groupID {
			index = i
			break
		}
	}
	if index < 0 {
		a.mu.Unlock()
		return AppState{}, newAppError(stage.CodeReference, "group does not exist")
	}
	group := &project.Groups[index]
	if patch.Members != nil {
		group.Members = append([]string(nil), patch.Members...)
	}
	if patch.ParentID != nil {
		group.ParentID = *patch.ParentID
	}
	if patch.Priority != nil {
		group.Priority = *patch.Priority
	}
	if committedEnd > 0 && (groupEditTouchesCommitted(a.project, groupID, committedEnd*1000) ||
		groupEditTouchesCommitted(project, groupID, committedEnd*1000)) {
		a.mu.Unlock()
		return AppState{}, newAppError(stage.CodeCommitted, "group edit can affect a committed group cue; reset the project first")
	}
	if err := stage.ValidateProject(project, stage.ValidateOptions{CheckTruth: false, CheckConflict: true}); err != nil {
		a.mu.Unlock()
		return AppState{}, err
	}
	evaluator, err := stage.NewEvaluator(project, a.params)
	if err != nil {
		a.mu.Unlock()
		return AppState{}, err
	}
	a.project = project
	a.evaluator = evaluator
	a.truthDirty = true
	a.lastVerify = nil
	rebased := 0
	if committedEnd > 0 {
		rebased, err = a.rebaseCheckpointLocked(project, oldHash, oldParamVersion)
		if err == nil {
			a.lastNotice = fmt.Sprintf("分组编辑不影响已提交区间；已原子重定基 %d 个窗口。", rebased)
		} else {
			a.project = oldProject
			a.evaluator = oldEvaluator
			a.truthDirty = oldTruthDirty
			a.lastRun = oldLastRun
			a.lastVerify = oldLastVerify
		}
	} else {
		a.lastRun = nil
		err = a.resetCheckpointLocked()
		if err == nil {
			a.lastNotice = "分组内容已变更；旧检查点已显式作废。"
		}
	}
	a.mu.Unlock()
	if err != nil {
		return AppState{}, err
	}
	return a.GetState()
}

func (a *App) resetCheckpointLocked() error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(a.dbPath + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func committedEndMS(result *stage.RunResult) int64 {
	if result == nil {
		return 0
	}
	var end int64
	for _, window := range result.Windows {
		if window.State == stage.WindowCommitted {
			if ms := window.EndUS / 1000; ms > end {
				end = ms
			}
		}
	}
	return end
}

func (a *App) rebaseCheckpointLocked(project *stage.Project, oldHash, oldParamVersion string) (int, error) {
	specs := stage.BuildWindowSpecs(project, a.params)
	newHash := stage.ContentHash(project)
	newParamVersion := stage.ParamVersion(a.params)
	store, err := stage.OpenCheckpoint(a.dbPath)
	if err != nil {
		return 0, err
	}
	defer store.Close()
	count, err := store.RebaseProject(project.ID, oldHash, newHash, oldParamVersion, newParamVersion, stage.AlgorithmVersion, len(specs))
	if err != nil {
		return 0, err
	}
	if a.lastRun != nil {
		for i := range a.lastRun.Windows {
			a.lastRun.Windows[i].ContentHash = newHash
			a.lastRun.Windows[i].ParamVersion = newParamVersion
			a.lastRun.Windows[i].AlgorithmVersion = stage.AlgorithmVersion
		}
	}
	return count, nil
}

func groupEditTouchesCommitted(project *stage.Project, groupID string, endUS int64) bool {
	affected := map[string]bool{groupID: true}
	changed := true
	for changed {
		changed = false
		for _, group := range project.Groups {
			if group.ParentID != "" && affected[group.ParentID] && !affected[group.ID] {
				affected[group.ID] = true
				changed = true
			}
			if affected[group.ID] && group.ParentID != "" && !affected[group.ParentID] {
				affected[group.ParentID] = true
				changed = true
			}
		}
	}
	for _, cue := range project.Cues {
		if cue.GroupID != "" && affected[cue.GroupID] && cue.TimeMS*1000 < endUS {
			return true
		}
	}
	return false
}

func summarizeRun(result stage.RunResult) *AppRunSummary {
	return &AppRunSummary{
		Mode: result.Mode, Decision: result.Decision, Progress: result.Progress,
		Partial: result.Partial, CommitOrder: append([]int(nil), result.CommitOrder...),
		BudgetLog:             append([]stage.BudgetRecord(nil), result.BudgetLog...),
		Statuses:              append([]stage.WindowStatus(nil), result.Statuses...),
		CheckpointInvalidated: result.CheckpointInvalidated,
		InvalidationReason:    result.InvalidationReason,
		StartedAt:             result.StartedAt, FinishedAt: result.FinishedAt,
	}
}

func newAppError(code stage.ErrorCode, message string) *stage.DomainError {
	return &stage.DomainError{Code: code, Message: message, Object: "app"}
}

func clone[T any](value T) T {
	raw, _ := json.Marshal(value)
	var out T
	_ = json.Unmarshal(raw, &out)
	return out
}

func sortCues(cues []stage.Cue) {
	sort.Slice(cues, func(i, j int) bool {
		if cues[i].TimeMS != cues[j].TimeMS {
			return cues[i].TimeMS < cues[j].TimeMS
		}
		return cues[i].ID < cues[j].ID
	})
}
