package lightshow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"strconv"
	"strings"
)

// Load parses any supported project version into the common internal model.
func Load(data []byte) (*Show, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, engineError(CodeDecode, "$", "invalid project JSON: %v", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, err
	}
	m, ok := root.(map[string]any)
	if !ok {
		return nil, engineError(CodeDecode, "$", "project root must be an object")
	}

	if schema, _ := m["schema"].(string); schema != "" {
		if schema != "stage-lights/v1" {
			return nil, engineError(CodeVersion, "schema", "unknown project schema %q", schema)
		}
		return loadV1(m)
	}

	var version int64
	var found bool
	for _, key := range []string{"formatVersion", "format_version"} {
		if raw, ok := m[key]; ok {
			n, err := valueInt(raw, key)
			if err != nil {
				return nil, err
			}
			version = n
			found = true
			break
		}
	}
	if !found {
		return nil, engineError(CodeVersion, "formatVersion", "required field is missing")
	}
	switch version {
	case 2:
		return loadV2(m)
	case 3:
		return loadV3(m)
	default:
		return nil, engineError(CodeVersion, "formatVersion", "unknown project version %q", strconv.FormatInt(version, 10))
	}
}

func LoadFile(path string) (*Show, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, engineError(CodeDecode, "$", "read project: %v", err)
	}
	return Load(data)
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return engineError(CodeDecode, "$", "multiple JSON values")
	}
	return engineError(CodeDecode, "$", "invalid trailing JSON: %v", err)
}

func required(m map[string]any, key, path string) (any, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, engineError(CodeVersion, path, "required field is missing")
	}
	return v, nil
}

func object(v any, path string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, engineError(CodeVersion, path, "must be an object")
	}
	return m, nil
}

func array(v any, path string) ([]any, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, engineError(CodeVersion, path, "must be an array")
	}
	return a, nil
}

func valueString(v any, path string) (string, error) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", engineError(CodeVersion, path, "must be a non-empty string")
	}
	return s, nil
}

func valueBool(v any, path string) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, engineError(CodeVersion, path, "must be a boolean")
	}
	return b, nil
}

func valueInt(v any, path string) (int64, error) {
	switch n := v.(type) {
	case json.Number:
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err == nil {
			return i, nil
		}
		if r, ok := new(big.Rat).SetString(n.String()); ok && r.IsInt() {
			return r.Num().Int64(), nil
		}
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case float64:
		if n == float64(int64(n)) {
			return int64(n), nil
		}
	}
	return 0, engineError(CodeVersion, path, "must be an integer")
}

func intField(m map[string]any, key, path string) (int64, error) {
	v, err := required(m, key, path)
	if err != nil {
		return 0, err
	}
	return valueInt(v, path)
}

func stringField(m map[string]any, key, path string) (string, error) {
	v, err := required(m, key, path)
	if err != nil {
		return "", err
	}
	return valueString(v, path)
}

func boolField(m map[string]any, key, path string) (bool, error) {
	v, err := required(m, key, path)
	if err != nil {
		return false, err
	}
	return valueBool(v, path)
}

func intArray3(v any, path string) ([3]int, error) {
	var out [3]int
	a, err := array(v, path)
	if err != nil {
		return out, err
	}
	if len(a) != 3 {
		return out, engineError(CodeVersion, path, "must contain exactly 3 integers")
	}
	for i := range out {
		n, err := valueInt(a[i], fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return out, err
		}
		out[i] = int(n)
	}
	return out, nil
}

func normalizedColor(v any, path string, step *big.Rat) (RGB, error) {
	a, err := array(v, path)
	if err != nil {
		return RGB{}, err
	}
	if len(a) != 3 {
		return RGB{}, engineError(CodeVersion, path, "must contain exactly 3 channels")
	}
	return NormalizedToRGB16([3]any{a[0], a[1], a[2]}, step)
}

func rgb8Color(v any, path string) (RGB, error) {
	a, err := array(v, path)
	if err != nil {
		return RGB{}, err
	}
	if len(a) != 3 {
		return RGB{}, engineError(CodeVersion, path, "must contain exactly 3 channels")
	}
	var channels [3]uint8
	for i := range channels {
		n, err := valueInt(a[i], fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return RGB{}, err
		}
		if n < 0 || n > 255 {
			return RGB{}, engineError(CodeVersion, fmt.Sprintf("%s[%d]", path, i), "8-bit channel outside 0..255")
		}
		channels[i] = uint8(n)
	}
	return RGB8To16(channels[0], channels[1], channels[2]), nil
}

func parseRounding(v any, path string) (TimeRounding, error) {
	s, err := valueString(v, path)
	if err != nil {
		return "", err
	}
	mode := TimeRounding(s)
	if mode != RoundNearest && mode != RoundFloor && mode != RoundCeil {
		return "", engineError(CodeVersion, path, "unsupported rounding %q", s)
	}
	return mode, nil
}

func parseInterpolation(v any, path string, fallback Interpolation) (Interpolation, error) {
	if v == nil {
		if fallback == "" {
			return "", engineError(CodeVersion, path, "required field is missing")
		}
		return fallback, nil
	}
	switch x := v.(type) {
	case string:
		switch strings.ToLower(x) {
		case "hold", "step":
			return Hold, nil
		case "linear", "lerp":
			return Linear, nil
		default:
			return "", engineError(CodeVersion, path, "unsupported interpolation %q", x)
		}
	case json.Number, int64, int, float64:
		n, err := valueInt(x, path)
		if err != nil {
			return "", err
		}
		switch n {
		case 0:
			return Hold, nil
		case 1:
			return Linear, nil
		default:
			return "", engineError(CodeVersion, path, "unsupported interpolation code %d", n)
		}
	default:
		return "", engineError(CodeVersion, path, "must be a string or integer code")
	}
}

func parseFixtureRanges(m map[string]any, path string) (FixtureRanges, error) {
	var out FixtureRanges
	intensity, err := objectField(m, "intensity", path+".intensity")
	if err != nil {
		return out, err
	}
	out.Intensity, err = parseRange(intensity, path+".intensity")
	if err != nil {
		return out, err
	}
	posRaw, err := required(m, "position", path+".position")
	if err != nil {
		return out, err
	}
	pos, err := array(posRaw, path+".position")
	if err != nil {
		return out, err
	}
	if len(pos) != 3 {
		return out, engineError(CodeVersion, path+".position", "must contain 3 ranges")
	}
	for i := range out.Position {
		item, err := object(pos[i], fmt.Sprintf("%s.position[%d]", path, i))
		if err != nil {
			return out, err
		}
		out.Position[i], err = parseRange(item, fmt.Sprintf("%s.position[%d]", path, i))
		if err != nil {
			return out, err
		}
	}
	focus, err := objectField(m, "focus", path+".focus")
	if err != nil {
		return out, err
	}
	out.Focus, err = parseRange(focus, path+".focus")
	return out, err
}

func objectField(m map[string]any, key, path string) (map[string]any, error) {
	v, err := required(m, key, path)
	if err != nil {
		return nil, err
	}
	return object(v, path)
}

func parseRange(m map[string]any, path string) (IntRange, error) {
	lo, err := intField(m, "min", path+".min")
	if err != nil {
		return IntRange{}, err
	}
	hi, err := intField(m, "max", path+".max")
	if err != nil {
		return IntRange{}, err
	}
	if lo > hi {
		return IntRange{}, engineError(CodeVersion, path, "min exceeds max")
	}
	return IntRange{Min: int(lo), Max: int(hi)}, nil
}

func applyOptionalValues(values map[string]any, path string, normalized bool, step *big.Rat, colorKey, intensityKey string) (Values, error) {
	var out Values
	if raw, ok := values[colorKey]; ok {
		var c RGB
		var err error
		if normalized {
			c, err = normalizedColor(raw, path+"."+colorKey, step)
		} else {
			c, err = rgb8Color(raw, path+"."+colorKey)
		}
		if err != nil {
			return out, err
		}
		out.Color = &c
	}
	if raw, ok := values[intensityKey]; ok {
		n, err := valueInt(raw, path+"."+intensityKey)
		if err != nil {
			return out, err
		}
		x := int(n)
		out.Intensity = &x
	}
	if raw, ok := values["position"]; ok {
		p, err := intArray3(raw, path+".position")
		if err != nil {
			return out, err
		}
		out.Position = &p
	}
	if raw, ok := values["focus"]; ok {
		n, err := valueInt(raw, path+".focus")
		if err != nil {
			return out, err
		}
		x := int(n)
		out.Focus = &x
	}
	return out, nil
}

func loadV1(root map[string]any) (*Show, error) {
	showObj, err := objectField(root, "show", "show")
	if err != nil {
		return nil, err
	}
	stageObj, err := objectField(root, "stage", "stage")
	if err != nil {
		return nil, err
	}
	id, err := stringField(showObj, "id", "show.id")
	if err != nil {
		return nil, err
	}
	fps, err := intField(showObj, "fps", "show.fps")
	if err != nil {
		return nil, err
	}
	durationFrames, err := intField(showObj, "duration_frames", "show.duration_frames")
	if err != nil {
		return nil, err
	}
	windowFrames, err := intField(showObj, "window_frames", "show.window_frames")
	if err != nil {
		return nil, err
	}
	roundingRaw, err := required(showObj, "time_rounding", "show.time_rounding")
	if err != nil {
		return nil, err
	}
	rounding, err := parseRounding(roundingRaw, "show.time_rounding")
	if err != nil {
		return nil, err
	}
	colorStep, err := stringField(showObj, "color_step", "show.color_step")
	if err != nil {
		return nil, err
	}
	step, err := ParseColorStep(colorStep)
	if err != nil {
		return nil, err
	}
	durationUS, err := FrameToUS(durationFrames, fps, rounding)
	if err != nil {
		return nil, err
	}
	windowUS, err := FrameToUS(windowFrames, fps, rounding)
	if err != nil {
		return nil, err
	}
	name, _ := showObj["name"].(string)

	s := &Show{
		ID:            id,
		Name:          name,
		FPS:           fps,
		DurationUS:    durationUS,
		WindowUS:      windowUS,
		TimeRounding:  rounding,
		ColorStep:     colorStep,
		SourceVersion: Version1,
	}

	fixturesRaw, err := required(stageObj, "fixtures", "stage.fixtures")
	if err != nil {
		return nil, err
	}
	fixtures, err := array(fixturesRaw, "stage.fixtures")
	if err != nil {
		return nil, err
	}
	for i, item := range fixtures {
		path := fmt.Sprintf("stage.fixtures[%d]", i)
		m, err := object(item, path)
		if err != nil {
			return nil, err
		}
		fixID, err := stringField(m, "fixture_id", path+".fixture_id")
		if err != nil {
			return nil, err
		}
		fixName, _ := m["name"].(string)
		colorRaw, err := required(m, "color", path+".color")
		if err != nil {
			return nil, err
		}
		color, err := normalizedColor(colorRaw, path+".color", step)
		if err != nil {
			return nil, err
		}
		intensity, err := intField(m, "intensity", path+".intensity")
		if err != nil {
			return nil, err
		}
		posRaw, err := required(m, "position", path+".position")
		if err != nil {
			return nil, err
		}
		position, err := intArray3(posRaw, path+".position")
		if err != nil {
			return nil, err
		}
		focus, err := intField(m, "focus", path+".focus")
		if err != nil {
			return nil, err
		}
		rangesRaw, err := objectField(m, "range", path+".range")
		if err != nil {
			return nil, err
		}
		ranges, err := parseFixtureRanges(rangesRaw, path+".range")
		if err != nil {
			return nil, err
		}
		s.Fixtures = append(s.Fixtures, Fixture{
			ID: fixID, Name: fixName, Color: color, Intensity: int(intensity),
			Position: position, Focus: int(focus), Ranges: ranges,
		})
	}

	changesRaw, err := required(root, "changes", "changes")
	if err != nil {
		return nil, err
	}
	changes, err := array(changesRaw, "changes")
	if err != nil {
		return nil, err
	}
	var previous int64 = -1
	for i, item := range changes {
		path := fmt.Sprintf("changes[%d]", i)
		m, err := object(item, path)
		if err != nil {
			return nil, err
		}
		changeID, err := stringField(m, "change_id", path+".change_id")
		if err != nil {
			return nil, err
		}
		fixtureID, err := stringField(m, "fixture_id", path+".fixture_id")
		if err != nil {
			return nil, err
		}
		frame, err := intField(m, "frame", path+".frame")
		if err != nil {
			return nil, err
		}
		if frame < previous {
			return nil, engineError(CodeOrder, path+".at",
				"change points are not in logical-time order (%d < %d)", frame, previous)
		}
		previous = frame
		interpRaw, err := required(m, "interpolation", path+".interpolation")
		if err != nil {
			return nil, err
		}
		interp, err := parseInterpolation(interpRaw, path+".interpolation", Hold)
		if err != nil {
			return nil, err
		}
		valuesRaw, err := objectField(m, "values", path+".values")
		if err != nil {
			return nil, err
		}
		values, err := applyOptionalValues(valuesRaw, path+".values", true, step, "color", "intensity")
		if err != nil {
			return nil, err
		}
		atUS, err := FrameToUS(frame, fps, rounding)
		if err != nil {
			return nil, err
		}
		s.Changes = append(s.Changes, Change{
			ID: changeID, TargetKind: "fixture", TargetID: fixtureID,
			AtUS: atUS, Interpolation: interp, Values: values,
		})
	}

	scenesRaw, err := required(stageObj, "scenes", "stage.scenes")
	if err != nil {
		return nil, err
	}
	scenes, err := array(scenesRaw, "stage.scenes")
	if err != nil {
		return nil, err
	}
	for i, item := range scenes {
		path := fmt.Sprintf("stage.scenes[%d]", i)
		m, err := object(item, path)
		if err != nil {
			return nil, err
		}
		sceneID, err := stringField(m, "scene_id", path+".scene_id")
		if err != nil {
			return nil, err
		}
		sceneName, _ := m["name"].(string)
		frame, err := intField(m, "at_frame", path+".at_frame")
		if err != nil {
			return nil, err
		}
		atUS, err := FrameToUS(frame, fps, rounding)
		if err != nil {
			return nil, err
		}
		valuesRaw, err := objectField(m, "values", path+".values")
		if err != nil {
			return nil, err
		}
		scene := Scene{ID: sceneID, Name: sceneName, AtUS: atUS, Values: map[string]Values{}}
		for fixtureID, raw := range valuesRaw {
			vm, err := object(raw, path+".values."+fixtureID)
			if err != nil {
				return nil, err
			}
			v, err := applyOptionalValues(vm, path+".values."+fixtureID, true, step, "color", "intensity")
			if err != nil {
				return nil, err
			}
			scene.Values[fixtureID] = v
		}
		s.Scenes = append(s.Scenes, scene)
	}

	s.Defaults = Defaults{GroupEnabled: true, GroupMode: "override", Interpolation: Hold}
	s.UpgradeTraces = []UpgradeTrace{
		{Path: "groups", OldValue: nil, DefaultValue: []any{}, DefaultSource: "v3 default: no group control"},
		{Path: "defaults.group_enabled", OldValue: nil, DefaultValue: true, DefaultSource: "v3 declared default"},
		{Path: "defaults.group_mode", OldValue: nil, DefaultValue: "override", DefaultSource: "v3 declared default"},
		{Path: "defaults.interpolation", OldValue: nil, DefaultValue: "hold", DefaultSource: "v3 declared default"},
	}
	if err := s.Normalize(); err != nil {
		return nil, err
	}
	return s, nil
}

func loadV2(root map[string]any) (*Show, error) {
	project, err := objectField(root, "project", "project")
	if err != nil {
		return nil, err
	}
	rig, err := objectField(root, "rig", "rig")
	if err != nil {
		return nil, err
	}
	id, err := stringField(project, "id", "project.id")
	if err != nil {
		return nil, err
	}
	name, _ := project["title"].(string)
	fps, err := intField(project, "fps", "project.fps")
	if err != nil {
		return nil, err
	}
	durationMS, err := intField(project, "duration_ms", "project.duration_ms")
	if err != nil {
		return nil, err
	}
	windowMS, err := intField(project, "window_ms", "project.window_ms")
	if err != nil {
		return nil, err
	}
	roundingRaw, err := required(project, "time_rounding", "project.time_rounding")
	if err != nil {
		return nil, err
	}
	rounding, err := parseRounding(roundingRaw, "project.time_rounding")
	if err != nil {
		return nil, err
	}
	colorStep, err := stringField(project, "color_step", "project.color_step")
	if err != nil {
		return nil, err
	}
	step, err := ParseColorStep(colorStep)
	if err != nil {
		return nil, err
	}

	s := &Show{
		ID: id, Name: name, FPS: fps, DurationUS: MSToUS(durationMS),
		WindowUS: MSToUS(windowMS), TimeRounding: rounding, ColorStep: colorStep,
		SourceVersion: Version2,
		Defaults:      Defaults{GroupEnabled: true, GroupMode: "override", Interpolation: Hold},
	}

	lightsRaw, err := required(rig, "lights", "rig.lights")
	if err != nil {
		return nil, err
	}
	lights, err := array(lightsRaw, "rig.lights")
	if err != nil {
		return nil, err
	}
	for i, item := range lights {
		path := fmt.Sprintf("rig.lights[%d]", i)
		m, err := object(item, path)
		if err != nil {
			return nil, err
		}
		fixID, err := stringField(m, "id", path+".id")
		if err != nil {
			return nil, err
		}
		fixName, _ := m["name"].(string)
		colorRaw, err := required(m, "rgb", path+".rgb")
		if err != nil {
			return nil, err
		}
		color, err := rgb8Color(colorRaw, path+".rgb")
		if err != nil {
			return nil, err
		}
		brightness, err := intField(m, "brightness", path+".brightness")
		if err != nil {
			return nil, err
		}
		posRaw, err := required(m, "position", path+".position")
		if err != nil {
			return nil, err
		}
		position, err := intArray3(posRaw, path+".position")
		if err != nil {
			return nil, err
		}
		focus, err := intField(m, "focus", path+".focus")
		if err != nil {
			return nil, err
		}
		limitsRaw, err := objectField(m, "limits", path+".limits")
		if err != nil {
			return nil, err
		}
		ranges, err := parseFixtureRanges(limitsRaw, path+".limits")
		if err != nil {
			return nil, err
		}
		s.Fixtures = append(s.Fixtures, Fixture{
			ID: fixID, Name: fixName, Color: color, Intensity: int(brightness),
			Position: position, Focus: int(focus), Ranges: ranges,
		})
	}

	groupsRaw, err := required(rig, "groups", "rig.groups")
	if err != nil {
		return nil, err
	}
	groups, err := array(groupsRaw, "rig.groups")
	if err != nil {
		return nil, err
	}
	for i, item := range groups {
		path := fmt.Sprintf("rig.groups[%d]", i)
		m, err := object(item, path)
		if err != nil {
			return nil, err
		}
		groupID, err := stringField(m, "group_id", path+".group_id")
		if err != nil {
			return nil, err
		}
		groupName, _ := m["label"].(string)
		priority, err := intField(m, "priority", path+".priority")
		if err != nil {
			return nil, err
		}
		enabled, err := boolField(m, "enabled", path+".enabled")
		if err != nil {
			return nil, err
		}
		idsRaw, err := required(m, "light_ids", path+".light_ids")
		if err != nil {
			return nil, err
		}
		idsArr, err := array(idsRaw, path+".light_ids")
		if err != nil {
			return nil, err
		}
		fixtures := make([]string, 0, len(idsArr))
		for j, raw := range idsArr {
			lightID, err := valueString(raw, fmt.Sprintf("%s.light_ids[%d]", path, j))
			if err != nil {
				return nil, err
			}
			fixtures = append(fixtures, lightID)
		}
		s.Groups = append(s.Groups, Group{
			ID: groupID, Name: groupName, Priority: int(priority), Enabled: enabled,
			Mode: "override", Fixtures: fixtures, Groups: []string{},
		})
		s.UpgradeTraces = append(s.UpgradeTraces, UpgradeTrace{
			Path: fmt.Sprintf("groups.%s.members.groups", groupID), OldValue: nil,
			DefaultValue: []string{}, DefaultSource: "v2 group schema has no nested membership",
		})
	}

	keyframesRaw, err := required(root, "keyframes", "keyframes")
	if err != nil {
		return nil, err
	}
	keyframes, err := array(keyframesRaw, "keyframes")
	if err != nil {
		return nil, err
	}
	var previous int64 = -1
	for i, item := range keyframes {
		path := fmt.Sprintf("keyframes[%d]", i)
		m, err := object(item, path)
		if err != nil {
			return nil, err
		}
		changeID, err := stringField(m, "keyframe_id", path+".keyframe_id")
		if err != nil {
			return nil, err
		}
		timeMS, err := intField(m, "time_ms", path+".time_ms")
		if err != nil {
			return nil, err
		}
		if timeMS < previous {
			return nil, engineError(CodeOrder, path+".at",
				"change points are not in logical-time order (%d < %d)", timeMS*1000, previous*1000)
		}
		previous = timeMS
		interpRaw, err := required(m, "interpolation", path+".interpolation")
		if err != nil {
			return nil, err
		}
		interp, err := parseInterpolation(interpRaw, path+".interpolation", Hold)
		if err != nil {
			return nil, err
		}
		valuesRaw, err := objectField(m, "values", path+".values")
		if err != nil {
			return nil, err
		}
		values, err := applyOptionalValues(valuesRaw, path+".values", false, step, "rgb", "brightness")
		if err != nil {
			return nil, err
		}
		change := Change{ID: changeID, AtUS: MSToUS(timeMS), Interpolation: interp, Values: values}
		if raw, ok := m["group_id"]; ok {
			groupID, err := valueString(raw, path+".group_id")
			if err != nil {
				return nil, err
			}
			change.TargetKind, change.TargetID = "group", groupID
		} else if raw, ok := m["light_id"]; ok {
			lightID, err := valueString(raw, path+".light_id")
			if err != nil {
				return nil, err
			}
			change.TargetKind, change.TargetID = "fixture", lightID
		} else {
			return nil, engineError(CodeVersion, path+".group_id", "one of group_id or light_id is required")
		}
		s.Changes = append(s.Changes, change)
	}

	scenesRaw, err := required(rig, "scenes", "rig.scenes")
	if err != nil {
		return nil, err
	}
	scenes, err := array(scenesRaw, "rig.scenes")
	if err != nil {
		return nil, err
	}
	for i, item := range scenes {
		path := fmt.Sprintf("rig.scenes[%d]", i)
		m, err := object(item, path)
		if err != nil {
			return nil, err
		}
		sceneID, err := stringField(m, "scene_id", path+".scene_id")
		if err != nil {
			return nil, err
		}
		sceneName, _ := m["label"].(string)
		timeMS, err := intField(m, "time_ms", path+".time_ms")
		if err != nil {
			return nil, err
		}
		valuesRaw, err := objectField(m, "values", path+".values")
		if err != nil {
			return nil, err
		}
		scene := Scene{ID: sceneID, Name: sceneName, AtUS: MSToUS(timeMS), Values: map[string]Values{}}
		for fixtureID, raw := range valuesRaw {
			vm, err := object(raw, path+".values."+fixtureID)
			if err != nil {
				return nil, err
			}
			v, err := applyOptionalValues(vm, path+".values."+fixtureID, false, step, "rgb", "brightness")
			if err != nil {
				return nil, err
			}
			scene.Values[fixtureID] = v
		}
		s.Scenes = append(s.Scenes, scene)
	}

	s.UpgradeTraces = append(s.UpgradeTraces,
		UpgradeTrace{Path: "defaults.group_enabled", OldValue: nil, DefaultValue: true, DefaultSource: "v3 declared default"},
		UpgradeTrace{Path: "defaults.group_mode", OldValue: nil, DefaultValue: "override", DefaultSource: "v3 declared default"},
		UpgradeTrace{Path: "defaults.interpolation", OldValue: nil, DefaultValue: "hold", DefaultSource: "v2 has no project-level default"},
	)
	if err := s.Normalize(); err != nil {
		return nil, err
	}
	return s, nil
}

func loadV3(root map[string]any) (*Show, error) {
	production, err := objectField(root, "production", "production")
	if err != nil {
		return nil, err
	}
	id, err := stringField(production, "id", "production.id")
	if err != nil {
		return nil, err
	}
	name, _ := production["name"].(string)
	fps, err := intField(production, "fps", "production.fps")
	if err != nil {
		return nil, err
	}
	durationMS, err := intField(production, "duration_ms", "production.duration_ms")
	if err != nil {
		return nil, err
	}
	windowMS, err := intField(production, "window_ms", "production.window_ms")
	if err != nil {
		return nil, err
	}
	roundingRaw, err := required(production, "time_rounding", "production.time_rounding")
	if err != nil {
		return nil, err
	}
	rounding, err := parseRounding(roundingRaw, "production.time_rounding")
	if err != nil {
		return nil, err
	}
	colorStep, err := stringField(production, "color_step", "production.color_step")
	if err != nil {
		return nil, err
	}
	step, err := ParseColorStep(colorStep)
	if err != nil {
		return nil, err
	}
	defaultsRaw, err := objectField(root, "defaults", "defaults")
	if err != nil {
		return nil, err
	}
	groupEnabled, err := boolField(defaultsRaw, "group_enabled", "defaults.group_enabled")
	if err != nil {
		return nil, err
	}
	groupMode, err := stringField(defaultsRaw, "group_mode", "defaults.group_mode")
	if err != nil {
		return nil, err
	}
	defaultInterpRaw, err := required(defaultsRaw, "interpolation", "defaults.interpolation")
	if err != nil {
		return nil, err
	}
	defaultInterp, err := parseInterpolation(defaultInterpRaw, "defaults.interpolation", Hold)
	if err != nil {
		return nil, err
	}

	s := &Show{
		ID: id, Name: name, FPS: fps, DurationUS: MSToUS(durationMS),
		WindowUS: MSToUS(windowMS), TimeRounding: rounding, ColorStep: colorStep,
		SourceVersion: Version3,
		Defaults:      Defaults{GroupEnabled: groupEnabled, GroupMode: groupMode, Interpolation: defaultInterp},
	}

	devicesRaw, err := required(root, "devices", "devices")
	if err != nil {
		return nil, err
	}
	devices, err := array(devicesRaw, "devices")
	if err != nil {
		return nil, err
	}
	for i, item := range devices {
		path := fmt.Sprintf("devices[%d]", i)
		m, err := object(item, path)
		if err != nil {
			return nil, err
		}
		fixID, err := stringField(m, "id", path+".id")
		if err != nil {
			return nil, err
		}
		fixName, _ := m["name"].(string)
		colorRaw, err := required(m, "color", path+".color")
		if err != nil {
			return nil, err
		}
		color, err := normalizedColor(colorRaw, path+".color", step)
		if err != nil {
			return nil, err
		}
		brightness, err := intField(m, "brightness", path+".brightness")
		if err != nil {
			return nil, err
		}
		posRaw, err := required(m, "position", path+".position")
		if err != nil {
			return nil, err
		}
		position, err := intArray3(posRaw, path+".position")
		if err != nil {
			return nil, err
		}
		focus, err := intField(m, "focus", path+".focus")
		if err != nil {
			return nil, err
		}
		rangesRaw, err := objectField(m, "ranges", path+".ranges")
		if err != nil {
			return nil, err
		}
		ranges, err := parseFixtureRanges(rangesRaw, path+".ranges")
		if err != nil {
			return nil, err
		}
		s.Fixtures = append(s.Fixtures, Fixture{
			ID: fixID, Name: fixName, Color: color, Intensity: int(brightness),
			Position: position, Focus: int(focus), Ranges: ranges,
		})
	}

	groupsRaw, err := required(root, "controlGroups", "controlGroups")
	if err != nil {
		return nil, err
	}
	groups, err := array(groupsRaw, "controlGroups")
	if err != nil {
		return nil, err
	}
	for i, item := range groups {
		path := fmt.Sprintf("controlGroups[%d]", i)
		m, err := object(item, path)
		if err != nil {
			return nil, err
		}
		groupID, err := stringField(m, "id", path+".id")
		if err != nil {
			return nil, err
		}
		groupName, _ := m["name"].(string)
		priority, err := intField(m, "priority", path+".priority")
		if err != nil {
			return nil, err
		}
		enabled, err := boolField(m, "enabled", path+".enabled")
		if err != nil {
			return nil, err
		}
		mode, err := stringField(m, "mode", path+".mode")
		if err != nil {
			return nil, err
		}
		members, err := objectField(m, "members", path+".members")
		if err != nil {
			return nil, err
		}
		g := Group{ID: groupID, Name: groupName, Priority: int(priority), Enabled: enabled, Mode: mode}
		if raw, ok := members["fixtures"]; ok {
			a, err := array(raw, path+".members.fixtures")
			if err != nil {
				return nil, err
			}
			for j, x := range a {
				id, err := valueString(x, fmt.Sprintf("%s.members.fixtures[%d]", path, j))
				if err != nil {
					return nil, err
				}
				g.Fixtures = append(g.Fixtures, id)
			}
		}
		if raw, ok := members["groups"]; ok {
			a, err := array(raw, path+".members.groups")
			if err != nil {
				return nil, err
			}
			for j, x := range a {
				id, err := valueString(x, fmt.Sprintf("%s.members.groups[%d]", path, j))
				if err != nil {
					return nil, err
				}
				g.Groups = append(g.Groups, id)
			}
		}
		s.Groups = append(s.Groups, g)
	}

	changesRaw, err := required(root, "changes", "changes")
	if err != nil {
		return nil, err
	}
	changes, err := array(changesRaw, "changes")
	if err != nil {
		return nil, err
	}
	var previous int64 = -1
	for i, item := range changes {
		path := fmt.Sprintf("changes[%d]", i)
		m, err := object(item, path)
		if err != nil {
			return nil, err
		}
		changeID, err := stringField(m, "id", path+".id")
		if err != nil {
			return nil, err
		}
		targetKind, err := stringField(m, "target_kind", path+".target_kind")
		if err != nil {
			return nil, err
		}
		targetID, err := stringField(m, "target_id", path+".target_id")
		if err != nil {
			return nil, err
		}
		atMS, err := intField(m, "at_ms", path+".at_ms")
		if err != nil {
			return nil, err
		}
		if atMS*1000 < previous {
			return nil, engineError(CodeOrder, path+".at",
				"change points are not in logical-time order (%d < %d)", atMS*1000, previous)
		}
		previous = atMS * 1000
		transition, err := objectField(m, "transition", path+".transition")
		if err != nil {
			return nil, err
		}
		kindRaw, err := required(transition, "kind", path+".transition.kind")
		if err != nil {
			return nil, err
		}
		interp, err := parseInterpolation(kindRaw, path+".transition.kind", "")
		if err != nil {
			return nil, err
		}
		update, err := objectField(m, "update", path+".update")
		if err != nil {
			return nil, err
		}
		values, err := applyOptionalValues(update, path+".update", true, step, "color", "brightness")
		if err != nil {
			return nil, err
		}
		s.Changes = append(s.Changes, Change{
			ID: changeID, TargetKind: targetKind, TargetID: targetID,
			AtUS: MSToUS(atMS), Interpolation: interp, Values: values,
		})
	}

	scenesRaw, err := required(root, "scenes", "scenes")
	if err != nil {
		return nil, err
	}
	scenes, err := array(scenesRaw, "scenes")
	if err != nil {
		return nil, err
	}
	for i, item := range scenes {
		path := fmt.Sprintf("scenes[%d]", i)
		m, err := object(item, path)
		if err != nil {
			return nil, err
		}
		sceneID, err := stringField(m, "scene_id", path+".scene_id")
		if err != nil {
			return nil, err
		}
		sceneName, _ := m["name"].(string)
		atMS, err := intField(m, "at_ms", path+".at_ms")
		if err != nil {
			return nil, err
		}
		valuesRaw, err := objectField(m, "values", path+".values")
		if err != nil {
			return nil, err
		}
		scene := Scene{ID: sceneID, Name: sceneName, AtUS: MSToUS(atMS), Values: map[string]Values{}}
		for fixtureID, raw := range valuesRaw {
			vm, err := object(raw, path+".values."+fixtureID)
			if err != nil {
				return nil, err
			}
			v, err := applyOptionalValues(vm, path+".values."+fixtureID, true, step, "color", "brightness")
			if err != nil {
				return nil, err
			}
			scene.Values[fixtureID] = v
		}
		s.Scenes = append(s.Scenes, scene)
	}

	if err := s.Normalize(); err != nil {
		return nil, err
	}
	return s, nil
}
