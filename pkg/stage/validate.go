package stage

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

type ValidateOptions struct {
	CheckTruth    bool
	CheckConflict bool
}

func DefaultValidateOptions() ValidateOptions {
	return ValidateOptions{CheckTruth: true, CheckConflict: true}
}

func ValidateProject(p *Project, opts ValidateOptions) error {
	if p == nil {
		return newError(CodeSchema, "project is missing", "project", "")
	}
	if p.SchemaVersion != SchemaVersion {
		return withDetail(
			newError(CodeSchema, "unsupported schema version", "project", "schema_version"),
			ErrorDetail{Expected: fmt.Sprint(SchemaVersion), Actual: fmt.Sprint(p.SchemaVersion)},
		)
	}
	if strings.TrimSpace(p.ID) == "" {
		return newError(CodeSchema, "project id is required", "project", "id")
	}
	if p.FPS <= 0 || p.FPS > 240 {
		return withDetail(
			newError(CodeSchema, "fps must be between 1 and 240", "project", "fps"),
			ErrorDetail{Actual: fmt.Sprint(p.FPS)},
		)
	}
	if p.DurationMS <= 0 {
		return newError(CodeSchema, "duration must be positive", "project", "duration_ms")
	}
	if p.WindowMS <= 0 {
		return newError(CodeSchema, "window size must be positive", "project", "window_ms")
	}
	if len(p.Lights) == 0 {
		return newError(CodeSchema, "at least one light is required", "project", "lights")
	}
	if len(p.Cues) == 0 {
		return newError(CodeSchema, "at least one cue is required", "project", "cues")
	}

	lights := make(map[string]*Light, len(p.Lights))
	for i := range p.Lights {
		light := &p.Lights[i]
		object := fmt.Sprintf("project.lights[%d]", i)
		if strings.TrimSpace(light.ID) == "" {
			return newError(CodeSchema, "light id is required", object, "id")
		}
		if _, exists := lights[light.ID]; exists {
			return newError(CodeSchema, "duplicate light id", object, "id")
		}
		switch light.Kind {
		case LightPar, LightMoving, LightWash:
		default:
			return withDetail(newError(CodeSchema, "unsupported light kind", object, "kind"), ErrorDetail{Actual: string(light.Kind)})
		}
		if err := ValidateRanges(light.Ranges); err != nil {
			if de, ok := AsDomainError(err); ok {
				de.Object = object + ".ranges"
			}
			return err
		}
		if err := validateValues(object+".base", light.Base, light.Ranges); err != nil {
			return err
		}
		lights[light.ID] = light
	}

	groups := make(map[string]*Group, len(p.Groups))
	for i := range p.Groups {
		group := &p.Groups[i]
		object := fmt.Sprintf("project.groups[%d]", i)
		if strings.TrimSpace(group.ID) == "" {
			return newError(CodeSchema, "group id is required", object, "id")
		}
		if _, exists := groups[group.ID]; exists {
			return newError(CodeSchema, "duplicate group id", object, "id")
		}
		if group.Priority < 0 || group.Priority > 9999 {
			return withDetail(newError(CodeParameter, "group priority out of range", object, "priority"), ErrorDetail{Expected: "0..9999", Actual: fmt.Sprint(group.Priority)})
		}
		seenMembers := map[string]struct{}{}
		for _, member := range group.Members {
			if _, ok := lights[member]; !ok {
				return newError(CodeReference, "group member does not reference a light", object, "members")
			}
			if _, duplicate := seenMembers[member]; duplicate {
				return newError(CodeSchema, "duplicate group member", object, "members")
			}
			seenMembers[member] = struct{}{}
		}
		groups[group.ID] = group
	}
	for i := range p.Groups {
		group := &p.Groups[i]
		if group.ParentID == "" {
			continue
		}
		if _, ok := groups[group.ParentID]; !ok {
			return newError(CodeReference, "group parent does not exist", fmt.Sprintf("project.groups[%d]", i), "parent_id")
		}
	}
	if err := validateGroupCycles(p.Groups); err != nil {
		return err
	}
	descendants, err := groupMembership(p, groups)
	if err != nil {
		return err
	}

	expandable := make(map[string][]string, len(groups))
	for groupID := range groups {
		expanded := descendants[groupID]
		if len(expanded) == 0 {
			return newError(CodeReference, "group has no effective light members", "group:"+groupID, "members")
		}
		expandable[groupID] = expanded
	}

	cuesID := make(map[string]struct{}, len(p.Cues))
	var previous int64 = -1
	for i := range p.Cues {
		cue := &p.Cues[i]
		object := fmt.Sprintf("project.cues[%d]", i)
		if strings.TrimSpace(cue.ID) == "" {
			return newError(CodeSchema, "cue id is required", object, "id")
		}
		if _, exists := cuesID[cue.ID]; exists {
			return newError(CodeSchema, "duplicate cue id", object, "id")
		}
		cuesID[cue.ID] = struct{}{}
		if cue.TimeMS < 0 || cue.TimeMS > p.DurationMS {
			return withDetail(newError(CodeParameter, "cue time is outside project duration", object, "time_ms"), ErrorDetail{Expected: fmt.Sprintf("0..%d", p.DurationMS), Actual: fmt.Sprint(cue.TimeMS)})
		}
		if cue.TimeMS < previous {
			return withDetail(newError(CodeSchema, "cue timestamps must be monotonically non-decreasing", object, "time_ms"), ErrorDetail{Left: fmt.Sprint(previous), Right: fmt.Sprint(cue.TimeMS)})
		}
		previous = cue.TimeMS
		if (cue.LightID == "") == (cue.GroupID == "") {
			return newError(CodeSchema, "cue must reference exactly one light or group", object, "light_id/group_id")
		}
		if cue.LightID != "" {
			if _, ok := lights[cue.LightID]; !ok {
				return newError(CodeReference, "cue light does not exist", object, "light_id")
			}
		}
		if cue.GroupID != "" {
			if _, ok := groups[cue.GroupID]; !ok {
				return newError(CodeReference, "cue group does not exist", object, "group_id")
			}
		}
		if cue.TransitionMS < 0 || cue.TransitionMS > p.DurationMS {
			return withDetail(newError(CodeParameter, "transition_ms is out of range", object, "transition_ms"), ErrorDetail{Expected: fmt.Sprintf("0..%d", p.DurationMS), Actual: fmt.Sprint(cue.TransitionMS)})
		}
		switch cue.Easing {
		case EaseLinear, EaseStep, EaseInOut:
		default:
			return withDetail(newError(CodeSchema, "unsupported easing", object, "easing"), ErrorDetail{Actual: string(cue.Easing)})
		}
		if !finite(cue.CurvePower) || cue.CurvePower < 0.1 || cue.CurvePower > 4 {
			return withDetail(newError(CodeParameter, "curve_power is out of range", object, "curve_power"), ErrorDetail{Expected: "0.1..4", Actual: fmt.Sprint(cue.CurvePower)})
		}
		if cue.Priority < 0 || cue.Priority > 9999 {
			return withDetail(newError(CodeParameter, "cue priority is out of range", object, "priority"), ErrorDetail{Expected: "0..9999", Actual: fmt.Sprint(cue.Priority)})
		}
		if cue.Values.IsEmpty() {
			return newError(CodeSchema, "cue has no target values", object, "values")
		}
		if err := validateCueValues(cue, object, lights, expandable); err != nil {
			return err
		}
	}

	scenes := make(map[string]struct{}, len(p.Scenes))
	for i := range p.Scenes {
		scene := &p.Scenes[i]
		object := fmt.Sprintf("project.scenes[%d]", i)
		if strings.TrimSpace(scene.ID) == "" {
			return newError(CodeSchema, "scene id is required", object, "id")
		}
		if _, exists := scenes[scene.ID]; exists {
			return newError(CodeSchema, "duplicate scene id", object, "id")
		}
		scenes[scene.ID] = struct{}{}
		for _, cueID := range scene.CueIDs {
			if _, ok := cuesID[cueID]; !ok {
				return newError(CodeReference, "scene cue does not exist", object, "cue_ids")
			}
		}
	}

	if opts.CheckTruth {
		if err := validateTruth(p, lights, groups); err != nil {
			return err
		}
	}
	if opts.CheckConflict {
		conflicts, err := detectCueConflicts(p, lights, groups, expandable)
		if err != nil {
			return err
		}
		if len(conflicts) > 0 {
			return conflicts[0]
		}
	}
	return nil
}

func validateValues(object string, values LightValues, ranges ParamRanges) error {
	checks := []struct {
		field string
		value float64
	}{
		{"rgb.r", values.RGB.R},
		{"rgb.g", values.RGB.G},
		{"rgb.b", values.RGB.B},
		{"intensity", values.Intensity},
		{"pan", values.Pan},
		{"tilt", values.Tilt},
		{"focus", values.Focus},
		{"color_temp", values.ColorTemp},
		{"strobe_hz", values.StrobeHz},
	}
	for _, check := range checks {
		paramRange, _ := ranges.fieldRange(check.field)
		if !finite(check.value) || !paramRange.Contains(check.value) {
			return withDetail(newError(CodeParameter, "value outside declared range", object, check.field), ErrorDetail{Expected: fmt.Sprintf("%g..%g", paramRange.Min, paramRange.Max), Actual: fmt.Sprint(check.value)})
		}
	}
	return nil
}

func validateCueValues(cue *Cue, object string, lights map[string]*Light, expandable map[string][]string) error {
	check := func(field string, value *float64) error {
		if value == nil {
			return nil
		}
		if !finite(*value) {
			return newError(CodeParameter, "non-finite cue value", object, field)
		}
		var targets []string
		if cue.LightID != "" {
			targets = []string{cue.LightID}
		} else {
			targets = expandable[cue.GroupID]
		}
		for _, lightID := range targets {
			light := lights[lightID]
			paramRange, _ := light.Ranges.fieldRange(field)
			if !paramRange.Contains(*value) {
				return withDetail(newError(CodeParameter, "cue value outside declared light range", "cue:"+cue.ID, field), ErrorDetail{Object: "light:" + lightID, Expected: fmt.Sprintf("%g..%g", paramRange.Min, paramRange.Max), Actual: fmt.Sprint(*value)})
			}
		}
		return nil
	}
	if cue.Values.RGB != nil {
		if err := check("rgb.r", &cue.Values.RGB.R); err != nil {
			return err
		}
		if err := check("rgb.g", &cue.Values.RGB.G); err != nil {
			return err
		}
		if err := check("rgb.b", &cue.Values.RGB.B); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name  string
		value *float64
	}{
		{"intensity", cue.Values.Intensity},
		{"pan", cue.Values.Pan},
		{"tilt", cue.Values.Tilt},
		{"focus", cue.Values.Focus},
		{"color_temp", cue.Values.ColorTemp},
		{"strobe_hz", cue.Values.StrobeHz},
	} {
		if err := check(field.name, field.value); err != nil {
			return err
		}
	}
	return nil
}

func validateGroupCycles(groups []Group) error {
	byID := make(map[string]*Group, len(groups))
	for i := range groups {
		byID[groups[i].ID] = &groups[i]
	}
	state := map[string]int{}
	var stack []string
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			start := 0
			for i, item := range stack {
				if item == id {
					start = i
					break
				}
			}
			cycle := append(append([]string(nil), stack[start:]...), id)
			err := newError(CodeCycle, "group parent relationship contains a cycle", "group:"+id, "parent_id")
			return withDetail(err, ErrorDetail{Actual: strings.Join(cycle, " -> ")})
		case 2:
			return nil
		}
		state[id] = 1
		stack = append(stack, id)
		if parent := byID[id].ParentID; parent != "" {
			if err := visit(parent); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return nil
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func groupMembership(p *Project, groups map[string]*Group) (map[string][]string, error) {
	children := make(map[string][]string)
	roots := make([]string, 0)
	for id, group := range groups {
		if group.ParentID == "" {
			roots = append(roots, id)
		} else {
			children[group.ParentID] = append(children[group.ParentID], id)
		}
	}
	sort.Strings(roots)
	for parent := range children {
		sort.Strings(children[parent])
	}
	var walk func(string) ([]string, error)
	walk = func(id string) ([]string, error) {
		set := map[string]struct{}{}
		for _, member := range groups[id].Members {
			set[member] = struct{}{}
		}
		for _, child := range children[id] {
			members, err := walk(child)
			if err != nil {
				return nil, err
			}
			for _, member := range members {
				set[member] = struct{}{}
			}
		}
		out := make([]string, 0, len(set))
		for member := range set {
			out = append(out, member)
		}
		sort.Strings(out)
		return out, nil
	}
	result := make(map[string][]string, len(groups))
	for id := range groups {
		members, err := walk(id)
		if err != nil {
			return nil, err
		}
		result[id] = members
	}
	return result, nil
}

func validateTruth(p *Project, lights map[string]*Light, groups map[string]*Group) error {
	if p.Truth.TotalDurationMS != p.DurationMS {
		return withDetail(newError(CodeSchema, "truth total duration disagrees with project", "project.truth", "total_duration_ms"), ErrorDetail{Expected: fmt.Sprint(p.DurationMS), Actual: fmt.Sprint(p.Truth.TotalDurationMS)})
	}
	if len(p.Truth.Samples) == 0 {
		return newError(CodeSchema, "truth samples are required", "project.truth", "samples")
	}
	var previous int64 = -1
	for i := range p.Truth.Samples {
		sample := &p.Truth.Samples[i]
		object := fmt.Sprintf("project.truth.samples[%d]", i)
		if sample.TimeMS < 0 || sample.TimeMS > p.DurationMS {
			return newError(CodeParameter, "truth sample time outside project duration", object, "time_ms")
		}
		if sample.TimeMS < previous {
			return newError(CodeSchema, "truth sample times must be monotonic", object, "time_ms")
		}
		previous = sample.TimeMS
		if sample.Frame < 0 || sample.Frame >= FrameCount(p.DurationMS, p.FPS) {
			return newError(CodeParameter, "truth sample frame outside range", object, "frame")
		}
		expectedFrame := int(math.Round(float64(sample.TimeMS) * float64(p.FPS) / 1000))
		if expectedFrame >= FrameCount(p.DurationMS, p.FPS) {
			expectedFrame = FrameCount(p.DurationMS, p.FPS) - 1
		}
		if expectedFrame != sample.Frame {
			return withDetail(newError(CodeSchema, "truth sample frame does not match time", object, "frame"), ErrorDetail{Expected: fmt.Sprint(expectedFrame), Actual: fmt.Sprint(sample.Frame)})
		}
		seen := map[string]struct{}{}
		for _, expected := range sample.Lights {
			if _, ok := lights[expected.LightID]; !ok {
				return newError(CodeReference, "truth sample references unknown light", object, "lights")
			}
			if _, duplicate := seen[expected.LightID]; duplicate {
				return newError(CodeSchema, "truth sample contains duplicate light", object, "lights")
			}
			seen[expected.LightID] = struct{}{}
		}
		if len(seen) != len(lights) {
			return withDetail(newError(CodeSchema, "truth sample must contain every light exactly once", object, "lights"), ErrorDetail{Expected: fmt.Sprint(len(lights)), Actual: fmt.Sprint(len(seen))})
		}
	}
	tol := p.Truth.Tolerance
	if tol.Color < 0 || tol.Position < 0 || tol.Intensity < 0 || tol.Focus < 0 || tol.ColorTemp < 0 || tol.StrobeHz < 0 {
		return newError(CodeSchema, "truth tolerances must be non-negative", "project.truth", "tolerance")
	}
	order := p.Truth.GroupOrder
	if len(order) != len(groups) {
		return withDetail(newError(CodeSchema, "truth group order must contain every group exactly once", "project.truth", "group_order"), ErrorDetail{Expected: fmt.Sprint(len(groups)), Actual: fmt.Sprint(len(order))})
	}
	expectedOrder := GroupPrecedenceOrder(groups)
	for i := range order {
		if order[i] != expectedOrder[i] {
			return withDetail(newError(CodeSchema, "truth group order disagrees with priority rules", "project.truth", "group_order"), ErrorDetail{Expected: strings.Join(expectedOrder, ","), Actual: strings.Join(order, ",")})
		}
	}
	return nil
}

func GroupPrecedenceOrder(groups map[string]*Group) []string {
	out := make([]string, 0, len(groups))
	for id := range groups {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := groups[out[i]], groups[out[j]]
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		return left.ID < right.ID
	})
	return out
}
