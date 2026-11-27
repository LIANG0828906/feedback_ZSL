package stage

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type ExportResult struct {
	Version          string            `json:"version"`
	Data             []byte            `json:"-"`
	Losses           []Loss            `json:"losses,omitempty"`
	ConversionTraces []ConversionTrace `json:"conversion_traces,omitempty"`
}

// ExportProject performs an explicit internal -> schema conversion. If the
// target cannot preserve structure or color exactly, the returned result still
// contains a loadable artifact and the error has code E_LOSSY.
func ExportProject(p *Project, target string) (ExportResult, error) {
	if p == nil {
		return ExportResult{}, domainError(CodeValue, "nil project", "project")
	}
	if err := ValidateProject(p); err != nil {
		return ExportResult{}, err
	}
	var (
		doc    map[string]any
		losses []Loss
		err    error
	)
	switch target {
	case SchemaV1:
		doc, losses, err = exportV1(p)
	case SchemaV2:
		doc, losses, err = exportV2(p)
	case SchemaV3:
		doc, losses, err = exportV3(p)
	default:
		return ExportResult{}, &Error{
			Code:    CodeSchema,
			Message: "unknown export target",
			Field:   "schema",
			Version: target,
		}
	}
	if err != nil {
		return ExportResult{}, err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return ExportResult{}, fmt.Errorf("marshal %s: %w", target, err)
	}
	data = append(data, '\n')
	// A downgraded artifact is only acceptable if the corresponding loader can
	// read it back and complete structural validation.
	loaded, err := LoadProject(data)
	if err != nil {
		return ExportResult{Version: target, Data: data, Losses: losses}, err
	}
	res := ExportResult{Version: target, Data: data, Losses: losses, ConversionTraces: loaded.Traces}
	if len(losses) > 0 {
		details := make([]string, 0, len(losses))
		for _, loss := range losses {
			details = append(details, loss.Error())
		}
		return res, &Error{
			Code:    CodeLossy,
			Message: fmt.Sprintf("%s export completed with %d lossy operation(s)", target, len(losses)),
			Version: target,
			Details: details,
		}
	}
	return res, nil
}

// ConvertProject is the explicit version-to-version API.
func ConvertProject(data []byte, target string) (ExportResult, error) {
	p, err := LoadProject(data)
	if err != nil {
		return ExportResult{}, err
	}
	return ExportProject(p, target)
}

func exportV1(p *Project) (map[string]any, []Loss, error) {
	losses := groupFlattenLosses(p, SchemaV1)
	fixtures := make([]any, 0, len(p.Fixtures))
	for i, f := range p.Fixtures {
		path := fmt.Sprintf("fixtures[%d]", i)
		pan, _, err := MicroDegToDeciDeg(f.PanMicroDeg)
		if err != nil {
			return nil, losses, withField(err, path+".orientation.pan_ddeg")
		}
		tilt, _, err := MicroDegToDeciDeg(f.TiltMicroDeg)
		if err != nil {
			return nil, losses, withField(err, path+".orientation.tilt_ddeg")
		}
		inner, _, err := MicroDegToDeciDeg(f.InnerHalfMicroDeg)
		if err != nil {
			return nil, losses, withField(err, path+".cone.inner_ddeg")
		}
		outer, _, err := MicroDegToDeciDeg(f.OuterHalfMicroDeg)
		if err != nil {
			return nil, losses, withField(err, path+".cone.outer_ddeg")
		}
		cct, _, err := CCTKelvinToPermille(f.CCTKelvin)
		if err != nil {
			return nil, losses, withField(err, path+".cct_permille")
		}
		level, _, err := PermyriadToPermille(f.LevelPermyriad)
		if err != nil {
			return nil, losses, withField(err, path+".level_permille")
		}
		rgb := [3]int64{}
		for c := range rgb {
			rgb[c], _, err = PermyriadToPermille(f.RGBPermyriad[c])
			if err != nil {
				return nil, losses, withField(err, fmt.Sprintf("%s.rgb_permille[%d]", path, c))
			}
		}
		if lossyColorPermille(path+".rgb_permille", f.ID, "", p.SourceVersion, SchemaV1, f.RGBPermyriad, rgb) {
			losses = append(losses, colorLoss(path+".rgb_permille", f.ID, "", p.SourceVersion, SchemaV1, f.RGBPermyriad, vecToArray(rgb)))
		}
		cone := map[string]any{"inner_ddeg": inner}
		defaultOuter, _, _ := ScaleInt(f.InnerHalfMicroDeg, 5, 2, RoundExact, "cone")
		if defaultOuter != f.OuterHalfMicroDeg {
			cone["outer_ddeg"] = outer
		}
		fixtures = append(fixtures, map[string]any{
			"id":             f.ID,
			"position_mm":    []int64{f.PositionUM[0] / 1000, f.PositionUM[1] / 1000, f.PositionUM[2] / 1000},
			"orientation":    map[string]any{"pan_ddeg": pan, "tilt_ddeg": tilt},
			"cone":           cone,
			"cct_permille":   cct,
			"level_permille": level,
			"rgb_permille":   []int64{rgb[0], rgb[1], rgb[2]},
		})
	}

	cues := make([]any, 0, len(p.Cues))
	for i, c := range p.Cues {
		path := fmt.Sprintf("cues[%d]", i)
		start, _, err := MSToTick(c.StartMS, 120)
		if err != nil {
			return nil, losses, withField(err, path+".start_tick")
		}
		end, _, err := MSToTick(c.EndMS, 120)
		if err != nil {
			return nil, losses, withField(err, path+".end_tick")
		}
		item := map[string]any{
			"id": c.ID, "fixture": c.FixtureID, "kind": string(c.Kind),
			"start_tick": start, "end_tick": end,
		}
		switch c.Kind {
		case CueFade:
			from, _, err := PermyriadToPermille(c.FromLevel)
			if err != nil {
				return nil, losses, withField(err, path+".from_level_permille")
			}
			to, _, err := PermyriadToPermille(c.ToLevel)
			if err != nil {
				return nil, losses, withField(err, path+".to_level_permille")
			}
			item["from_level_permille"], item["to_level_permille"] = from, to
		case CueSweep:
			fromPan, _, err := MicroDegToDeciDeg(c.FromPan)
			if err != nil {
				return nil, losses, withField(err, path+".from_pan_ddeg")
			}
			toPan, _, err := MicroDegToDeciDeg(c.ToPan)
			if err != nil {
				return nil, losses, withField(err, path+".to_pan_ddeg")
			}
			fromTilt, _, err := MicroDegToDeciDeg(c.FromTilt)
			if err != nil {
				return nil, losses, withField(err, path+".from_tilt_ddeg")
			}
			toTilt, _, err := MicroDegToDeciDeg(c.ToTilt)
			if err != nil {
				return nil, losses, withField(err, path+".to_tilt_ddeg")
			}
			item["from_pan_ddeg"], item["to_pan_ddeg"] = fromPan, toPan
			item["from_tilt_ddeg"], item["to_tilt_ddeg"] = fromTilt, toTilt
		case CueColor:
			from := [3]int64{}
			to := [3]int64{}
			for ch := 0; ch < 3; ch++ {
				from[ch], _, err = PermyriadToPermille(c.FromRGB[ch])
				if err != nil {
					return nil, losses, withField(err, fmt.Sprintf("%s.from_rgb_permille[%d]", path, ch))
				}
				to[ch], _, err = PermyriadToPermille(c.ToRGB[ch])
				if err != nil {
					return nil, losses, withField(err, fmt.Sprintf("%s.to_rgb_permille[%d]", path, ch))
				}
			}
			if lossyColorPermille(path+".from_rgb_permille", "", c.ID, p.SourceVersion, SchemaV1, c.FromRGB, from) {
				losses = append(losses, colorLoss(path+".from_rgb_permille", "", c.ID, p.SourceVersion, SchemaV1, c.FromRGB, vecToArray(from)))
			}
			if lossyColorPermille(path+".to_rgb_permille", "", c.ID, p.SourceVersion, SchemaV1, c.ToRGB, to) {
				losses = append(losses, colorLoss(path+".to_rgb_permille", "", c.ID, p.SourceVersion, SchemaV1, c.ToRGB, vecToArray(to)))
			}
			item["from_rgb_permille"], item["to_rgb_permille"] = []int64{from[0], from[1], from[2]}, []int64{to[0], to[1], to[2]}
		}
		cues = append(cues, item)
	}
	duration, _, err := MSToTick(p.Timeline.DurationMS, 120)
	if err != nil {
		return nil, losses, withField(err, "timeline.duration_tick")
	}
	windows := make([]any, 0, len(p.Timeline.Windows))
	for i, w := range p.Timeline.Windows {
		path := fmt.Sprintf("timeline.windows[%d]", i)
		start, _, err := MSToTick(w.StartMS, 120)
		if err != nil {
			return nil, losses, withField(err, path+".start_tick")
		}
		end, _, err := MSToTick(w.EndMS, 120)
		if err != nil {
			return nil, losses, withField(err, path+".end_tick")
		}
		samples := make([]int64, 0, len(w.Samples))
		for j, sample := range w.Samples {
			tick, _, err := MSToTick(sample, 120)
			if err != nil {
				return nil, losses, withField(err, fmt.Sprintf("%s.samples_tick[%d]", path, j))
			}
			samples = append(samples, tick)
		}
		windows = append(windows, map[string]any{
			"number": w.Number, "start_tick": start, "end_tick": end, "samples_tick": samples,
		})
	}
	return map[string]any{
		"schema": SchemaV1, "project_id": p.ProjectID, "seed": p.Seed,
		"parameter_version": p.ParameterVersion,
		"timebase":          map[string]any{"tick_hz": 120},
		"fixtures":          fixtures, "cues": cues,
		"timeline": map[string]any{"duration_tick": duration, "windows": windows},
	}, losses, nil
}

func exportV2(p *Project) (map[string]any, []Loss, error) {
	losses := groupFlattenLosses(p, SchemaV2)
	fixtures := make([]any, 0, len(p.Fixtures))
	for i, f := range p.Fixtures {
		path := fmt.Sprintf("fixtures[%d]", i)
		pan, _, err := MicroDegToMicroRad(f.PanMicroDeg)
		if err != nil {
			return nil, losses, withField(err, path+".aim.pan_urad")
		}
		tilt, _, err := MicroDegToMicroRad(f.TiltMicroDeg)
		if err != nil {
			return nil, losses, withField(err, path+".aim.tilt_urad")
		}
		inner, _, err := MicroDegToMicroRad(f.InnerHalfMicroDeg)
		if err != nil {
			return nil, losses, withField(err, path+".beam.inner_half_urad")
		}
		outer, _, err := MicroDegToMicroRad(f.OuterHalfMicroDeg)
		if err != nil {
			return nil, losses, withField(err, path+".beam.outer_half_urad")
		}
		colorText, err := hexRGB(f.RGBPermyriad)
		if err != nil {
			return nil, losses, err
		}
		loadedColor, err := parseHexRGB(colorText, SchemaV2, path+".color_hex")
		if err != nil {
			return nil, losses, err
		}
		if loadedColor != [3]int64(f.RGBPermyriad) {
			losses = append(losses, colorLoss(path+".color_hex", f.ID, "", p.SourceVersion, SchemaV2, f.RGBPermyriad, loadedColor))
		}
		fixtures = append(fixtures, map[string]any{
			"uid":          f.ID,
			"position_mm":  []int64{f.PositionUM[0] / 1000, f.PositionUM[1] / 1000, f.PositionUM[2] / 1000},
			"aim":          map[string]any{"pan_urad": pan, "tilt_urad": tilt},
			"beam":         map[string]any{"inner_half_urad": inner, "outer_half_urad": outer},
			"cct_kelvin":   f.CCTKelvin,
			"level_myriad": f.LevelPermyriad,
			"color_hex":    colorText,
		})
	}
	cues := make([]any, 0, len(p.Cues))
	for i, c := range p.Cues {
		path := fmt.Sprintf("cues[%d]", i)
		start, _, err := MSToMicrosecond(c.StartMS)
		if err != nil {
			return nil, losses, withField(err, path+".start_us")
		}
		end, _, err := MSToMicrosecond(c.EndMS)
		if err != nil {
			return nil, losses, withField(err, path+".end_us")
		}
		item := map[string]any{"id": c.ID, "fixture": c.FixtureID, "kind": string(c.Kind), "start_us": start, "end_us": end}
		switch c.Kind {
		case CueFade:
			item["from_level_myriad"], item["to_level_myriad"] = c.FromLevel, c.ToLevel
		case CueSweep:
			fromPan, _, err := MicroDegToMicroRad(c.FromPan)
			if err != nil {
				return nil, losses, withField(err, path+".from_pan_urad")
			}
			toPan, _, err := MicroDegToMicroRad(c.ToPan)
			if err != nil {
				return nil, losses, withField(err, path+".to_pan_urad")
			}
			fromTilt, _, err := MicroDegToMicroRad(c.FromTilt)
			if err != nil {
				return nil, losses, withField(err, path+".from_tilt_urad")
			}
			toTilt, _, err := MicroDegToMicroRad(c.ToTilt)
			if err != nil {
				return nil, losses, withField(err, path+".to_tilt_urad")
			}
			item["from_pan_urad"], item["to_pan_urad"] = fromPan, toPan
			item["from_tilt_urad"], item["to_tilt_urad"] = fromTilt, toTilt
		case CueColor:
			fromText, err := hexRGB(c.FromRGB)
			if err != nil {
				return nil, losses, err
			}
			toText, err := hexRGB(c.ToRGB)
			if err != nil {
				return nil, losses, err
			}
			from, _ := parseHexRGB(fromText, SchemaV2, path+".from_color_hex")
			to, _ := parseHexRGB(toText, SchemaV2, path+".to_color_hex")
			if from != [3]int64(c.FromRGB) {
				losses = append(losses, colorLoss(path+".from_color_hex", "", c.ID, p.SourceVersion, SchemaV2, c.FromRGB, from))
			}
			if to != [3]int64(c.ToRGB) {
				losses = append(losses, colorLoss(path+".to_color_hex", "", c.ID, p.SourceVersion, SchemaV2, c.ToRGB, to))
			}
			item["from_color_hex"], item["to_color_hex"] = fromText, toText
		}
		cues = append(cues, item)
	}
	duration, _, err := MSToMicrosecond(p.Timeline.DurationMS)
	if err != nil {
		return nil, losses, withField(err, "timeline.duration_us")
	}
	windows := make([]any, 0, len(p.Timeline.Windows))
	for i, w := range p.Timeline.Windows {
		path := fmt.Sprintf("timeline.windows[%d]", i)
		start, _, err := MSToMicrosecond(w.StartMS)
		if err != nil {
			return nil, losses, withField(err, path+".start_us")
		}
		end, _, err := MSToMicrosecond(w.EndMS)
		if err != nil {
			return nil, losses, withField(err, path+".end_us")
		}
		samples := make([]int64, 0, len(w.Samples))
		for j, sample := range w.Samples {
			us, _, err := MSToMicrosecond(sample)
			if err != nil {
				return nil, losses, withField(err, fmt.Sprintf("%s.samples_us[%d]", path, j))
			}
			samples = append(samples, us)
		}
		windows = append(windows, map[string]any{"number": w.Number, "start_us": start, "end_us": end, "samples_us": samples})
	}
	return map[string]any{
		"schema": SchemaV2, "project_id": p.ProjectID, "seed": p.Seed,
		"parameter_version": p.ParameterVersion,
		"fixtures":          fixtures, "cues": cues,
		"timeline": map[string]any{"duration_us": duration, "windows": windows},
	}, losses, nil
}

func exportV3(p *Project) (map[string]any, []Loss, error) {
	rigGroups := make(map[string]map[string][]any)
	labels := make(map[string]string)
	for _, g := range p.Groups {
		rig := g.Rig
		if rig == "" {
			rig = "main"
		}
		group := g.Name
		if group == "" {
			group = "default"
		}
		if rigGroups[rig] == nil {
			rigGroups[rig] = make(map[string][]any)
		}
		if _, ok := rigGroups[rig][group]; !ok {
			rigGroups[rig][group] = []any{}
		}
		labels[rig+"\x00"+group] = g.Label
	}
	for i, f := range p.Fixtures {
		path := fmt.Sprintf("fixtures[%d]", i)
		pan, _, err := MicroDegToMilliDeg(f.PanMicroDeg)
		if err != nil {
			return nil, nil, withField(err, path+".pan_mdeg")
		}
		tilt, _, err := MicroDegToMilliDeg(f.TiltMicroDeg)
		if err != nil {
			return nil, nil, withField(err, path+".tilt_mdeg")
		}
		inner, _, err := MicroDegToMilliDeg(f.InnerHalfMicroDeg)
		if err != nil {
			return nil, nil, withField(err, path+".cone.inner_half_mdeg")
		}
		outer, _, err := MicroDegToMilliDeg(f.OuterHalfMicroDeg)
		if err != nil {
			return nil, nil, withField(err, path+".cone.outer_half_mdeg")
		}
		cct, _, err := CCTKelvinToPermyriad(f.CCTKelvin)
		if err != nil {
			return nil, nil, withField(err, path+".cct_myriad")
		}
		rig := f.Rig
		if rig == "" {
			rig = "main"
		}
		group := f.Group
		if group == "" {
			group = "default"
		}
		if rigGroups[rig] == nil {
			rigGroups[rig] = make(map[string][]any)
		}
		if _, ok := rigGroups[rig][group]; !ok {
			rigGroups[rig][group] = []any{}
		}
		rigGroups[rig][group] = append(rigGroups[rig][group], map[string]any{
			"id":           f.ID,
			"position_mm":  []int64{f.PositionUM[0] / 1000, f.PositionUM[1] / 1000, f.PositionUM[2] / 1000},
			"pan_mdeg":     pan,
			"tilt_mdeg":    tilt,
			"cone":         map[string]any{"inner_half_mdeg": inner, "outer_half_mdeg": outer},
			"cct_myriad":   cct,
			"level_myriad": f.LevelPermyriad,
			"rgb_myriad":   []int64{f.RGBPermyriad[0], f.RGBPermyriad[1], f.RGBPermyriad[2]},
		})
	}
	rigNames := make([]string, 0, len(rigGroups))
	for rig := range rigGroups {
		rigNames = append(rigNames, rig)
	}
	sort.Strings(rigNames)
	rigs := make([]any, 0, len(rigNames))
	for _, rig := range rigNames {
		groupNames := make([]string, 0, len(rigGroups[rig]))
		for group := range rigGroups[rig] {
			groupNames = append(groupNames, group)
		}
		sort.Strings(groupNames)
		groups := make([]any, 0, len(groupNames))
		for _, group := range groupNames {
			item := map[string]any{"name": group, "fixtures": rigGroups[rig][group]}
			if label := labels[rig+"\x00"+group]; label != "" {
				item["label"] = label
			}
			groups = append(groups, item)
		}
		rigs = append(rigs, map[string]any{"name": rig, "groups": groups})
	}
	cues := make([]any, 0, len(p.Cues))
	for i, c := range p.Cues {
		path := fmt.Sprintf("cues[%d]", i)
		item := map[string]any{"id": c.ID, "fixture": c.FixtureID, "kind": string(c.Kind), "start_ms": c.StartMS, "end_ms": c.EndMS}
		switch c.Kind {
		case CueFade:
			item["from_level_myriad"], item["to_level_myriad"] = c.FromLevel, c.ToLevel
		case CueSweep:
			fromPan, _, err := MicroDegToMilliDeg(c.FromPan)
			if err != nil {
				return nil, nil, withField(err, path+".from_pan_mdeg")
			}
			toPan, _, err := MicroDegToMilliDeg(c.ToPan)
			if err != nil {
				return nil, nil, withField(err, path+".to_pan_mdeg")
			}
			fromTilt, _, err := MicroDegToMilliDeg(c.FromTilt)
			if err != nil {
				return nil, nil, withField(err, path+".from_tilt_mdeg")
			}
			toTilt, _, err := MicroDegToMilliDeg(c.ToTilt)
			if err != nil {
				return nil, nil, withField(err, path+".to_tilt_mdeg")
			}
			item["from_pan_mdeg"], item["to_pan_mdeg"] = fromPan, toPan
			item["from_tilt_mdeg"], item["to_tilt_mdeg"] = fromTilt, toTilt
		case CueColor:
			item["from_rgb_myriad"] = []int64{c.FromRGB[0], c.FromRGB[1], c.FromRGB[2]}
			item["to_rgb_myriad"] = []int64{c.ToRGB[0], c.ToRGB[1], c.ToRGB[2]}
		}
		cues = append(cues, item)
	}
	windows := make([]any, 0, len(p.Timeline.Windows))
	for _, w := range p.Timeline.Windows {
		windows = append(windows, map[string]any{
			"number": w.Number, "start_ms": w.StartMS, "end_ms": w.EndMS,
			"samples_ms": append([]int64(nil), w.Samples...),
		})
	}
	return map[string]any{
		"schema": SchemaV3, "project_id": p.ProjectID, "seed": p.Seed,
		"parameter_version": p.ParameterVersion,
		"rigs":              rigs, "cues": cues,
		"timeline": map[string]any{"duration_ms": p.Timeline.DurationMS, "windows": windows},
	}, nil, nil
}

func groupFlattenLosses(p *Project, target string) []Loss {
	seen := map[string]struct{}{}
	var out []Loss
	groups := append([]FixtureGroup(nil), p.Groups...)
	for _, f := range p.Fixtures {
		if f.Rig != "" || f.Group != "" {
			groups = append(groups, FixtureGroup{Rig: f.Rig, Name: f.Group})
		}
	}
	for _, g := range groups {
		rig := g.Rig
		if rig == "" {
			rig = "main"
		}
		group := g.Name
		if group == "" {
			group = "default"
		}
		key := rig + "\x00" + group
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, Loss{
			Code: CodeLossy, Field: "groups." + rig + "." + group,
			SourceVersion: p.SourceVersion, TargetVersion: target,
			Reason:      "target schema has no grouped fixture array",
			Disposition: "flattened into a flat fixture list; IDs and all optical fields are preserved",
			Original:    rig + "/" + group,
			Exported:    "fixtures[]",
		})
	}
	return out
}

func colorLoss(field, fixtureID, cueID, source, target string, original, exported [3]int64) Loss {
	return Loss{
		Code: CodeLossy, Field: field, FixtureID: fixtureID, CueID: cueID,
		SourceVersion: source, TargetVersion: target,
		Reason:      "target color encoding has lower precision",
		Disposition: "nearest representable color emitted; loadback remains structurally valid",
		Original:    formatRGB(original),
		Exported:    formatRGB(exported),
	}
}

func lossyColorPermille(field, fixtureID, cueID, source, target string, original, exported [3]int64) bool {
	for i := range original {
		if exported[i]*10 != original[i] {
			return true
		}
	}
	return false
}

func vecToArray(v [3]int64) [3]int64 {
	return v
}

func withField(err error, field string) error {
	var de *Error
	if e, ok := err.(*Error); ok {
		de = e
	} else {
		return err
	}
	de.Field = field
	de.FieldName = field
	if strings.TrimSpace(de.Message) == "" {
		de.Message = "conversion failed"
	}
	return de
}
