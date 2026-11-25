package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"skillbudget"
)

type RunMode string

const (
	RunRealtime RunMode = "realtime"
	RunResume   RunMode = "resume"
	RunFull     RunMode = "full_recompute"
)

type RunConfig struct {
	RunID            string
	WindowSize       int64
	TotalTicks       int64
	Workers          int
	BufferLimit      int
	SampleEvery      int
	UseCache         bool
	CacheKeys        []string
	StopAfterWindows int
	MaxAttempts      int
	InjectFailure    bool
	FailureWindow    int
	FailureAttempts  int
	InjectPanic      bool
	PanicWindow      int
}

type WindowSummary struct {
	Index          int                   `json:"index"`
	StartTick      int64                 `json:"startTick"`
	EndTick        int64                 `json:"endTick"`
	Status         skillcore.WindowState `json:"status"`
	Attempt        int                   `json:"attempt"`
	Reused         bool                  `json:"reused"`
	CacheHit       bool                  `json:"cacheHit"`
	DecisionDigest string                `json:"decisionDigest"`
	Error          string                `json:"error,omitempty"`
}

type RunResult struct {
	Mode           RunMode                  `json:"mode"`
	RunID          string                   `json:"runId"`
	RuleHash       string                   `json:"ruleHash"`
	SourceVersion  int                      `json:"sourceVersion"`
	Algorithm      string                   `json:"algorithm"`
	Windows        []WindowSummary          `json:"windows"`
	Ticks          []skillcore.TickSnapshot `json:"ticks"`
	Events         []skillcore.WorkerEvent  `json:"events,omitempty"`
	ResumedTick    int64                    `json:"resumedTick"`
	ResumedWindows []int                    `json:"resumedWindows,omitempty"`
	CacheHits      []int                    `json:"cacheHits,omitempty"`
	Invalidated    bool                     `json:"invalidated"`
	Complete       bool                     `json:"complete"`
}

type Runner struct {
	store *Store
}

func NewRunner(store *Store) *Runner { return &Runner{store: store} }

func (r *Runner) Run(ctx context.Context, project *skillcore.Project, intents []skillcore.Intent, mode RunMode, cfg RunConfig) (*RunResult, error) {
	if project == nil {
		return nil, &skillcore.Error{Code: skillcore.CodeInput, Path: "project", Message: "nil project"}
	}
	if cfg.WindowSize <= 0 {
		return nil, &skillcore.Error{Code: skillcore.CodeInput, Path: "windowSize", Message: "must be positive"}
	}
	if cfg.TotalTicks < 0 {
		return nil, &skillcore.Error{Code: skillcore.CodeInput, Path: "totalTicks", Message: "must be non-negative"}
	}
	if cfg.SampleEvery == 0 {
		cfg.SampleEvery = 1
	}
	if cfg.SampleEvery != 1 {
		return nil, &skillcore.Error{
			Code: skillcore.CodeInput, Path: "sampleEvery",
			Message: "checkpoint runner requires per-tick sampling (sampleEvery=1)",
		}
	}
	if cfg.Workers == 0 {
		cfg.Workers = 1
	}
	if cfg.BufferLimit == 0 {
		cfg.BufferLimit = 1
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 1
	}
	if cfg.MaxAttempts < 1 {
		return nil, &skillcore.Error{Code: skillcore.CodeInput, Path: "maxAttempts", Message: "must be at least one"}
	}
	if cfg.StopAfterWindows < 0 {
		return nil, &skillcore.Error{Code: skillcore.CodeInput, Path: "stopAfterWindows", Message: "must be non-negative"}
	}
	engine, err := skillcore.NewEngine(project)
	if err != nil {
		return nil, err
	}
	windowCount := int((cfg.TotalTicks + cfg.WindowSize - 1) / cfg.WindowSize)
	if len(cfg.CacheKeys) > 0 && len(cfg.CacheKeys) != windowCount {
		return nil, &skillcore.Error{
			Code: skillcore.CodeInput, Path: "cacheKeys",
			Message: fmt.Sprintf("expected %d dependency keys, got %d", windowCount, len(cfg.CacheKeys)),
		}
	}
	runID := cfg.RunID
	if runID == "" {
		runID = "run-" + shortHash(project.ContentHash())
	}
	cfg.RunID = runID
	result := &RunResult{
		Mode: mode, RunID: runID, RuleHash: project.ContentHash(),
		SourceVersion: project.SourceVersion, Algorithm: skillcore.AlgorithmVersion,
		Complete: true,
	}
	if mode != RunRealtime && mode != RunResume && mode != RunFull {
		return nil, &skillcore.Error{Code: skillcore.CodeInput, Path: "mode", Message: fmt.Sprintf("unknown run mode %q", mode)}
	}
	if mode != RunFull && r.store == nil {
		return nil, &skillcore.Error{Code: skillcore.CodeInput, Path: "store", Message: "checkpoint store is required"}
	}

	var (
		state      = engine.InitialState()
		startIndex = 0
		useCache   = cfg.UseCache
	)
	if mode == RunFull {
		useCache = false
		result.ResumedTick = 0
	} else if mode == RunRealtime {
		if err := r.store.ResetRun(ctx, runID); err != nil {
			return nil, err
		}
		result.ResumedTick = 0
	} else {
		if _, err := r.store.RecoverInterrupted(ctx, runID); err != nil {
			return nil, err
		}
		resume, err := r.loadCommitted(ctx, engine, project, intents, cfg, windowCount)
		if err != nil {
			return nil, err
		}
		if resume.invalid {
			if err := r.store.ResetRun(ctx, runID); err != nil {
				return nil, err
			}
			result.Invalidated = true
			useCache = false
			result.ResumedTick = 0
		} else {
			state = resume.state
			startIndex = resume.committedWindows
			result.ResumedTick = state.NextTick
			result.ResumedWindows = append(result.ResumedWindows, resume.reusedIndexes...)
			result.Ticks = append(result.Ticks, resume.ticks...)
			result.Events = append(result.Events, resume.events...)
			result.Windows = append(result.Windows, resume.summaries...)
		}
	}
	if startIndex >= windowCount {
		result.Complete = true
		return result, nil
	}

	attemptByWindow := make(map[int]int)
	completedThisRun := 0
	for index := startIndex; index < windowCount; index++ {
		startTick := int64(index) * cfg.WindowSize
		endTick := startTick + cfg.WindowSize
		if endTick > cfg.TotalTicks {
			endTick = cfg.TotalTicks
		}
		var computation *windowComputation
		var lastErr error
		for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
			attemptByWindow[index]++
			computation, lastErr = r.processWindow(ctx, engine, project, intents, cfg, runID, index,
				startTick, endTick, state, useCache, attemptByWindow[index])
			if lastErr == nil {
				break
			}
		}
		if lastErr != nil {
			result.Complete = false
			return result, lastErr
		}
		state = computation.result.FinalState
		result.Ticks = append(result.Ticks, computation.result.Ticks...)
		result.Events = append(result.Events, computation.result.Events...)
		if computation.cacheHit {
			result.CacheHits = append(result.CacheHits, index)
		}
		result.Windows = append(result.Windows, WindowSummary{
			Index: index, StartTick: startTick, EndTick: endTick,
			Status: skillcore.WindowCommitted, Attempt: attemptByWindow[index],
			CacheHit: computation.cacheHit, DecisionDigest: computation.decisionDigest,
		})
		completedThisRun++
		if cfg.StopAfterWindows > 0 && completedThisRun >= cfg.StopAfterWindows {
			result.Complete = index == windowCount-1
			return result, nil
		}
	}
	result.Complete = true
	return result, nil
}

type windowComputation struct {
	result         *skillcore.SimResult
	decisionDigest string
	cacheHit       bool
}

func (r *Runner) processWindow(
	ctx context.Context,
	engine *skillcore.Engine,
	project *skillcore.Project,
	intents []skillcore.Intent,
	cfg RunConfig,
	runID string,
	index int,
	startTick, endTick int64,
	state *skillcore.RuntimeState,
	useCache bool,
	attempt int,
) (computation *windowComputation, err error) {
	stateBeforeJSON, err := marshalState(state)
	if err != nil {
		return nil, &skillcore.Error{Code: skillcore.CodeInternal, Path: "state", Message: "marshal state", Cause: err}
	}
	stateDigest := digestString(stateBeforeJSON)
	if r.store != nil {
		if err := r.store.EnsureWindow(ctx, runID, index, startTick, endTick); err != nil {
			return nil, err
		}
		if err := r.store.BeginWindow(ctx, BeginWindowOptions{
			RunID: runID, Index: index, StartTick: startTick, EndTick: endTick,
			RuleHash: project.ContentHash(), SourceVersion: project.SourceVersion,
			AlgorithmVersion: skillcore.AlgorithmVersion, StateBeforeJSON: stateBeforeJSON,
		}); err != nil {
			return nil, err
		}
	}
	fail := func(cause error) (*windowComputation, error) {
		if r.store != nil {
			if failErr := r.store.FailWindow(ctx, runID, index, cause.Error()); failErr != nil {
				return nil, &skillcore.Error{
					Code: skillcore.CodeState, Path: fmt.Sprintf("windows[%d]", index),
					Message: "window failed and its failure state could not be persisted", Cause: failErr,
				}
			}
		}
		return nil, &skillcore.Error{
			Code: skillcore.CodeWindow, Path: fmt.Sprintf("windows[%d]", index),
			Message: "window settlement failed", Cause: cause,
		}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			computation, err = fail(&skillcore.Error{
				Code: skillcore.CodeInternal, Path: "worker",
				Message: fmt.Sprintf("panic isolated in window %d: %v", index, recovered),
			})
		}
	}()
	attemptForWindow := attempt
	if cfg.InjectFailure && index == cfg.FailureWindow && attemptForWindow <= cfg.FailureAttempts {
		return fail(fmt.Errorf("injected failure attempt %d", attemptForWindow))
	}
	if cfg.InjectPanic && index == cfg.PanicWindow {
		panic("injected window panic")
	}

	var sim *skillcore.SimResult
	cacheKey := ""
	if useCache && r.store != nil {
		if len(cfg.CacheKeys) > 0 {
			cacheKey = cfg.CacheKeys[index]
		} else {
			cacheKey = skillcore.WindowDependencyKey(project, startTick, endTick, intents)
		}
		entry, ok, cacheErr := r.store.GetCachedWindow(ctx, cacheKey, stateDigest, skillcore.AlgorithmVersion)
		if cacheErr != nil {
			return fail(cacheErr)
		}
		if ok {
			var cached skillcore.SimResult
			if err := json.Unmarshal([]byte(entry.ResultJSON), &cached); err != nil {
				return fail(fmt.Errorf("decode cached result: %w", err))
			}
			if cached.EndTick != endTick || cached.FinalState == nil || cached.FinalState.NextTick != endTick {
				return fail(fmt.Errorf("cached result does not cover the requested window"))
			}
			sim = &cached
			computation = &windowComputation{
				result:         sim,
				decisionDigest: digestDecisions(sim.Ticks),
				cacheHit:       true,
			}
		}
	}
	if sim == nil {
		sim, err = engine.Run(state, intents, endTick, skillcore.SimOptions{
			SampleEvery: cfg.SampleEvery, Workers: cfg.Workers, BufferLimit: cfg.BufferLimit,
		})
		if err != nil {
			return fail(err)
		}
		computation = &windowComputation{
			result: sim, decisionDigest: digestDecisions(sim.Ticks),
		}
	}
	stateAfterJSON, err := marshalState(sim.FinalState)
	if err != nil {
		return fail(err)
	}
	resultJSON, err := json.Marshal(sim)
	if err != nil {
		return fail(err)
	}
	if r.store != nil {
		if err := r.store.CommitWindow(ctx, CommitWindowOptions{
			RunID: runID, Index: index, StateAfterJSON: stateAfterJSON,
			ResultJSON: string(resultJSON), DecisionDigest: computation.decisionDigest,
		}); err != nil {
			return nil, err
		}
		if useCache {
			if err := r.store.PutCachedWindow(ctx, CacheEntry{
				Key: cacheKey, StateDigest: stateDigest, Algorithm: skillcore.AlgorithmVersion,
				RuleHash: project.ContentHash(), ResultJSON: string(resultJSON), StateAfterJSON: stateAfterJSON,
			}); err != nil {
				return nil, err
			}
		}
	}
	return computation, nil
}

type resumeData struct {
	state            *skillcore.RuntimeState
	committedWindows int
	reusedIndexes    []int
	ticks            []skillcore.TickSnapshot
	events           []skillcore.WorkerEvent
	summaries        []WindowSummary
	invalid          bool
}

func (r *Runner) loadCommitted(
	ctx context.Context,
	engine *skillcore.Engine,
	project *skillcore.Project,
	intents []skillcore.Intent,
	cfg RunConfig,
	windowCount int,
) (*resumeData, error) {
	rows, err := r.store.LoadWindows(ctx, cfg.RunID)
	if err != nil {
		return nil, err
	}
	data := &resumeData{state: engine.InitialState()}
	if len(rows) == 0 {
		return data, nil
	}
	byIndex := make(map[int]StoredWindow, len(rows))
	for _, row := range rows {
		byIndex[row.Index] = row
	}
	firstGap := windowCount
	for index := 0; index < windowCount; index++ {
		row, ok := byIndex[index]
		if !ok || row.Status != skillcore.WindowCommitted {
			firstGap = index
			break
		}
	}
	for index := firstGap; index < windowCount; index++ {
		if row, ok := byIndex[index]; ok && row.Status == skillcore.WindowCommitted {
			data.invalid = true
			return data, nil
		}
	}
	expectedHash := project.ContentHash()
	var previousDigest string
	for index := 0; index < firstGap; index++ {
		row := byIndex[index]
		startTick := int64(index) * cfg.WindowSize
		endTick := startTick + cfg.WindowSize
		if endTick > cfg.TotalTicks {
			endTick = cfg.TotalTicks
		}
		if row.RuleHash != expectedHash || row.SourceVersion != project.SourceVersion ||
			row.AlgorithmVersion != skillcore.AlgorithmVersion ||
			row.StartTick != startTick || row.EndTick != endTick {
			data.invalid = true
			return data, nil
		}
		before, err := unmarshalState(row.StateBeforeJSON)
		if err != nil {
			data.invalid = true
			return data, nil
		}
		after, err := unmarshalState(row.StateAfterJSON)
		if err != nil {
			data.invalid = true
			return data, nil
		}
		if err := engine.ValidateState(before); err != nil {
			data.invalid = true
			return data, nil
		}
		if err := engine.ValidateState(after); err != nil || after.NextTick != endTick {
			data.invalid = true
			return data, nil
		}
		beforeJSON, _ := marshalState(before)
		beforeDigest := digestString(beforeJSON)
		if index == 0 {
			initialJSON, _ := marshalState(engine.InitialState())
			expectedDigest := digestString(initialJSON)
			if beforeDigest != expectedDigest {
				data.invalid = true
				return data, nil
			}
		} else if beforeDigest != previousDigest {
			data.invalid = true
			return data, nil
		}
		var sim skillcore.SimResult
		if err := json.Unmarshal([]byte(row.ResultJSON), &sim); err != nil {
			data.invalid = true
			return data, nil
		}
		if sim.EndTick != endTick || sim.FinalState == nil || sim.FinalState.NextTick != endTick ||
			digestDecisions(sim.Ticks) != row.DecisionDigest {
			data.invalid = true
			return data, nil
		}
		afterJSON, _ := marshalState(after)
		previousDigest = digestString(afterJSON)
		data.state = after
		data.committedWindows = index + 1
		data.reusedIndexes = append(data.reusedIndexes, index)
		data.ticks = append(data.ticks, sim.Ticks...)
		data.events = append(data.events, sim.Events...)
		data.summaries = append(data.summaries, WindowSummary{
			Index: index, StartTick: startTick, EndTick: endTick,
			Status: skillcore.WindowCommitted, Attempt: row.Attempt, Reused: true,
			DecisionDigest: row.DecisionDigest,
		})
	}
	return data, nil
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func digestDecisions(ticks []skillcore.TickSnapshot) string {
	var decisions []skillcore.Decision
	for _, tick := range ticks {
		decisions = append(decisions, tick.Decisions...)
	}
	return skillcore.DecisionDigest(decisions)
}

func shortHash(value string) string {
	if len(value) > 16 {
		return value[:16]
	}
	return value
}

// SortWindowSummaries is a deterministic helper for diagnostics and tests.
func SortWindowSummaries(items []WindowSummary) {
	sort.Slice(items, func(i, j int) bool { return items[i].Index < items[j].Index })
}
