package lightshow

import (
	"encoding/json"
	"fmt"
	"sort"
)

type Param string

const (
	ParamColorR    Param = "color.r"
	ParamColorG    Param = "color.g"
	ParamColorB    Param = "color.b"
	ParamIntensity Param = "intensity"
	ParamPosX      Param = "position.x"
	ParamPosY      Param = "position.y"
	ParamPosZ      Param = "position.z"
	ParamFocus     Param = "focus"
)

var parameterOrder = []Param{
	ParamColorR, ParamColorG, ParamColorB, ParamIntensity,
	ParamPosX, ParamPosY, ParamPosZ, ParamFocus,
}

type LightOutput struct {
	FixtureID string `json:"fixture_id"`
	Color     RGB    `json:"color"`
	Intensity int    `json:"intensity"`
	Position  [3]int `json:"position"`
	Focus     int    `json:"focus"`
}

type FrameOutput struct {
	Frame  int           `json:"frame"`
	AtUS   int64         `json:"at_us"`
	Lights []LightOutput `json:"lights"`
}

type Candidate struct {
	Source   string `json:"source"`
	Priority int    `json:"priority"`
	Value    int    `json:"value"`
	ChangeID string `json:"change_id"`
}

type Decision struct {
	Frame      int         `json:"frame"`
	AtUS       int64       `json:"at_us"`
	FixtureID  string      `json:"fixture_id"`
	Parameter  string      `json:"parameter"`
	Winner     Candidate   `json:"winner"`
	Overridden []Candidate `json:"overridden"`
	Basis      string      `json:"basis"`
}

type ChangeConsumption struct {
	Ordinal  int    `json:"ordinal"`
	ChangeID string `json:"change_id"`
	AtUS     int64  `json:"at_us"`
	Frame    int64  `json:"frame"`
}

type GroupSnapshotEntry struct {
	ID       string   `json:"id"`
	Priority int      `json:"priority"`
	Enabled  bool     `json:"enabled"`
	Mode     string   `json:"mode"`
	Fixtures []string `json:"fixtures"`
}

type GroupSnapshot struct {
	Groups []GroupSnapshotEntry `json:"groups"`
}

type Dependency struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}

type WindowResult struct {
	WindowID          string              `json:"window_id"`
	Index             int                 `json:"index"`
	StartUS           int64               `json:"start_us"`
	EndUS             int64               `json:"end_us"`
	FrameStart        int64               `json:"frame_start"`
	FrameEnd          int64               `json:"frame_end"`
	Outputs           []FrameOutput       `json:"outputs"`
	Decisions         []Decision          `json:"decisions"`
	Consumption       []ChangeConsumption `json:"consumption"`
	GroupSnapshot     GroupSnapshot       `json:"group_snapshot"`
	Dependencies      []Dependency        `json:"dependencies"`
	ChangeOffsetStart int                 `json:"change_offset_start"`
	ChangeOffsetEnd   int                 `json:"change_offset_end"`
	ContentHash       string              `json:"content_hash"`
}

type ScheduleResult struct {
	Frames      []FrameOutput
	Decisions   []Decision
	Consumption []ChangeConsumption
	Windows     []WindowResult
}

type windowPlan struct {
	id         string
	index      int
	startUS    int64
	endUS      int64
	frameStart int64
	frameEnd   int64
	deps       []Dependency
}

type trackKey struct {
	kind  string
	id    string
	param Param
}

type trackUpdate struct {
	changeID string
	atUS     int64
	value    int
	interp   Interpolation
}

type track struct {
	key     trackKey
	updates []trackUpdate
}

type candidateValue struct {
	candidate Candidate
	groupMode string
}

type compiledShow struct {
	show           *Show
	tracks         map[trackKey]*track
	memberships    map[string]map[string]bool
	groupOrder     []string
	groupByID      map[string]Group
	fixtureOrder   []string
	fixtureByID    map[string]Fixture
	durationFrames int64
}

func newCompiledShow(show *Show) (*compiledShow, error) {
	work := show.Clone()
	if work == nil {
		return nil, engineError(CodeVersion, "project", "show is nil")
	}
	if err := work.Normalize(); err != nil {
		return nil, err
	}
	show = work
	c := &compiledShow{
		show:           show,
		tracks:         make(map[trackKey]*track),
		memberships:    make(map[string]map[string]bool),
		groupByID:      make(map[string]Group),
		fixtureByID:    make(map[string]Fixture),
		durationFrames: show.DurationFrames(),
	}
	for _, f := range show.Fixtures {
		c.fixtureOrder = append(c.fixtureOrder, f.ID)
		c.fixtureByID[f.ID] = f
	}
	for _, g := range show.Groups {
		c.groupOrder = append(c.groupOrder, g.ID)
		c.groupByID[g.ID] = g
	}
	sort.Strings(c.groupOrder)
	for _, g := range show.Groups {
		fixtures, err := show.ResolveGroupFixtures(g.ID)
		if err != nil {
			return nil, err
		}
		set := make(map[string]bool, len(fixtures))
		for _, id := range fixtures {
			set[id] = true
		}
		c.memberships[g.ID] = set
	}
	for i := range show.Changes {
		ch := &show.Changes[i]
		for param, value := range valuesToParams(ch.Values) {
			key := trackKey{kind: ch.TargetKind, id: ch.TargetID, param: param}
			t := c.tracks[key]
			if t == nil {
				t = &track{key: key}
				c.tracks[key] = t
			}
			t.updates = append(t.updates, trackUpdate{
				changeID: ch.ID, atUS: ch.AtUS, value: value, interp: ch.Interpolation,
			})
		}
	}
	for _, t := range c.tracks {
		sort.SliceStable(t.updates, func(i, j int) bool {
			return t.updates[i].atUS < t.updates[j].atUS
		})
	}
	return c, nil
}

func valuesToParams(v Values) map[Param]int {
	out := make(map[Param]int, 8)
	if v.Color != nil {
		out[ParamColorR] = int(v.Color.R)
		out[ParamColorG] = int(v.Color.G)
		out[ParamColorB] = int(v.Color.B)
	}
	if v.Intensity != nil {
		out[ParamIntensity] = *v.Intensity
	}
	if v.Position != nil {
		out[ParamPosX] = v.Position[0]
		out[ParamPosY] = v.Position[1]
		out[ParamPosZ] = v.Position[2]
	}
	if v.Focus != nil {
		out[ParamFocus] = *v.Focus
	}
	return out
}

func (t *track) at(atUS int64) (value int, changeID string, current, next *trackUpdate) {
	idx := sort.Search(len(t.updates), func(i int) bool {
		return t.updates[i].atUS > atUS
	}) - 1
	if idx < 0 {
		return 0, "", nil, nil
	}
	current = &t.updates[idx]
	value = current.value
	changeID = current.changeID
	if idx+1 < len(t.updates) {
		next = &t.updates[idx+1]
		changeID = next.changeID
		if next.interp == Linear {
			value = interpolateInt(current.value, next.value, atUS-current.atUS, next.atUS-current.atUS)
		}
	}
	return value, changeID, current, next
}

func (c *compiledShow) baseline(fixtureID string, param Param, atUS int64) int {
	f := c.fixtureByID[fixtureID]
	for i := len(c.show.Scenes) - 1; i >= 0; i-- {
		scene := &c.show.Scenes[i]
		if scene.AtUS > atUS {
			continue
		}
		if values, ok := scene.Values[fixtureID]; ok {
			if v, ok := valuesToParams(values)[param]; ok {
				return v
			}
		}
	}
	switch param {
	case ParamColorR:
		return int(f.Color.R)
	case ParamColorG:
		return int(f.Color.G)
	case ParamColorB:
		return int(f.Color.B)
	case ParamIntensity:
		return f.Intensity
	case ParamPosX:
		return f.Position[0]
	case ParamPosY:
		return f.Position[1]
	case ParamPosZ:
		return f.Position[2]
	case ParamFocus:
		return f.Focus
	}
	return 0
}

func (c *compiledShow) candidates(fixtureID string, param Param, atUS int64) []candidateValue {
	out := make([]candidateValue, 0, 4)
	fixture := c.fixtureByID[fixtureID]
	fixtureKey := trackKey{kind: "fixture", id: fixtureID, param: param}
	if t := c.tracks[fixtureKey]; t != nil {
		if value, changeID, _, _ := t.at(atUS); changeID != "" {
			out = append(out, candidateValue{candidate: Candidate{
				Source: "fixture:" + fixtureID, Priority: 1 << 30, Value: c.clamp(fixture, param, value), ChangeID: changeID,
			}})
		}
	}
	for _, groupID := range c.groupOrder {
		g := c.groupByID[groupID]
		if !g.Enabled || !c.memberships[groupID][fixtureID] {
			continue
		}
		key := trackKey{kind: "group", id: groupID, param: param}
		t := c.tracks[key]
		if t == nil {
			continue
		}
		if value, changeID, _, _ := t.at(atUS); changeID != "" {
			out = append(out, candidateValue{
				candidate: Candidate{Source: "group:" + groupID, Priority: g.Priority, Value: c.clamp(fixture, param, value), ChangeID: changeID},
				groupMode: g.Mode,
			})
		}
	}
	return out
}

func chooseCandidate(candidates []candidateValue) (winner candidateValue, overridden []Candidate, basis string) {
	winner = candidates[0]
	for _, cand := range candidates[1:] {
		if cand.candidate.Priority > winner.candidate.Priority ||
			(cand.candidate.Priority == winner.candidate.Priority && cand.candidate.Source < winner.candidate.Source) {
			winner = cand
		}
	}
	for _, cand := range candidates {
		if cand.candidate.Source != winner.candidate.Source {
			overridden = append(overridden, cand.candidate)
		}
	}
	sort.Slice(overridden, func(i, j int) bool {
		if overridden[i].Priority != overridden[j].Priority {
			return overridden[i].Priority > overridden[j].Priority
		}
		return overridden[i].Source < overridden[j].Source
	})
	basis = "group_priority"
	if winner.candidate.Priority == 1<<30 {
		basis = "fixture_override"
	}
	return winner, overridden, basis
}

func (c *compiledShow) clamp(fixture Fixture, param Param, value int) int {
	switch param {
	case ParamColorR, ParamColorG, ParamColorB:
		if value < 0 {
			return 0
		}
		if value > 65535 {
			return 65535
		}
		return value
	case ParamIntensity:
		return fixture.Ranges.Intensity.Clamp(value)
	case ParamPosX:
		return fixture.Ranges.Position[0].Clamp(value)
	case ParamPosY:
		return fixture.Ranges.Position[1].Clamp(value)
	case ParamPosZ:
		return fixture.Ranges.Position[2].Clamp(value)
	case ParamFocus:
		return fixture.Ranges.Focus.Clamp(value)
	default:
		return value
	}
}

func (c *compiledShow) compileFrame(frame int) FrameOutput {
	atUS, _ := FrameToUS(int64(frame), c.show.FPS, c.show.TimeRounding)
	frameOut := FrameOutput{Frame: frame, AtUS: atUS, Lights: make([]LightOutput, 0, len(c.fixtureOrder))}
	for _, fixtureID := range c.fixtureOrder {
		fixture := c.fixtureByID[fixtureID]
		values := make(map[Param]int, len(parameterOrder))
		for _, param := range parameterOrder {
			values[param] = c.baseline(fixtureID, param, atUS)
		}
		light := LightOutput{FixtureID: fixtureID}
		for _, param := range parameterOrder {
			cands := c.candidates(fixtureID, param, atUS)
			if len(cands) > 0 {
				winner, _, _ := chooseCandidate(cands)
				values[param] = winner.candidate.Value
			}
			values[param] = c.clamp(fixture, param, values[param])
		}
		light.Color = RGB{R: uint16(values[ParamColorR]), G: uint16(values[ParamColorG]), B: uint16(values[ParamColorB])}
		light.Intensity = values[ParamIntensity]
		light.Position = [3]int{values[ParamPosX], values[ParamPosY], values[ParamPosZ]}
		light.Focus = values[ParamFocus]
		frameOut.Lights = append(frameOut.Lights, light)
	}
	return frameOut
}

func (c *compiledShow) compileDecisions(frame FrameOutput) []Decision {
	var out []Decision
	for _, fixtureID := range c.fixtureOrder {
		for _, param := range parameterOrder {
			cands := c.candidates(fixtureID, param, frame.AtUS)
			if len(cands) < 2 {
				continue
			}
			winner, overridden, basis := chooseCandidate(cands)
			out = append(out, Decision{
				Frame: frame.Frame, AtUS: frame.AtUS, FixtureID: fixtureID,
				Parameter: string(param), Winner: winner.candidate,
				Overridden: overridden, Basis: basis,
			})
		}
	}
	return out
}

func (c *compiledShow) groupSnapshot() GroupSnapshot {
	snapshot := GroupSnapshot{Groups: make([]GroupSnapshotEntry, 0, len(c.groupOrder))}
	for _, id := range c.groupOrder {
		g := c.groupByID[id]
		fixtures, _ := c.show.ResolveGroupFixtures(id)
		snapshot.Groups = append(snapshot.Groups, GroupSnapshotEntry{
			ID: g.ID, Priority: g.Priority, Enabled: g.Enabled, Mode: g.Mode, Fixtures: fixtures,
		})
	}
	return snapshot
}

func (c *compiledShow) dependencies(startUS, endUS int64, frameStart, frameEnd int64, windowIndex int) []Dependency {
	set := make(map[Dependency]bool)
	add := func(kind, key string) {
		if key != "" {
			set[Dependency{Kind: kind, Key: key}] = true
		}
	}
	lastFrameUS, _ := FrameToUS(frameEnd-1, c.show.FPS, c.show.TimeRounding)
	if frameEnd <= frameStart {
		lastFrameUS = startUS
	}

	sources := make([]trackKey, 0, len(c.tracks))
	for key := range c.tracks {
		if key.kind == "fixture" {
			sources = append(sources, key)
			continue
		}
		if g, ok := c.groupByID[key.id]; ok && g.Enabled {
			sources = append(sources, key)
		}
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].kind != sources[j].kind {
			return sources[i].kind < sources[j].kind
		}
		if sources[i].id != sources[j].id {
			return sources[i].id < sources[j].id
		}
		return sources[i].param < sources[j].param
	})
	for _, key := range sources {
		t := c.tracks[key]
		idx := sort.Search(len(t.updates), func(i int) bool {
			return t.updates[i].atUS > lastFrameUS
		}) - 1
		if idx < 0 {
			continue
		}
		current := t.updates[idx]
		add("change", current.changeID)
		add("interval", fmt.Sprintf("%s:%s:%s", key.kind+"+"+key.id, key.param, current.changeID))
		if idx+1 < len(t.updates) {
			next := t.updates[idx+1]
			add("change", next.changeID)
			add("interval", fmt.Sprintf("%s:%s:%s->%s", key.kind+"+"+key.id, key.param, current.changeID, next.changeID))
		}
		if key.kind == "group" {
			add("group", key.id)
		}
	}
	for _, ch := range c.show.Changes {
		if ch.AtUS >= startUS && ch.AtUS < endUS {
			add("change", ch.ID)
		}
	}
	for _, g := range c.show.Groups {
		if g.Enabled {
			add("group", g.ID)
		}
	}
	for i := len(c.show.Scenes) - 1; i >= 0; i-- {
		scene := c.show.Scenes[i]
		if scene.AtUS <= lastFrameUS {
			add("scene", scene.ID)
			break
		}
	}
	add("export", windowID(windowIndex))
	out := make([]Dependency, 0, len(set))
	for dep := range set {
		out = append(out, dep)
	}
	sortDependencies(out)
	return out
}

func (c *compiledShow) planWindow(index int) windowPlan {
	startUS := int64(index) * c.show.WindowUS
	endUS := startUS + c.show.WindowUS
	if endUS > c.show.DurationUS {
		endUS = c.show.DurationUS
	}
	frameStart := frameAtOrAfter(startUS, c.show.FPS, c.show.TimeRounding)
	frameEnd := frameAtOrAfter(endUS, c.show.FPS, c.show.TimeRounding)
	if frameStart < 0 {
		frameStart = 0
	}
	if frameEnd > c.durationFrames {
		frameEnd = c.durationFrames
	}
	if frameEnd < frameStart {
		frameEnd = frameStart
	}
	return windowPlan{
		id: windowID(index), index: index, startUS: startUS, endUS: endUS,
		frameStart: frameStart, frameEnd: frameEnd,
		deps: c.dependencies(startUS, endUS, frameStart, frameEnd, index),
	}
}

func (c *compiledShow) allWindowPlans() []windowPlan {
	count := int(c.show.WindowCount())
	plans := make([]windowPlan, 0, count)
	for i := 0; i < count; i++ {
		plans = append(plans, c.planWindow(i))
	}
	return plans
}

func (c *compiledShow) compileWindow(plan windowPlan, snapshot GroupSnapshot) WindowResult {
	w := WindowResult{
		WindowID: plan.id, Index: plan.index, StartUS: plan.startUS, EndUS: plan.endUS,
		FrameStart: plan.frameStart, FrameEnd: plan.frameEnd, GroupSnapshot: snapshot,
		Dependencies: plan.deps,
	}
	for _, ch := range c.show.Changes {
		if ch.AtUS >= plan.startUS && ch.AtUS < plan.endUS && ch.AtUS < c.show.DurationUS {
			frame, _ := USToFrame(ch.AtUS, c.show.FPS, c.show.TimeRounding)
			w.Consumption = append(w.Consumption, ChangeConsumption{
				Ordinal: len(w.Consumption) + 1, ChangeID: ch.ID, AtUS: ch.AtUS, Frame: frame,
			})
		}
	}
	if len(w.Consumption) > 0 {
		w.ChangeOffsetStart = 1
		w.ChangeOffsetEnd = len(w.Consumption)
	}
	for fi := plan.frameStart; fi < plan.frameEnd; fi++ {
		frame := c.compileFrame(int(fi))
		w.Outputs = append(w.Outputs, frame)
		w.Decisions = append(w.Decisions, c.compileDecisions(frame)...)
	}
	w.ContentHash = w.ComputeContentHash()
	return w
}

// Schedule computes the complete deterministic timeline and its windows.
func Schedule(show *Show) (*ScheduleResult, error) {
	c, err := newCompiledShow(show)
	if err != nil {
		return nil, err
	}
	result := &ScheduleResult{}
	frames := make([]FrameOutput, 0, c.durationFrames)
	for i := int64(0); i < c.durationFrames; i++ {
		frame := c.compileFrame(int(i))
		frames = append(frames, frame)
		result.Frames = append(result.Frames, frame)
		result.Decisions = append(result.Decisions, c.compileDecisions(frame)...)
	}
	var consumed int
	for i, ch := range c.show.Changes {
		if ch.AtUS < 0 || ch.AtUS >= c.show.DurationUS {
			continue
		}
		frame, _ := USToFrame(ch.AtUS, c.show.FPS, c.show.TimeRounding)
		result.Consumption = append(result.Consumption, ChangeConsumption{
			Ordinal: consumed + 1, ChangeID: ch.ID, AtUS: ch.AtUS, Frame: frame,
		})
		consumed++
		_ = i
	}

	snapshot := c.groupSnapshot()
	for _, plan := range c.allWindowPlans() {
		result.Windows = append(result.Windows, c.compileWindow(plan, snapshot))
	}
	return result, nil
}

// ScheduleWindows compiles only the requested window indices. It is used by
// the incremental path so unaffected windows are never re-evaluated.
func ScheduleWindows(show *Show, indices []int) ([]WindowResult, error) {
	c, err := newCompiledShow(show)
	if err != nil {
		return nil, err
	}
	count := int(c.show.WindowCount())
	snapshot := c.groupSnapshot()
	seen := make(map[int]bool, len(indices))
	out := make([]WindowResult, 0, len(indices))
	for _, index := range indices {
		if index < 0 || index >= count || seen[index] {
			continue
		}
		seen[index] = true
		out = append(out, c.compileWindow(c.planWindow(index), snapshot))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

// WindowDependencyPlans exposes the dependency plan without frame compilation.
func WindowDependencyPlans(show *Show) ([]WindowResult, error) {
	c, err := newCompiledShow(show)
	if err != nil {
		return nil, err
	}
	plans := c.allWindowPlans()
	out := make([]WindowResult, 0, len(plans))
	for _, plan := range plans {
		out = append(out, WindowResult{
			WindowID: plan.id, Index: plan.index, StartUS: plan.startUS, EndUS: plan.endUS,
			FrameStart: plan.frameStart, FrameEnd: plan.frameEnd, Dependencies: plan.deps,
		})
	}
	return out, nil
}

func frameAtOrAfter(us, fps int64, rounding TimeRounding) int64 {
	frame, _ := USToFrame(us, fps, rounding)
	for {
		got, _ := FrameToUS(frame, fps, rounding)
		if got >= us {
			return frame
		}
		frame++
	}
}

func windowID(index int) string {
	return fmt.Sprintf("window-%04d", index)
}

// DecodeWindowPayload validates roster JSON read from a checkpoint.
func DecodeWindowPayload(outputs, decisions, consumption, groups, dependencies []byte) (WindowResult, error) {
	var w WindowResult
	if err := json.Unmarshal(outputs, &w.Outputs); err != nil {
		return w, engineError(CodeDecode, "checkpoint.outputs", "invalid output payload: %v", err)
	}
	if err := json.Unmarshal(decisions, &w.Decisions); err != nil {
		return w, engineError(CodeDecode, "checkpoint.decisions", "invalid decision payload: %v", err)
	}
	if err := json.Unmarshal(consumption, &w.Consumption); err != nil {
		return w, engineError(CodeDecode, "checkpoint.consumption", "invalid consumption payload: %v", err)
	}
	if err := json.Unmarshal(groups, &w.GroupSnapshot); err != nil {
		return w, engineError(CodeDecode, "checkpoint.group_snapshot", "invalid group snapshot: %v", err)
	}
	if err := json.Unmarshal(dependencies, &w.Dependencies); err != nil {
		return w, engineError(CodeDecode, "checkpoint.dependencies", "invalid dependency payload: %v", err)
	}
	return w, nil
}
