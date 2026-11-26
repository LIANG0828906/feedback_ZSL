package engine

import (
	"context"
	"fmt"
	"sort"

	"screenrelay/internal/checkpoint"
	"screenrelay/internal/dispatch"
	"screenrelay/internal/mapping"
	"screenrelay/internal/model"
	"screenrelay/internal/statemachine"
	"screenrelay/internal/topology"
)

type Options struct {
	Budget    BudgetConfig
	Store     *checkpoint.Store
	StopAfter int
}

func ValidateProject(p *model.Project) error {
	if p == nil {
		return model.NewError(model.ESchema, "nil project")
	}
	if len(p.Windows) == 0 {
		return model.SchemaError("$.windows", "at least one window is required")
	}
	for i, screen := range p.Screens {
		if err := mapping.ValidateCapabilities(screen); err != nil {
			return err
		}
		if i > 0 && p.Screens[i-1].ID == screen.ID {
			return model.NewError(model.ESchema, "duplicate initial screen %s", screen.ID)
		}
	}
	for _, event := range p.Events {
		if event.Screen != nil && (event.Kind == model.EventJoin || event.Kind == model.EventTopology) {
			if err := mapping.ValidateCapabilities(*event.Screen); err != nil {
				return err
			}
		}
	}
	for i := 1; i < len(p.Windows); i++ {
		if p.Windows[i].Tick < p.Windows[i-1].Tick {
			return &model.Error{
				Code: model.EOrder, Field: fmt.Sprintf("windows[%d].tick", i),
				Path: fmt.Sprintf("$.windows[%d].tick", i), Message: "window instruction time goes backwards",
				OpA: fmt.Sprint(p.Windows[i].Tick), OpB: fmt.Sprint(p.Windows[i-1].Tick),
				Details: map[string]any{"window_id": p.Windows[i].ID, "previous_window_id": p.Windows[i-1].ID},
			}
		}
		if p.Windows[i].Seq < p.Windows[i-1].Seq {
			return &model.Error{
				Code: model.EOrder, Field: fmt.Sprintf("windows[%d].seq", i),
				Path: fmt.Sprintf("$.windows[%d].seq", i), Message: "window sequence goes backwards",
				OpA: fmt.Sprint(p.Windows[i].Seq), OpB: fmt.Sprint(p.Windows[i-1].Seq),
			}
		}
	}
	for i := 1; i < len(p.Events); i++ {
		if p.Events[i].Tick < p.Events[i-1].Tick {
			return &model.Error{
				Code: model.EOrder, Field: fmt.Sprintf("events[%d].tick", i),
				Path: fmt.Sprintf("$.events[%d].tick", i), Message: "topology event time goes backwards",
				OpA: fmt.Sprint(p.Events[i].Tick), OpB: fmt.Sprint(p.Events[i-1].Tick),
				Details: map[string]any{"event_id": p.Events[i].ID, "previous_event_id": p.Events[i-1].ID},
			}
		}
	}
	return nil
}

func newPersistentState(p *model.Project) *PersistentState {
	ids := []string{}
	screens := map[string]model.Screen{}
	for _, screen := range p.Screens {
		screens[screen.ID] = screen
		ids = append(ids, screen.ID)
	}
	return &PersistentState{
		Result: RunResult{
			SessionID: p.SessionID, Seed: p.Seed, SourceVersion: p.SourceVersion,
			SchemaMajor: p.SchemaMajor, AlgorithmVersion: model.AlgorithmVersion,
			Windows: []WindowResult{}, Decisions: []topology.Decision{},
			CommitSequence: []int{}, FinalScreenIDs: ids, SessionState: string(statemachine.SessionCasting),
		},
		Screens: screens, AppliedEventIDs: map[string]bool{}, CompletedWindows: 0,
	}
}

func Run(ctx context.Context, p *model.Project, opts Options) (*RunResult, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	state := newPersistentState(p)
	return runFromState(ctx, p, opts, state, "")
}

func Resume(ctx context.Context, p *model.Project, store *checkpoint.Store, opts Options) (*RunResult, checkpoint.Recovery, error) {
	if err := ValidateProject(p); err != nil {
		return nil, checkpoint.Recovery{}, err
	}
	state := &PersistentState{}
	recovery, err := store.LoadPayloadVersions(model.AlgorithmVersion, p.SourceVersion, state)
	if err != nil {
		return nil, recovery, err
	}
	if recovery.InvalidSeq != 0 && recovery.Record == nil {
		return nil, recovery, model.NewError(model.EDecode, "checkpoint has no recoverable committed prefix: seq=%d reason=%s", recovery.InvalidSeq, recovery.InvalidReason)
	}
	if recovery.Record == nil {
		fresh := newPersistentState(p)
		result, err := runFromState(ctx, p, opts, fresh, "")
		return result, recovery, err
	}
	if state.Screens == nil || state.AppliedEventIDs == nil || state.Result.AlgorithmVersion == "" {
		return nil, recovery, model.NewError(model.EDecode, "checkpoint payload %d is incomplete", recovery.Record.WindowSeq)
	}
	if state.Result.SessionID != p.SessionID {
		return nil, recovery, model.NewError(model.EDecode, "checkpoint session %q does not match project %q", state.Result.SessionID, p.SessionID)
	}
	result, err := runFromState(ctx, p, opts, state, recovery.Record.ContentHash)
	return result, recovery, err
}

func runFromState(ctx context.Context, p *model.Project, opts Options, state *PersistentState, prevHash string) (*RunResult, error) {
	budget := opts.Budget
	if budget.MaxParallel <= 0 {
		budget = DefaultBudget()
	}
	session := statemachine.NewSession()
	if err := session.Transition(statemachine.SessionNegotiating); err != nil {
		return nil, err
	}
	if err := session.Transition(statemachine.SessionCasting); err != nil {
		return nil, err
	}
	screenMachines := map[string]*statemachine.ScreenMachine{}
	timeQuantum := timeQuantumFor(p.SchemaMajor)

	for windowIndex := state.CompletedWindows; windowIndex < len(p.Windows); windowIndex++ {
		if opts.StopAfter > 0 && state.CompletedWindows >= opts.StopAfter {
			break
		}
		window := p.Windows[windowIndex]
		wm := statemachine.NewWindow(window.Seq)
		if err := wm.Transition(statemachine.WindowDistributing); err != nil {
			return &state.Result, err
		}

		pending := topology.PendingThrough(p.Events, window.Tick, state.AppliedEventIDs)
		decisions, winners, err := topology.Adjudicate(pending)
		if err != nil {
			_ = wm.Transition(statemachine.WindowFailed)
			failed := failedWindow(window, wm.Current, decisions, nil, budget, err)
			state.Result.Windows = append(state.Result.Windows, failed)
			return &state.Result, err
		}
		for _, decision := range decisions {
			state.Result.Decisions = append(state.Result.Decisions, decision)
		}
		joined := map[string]bool{}
		for _, winner := range winners {
			if err := state.applyWinner(winner); err != nil {
				_ = wm.Transition(statemachine.WindowFailed)
				return &state.Result, err
			}
			if winner.Kind == model.EventJoin {
				joined[winner.ScreenID] = true
			}
		}
		topology.MarkApplied(state.AppliedEventIDs, pending...)

		activeIDs := sortedMapKeys(state.Screens)
		replays := map[string]*ReplayBundle{}
		if len(state.Result.Windows) > 0 {
			lastCommitted := state.Result.Windows[len(state.Result.Windows)-1]
			for joinedID := range joined {
				replay, replayErr := buildJoinReplay(p, lastCommitted.Seq, state.Screens[joinedID], window.Seq, timeQuantum)
				if replayErr != nil {
					_ = wm.Transition(statemachine.WindowFailed)
					return &state.Result, replayErr
				}
				replays[joinedID] = replay
			}
		}
		tasks := make([]ScreenTask, 0, len(activeIDs))
		for _, id := range activeIDs {
			screen := state.Screens[id]
			machine := screenMachines[id]
			if machine == nil {
				machine = statemachine.NewScreen(id)
				screenMachines[id] = machine
			}
			if machine.Current == statemachine.ScreenPending {
				if err := machine.Transition(statemachine.ScreenNegotiating); err != nil {
					return &state.Result, err
				}
			}
			if machine.Current == statemachine.ScreenCommitted {
				if err := machine.Transition(statemachine.ScreenNegotiating); err != nil {
					return &state.Result, err
				}
			}
			tasks = append(tasks, ScreenTask{
				WindowSeq: window.Seq, Tick: window.Tick, Region: window.Region, Screen: screen,
				TimeQuantum: timeQuantum, MaxPacketBytes: window.MaxPacketBytes,
				ContentSeed: window.ContentSeed, JoinedThisWindow: joined[id],
			})
		}

		outcomes := RunScreenPool(ctx, budget, tasks)
		screenResults := make([]ScreenResult, 0, len(outcomes))
		windowBudget := make([]BudgetRecord, 0, len(outcomes))
		var firstErr error
		for _, outcome := range outcomes {
			windowBudget = append(windowBudget, outcome.Budget)
			machine := screenMachines[outcome.ScreenID]
			if outcome.Err != nil {
				if machine != nil {
					_ = machine.Transition(statemachine.ScreenFailed)
				}
				if firstErr == nil {
					firstErr = outcome.Err
				}
				screenResults = append(screenResults, ScreenResult{
					ScreenID: outcome.ScreenID, ScreenState: string(statemachine.ScreenFailed),
					Budget: outcome.Budget, Mapping: outcome.Mapping, Dispatch: outcome.Dispatch,
				})
				continue
			}
			if machine != nil {
				if machine.Current == statemachine.ScreenNegotiating {
					if err := machine.Transition(statemachine.ScreenCommitted); err != nil {
						return &state.Result, err
					}
				}
			}
			screenResult := ScreenResult{
				ScreenID: outcome.ScreenID, JoinedThisWindow: joined[outcome.ScreenID],
				ScreenState: string(statemachine.ScreenCommitted), Mapping: outcome.Mapping,
				Dispatch: outcome.Dispatch, Budget: outcome.Budget,
			}
			screenResult.Replay = replays[outcome.ScreenID]
			screenResults = append(screenResults, screenResult)
		}

		if firstErr != nil {
			_ = wm.Transition(statemachine.WindowFailed)
			wr := failedWindow(window, wm.Current, decisions, screenResults, budget, firstErr)
			wr.Budget = windowBudget
			state.Result.Windows = append(state.Result.Windows, wr)
			return &state.Result, firstErr
		}

		contentHash, err := dispatch.HashCanonical(map[string]any{
			"window": window.Seq, "tick": window.Tick, "screens": screenResults,
		})
		if err != nil {
			return &state.Result, err
		}
		if err := wm.Transition(statemachine.WindowCommitted); err != nil {
			return &state.Result, err
		}
		wr := WindowResult{
			Seq: window.Seq, ID: window.ID, Tick: window.Tick, State: string(statemachine.WindowCommitted),
			ActiveScreenIDs: activeIDs, ScreenResults: screenResults, Decisions: decisions,
			ContentHash: contentHash, Budget: windowBudget,
		}
		state.Result.Windows = append(state.Result.Windows, wr)
		state.Result.CommitSequence = append(state.Result.CommitSequence, window.Seq)
		state.CompletedWindows = len(state.Result.Windows)
		state.Result.FinalScreenIDs = activeIDs
		state.Result.SessionState = string(session.Current)

		if opts.Store != nil {
			state.PrevCheckpointHash = prevHash
			record, err := opts.Store.Commit(checkpoint.Commit{
				WindowSeq: window.Seq, WindowID: window.ID, Tick: window.Tick,
				SourceVersion: p.SourceVersion, AlgorithmVersion: model.AlgorithmVersion,
				PrevHash: prevHash, Payload: state,
			})
			if err != nil {
				return &state.Result, err
			}
			prevHash = record.ContentHash
		}
	}
	state.Result.FinalScreenIDs = sortedMapKeys(state.Screens)
	return &state.Result, nil
}

func (s *PersistentState) applyWinner(event model.TopologyEvent) error {
	switch event.Kind {
	case model.EventLeave:
		delete(s.Screens, event.ScreenID)
	case model.EventJoin, model.EventTopology:
		if event.Screen == nil {
			return model.NewError(model.EInternal, "event %s missing screen payload", event.ID)
		}
		s.Screens[event.ScreenID] = *event.Screen
	default:
		return model.SchemaError("$.events", "unknown event kind %q", event.Kind)
	}
	return nil
}

func sortedMapKeys(m map[string]model.Screen) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func buildJoinReplay(project *model.Project, sourceSeq int, screen model.Screen, targetSeq int, timeQuantum int64) (*ReplayBundle, error) {
	var instruction *model.WindowInstruction
	for i := range project.Windows {
		if project.Windows[i].Seq == sourceSeq {
			instruction = &project.Windows[i]
			break
		}
	}
	if instruction == nil {
		return nil, model.NewError(model.EInternal, "missing instruction for replay window %d", sourceSeq)
	}
	mapped, err := mapping.MapScreen(mapping.Request{
		Region: instruction.Region, Screen: screen, TimeQuantum: timeQuantum,
		PacketBytes: instruction.MaxPacketBytes,
	}, instruction.Tick)
	if err != nil {
		return nil, err
	}
	packets, err := dispatch.ReplayPackets(targetSeq, sourceSeq, screen.ID, mapped.PixelRegion, instruction.MaxPacketBytes, instruction.ContentSeed)
	if err != nil {
		return nil, err
	}
	return &ReplayBundle{
		TargetWindowSeq: targetSeq, SourceWindowSeq: sourceSeq,
		ScreenID: screen.ID, Packets: packets,
	}, nil
}

func failedWindow(window model.WindowInstruction, state statemachine.WindowState, decisions []topology.Decision, screens []ScreenResult, _ BudgetConfig, err error) WindowResult {
	_ = err
	return WindowResult{
		Seq: window.Seq, ID: window.ID, Tick: window.Tick, State: string(state),
		ScreenResults: screens, Decisions: decisions,
	}
}

func timeQuantumFor(major int) int64 {
	switch major {
	case 1:
		return model.TicksPerSecond / 1000
	case 2:
		return model.TicksPerSecond / 60
	default:
		return 1
	}
}
