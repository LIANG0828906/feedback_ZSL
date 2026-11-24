package syncgame

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

const (
	ModeRealtime     = "realtime"
	ModeReplay       = "replay"
	ModeRecovered    = "recovered"
	ModeFallbackFull = "full-replay-after-invalid-snapshot"
)

type RunOptions struct {
	Parallel      bool
	Workers       int
	PoolConfig    PoolConfig
	TierID        string
	ClientOptions ClientOptions
	Store         *Store
	RunID         string
	WindowTicks   int
	InterruptAt   int
}

type RunResult struct {
	Mode           string          `json:"mode"`
	Authority      *AuthorityRun   `json:"authority"`
	Clients        []*ClientRun    `json:"clients"`
	RollbackEvents []RollbackEvent `json:"rollback_events"`
	Settlement     Settlement      `json:"settlement"`
	Pool           *PoolStats      `json:"pool,omitempty"`
	Windows        []WindowStatus  `json:"windows,omitempty"`
	PersistedTick  int             `json:"persisted_tick"`
	RunID          string          `json:"run_id,omitempty"`
}

func RunRealtime(sample *Sample, opts RunOptions) (*RunResult, error) {
	engine, err := NewEngine(sample)
	if err != nil {
		return nil, err
	}
	state := engine.InitialState()
	states := []WorldState{CloneWorld(state)}
	limit := len(sample.Intents)
	if limit > sample.MaxTicks {
		limit = sample.MaxTicks
	}
	windowTicks := opts.WindowTicks
	if windowTicks <= 0 {
		windowTicks = 20
	}
	runID := opts.RunID
	if runID == "" {
		runID = "run-" + sample.TruthFingerprint()[:12]
	}
	lastSavedTick := 0
	windowIndex := 0
	for i := 0; i < limit; i++ {
		if state.Finished {
			break
		}
		intent := sample.Intents[i]
		if opts.Parallel {
			state, err = engine.StepParallel(state, intent, effectiveWorkers(opts.Workers))
		} else {
			state, err = engine.Step(state, intent)
		}
		if err != nil {
			return nil, err
		}
		states = append(states, CloneWorld(state))

		shouldPersist := opts.Store != nil &&
			(state.Tick%windowTicks == 0 || state.Finished || state.Tick == limit || state.Tick == opts.InterruptAt)
		if shouldPersist {
			authPrefix := &AuthorityRun{
				States:       cloneStates(states),
				InputOffset:  state.Tick,
				RandomCursor: state.Tick,
				Settlement:   engine.Settlement(state),
				Algorithm:    sample.AlgorithmVersion,
				StreamHash:   sample.StreamFingerprint(),
			}
			if err := saveCheckpoint(context.Background(), opts.Store, sample, runID, windowIndex,
				lastSavedTick, state.Tick, authPrefix); err != nil {
				return nil, err
			}
			lastSavedTick = state.Tick
			windowIndex++
		}
		if opts.InterruptAt > 0 && state.Tick >= opts.InterruptAt {
			break
		}
	}
	if opts.Store != nil && lastSavedTick < state.Tick {
		authPrefix := &AuthorityRun{
			States:       cloneStates(states),
			InputOffset:  state.Tick,
			RandomCursor: state.Tick,
			Settlement:   engine.Settlement(state),
			Algorithm:    sample.AlgorithmVersion,
			StreamHash:   sample.StreamFingerprint(),
		}
		if err := saveCheckpoint(context.Background(), opts.Store, sample, runID, windowIndex,
			lastSavedTick, state.Tick, authPrefix); err != nil {
			return nil, err
		}
		lastSavedTick = state.Tick
	}
	auth := &AuthorityRun{
		States:       states,
		InputOffset:  state.Tick,
		RandomCursor: state.Tick,
		Settlement:   engine.Settlement(state),
		Algorithm:    sample.AlgorithmVersion,
		StreamHash:   sample.StreamFingerprint(),
	}
	result, err := buildRunResult(ModeRealtime, sample, auth, opts)
	if err != nil {
		return nil, err
	}
	result.PersistedTick = lastSavedTick
	result.RunID = runID
	if opts.Store != nil {
		windows, windowsErr := opts.Store.Windows(context.Background(), runID)
		if windowsErr != nil {
			return nil, windowsErr
		}
		result.Windows = windows
	}
	return result, nil
}

func RunReplay(sample *Sample, opts RunOptions) (*RunResult, error) {
	auth, err := RunAuthoritative(sample, opts.Parallel, effectiveWorkers(opts.Workers))
	if err != nil {
		return nil, err
	}
	return buildRunResult(ModeReplay, sample, auth, opts)
}

func RunRecovered(sample *Sample, opts RunOptions) (*RunResult, error) {
	if opts.Store == nil {
		return nil, &GameError{Code: CodeSnapshot, Message: "recovery requires a checkpoint store"}
	}
	runID := opts.RunID
	if runID == "" {
		runID = "run-" + sample.TruthFingerprint()[:12]
	}
	checkpoint, err := opts.Store.LoadLatest(context.Background(), sample, runID)
	if err != nil {
		if errors.Is(err, ErrNoCheckpoint) {
			return RunReplay(sample, opts)
		}
		if IsCode(err, CodeSnapshot) {
			fallbackOpts := opts
			fallbackOpts.Store = nil
			result, replayErr := RunReplay(sample, fallbackOpts)
			if replayErr != nil {
				return nil, replayErr
			}
			result.Mode = ModeFallbackFull
			result.RunID = runID
			return result, nil
		}
		return nil, err
	}
	auth, err := ResumeAuthoritative(sample, checkpoint, opts.Parallel, effectiveWorkers(opts.Workers))
	if err != nil {
		return nil, err
	}
	result, err := buildRunResult(ModeRecovered, sample, auth, opts)
	if err != nil {
		return nil, err
	}
	result.PersistedTick = checkpoint.Tick
	result.RunID = runID
	windows, err := opts.Store.Windows(context.Background(), runID)
	if err != nil {
		return nil, err
	}
	result.Windows = windows
	return result, nil
}

func ResumeAuthoritative(sample *Sample, checkpoint *Checkpoint, parallel bool, workers int) (*AuthorityRun, error) {
	if checkpoint == nil {
		return nil, &GameError{Code: CodeSnapshot, Message: "checkpoint is nil"}
	}
	if err := checkpoint.Validate(); err != nil {
		return nil, err
	}
	if checkpoint.Seed != sample.Seed ||
		checkpoint.AlgorithmVersion != sample.AlgorithmVersion ||
		checkpoint.StreamFingerprint != sample.StreamFingerprint() {
		return nil, &GameError{
			Code:    CodeSnapshot,
			Message: "checkpoint identity does not match sample",
			Tick:    checkpoint.Tick,
			Field:   "run_identity",
		}
	}
	engine, err := NewEngine(sample)
	if err != nil {
		return nil, err
	}
	state := CloneWorld(checkpoint.State)
	states := cloneStates(checkpoint.Trace)
	if len(states) == 0 || states[len(states)-1].Tick != state.Tick {
		return nil, &GameError{Code: CodeSnapshot, Message: "checkpoint trace tail is invalid", Tick: state.Tick}
	}
	limit := len(sample.Intents)
	if limit > sample.MaxTicks {
		limit = sample.MaxTicks
	}
	for i := state.Tick; i < limit && !state.Finished; i++ {
		intent := sample.Intents[i]
		if parallel {
			state, err = engine.StepParallel(state, intent, workers)
		} else {
			state, err = engine.Step(state, intent)
		}
		if err != nil {
			return nil, err
		}
		states = append(states, CloneWorld(state))
	}
	return &AuthorityRun{
		States:       states,
		InputOffset:  state.Tick,
		RandomCursor: state.Tick,
		Settlement:   engine.Settlement(state),
		Algorithm:    sample.AlgorithmVersion,
		StreamHash:   sample.StreamFingerprint(),
	}, nil
}

func buildRunResult(mode string, sample *Sample, auth *AuthorityRun, opts RunOptions) (*RunResult, error) {
	clients, stats, err := simulateClients(sample, auth, opts)
	if err != nil {
		return nil, err
	}
	var events []RollbackEvent
	for _, client := range clients {
		events = append(events, client.RollbackEvents...)
	}
	SortRollbackEvents(events)
	return &RunResult{
		Mode:           mode,
		Authority:      auth,
		Clients:        clients,
		RollbackEvents: events,
		Settlement:     auth.Settlement,
		Pool:           stats,
	}, nil
}

func simulateClients(sample *Sample, auth *AuthorityRun, opts RunOptions) ([]*ClientRun, *PoolStats, error) {
	config := opts.PoolConfig
	if config.Workers <= 0 {
		config.Workers = effectiveWorkers(opts.Workers)
	}
	if config.Workers > len(sample.Clients) {
		config.Workers = len(sample.Clients)
	}
	if config.Workers < 1 {
		config.Workers = 1
	}
	if config.QueuePerWorker <= 0 {
		config.QueuePerWorker = 4
	}
	if config.StateBufferSize <= 0 {
		config.StateBufferSize = config.QueuePerWorker
	}
	pool := NewWorkerPool(config)
	defer pool.Close()

	names := make([]string, 0, len(sample.Clients))
	tasks := make([]func() (any, error), 0, len(sample.Clients))
	for _, client := range sample.Clients {
		client := client
		tierID := opts.TierID
		if tierID == "" {
			tierID = client.LatencyTier
		}
		names = append(names, "client:"+client.ID)
		tasks = append(tasks, func() (any, error) {
			run, err := SimulateClient(sample, auth, client.ID, tierID, opts.ClientOptions)
			if err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(run.Frames)
			if err != nil {
				return nil, err
			}
			run.TraceHash = hashBytes(encoded)
			return run, nil
		})
	}
	values, err := pool.SubmitBatch(context.Background(), names, tasks)
	if err != nil {
		return nil, nil, err
	}
	clients := make([]*ClientRun, len(values))
	for i, value := range values {
		run, ok := value.(*ClientRun)
		if !ok {
			return nil, nil, fmt.Errorf("worker returned %T, expected *ClientRun", value)
		}
		clients[i] = run
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].ClientID < clients[j].ClientID })
	stats := pool.Stats()
	return clients, &stats, nil
}

func saveCheckpoint(ctx context.Context, store *Store, sample *Sample, runID string, windowIndex, startTick, endTick int, auth *AuthorityRun) error {
	if store == nil {
		return nil
	}
	clients, _, err := simulateClients(sample, auth, RunOptions{})
	if err != nil {
		return err
	}
	var events []RollbackEvent
	for _, client := range clients {
		events = append(events, client.RollbackEvents...)
	}
	SortRollbackEvents(events)
	state := auth.States[len(auth.States)-1]
	checkpoint := Checkpoint{
		RunID:                runID,
		WindowIndex:          windowIndex,
		StartTick:            startTick,
		EndTick:              endTick,
		Tick:                 state.Tick,
		Seed:                 sample.Seed,
		AlgorithmVersion:     sample.AlgorithmVersion,
		StreamFingerprint:    sample.StreamFingerprint(),
		State:                CloneWorld(state),
		Trace:                cloneStates(auth.States),
		InputOffset:          endTick,
		RandomCursor:         endTick,
		ClientConfirmedFrame: endTick * sample.RenderFPS / sample.TickRate,
		RollbackEvents:       events,
	}
	checkpoint.Seal()
	_, err = store.SaveWindow(ctx, checkpoint)
	if err != nil {
		_ = store.RecordWindowFailure(ctx, runID, windowIndex, startTick, endTick, err)
		return err
	}
	return nil
}

type VerificationOptions struct {
	RunOptions
	StorePath string
	KeepStore bool
}

type VerificationReport struct {
	Realtime            *RunResult `json:"realtime"`
	Replay              *RunResult `json:"replay"`
	Recovered           *RunResult `json:"recovered"`
	RealtimeVsReplay    *GameError `json:"realtime_vs_replay,omitempty"`
	RealtimeVsRecovered *GameError `json:"realtime_vs_recovered,omitempty"`
	InterruptedTick     int        `json:"interrupted_tick"`
	RecoveredFromTick   int        `json:"recovered_from_tick"`
	StorePath           string     `json:"store_path,omitempty"`
	Passed              bool       `json:"passed"`
}

// VerifyThreePaths runs the same fixture through realtime, a fresh replay, and
// a restart from a persisted window. All comparisons are tick-by-tick.
func VerifyThreePaths(sample *Sample, opts VerificationOptions) (*VerificationReport, error) {
	realtimeOpts := opts.RunOptions
	realtimeOpts.Store = nil
	realtimeOpts.InterruptAt = 0
	realtime, err := RunRealtime(sample, realtimeOpts)
	if err != nil {
		return nil, err
	}
	replay, err := RunReplay(sample, realtimeOpts)
	if err != nil {
		return nil, err
	}

	storePath := opts.StorePath
	cleanupDir := ""
	if storePath == "" {
		dir, err := os.MkdirTemp("", "syncgame-verify-*")
		if err != nil {
			return nil, err
		}
		cleanupDir = dir
		storePath = filepath.Join(dir, "checkpoints.db")
	}
	if !opts.KeepStore && cleanupDir != "" {
		defer os.RemoveAll(cleanupDir)
	}
	store, err := OpenStore(storePath)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	windowTicks := realtimeOpts.WindowTicks
	if windowTicks <= 0 {
		windowTicks = 20
	}
	finalTick := realtime.Authority.States[len(realtime.Authority.States)-1].Tick
	interruptAt := (finalTick / 2 / windowTicks) * windowTicks
	if interruptAt < windowTicks {
		interruptAt = windowTicks
	}
	if interruptAt > finalTick {
		interruptAt = finalTick
	}
	runID := realtimeOpts.RunID
	if runID == "" {
		runID = "verify-" + sample.TruthFingerprint()[:12]
	}
	persistedOpts := realtimeOpts
	persistedOpts.Store = store
	persistedOpts.RunID = runID
	persistedOpts.InterruptAt = interruptAt
	if _, err := RunRealtime(sample, persistedOpts); err != nil {
		return nil, err
	}
	if err := store.Close(); err != nil {
		return nil, err
	}
	store, err = OpenStore(storePath)
	if err != nil {
		return nil, err
	}
	recoveredOpts := realtimeOpts
	recoveredOpts.Store = store
	recoveredOpts.RunID = runID
	recovered, err := RunRecovered(sample, recoveredOpts)
	if err != nil {
		return nil, err
	}
	checkpoint, err := store.LoadLatest(context.Background(), sample, runID)
	if err != nil {
		return nil, err
	}
	report := &VerificationReport{
		Realtime:            realtime,
		Replay:              replay,
		Recovered:           recovered,
		InterruptedTick:     interruptAt,
		RecoveredFromTick:   checkpoint.Tick,
		StorePath:           storePath,
		RealtimeVsReplay:    CompareRunResults(realtime, replay),
		RealtimeVsRecovered: CompareRunResults(realtime, recovered),
	}
	report.Passed = report.RealtimeVsReplay == nil && report.RealtimeVsRecovered == nil
	if !report.Passed {
		if report.RealtimeVsReplay != nil {
			return report, report.RealtimeVsReplay
		}
		return report, report.RealtimeVsRecovered
	}
	return report, nil
}

func CompareRunResults(left, right *RunResult) *GameError {
	if left == nil || right == nil {
		return &GameError{Code: CodeDiverge, Message: "run result is nil", Field: "run"}
	}
	if mismatch := CompareAuthority(left.Authority, right.Authority); mismatch != nil {
		return mismatch
	}
	leftClients := clientRunMap(left.Clients)
	rightClients := clientRunMap(right.Clients)
	if len(leftClients) != len(rightClients) {
		return &GameError{
			Code:    CodeDiverge,
			Message: "client count differs",
			Field:   "client_count",
			Left:    len(leftClients),
			Right:   len(rightClients),
		}
	}
	ids := make([]string, 0, len(leftClients))
	for id := range leftClients {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if mismatch := CompareClientRuns(leftClients[id], rightClients[id]); mismatch != nil {
			return mismatch
		}
	}
	if left.Settlement.Winner != right.Settlement.Winner ||
		left.Settlement.LockedTick != right.Settlement.LockedTick {
		return &GameError{
			Code:    CodeDiverge,
			Message: "settlement differs",
			Tick:    left.Settlement.LockedTick,
			Field:   "settlement",
			Left:    left.Settlement,
			Right:   right.Settlement,
		}
	}
	return nil
}

func clientRunMap(clients []*ClientRun) map[string]*ClientRun {
	m := make(map[string]*ClientRun, len(clients))
	for _, client := range clients {
		m[client.ClientID] = client
	}
	return m
}

func cloneStates(states []WorldState) []WorldState {
	out := make([]WorldState, len(states))
	for i := range states {
		out[i] = CloneWorld(states[i])
	}
	return out
}

func effectiveWorkers(workers int) int {
	if workers > MaxParallelism {
		workers = MaxParallelism
	}
	if workers > 0 {
		return workers
	}
	workers = runtime.GOMAXPROCS(0)
	if workers < 1 {
		return 1
	}
	if workers > MaxParallelism {
		workers = MaxParallelism
	}
	return workers
}
