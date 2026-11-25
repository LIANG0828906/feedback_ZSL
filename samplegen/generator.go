package samplegen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"example.com/multiscreen-cast/protocol"
)

type Options struct {
	Seed     int64
	Screens  int
	Windows  int
	Sessions int
}

func (o Options) normalized() (Options, error) {
	if o.Screens <= 0 || o.Screens > 8 {
		return o, fmt.Errorf("screens must be between 1 and 8")
	}
	if o.Windows <= 0 || o.Windows > 10000 {
		return o, fmt.Errorf("windows must be between 1 and 10000")
	}
	if o.Sessions <= 0 || o.Sessions > 1000 {
		return o, fmt.Errorf("sessions must be between 1 and 1000")
	}
	return o, nil
}

type random struct {
	state uint64
}

func newRandom(seed int64) *random {
	return &random{state: uint64(seed) ^ 0x9e3779b97f4a7c15}
}

func (r *random) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *random) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

type preset struct {
	resolution protocol.Resolution
	scale      protocol.Ratio
	crop       protocol.CropMode
}

var presets = []preset{
	{resolution: protocol.Resolution{Width: 1920, Height: 1080}, scale: protocol.NewRatio(1, 2), crop: protocol.CropFloor},
	{resolution: protocol.Resolution{Width: 2560, Height: 1440}, scale: protocol.NewRatio(2, 3), crop: protocol.CropCeil},
	{resolution: protocol.Resolution{Width: 1280, Height: 720}, scale: protocol.NewRatio(1, 3), crop: protocol.CropNearest},
	{resolution: protocol.Resolution{Width: 3840, Height: 2160}, scale: protocol.NewRatio(1, 1), crop: protocol.CropOutward},
	{resolution: protocol.Resolution{Width: 1600, Height: 900}, scale: protocol.NewRatio(5, 12), crop: protocol.CropInward},
	{resolution: protocol.Resolution{Width: 2560, Height: 1080}, scale: protocol.NewRatio(2, 3), crop: protocol.CropFloor},
	{resolution: protocol.Resolution{Width: 1920, Height: 1200}, scale: protocol.NewRatio(1, 2), crop: protocol.CropCeil},
	{resolution: protocol.Resolution{Width: 1280, Height: 800}, scale: protocol.NewRatio(1, 3), crop: protocol.CropNearest},
}

func Generate(options Options) (*protocol.SampleSet, error) {
	options, err := options.normalized()
	if err != nil {
		return nil, err
	}
	set := &protocol.SampleSet{
		Seed:    options.Seed,
		Screens: options.Screens,
		Windows: options.Windows,
	}
	for i := 0; i < options.Sessions; i++ {
		session, truth, err := generateSession(options, i)
		if err != nil {
			return nil, err
		}
		set.Sessions = append(set.Sessions, *session)
		set.Truths = append(set.Truths, *truth)
	}
	return set, nil
}

func WriteFile(path string, options Options) (*protocol.SampleSet, error) {
	set, err := Generate(options)
	if err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return nil, err
	}
	return set, nil
}

func generateSession(options Options, index int) (*protocol.Session, *protocol.Truth, error) {
	rng := newRandom(options.Seed*1009 + int64(index)*7919)
	sessionID := fmt.Sprintf("session-%06d-%02d", options.Seed, index)
	master := protocol.Endpoint{
		ID: "master",
		Topology: protocol.Topology{
			X: 0, Y: 0, Width: 3840, Height: 2160,
			Resolution: protocol.Resolution{Width: 3840, Height: 2160},
			Scale:      protocol.NewRatio(1, 1),
		},
		Capability: protocol.Capability{
			Resolution: protocol.Resolution{Width: 3840, Height: 2160},
			Scale:      protocol.NewRatio(1, 1),
			CropMode:   protocol.CropNearest,
		},
	}
	displays := make([]protocol.Endpoint, options.Screens)
	for i := 0; i < options.Screens; i++ {
		p := presets[i%len(presets)]
		width := int(int64(p.resolution.Width) * p.scale.Den / p.scale.Num)
		height := int(int64(p.resolution.Height) * p.scale.Den / p.scale.Num)
		displays[i] = protocol.Endpoint{
			ID: fmt.Sprintf("screen-%c", 'a'+i),
			Topology: protocol.Topology{
				X:          (rng.intn(8) * 47),
				Y:          (rng.intn(6) * 43),
				Width:      width,
				Height:     height,
				Resolution: p.resolution,
				Scale:      p.scale,
			},
			Capability: protocol.Capability{
				Resolution: p.resolution,
				Scale:      p.scale,
				CropMode:   p.crop,
			},
		}
	}
	events := buildEvents(options, displays, rng)
	windows, windowTruths, _ := buildWindows(options, displays, events, rng)
	truthEvents, finalEndpoints, finalState := buildTruthEvents(events, windows, displays)
	session := &protocol.Session{
		SchemaVersion:    protocol.CurrentSchemaVersion,
		ID:               sessionID,
		AlgorithmVersion: protocol.CurrentAlgorithmVersion,
		Master:           master,
		Displays:         displays,
		InitialDisplays:  []string{displays[0].ID},
		Events:           events,
		Windows:          windows,
		TotalWindows:     len(windows),
	}
	if err := protocol.ValidateSession(session, "samplegen", "$"); err != nil {
		return nil, nil, fmt.Errorf("generated invalid session: %w", err)
	}
	truth := &protocol.Truth{
		SessionID:      sessionID,
		Windows:        windowTruths,
		EventEffects:   truthEvents,
		FinalEndpoints: finalEndpoints,
		FinalState:     finalState,
	}
	return session, truth, nil
}

func buildEvents(options Options, displays []protocol.Endpoint, rng *random) []protocol.Event {
	type spec struct {
		time      int64
		kind      protocol.EventKind
		displayID string
		topology  *protocol.Topology
	}
	specs := []spec{
		{time: 0, kind: protocol.EventNegotiate},
		{time: 1, kind: protocol.EventStart},
	}
	joinTime := int64(15)
	if options.Windows == 1 {
		joinTime = 5
	}
	for i := 1; i < len(displays); i++ {
		eventTime := joinTime
		switch {
		case i >= 5:
			eventTime = 45
		case i >= 3:
			eventTime = 35
		}
		if eventTime >= int64(options.Windows*10) {
			eventTime = int64(options.Windows*10) + 5
		}
		specs = append(specs, spec{time: eventTime, kind: protocol.EventJoin, displayID: displays[i].ID})
	}
	if len(displays) > 1 && options.Windows >= 3 {
		topology := displays[1].Topology
		topology.X += 64 + rng.intn(32)
		topology.Y += 32 + rng.intn(32)
		specs = append(specs, spec{time: 25, kind: protocol.EventTopology, displayID: displays[1].ID, topology: &topology})
	}
	if len(displays) > 2 && options.Windows >= 3 {
		topology := displays[2].Topology
		topology.X += 48 + rng.intn(32)
		specs = append(specs, spec{time: 27, kind: protocol.EventTopology, displayID: displays[2].ID, topology: &topology})
	}
	if len(displays) >= 4 && options.Windows >= 5 {
		specs = append(specs, spec{time: 45, kind: protocol.EventLeave, displayID: displays[0].ID})
	}
	if options.Windows >= 6 {
		specs = append(specs, spec{time: 55, kind: protocol.EventPause})
		specs = append(specs, spec{time: 56, kind: protocol.EventResume})
	}
	specs = append(specs, spec{time: int64(options.Windows*10) + 100, kind: protocol.EventEnd})
	sort.SliceStable(specs, func(i, j int) bool {
		if specs[i].time != specs[j].time {
			return specs[i].time < specs[j].time
		}
		leftPhase := eventSortPhase(specs[i].kind)
		rightPhase := eventSortPhase(specs[j].kind)
		if leftPhase != rightPhase {
			return leftPhase < rightPhase
		}
		return specs[i].displayID < specs[j].displayID
	})
	events := make([]protocol.Event, 0, len(specs))
	for i, item := range specs {
		event := protocol.Event{
			Seq:       int64(i + 1),
			Time:      item.time,
			Kind:      item.kind,
			DisplayID: item.displayID,
			Topology:  item.topology,
		}
		events = append(events, event)
	}
	return events
}

func eventSortPhase(kind protocol.EventKind) int {
	switch kind {
	case protocol.EventNegotiate:
		return 10
	case protocol.EventTopology:
		return 20
	case protocol.EventLeave:
		return 30
	case protocol.EventJoin:
		return 40
	default:
		return 50
	}
}

func buildWindows(options Options, displays []protocol.Endpoint, events []protocol.Event,
	rng *random) ([]protocol.FrameWindow, []protocol.WindowTruth, map[string]protocol.Endpoint) {
	active := map[string]bool{displays[0].ID: true}
	displayByID := make(map[string]protocol.Endpoint, len(displays))
	for _, display := range displays {
		displayByID[display.ID] = display
	}
	eventIndex := 0
	windows := make([]protocol.FrameWindow, 0, options.Windows)
	truths := make([]protocol.WindowTruth, 0, options.Windows)
	for i := 0; i < options.Windows; i++ {
		windowTime := int64((i + 1) * 10)
		for eventIndex < len(events) && events[eventIndex].Time <= windowTime &&
			eventSortPhase(events[eventIndex].Kind) < 60 {
			event := events[eventIndex]
			switch event.Kind {
			case protocol.EventJoin:
				active[event.DisplayID] = true
			case protocol.EventLeave:
				delete(active, event.DisplayID)
			case protocol.EventTopology:
				display := displayByID[event.DisplayID]
				display.Topology = *event.Topology
				if event.Capability != nil {
					display.Capability = *event.Capability
				}
				displayByID[event.DisplayID] = display
			}
			eventIndex++
		}
		targets := sortedKeys(active)
		x := rng.intn(620)
		y := rng.intn(360)
		width := 1900 + rng.intn(1700)
		height := 1100 + rng.intn(900)
		if x+width > 3840 {
			width = 3840 - x
		}
		if y+height > 2160 {
			height = 2160 - y
		}
		region := protocol.Rect{X: x, Y: y, Width: width, Height: height}
		window := protocol.FrameWindow{
			Index:   i + 1,
			Time:    windowTime,
			Region:  region,
			Targets: append([]string(nil), targets...),
			Interactions: []protocol.Interaction{{
				Seq:      int64(i + 1),
				Time:     windowTime,
				Kind:     "pointer_move",
				LogicalX: x + width/2,
				LogicalY: y + height/2,
			}},
		}
		windows = append(windows, window)
		screens := make([]protocol.ScreenTruth, 0, len(targets))
		for _, id := range targets {
			screen := truthMap(displayByID[id], region, i+1)
			screens = append(screens, screen)
		}
		truths = append(truths, protocol.WindowTruth{
			Window: i + 1, Time: windowTime, Endpoints: append([]string(nil), targets...), Screens: screens,
		})
	}
	return windows, truths, displayByID
}

func buildTruthEvents(events []protocol.Event, windows []protocol.FrameWindow,
	displays []protocol.Endpoint) ([]protocol.EventTruth, []string, protocol.SessionState) {
	active := map[string]bool{displays[0].ID: true}
	displayByID := make(map[string]protocol.Endpoint, len(displays))
	for _, display := range displays {
		displayByID[display.ID] = display
	}
	state := protocol.StateEstablished
	var truths []protocol.EventTruth
	for _, event := range events {
		before := state
		var replay *protocol.ScreenTruth
		replayWindow := 0
		committedWindow := 0
		for i := range windows {
			if windows[i].Time < event.Time {
				committedWindow = i + 1
			}
		}
		switch event.Kind {
		case protocol.EventNegotiate:
			state = protocol.StateNegotiating
		case protocol.EventStart:
			state = protocol.StateCasting
		case protocol.EventPause:
			state = protocol.StatePaused
		case protocol.EventResume:
			state = protocol.StateCasting
		case protocol.EventEnd:
			state = protocol.StateEnded
		case protocol.EventJoin:
			if (before == protocol.StateCasting || before == protocol.StatePaused) && len(windows) > 0 {
				latest := -1
				for i := range windows {
					if windows[i].Time < event.Time {
						latest = i
					}
				}
				if latest >= 0 {
					display := displayByID[event.DisplayID]
					screen := truthMap(display, windows[latest].Region, latest+1)
					replay = &screen
					replayWindow = latest + 1
				}
			}
			active[event.DisplayID] = true
		case protocol.EventLeave:
			delete(active, event.DisplayID)
		case protocol.EventTopology:
			display := displayByID[event.DisplayID]
			display.Topology = *event.Topology
			if event.Capability != nil {
				display.Capability = *event.Capability
			}
			displayByID[event.DisplayID] = display
		}
		truths = append(truths, protocol.EventTruth{
			Seq:             event.Seq,
			Time:            event.Time,
			Kind:            event.Kind,
			DisplayID:       event.DisplayID,
			EffectiveTime:   event.Time,
			CommittedWindow: committedWindow,
			ReplayWindow:    replayWindow,
			EndpointsAfter:  sortedKeys(active),
			StateAfter:      state,
			Replay:          replay,
		})
	}
	return truths, sortedKeys(active), state
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func truthMap(display protocol.Endpoint, region protocol.Rect, sourceWindow int) protocol.ScreenTruth {
	topology := display.Topology
	scale := display.Capability.Scale
	left := max(region.X, topology.X)
	top := max(region.Y, topology.Y)
	right := min(region.X+region.Width, topology.X+topology.Width)
	bottom := min(region.Y+region.Height, topology.Y+topology.Height)
	if right <= left || bottom <= top {
		return protocol.ScreenTruth{DisplayID: display.ID, SourceWindow: sourceWindow}
	}
	localLeft := int64(left - topology.X)
	localTop := int64(top - topology.Y)
	localRight := int64(right - topology.X)
	localBottom := int64(bottom - topology.Y)
	mode := display.Capability.CropMode
	pxLeft := truthScale(leftEdgeMode(mode), localLeft, scale)
	pxRight := truthScale(rightEdgeMode(mode), localRight, scale)
	pyTop := truthScale(leftEdgeMode(mode), localTop, scale)
	pyBottom := truthScale(rightEdgeMode(mode), localBottom, scale)
	pxLeft = clamp(pxLeft, 0, int64(display.Capability.Resolution.Width))
	pxRight = clamp(pxRight, 0, int64(display.Capability.Resolution.Width))
	pyTop = clamp(pyTop, 0, int64(display.Capability.Resolution.Height))
	pyBottom = clamp(pyBottom, 0, int64(display.Capability.Resolution.Height))
	if pxRight < pxLeft {
		pxRight = pxLeft
	}
	if pyBottom < pyTop {
		pyBottom = pyTop
	}
	return protocol.ScreenTruth{
		DisplayID: display.ID,
		Pixel: protocol.Rect{
			X: int(pxLeft), Y: int(pyTop), Width: int(pxRight - pxLeft), Height: int(pyBottom - pyTop),
		},
		World: protocol.Rect{X: left, Y: top, Width: right - left, Height: bottom - top},
		ScreenWorld: protocol.Rect{
			X: int(localLeft), Y: int(localTop), Width: int(localRight - localLeft), Height: int(localBottom - localTop),
		},
		SourceWindow: sourceWindow,
	}
}

func leftEdgeMode(mode protocol.CropMode) protocol.CropMode {
	switch mode {
	case protocol.CropOutward:
		return protocol.CropFloor
	case protocol.CropInward:
		return protocol.CropCeil
	default:
		return mode
	}
}

func rightEdgeMode(mode protocol.CropMode) protocol.CropMode {
	switch mode {
	case protocol.CropOutward:
		return protocol.CropCeil
	case protocol.CropInward:
		return protocol.CropFloor
	default:
		return mode
	}
}

func truthScale(mode protocol.CropMode, value int64, scale protocol.Ratio) int64 {
	n := value * scale.Num
	d := scale.Den
	switch mode {
	case protocol.CropCeil:
		q := n / d
		if n%d != 0 {
			q++
		}
		return q
	case protocol.CropNearest:
		if n >= 0 {
			return (n + d/2) / d
		}
		return -((-n + d/2) / d)
	default:
		q := n / d
		if n < 0 && n%d != 0 {
			q--
		}
		return q
	}
}

func clamp(value, low, high int64) int64 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
