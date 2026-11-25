package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"obsproj/internal/model"
)

type EngineOptions struct {
	MapPool             MapPoolOptions
	InjectWindowFailure int
	InjectScreenFailure string
}

func DefaultEngineOptions() EngineOptions {
	return EngineOptions{MapPool: DefaultMapPoolOptions()}
}

type windowPlan struct {
	Window   int
	At       int64
	Source   model.Rect
	Commands []model.Command
}

type Engine struct {
	session        *model.Session
	options        EngineOptions
	screens        map[string]model.Screen
	active         map[string]bool
	screenFSM      map[string]*model.ScreenFSM
	sessionFSM     *model.SessionFSM
	windowFSM      map[int]*model.WindowFSM
	topology       int64
	eventCursor    int
	lastSource     *model.Rect
	lastWindow     int
	commits        []model.WindowCommit
	plans          []windowPlan
	plansValidated bool
}

func NewEngine(session *model.Session, options EngineOptions) (*Engine, error) {
	if session == nil {
		return nil, model.NewError(model.CodeSchema, "nil session")
	}
	work := session.Clone()
	work.Normalize()
	e := &Engine{
		session:    work,
		options:    options,
		screens:    make(map[string]model.Screen, len(work.Screens)),
		active:     make(map[string]bool, len(work.Screens)),
		screenFSM:  make(map[string]*model.ScreenFSM, len(work.Screens)),
		sessionFSM: model.NewSessionFSM(),
		windowFSM:  make(map[int]*model.WindowFSM),
		topology:   1,
	}
	for _, screen := range work.Screens {
		e.screens[screen.ID] = screen.Clone()
		e.screenFSM[screen.ID] = model.NewScreenFSM()
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return e, nil
}

func Restore(session *model.Session, snapshot model.RuntimeSnapshot, options EngineOptions) (*Engine, error) {
	e, err := NewEngine(session, options)
	if err != nil {
		return nil, err
	}
	e.screens = make(map[string]model.Screen, len(snapshot.Screens))
	e.screenFSM = make(map[string]*model.ScreenFSM, len(snapshot.Screens))
	e.active = make(map[string]bool, len(snapshot.ActiveScreens))
	for _, runtimeScreen := range snapshot.Screens {
		screen := runtimeScreen.Screen.Clone()
		e.screens[screen.ID] = screen
		fsm := &model.ScreenFSM{State: runtimeScreen.State, Progress: runtimeScreen.LastWindow}
		if fsm.State == "" {
			fsm.State = model.ScreenPending
		}
		e.screenFSM[screen.ID] = fsm
	}
	for _, screenID := range snapshot.ActiveScreens {
		if _, ok := e.screens[screenID]; !ok {
			return nil, &model.KernelError{
				Code:     model.CodeState,
				Message:  "checkpoint active set references an unknown screen",
				ScreenID: screenID,
				State:    string(snapshot.SessionState),
			}
		}
		e.active[screenID] = true
	}
	e.sessionFSM = &model.SessionFSM{State: snapshot.SessionState, Progress: snapshot.LastCommittedWindow}
	if e.sessionFSM.State == "" {
		e.sessionFSM.State = model.SessionStreaming
	}
	e.topology = snapshot.TopologyVersion
	if e.topology == 0 {
		e.topology = 1
	}
	e.eventCursor = snapshot.EventCursor
	e.lastWindow = snapshot.LastCommittedWindow
	if snapshot.LastSourceRegion != nil {
		copyRect := *snapshot.LastSourceRegion
		e.lastSource = &copyRect
	}
	return e, nil
}

func (e *Engine) Session() *model.Session {
	return e.session.Clone()
}

func (e *Engine) SessionHash() string {
	return e.session.Fingerprint()
}

func (e *Engine) Commits() []model.WindowCommit {
	return append([]model.WindowCommit(nil), e.commits...)
}

func (e *Engine) Validate() *model.KernelError {
	if e.session.TickRate <= 0 {
		return &model.KernelError{Code: model.CodeOrder, Message: "tick rate must be positive", Field: "tick_rate"}
	}
	if len(e.session.Commands) == 0 {
		return &model.KernelError{Code: model.CodeOrder, Message: "session contains no frame commands", Field: "frames"}
	}
	for _, screen := range e.session.Screens {
		if !screen.AllowsScale(screen.Scale) {
			return &model.KernelError{
				Code:           model.CodeCapacity,
				Message:        "selected scale is outside declared capability",
				ScreenID:       screen.ID,
				RequestedScale: &screen.Scale,
				AllowedScales:  append([]model.Scale(nil), screen.Capability.Scales...),
			}
		}
		if !screen.AllowsChroma(screen.Chroma) {
			return &model.KernelError{
				Code:            model.CodeCapacity,
				Message:         "selected chroma is outside declared capability",
				ScreenID:        screen.ID,
				RequestedChroma: &screen.Chroma,
				AllowedChromas:  append([]model.Chroma(nil), screen.Capability.Chromas...),
			}
		}
	}
	seenEvents := map[string]bool{}
	for _, event := range e.session.Events {
		if seenEvents[event.ID] {
			return &model.KernelError{
				Code:     model.CodeConflict,
				Message:  "duplicate event id cannot be arbitrated",
				Window:   event.Window,
				ScreenID: event.ScreenID,
				Field:    "events.event_id",
			}
		}
		seenEvents[event.ID] = true
		if event.At < 0 {
			return &model.KernelError{
				Code:     model.CodeOrder,
				Message:  "event time must not be negative",
				Window:   event.Window,
				ScreenID: event.ScreenID,
				Field:    "events.at_tick",
			}
		}
		if _, ok := e.screens[event.ScreenID]; !ok {
			return &model.KernelError{
				Code:     model.CodeCapacity,
				Message:  "event references an unknown screen",
				Window:   event.Window,
				ScreenID: event.ScreenID,
				Field:    "events.screen_id",
			}
		}
	}
	plans, planErr := buildWindowPlans(e.session)
	if planErr != nil {
		return planErr
	}
	e.plans = plans
	e.plansValidated = true
	return nil
}

func buildWindowPlans(session *model.Session) ([]windowPlan, *model.KernelError) {
	byWindow := make(map[int][]model.Command)
	for _, command := range session.Commands {
		if _, ok := findScreen(session.Screens, command.ScreenID); !ok {
			return nil, &model.KernelError{
				Code:     model.CodeCapacity,
				Message:  "frame references an unknown screen",
				Window:   command.Window,
				ScreenID: command.ScreenID,
				Field:    "frames.screen_id",
			}
		}
		byWindow[command.Window] = append(byWindow[command.Window], command)
	}
	windows := make([]int, 0, len(byWindow))
	for window := range byWindow {
		windows = append(windows, window)
	}
	sort.Ints(windows)
	plans := make([]windowPlan, 0, len(windows))
	previousAt := int64(-1)
	for _, window := range windows {
		commands := byWindow[window]
		sort.Slice(commands, func(i, j int) bool {
			return commands[i].ScreenID < commands[j].ScreenID
		})
		windowAt := commands[0].At
		seen := map[string]bool{}
		for _, command := range commands {
			if seen[command.ScreenID] {
				return nil, &model.KernelError{
					Code:     model.CodeOrder,
					Message:  "duplicate frame command for screen/window",
					Window:   window,
					ScreenID: command.ScreenID,
					Field:    "frames",
				}
			}
			seen[command.ScreenID] = true
			if command.At != windowAt {
				return nil, &model.KernelError{
					Code:     model.CodeOrder,
					Message:  "frame commands in one window have different logical times",
					Window:   window,
					ScreenID: command.ScreenID,
					Field:    "frames.at_tick",
				}
			}
		}
		if windowAt < previousAt {
			return nil, &model.KernelError{
				Code:     model.CodeOrder,
				Message:  "frame window time is earlier than the previous window",
				Window:   window,
				ScreenID: commands[0].ScreenID,
				Field:    "frames.at_tick",
				Operands: []int64{previousAt, windowAt},
			}
		}
		previousAt = windowAt
		plans = append(plans, windowPlan{
			Window:   window,
			At:       windowAt,
			Source:   commands[0].Region,
			Commands: commands,
		})
	}
	return plans, nil
}

func findScreen(screens []model.Screen, id string) (model.Screen, bool) {
	for _, screen := range screens {
		if screen.ID == id {
			return screen, true
		}
	}
	return model.Screen{}, false
}

func (e *Engine) planForWindow(window int) (windowPlan, int, bool) {
	for i, plan := range e.plans {
		if plan.Window == window {
			return plan, i, true
		}
	}
	return windowPlan{}, -1, false
}

func (e *Engine) isNextWindow(window int) bool {
	if e.lastWindow == 0 {
		return len(e.plans) > 0 && e.plans[0].Window == window
	}
	for i, plan := range e.plans {
		if plan.Window == e.lastWindow {
			return i+1 < len(e.plans) && e.plans[i+1].Window == window
		}
	}
	return false
}

func (e *Engine) PrepareSessionForWindow() *model.KernelError {
	switch e.sessionFSM.State {
	case model.SessionEstablished:
		if err := e.sessionFSM.Transition(model.SessionNegotiating); err != nil {
			return err
		}
		return e.sessionFSM.Transition(model.SessionStreaming)
	case model.SessionNegotiating:
		return e.sessionFSM.Transition(model.SessionStreaming)
	case model.SessionStreaming:
		return nil
	case model.SessionPaused:
		return e.sessionFSM.Transition(model.SessionStreaming)
	default:
		return &model.KernelError{
			Code:        model.CodeState,
			Message:     "session cannot advance from its current state",
			State:       string(e.sessionFSM.State),
			TargetState: string(model.SessionStreaming),
			Progress:    e.sessionFSM.Progress,
		}
	}
}

func (e *Engine) ProcessWindow(window int) (model.WindowCommit, []model.QueueObservation, error) {
	if !e.plansValidated {
		if err := e.Validate(); err != nil {
			return model.WindowCommit{}, nil, err
		}
	}
	plan, _, ok := e.planForWindow(window)
	if !ok {
		return model.WindowCommit{}, nil, &model.KernelError{
			Code:     model.CodeState,
			Message:  "unknown frame window",
			Window:   window,
			State:    string(e.sessionFSM.State),
			Progress: e.lastWindow,
		}
	}
	if !e.isNextWindow(window) {
		return model.WindowCommit{}, nil, &model.KernelError{
			Code:        model.CodeState,
			Message:     "window is not the next contiguous checkpoint",
			Window:      window,
			State:       string(e.sessionFSM.State),
			TargetState: string(model.WindowDistributing),
			Progress:    e.lastWindow,
		}
	}
	if err := e.PrepareSessionForWindow(); err != nil {
		return model.WindowCommit{}, nil, err
	}
	windowFSM := model.NewWindowFSM()
	e.windowFSM[window] = windowFSM
	if err := windowFSM.Transition(model.WindowDistributing); err != nil {
		return model.WindowCommit{}, nil, err
	}
	failWindow := func(err *model.KernelError) (model.WindowCommit, []model.QueueObservation, error) {
		_ = windowFSM.Transition(model.WindowFailed)
		if e.sessionFSM.State == model.SessionStreaming {
			_ = e.sessionFSM.Transition(model.SessionPaused)
		}
		return model.WindowCommit{}, nil, err
	}
	if e.options.InjectWindowFailure == window {
		screenID := e.options.InjectScreenFailure
		if screenID != "" && e.screenFSM[screenID] != nil {
			_ = e.screenFSM[screenID].Transition(model.ScreenFailed)
		}
		return failWindow(&model.KernelError{
			Code:     model.CodeSegment,
			Message:  "injected single-window internal failure",
			Window:   window,
			ScreenID: screenID,
			Field:    "window.pipeline",
		})
	}
	outcome, eventErr := e.processEvents(window, plan.At)
	if eventErr != nil {
		return failWindow(eventErr)
	}
	e.eventCursor = outcome.cursor
	e.topology = outcome.topologyVersion
	activeScreens := e.activeScreenIDs()
	commands := make(map[string]model.Command, len(plan.Commands))
	for _, command := range plan.Commands {
		commands[command.ScreenID] = command
	}
	jobs := make([]MapJob, 0, len(activeScreens))
	for _, screenID := range activeScreens {
		command, ok := commands[screenID]
		if !ok {
			if e.screenFSM[screenID] != nil {
				_ = e.screenFSM[screenID].Transition(model.ScreenFailed)
			}
			return failWindow(&model.KernelError{
				Code:     model.CodeSegment,
				Message:  "active screen has no frame command in this window",
				Window:   window,
				ScreenID: screenID,
				Field:    "frames",
			})
		}
		timeResidual := command.At - plan.At
		if timeResidual < 0 {
			timeResidual = -timeResidual
		}
		jobs = append(jobs, MapJob{
			Window:       window,
			Screen:       e.screens[screenID].Clone(),
			Source:       command.Region,
			TimeResidual: timeResidual,
		})
	}
	results, queue, mapErr := MapAll(jobs, e.options.MapPool)
	if mapErr != nil {
		if mapErr.ScreenID != "" && e.screenFSM[mapErr.ScreenID] != nil {
			_ = e.screenFSM[mapErr.ScreenID].Transition(model.ScreenFailed)
		}
		mapErr.Window = window
		return failWindow(mapErr)
	}
	newJoinSet := map[string]bool{}
	for _, screenID := range outcome.newJoins {
		newJoinSet[screenID] = true
	}
	screenCommits := make([]model.ScreenCommit, 0, len(results))
	for _, result := range results {
		if result.Err != nil {
			if e.screenFSM[result.Mapped.ScreenID] != nil {
				_ = e.screenFSM[result.Mapped.ScreenID].Transition(model.ScreenFailed)
			}
			result.Err.Window = window
			return failWindow(result.Err)
		}
		screen := e.screens[result.Mapped.ScreenID].Clone()
		var packets []model.Packet
		nextSeq := 0
		var replayWindows []int
		if newJoinSet[screen.ID] && e.lastSource != nil && e.lastWindow > 0 {
			replayMapped, replayMetrics, replayErr := MapRegion(screen, *e.lastSource, 0)
			if replayErr != nil {
				replayErr.Window = window
				replayErr.Message = "join replay failed: " + replayErr.Message
				return failWindow(replayErr)
			}
			replayPackets, packetErr := buildPackets(e.session.ID, window, screen, replayMapped, true, e.lastWindow, nextSeq)
			if packetErr != nil {
				packetErr.Window = window
				return failWindow(packetErr)
			}
			packets = append(packets, replayPackets...)
			nextSeq += len(replayPackets)
			replayWindows = append(replayWindows, e.lastWindow)
			_ = replayMetrics
		}
		currentPackets, packetErr := buildPackets(e.session.ID, window, screen, result.Mapped, false, window, nextSeq)
		if packetErr != nil {
			packetErr.Window = window
			return failWindow(packetErr)
		}
		packets = append(packets, currentPackets...)
		commit := model.ScreenCommit{
			ScreenID:      screen.ID,
			SourceWindow:  window,
			Mapped:        result.Mapped,
			Metrics:       result.Metrics,
			Packets:       packets,
			ReplayWindows: replayWindows,
		}
		commit.ContentHash = model.HashJSON(commit)
		screenCommits = append(screenCommits, commit)
		if e.screenFSM[screen.ID] != nil {
			e.screenFSM[screen.ID].AdvanceProgress(window)
		}
	}
	globalSeq := 0
	for i := range screenCommits {
		for j := range screenCommits[i].Packets {
			screenCommits[i].Packets[j].GlobalSeq = globalSeq
			globalSeq++
		}
		screenCommits[i].ContentHash = model.HashJSON(model.ScreenCommit{
			ScreenID:      screenCommits[i].ScreenID,
			SourceWindow:  screenCommits[i].SourceWindow,
			Mapped:        screenCommits[i].Mapped,
			Metrics:       screenCommits[i].Metrics,
			Packets:       screenCommits[i].Packets,
			ReplayWindows: screenCommits[i].ReplayWindows,
		})
	}
	e.lastWindow = window
	sourceCopy := plan.Source
	e.lastSource = &sourceCopy
	e.advanceActiveProgress(window)
	e.sessionFSM.AdvanceProgress(window)
	e.eventCursor = outcome.cursor
	snapshot := e.snapshot(window, outcome.topologyVersion)
	commit := model.WindowCommit{
		Window:           window,
		At:               plan.At,
		SourceRegion:     plan.Source,
		ActiveScreens:    activeScreens,
		Decisions:        outcome.decisions,
		TopologyVersion:  outcome.topologyVersion,
		Screens:          screenCommits,
		Snapshot:         snapshot,
		SessionHash:      e.session.Fingerprint(),
		AlgorithmVersion: model.AlgorithmVersion,
	}
	commit.ContentHash = model.HashJSON(commit)
	windowFSM.AdvanceProgress(len(screenCommits))
	if err := windowFSM.Transition(model.WindowCommitted); err != nil {
		return failWindow(err)
	}
	e.commits = append(e.commits, commit)
	return commit, queue, nil
}

func (e *Engine) advanceActiveProgress(window int) {
	for screenID, active := range e.active {
		if !active {
			continue
		}
		if fsm := e.screenFSM[screenID]; fsm != nil && fsm.State == model.ScreenPending {
			e.activateScreenFSM(screenID)
		}
		if fsm := e.screenFSM[screenID]; fsm != nil {
			fsm.AdvanceProgress(window)
		}
	}
}

func (e *Engine) activeScreenIDs() []string {
	out := make([]string, 0, len(e.active))
	for screenID, active := range e.active {
		if active {
			out = append(out, screenID)
		}
	}
	sort.Strings(out)
	return out
}

func (e *Engine) snapshot(window int, topologyVersion int64) model.RuntimeSnapshot {
	active := e.activeScreenIDs()
	screenIDs := make([]string, 0, len(e.screens))
	for screenID := range e.screens {
		screenIDs = append(screenIDs, screenID)
	}
	sort.Strings(screenIDs)
	runtimeScreens := make([]model.RuntimeScreen, 0, len(screenIDs))
	for _, screenID := range screenIDs {
		fsm := e.screenFSM[screenID]
		state := model.ScreenPending
		progress := 0
		if fsm != nil {
			state = fsm.State
			progress = fsm.Progress
		}
		runtimeScreens = append(runtimeScreens, model.RuntimeScreen{
			Screen:     e.screens[screenID].Clone(),
			State:      state,
			Active:     e.active[screenID],
			LastWindow: progress,
		})
	}
	var source *model.Rect
	if e.lastSource != nil {
		copyRect := *e.lastSource
		source = &copyRect
	}
	return model.RuntimeSnapshot{
		Window:              window,
		SessionState:        e.sessionFSM.State,
		ActiveScreens:       active,
		Screens:             runtimeScreens,
		TopologyVersion:     topologyVersion,
		LastSourceRegion:    source,
		LastCommittedWindow: e.lastWindow,
		EventCursor:         e.eventCursor,
	}
}

func (e *Engine) FinalSnapshot() model.RuntimeSnapshot {
	return e.snapshot(e.lastWindow, e.topology)
}

func (e *Engine) Pause() *model.KernelError {
	if e.sessionFSM.State != model.SessionStreaming {
		return &model.KernelError{
			Code:        model.CodeState,
			Message:     "pause is only legal while streaming",
			State:       string(e.sessionFSM.State),
			TargetState: string(model.SessionPaused),
			Progress:    e.sessionFSM.Progress,
		}
	}
	return e.sessionFSM.Transition(model.SessionPaused)
}

func (e *Engine) End() *model.KernelError {
	return e.sessionFSM.Transition(model.SessionEnded)
}

func (e *Engine) Window() int {
	return e.lastWindow
}

func (e *Engine) EventCursor() int {
	return e.eventCursor
}

func (e *Engine) WindowNumbers() []int {
	out := make([]int, 0, len(e.plans))
	for _, plan := range e.plans {
		out = append(out, plan.Window)
	}
	return out
}

func buildPackets(sessionID string, window int, screen model.Screen, mapped model.MappedRegion, replay bool, sourceWindow, startSeq int) ([]model.Packet, *model.KernelError) {
	region := mapped.Pixels
	if region.W <= 0 || region.H <= 0 {
		return nil, &model.KernelError{
			Code:     model.CodeParam,
			Message:  "cannot packetize an empty region",
			Window:   window,
			ScreenID: screen.ID,
			Field:    "packets.coverage",
		}
	}
	bpp := screen.Chroma.BytesPerPixel()
	maxPacket := int64(screen.Capability.MaxPacketSize)
	maxPixels := maxPacket / bpp
	if maxPixels < 1 {
		return nil, &model.KernelError{
			Code:     model.CodeParam,
			Message:  "max packet size cannot hold one pixel",
			Window:   window,
			ScreenID: screen.ID,
			Field:    "capability.max_packet_size",
		}
	}
	columnWidth := region.W
	if columnWidth > maxPixels {
		columnWidth = maxPixels
	}
	var packets []model.Packet
	sequence := startSeq
	for y := int64(0); y < region.H; {
		rows := int64(1)
		if region.W <= maxPixels {
			rows = maxPixels / region.W
			if rows < 1 {
				rows = 1
			}
		}
		remainingRows := region.H - y
		if rows > remainingRows {
			rows = remainingRows
		}
		for x := int64(0); x < region.W; x += columnWidth {
			width := columnWidth
			if x+width > region.W {
				width = region.W - x
			}
			area, err := model.CheckedMul("packet.area", width, rows)
			if err != nil {
				return nil, err
			}
			byteLength, err := model.CheckedMul("packet.bytes", area, bpp)
			if err != nil {
				return nil, err
			}
			if byteLength > maxPacket {
				return nil, &model.KernelError{
					Code:     model.CodeSegment,
					Message:  "packet split exceeded max packet size",
					Window:   window,
					ScreenID: screen.ID,
					Field:    "packets.byte_length",
					Operands: []int64{byteLength, maxPacket},
				}
			}
			coverage := model.PixelRect{X: region.X + x, Y: region.Y + y, W: width, H: rows}
			hashInput := fmt.Sprintf("%s|%d|%s|%d|%t|%d|%d,%d,%d,%d|%d|%d",
				sessionID, window, screen.ID, sourceWindow, replay, sequence,
				coverage.X, coverage.Y, coverage.W, coverage.H, byteLength, screen.Chroma)
			sum := sha256.Sum256([]byte(hashInput))
			packets = append(packets, model.Packet{
				Sequence:     sequence,
				Coverage:     coverage,
				ByteLength:   byteLength,
				PayloadHash:  hex.EncodeToString(sum[:]),
				Replay:       replay,
				SourceWindow: sourceWindow,
			})
			sequence++
		}
		y += rows
	}
	return packets, nil
}
