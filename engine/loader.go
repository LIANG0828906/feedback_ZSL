package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// LoadScene reads one of the three supported schema versions into the internal model.
func LoadScene(data []byte) (*Scene, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "invalid JSON: " + err.Error()}
	}
	version, verPath, err := detectVersion(raw)
	if err != nil {
		return nil, err
	}
	var scene *Scene
	switch version {
	case Version1:
		scene, err = parseV1(raw)
	case Version2:
		scene, err = parseV2(raw)
	case Version3:
		scene, err = parseV3(raw)
	default:
		return nil, &Error{Code: CodeVer, Path: verPath, Message: "unknown schema version"}
	}
	if err != nil {
		return nil, err
	}
	scene.SourceVersion = version
	StandardDefaults().Apply(scene)
	if err := ValidateScene(scene); err != nil {
		return nil, err
	}
	return scene, nil
}

func LoadSceneFile(path string) (*Scene, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scene: %w", err)
	}
	return LoadScene(b)
}

func detectVersion(raw map[string]any) (Version, string, error) {
	for _, key := range []string{"schemaVersion", "schema_version", "version"} {
		if v, ok := raw[key]; ok {
			ver, ok := normalizedVersion(v)
			if !ok {
				return "", key, &Error{Code: CodeVer, Path: key, Message: fmt.Sprintf("unsupported version %v", v)}
			}
			return ver, key, nil
		}
	}
	return "", "schemaVersion", &Error{Code: CodeSchema, Path: "schemaVersion", Message: "required version field is missing"}
}

func asMap(v any, path string) (map[string]any, *Error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &Error{Code: CodeSchema, Path: path, Message: "expected object"}
	}
	return m, nil
}

func asSlice(v any, path string) ([]any, *Error) {
	a, ok := v.([]any)
	if !ok {
		return nil, &Error{Code: CodeSchema, Path: path, Message: "expected array"}
	}
	return a, nil
}

func field(m map[string]any, key, path string) (any, *Error) {
	v, ok := m[key]
	if !ok {
		return nil, &Error{Code: CodeSchema, Path: joinPath(path, key), Message: "required field is missing"}
	}
	return v, nil
}

func optional(m map[string]any, keys ...string) (any, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v, true
		}
	}
	return nil, false
}

func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	if strings.HasPrefix(key, "[") {
		return base + key
	}
	return base + "." + key
}

func idxPath(base string, i int) string {
	return fmt.Sprintf("%s[%d]", base, i)
}

func asString(v any, path string) (string, *Error) {
	s, ok := v.(string)
	if !ok {
		return "", &Error{Code: CodeSchema, Path: path, Message: "expected string"}
	}
	return s, nil
}

func asBool(v any, path string) (bool, *Error) {
	b, ok := v.(bool)
	if !ok {
		return false, &Error{Code: CodeSchema, Path: path, Message: "expected boolean"}
	}
	return b, nil
}

func asInt(v any, path string) (int64, *Error) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, &Error{Code: CodeSchema, Path: path, Message: "expected integer"}
		}
		return i, nil
	case float64:
		if n != float64(int64(n)) {
			return 0, &Error{Code: CodeSchema, Path: path, Message: "expected integer"}
		}
		return int64(n), nil
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	default:
		return 0, &Error{Code: CodeSchema, Path: path, Message: "expected integer"}
	}
}

func stringField(m map[string]any, key, path string) (string, *Error) {
	v, err := field(m, key, path)
	if err != nil {
		return "", err
	}
	return asString(v, joinPath(path, key))
}

func intField(m map[string]any, key, path string) (int64, *Error) {
	v, err := field(m, key, path)
	if err != nil {
		return 0, err
	}
	return asInt(v, joinPath(path, key))
}

func parsePoint(v any, path string, encoding string, base Vec2) (Vec2, *Error) {
	m, err := asMap(v, path)
	if err != nil {
		return Vec2{}, err
	}
	switch encoding {
	case "relative-delta":
		dx, e := intField(m, "dx", path)
		if e != nil {
			return Vec2{}, e
		}
		dy, e := intField(m, "dy", path)
		if e != nil {
			return Vec2{}, e
		}
		return base.Add(Vec2{X: dx, Y: dy}), nil
	default:
		var x, y int64
		var e *Error
		if _, ok := m["x"]; ok {
			x, e = intField(m, "x", path)
			if e != nil {
				return Vec2{}, e
			}
		} else if _, ok := m["cx"]; ok {
			x, e = intField(m, "cx", path)
			if e != nil {
				return Vec2{}, e
			}
		} else {
			return Vec2{}, &Error{Code: CodeSchema, Path: joinPath(path, "x"), Message: "required field is missing"}
		}
		if _, ok := m["y"]; ok {
			y, e = intField(m, "y", path)
			if e != nil {
				return Vec2{}, e
			}
		} else if _, ok := m["cy"]; ok {
			y, e = intField(m, "cy", path)
			if e != nil {
				return Vec2{}, e
			}
		} else {
			return Vec2{}, &Error{Code: CodeSchema, Path: joinPath(path, "y"), Message: "required field is missing"}
		}
		return Vec2{X: x, Y: y}, nil
	}
}

func parseColor(v any, path, pathID string) (Color, *Error) {
	arr, ok := v.([]any)
	if !ok {
		// Version 3 may use an object form.
		if m, mok := v.(map[string]any); mok {
			var out Color
			keys := []struct {
				k string
				p *uint8
			}{{"r", &out.R}, {"g", &out.G}, {"b", &out.B}, {"a", &out.A}}
			for _, k := range keys {
				val, err := intField(m, k.k, path)
				if err != nil {
					return Color{}, err
				}
				if val < 0 || val > 255 {
					return Color{}, &Error{Code: CodeComp, Path: joinPath(path, k.k), Message: fmt.Sprintf("channel out of range for path %s", pathID)}
				}
				*k.p = uint8(val)
			}
			return out, nil
		}
		return Color{}, &Error{Code: CodeComp, Path: path, Message: fmt.Sprintf("invalid color channel representation for path %s", pathID)}
	}
	if len(arr) != 4 {
		return Color{}, &Error{Code: CodeComp, Path: path, Message: fmt.Sprintf("expected 4 channels for path %s, got %d", pathID, len(arr))}
	}
	var out Color
	ps := []*uint8{&out.R, &out.G, &out.B, &out.A}
	for i, v := range arr {
		n, err := asInt(v, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return Color{}, err
		}
		if n < 0 || n > 255 {
			return Color{}, &Error{Code: CodeComp, Path: fmt.Sprintf("%s[%d]", path, i), Message: fmt.Sprintf("channel out of range for path %s", pathID)}
		}
		*ps[i] = uint8(n)
	}
	return out, nil
}

func parseBlend(v any, path, pathID string, fallback BlendMode) (BlendMode, *Error) {
	if v == nil {
		return fallback, nil
	}
	s, err := asString(v, path)
	if err != nil {
		return "", err
	}
	b := BlendMode(s)
	if !validBlend(b) {
		return "", &Error{Code: CodeComp, Path: path, Message: fmt.Sprintf("invalid blend mode for path %s", pathID)}
	}
	return b, nil
}

func parseJoin(v any, path, pathID string, fallback JoinKind) (JoinKind, *Error) {
	if v == nil {
		return fallback, nil
	}
	s, err := asString(v, path)
	if err != nil {
		return "", err
	}
	j := JoinKind(s)
	if j != JoinRound && j != JoinBevel && j != JoinMiter {
		return "", &Error{Code: CodeComp, Path: path, Message: fmt.Sprintf("invalid line join for path %s", pathID)}
	}
	return j, nil
}

func parseSegmentsV1(v any, path string) ([]Segment, *Error) {
	arr, err := asSlice(v, path)
	if err != nil {
		return nil, err
	}
	if len(arr) == 0 {
		return nil, &Error{Code: CodeSelf, Path: path, Message: "contour has no segments"}
	}
	out := make([]Segment, 0, len(arr))
	for i, raw := range arr {
		p := idxPath(path, i)
		m, e := asMap(raw, p)
		if e != nil {
			return nil, e
		}
		kind, e := stringField(m, "curve", p)
		if e != nil {
			return nil, e
		}
		seg := Segment{Kind: normalizeKind(kind)}
		if seg.Kind != Quadratic && seg.Kind != Cubic {
			return nil, &Error{Code: CodeSelf, Path: joinPath(p, "curve"), Message: "unsupported curve degree"}
		}
		if seg.Start, e = parsePointField(m, "start", p, "absolute", Vec2{}); e != nil {
			return nil, e
		}
		if seg.C1, e = parsePointField(m, "c1", p, "absolute", Vec2{}); e != nil {
			return nil, e
		}
		if seg.Kind == Cubic {
			if seg.C2, e = parsePointField(m, "c2", p, "absolute", Vec2{}); e != nil {
				return nil, e
			}
		}
		if seg.End, e = parsePointField(m, "end", p, "absolute", Vec2{}); e != nil {
			return nil, e
		}
		out = append(out, seg)
	}
	return out, nil
}

func parsePointField(m map[string]any, key, base, encoding string, origin Vec2) (Vec2, *Error) {
	v, err := field(m, key, base)
	if err != nil {
		return Vec2{}, err
	}
	return parsePoint(v, joinPath(base, key), encoding, origin)
}

func parseSegmentsV2(v any, path string) ([]Segment, *Error) {
	arr, err := asSlice(v, path)
	if err != nil {
		return nil, err
	}
	if len(arr) == 0 {
		return nil, &Error{Code: CodeSelf, Path: path, Message: "contour has no segments"}
	}
	out := make([]Segment, 0, len(arr))
	for i, raw := range arr {
		p := idxPath(path, i)
		m, e := asMap(raw, p)
		if e != nil {
			return nil, e
		}
		degree, e := intField(m, "degree", p)
		if e != nil {
			return nil, e
		}
		seg := Segment{Kind: Quadratic}
		if degree == 3 {
			seg.Kind = Cubic
		} else if degree != 2 {
			return nil, &Error{Code: CodeSelf, Path: joinPath(p, "degree"), Message: "unsupported curve degree"}
		}
		if seg.Start, e = parsePointField(m, "p0", p, "absolute", Vec2{}); e != nil {
			return nil, e
		}
		if seg.C1, e = parsePointField(m, "p1", p, "absolute", Vec2{}); e != nil {
			return nil, e
		}
		if seg.Kind == Cubic {
			if seg.C2, e = parsePointField(m, "p2", p, "absolute", Vec2{}); e != nil {
				return nil, e
			}
		}
		if seg.End, e = parsePointField(m, "p3", p, "absolute", Vec2{}); e != nil {
			return nil, e
		}
		out = append(out, seg)
	}
	return out, nil
}

func parseSegmentsV3(v any, base Vec2, path string) ([]Segment, *Error) {
	arr, err := asSlice(v, path)
	if err != nil {
		return nil, err
	}
	if len(arr) == 0 {
		return nil, &Error{Code: CodeSelf, Path: path, Message: "contour has no segments"}
	}
	out := make([]Segment, 0, len(arr))
	for i, raw := range arr {
		p := idxPath(path, i)
		m, e := asMap(raw, p)
		if e != nil {
			return nil, e
		}
		kind, e := stringField(m, "kind", p)
		if e != nil {
			return nil, e
		}
		seg := Segment{Kind: normalizeKind(kind)}
		if seg.Kind != Quadratic && seg.Kind != Cubic {
			return nil, &Error{Code: CodeSelf, Path: joinPath(p, "kind"), Message: "unsupported curve kind"}
		}
		if seg.Start, e = parsePointField(m, "p0", p, "relative-delta", base); e != nil {
			return nil, e
		}
		if seg.C1, e = parsePointField(m, "p1", p, "relative-delta", base); e != nil {
			return nil, e
		}
		if seg.Kind == Cubic {
			if seg.C2, e = parsePointField(m, "p2", p, "relative-delta", base); e != nil {
				return nil, e
			}
		}
		if seg.End, e = parsePointField(m, "p3", p, "relative-delta", base); e != nil {
			return nil, e
		}
		out = append(out, seg)
	}
	return out, nil
}

func normalizeKind(s string) SegmentKind {
	switch strings.ToLower(s) {
	case "quad", "quadratic", "q":
		return Quadratic
	case "cubic", "bezier3", "c":
		return Cubic
	default:
		return SegmentKind(strings.ToLower(s))
	}
}

func validBlend(b BlendMode) bool {
	switch b {
	case BlendSourceOver, BlendMultiply, BlendScreen, BlendReplace:
		return true
	default:
		return false
	}
}

func parseDefaultsV1(raw map[string]any) (InterpOptions, int64, int, int64, *Error) {
	interp := InterpOptions{Mode: "linear", PrecisionBits: 12}
	tol, grid, scale := int64(64), 4, CoordScale
	v, ok := raw["defaults"]
	if !ok {
		return interp, tol, grid, scale, nil
	}
	m, err := asMap(v, "defaults")
	if err != nil {
		return interp, 0, 0, 0, err
	}
	if x, ok := optional(m, "interpolation"); ok {
		if s, ok := x.(string); ok {
			interp.Mode = s
		} else if im, e := asMap(x, "defaults.interpolation"); e == nil {
			if mode, ok := optional(im, "mode"); ok {
				interp.Mode, _ = asString(mode, "defaults.interpolation.mode")
			}
			if bits, ok := optional(im, "precision", "precisionBits"); ok {
				interp.PrecisionBits = int(mustInt(bits))
			}
		}
	}
	if x, ok := optional(m, "interpolationPrecision"); ok {
		interp.PrecisionBits = int(mustInt(x))
	}
	if x, ok := optional(m, "toleranceQ10"); ok {
		tol = mustInt(x)
	}
	if x, ok := optional(m, "subsamples"); ok {
		grid = int(mustInt(x))
	}
	if x, ok := optional(m, "scaleQ10"); ok {
		scale = mustInt(x)
	}
	return interp, tol, grid, scale, nil
}

func mustInt(v any) int64 {
	i, _ := asInt(v, "value")
	return i
}

func parseDefaultsV2(raw map[string]any) (InterpOptions, int64, int, int64, *Error) {
	interp := InterpOptions{Mode: "linear", PrecisionBits: 12}
	tol, grid, scale := int64(64), 4, CoordScale
	v, ok := raw["defaults"]
	if !ok {
		return interp, tol, grid, scale, nil
	}
	m, err := asMap(v, "defaults")
	if err != nil {
		return interp, 0, 0, 0, err
	}
	if x, ok := optional(m, "interpolation"); ok {
		if im, e := asMap(x, "defaults.interpolation"); e == nil {
			if mode, ok := optional(im, "mode"); ok {
				interp.Mode, _ = asString(mode, "defaults.interpolation.mode")
			}
			if bits, ok := optional(im, "precision"); ok {
				interp.PrecisionBits = int(mustInt(bits))
			}
		}
	}
	if x, ok := optional(m, "flattenTolerance", "toleranceQ10"); ok {
		tol = mustInt(x)
	}
	if x, ok := optional(m, "aaGrid", "subsamples"); ok {
		grid = int(mustInt(x))
	}
	if x, ok := optional(m, "renderScale", "scaleQ10"); ok {
		scale = mustInt(x)
	}
	return interp, tol, grid, scale, nil
}

func parseDefaultsV3(raw map[string]any) (InterpOptions, int64, int, int64, *Error) {
	interp := InterpOptions{Mode: "linear", PrecisionBits: 12}
	tol, grid, scale := int64(64), 4, CoordScale
	v, ok := raw["defaults"]
	if !ok {
		return interp, tol, grid, scale, nil
	}
	m, err := asMap(v, "defaults")
	if err != nil {
		return interp, 0, 0, 0, err
	}
	if x, ok := optional(m, "interp"); ok {
		if im, e := asMap(x, "defaults.interp"); e == nil {
			if mode, ok := optional(im, "mode"); ok {
				interp.Mode, _ = asString(mode, "defaults.interp.mode")
			}
			if bits, ok := optional(im, "precisionBits", "precision"); ok {
				interp.PrecisionBits = int(mustInt(bits))
			}
		}
	}
	if x, ok := optional(m, "flattenToleranceQ10", "toleranceQ10"); ok {
		tol = mustInt(x)
	}
	if x, ok := optional(m, "subsampleGrid", "subsamples"); ok {
		grid = int(mustInt(x))
	}
	if x, ok := optional(m, "pixelScaleQ10", "scaleQ10"); ok {
		scale = mustInt(x)
	}
	return interp, tol, grid, scale, nil
}

func parseV1(raw map[string]any) (*Scene, error) {
	if _, err := stringField(raw, "schemaVersion", ""); err != nil {
		return nil, err
	}
	scene := &Scene{}
	var err *Error
	if scene.ID, err = stringField(raw, "sceneId", ""); err != nil {
		return nil, err
	}
	clock, e := asMap(mustField(raw, "clock", ""), "clock")
	if e != nil {
		return nil, e
	}
	scene.TimeUnit, err = stringField(clock, "unit", "clock")
	if err != nil {
		return nil, err
	}
	scene.Rounding = "floor"
	if v, ok := optional(clock, "rounding"); ok {
		scene.Rounding, _ = asString(v, "clock.rounding")
	}
	scene.Encoding, err = stringField(raw, "coordinateEncoding", "")
	if err != nil {
		return nil, err
	}
	if scene.TimeUnit != "ms" {
		return nil, &Error{Code: CodeSchema, Path: "clock.unit", Message: "version 1 requires millisecond clock"}
	}
	if scene.Encoding != "absolute" && scene.Encoding != "absolute-q10" {
		return nil, &Error{Code: CodeSchema, Path: "coordinateEncoding", Message: "version 1 requires absolute coordinates"}
	}
	scene.Interpolation, scene.Tolerance, scene.Subsamples, scene.Scale, err = parseDefaultsV1(raw)
	if err != nil {
		return nil, err
	}
	pathsRaw, e2 := asSlice(mustField(raw, "paths", ""), "paths")
	if e2 != nil {
		return nil, e2
	}
	for i, pv := range pathsRaw {
		p, e3 := parsePathV1(pv, idxPath("paths", i))
		if e3 != nil {
			return nil, e3
		}
		scene.Paths = append(scene.Paths, p)
	}
	samplesRaw, e2 := asSlice(mustField(raw, "samplesMs", ""), "samplesMs")
	if e2 != nil {
		return nil, e2
	}
	for i, sv := range samplesRaw {
		ms, e3 := asInt(sv, idxPath("samplesMs", i))
		if e3 != nil {
			return nil, e3
		}
		scene.Samples = append(scene.Samples, millisToTicks(ms))
	}
	return scene, nil
}

func mustField(m map[string]any, key, path string) any {
	v, _ := field(m, key, path)
	return v
}

func parsePathV1(v any, path string) (Path, *Error) {
	m, err := asMap(v, path)
	if err != nil {
		return Path{}, err
	}
	p := Path{LineJoin: JoinRound, Blend: BlendSourceOver, FillRule: "nonzero", Opacity: 255}
	if p.ID, err = stringField(m, "id", path); err != nil {
		return p, err
	}
	if raw, ok := optional(m, "closed"); ok {
		closed, e := asBool(raw, joinPath(path, "closed"))
		if e != nil {
			return p, e
		}
		if !closed {
			return p, &Error{Code: CodeSelf, Path: joinPath(path, "closed"), Message: "contour must be closed"}
		}
	}
	paint := "fill"
	if raw, ok := optional(m, "paint", "flags"); ok {
		paint, _ = asString(raw, joinPath(path, "paint"))
	}
	p.HasFill = strings.Contains(paint, "fill")
	p.HasStroke = strings.Contains(paint, "stroke")
	if !p.HasFill && !p.HasStroke {
		return p, &Error{Code: CodeSchema, Path: joinPath(path, "paint"), Message: "path must enable fill or stroke"}
	}
	fill, e := field(m, "fillColor", path)
	if e != nil {
		return p, e
	}
	if p.Fill, e = parseColor(fill, joinPath(path, "fillColor"), p.ID); e != nil {
		return p, e
	}
	p.StrokeColor = p.Fill
	if raw, ok := optional(m, "strokeColor"); ok {
		if p.StrokeColor, e = parseColor(raw, joinPath(path, "strokeColor"), p.ID); e != nil {
			return p, e
		}
	}
	if p.HasStroke {
		width, e := intField(m, "strokeWidthQ10", path)
		if e != nil {
			return p, e
		}
		p.StrokeWidth = width
		if raw, ok := optional(m, "lineJoin"); ok {
			if p.LineJoin, e = parseJoin(raw, joinPath(path, "lineJoin"), p.ID, JoinRound); e != nil {
				return p, e
			}
		}
	}
	if raw, ok := optional(m, "blend"); ok {
		if p.Blend, e = parseBlend(raw, joinPath(path, "blend"), p.ID, BlendSourceOver); e != nil {
			return p, e
		}
		if p.Blend == BlendReplace {
			return p, &Error{Code: CodeComp, Path: joinPath(path, "blend"), Message: fmt.Sprintf("blend mode replace is not supported by version 1 for path %s", p.ID)}
		}
	}
	if raw, ok := optional(m, "fillRule"); ok {
		p.FillRule, _ = asString(raw, joinPath(path, "fillRule"))
	}
	if raw, ok := optional(m, "opacity"); ok {
		n, e := asInt(raw, joinPath(path, "opacity"))
		if e != nil || n < 0 || n > 255 {
			return p, &Error{Code: CodeComp, Path: joinPath(path, "opacity"), Message: fmt.Sprintf("invalid opacity for path %s", p.ID)}
		}
		p.Opacity = uint8(n)
	}
	keys, e := asSlice(mustField(m, "keyframes", path), joinPath(path, "keyframes"))
	if e != nil {
		return p, e
	}
	for i, kv := range keys {
		kp := idxPath(joinPath(path, "keyframes"), i)
		km, e := asMap(kv, kp)
		if e != nil {
			return p, e
		}
		ms, e := intField(km, "timeMs", kp)
		if e != nil {
			return p, e
		}
		segs, e := parseSegmentsV1(mustField(km, "segments", kp), joinPath(kp, "segments"))
		if e != nil {
			return p, e
		}
		p.Keyframes = append(p.Keyframes, Keyframe{Time: millisToTicks(ms), Segments: segs})
	}
	return p, nil
}

func parseV2(raw map[string]any) (*Scene, error) {
	if _, err := stringField(raw, "schema_version", ""); err != nil {
		return nil, err
	}
	scene := &Scene{}
	var err *Error
	if scene.ID, err = stringField(raw, "sceneId", ""); err != nil {
		return nil, err
	}
	clock, e := asMap(mustField(raw, "clock", ""), "clock")
	if e != nil {
		return nil, e
	}
	scene.TimeUnit, err = stringField(clock, "unit", "clock")
	if err != nil {
		return nil, err
	}
	scene.Rounding = "floor"
	if v, ok := optional(clock, "rounding"); ok {
		scene.Rounding, _ = asString(v, "clock.rounding")
	}
	scene.Encoding, err = stringField(raw, "controlPoints", "")
	if err != nil {
		return nil, err
	}
	if scene.TimeUnit != "frame" && scene.TimeUnit != "frames" {
		return nil, &Error{Code: CodeSchema, Path: "clock.unit", Message: "version 2 requires frame clock"}
	}
	if scene.Encoding != "absolute" {
		return nil, &Error{Code: CodeSchema, Path: "controlPoints", Message: "version 2 requires absolute control points"}
	}
	scene.Interpolation, scene.Tolerance, scene.Subsamples, scene.Scale, err = parseDefaultsV2(raw)
	if err != nil {
		return nil, err
	}
	pathsRaw, e2 := asSlice(mustField(raw, "paths", ""), "paths")
	if e2 != nil {
		return nil, e2
	}
	for i, pv := range pathsRaw {
		p, e3 := parsePathV2(pv, idxPath("paths", i))
		if e3 != nil {
			return nil, e3
		}
		scene.Paths = append(scene.Paths, p)
	}
	samplesRaw, e2 := asSlice(mustField(raw, "sampleTimes", ""), "sampleTimes")
	if e2 != nil {
		return nil, e2
	}
	for i, sv := range samplesRaw {
		frames, e3 := asInt(sv, idxPath("sampleTimes", i))
		if e3 != nil {
			return nil, e3
		}
		scene.Samples = append(scene.Samples, framesToTicks(frames))
	}
	return scene, nil
}

func parsePathV2(v any, path string) (Path, *Error) {
	m, err := asMap(v, path)
	if err != nil {
		return Path{}, err
	}
	p := Path{LineJoin: JoinRound, Blend: BlendSourceOver, FillRule: "nonzero", Opacity: 255}
	if p.ID, err = stringField(m, "path_id", path); err != nil {
		return p, err
	}
	if raw, ok := optional(m, "closed"); ok {
		closed, e := asBool(raw, joinPath(path, "closed"))
		if e != nil {
			return p, e
		}
		if !closed {
			return p, &Error{Code: CodeSelf, Path: joinPath(path, "closed"), Message: "contour must be closed"}
		}
	}
	paint, err := stringField(m, "paint", path)
	if err != nil {
		return p, err
	}
	p.HasFill = strings.Contains(paint, "fill")
	p.HasStroke = strings.Contains(paint, "stroke")
	if !p.HasFill && !p.HasStroke {
		return p, &Error{Code: CodeSchema, Path: joinPath(path, "paint"), Message: "path must enable fill or stroke"}
	}
	fill, e := field(m, "fill_color", path)
	if e != nil {
		return p, e
	}
	if p.Fill, e = parseColor(fill, joinPath(path, "fill_color"), p.ID); e != nil {
		return p, e
	}
	p.StrokeColor = p.Fill
	if raw, ok := optional(m, "stroke_color"); ok {
		if p.StrokeColor, e = parseColor(raw, joinPath(path, "stroke_color"), p.ID); e != nil {
			return p, e
		}
	}
	if p.HasStroke {
		width, e := intField(m, "stroke_width", path)
		if e != nil {
			return p, e
		}
		p.StrokeWidth = width
		if raw, ok := optional(m, "line_join"); ok {
			if p.LineJoin, e = parseJoin(raw, joinPath(path, "line_join"), p.ID, JoinRound); e != nil {
				return p, e
			}
		}
	}
	if raw, ok := optional(m, "composite", "blend"); ok {
		if p.Blend, e = parseBlend(raw, joinPath(path, "composite"), p.ID, BlendSourceOver); e != nil {
			return p, e
		}
		if p.Blend == BlendReplace {
			return p, &Error{Code: CodeComp, Path: joinPath(path, "composite"), Message: fmt.Sprintf("blend mode replace is not supported by version 2 for path %s", p.ID)}
		}
	}
	if raw, ok := optional(m, "fill_rule"); ok {
		p.FillRule, _ = asString(raw, joinPath(path, "fill_rule"))
	}
	if raw, ok := optional(m, "opacity"); ok {
		n, e := asInt(raw, joinPath(path, "opacity"))
		if e != nil || n < 0 || n > 255 {
			return p, &Error{Code: CodeComp, Path: joinPath(path, "opacity"), Message: fmt.Sprintf("invalid opacity for path %s", p.ID)}
		}
		p.Opacity = uint8(n)
	}
	keys, e := asSlice(mustField(m, "keys", path), joinPath(path, "keys"))
	if e != nil {
		return p, e
	}
	for i, kv := range keys {
		kp := idxPath(joinPath(path, "keys"), i)
		km, e := asMap(kv, kp)
		if e != nil {
			return p, e
		}
		frames, e := intField(km, "t", kp)
		if e != nil {
			return p, e
		}
		segs, e := parseSegmentsV2(mustField(km, "curves", kp), joinPath(kp, "curves"))
		if e != nil {
			return p, e
		}
		p.Keyframes = append(p.Keyframes, Keyframe{Time: framesToTicks(frames), Segments: segs})
	}
	return p, nil
}

func parseV3(raw map[string]any) (*Scene, error) {
	if _, err := asInt(mustField(raw, "version", ""), "version"); err != nil {
		return nil, err
	}
	scene := &Scene{}
	var err *Error
	if scene.ID, err = stringField(raw, "id", ""); err != nil {
		return nil, err
	}
	clock, e := asMap(mustField(raw, "clock", ""), "clock")
	if e != nil {
		return nil, e
	}
	scene.TimeUnit, err = stringField(clock, "unit", "clock")
	if err != nil {
		return nil, err
	}
	scene.Rounding = "floor"
	if v, ok := optional(clock, "round"); ok {
		scene.Rounding, _ = asString(v, "clock.round")
	}
	scene.Encoding, err = stringField(raw, "controlPoints", "")
	if err != nil {
		return nil, err
	}
	if scene.TimeUnit != "ms" {
		return nil, &Error{Code: CodeSchema, Path: "clock.unit", Message: "version 3 requires millisecond clock"}
	}
	if scene.Encoding != "relative-delta" {
		return nil, &Error{Code: CodeSchema, Path: "controlPoints", Message: "version 3 requires relative-delta control points"}
	}
	scene.Interpolation, scene.Tolerance, scene.Subsamples, scene.Scale, err = parseDefaultsV3(raw)
	if err != nil {
		return nil, err
	}
	pathsRaw, e2 := asSlice(mustField(raw, "paths", ""), "paths")
	if e2 != nil {
		return nil, e2
	}
	for i, pv := range pathsRaw {
		p, e3 := parsePathV3(pv, idxPath("paths", i))
		if e3 != nil {
			return nil, e3
		}
		scene.Paths = append(scene.Paths, p)
	}
	samplesRaw, e2 := asSlice(mustField(raw, "samples", ""), "samples")
	if e2 != nil {
		return nil, e2
	}
	for i, sv := range samplesRaw {
		ms, e3 := asInt(sv, idxPath("samples", i))
		if e3 != nil {
			return nil, e3
		}
		scene.Samples = append(scene.Samples, millisToTicks(ms))
	}
	return scene, nil
}

func parsePathV3(v any, path string) (Path, *Error) {
	m, err := asMap(v, path)
	if err != nil {
		return Path{}, err
	}
	p := Path{LineJoin: JoinRound, Blend: BlendSourceOver, FillRule: "nonzero", Opacity: 255}
	if p.ID, err = stringField(m, "id", path); err != nil {
		return p, err
	}
	if raw, ok := optional(m, "closed"); ok {
		closed, e := asBool(raw, joinPath(path, "closed"))
		if e != nil {
			return p, e
		}
		if !closed {
			return p, &Error{Code: CodeSelf, Path: joinPath(path, "closed"), Message: "contour must be closed"}
		}
	}
	flagsRaw, e := asSlice(mustField(m, "flags", path), joinPath(path, "flags"))
	if e != nil {
		return p, e
	}
	for i, fv := range flagsRaw {
		flag, e := asString(fv, idxPath(joinPath(path, "flags"), i))
		if e != nil {
			return p, e
		}
		switch flag {
		case "fill":
			p.HasFill = true
		case "stroke":
			p.HasStroke = true
		default:
			return p, &Error{Code: CodeSchema, Path: idxPath(joinPath(path, "flags"), i), Message: "unknown paint flag"}
		}
	}
	if !p.HasFill && !p.HasStroke {
		return p, &Error{Code: CodeSchema, Path: joinPath(path, "flags"), Message: "path must enable fill or stroke"}
	}
	fillRaw, e := field(m, "fill", path)
	if e != nil {
		return p, e
	}
	fillMap, e := asMap(fillRaw, joinPath(path, "fill"))
	if e != nil {
		return p, e
	}
	colorRaw, e := field(fillMap, "color", joinPath(path, "fill"))
	if e != nil {
		return p, e
	}
	if p.Fill, e = parseColor(colorRaw, joinPath(joinPath(path, "fill"), "color"), p.ID); e != nil {
		return p, e
	}
	p.StrokeColor = p.Fill
	if raw, ok := optional(m, "stroke"); ok {
		strokeMap, e := asMap(raw, joinPath(path, "stroke"))
		if e != nil {
			return p, e
		}
		if colorRaw, ok := optional(strokeMap, "color"); ok {
			if p.StrokeColor, e = parseColor(colorRaw, joinPath(joinPath(path, "stroke"), "color"), p.ID); e != nil {
				return p, e
			}
		}
		if p.HasStroke {
			width, e := intField(strokeMap, "widthQ10", joinPath(path, "stroke"))
			if e != nil {
				return p, e
			}
			p.StrokeWidth = width
		}
		if raw, ok := optional(strokeMap, "join"); ok {
			if p.LineJoin, e = parseJoin(raw, joinPath(joinPath(path, "stroke"), "join"), p.ID, JoinRound); e != nil {
				return p, e
			}
		}
	} else if p.HasStroke {
		return p, &Error{Code: CodeSchema, Path: joinPath(path, "stroke"), Message: "required field is missing for stroked path"}
	}
	if raw, ok := optional(m, "blend"); ok {
		if p.Blend, e = parseBlend(raw, joinPath(path, "blend"), p.ID, BlendSourceOver); e != nil {
			return p, e
		}
	}
	if raw, ok := optional(m, "fillRule"); ok {
		p.FillRule, _ = asString(raw, joinPath(path, "fillRule"))
	}
	if raw, ok := optional(m, "opacity"); ok {
		n, e := asInt(raw, joinPath(path, "opacity"))
		if e != nil || n < 0 || n > 255 {
			return p, &Error{Code: CodeComp, Path: joinPath(path, "opacity"), Message: fmt.Sprintf("invalid opacity for path %s", p.ID)}
		}
		p.Opacity = uint8(n)
	}
	keys, e := asSlice(mustField(m, "keys", path), joinPath(path, "keys"))
	if e != nil {
		return p, e
	}
	for i, kv := range keys {
		kp := idxPath(joinPath(path, "keys"), i)
		km, e := asMap(kv, kp)
		if e != nil {
			return p, e
		}
		ms, e := intField(km, "timeMs", kp)
		if e != nil {
			return p, e
		}
		base, e := parsePoint(mustField(km, "base", kp), joinPath(kp, "base"), "absolute", Vec2{})
		if e != nil {
			return p, e
		}
		segs, e := parseSegmentsV3(mustField(km, "segments", kp), base, joinPath(kp, "segments"))
		if e != nil {
			return p, e
		}
		p.Keyframes = append(p.Keyframes, Keyframe{Time: millisToTicks(ms), Segments: segs})
	}
	return p, nil
}

// ValidateScene performs cross-field checks shared by all schema versions.
func ValidateScene(s *Scene) error {
	if s == nil {
		return &Error{Code: CodeSchema, Path: "$", Message: "scene is nil"}
	}
	if s.ID == "" {
		return &Error{Code: CodeSchema, Path: "sceneId", Message: "scene id is required"}
	}
	if len(s.Paths) == 0 {
		return &Error{Code: CodeSchema, Path: "paths", Message: "at least one path is required"}
	}
	if err := s.ValidateSamples(); err != nil {
		return err
	}
	if err := ValidateParameters(s); err != nil {
		return err
	}
	ids := map[string]bool{}
	for pi := range s.Paths {
		p := &s.Paths[pi]
		base := idxPath("paths", pi)
		if p.ID == "" {
			return &Error{Code: CodeSchema, Path: joinPath(base, "id"), Message: "path id is required"}
		}
		if ids[p.ID] {
			return &Error{Code: CodeSchema, Path: joinPath(base, "id"), Message: "duplicate path id"}
		}
		ids[p.ID] = true
		if !p.HasFill && !p.HasStroke {
			return &Error{Code: CodeSchema, Path: joinPath(base, "paint"), Message: "fill or stroke must be enabled"}
		}
		if !validBlend(p.Blend) {
			return &Error{Code: CodeComp, Path: joinPath(base, "blend"), Message: fmt.Sprintf("invalid blend mode for path %s", p.ID)}
		}
		if p.LineJoin != JoinRound && p.LineJoin != JoinBevel && p.LineJoin != JoinMiter {
			return &Error{Code: CodeComp, Path: joinPath(base, "lineJoin"), Message: fmt.Sprintf("invalid line join for path %s", p.ID)}
		}
		if p.HasStroke && p.StrokeWidth < 0 {
			return &Error{Code: CodePara, Path: joinPath(base, "strokeWidthQ10"), Message: "stroke width must be non-negative"}
		}
		if len(p.Keyframes) == 0 {
			return &Error{Code: CodeSelf, Path: joinPath(base, "keyframes"), Message: "path has no keyframes"}
		}
		for ki := range p.Keyframes {
			k := &p.Keyframes[ki]
			kp := idxPath(joinPath(base, "keyframes"), ki)
			if ki > 0 && k.Time <= p.Keyframes[ki-1].Time {
				return &Error{Code: CodeSelf, Path: joinPath(kp, "time"), Message: "keyframe times must be strictly increasing"}
			}
			if err := validateContour(k.Segments, joinPath(kp, "segments")); err != nil {
				return err
			}
			if ki > 0 && !sameTopology(p.Keyframes[ki-1].Segments, k.Segments) {
				return &Error{Code: CodeSelf, Path: joinPath(kp, "segments"), Message: "keyframe contour topology changed"}
			}
		}
	}
	return nil
}

func validateContour(segs []Segment, path string) error {
	if len(segs) == 0 {
		return &Error{Code: CodeSelf, Path: path, Message: "contour has no segments"}
	}
	for i := range segs {
		seg := &segs[i]
		p := idxPath(path, i)
		if seg.Kind != Quadratic && seg.Kind != Cubic {
			return &Error{Code: CodeSelf, Path: joinPath(p, "kind"), Message: "unsupported curve kind"}
		}
		if i > 0 && segs[i-1].End != seg.Start {
			return &Error{
				Code:    CodeSelf,
				Path:    joinPath(p, "start"),
				Message: "segments are not head-to-tail continuous",
				Left:    fmt.Sprintf("%+v", segs[i-1].End),
				Right:   fmt.Sprintf("%+v", seg.Start),
			}
		}
	}
	if segs[len(segs)-1].End != segs[0].Start {
		return &Error{
			Code:    CodeSelf,
			Path:    idxPath(path, len(segs)-1) + ".end",
			Message: "contour is not closed",
			Left:    fmt.Sprintf("%+v", segs[len(segs)-1].End),
			Right:   fmt.Sprintf("%+v", segs[0].Start),
		}
	}
	return nil
}

func sameTopology(a, b []Segment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Kind != b[i].Kind {
			return false
		}
	}
	return true
}

// ValidateParameters checks the renderer configuration and returns E_PARA.
func ValidateParameters(s *Scene) error {
	switch s.Interpolation.Mode {
	case "linear", "step", "smooth":
	default:
		return &Error{Code: CodePara, Path: "interpolation.mode", Field: "interpolation.mode", Message: fmt.Sprintf("unsupported interpolation %q", s.Interpolation.Mode)}
	}
	if s.Interpolation.PrecisionBits < 4 || s.Interpolation.PrecisionBits > 24 {
		return &Error{Code: CodePara, Path: "interpolation.precisionBits", Field: "interpolation.precisionBits", Message: "precision must be in [4,24]"}
	}
	if s.Tolerance < 1 || s.Tolerance > 16*CoordScale {
		return &Error{Code: CodePara, Path: "tolerance", Field: "tolerance", Message: "tolerance must be in [1, 16384] Q10 units"}
	}
	if s.Subsamples < 1 || s.Subsamples > 16 {
		return &Error{Code: CodePara, Path: "subsamples", Field: "subsamples", Message: "sub-sample grid must be in [1,16]"}
	}
	if s.Scale < 1 || s.Scale > 16*CoordScale {
		return &Error{Code: CodePara, Path: "scale", Field: "scale", Message: "scale must be in [1, 16384] Q10"}
	}
	if _, err := parseRounding(s.Rounding); err != nil {
		return err
	}
	return nil
}
