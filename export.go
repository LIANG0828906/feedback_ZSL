package lightshow

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type ExportResult struct {
	Version Version
	Data    []byte
	Losses  []Loss
}

// Export converts the internal model to a concrete source version. Lossy
// conversions still produce a usable document and return the exact loss list.
func (s *Show) Export(version Version) (ExportResult, error) {
	if s == nil {
		return ExportResult{}, engineError(CodeVersion, "project", "project is nil")
	}
	show := s.Clone()
	if err := show.Normalize(); err != nil {
		return ExportResult{}, err
	}
	var (
		data   any
		losses []Loss
		err    error
	)
	switch version {
	case Version1:
		data, losses, err = exportV1(show)
	case Version2:
		data, losses, err = exportV2(show)
	case Version3:
		data, losses, err = exportV3(show)
	default:
		return ExportResult{}, engineError(CodeVersion, "targetVersion", "unknown project version %q", version)
	}
	if err != nil {
		return ExportResult{}, err
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return ExportResult{}, engineError(CodeVersion, "export", "encode project: %v", err)
	}
	return ExportResult{Version: version, Data: b, Losses: losses}, nil
}

// ExportStrict is the E_LOSSY variant of Export. The result is populated even
// when an error is returned so callers can persist both output and losses.
func (s *Show) ExportStrict(version Version) (ExportResult, error) {
	result, err := s.Export(version)
	if err != nil {
		return result, err
	}
	if len(result.Losses) > 0 {
		return result, lossyError(result.Losses)
	}
	return result, nil
}

func normalizedNumber(v uint16) json.Number {
	s := strconv.FormatFloat(Color16ToNormalized(v), 'f', 12, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-0" {
		return json.Number("0")
	}
	return json.Number(s)
}

func rgb8Numbers(c RGB) []int {
	r, g, b := RGB16To8(c)
	return []int{int(r), int(g), int(b)}
}

func normalized8(c RGB) []json.Number {
	r, g, b := RGB16To8(c)
	return []json.Number{
		normalizedNumber(uint16(r) * 257),
		normalizedNumber(uint16(g) * 257),
		normalizedNumber(uint16(b) * 257),
	}
}

func colorIs8Bit(c RGB) bool {
	return c.R%257 == 0 && c.G%257 == 0 && c.B%257 == 0
}

func valuesV1V3(v Values) map[string]any {
	out := map[string]any{}
	if v.Color != nil {
		out["color"] = []json.Number{
			normalizedNumber(v.Color.R), normalizedNumber(v.Color.G), normalizedNumber(v.Color.B),
		}
	}
	if v.Intensity != nil {
		out["intensity"] = *v.Intensity
	}
	if v.Position != nil {
		out["position"] = []int{v.Position[0], v.Position[1], v.Position[2]}
	}
	if v.Focus != nil {
		out["focus"] = *v.Focus
	}
	return out
}

func valuesV3(v Values) map[string]any {
	out := map[string]any{}
	if v.Color != nil {
		out["color"] = []json.Number{
			normalizedNumber(v.Color.R), normalizedNumber(v.Color.G), normalizedNumber(v.Color.B),
		}
	}
	if v.Intensity != nil {
		out["brightness"] = *v.Intensity
	}
	if v.Position != nil {
		out["position"] = []int{v.Position[0], v.Position[1], v.Position[2]}
	}
	if v.Focus != nil {
		out["focus"] = *v.Focus
	}
	return out
}

func valuesV2(v Values) map[string]any {
	out := map[string]any{}
	if v.Color != nil {
		out["rgb"] = rgb8Numbers(*v.Color)
	}
	if v.Intensity != nil {
		out["brightness"] = *v.Intensity
	}
	if v.Position != nil {
		out["position"] = []int{v.Position[0], v.Position[1], v.Position[2]}
	}
	if v.Focus != nil {
		out["focus"] = *v.Focus
	}
	return out
}

func rangesV1(f Fixture) map[string]any {
	return map[string]any{
		"intensity": map[string]any{"min": f.Ranges.Intensity.Min, "max": f.Ranges.Intensity.Max},
		"position": []any{
			map[string]any{"min": f.Ranges.Position[0].Min, "max": f.Ranges.Position[0].Max},
			map[string]any{"min": f.Ranges.Position[1].Min, "max": f.Ranges.Position[1].Max},
			map[string]any{"min": f.Ranges.Position[2].Min, "max": f.Ranges.Position[2].Max},
		},
		"focus": map[string]any{"min": f.Ranges.Focus.Min, "max": f.Ranges.Focus.Max},
	}
}

func exportV1(show *Show) (any, []Loss, error) {
	schedule, err := Schedule(show)
	if err != nil {
		return nil, nil, err
	}
	losses := make([]Loss, 0, len(show.Groups)+1)
	for _, id := range sortedGroupIDs(show.Groups) {
		losses = append(losses, Loss{
			Path: "groups." + id, Value: id, Reason: "v1 has no group control fields",
			Disposition: "group arbitration baked into per-fixture, per-frame changes; group control metadata is not emitted",
		})
	}
	if len(show.Changes) > 0 {
		losses = append(losses, Loss{
			Path: "changes", Value: fmt.Sprintf("%d source change points", len(show.Changes)),
			Reason:      "v1 has no group layer arbitration",
			Disposition: "effective fixture outputs sampled at every logical frame and emitted as hold keyframes",
		})
	}
	fixtures := make([]any, 0, len(show.Fixtures))
	for _, f := range show.Fixtures {
		fixtures = append(fixtures, map[string]any{
			"fixture_id": f.ID, "name": f.Name, "color": normalized8(f.Color),
			"intensity": f.Intensity, "position": []int{f.Position[0], f.Position[1], f.Position[2]},
			"focus": f.Focus, "range": rangesV1(f),
		})
	}
	scenes := make([]any, 0, len(show.Scenes))
	for _, scene := range show.Scenes {
		atFrame, err := USToFrame(scene.AtUS, show.FPS, show.TimeRounding)
		if err != nil {
			return nil, nil, err
		}
		values := map[string]any{}
		for id, value := range scene.Values {
			values[id] = valuesV1V3(value)
		}
		scenes = append(scenes, map[string]any{
			"scene_id": scene.ID, "name": scene.Name, "at_frame": atFrame, "values": values,
		})
	}
	changes := make([]any, 0, len(schedule.Frames)*len(show.Fixtures))
	for _, frame := range schedule.Frames {
		for _, light := range frame.Lights {
			changes = append(changes, map[string]any{
				"change_id":  fmt.Sprintf("bake.%s.%d", light.FixtureID, frame.Frame),
				"fixture_id": light.FixtureID, "frame": frame.Frame, "interpolation": "hold",
				"priority": 900000000,
				"values": map[string]any{
					"color":     normalized8(light.Color),
					"intensity": light.Intensity,
					"position":  []int{light.Position[0], light.Position[1], light.Position[2]},
					"focus":     light.Focus,
				},
			})
		}
	}
	windowFrames, err := USToFrame(show.WindowUS, show.FPS, show.TimeRounding)
	if err != nil {
		return nil, nil, err
	}
	root := map[string]any{
		"schema": "stage-lights/v1",
		"show": map[string]any{
			"id": show.ID, "name": show.Name, "fps": show.FPS,
			"duration_frames": show.DurationFrames(), "window_frames": windowFrames,
			"time_rounding": string(show.TimeRounding), "color_step": show.ColorStep,
		},
		"stage":   map[string]any{"fixtures": fixtures, "scenes": scenes},
		"changes": changes,
	}
	return root, losses, nil
}

func exportV2(show *Show) (any, []Loss, error) {
	var losses []Loss
	lights := make([]any, 0, len(show.Fixtures))
	for _, f := range show.Fixtures {
		lights = append(lights, map[string]any{
			"id": f.ID, "name": f.Name, "rgb": rgb8Numbers(f.Color),
			"brightness": f.Intensity, "position": []int{f.Position[0], f.Position[1], f.Position[2]},
			"focus": f.Focus, "limits": rangesV1(f),
		})
	}
	groups := make([]any, 0, len(show.Groups))
	for _, id := range sortedGroupIDs(show.Groups) {
		g := show.GroupByID(id)
		fixtures, err := show.ResolveGroupFixtures(id)
		if err != nil {
			return nil, nil, err
		}
		if len(g.Groups) > 0 {
			losses = append(losses, Loss{
				Path: "groups." + g.ID + ".members.groups", Value: "nested group membership",
				Reason:      "v2 groups can only contain fixtures",
				Disposition: "nested membership flattened to the deterministic transitive fixture set",
			})
		}
		if g.Mode != "override" {
			losses = append(losses, Loss{
				Path: "groups." + g.ID + ".mode", Value: g.Mode,
				Reason: "v2 group values have no mode field", Disposition: "group mode reset to override",
			})
		}
		groups = append(groups, map[string]any{
			"group_id": g.ID, "label": g.Name, "light_ids": fixtures,
			"priority": g.Priority, "enabled": g.Enabled,
		})
	}
	scenes := make([]any, 0, len(show.Scenes))
	for _, scene := range show.Scenes {
		values := map[string]any{}
		for id, value := range scene.Values {
			if value.Color != nil && !colorIs8Bit(*value.Color) {
				losses = append(losses, Loss{
					Path: "scenes." + scene.ID + ".values." + id + ".rgb", Value: value.Color.String(),
					Reason: "v2 stores 8-bit RGB", Disposition: "nearest 1/255 quantization",
				})
			}
			values[id] = valuesV2(value)
		}
		scenes = append(scenes, map[string]any{
			"scene_id": scene.ID, "label": scene.Name, "time_ms": USToMS(scene.AtUS, show.TimeRounding), "values": values,
		})
	}
	keyframes := make([]any, 0, len(show.Changes))
	for _, ch := range show.Changes {
		if ch.Values.Color != nil && !colorIs8Bit(*ch.Values.Color) {
			losses = append(losses, Loss{
				Path: ch.ID + ".values.rgb", Value: ch.Values.Color.String(),
				Reason: "v2 stores 8-bit RGB", Disposition: "nearest 1/255 quantization",
			})
		}
		interp := 0
		if ch.Interpolation == Linear {
			interp = 1
		}
		kf := map[string]any{
			"keyframe_id": ch.ID, "time_ms": USToMS(ch.AtUS, show.TimeRounding),
			"interpolation": interp, "values": valuesV2(ch.Values),
		}
		if ch.TargetKind == "group" {
			kf["group_id"] = ch.TargetID
		} else {
			kf["light_id"] = ch.TargetID
		}
		keyframes = append(keyframes, kf)
	}
	sort.Slice(losses, func(i, j int) bool {
		if losses[i].Path != losses[j].Path {
			return losses[i].Path < losses[j].Path
		}
		return losses[i].Value < losses[j].Value
	})
	root := map[string]any{
		"format_version": 2,
		"project": map[string]any{
			"id": show.ID, "title": show.Name, "fps": show.FPS,
			"duration_ms":   USToMS(show.DurationUS, show.TimeRounding),
			"window_ms":     USToMS(show.WindowUS, show.TimeRounding),
			"time_rounding": string(show.TimeRounding), "color_step": show.ColorStep,
		},
		"rig":       map[string]any{"lights": lights, "groups": groups, "scenes": scenes},
		"keyframes": keyframes,
	}
	return root, losses, nil
}

func exportV3(show *Show) (any, []Loss, error) {
	devices := make([]any, 0, len(show.Fixtures))
	for _, f := range show.Fixtures {
		devices = append(devices, map[string]any{
			"id": f.ID, "name": f.Name,
			"color": []json.Number{
				normalizedNumber(f.Color.R), normalizedNumber(f.Color.G), normalizedNumber(f.Color.B),
			},
			"brightness": f.Intensity,
			"position":   []int{f.Position[0], f.Position[1], f.Position[2]},
			"focus":      f.Focus, "ranges": rangesV1(f),
		})
	}
	groups := make([]any, 0, len(show.Groups))
	for _, id := range sortedGroupIDs(show.Groups) {
		g := show.GroupByID(id)
		groups = append(groups, map[string]any{
			"id": g.ID, "name": g.Name, "priority": g.Priority, "enabled": g.Enabled, "mode": g.Mode,
			"members": map[string]any{"fixtures": g.Fixtures, "groups": g.Groups},
		})
	}
	changes := make([]any, 0, len(show.Changes))
	for _, ch := range show.Changes {
		changes = append(changes, map[string]any{
			"id": ch.ID, "target_kind": ch.TargetKind, "target_id": ch.TargetID,
			"at_ms":      USToMS(ch.AtUS, show.TimeRounding),
			"transition": map[string]any{"kind": string(ch.Interpolation)},
			"update":     valuesV3(ch.Values),
		})
	}
	scenes := make([]any, 0, len(show.Scenes))
	for _, scene := range show.Scenes {
		values := map[string]any{}
		for id, value := range scene.Values {
			values[id] = valuesV3(value)
		}
		scenes = append(scenes, map[string]any{
			"scene_id": scene.ID, "name": scene.Name, "at_ms": USToMS(scene.AtUS, show.TimeRounding), "values": values,
		})
	}
	root := map[string]any{
		"formatVersion": 3,
		"production": map[string]any{
			"id": show.ID, "name": show.Name, "fps": show.FPS,
			"duration_ms":   USToMS(show.DurationUS, show.TimeRounding),
			"window_ms":     USToMS(show.WindowUS, show.TimeRounding),
			"time_rounding": string(show.TimeRounding), "color_step": show.ColorStep,
		},
		"defaults": map[string]any{
			"group_enabled": show.Defaults.GroupEnabled,
			"group_mode":    show.Defaults.GroupMode,
			"interpolation": string(show.Defaults.Interpolation),
		},
		"devices":       devices,
		"controlGroups": groups,
		"scenes":        scenes,
		"changes":       changes,
	}
	return root, nil, nil
}
