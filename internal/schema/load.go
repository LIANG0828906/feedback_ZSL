package schema

import (
	"fmt"

	"screenrelay/internal/model"
	"screenrelay/internal/xconv"
)

func Load(data []byte) (*model.Project, *model.ConvertReport, error) {
	root, err := decodeObject(data)
	if err != nil {
		return nil, nil, err
	}
	major, source, versionPath, err := detectMajor(root)
	if err != nil {
		return nil, nil, err
	}
	report := &model.ConvertReport{SourceVersion: source}

	var project *model.Project
	switch major {
	case 1:
		project, err = loadV1(root, report)
	case 2:
		project, err = loadV2(root)
	case 3:
		project, err = loadV3(root)
	default:
		err = model.SchemaError(versionPath, "unsupported schema major %d", major)
	}
	if err != nil {
		return nil, report, err
	}
	project.SchemaMajor = major
	project.SourceVersion = source
	if project.CoordBasis.InternalUnitsPerLogical == 0 {
		project.CoordBasis = model.CoordBasis{
			InternalUnitsPerLogical: model.InternalCoordUnit,
			Expression:              "integer logical units multiplied by 256",
		}
	}
	if project.TimeBases == nil {
		project.TimeBases = map[string]model.TimeUnit{}
	}
	return project, report, nil
}

func loadV1(root map[string]any, report *model.ConvertReport) (*model.Project, error) {
	p := &model.Project{TimeBases: map[string]model.TimeUnit{
		"ms": {SourceName: "millisecond", SourceTicksPerSec: 1000, InternalPerSource: model.TicksPerSecond / 1000},
	}}
	if v, path, err := field(root, "sid", "$"); err != nil {
		return nil, err
	} else if p.SessionID, err = asString(v, path); err != nil {
		return nil, err
	}
	if v, ok := optionalField(root, "seed"); ok {
		n, err := asInt(v, "$.seed")
		if err != nil {
			return nil, err
		}
		p.Seed = n
	}
	if v, _, err := field(root, "canvas", "$"); err == nil {
		obj, err := asObject(v, "$.canvas")
		if err != nil {
			return nil, err
		}
		if p.Master, err = logicalRect(obj, []string{"left", "x"}, []string{"top", "y"}, []string{"width", "w"}, []string{"height", "h"}, "$.canvas"); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}

	panelsV, panelsPath, err := field(root, "panels", "$")
	if err != nil {
		return nil, err
	}
	panelItems, err := asSlice(panelsV, panelsPath)
	if err != nil {
		return nil, err
	}
	origins := map[string]model.Point{}
	for i, item := range panelItems {
		path := fmt.Sprintf("$.panels[%d]", i)
		obj, err := asObject(item, path)
		if err != nil {
			return nil, err
		}
		screen, err := parseV1Screen(obj, path, report)
		if err != nil {
			return nil, err
		}
		p.Screens = append(p.Screens, screen)
		origins[screen.ID] = screen.Origin
	}

	if rawEvents, ok := optionalField(root, "events"); ok {
		items, err := asSlice(rawEvents, "$.events")
		if err != nil {
			return nil, err
		}
		for i, item := range items {
			path := fmt.Sprintf("$.events[%d]", i)
			obj, err := asObject(item, path)
			if err != nil {
				return nil, err
			}
			event, err := parseV1Event(obj, path, i, report)
			if err != nil {
				return nil, err
			}
			p.Events = append(p.Events, event)
		}
	}

	framesV, framesPath, err := field(root, "frames", "$")
	if err != nil {
		return nil, err
	}
	frameItems, err := asSlice(framesV, framesPath)
	if err != nil {
		return nil, err
	}
	for i, item := range frameItems {
		path := fmt.Sprintf("$.frames[%d]", i)
		obj, err := asObject(item, path)
		if err != nil {
			return nil, err
		}
		window, err := parseV1Window(obj, path, i, origins)
		if err != nil {
			return nil, err
		}
		p.Windows = append(p.Windows, window)
	}
	return p, nil
}

func parseV1Screen(obj map[string]any, path string, report *model.ConvertReport) (model.Screen, error) {
	s := model.Screen{}
	v, p, err := field(obj, "pid", path)
	if err != nil {
		return s, err
	}
	if s.ID, err = asString(v, p); err != nil {
		return s, err
	}
	if v, ok := optionalField(obj, "rev"); ok {
		if s.Revision, err = asInt(v, path+".rev"); err != nil {
			return s, err
		}
	} else {
		s.Revision = 1
	}
	s.Origin, err = logicalPoint(obj, "ox", "oy", path)
	if err != nil {
		return s, err
	}
	if v, p, err = field(obj, "wpx", path); err != nil {
		return s, err
	} else if s.PixelW, err = asInt(v, p); err != nil {
		return s, err
	}
	if v, p, err = field(obj, "hpx", path); err != nil {
		return s, err
	} else if s.PixelH, err = asInt(v, p); err != nil {
		return s, err
	}

	scalePath := path + ".zoom"
	if v, ok := optionalField(obj, "zoom"); ok {
		if s.Scale, err = parseV1Scale(v, scalePath); err != nil {
			return s, err
		}
	} else {
		s.Scale = model.Scale100
		report.Fills = append(report.Fills, model.DefaultFill{
			Path: scalePath, OldValue: "<missing>", DefaultValue: "1(100)",
			Reason: "v1 legacy panel declares no zoom; schema default is code 1",
		})
	}
	if v, p, err := field(obj, "zoom_modes", path); err == nil {
		if s.SupportedScales, err = asEnumList(v, p, parseV1Scale); err != nil {
			return s, err
		}
	} else {
		return s, err
	}

	chromaPath := path + ".color"
	if v, ok := optionalField(obj, "color"); ok {
		if s.Chroma, err = parseV1Chroma(v, chromaPath); err != nil {
			return s, err
		}
	} else {
		s.Chroma = model.Chroma420
		report.Fills = append(report.Fills, model.DefaultFill{
			Path: chromaPath, OldValue: "<missing>", DefaultValue: "0(4:2:0)",
			Reason: "v1 legacy panel declares no color sampling; schema default is code 0",
		})
	}
	if v, p, err := field(obj, "color_modes", path); err == nil {
		if s.SupportedChromas, err = asEnumList(v, p, parseV1Chroma); err != nil {
			return s, err
		}
	} else {
		return s, err
	}
	return s, nil
}

func parseV1Event(obj map[string]any, path string, index int, report *model.ConvertReport) (model.TopologyEvent, error) {
	e := model.TopologyEvent{Seq: index + 1}
	if v, p, err := field(obj, "eid", path); err != nil {
		return e, err
	} else if e.ID, err = asString(v, p); err != nil {
		return e, err
	}
	if v, p, err := field(obj, "type", path); err != nil {
		return e, err
	} else {
		kind, err := asString(v, p)
		if err != nil {
			return e, err
		}
		switch kind {
		case "in":
			e.Kind = model.EventJoin
		case "out":
			e.Kind = model.EventLeave
		case "layout":
			e.Kind = model.EventTopology
		default:
			return e, model.SchemaError(p, "unknown v1 event type %q", kind)
		}
	}
	if v, p, err := field(obj, "pid", path); err != nil {
		return e, err
	} else if e.ScreenID, err = asString(v, p); err != nil {
		return e, err
	}
	if v, p, err := field(obj, "ms", path); err != nil {
		return e, err
	} else {
		ms, err := asInt(v, p)
		if err != nil {
			return e, err
		}
		if e.Tick, err = xconv.MillisecondsToTick(ms, p); err != nil {
			return e, err
		}
	}
	if v, ok := optionalField(obj, "rev"); ok {
		n, err := asInt(v, path+".rev")
		if err != nil {
			return e, err
		}
		e.Revision = n
	}
	if e.Kind == model.EventJoin || e.Kind == model.EventTopology {
		v, p, err := field(obj, "panel", path)
		if err != nil {
			return e, err
		}
		panelObj, err := asObject(v, p)
		if err != nil {
			return e, err
		}
		screen, err := parseV1Screen(panelObj, p, report)
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

func parseV1Window(obj map[string]any, path string, index int, origins map[string]model.Point) (model.WindowInstruction, error) {
	w := model.WindowInstruction{Seq: index + 1}
	if v, p, err := field(obj, "fid", path); err != nil {
		return w, err
	} else if w.ID, err = asString(v, p); err != nil {
		return w, err
	}
	if v, ok := optionalField(obj, "no"); ok {
		n, err := asInt(v, path+".no")
		if err != nil {
			return w, err
		}
		w.Seq = int(n)
	}
	if v, p, err := field(obj, "ms", path); err != nil {
		return w, err
	} else {
		ms, err := asInt(v, p)
		if err != nil {
			return w, err
		}
		if w.Tick, err = xconv.MillisecondsToTick(ms, p); err != nil {
			return w, err
		}
	}
	bytesV, bytesPath, err := field(obj, "bytes", path)
	if err != nil {
		return w, err
	}
	if w.MaxPacketBytes, err = asInt(bytesV, bytesPath); err != nil {
		return w, err
	}
	if w.MaxPacketBytes < model.PixelBytes {
		return w, model.SchemaError(bytesPath, "bytes must be at least %d", model.PixelBytes)
	}
	if v, ok := optionalField(obj, "seed"); ok {
		seed, seedErr := asInt(v, path+".seed")
		if seedErr != nil {
			return w, seedErr
		}
		w.ContentSeed = seed
	}

	anchorV, anchorPath, err := field(obj, "anchor", path)
	if err != nil {
		return w, err
	}
	anchor, err := asString(anchorV, anchorPath)
	if err != nil {
		return w, err
	}
	origin, ok := origins[anchor]
	if !ok {
		return w, model.SchemaError(anchorPath, "unknown anchor panel %q", anchor)
	}
	dx, err := logicalPoint(obj, "dx", "dy", path)
	if err != nil {
		return w, err
	}
	abs, err := xconv.OffsetToAbsolute(dx, origin, path+".offset")
	if err != nil {
		return w, err
	}
	widthV, widthPath, err := field(obj, "width", path)
	if err != nil {
		return w, err
	}
	heightV, heightPath, err := field(obj, "height", path)
	if err != nil {
		return w, err
	}
	widthLogical, err := asInt(widthV, widthPath)
	if err != nil {
		return w, err
	}
	heightLogical, err := asInt(heightV, heightPath)
	if err != nil {
		return w, err
	}
	width, err := logicalToCoord(widthLogical, widthPath)
	if err != nil {
		return w, err
	}
	height, err := logicalToCoord(heightLogical, heightPath)
	if err != nil {
		return w, err
	}
	w.Region = model.Rect{X: abs.X, Y: abs.Y, W: width, H: height}
	return w, nil
}
