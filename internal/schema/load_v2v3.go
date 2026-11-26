package schema

import (
	"fmt"

	"screenrelay/internal/model"
)

func loadV2(root map[string]any) (*model.Project, error) {
	var err error
	p := &model.Project{TimeBases: map[string]model.TimeUnit{
		"tick60": {SourceName: "60hz-frame-tick", SourceTicksPerSec: 60, InternalPerSource: model.TicksPerSecond / 60},
	}}
	if v, path, err := field(root, "sessionId", "$"); err != nil {
		return nil, err
	} else if p.SessionID, err = asString(v, path); err != nil {
		return nil, err
	}
	if v, ok := optionalField(root, "rngSeed"); ok {
		if p.Seed, err = asInt(v, "$.rngSeed"); err != nil {
			return nil, err
		}
	}
	if v, _, err := field(root, "master", "$"); err != nil {
		return nil, err
	} else {
		obj, err := asObject(v, "$.master")
		if err != nil {
			return nil, err
		}
		if p.Master, err = logicalRect(obj, []string{"x", "absX"}, []string{"y", "absY"}, []string{"w", "width"}, []string{"h", "height"}, "$.master"); err != nil {
			return nil, err
		}
	}

	items, err := requiredSlice(root, "displays", "$")
	if err != nil {
		return nil, err
	}
	for i, item := range items {
		path := fmt.Sprintf("$.displays[%d]", i)
		obj, err := asObject(item, path)
		if err != nil {
			return nil, err
		}
		screen, err := parseV2Screen(obj, path)
		if err != nil {
			return nil, err
		}
		p.Screens = append(p.Screens, screen)
	}

	if raw, ok := optionalField(root, "changes"); ok {
		items, err := asSlice(raw, "$.changes")
		if err != nil {
			return nil, err
		}
		for i, item := range items {
			path := fmt.Sprintf("$.changes[%d]", i)
			obj, err := asObject(item, path)
			if err != nil {
				return nil, err
			}
			event, err := parseV2Event(obj, path, i)
			if err != nil {
				return nil, err
			}
			p.Events = append(p.Events, event)
		}
	}

	items, err = requiredSlice(root, "windows", "$")
	if err != nil {
		return nil, err
	}
	for i, item := range items {
		path := fmt.Sprintf("$.windows[%d]", i)
		obj, err := asObject(item, path)
		if err != nil {
			return nil, err
		}
		window, err := parseV2Window(obj, path, i)
		if err != nil {
			return nil, err
		}
		p.Windows = append(p.Windows, window)
	}
	return p, nil
}

func requiredSlice(obj map[string]any, key, parentPath string) ([]any, error) {
	v, path, err := field(obj, key, parentPath)
	if err != nil {
		return nil, err
	}
	return asSlice(v, path)
}

func parseV2Screen(obj map[string]any, path string) (model.Screen, error) {
	s := model.Screen{}
	v, p, err := field(obj, "displayId", path)
	if err != nil {
		return s, err
	}
	if s.ID, err = asString(v, p); err != nil {
		return s, err
	}
	if v, ok := optionalField(obj, "revision"); ok {
		if s.Revision, err = asInt(v, path+".revision"); err != nil {
			return s, err
		}
	} else {
		s.Revision = 1
	}
	ox, err := intField(obj, "originX", path)
	if err != nil {
		return s, err
	}
	oy, err := intField(obj, "originY", path)
	if err != nil {
		return s, err
	}
	if s.Origin.X, err = logicalToCoord(ox, path+".originX"); err != nil {
		return s, err
	}
	if s.Origin.Y, err = logicalToCoord(oy, path+".originY"); err != nil {
		return s, err
	}
	if s.PixelW, err = intField(obj, "pixelWidth", path); err != nil {
		return s, err
	}
	if s.PixelH, err = intField(obj, "pixelHeight", path); err != nil {
		return s, err
	}
	if v, p, err = field(obj, "scale", path); err != nil {
		return s, err
	} else if s.Scale, err = parseV2Scale(v, p); err != nil {
		return s, err
	}
	if v, p, err = field(obj, "scaleCaps", path); err != nil {
		return s, err
	} else if s.SupportedScales, err = asEnumList(v, p, parseV2Scale); err != nil {
		return s, err
	}
	if v, p, err = field(obj, "chroma", path); err != nil {
		return s, err
	} else if s.Chroma, err = parseV2Chroma(v, p); err != nil {
		return s, err
	}
	if v, p, err = field(obj, "chromaCaps", path); err != nil {
		return s, err
	} else if s.SupportedChromas, err = asEnumList(v, p, parseV2Chroma); err != nil {
		return s, err
	}
	return s, nil
}

func intField(obj map[string]any, key, path string) (int64, error) {
	v, p, err := field(obj, key, path)
	if err != nil {
		return 0, err
	}
	return asInt(v, p)
}

func parseV2Event(obj map[string]any, path string, index int) (model.TopologyEvent, error) {
	e := model.TopologyEvent{Seq: index + 1}
	var err error
	if e.ID, err = stringField(obj, "id", path); err != nil {
		return e, err
	}
	kind, err := stringField(obj, "kind", path)
	if err != nil {
		return e, err
	}
	switch kind {
	case "joined":
		e.Kind = model.EventJoin
	case "left":
		e.Kind = model.EventLeave
	case "layoutChanged":
		e.Kind = model.EventTopology
	default:
		return e, model.SchemaError(path+".kind", "unknown v2 kind %q", kind)
	}
	if e.ScreenID, err = stringField(obj, "displayId", path); err != nil {
		return e, err
	}
	tick60, err := intField(obj, "atTick60", path)
	if err != nil {
		return e, err
	}
	e.Tick, err = mulCheckedLocal(tick60, model.TicksPerSecond/60, path+".atTick60")
	if err != nil {
		return e, err
	}
	if e.Revision, err = intField(obj, "revision", path); err != nil {
		if !model.HasCode(err, model.ESchema) {
			return e, err
		}
		e.Revision = 0
	}
	if e.Kind != model.EventLeave {
		v, p, err := field(obj, "display", path)
		if err != nil {
			return e, err
		}
		dobj, err := asObject(v, p)
		if err != nil {
			return e, err
		}
		screen, err := parseV2Screen(dobj, p)
		if err != nil {
			return e, err
		}
		e.Screen = &screen
		if e.Revision == 0 {
			e.Revision = screen.Revision
		}
	}
	return e, nil
}

func stringField(obj map[string]any, key, path string) (string, error) {
	v, p, err := field(obj, key, path)
	if err != nil {
		return "", err
	}
	return asString(v, p)
}

func parseV2Window(obj map[string]any, path string, index int) (model.WindowInstruction, error) {
	w := model.WindowInstruction{Seq: index + 1}
	var err error
	if w.ID, err = stringField(obj, "windowId", path); err != nil {
		return w, err
	}
	if seq, err := intField(obj, "sequence", path); err == nil {
		w.Seq = int(seq)
	} else {
		return w, err
	}
	tick60, err := intField(obj, "atTick60", path)
	if err != nil {
		return w, err
	}
	if w.Tick, err = mulCheckedLocal(tick60, model.TicksPerSecond/60, path+".atTick60"); err != nil {
		return w, err
	}
	if w.MaxPacketBytes, err = intField(obj, "maxPacketBytes", path); err != nil {
		return w, err
	}
	if w.ContentSeed, err = intField(obj, "contentSeed", path); err != nil {
		return w, err
	}
	rv, rp, err := field(obj, "region", path)
	if err != nil {
		return w, err
	}
	robj, err := asObject(rv, rp)
	if err != nil {
		return w, err
	}
	if w.Region, err = logicalRect(robj, []string{"absX", "x"}, []string{"absY", "y"}, []string{"width", "w"}, []string{"height", "h"}, rp); err != nil {
		return w, err
	}
	return w, nil
}

func loadV3(root map[string]any) (*model.Project, error) {
	p := &model.Project{TimeBases: map[string]model.TimeUnit{
		"tick90k": {SourceName: "90khz-pts", SourceTicksPerSec: model.TicksPerSecond, InternalPerSource: 1},
	}}
	var err error
	if p.SessionID, err = stringField(root, "session_id", "$"); err != nil {
		return nil, err
	}
	if p.Seed, err = intField(root, "seed", "$"); err != nil {
		return nil, err
	}
	mv, mp, err := field(root, "master", "$")
	if err != nil {
		return nil, err
	}
	mobj, err := asObject(mv, mp)
	if err != nil {
		return nil, err
	}
	if p.Master, err = logicalRect(mobj, []string{"x"}, []string{"y"}, []string{"w", "width"}, []string{"h", "height"}, mp); err != nil {
		return nil, err
	}

	items, err := requiredSlice(root, "screens", "$")
	if err != nil {
		return nil, err
	}
	for i, item := range items {
		path := fmt.Sprintf("$.screens[%d]", i)
		obj, err := asObject(item, path)
		if err != nil {
			return nil, err
		}
		screen, err := parseV3Screen(obj, path)
		if err != nil {
			return nil, err
		}
		p.Screens = append(p.Screens, screen)
	}

	if raw, ok := optionalField(root, "events"); ok {
		items, err := asSlice(raw, "$.events")
		if err != nil {
			return nil, err
		}
		for i, item := range items {
			path := fmt.Sprintf("$.events[%d]", i)
			obj, err := asObject(item, path)
			if err != nil {
				return nil, err
			}
			event, err := parseV3Event(obj, path, i)
			if err != nil {
				return nil, err
			}
			p.Events = append(p.Events, event)
		}
	}

	items, err = requiredSlice(root, "windows", "$")
	if err != nil {
		return nil, err
	}
	for i, item := range items {
		path := fmt.Sprintf("$.windows[%d]", i)
		obj, err := asObject(item, path)
		if err != nil {
			return nil, err
		}
		window, err := parseV3Window(obj, path, i)
		if err != nil {
			return nil, err
		}
		p.Windows = append(p.Windows, window)
	}
	return p, nil
}

func parseV3Screen(obj map[string]any, path string) (model.Screen, error) {
	s := model.Screen{}
	var err error
	if s.ID, err = stringField(obj, "id", path); err != nil {
		return s, err
	}
	if s.Revision, err = intField(obj, "revision", path); err != nil {
		return s, err
	}
	op, opp, err := field(obj, "origin", path)
	if err != nil {
		return s, err
	}
	oobj, err := asObject(op, opp)
	if err != nil {
		return s, err
	}
	if s.Origin, err = logicalPoint(oobj, "x", "y", opp); err != nil {
		return s, err
	}
	rp, rpp, err := field(obj, "resolution", path)
	if err != nil {
		return s, err
	}
	robj, err := asObject(rp, rpp)
	if err != nil {
		return s, err
	}
	if s.PixelW, err = intField(robj, "width", rpp); err != nil {
		return s, err
	}
	if s.PixelH, err = intField(robj, "height", rpp); err != nil {
		return s, err
	}
	if v, p, err := field(obj, "scale_tier", path); err != nil {
		return s, err
	} else if s.Scale, err = parseV3Scale(v, p); err != nil {
		return s, err
	}
	if v, p, err := field(obj, "scale_capabilities", path); err != nil {
		return s, err
	} else if s.SupportedScales, err = asEnumList(v, p, parseV3Scale); err != nil {
		return s, err
	}
	if v, p, err := field(obj, "chroma", path); err != nil {
		return s, err
	} else if s.Chroma, err = parseV3Chroma(v, p); err != nil {
		return s, err
	}
	if v, p, err := field(obj, "chroma_capabilities", path); err != nil {
		return s, err
	} else if s.SupportedChromas, err = asEnumList(v, p, parseV3Chroma); err != nil {
		return s, err
	}
	return s, nil
}

func parseV3Event(obj map[string]any, path string, index int) (model.TopologyEvent, error) {
	e := model.TopologyEvent{Seq: index + 1}
	var err error
	if e.ID, err = stringField(obj, "id", path); err != nil {
		return e, err
	}
	kind, err := stringField(obj, "type", path)
	if err != nil {
		return e, err
	}
	switch model.EventKind(kind) {
	case model.EventJoin, model.EventLeave, model.EventTopology:
		e.Kind = model.EventKind(kind)
	default:
		return e, model.SchemaError(path+".type", "unknown event type %q", kind)
	}
	if e.ScreenID, err = stringField(obj, "screen_id", path); err != nil {
		return e, err
	}
	if e.Tick, err = intField(obj, "tick90k", path); err != nil {
		return e, err
	}
	if e.Revision, err = intField(obj, "revision", path); err != nil {
		if !model.HasCode(err, model.ESchema) {
			return e, err
		}
		e.Revision = 0
	}
	if e.Kind != model.EventLeave {
		v, p, err := field(obj, "screen", path)
		if err != nil {
			return e, err
		}
		sobj, err := asObject(v, p)
		if err != nil {
			return e, err
		}
		screen, err := parseV3Screen(sobj, p)
		if err != nil {
			return e, err
		}
		e.Screen = &screen
		if e.Revision == 0 {
			e.Revision = screen.Revision
		}
	}
	return e, nil
}

func parseV3Window(obj map[string]any, path string, index int) (model.WindowInstruction, error) {
	w := model.WindowInstruction{Seq: index + 1}
	var err error
	if w.ID, err = stringField(obj, "id", path); err != nil {
		return w, err
	}
	seq, err := intField(obj, "seq", path)
	if err != nil {
		return w, err
	}
	w.Seq = int(seq)
	if w.Tick, err = intField(obj, "tick90k", path); err != nil {
		return w, err
	}
	if w.MaxPacketBytes, err = intField(obj, "max_packet_bytes", path); err != nil {
		return w, err
	}
	if w.ContentSeed, err = intField(obj, "content_seed", path); err != nil {
		return w, err
	}
	rv, rp, err := field(obj, "region", path)
	if err != nil {
		return w, err
	}
	robj, err := asObject(rv, rp)
	if err != nil {
		return w, err
	}
	if w.Region, err = logicalRect(robj, []string{"x"}, []string{"y"}, []string{"w", "width"}, []string{"h", "height"}, rp); err != nil {
		return w, err
	}
	return w, nil
}

func mulCheckedLocal(a, b int64, path string) (int64, error) {
	// The values in generated samples are small; this wrapper keeps field paths local.
	if a > (1<<62)/b || a < -(1<<62)/b {
		return 0, model.OvfError(path, a, b)
	}
	return a * b, nil
}
