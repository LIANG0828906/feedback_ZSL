package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"example.com/multiscreen-cast/protocol"
)

type Config struct {
	Mapping          MappingConfig
	AlgorithmVersion string
	Checkpoint       CheckpointSink
	FailureInjector  func(window int) (displayID string, message string, fail bool)
}

func (c Config) normalized() (Config, *protocol.KernelError) {
	if c.AlgorithmVersion == "" {
		c.AlgorithmVersion = protocol.CurrentAlgorithmVersion
	}
	defaults := DefaultMappingConfig()
	if c.Mapping.MaxWorkers < 0 || c.Mapping.PerWorkerPixelBytes < 0 || c.Mapping.MaxResolution < 0 {
		return c, protocol.Errorf(protocol.CodePara, "engine", "$.mapping", "mapping limits must not be negative")
	}
	if c.Mapping.MaxWorkers == 0 {
		c.Mapping.MaxWorkers = defaults.MaxWorkers
	}
	if c.Mapping.PerWorkerPixelBytes == 0 {
		c.Mapping.PerWorkerPixelBytes = defaults.PerWorkerPixelBytes
	}
	if c.Mapping.MaxResolution == 0 {
		c.Mapping.MaxResolution = defaults.MaxResolution
	}
	if c.Mapping.MinScale == (protocol.Ratio{}) {
		c.Mapping.MinScale = defaults.MinScale
	}
	if c.Mapping.MaxScale == (protocol.Ratio{}) {
		c.Mapping.MaxScale = defaults.MaxScale
	}
	mapping, err := c.Mapping.normalized()
	if err != nil {
		return c, err
	}
	c.Mapping = mapping
	return c, nil
}

type Engine struct {
	mu sync.Mutex

	session     *protocol.Session
	schedule    []protocol.Action
	cfg         Config
	contentHash string

	state           protocol.SessionState
	topologyVersion int
	cursor          int
	committed       int
	lastRegion      *protocol.Rect
	active          map[string]bool
	displays        map[string]protocol.Endpoint
	screens         map[string]*ScreenState
	trace           Trace
	adjudicated     map[int64]bool
	failures        []WindowFailure
}

func NewEngine(session *protocol.Session, cfg Config) (*Engine, *protocol.KernelError) {
	if err := protocol.ValidateSession(session, "engine", "$"); err != nil {
		return nil, err
	}
	normalized, cfgErr := cfg.normalized()
	if cfgErr != nil {
		return nil, cfgErr
	}
	if session.AlgorithmVersion != normalized.AlgorithmVersion {
		return nil, protocol.Errorf(protocol.CodeSchema, "engine", "$.algorithm_version",
			"session algorithm %q does not match engine algorithm %q", session.AlgorithmVersion, normalized.AlgorithmVersion)
	}
	contentHash, err := protocol.SessionContentHash(session)
	if err != nil {
		return nil, protocol.AsKernelError(err)
	}
	engine := &Engine{
		session:         session,
		schedule:        protocol.BuildSchedule(session),
		cfg:             normalized,
		contentHash:     contentHash,
		state:           protocol.StateEstablished,
		topologyVersion: 1,
		active:          make(map[string]bool, len(session.InitialDisplays)),
		displays:        make(map[string]protocol.Endpoint, len(session.Displays)),
		screens:         make(map[string]*ScreenState, len(session.Displays)),
		adjudicated:     make(map[int64]bool),
	}
	for _, display := range session.Displays {
		engine.displays[display.ID] = display
	}
	for _, id := range session.InitialDisplays {
		engine.active[id] = true
		display := engine.displays[id]
		engine.screens[id] = &ScreenState{DisplayID: id, Endpoint: display}
	}
	engine.trace.FinalState = engine.state
	engine.trace.FinalEndpoints = engine.activeIDs()
	return engine, nil
}

func RestoreEngine(session *protocol.Session, cfg Config, snapshot RuntimeSnapshot) (*Engine, *protocol.KernelError) {
	engine, err := NewEngine(session, cfg)
	if err != nil {
		return nil, err
	}
	if snapshot.SessionID != session.ID {
		return nil, protocol.Errorf(protocol.CodeState, "restore", "$.session_id",
			"checkpoint session %q does not match %q", snapshot.SessionID, session.ID)
	}
	if snapshot.AlgorithmVersion != engine.cfg.AlgorithmVersion {
		return nil, protocol.Errorf(protocol.CodeState, "restore", "$.algorithm_version",
			"checkpoint algorithm %q does not match %q", snapshot.AlgorithmVersion, engine.cfg.AlgorithmVersion)
	}
	if snapshot.ContentHash != engine.contentHash {
		return nil, protocol.Errorf(protocol.CodeState, "restore", "$.content_hash",
			"checkpoint content hash does not match current input")
	}
	if snapshot.TotalWindows != len(session.Windows) {
		return nil, protocol.Errorf(protocol.CodeState, "restore", "$.total_windows",
			"checkpoint total windows %d does not match %d", snapshot.TotalWindows, len(session.Windows))
	}
	if snapshot.CommittedWindows < 0 || snapshot.CommittedWindows > snapshot.TotalWindows {
		return nil, protocol.Errorf(protocol.CodeState, "restore", "$.committed_windows",
			"invalid committed window count %d", snapshot.CommittedWindows)
	}
	if snapshot.NextAction < 0 || snapshot.NextAction > len(engine.schedule) {
		return nil, protocol.Errorf(protocol.CodeState, "restore", "$.next_action",
			"next action %d is outside schedule", snapshot.NextAction)
	}
	if snapshot.CommittedWindows > 0 {
		if snapshot.NextAction == 0 {
			return nil, protocol.Errorf(protocol.CodeState, "restore", "$.next_action",
				"checkpoint committed windows require a next action")
		}
		last := engine.schedule[snapshot.NextAction-1]
		if !last.Frame || last.WindowIndex+1 != snapshot.CommittedWindows {
			return nil, protocol.Errorf(protocol.CodeState, "restore", "$.next_action",
				"checkpoint does not end at committed window %d", snapshot.CommittedWindows)
		}
	}
	expectedVersion := protocol.ExpectedTopologyVersionAt(engine.schedule, snapshot.NextAction)
	if snapshot.TopologyVersion != expectedVersion {
		return nil, protocol.Errorf(protocol.CodeState, "restore", "$.topology_version",
			"checkpoint topology version %d does not match expected %d", snapshot.TopologyVersion, expectedVersion)
	}
	if len(snapshot.Trace.Windows) != snapshot.CommittedWindows {
		return nil, protocol.Errorf(protocol.CodeState, "restore", "$.trace.windows",
			"trace has %d windows, checkpoint says %d", len(snapshot.Trace.Windows), snapshot.CommittedWindows)
	}
	if snapshot.State != protocol.StateEstablished &&
		snapshot.State != protocol.StateNegotiating &&
		snapshot.State != protocol.StateCasting &&
		snapshot.State != protocol.StatePaused &&
		snapshot.State != protocol.StateEnded {
		return nil, protocol.Errorf(protocol.CodeState, "restore", "$.state", "invalid state %q", snapshot.State)
	}

	engine.state = snapshot.State
	engine.topologyVersion = snapshot.TopologyVersion
	engine.cursor = snapshot.NextAction
	engine.committed = snapshot.CommittedWindows
	engine.lastRegion = cloneRect(snapshot.LastRegion)
	engine.trace = snapshot.Trace
	engine.trace.FinalState = engine.state
	engine.trace.FinalEndpoints = sortedCopy(snapshot.Endpoints)
	engine.active = make(map[string]bool, len(snapshot.Endpoints))
	engine.displays = make(map[string]protocol.Endpoint, len(snapshot.Displays))
	engine.screens = make(map[string]*ScreenState, len(snapshot.Screens))
	engine.adjudicated = make(map[int64]bool, len(engine.trace.Adjudications))
	for _, adjudication := range engine.trace.Adjudications {
		engine.adjudicated[adjudication.Time] = true
	}
	for _, display := range snapshot.Displays {
		if display.ID == "" {
			return nil, protocol.Errorf(protocol.CodeState, "restore", "$.displays", "checkpoint contains empty display id")
		}
		if err := protocol.ValidateEndpoint(display, "restore", "$.displays["+display.ID+"]"); err != nil {
			return nil, err
		}
		engine.displays[display.ID] = display
	}
	for _, id := range session.InitialDisplays {
		if _, ok := engine.displays[id]; !ok {
			return nil, protocol.Errorf(protocol.CodeState, "restore", "$.displays", "checkpoint omitted display %q", id)
		}
	}
	for _, id := range snapshot.Endpoints {
		if _, ok := engine.displays[id]; !ok {
			return nil, protocol.Errorf(protocol.CodeState, "restore", "$.endpoints", "checkpoint endpoint %q is unknown", id)
		}
		if engine.active[id] {
			return nil, protocol.Errorf(protocol.CodeState, "restore", "$.endpoints", "checkpoint contains duplicate endpoint %q", id)
		}
		engine.active[id] = true
	}
	for _, screen := range snapshot.Screens {
		if !engine.active[screen.DisplayID] {
			return nil, protocol.Errorf(protocol.CodeState, "restore", "$.screens", "checkpoint screen %q is not active", screen.DisplayID)
		}
		copyScreen := screen
		engine.screens[screen.DisplayID] = &copyScreen
	}
	for id := range engine.active {
		if _, ok := engine.screens[id]; !ok {
			engine.screens[id] = &ScreenState{DisplayID: id, Endpoint: engine.displays[id]}
		}
	}
	return engine, nil
}

func (e *Engine) State() protocol.SessionState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

func (e *Engine) Progress() Progress {
	e.mu.Lock()
	defer e.mu.Unlock()
	return Progress{Committed: e.committed, Total: len(e.session.Windows)}
}

func (e *Engine) Done() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cursor >= len(e.schedule)
}

func (e *Engine) Trace() Trace {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneTrace(e.trace)
}

func (e *Engine) Failures() []WindowFailure {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]WindowFailure(nil), e.failures...)
}

func (e *Engine) SetFailureInjector(injector func(window int) (displayID string, message string, fail bool)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg.FailureInjector = injector
}

func (e *Engine) Snapshot() RuntimeSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshot()
}

func (e *Engine) Run(ctx context.Context) (*Trace, *protocol.KernelError) {
	for {
		e.mu.Lock()
		done := e.cursor >= len(e.schedule)
		e.mu.Unlock()
		if done {
			trace := e.Trace()
			return &trace, nil
		}
		if _, err := e.Advance(ctx); err != nil {
			return nil, err
		}
	}
}

func (e *Engine) Advance(ctx context.Context) (*Step, *protocol.KernelError) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cursor >= len(e.schedule) {
		return nil, nil
	}
	action := e.schedule[e.cursor]
	if !action.Frame && e.state == protocol.StateEnded {
		return nil, e.stateError(action)
	}
	if !stateAllows(e.state, action) {
		return nil, e.stateError(action)
	}
	if action.Frame {
		return e.advanceFrame(ctx, action)
	}
	return e.advanceEvent(action)
}

func (e *Engine) advanceEvent(action protocol.Action) (*Step, *protocol.KernelError) {
	event := e.session.Events[action.EventIndex]
	if event.Kind.IsEndpoint() && !e.adjudicated[event.Time] {
		if err := e.preflightEndpointGroup(action); err != nil {
			return nil, err
		}
	}
	stateBefore := e.state
	var replay *ScreenResult
	var replayWindow int
	switch event.Kind {
	case protocol.EventNegotiate:
		e.state = protocol.StateNegotiating
	case protocol.EventStart:
		if len(e.active) == 0 {
			return nil, protocol.WithTime(protocol.WithDisplay(protocol.Errorf(protocol.CodeState, "engine", action.Path()+".kind",
				"cannot start casting without an active display"), "", "endpoints"), event.Time)
		}
		e.state = protocol.StateCasting
	case protocol.EventPause:
		e.state = protocol.StatePaused
	case protocol.EventResume:
		e.state = protocol.StateCasting
	case protocol.EventEnd:
		e.state = protocol.StateEnded
	case protocol.EventJoin:
		if e.active[event.DisplayID] {
			return nil, e.conflict(action, "display is already joined")
		}
		e.topologyVersion++
		if (stateBefore == protocol.StateCasting || stateBefore == protocol.StatePaused) &&
			e.lastRegion != nil && e.committed > 0 {
			display := e.displays[event.DisplayID]
			results, _, mapErr := mapWindow(context.Background(), []protocol.Endpoint{display}, *e.lastRegion,
				e.committed, e.topologyVersion, e.cfg.AlgorithmVersion, e.cfg.Mapping)
			if mapErr != nil {
				e.topologyVersion--
				return nil, protocol.WithWindow(mapErr, e.committed)
			}
			if len(results) == 1 {
				resultCopy := results[0]
				replay = &resultCopy
				replayWindow = e.committed
			}
		}
		e.active[event.DisplayID] = true
		display := e.displays[event.DisplayID]
		screen := &ScreenState{DisplayID: event.DisplayID, Endpoint: display}
		if replay != nil {
			regionCopy := *e.lastRegion
			screen.LastRegion = &regionCopy
			resultCopy := *replay
			screen.LastResult = &resultCopy
			screen.SourceWindow = replayWindow
		}
		e.screens[event.DisplayID] = screen
	case protocol.EventLeave:
		if !e.active[event.DisplayID] {
			return nil, e.conflict(action, "display is not joined")
		}
		e.topologyVersion++
		delete(e.active, event.DisplayID)
		delete(e.screens, event.DisplayID)
	case protocol.EventTopology:
		display := e.displays[event.DisplayID]
		display.Topology = *event.Topology
		if event.Capability != nil {
			display.Capability = *event.Capability
		}
		if err := protocol.ValidateEndpoint(display, "engine", action.Path()); err != nil {
			return nil, protocol.WithTime(err, event.Time)
		}
		e.topologyVersion++
		e.displays[event.DisplayID] = display
		if screen, ok := e.screens[event.DisplayID]; ok {
			screen.Endpoint = display
		}
	default:
		return nil, protocol.WithTime(protocol.Errorf(protocol.CodeSchema, "engine", action.Path()+".kind",
			"unknown event kind %q", event.Kind), event.Time)
	}

	if event.Kind.IsEndpoint() {
		if err := e.recordAdjudication(action); err != nil {
			return nil, err
		}
	}
	effect := EventEffect{
		Seq:             event.Seq,
		Time:            event.Time,
		Kind:            event.Kind,
		DisplayID:       event.DisplayID,
		EffectiveTime:   event.Time,
		CommittedWindow: e.committed,
		StateBefore:     stateBefore,
		StateAfter:      e.state,
		TopologyVersion: e.topologyVersion,
		EndpointsAfter:  e.activeIDs(),
		ReplayWindow:    replayWindow,
		Replay:          replay,
	}
	if stateBefore != e.state {
		e.trace.Transitions = append(e.trace.Transitions, StateTransition{
			Seq: event.Seq, Time: event.Time, Kind: event.Kind, Before: stateBefore, After: e.state,
		})
	}
	e.trace.EventEffects = append(e.trace.EventEffects, effect)
	e.trace.FinalState = e.state
	e.trace.FinalEndpoints = e.activeIDs()
	e.cursor++
	return &Step{
		Action:     action,
		Event:      &effect,
		StateAfter: e.state,
		Progress:   Progress{Committed: e.committed, Total: len(e.session.Windows)},
	}, nil
}

func (e *Engine) advanceFrame(ctx context.Context, action protocol.Action) (*Step, *protocol.KernelError) {
	window := e.session.Windows[action.WindowIndex]
	if e.cfg.FailureInjector != nil {
		displayID, message, fail := e.cfg.FailureInjector(window.Index)
		if fail {
			if displayID == "" {
				endpoints := e.activeIDs()
				if len(endpoints) > 0 {
					displayID = endpoints[0]
				}
			}
			err := protocol.WithWindow(protocol.WithTime(protocol.WithDisplay(
				protocol.Errorf(protocol.CodeSeg, "engine", action.Path(), "injected window failure: %s", message),
				displayID, "window"), window.Time), window.Index)
			e.recordFailure(window.Index, displayID, message)
			return nil, err
		}
	}

	targetIDs := append([]string(nil), window.Targets...)
	if len(targetIDs) == 0 {
		targetIDs = e.activeIDs()
	} else {
		sort.Strings(targetIDs)
	}
	targets := make([]protocol.Endpoint, 0, len(targetIDs))
	for _, id := range targetIDs {
		if !e.active[id] {
			err := protocol.WithWindow(protocol.WithTime(protocol.WithDisplay(
				protocol.Errorf(protocol.CodeRef, "engine", action.Path()+".targets", "display %q is not joined", id),
				id, "targets"), window.Time), window.Index)
			e.recordFailure(window.Index, id, err.Message)
			return nil, err
		}
		targets = append(targets, e.displays[id])
	}
	results, queues, mapErr := mapWindow(ctx, targets, window.Region, window.Index, e.topologyVersion, e.cfg.AlgorithmVersion, e.cfg.Mapping)
	if mapErr != nil {
		e.recordFailure(window.Index, mapErr.DisplayID, mapErr.Message)
		return nil, protocol.WithWindow(protocol.WithTime(mapErr, window.Time), window.Index)
	}
	resultByID := make(map[string]ScreenResult, len(results))
	for _, result := range results {
		resultByID[result.DisplayID] = result
	}

	nextScreens := make(map[string]*ScreenState, len(e.screens))
	for id, state := range e.screens {
		copyState := *state
		if state.LastRegion != nil {
			regionCopy := *state.LastRegion
			copyState.LastRegion = &regionCopy
		}
		if state.LastResult != nil {
			resultCopy := *state.LastResult
			resultCopy.Window = window.Index
			copyState.LastResult = &resultCopy
		}
		nextScreens[id] = &copyState
	}
	for id, result := range resultByID {
		screen, ok := nextScreens[id]
		if !ok {
			return nil, protocol.WithWindow(protocol.Errorf(protocol.CodeSeg, "engine", action.Path(),
				"mapping returned result for inactive display %q", id), window.Index)
		}
		result.Window = window.Index
		resultCopy := result
		regionCopy := window.Region
		screen.LastRegion = &regionCopy
		screen.LastResult = &resultCopy
		screen.SourceWindow = window.Index
	}
	screenSnapshot := make([]ScreenResult, 0, len(nextScreens))
	for id, state := range nextScreens {
		if !e.active[id] || state.LastResult == nil {
			continue
		}
		resultCopy := *state.LastResult
		resultCopy.Window = window.Index
		screenSnapshot = append(screenSnapshot, resultCopy)
	}
	sort.Slice(screenSnapshot, func(i, j int) bool {
		return screenSnapshot[i].DisplayID < screenSnapshot[j].DisplayID
	})
	windowSnapshot := WindowSnapshot{
		Window:          window.Index,
		Time:            window.Time,
		State:           e.state,
		TopologyVersion: e.topologyVersion,
		Endpoints:       e.activeIDs(),
		Screens:         screenSnapshot,
		QueueRecords:    queues,
		Adjudications:   cloneAdjudications(e.trace.Adjudications),
	}
	nextTrace := cloneTrace(e.trace)
	nextTrace.Windows = append(nextTrace.Windows, windowSnapshot)

	regionCopy := window.Region
	nextSnapshot := RuntimeSnapshot{
		SessionID:        e.session.ID,
		AlgorithmVersion: e.cfg.AlgorithmVersion,
		ContentHash:      e.contentHash,
		State:            e.state,
		TopologyVersion:  e.topologyVersion,
		Endpoints:        e.activeIDs(),
		Displays:         e.displayList(),
		Screens:          screenStates(nextScreens, e.active),
		LastRegion:       &regionCopy,
		CommittedWindows: e.committed + 1,
		TotalWindows:     len(e.session.Windows),
		NextAction:       e.cursor + 1,
		Trace:            nextTrace,
	}
	if e.cfg.Checkpoint != nil {
		if err := e.cfg.Checkpoint.SaveCheckpoint(nextSnapshot); err != nil {
			return nil, protocol.WithWindow(protocol.Errorf(protocol.CodeStore, "checkpoint", action.Path(),
				"cannot persist committed window: %v", err), window.Index)
		}
	}

	e.screens = nextScreens
	e.lastRegion = &regionCopy
	e.committed++
	e.cursor++
	e.trace = nextTrace
	e.trace.FinalState = e.state
	e.trace.FinalEndpoints = e.activeIDs()
	return &Step{
		Action:     action,
		Window:     &windowSnapshot,
		StateAfter: e.state,
		Progress:   Progress{Committed: e.committed, Total: len(e.session.Windows)},
	}, nil
}

func (e *Engine) snapshot() RuntimeSnapshot {
	return RuntimeSnapshot{
		SessionID:        e.session.ID,
		AlgorithmVersion: e.cfg.AlgorithmVersion,
		ContentHash:      e.contentHash,
		State:            e.state,
		TopologyVersion:  e.topologyVersion,
		Endpoints:        e.activeIDs(),
		Displays:         e.displayList(),
		Screens:          screenStates(e.screens, e.active),
		LastRegion:       cloneRect(e.lastRegion),
		CommittedWindows: e.committed,
		TotalWindows:     len(e.session.Windows),
		NextAction:       e.cursor,
		Trace:            cloneTrace(e.trace),
	}
}

func stateAllows(state protocol.SessionState, action protocol.Action) bool {
	if action.Frame {
		return state == protocol.StateCasting
	}
	switch action.Kind {
	case protocol.EventNegotiate:
		return state == protocol.StateEstablished
	case protocol.EventStart:
		return state == protocol.StateNegotiating
	case protocol.EventPause:
		return state == protocol.StateCasting
	case protocol.EventResume:
		return state == protocol.StatePaused
	case protocol.EventEnd:
		return state != protocol.StateEnded
	case protocol.EventJoin, protocol.EventLeave, protocol.EventTopology:
		return state != protocol.StateEnded
	default:
		return false
	}
}

func (e *Engine) stateError(action protocol.Action) *protocol.KernelError {
	kind := string(action.Kind)
	window := 0
	if action.Frame {
		kind = "frame"
		window = action.WindowIndex + 1
	}
	err := protocol.Errorf(protocol.CodeState, "engine", action.Path()+".kind",
		"instruction %s is not allowed in state %s", kind, e.state)
	err.State = string(e.state)
	err.Window = window
	if !action.Frame {
		event := e.session.Events[action.EventIndex]
		protocol.WithTime(err, event.Time)
		err.DisplayID = event.DisplayID
	} else {
		protocol.WithTime(err, e.session.Windows[action.WindowIndex].Time)
	}
	err.Field = "state"
	return err
}

func (e *Engine) conflict(action protocol.Action, message string) *protocol.KernelError {
	event := e.session.Events[action.EventIndex]
	err := protocol.Errorf(protocol.CodeConflict, "engine", action.Path(),
		"%s at time %d for display %q", message, event.Time, event.DisplayID)
	protocol.WithTime(err, event.Time)
	protocol.WithDisplay(err, event.DisplayID, "display_id")
	return err
}

func (e *Engine) preflightEndpointGroup(action protocol.Action) *protocol.KernelError {
	event := e.session.Events[action.EventIndex]
	group := make([]protocol.Action, 0, 4)
	for _, candidate := range e.schedule {
		if candidate.Frame || candidate.Time != event.Time {
			continue
		}
		if e.session.Events[candidate.EventIndex].Kind.IsEndpoint() {
			group = append(group, candidate)
		}
	}
	tempActive := make(map[string]bool, len(e.active))
	for id := range e.active {
		tempActive[id] = true
	}
	seenOperations := make(map[string]bool, len(group))
	for _, candidate := range group {
		candidateEvent := e.session.Events[candidate.EventIndex]
		if seenOperations[candidateEvent.DisplayID] {
			return e.conflict(candidate, "multiple endpoint operations for one display at the same logical time are ambiguous")
		}
		seenOperations[candidateEvent.DisplayID] = true
		switch candidateEvent.Kind {
		case protocol.EventJoin:
			if tempActive[candidateEvent.DisplayID] {
				return e.conflict(candidate, "concurrent join conflicts with an already joined display")
			}
			tempActive[candidateEvent.DisplayID] = true
		case protocol.EventLeave:
			if !tempActive[candidateEvent.DisplayID] {
				return e.conflict(candidate, "concurrent leave conflicts with a display that is not joined")
			}
			delete(tempActive, candidateEvent.DisplayID)
		}
	}
	return nil
}

func (e *Engine) recordAdjudication(action protocol.Action) *protocol.KernelError {
	event := e.session.Events[action.EventIndex]
	if e.adjudicated[event.Time] {
		return nil
	}
	group := make([]protocol.Action, 0, 4)
	for _, candidate := range e.schedule {
		if candidate.Frame || candidate.Time != event.Time {
			continue
		}
		if e.session.Events[candidate.EventIndex].Kind.IsEndpoint() {
			group = append(group, candidate)
		}
	}
	if len(group) < 2 {
		e.adjudicated[event.Time] = true
		return nil
	}
	adjudication := Adjudication{
		Time:  event.Time,
		Rule:  "same-time endpoint events: leave before join, then display_id and seq ascending; one display has at most one endpoint operation per time",
		Items: make([]AdjudicationItem, 0, len(group)),
	}
	for _, candidate := range group {
		candidateEvent := e.session.Events[candidate.EventIndex]
		adjudication.Items = append(adjudication.Items, AdjudicationItem{
			Seq:       candidateEvent.Seq,
			Kind:      candidateEvent.Kind,
			DisplayID: candidateEvent.DisplayID,
			Phase:     candidate.Phase,
			Status:    "applied",
		})
	}
	e.trace.Adjudications = append(e.trace.Adjudications, adjudication)
	e.adjudicated[event.Time] = true
	return nil
}

func (e *Engine) activeIDs() []string {
	ids := make([]string, 0, len(e.active))
	for id := range e.active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedCopy(values []string) []string {
	output := append([]string(nil), values...)
	sort.Strings(output)
	return output
}

func (e *Engine) displayList() []protocol.Endpoint {
	displays := make([]protocol.Endpoint, 0, len(e.displays))
	for _, display := range e.displays {
		displays = append(displays, display)
	}
	sort.Slice(displays, func(i, j int) bool { return displays[i].ID < displays[j].ID })
	return displays
}

func (e *Engine) recordFailure(window int, displayID, message string) {
	e.failures = append(e.failures, WindowFailure{
		Window:    window,
		DisplayID: displayID,
		Message:   message,
		At:        time.Now().UTC(),
	})
}

func screenStates(screens map[string]*ScreenState, active map[string]bool) []ScreenState {
	states := make([]ScreenState, 0, len(screens))
	for id, state := range screens {
		if !active[id] {
			continue
		}
		copyState := *state
		if state.LastRegion != nil {
			regionCopy := *state.LastRegion
			copyState.LastRegion = &regionCopy
		}
		if state.LastResult != nil {
			resultCopy := *state.LastResult
			copyState.LastResult = &resultCopy
		}
		states = append(states, copyState)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].DisplayID < states[j].DisplayID })
	return states
}

func cloneRect(rect *protocol.Rect) *protocol.Rect {
	if rect == nil {
		return nil
	}
	copyRect := *rect
	return &copyRect
}

func cloneAdjudications(input []Adjudication) []Adjudication {
	if len(input) == 0 {
		return nil
	}
	output := make([]Adjudication, len(input))
	for i := range input {
		output[i] = input[i]
		output[i].Items = append([]AdjudicationItem(nil), input[i].Items...)
	}
	return output
}

func cloneTrace(input Trace) Trace {
	output := input
	output.Windows = append([]WindowSnapshot(nil), input.Windows...)
	for i := range output.Windows {
		output.Windows[i].Endpoints = append([]string(nil), input.Windows[i].Endpoints...)
		output.Windows[i].Screens = append([]ScreenResult(nil), input.Windows[i].Screens...)
		output.Windows[i].QueueRecords = append([]QueueRecord(nil), input.Windows[i].QueueRecords...)
		output.Windows[i].Adjudications = cloneAdjudications(input.Windows[i].Adjudications)
	}
	output.EventEffects = append([]EventEffect(nil), input.EventEffects...)
	for i := range output.EventEffects {
		output.EventEffects[i].EndpointsAfter = append([]string(nil), input.EventEffects[i].EndpointsAfter...)
		if input.EventEffects[i].Replay != nil {
			replay := *input.EventEffects[i].Replay
			output.EventEffects[i].Replay = &replay
		}
	}
	output.Adjudications = cloneAdjudications(input.Adjudications)
	output.Transitions = append([]StateTransition(nil), input.Transitions...)
	output.FinalEndpoints = append([]string(nil), input.FinalEndpoints...)
	return output
}

func SnapshotJSON(snapshot RuntimeSnapshot) ([]byte, string, error) {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return nil, "", fmt.Errorf("marshal checkpoint: %w", err)
	}
	return payload, protocol.HashBytes(payload), nil
}
