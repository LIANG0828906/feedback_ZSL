package schema

import (
	"encoding/json"
	"fmt"
	"sort"

	"screenrelay/internal/model"
	"screenrelay/internal/xconv"
)

func Export(project *model.Project, major int) ([]byte, *model.ConvertReport, error) {
	report := &model.ConvertReport{
		SourceVersion: project.SourceVersion,
		TargetVersion: fmt.Sprintf("%d.0", major),
	}
	var doc map[string]any
	var err error
	switch major {
	case 1:
		doc, err = encodeV1(project, report)
	case 2:
		doc, err = encodeV2(project, report)
	case 3:
		doc, err = encodeV3(project)
	default:
		return nil, report, model.SchemaError("$.schema_version", "cannot export unknown major %d", major)
	}
	if err != nil {
		return nil, report, err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, report, model.NewError(model.EInternal, "encode json: %v", err)
	}
	return data, report, nil
}

func sortedScreens(screens []model.Screen) []model.Screen {
	out := append([]model.Screen(nil), screens...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func logicalOrLossy(v int64, path string, report *model.ConvertReport) int64 {
	q, rem := xconv.CoordToLogicalFloor(v)
	if rem != 0 {
		report.Lossy = append(report.Lossy, model.LossyItem{
			Path: path, Value: fmt.Sprintf("%d subunits", v), Code: model.ELossy,
			Disposition: "floor_to_integer_logical_unit", Replacement: fmt.Sprintf("%d (remainder %d/256)", q, rem),
		})
	}
	return q
}

func msOrLossy(tick int64, report *model.ConvertReport) int64 {
	ms, rem := xconv.TickToMillisecondsFloor(tick)
	if rem != 0 {
		report.Lossy = append(report.Lossy, model.LossyItem{
			Path: "$.events/windows.time", Value: fmt.Sprintf("%d ticks", tick), Code: model.ELossy,
			Disposition: "floor_to_millisecond", Replacement: fmt.Sprintf("%d ms (remainder %d ticks)", ms, rem),
		})
	}
	return ms
}

func tick60OrLossy(tick int64, report *model.ConvertReport) int64 {
	const per60 = model.TicksPerSecond / 60
	q, rem := xconv.DivModFloor(tick, per60)
	if rem != 0 {
		report.Lossy = append(report.Lossy, model.LossyItem{
			Path: "$.events/windows.atTick60", Value: fmt.Sprintf("%d internal ticks", tick), Code: model.ELossy,
			Disposition: "floor_to_60hz_tick", Replacement: fmt.Sprintf("%d (remainder %d internal ticks)", q, rem),
		})
	}
	return q
}

func encodeV3(p *model.Project) (map[string]any, error) {
	screens := make([]any, 0, len(p.Screens))
	for _, s := range sortedScreens(p.Screens) {
		screens = append(screens, v3Screen(s))
	}
	events := make([]any, 0, len(p.Events))
	for _, e := range p.Events {
		item := map[string]any{
			"id": e.ID, "type": string(e.Kind), "screen_id": e.ScreenID,
			"tick90k": e.Tick, "revision": e.Revision,
		}
		if e.Screen != nil {
			item["screen"] = v3Screen(*e.Screen)
		}
		events = append(events, item)
	}
	windows := make([]any, 0, len(p.Windows))
	for _, w := range p.Windows {
		windows = append(windows, map[string]any{
			"id": w.ID, "seq": w.Seq, "tick90k": w.Tick,
			"max_packet_bytes": w.MaxPacketBytes, "content_seed": w.ContentSeed,
			"region": map[string]any{
				"x": w.Region.X / model.InternalCoordUnit,
				"y": w.Region.Y / model.InternalCoordUnit,
				"w": w.Region.W / model.InternalCoordUnit,
				"h": w.Region.H / model.InternalCoordUnit,
			},
		})
	}
	return map[string]any{
		"schema_version": "3.0",
		"session_id":     p.SessionID,
		"seed":           p.Seed,
		"master": map[string]any{
			"x": p.Master.X / model.InternalCoordUnit,
			"y": p.Master.Y / model.InternalCoordUnit,
			"w": p.Master.W / model.InternalCoordUnit,
			"h": p.Master.H / model.InternalCoordUnit,
		},
		"screens": screens, "events": events, "windows": windows,
	}, nil
}

func v3Screen(s model.Screen) map[string]any {
	scales := make([]string, 0, len(s.SupportedScales))
	for _, scale := range s.SupportedScales {
		scales = append(scales, string(scale))
	}
	chromas := make([]string, 0, len(s.SupportedChromas))
	for _, chroma := range s.SupportedChromas {
		chromas = append(chromas, string(chroma))
	}
	return map[string]any{
		"id": s.ID, "revision": s.Revision,
		"origin":     map[string]any{"x": s.Origin.X / model.InternalCoordUnit, "y": s.Origin.Y / model.InternalCoordUnit},
		"resolution": map[string]any{"width": s.PixelW, "height": s.PixelH},
		"scale_tier": string(s.Scale), "scale_capabilities": scales,
		"chroma": string(s.Chroma), "chroma_capabilities": chromas,
	}
}

func encodeV2(p *model.Project, report *model.ConvertReport) (map[string]any, error) {
	screens := make([]any, 0, len(p.Screens))
	for _, s := range sortedScreens(p.Screens) {
		scale, ok := v2ScaleCode(s.Scale)
		if !ok {
			return nil, model.NewError(model.EInternal, "v2 cannot encode scale %s", s.Scale)
		}
		chroma, ok := v2ChromaCode(s.Chroma)
		if !ok {
			return nil, model.NewError(model.EInternal, "v2 cannot encode chroma %s", s.Chroma)
		}
		scaleCaps := make([]string, 0, len(s.SupportedScales))
		for _, tier := range s.SupportedScales {
			code, ok := v2ScaleCode(tier)
			if !ok {
				return nil, model.NewError(model.EInternal, "v2 cannot encode scale cap %s", tier)
			}
			scaleCaps = append(scaleCaps, code)
		}
		chromaCaps := make([]string, 0, len(s.SupportedChromas))
		for _, c := range s.SupportedChromas {
			code, ok := v2ChromaCode(c)
			if !ok {
				return nil, model.NewError(model.EInternal, "v2 cannot encode chroma cap %s", c)
			}
			chromaCaps = append(chromaCaps, code)
		}
		screens = append(screens, map[string]any{
			"displayId": s.ID, "revision": s.Revision,
			"originX": s.Origin.X / model.InternalCoordUnit, "originY": s.Origin.Y / model.InternalCoordUnit,
			"pixelWidth": s.PixelW, "pixelHeight": s.PixelH,
			"scale": scale, "scaleCaps": scaleCaps, "chroma": chroma, "chromaCaps": chromaCaps,
		})
	}
	events := make([]any, 0, len(p.Events))
	for _, e := range p.Events {
		kind := map[model.EventKind]string{model.EventJoin: "joined", model.EventLeave: "left", model.EventTopology: "layoutChanged"}[e.Kind]
		item := map[string]any{
			"id": e.ID, "kind": kind, "displayId": e.ScreenID,
			"atTick60": tick60OrLossy(e.Tick, report), "revision": e.Revision,
		}
		if e.Screen != nil {
			item["display"] = encodeV2ScreenMap(*e.Screen)
		}
		events = append(events, item)
	}
	windows := make([]any, 0, len(p.Windows))
	for _, w := range p.Windows {
		windows = append(windows, map[string]any{
			"windowId": w.ID, "sequence": w.Seq, "atTick60": tick60OrLossy(w.Tick, report),
			"maxPacketBytes": w.MaxPacketBytes, "contentSeed": w.ContentSeed,
			"region": map[string]any{
				"absX":   logicalOrLossy(w.Region.X, "$.window.region.absX", report),
				"absY":   logicalOrLossy(w.Region.Y, "$.window.region.absY", report),
				"width":  logicalOrLossy(w.Region.W, "$.window.region.width", report),
				"height": logicalOrLossy(w.Region.H, "$.window.region.height", report),
			},
		})
	}
	return map[string]any{
		"schemaVersion": "2.3", "sessionId": p.SessionID, "rngSeed": p.Seed,
		"master": map[string]any{
			"x": p.Master.X / model.InternalCoordUnit, "y": p.Master.Y / model.InternalCoordUnit,
			"width": p.Master.W / model.InternalCoordUnit, "height": p.Master.H / model.InternalCoordUnit,
		},
		"displays": screens, "changes": events, "windows": windows,
	}, nil
}

func encodeV2ScreenMap(s model.Screen) map[string]any {
	scale, _ := v2ScaleCode(s.Scale)
	chroma, _ := v2ChromaCode(s.Chroma)
	scaleCaps := []any{}
	for _, v := range s.SupportedScales {
		code, _ := v2ScaleCode(v)
		scaleCaps = append(scaleCaps, code)
	}
	chromaCaps := []any{}
	for _, v := range s.SupportedChromas {
		code, _ := v2ChromaCode(v)
		chromaCaps = append(chromaCaps, code)
	}
	return map[string]any{
		"displayId": s.ID, "revision": s.Revision,
		"originX": s.Origin.X / model.InternalCoordUnit, "originY": s.Origin.Y / model.InternalCoordUnit,
		"pixelWidth": s.PixelW, "pixelHeight": s.PixelH,
		"scale": scale, "scaleCaps": scaleCaps, "chroma": chroma, "chromaCaps": chromaCaps,
	}
}

func encodeV1(p *model.Project, report *model.ConvertReport) (map[string]any, error) {
	anchor := ""
	if len(p.Screens) > 0 {
		anchor = sortedScreens(p.Screens)[0].ID
	}
	origins := map[string]model.Point{}
	screens := make([]any, 0, len(p.Screens))
	for _, s := range sortedScreens(p.Screens) {
		origins[s.ID] = s.Origin
		screens = append(screens, encodeV1Screen(s, "$.panels", report))
	}
	events := make([]any, 0, len(p.Events))
	for _, e := range p.Events {
		kind := map[model.EventKind]string{model.EventJoin: "in", model.EventLeave: "out", model.EventTopology: "layout"}[e.Kind]
		item := map[string]any{
			"eid": e.ID, "ms": msOrLossy(e.Tick, report), "type": kind,
			"pid": e.ScreenID, "rev": e.Revision,
		}
		if e.Screen != nil {
			item["panel"] = encodeV1Screen(*e.Screen, "$.events."+e.ID+".panel", report)
		}
		events = append(events, item)
	}
	windows := make([]any, 0, len(p.Windows))
	for _, w := range p.Windows {
		origin := origins[anchor]
		dx, err := xconv.SubChecked(w.Region.X, origin.X, "$.frames.dx")
		if err != nil {
			return nil, err
		}
		dy, err := xconv.SubChecked(w.Region.Y, origin.Y, "$.frames.dy")
		if err != nil {
			return nil, err
		}
		windows = append(windows, map[string]any{
			"fid": w.ID, "no": w.Seq, "ms": msOrLossy(w.Tick, report),
			"bytes": w.MaxPacketBytes, "seed": w.ContentSeed, "anchor": anchor,
			"dx":     logicalOrLossy(dx, "$.frames."+w.ID+".dx", report),
			"dy":     logicalOrLossy(dy, "$.frames."+w.ID+".dy", report),
			"width":  logicalOrLossy(w.Region.W, "$.frames."+w.ID+".width", report),
			"height": logicalOrLossy(w.Region.H, "$.frames."+w.ID+".height", report),
		})
	}
	return map[string]any{
		"ver": "1.0", "sid": p.SessionID, "seed": p.Seed,
		"canvas": map[string]any{
			"left": p.Master.X / model.InternalCoordUnit, "top": p.Master.Y / model.InternalCoordUnit,
			"width": p.Master.W / model.InternalCoordUnit, "height": p.Master.H / model.InternalCoordUnit,
		},
		"panels": screens, "events": events, "frames": windows,
	}, nil
}

func encodeV1Screen(s model.Screen, path string, report *model.ConvertReport) map[string]any {
	scaleCode := downgradeV1Scale(s.Scale, path+".zoom", report)
	chromaCode := downgradeV1Chroma(s.Chroma, path+".color", report)
	scaleModes := []any{}
	for i, tier := range s.SupportedScales {
		code, ok := v1ScaleCode(tier)
		if !ok {
			code = downgradeV1Scale(tier, fmt.Sprintf("%s.zoom_modes[%d]", path, i), report)
		}
		scaleModes = appendUniqueInt(scaleModes, code)
	}
	colorModes := []any{}
	for i, c := range s.SupportedChromas {
		code, ok := v1ChromaCode(c)
		if !ok {
			code = downgradeV1Chroma(c, fmt.Sprintf("%s.color_modes[%d]", path, i), report)
		}
		colorModes = appendUniqueInt(colorModes, code)
	}
	return map[string]any{
		"pid": s.ID, "rev": s.Revision,
		"ox": s.Origin.X / model.InternalCoordUnit, "oy": s.Origin.Y / model.InternalCoordUnit,
		"wpx": s.PixelW, "hpx": s.PixelH,
		"zoom": scaleCode, "zoom_modes": scaleModes,
		"color": chromaCode, "color_modes": colorModes,
	}
}

func appendUniqueInt(items []any, value int64) []any {
	for _, existing := range items {
		if existing == value {
			return items
		}
	}
	return append(items, value)
}

func downgradeV1Scale(tier model.ScaleTier, path string, report *model.ConvertReport) int64 {
	if code, ok := v1ScaleCode(tier); ok {
		return code
	}
	replace := int64(1)
	switch tier {
	case model.Scale125, model.Scale150:
		replace = 1
	default:
		replace = 1
	}
	report.Lossy = append(report.Lossy, model.LossyItem{
		Path: path, Value: string(tier), Code: model.ELossy,
		Disposition: "clamp_to_highest_v1_scale_not_above_original",
		Replacement: fmt.Sprintf("%d(%s)", replace, model.Scale100),
	})
	return replace
}

func downgradeV1Chroma(chroma model.Chroma, path string, report *model.ConvertReport) int64 {
	if code, ok := v1ChromaCode(chroma); ok {
		return code
	}
	report.Lossy = append(report.Lossy, model.LossyItem{
		Path: path, Value: string(chroma), Code: model.ELossy,
		Disposition: "clamp_down_to_v1_4:2:0", Replacement: "0(4:2:0)",
	})
	return 0
}
