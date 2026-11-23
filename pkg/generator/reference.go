package generator

import (
	"fmt"
	"math"
	"sort"

	"stageworkbench/pkg/stage"
)

const (
	refR = iota
	refG
	refB
	refIntensity
	refPan
	refTilt
	refFocus
	refColorTemp
	refStrobeHz
	refFieldCount
)

type reference struct {
	project *stage.Project
	lights  []string
	tracks  map[string]*refTrack
}

type refTrack struct {
	light  *stage.Light
	fields [refFieldCount][]refEvent
}

type refEvent struct {
	atUS         int64
	start        float64
	target       float64
	transitionUS int64
	easing       stage.Easing
	curvePower   float64
	scope        int
	groupPri     int
	cuePri       int
	groupID      string
	cueID        string
	cueIndex     int
}

type refUpdate struct {
	field  int
	target float64
}

func newReference(p *stage.Project) (*reference, error) {
	lightsByID := make(map[string]*stage.Light, len(p.Lights))
	for i := range p.Lights {
		lightsByID[p.Lights[i].ID] = &p.Lights[i]
	}
	groupsByID := make(map[string]*stage.Group, len(p.Groups))
	for i := range p.Groups {
		groupsByID[p.Groups[i].ID] = &p.Groups[i]
	}
	members := make(map[string][]string, len(p.Groups))
	var resolve func(string, map[string]bool) ([]string, error)
	resolve = func(groupID string, visiting map[string]bool) ([]string, error) {
		if visiting[groupID] {
			return nil, fmt.Errorf("cycle in reference group resolver")
		}
		visiting[groupID] = true
		set := map[string]struct{}{}
		group := groupsByID[groupID]
		for _, member := range group.Members {
			set[member] = struct{}{}
		}
		for _, candidate := range p.Groups {
			if candidate.ParentID == groupID {
				children, err := resolve(candidate.ID, visiting)
				if err != nil {
					return nil, err
				}
				for _, member := range children {
					set[member] = struct{}{}
				}
			}
		}
		delete(visiting, groupID)
		out := make([]string, 0, len(set))
		for member := range set {
			out = append(out, member)
		}
		sort.Strings(out)
		members[groupID] = out
		return out, nil
	}
	for i := range p.Groups {
		if _, err := resolve(p.Groups[i].ID, map[string]bool{}); err != nil {
			return nil, err
		}
	}
	ref := &reference{project: p, tracks: make(map[string]*refTrack)}
	ref.lights = make([]string, 0, len(p.Lights))
	for i := range p.Lights {
		light := &p.Lights[i]
		ref.lights = append(ref.lights, light.ID)
		ref.tracks[light.ID] = &refTrack{light: light}
	}
	sort.Strings(ref.lights)

	for cueIndex := range p.Cues {
		cue := &p.Cues[cueIndex]
		updates := refUpdates(cue.Values)
		targets := []string{cue.LightID}
		scope := 1
		groupPri := 0
		if cue.GroupID != "" {
			targets = members[cue.GroupID]
			scope = 0
			groupPri = groupsByID[cue.GroupID].Priority
		}
		for _, lightID := range targets {
			track := ref.tracks[lightID]
			for _, update := range updates {
				track.fields[update.field] = append(track.fields[update.field], refEvent{
					atUS:         cue.TimeMS * 1000,
					target:       update.target,
					transitionUS: cue.TransitionMS * 1000,
					easing:       cue.Easing,
					curvePower:   cue.CurvePower,
					scope:        scope,
					groupPri:     groupPri,
					cuePri:       cue.Priority,
					groupID:      cue.GroupID,
					cueID:        cue.ID,
					cueIndex:     cueIndex,
				})
			}
		}
	}
	for _, track := range ref.tracks {
		for field := 0; field < refFieldCount; field++ {
			events := track.fields[field]
			if len(events) == 0 {
				continue
			}
			sort.Slice(events, func(i, j int) bool {
				if events[i].atUS != events[j].atUS {
					return events[i].atUS < events[j].atUS
				}
				return refApplicationLess(events[i], events[j])
			})
			previous := refEvent{
				atUS:       -1,
				start:      refBase(track.light.Base, field),
				target:     refBase(track.light.Base, field),
				easing:     stage.EaseStep,
				curvePower: 1,
			}
			for i := 0; i < len(events); {
				j := i + 1
				for j < len(events) && events[j].atUS == events[i].atUS {
					j++
				}
				batchStart := refEvalEvent(previous, events[i].atUS)
				for k := i; k < j; k++ {
					events[k].start = batchStart
				}
				previous = events[j-1]
				i = j
			}
			track.fields[field] = events
		}
	}
	return ref, nil
}

func refUpdates(values stage.CueValues) []refUpdate {
	var out []refUpdate
	if values.RGB != nil {
		out = append(out,
			refUpdate{refR, values.RGB.R},
			refUpdate{refG, values.RGB.G},
			refUpdate{refB, values.RGB.B},
		)
	}
	add := func(field int, value *float64) {
		if value != nil {
			out = append(out, refUpdate{field, *value})
		}
	}
	add(refIntensity, values.Intensity)
	add(refPan, values.Pan)
	add(refTilt, values.Tilt)
	add(refFocus, values.Focus)
	add(refColorTemp, values.ColorTemp)
	add(refStrobeHz, values.StrobeHz)
	return out
}

func refApplicationLess(left, right refEvent) bool {
	if left.scope != right.scope {
		return left.scope < right.scope
	}
	if left.scope == 0 {
		if left.groupPri != right.groupPri {
			return left.groupPri < right.groupPri
		}
		if left.cuePri != right.cuePri {
			return left.cuePri < right.cuePri
		}
		if left.groupID != right.groupID {
			return left.groupID > right.groupID
		}
	} else if left.cuePri != right.cuePri {
		return left.cuePri < right.cuePri
	}
	if left.cueID != right.cueID {
		return left.cueID > right.cueID
	}
	return left.cueIndex > right.cueIndex
}

func refEvalEvent(event refEvent, timeUS int64) float64 {
	if event.atUS < 0 || timeUS < event.atUS {
		return event.start
	}
	if event.easing == stage.EaseStep || event.transitionUS == 0 {
		return event.target
	}
	elapsed := timeUS - event.atUS
	if elapsed >= event.transitionUS {
		return event.target
	}
	u := float64(elapsed) / float64(event.transitionUS)
	if event.easing == stage.EaseInOut {
		u = u * u * (3 - 2*u)
	}
	if event.curvePower > 0 && event.curvePower != 1 {
		u = math.Pow(u, event.curvePower)
	}
	return event.start + (event.target-event.start)*u
}

func (r *reference) sample(frame int) []stage.TruthLight {
	timeUS := stage.FrameTimeUS(frame, r.project.FPS)
	out := make([]stage.TruthLight, 0, len(r.lights))
	for _, lightID := range r.lights {
		track := r.tracks[lightID]
		raw := stage.RGB{
			R: refValue(track, refR, timeUS),
			G: refValue(track, refG, timeUS),
			B: refValue(track, refB, timeUS),
		}
		intensity := refValue(track, refIntensity, timeUS)
		pan := refValue(track, refPan, timeUS)
		tilt := refValue(track, refTilt, timeUS)
		focus := refValue(track, refFocus, timeUS)
		colorTemp := refValue(track, refColorTemp, timeUS)
		strobeHz := refValue(track, refStrobeHz, timeUS)
		gate := 1.0
		if strobeHz > 0 {
			phase := math.Mod(float64(timeUS)*strobeHz/1_000_000, 1)
			if phase >= 0.5 {
				gate = 0
			}
		}
		effective := refClamp(intensity*gate, 0, 1)
		t := refClamp((colorTemp-2000)/8000, 0, 1)
		tempR, tempG, tempB := 1-0.25*t, 0.72+0.18*t, 0.45+0.55*t
		out = append(out, stage.TruthLight{
			LightID:   lightID,
			RGB:       stage.RGB{R: refRound(refClamp(raw.R*effective*tempR, 0, 255)), G: refRound(refClamp(raw.G*effective*tempG, 0, 255)), B: refRound(refClamp(raw.B*effective*tempB, 0, 255))},
			Intensity: refRound(effective),
			Pan:       refRound(pan),
			Tilt:      refRound(tilt),
			Focus:     refRound(focus),
			ColorTemp: refRound(colorTemp),
			StrobeHz:  refRound(strobeHz),
		})
	}
	return out
}

func refValue(track *refTrack, field int, timeUS int64) float64 {
	events := track.fields[field]
	if len(events) == 0 {
		return refBase(track.light.Base, field)
	}
	index := sort.Search(len(events), func(i int) bool {
		return events[i].atUS > timeUS
	}) - 1
	if index < 0 {
		return refBase(track.light.Base, field)
	}
	return refEvalEvent(events[index], timeUS)
}

func refBase(base stage.LightValues, field int) float64 {
	switch field {
	case refR:
		return base.RGB.R
	case refG:
		return base.RGB.G
	case refB:
		return base.RGB.B
	case refIntensity:
		return base.Intensity
	case refPan:
		return base.Pan
	case refTilt:
		return base.Tilt
	case refFocus:
		return base.Focus
	case refColorTemp:
		return base.ColorTemp
	case refStrobeHz:
		return base.StrobeHz
	default:
		return 0
	}
}

func refRound(v float64) float64 {
	if math.Abs(v) < 0.0000005 {
		return 0
	}
	return math.Round(v*1e6) / 1e6
}

func refClamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
