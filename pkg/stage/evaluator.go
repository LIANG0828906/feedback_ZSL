package stage

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

type fieldID int

const (
	fieldR fieldID = iota
	fieldG
	fieldB
	fieldIntensity
	fieldPan
	fieldTilt
	fieldFocus
	fieldColorTemp
	fieldStrobeHz
	fieldCount
)

var fieldNames = [fieldCount]string{
	"rgb.r",
	"rgb.g",
	"rgb.b",
	"intensity",
	"pan",
	"tilt",
	"focus",
	"color_temp",
	"strobe_hz",
}

func (f fieldID) String() string {
	if f >= 0 && f < fieldCount {
		return fieldNames[f]
	}
	return fmt.Sprintf("field[%d]", int(f))
}

type fieldUpdate struct {
	field fieldID
	value float64
}

func cueUpdates(cue *Cue) []fieldUpdate {
	var out []fieldUpdate
	if cue.Values.RGB != nil {
		out = append(out,
			fieldUpdate{fieldR, cue.Values.RGB.R},
			fieldUpdate{fieldG, cue.Values.RGB.G},
			fieldUpdate{fieldB, cue.Values.RGB.B},
		)
	}
	add := func(field fieldID, value *float64) {
		if value != nil {
			out = append(out, fieldUpdate{field, *value})
		}
	}
	add(fieldIntensity, cue.Values.Intensity)
	add(fieldPan, cue.Values.Pan)
	add(fieldTilt, cue.Values.Tilt)
	add(fieldFocus, cue.Values.Focus)
	add(fieldColorTemp, cue.Values.ColorTemp)
	add(fieldStrobeHz, cue.Values.StrobeHz)
	return out
}

type sourceRank struct {
	scope         int
	groupPriority int
	cuePriority   int
	groupID       string
	sourceID      string
	cueID         string
	cueIndex      int
}

// winnerCompare returns >0 when left has higher business precedence.
// Single-light cues always outrank group cues. Group cues use group priority,
// cue priority, then lexicographic group id. The complete result never depends
// on input array order.
func winnerCompare(left, right sourceRank) int {
	if left.scope != right.scope {
		if left.scope > right.scope {
			return 1
		}
		return -1
	}
	if left.scope == 0 {
		if left.groupPriority != right.groupPriority {
			if left.groupPriority > right.groupPriority {
				return 1
			}
			return -1
		}
		if left.cuePriority != right.cuePriority {
			if left.cuePriority > right.cuePriority {
				return 1
			}
			return -1
		}
		if left.groupID != right.groupID {
			if left.groupID < right.groupID {
				return 1
			}
			return -1
		}
	}
	if left.cuePriority != right.cuePriority {
		if left.cuePriority > right.cuePriority {
			return 1
		}
		return -1
	}
	if left.cueID != right.cueID {
		if left.cueID < right.cueID {
			return 1
		}
		return -1
	}
	return 0
}

func applicationOrder(left, right sourceRank) bool {
	cmp := winnerCompare(left, right)
	if cmp != 0 {
		return cmp < 0
	}
	return left.cueIndex > right.cueIndex
}

type fieldEvent struct {
	atUS         int64
	start        float64
	target       float64
	transitionUS int64
	easing       Easing
	curvePower   float64
	rank         sourceRank
}

type lightTrack struct {
	light  *Light
	fields [fieldCount][]fieldEvent
}

type Evaluator struct {
	project     *Project
	contentHash string
	paramVer    string
	lights      []*lightTrack
	lightIndex  map[string]*lightTrack
	decisions   []Arbitration
	descendants map[string][]string
}

func NewEvaluator(p *Project, params Params) (*Evaluator, error) {
	params = params.WithDefaults(p)
	if err := params.Validate(); err != nil {
		return nil, err
	}
	if err := ValidateProject(p, ValidateOptions{CheckTruth: false, CheckConflict: true}); err != nil {
		return nil, err
	}
	lights := make(map[string]*Light, len(p.Lights))
	for i := range p.Lights {
		lights[p.Lights[i].ID] = &p.Lights[i]
	}
	groups := make(map[string]*Group, len(p.Groups))
	for i := range p.Groups {
		groups[p.Groups[i].ID] = &p.Groups[i]
	}
	descendants, err := groupMembership(p, groups)
	if err != nil {
		return nil, err
	}

	e := &Evaluator{
		project:     p,
		contentHash: ContentHash(p),
		paramVer:    ParamVersion(params),
		lightIndex:  make(map[string]*lightTrack, len(p.Lights)),
		descendants: descendants,
	}
	ids := sortedLightIDs(p)
	for _, id := range ids {
		track := &lightTrack{light: lights[id]}
		e.lights = append(e.lights, track)
		e.lightIndex[id] = track
	}

	rawEvents := make(map[string]map[fieldID][]fieldEvent)
	for cueIndex := range p.Cues {
		cue := &p.Cues[cueIndex]
		updates := cueUpdates(cue)
		if len(updates) == 0 {
			continue
		}
		targets := []string{cue.LightID}
		scope := 1
		groupPriority := 0
		if cue.GroupID != "" {
			targets = descendants[cue.GroupID]
			scope = 0
			groupPriority = groups[cue.GroupID].Priority
		}
		for _, lightID := range targets {
			if rawEvents[lightID] == nil {
				rawEvents[lightID] = make(map[fieldID][]fieldEvent)
			}
			for _, update := range updates {
				rawEvents[lightID][update.field] = append(rawEvents[lightID][update.field], fieldEvent{
					atUS:         cue.TimeMS * 1000,
					target:       update.value,
					transitionUS: cue.TransitionMS * 1000,
					easing:       cue.Easing,
					curvePower:   cue.CurvePower,
					rank: sourceRank{
						scope:         scope,
						groupPriority: groupPriority,
						cuePriority:   cue.Priority,
						groupID:       cue.GroupID,
						sourceID:      chooseNonEmpty(cue.LightID, cue.GroupID),
						cueID:         cue.ID,
						cueIndex:      cueIndex,
					},
				})
			}
		}
	}

	for lightID, fields := range rawEvents {
		track := e.lightIndex[lightID]
		for field := fieldID(0); field < fieldCount; field++ {
			events := fields[field]
			if len(events) == 0 {
				continue
			}
			sort.Slice(events, func(i, j int) bool {
				if events[i].atUS != events[j].atUS {
					return events[i].atUS < events[j].atUS
				}
				return applicationOrder(events[i].rank, events[j].rank)
			})
			previous := fieldEvent{
				atUS:       -1,
				start:      baseField(track.light.Base, field),
				target:     baseField(track.light.Base, field),
				easing:     EaseStep,
				curvePower: 1,
			}
			for i := 0; i < len(events); {
				j := i + 1
				for j < len(events) && events[j].atUS == events[i].atUS {
					j++
				}
				batchStart := evaluateFieldEvent(previous, events[i].atUS)
				for k := i; k < j; k++ {
					events[k].start = batchStart
				}
				previous = events[j-1]
				i = j
			}
			track.fields[field] = events
		}
	}
	e.decisions = buildArbitration(p, descendants, lights, groups)
	return e, nil
}

func chooseNonEmpty(left, right string) string {
	if left != "" {
		return left
	}
	return right
}

func baseField(base LightValues, field fieldID) float64 {
	switch field {
	case fieldR:
		return base.RGB.R
	case fieldG:
		return base.RGB.G
	case fieldB:
		return base.RGB.B
	case fieldIntensity:
		return base.Intensity
	case fieldPan:
		return base.Pan
	case fieldTilt:
		return base.Tilt
	case fieldFocus:
		return base.Focus
	case fieldColorTemp:
		return base.ColorTemp
	case fieldStrobeHz:
		return base.StrobeHz
	default:
		return 0
	}
}

func evaluateFieldEvent(event fieldEvent, timeUS int64) float64 {
	if event.atUS < 0 || timeUS < event.atUS {
		return event.start
	}
	if event.easing == EaseStep || event.transitionUS == 0 {
		return event.target
	}
	elapsed := timeUS - event.atUS
	if elapsed >= event.transitionUS {
		return event.target
	}
	u := float64(elapsed) / float64(event.transitionUS)
	switch event.easing {
	case EaseLinear:
	case EaseInOut:
		u = u * u * (3 - 2*u)
	default:
	}
	if event.curvePower > 0 && event.curvePower != 1 {
		u = math.Pow(u, event.curvePower)
	}
	return event.start + (event.target-event.start)*u
}

func (e *Evaluator) fieldValue(track *lightTrack, field fieldID, timeUS int64) float64 {
	events := track.fields[field]
	if len(events) == 0 {
		return baseField(track.light.Base, field)
	}
	index := sort.Search(len(events), func(i int) bool {
		return events[i].atUS > timeUS
	}) - 1
	if index < 0 {
		return baseField(track.light.Base, field)
	}
	return evaluateFieldEvent(events[index], timeUS)
}

func (e *Evaluator) Frame(frame int) FrameOutput {
	timeUS := FrameTimeUS(frame, e.project.FPS)
	return e.FrameAtUS(frame, timeUS)
}

func (e *Evaluator) FrameAtUS(frame int, timeUS int64) FrameOutput {
	out := FrameOutput{Frame: frame, TimeUS: timeUS, Lights: make([]LightOutput, 0, len(e.lights))}
	for _, track := range e.lights {
		out.Lights = append(out.Lights, e.lightOutput(track, timeUS))
	}
	return out
}

func (e *Evaluator) lightOutput(track *lightTrack, timeUS int64) LightOutput {
	raw := RGB{
		R: e.fieldValue(track, fieldR, timeUS),
		G: e.fieldValue(track, fieldG, timeUS),
		B: e.fieldValue(track, fieldB, timeUS),
	}
	intensity := e.fieldValue(track, fieldIntensity, timeUS)
	pan := e.fieldValue(track, fieldPan, timeUS)
	tilt := e.fieldValue(track, fieldTilt, timeUS)
	focus := e.fieldValue(track, fieldFocus, timeUS)
	colorTemp := e.fieldValue(track, fieldColorTemp, timeUS)
	strobeHz := e.fieldValue(track, fieldStrobeHz, timeUS)

	strobeOpen := true
	strobeGate := 1.0
	if strobeHz > 0 {
		phase := math.Mod(float64(timeUS)*strobeHz/1_000_000, 1)
		strobeOpen = phase < 0.5
		if !strobeOpen {
			strobeGate = 0
		}
	}
	effectiveIntensity := clamp(intensity*strobeGate, 0, 1)
	tempR, tempG, tempB := colorTemperatureTint(colorTemp)
	rendered := RGB{
		R: clamp(raw.R*effectiveIntensity*tempR, 0, 255),
		G: clamp(raw.G*effectiveIntensity*tempG, 0, 255),
		B: clamp(raw.B*effectiveIntensity*tempB, 0, 255),
	}
	layout := track.light.Layout
	panRadians := pan * math.Pi / 180
	tiltRadians := tilt * math.Pi / 180
	beam := 10.0
	return LightOutput{
		LightID:    track.light.ID,
		RGB:        RGB{round6(rendered.R), round6(rendered.G), round6(rendered.B)},
		RawRGB:     RGB{round6(raw.R), round6(raw.G), round6(raw.B)},
		Intensity:  round6(effectiveIntensity),
		Pan:        round6(pan),
		Tilt:       round6(tilt),
		Focus:      round6(focus),
		ColorTemp:  round6(colorTemp),
		StrobeHz:   round6(strobeHz),
		StrobeOpen: strobeOpen,
		X:          round6(layout.X + math.Sin(panRadians)*beam),
		Y:          round6(layout.Y + math.Cos(panRadians)*beam),
		Z:          round6(layout.Z + math.Tan(tiltRadians)*beam),
	}
}

func colorTemperatureTint(kelvin float64) (float64, float64, float64) {
	t := clamp((kelvin-2000)/8000, 0, 1)
	return 1 - 0.25*t, 0.72 + 0.18*t, 0.45 + 0.55*t
}

func (e *Evaluator) ContentHash() string  { return e.contentHash }
func (e *Evaluator) ParamVersion() string { return e.paramVer }
func (e *Evaluator) Decisions() []Arbitration {
	return append([]Arbitration(nil), e.decisions...)
}

func (e *Evaluator) CueOffsetBefore(endUS int64) int {
	index := sort.Search(len(e.project.Cues), func(i int) bool {
		return e.project.Cues[i].TimeMS*1000 >= endUS
	})
	return index
}

func (e *Evaluator) CueOffsetStart(startUS int64) int {
	index := sort.Search(len(e.project.Cues), func(i int) bool {
		return e.project.Cues[i].TimeMS*1000 >= startUS
	})
	return index
}

func (e *Evaluator) DecisionsInRange(startUS, endUS int64) []Arbitration {
	start := sort.Search(len(e.decisions), func(i int) bool {
		return e.decisions[i].TimeUS >= startUS
	})
	end := sort.Search(len(e.decisions), func(i int) bool {
		return e.decisions[i].TimeUS >= endUS
	})
	return append([]Arbitration(nil), e.decisions[start:end]...)
}

func (e *Evaluator) GroupSnapshotsBefore(endUS int64) []GroupSnapshot {
	groups := make(map[string]*Group, len(e.project.Groups))
	for i := range e.project.Groups {
		groups[e.project.Groups[i].ID] = &e.project.Groups[i]
	}
	order := GroupPrecedenceOrder(groups)
	out := make([]GroupSnapshot, 0, len(order))
	for _, groupID := range order {
		group := groups[groupID]
		snapshot := GroupSnapshot{
			GroupID:  group.ID,
			Members:  append([]string(nil), e.descendants[group.ID]...),
			Priority: group.Priority,
		}
		var winner *Cue
		for i := range e.project.Cues {
			cue := &e.project.Cues[i]
			if cue.GroupID != group.ID || cue.TimeMS*1000 >= endUS {
				continue
			}
			if winner == nil || cue.TimeMS > winner.TimeMS ||
				(cue.TimeMS == winner.TimeMS && groupCueSnapshotWins(cue, winner)) {
				winner = cue
			}
		}
		if winner != nil {
			snapshot.LastCue = winner.ID
			snapshot.LastTime = winner.TimeMS
			snapshot.Values = cloneJSON(winner.Values)
		}
		out = append(out, snapshot)
	}
	return out
}

func groupCueSnapshotWins(candidate, current *Cue) bool {
	if candidate.Priority != current.Priority {
		return candidate.Priority > current.Priority
	}
	return candidate.ID < current.ID
}

func buildArbitration(p *Project, descendants map[string][]string, lights map[string]*Light, groups map[string]*Group) []Arbitration {
	type candidateKey struct {
		lightID string
		field   fieldID
		timeUS  int64
	}
	type rankedCandidate struct {
		rank      sourceRank
		candidate Candidate
	}
	byKey := make(map[candidateKey][]rankedCandidate)
	for cueIndex := range p.Cues {
		cue := &p.Cues[cueIndex]
		updates := cueUpdates(cue)
		targets := []string{cue.LightID}
		scope := 1
		groupPriority := 0
		if cue.GroupID != "" {
			targets = descendants[cue.GroupID]
			scope = 0
			groupPriority = groups[cue.GroupID].Priority
		}
		for _, lightID := range targets {
			for _, update := range updates {
				key := candidateKey{lightID: lightID, field: update.field, timeUS: cue.TimeMS * 1000}
				byKey[key] = append(byKey[key], rankedCandidate{
					rank: sourceRank{
						scope:         scope,
						groupPriority: groupPriority,
						cuePriority:   cue.Priority,
						groupID:       cue.GroupID,
						sourceID:      chooseNonEmpty(cue.LightID, cue.GroupID),
						cueID:         cue.ID,
						cueIndex:      cueIndex,
					},
					candidate: Candidate{
						SourceType: map[int]string{1: "single", 0: "group"}[scope],
						SourceID:   chooseNonEmpty(cue.LightID, cue.GroupID),
						CueID:      cue.ID,
						Priority:   cue.Priority,
						Value:      update.value,
					},
				})
			}
		}
	}
	keys := make([]candidateKey, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].timeUS != keys[j].timeUS {
			return keys[i].timeUS < keys[j].timeUS
		}
		if keys[i].lightID != keys[j].lightID {
			return keys[i].lightID < keys[j].lightID
		}
		return keys[i].field < keys[j].field
	})
	var decisions []Arbitration
	for _, key := range keys {
		candidates := byKey[key]
		sourceSet := map[string]struct{}{}
		for _, item := range candidates {
			sourceSet[fmt.Sprintf("%d:%s", item.rank.scope, item.rank.sourceID)] = struct{}{}
		}
		if len(sourceSet) < 2 {
			continue
		}
		sort.Slice(candidates, func(i, j int) bool {
			return applicationOrder(candidates[i].rank, candidates[j].rank)
		})
		winner := candidates[len(candidates)-1]
		rule := "cue_priority_then_cue_id"
		hasSingle := false
		hasGroup := false
		for _, item := range candidates {
			if item.rank.scope == 1 {
				hasSingle = true
			} else {
				hasGroup = true
			}
		}
		switch {
		case hasSingle && hasGroup:
			rule = "single_overrides_group"
		case hasGroup:
			rule = "group_priority_then_group_id"
		}
		out := Arbitration{
			TimeUS:     key.timeUS,
			LightID:    key.lightID,
			Field:      key.field.String(),
			Winner:     "cue:" + winner.candidate.CueID,
			Rule:       rule,
			Candidates: make([]Candidate, 0, len(candidates)),
		}
		for _, item := range candidates {
			out.Candidates = append(out.Candidates, item.candidate)
		}
		decisions = append(decisions, out)
	}
	return decisions
}

func detectCueConflicts(p *Project, lights map[string]*Light, groups map[string]*Group, descendants map[string][]string) ([]error, error) {
	type conflictKey struct {
		lightID  string
		field    fieldID
		timeUS   int64
		scope    int
		sourceID string
		groupPri int
		cuePri   int
	}
	type conflictCandidate struct {
		cueID string
		value float64
	}
	byKey := make(map[conflictKey][]conflictCandidate)
	for cueIndex := range p.Cues {
		cue := &p.Cues[cueIndex]
		targets := []string{cue.LightID}
		scope := 1
		groupPriority := 0
		sourceID := cue.LightID
		if cue.GroupID != "" {
			targets = descendants[cue.GroupID]
			scope = 0
			sourceID = cue.GroupID
			groupPriority = groups[cue.GroupID].Priority
		}
		for _, lightID := range targets {
			for _, update := range cueUpdates(cue) {
				key := conflictKey{
					lightID:  lightID,
					field:    update.field,
					timeUS:   cue.TimeMS * 1000,
					scope:    scope,
					sourceID: sourceID,
					groupPri: groupPriority,
					cuePri:   cue.Priority,
				}
				byKey[key] = append(byKey[key], conflictCandidate{cueID: cue.ID, value: update.value})
			}
		}
	}
	keys := make([]conflictKey, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].timeUS != keys[j].timeUS {
			return keys[i].timeUS < keys[j].timeUS
		}
		if keys[i].lightID != keys[j].lightID {
			return keys[i].lightID < keys[j].lightID
		}
		if keys[i].field != keys[j].field {
			return keys[i].field < keys[j].field
		}
		return keys[i].sourceID < keys[j].sourceID
	})
	var errs []error
	for _, key := range keys {
		candidates := byKey[key]
		if len(candidates) < 2 {
			continue
		}
		first := candidates[0]
		for _, candidate := range candidates[1:] {
			if math.Abs(candidate.value-first.value) > 1e-9 {
				err := newError(CodeConflict, "same-scope cues cannot be deterministically ordered", "cue:"+first.cueID, key.field.String())
				err.Details = append(err.Details,
					ErrorDetail{Object: "light:" + key.lightID, Field: key.field.String(), Left: fmt.Sprintf("cue:%s=%g", first.cueID, first.value), Right: fmt.Sprintf("cue:%s=%g", candidate.cueID, candidate.value), TimeMS: key.timeUS / 1000},
				)
				errs = append(errs, err)
				break
			}
		}
	}
	return errs, nil
}

func (e *Evaluator) LightIDs() []string {
	out := make([]string, 0, len(e.lights))
	for _, track := range e.lights {
		out = append(out, track.light.ID)
	}
	return out
}

func (e *Evaluator) GroupDescendants(groupID string) []string {
	return append([]string(nil), e.descendants[groupID]...)
}

func (e *Evaluator) Summary() string {
	return fmt.Sprintf("%s lights=%d groups=%d cues=%d decisions=%d", strings.TrimSpace(e.project.ID), len(e.project.Lights), len(e.project.Groups), len(e.project.Cues), len(e.decisions))
}
