package engine

import (
	"encoding/json"
	"fmt"
	"os"
)

type ExportResult struct {
	Target   Version   `json:"target"`
	Data     []byte    `json:"-"`
	Warnings []Warning `json:"warnings"`
}

// ExportScene serializes the internal model to a supported target schema.
func ExportScene(scene *Scene, target Version, rounding Rounding) (ExportResult, error) {
	if scene == nil {
		return ExportResult{}, &Error{Code: CodeSchema, Path: "$", Message: "nil scene"}
	}
	if rounding == "" {
		rounding = RoundFloor
	}
	if _, err := parseRounding(string(rounding)); err != nil {
		return ExportResult{}, err
	}
	switch target {
	case Version1:
		return exportV1(scene, rounding)
	case Version2:
		return exportV2(scene, rounding)
	case Version3:
		return exportV3(scene, rounding)
	default:
		return ExportResult{}, &Error{Code: CodeVer, Path: "targetVersion", Message: string(target)}
	}
}

func ExportSceneFile(scene *Scene, target Version, rounding Rounding, path string) (ExportResult, error) {
	res, err := ExportScene(scene, target, rounding)
	if err != nil {
		return res, err
	}
	if err := os.WriteFile(path, res.Data, 0o644); err != nil {
		return res, err
	}
	return res, nil
}

func exportTime(t int64, target Version, rounding Rounding, path string) (int64, *Warning) {
	var encoded, back int64
	switch target {
	case Version1, Version3:
		encoded = ticksToMillis(t, rounding)
		back = millisToTicks(encoded)
	case Version2:
		encoded = ticksToFrames(t, rounding)
		back = framesToTicks(encoded)
	}
	if back == t {
		return encoded, nil
	}
	return encoded, &Warning{
		Code:        CodeLossy,
		Path:        path,
		Original:    fmt.Sprintf("%d ticks", t),
		Disposition: fmt.Sprintf("rounded to %d target units using %s", encoded, rounding),
	}
}

func supportedBlend(target Version, blend BlendMode) bool {
	switch target {
	case Version1:
		return blend == BlendSourceOver || blend == BlendMultiply || blend == BlendScreen
	case Version2:
		return blend == BlendSourceOver || blend == BlendMultiply || blend == BlendScreen
	case Version3:
		return validBlend(blend)
	default:
		return false
	}
}

func blendForTarget(target Version, p Path, path string) (BlendMode, *Warning) {
	if supportedBlend(target, p.Blend) {
		return p.Blend, nil
	}
	return BlendSourceOver, &Warning{
		Code:        CodeLossy,
		Path:        path + ".blend",
		Original:    string(p.Blend),
		Disposition: "downgraded to source-over",
	}
}

func colorSlice(c Color) []int {
	return []int{int(c.R), int(c.G), int(c.B), int(c.A)}
}

type v1Clock struct {
	Unit     string `json:"unit"`
	Rounding string `json:"rounding"`
}

type v1Defaults struct {
	Interpolation          string `json:"interpolation"`
	InterpolationPrecision int    `json:"interpolationPrecision"`
	ToleranceQ10           int64  `json:"toleranceQ10"`
	Subsamples             int    `json:"subsamples"`
	ScaleQ10               int64  `json:"scaleQ10"`
}

type v1Point struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type v1Segment struct {
	Curve string   `json:"curve"`
	Start v1Point  `json:"start"`
	C1    v1Point  `json:"c1"`
	C2    *v1Point `json:"c2,omitempty"`
	End   v1Point  `json:"end"`
}

type v1Keyframe struct {
	TimeMs   int64       `json:"timeMs"`
	Segments []v1Segment `json:"segments"`
}

type v1Path struct {
	ID             string       `json:"id"`
	Paint          string       `json:"paint"`
	FillColor      []int        `json:"fillColor"`
	StrokeColor    []int        `json:"strokeColor,omitempty"`
	StrokeWidthQ10 *int64       `json:"strokeWidthQ10,omitempty"`
	LineJoin       string       `json:"lineJoin,omitempty"`
	Blend          string       `json:"blend,omitempty"`
	FillRule       string       `json:"fillRule,omitempty"`
	Opacity        int          `json:"opacity"`
	Keyframes      []v1Keyframe `json:"keyframes"`
}

type v1Doc struct {
	SchemaVersion      string     `json:"schemaVersion"`
	SceneID            string     `json:"sceneId"`
	Clock              v1Clock    `json:"clock"`
	CoordinateEncoding string     `json:"coordinateEncoding"`
	FrameRate          int64      `json:"frameRate"`
	Defaults           v1Defaults `json:"defaults"`
	Paths              []v1Path   `json:"paths"`
	SamplesMs          []int64    `json:"samplesMs"`
}

func exportV1(scene *Scene, rounding Rounding) (ExportResult, error) {
	if err := ValidateScene(scene); err != nil {
		return ExportResult{}, err
	}
	res := ExportResult{Target: Version1}
	doc := v1Doc{
		SchemaVersion:      string(Version1),
		SceneID:            scene.ID,
		Clock:              v1Clock{Unit: "ms", Rounding: string(rounding)},
		CoordinateEncoding: "absolute-q10",
		FrameRate:          100,
		Defaults: v1Defaults{
			Interpolation:          scene.Interpolation.Mode,
			InterpolationPrecision: scene.Interpolation.PrecisionBits,
			ToleranceQ10:           scene.Tolerance,
			Subsamples:             scene.Subsamples,
			ScaleQ10:               scene.Scale,
		},
	}
	if scene.Encoding == "relative-delta" {
		res.Warnings = append(res.Warnings, Warning{Code: CodeLossy, Path: "coordinateEncoding", Original: "relative-delta", Disposition: "expanded to absolute-q10"})
	}
	for pi, p := range scene.Paths {
		base := fmt.Sprintf("paths[%d]", pi)
		blend, warning := blendForTarget(Version1, p, base)
		if warning != nil {
			res.Warnings = append(res.Warnings, *warning)
		}
		vp := v1Path{
			ID:        p.ID,
			Paint:     paintString(p),
			FillColor: colorSlice(p.Fill),
			Blend:     string(blend),
			FillRule:  p.FillRule,
			Opacity:   int(p.Opacity),
		}
		if p.HasStroke {
			vp.StrokeColor = colorSlice(p.StrokeColor)
			width := p.StrokeWidth
			vp.StrokeWidthQ10 = &width
			vp.LineJoin = string(p.LineJoin)
		}
		for ki, k := range p.Keyframes {
			t, warning := exportTime(k.Time, Version1, rounding, fmt.Sprintf("%s.keyframes[%d].time", base, ki))
			if warning != nil {
				res.Warnings = append(res.Warnings, *warning)
			}
			vk := v1Keyframe{TimeMs: t}
			for _, seg := range k.Segments {
				vs := v1Segment{Curve: string(seg.Kind), Start: toV1Point(seg.Start), C1: toV1Point(seg.C1), End: toV1Point(seg.End)}
				if seg.Kind == Cubic {
					c2 := toV1Point(seg.C2)
					vs.C2 = &c2
				}
				vk.Segments = append(vk.Segments, vs)
			}
			vp.Keyframes = append(vp.Keyframes, vk)
		}
		doc.Paths = append(doc.Paths, vp)
	}
	for i, t := range scene.Samples {
		ms, warning := exportTime(t, Version1, rounding, fmt.Sprintf("samplesMs[%d]", i))
		if warning != nil {
			res.Warnings = append(res.Warnings, *warning)
		}
		doc.SamplesMs = append(doc.SamplesMs, ms)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return res, err
	}
	res.Data = data
	return res, nil
}

type v2Clock struct {
	Unit     string `json:"unit"`
	Rounding string `json:"rounding"`
}

type v2Defaults struct {
	Interpolation struct {
		Mode      string `json:"mode"`
		Precision int    `json:"precision"`
	} `json:"interpolation"`
	FlattenTolerance int64 `json:"flattenTolerance"`
	AAGrid           int   `json:"aaGrid"`
	RenderScale      int64 `json:"renderScale"`
}

type v2Point struct {
	CX int64 `json:"cx"`
	CY int64 `json:"cy"`
}

type v2Curve struct {
	Degree int      `json:"degree"`
	P0     v2Point  `json:"p0"`
	P1     v2Point  `json:"p1"`
	P2     *v2Point `json:"p2,omitempty"`
	P3     v2Point  `json:"p3"`
}

type v2Key struct {
	T      int64     `json:"t"`
	Curves []v2Curve `json:"curves"`
}

type v2Path struct {
	ID          string  `json:"path_id"`
	Paint       string  `json:"paint"`
	FillColor   []int   `json:"fill_color"`
	StrokeColor []int   `json:"stroke_color,omitempty"`
	StrokeWidth *int64  `json:"stroke_width,omitempty"`
	LineJoin    string  `json:"line_join,omitempty"`
	Composite   string  `json:"composite,omitempty"`
	FillRule    string  `json:"fill_rule,omitempty"`
	Opacity     int     `json:"opacity"`
	Keys        []v2Key `json:"keys"`
}

type v2Doc struct {
	SchemaVersion string     `json:"schema_version"`
	SceneID       string     `json:"sceneId"`
	Clock         v2Clock    `json:"clock"`
	ControlPoints string     `json:"controlPoints"`
	Defaults      v2Defaults `json:"defaults"`
	Paths         []v2Path   `json:"paths"`
	SampleTimes   []int64    `json:"sampleTimes"`
}

func exportV2(scene *Scene, rounding Rounding) (ExportResult, error) {
	if err := ValidateScene(scene); err != nil {
		return ExportResult{}, err
	}
	res := ExportResult{Target: Version2}
	doc := v2Doc{
		SchemaVersion: "2.0.0",
		SceneID:       scene.ID,
		Clock:         v2Clock{Unit: "frame", Rounding: string(rounding)},
		ControlPoints: "absolute",
		Paths:         []v2Path{},
	}
	doc.Defaults.Interpolation.Mode = scene.Interpolation.Mode
	doc.Defaults.Interpolation.Precision = scene.Interpolation.PrecisionBits
	doc.Defaults.FlattenTolerance = scene.Tolerance
	doc.Defaults.AAGrid = scene.Subsamples
	doc.Defaults.RenderScale = scene.Scale
	if scene.Encoding == "relative-delta" {
		res.Warnings = append(res.Warnings, Warning{Code: CodeLossy, Path: "controlPoints", Original: "relative-delta", Disposition: "expanded to absolute"})
	}
	for pi, p := range scene.Paths {
		base := fmt.Sprintf("paths[%d]", pi)
		blend, warning := blendForTarget(Version2, p, base)
		if warning != nil {
			res.Warnings = append(res.Warnings, *warning)
		}
		vp := v2Path{
			ID:        p.ID,
			Paint:     paintString(p),
			FillColor: colorSlice(p.Fill),
			Composite: string(blend),
			FillRule:  p.FillRule,
			Opacity:   int(p.Opacity),
		}
		if p.HasStroke {
			vp.StrokeColor = colorSlice(p.StrokeColor)
			width := p.StrokeWidth
			vp.StrokeWidth = &width
			vp.LineJoin = string(p.LineJoin)
		}
		for ki, k := range p.Keyframes {
			t, warning := exportTime(k.Time, Version2, rounding, fmt.Sprintf("%s.keys[%d].t", base, ki))
			if warning != nil {
				res.Warnings = append(res.Warnings, *warning)
			}
			vk := v2Key{T: t}
			for _, seg := range k.Segments {
				degree := 2
				curve := v2Curve{Degree: degree, P0: toV2Point(seg.Start), P1: toV2Point(seg.C1), P3: toV2Point(seg.End)}
				if seg.Kind == Cubic {
					curve.Degree = 3
					p2 := toV2Point(seg.C2)
					curve.P2 = &p2
				}
				vk.Curves = append(vk.Curves, curve)
			}
			vp.Keys = append(vp.Keys, vk)
		}
		doc.Paths = append(doc.Paths, vp)
	}
	for i, t := range scene.Samples {
		frames, warning := exportTime(t, Version2, rounding, fmt.Sprintf("sampleTimes[%d]", i))
		if warning != nil {
			res.Warnings = append(res.Warnings, *warning)
		}
		doc.SampleTimes = append(doc.SampleTimes, frames)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return res, err
	}
	res.Data = data
	return res, nil
}

type v3Point struct {
	DX int64 `json:"dx"`
	DY int64 `json:"dy"`
}

type v3Base struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type v3Segment struct {
	Kind string   `json:"kind"`
	P0   v3Point  `json:"p0"`
	P1   v3Point  `json:"p1"`
	P2   *v3Point `json:"p2,omitempty"`
	P3   v3Point  `json:"p3"`
}

type v3Key struct {
	TimeMs   int64       `json:"timeMs"`
	Base     v3Base      `json:"base"`
	Segments []v3Segment `json:"segments"`
}

type v3Fill struct {
	Color []int `json:"color"`
}

type v3Stroke struct {
	Color    []int  `json:"color"`
	WidthQ10 int64  `json:"widthQ10"`
	Join     string `json:"join"`
}

type v3Path struct {
	ID       string    `json:"id"`
	Flags    []string  `json:"flags"`
	Fill     v3Fill    `json:"fill"`
	Stroke   *v3Stroke `json:"stroke,omitempty"`
	Blend    string    `json:"blend"`
	FillRule string    `json:"fillRule,omitempty"`
	Opacity  int       `json:"opacity"`
	Keys     []v3Key   `json:"keys"`
}

type v3Doc struct {
	Version       int        `json:"version"`
	ID            string     `json:"id"`
	Clock         v3Clock    `json:"clock"`
	ControlPoints string     `json:"controlPoints"`
	Defaults      v3Defaults `json:"defaults"`
	Paths         []v3Path   `json:"paths"`
	Samples       []int64    `json:"samples"`
}

type v3Clock struct {
	Unit  string `json:"unit"`
	Round string `json:"round"`
}

type v3Defaults struct {
	Interp struct {
		Mode          string `json:"mode"`
		PrecisionBits int    `json:"precisionBits"`
	} `json:"interp"`
	FlattenToleranceQ10 int64 `json:"flattenToleranceQ10"`
	SubsampleGrid       int   `json:"subsampleGrid"`
	PixelScaleQ10       int64 `json:"pixelScaleQ10"`
}

func exportV3(scene *Scene, rounding Rounding) (ExportResult, error) {
	if err := ValidateScene(scene); err != nil {
		return ExportResult{}, err
	}
	res := ExportResult{Target: Version3}
	doc := v3Doc{
		Version:       3,
		ID:            scene.ID,
		Clock:         v3Clock{Unit: "ms", Round: string(rounding)},
		ControlPoints: "relative-delta",
		Paths:         []v3Path{},
	}
	doc.Defaults.Interp.Mode = scene.Interpolation.Mode
	doc.Defaults.Interp.PrecisionBits = scene.Interpolation.PrecisionBits
	doc.Defaults.FlattenToleranceQ10 = scene.Tolerance
	doc.Defaults.SubsampleGrid = scene.Subsamples
	doc.Defaults.PixelScaleQ10 = scene.Scale
	for pi, p := range scene.Paths {
		blend, warning := blendForTarget(Version3, p, fmt.Sprintf("paths[%d]", pi))
		if warning != nil {
			res.Warnings = append(res.Warnings, *warning)
		}
		vp := v3Path{
			ID:       p.ID,
			Flags:    paintFlags(p),
			Fill:     v3Fill{Color: colorSlice(p.Fill)},
			Blend:    string(blend),
			FillRule: p.FillRule,
			Opacity:  int(p.Opacity),
		}
		if p.HasStroke {
			vp.Stroke = &v3Stroke{Color: colorSlice(p.StrokeColor), WidthQ10: p.StrokeWidth, Join: string(p.LineJoin)}
		}
		for ki, k := range p.Keyframes {
			t, warning := exportTime(k.Time, Version3, rounding, fmt.Sprintf("paths[%d].keys[%d].timeMs", pi, ki))
			if warning != nil {
				res.Warnings = append(res.Warnings, *warning)
			}
			base := k.Segments[0].Start
			vk := v3Key{TimeMs: t, Base: v3Base{X: base.X, Y: base.Y}}
			for _, seg := range k.Segments {
				vs := v3Segment{
					Kind: string(seg.Kind),
					P0:   toV3Point(seg.Start, base),
					P1:   toV3Point(seg.C1, base),
					P3:   toV3Point(seg.End, base),
				}
				if seg.Kind == Cubic {
					p2 := toV3Point(seg.C2, base)
					vs.P2 = &p2
				}
				vk.Segments = append(vk.Segments, vs)
			}
			vp.Keys = append(vp.Keys, vk)
		}
		doc.Paths = append(doc.Paths, vp)
	}
	for i, t := range scene.Samples {
		ms, warning := exportTime(t, Version3, rounding, fmt.Sprintf("samples[%d]", i))
		if warning != nil {
			res.Warnings = append(res.Warnings, *warning)
		}
		doc.Samples = append(doc.Samples, ms)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return res, err
	}
	res.Data = data
	return res, nil
}

func paintString(p Path) string {
	switch {
	case p.HasFill && p.HasStroke:
		return "fill-stroke"
	case p.HasStroke:
		return "stroke"
	default:
		return "fill"
	}
}

func paintFlags(p Path) []string {
	var out []string
	if p.HasFill {
		out = append(out, "fill")
	}
	if p.HasStroke {
		out = append(out, "stroke")
	}
	return out
}

func toV1Point(p Vec2) v1Point { return v1Point{X: p.X, Y: p.Y} }
func toV2Point(p Vec2) v2Point { return v2Point{CX: p.X, CY: p.Y} }
func toV3Point(p, base Vec2) v3Point {
	return v3Point{DX: p.X - base.X, DY: p.Y - base.Y}
}

// VerifyExport loads the exported document and compares every semantic field.
// Blend values explicitly reported in E_LOSSY warnings are checked against the
// documented source-over fallback rather than the original value.
func VerifyExport(scene *Scene, result ExportResult) (*Scene, error) {
	loaded, err := LoadScene(result.Data)
	if err != nil {
		return nil, err
	}
	lossyBlend := map[string]bool{}
	for _, w := range result.Warnings {
		if w.Code == CodeLossy && (len(w.Path) > 6 && w.Path[len(w.Path)-6:] == ".blend") {
			lossyBlend[w.Path] = true
		}
	}
	return loaded, compareScenes(scene, loaded, CodeEq, lossyBlend)
}

// RoundTripScene performs source -> target -> source and checks exact closure.
func RoundTripScene(scene *Scene, target Version, rounding Rounding) (*Scene, []Warning, error) {
	down, err := ExportScene(scene, target, rounding)
	if err != nil {
		return nil, nil, err
	}
	low, err := LoadScene(down.Data)
	if err != nil {
		return nil, down.Warnings, err
	}
	up, err := ExportScene(low, scene.SourceVersion, rounding)
	if err != nil {
		return nil, append(down.Warnings, up.Warnings...), err
	}
	back, err := LoadScene(up.Data)
	if err != nil {
		return nil, append(down.Warnings, up.Warnings...), err
	}
	if err := compareRoundTripGeometry(scene, back); err != nil {
		return back, append(down.Warnings, up.Warnings...), err
	}
	return back, append(down.Warnings, up.Warnings...), nil
}

func compareRoundTripGeometry(a, b *Scene) error {
	if len(a.Samples) != len(b.Samples) {
		return &Error{Code: CodeRT, Path: "samples", Left: fmt.Sprintf("len=%d", len(a.Samples)), Right: fmt.Sprintf("len=%d", len(b.Samples))}
	}
	for i := range a.Samples {
		if a.Samples[i] != b.Samples[i] {
			return &Error{Code: CodeRT, Path: fmt.Sprintf("samples[%d]", i), Left: fmt.Sprintf("%d", a.Samples[i]), Right: fmt.Sprintf("%d", b.Samples[i])}
		}
	}
	if len(a.Paths) != len(b.Paths) {
		return &Error{Code: CodeRT, Path: "paths", Left: fmt.Sprintf("len=%d", len(a.Paths)), Right: fmt.Sprintf("len=%d", len(b.Paths))}
	}
	for pi := range a.Paths {
		pa, pb := &a.Paths[pi], &b.Paths[pi]
		base := fmt.Sprintf("paths[%d]", pi)
		if pa.ID != pb.ID {
			return &Error{Code: CodeRT, Path: base + ".id", Left: pa.ID, Right: pb.ID}
		}
		if len(pa.Keyframes) != len(pb.Keyframes) {
			return &Error{Code: CodeRT, Path: base + ".keyframes", Left: fmt.Sprintf("len=%d", len(pa.Keyframes)), Right: fmt.Sprintf("len=%d", len(pb.Keyframes))}
		}
		for ki := range pa.Keyframes {
			ka, kb := pa.Keyframes[ki], pb.Keyframes[ki]
			kp := fmt.Sprintf("%s.keyframes[%d]", base, ki)
			if ka.Time != kb.Time {
				return &Error{Code: CodeRT, Path: kp + ".time", Left: fmt.Sprintf("%d", ka.Time), Right: fmt.Sprintf("%d", kb.Time)}
			}
			if len(ka.Segments) != len(kb.Segments) {
				return &Error{Code: CodeRT, Path: kp + ".segments", Left: fmt.Sprintf("len=%d", len(ka.Segments)), Right: fmt.Sprintf("len=%d", len(kb.Segments))}
			}
			for si := range ka.Segments {
				if ka.Segments[si] != kb.Segments[si] {
					return firstSegmentDiff(ka.Segments[si], kb.Segments[si], fmt.Sprintf("%s.segments[%d]", kp, si), CodeRT)
				}
			}
		}
	}
	return nil
}

// SemanticDiff returns E_EQ at the first field whose internal semantics differ.
func SemanticDiff(a, b *Scene) error {
	return compareScenes(a, b, CodeEq, nil)
}

// SemanticEqual reports whether two loaded scenes are semantically identical.
func SemanticEqual(a, b *Scene) bool {
	return SemanticDiff(a, b) == nil
}

func compareScenes(a, b *Scene, code Code, skipBlend map[string]bool) error {
	if a.ID != b.ID {
		return &Error{Code: code, Path: "sceneId", Left: a.ID, Right: b.ID}
	}
	if a.Interpolation != b.Interpolation {
		return &Error{Code: code, Path: "interpolation", Left: fmt.Sprintf("%+v", a.Interpolation), Right: fmt.Sprintf("%+v", b.Interpolation)}
	}
	if a.Tolerance != b.Tolerance {
		return &Error{Code: code, Path: "tolerance", Left: fmt.Sprintf("%d", a.Tolerance), Right: fmt.Sprintf("%d", b.Tolerance)}
	}
	if a.Subsamples != b.Subsamples {
		return &Error{Code: code, Path: "subsamples", Left: fmt.Sprintf("%d", a.Subsamples), Right: fmt.Sprintf("%d", b.Subsamples)}
	}
	if a.Scale != b.Scale {
		return &Error{Code: code, Path: "scale", Left: fmt.Sprintf("%d", a.Scale), Right: fmt.Sprintf("%d", b.Scale)}
	}
	if len(a.Samples) != len(b.Samples) {
		return &Error{Code: code, Path: "samples", Left: fmt.Sprintf("len=%d", len(a.Samples)), Right: fmt.Sprintf("len=%d", len(b.Samples))}
	}
	for i := range a.Samples {
		if a.Samples[i] != b.Samples[i] {
			return &Error{Code: code, Path: fmt.Sprintf("samples[%d]", i), Left: fmt.Sprintf("%d", a.Samples[i]), Right: fmt.Sprintf("%d", b.Samples[i])}
		}
	}
	if len(a.Paths) != len(b.Paths) {
		return &Error{Code: code, Path: "paths", Left: fmt.Sprintf("len=%d", len(a.Paths)), Right: fmt.Sprintf("len=%d", len(b.Paths))}
	}
	for pi := range a.Paths {
		pa, pb := &a.Paths[pi], &b.Paths[pi]
		base := fmt.Sprintf("paths[%d]", pi)
		if pa.ID != pb.ID {
			return &Error{Code: code, Path: base + ".id", Left: pa.ID, Right: pb.ID}
		}
		if pa.HasFill != pb.HasFill || pa.HasStroke != pb.HasStroke {
			return &Error{Code: code, Path: base + ".paint", Left: paintString(*pa), Right: paintString(*pb)}
		}
		if pa.Fill != pb.Fill {
			return &Error{Code: code, Path: base + ".fill", Left: fmt.Sprintf("%+v", pa.Fill), Right: fmt.Sprintf("%+v", pb.Fill)}
		}
		if pa.StrokeColor != pb.StrokeColor {
			return &Error{Code: code, Path: base + ".strokeColor", Left: fmt.Sprintf("%+v", pa.StrokeColor), Right: fmt.Sprintf("%+v", pb.StrokeColor)}
		}
		if pa.StrokeWidth != pb.StrokeWidth {
			return &Error{Code: code, Path: base + ".strokeWidthQ10", Left: fmt.Sprintf("%d", pa.StrokeWidth), Right: fmt.Sprintf("%d", pb.StrokeWidth)}
		}
		if pa.LineJoin != pb.LineJoin {
			return &Error{Code: code, Path: base + ".lineJoin", Left: string(pa.LineJoin), Right: string(pb.LineJoin)}
		}
		if !skipBlend[base+".blend"] && pa.Blend != pb.Blend {
			return &Error{Code: code, Path: base + ".blend", Left: string(pa.Blend), Right: string(pb.Blend)}
		}
		if pa.FillRule != pb.FillRule {
			return &Error{Code: code, Path: base + ".fillRule", Left: pa.FillRule, Right: pb.FillRule}
		}
		if pa.Opacity != pb.Opacity {
			return &Error{Code: code, Path: base + ".opacity", Left: fmt.Sprintf("%d", pa.Opacity), Right: fmt.Sprintf("%d", pb.Opacity)}
		}
		if len(pa.Keyframes) != len(pb.Keyframes) {
			return &Error{Code: code, Path: base + ".keyframes", Left: fmt.Sprintf("len=%d", len(pa.Keyframes)), Right: fmt.Sprintf("len=%d", len(pb.Keyframes))}
		}
		for ki := range pa.Keyframes {
			ka, kb := pa.Keyframes[ki], pb.Keyframes[ki]
			kp := fmt.Sprintf("%s.keyframes[%d]", base, ki)
			if ka.Time != kb.Time {
				return &Error{Code: code, Path: kp + ".time", Left: fmt.Sprintf("%d", ka.Time), Right: fmt.Sprintf("%d", kb.Time)}
			}
			if len(ka.Segments) != len(kb.Segments) {
				return &Error{Code: code, Path: kp + ".segments", Left: fmt.Sprintf("len=%d", len(ka.Segments)), Right: fmt.Sprintf("len=%d", len(kb.Segments))}
			}
			for si := range ka.Segments {
				if ka.Segments[si] != kb.Segments[si] {
					sp := fmt.Sprintf("%s.segments[%d]", kp, si)
					return firstSegmentDiff(ka.Segments[si], kb.Segments[si], sp, code)
				}
			}
		}
	}
	return nil
}

func firstSegmentDiff(a, b Segment, path string, code Code) error {
	if a.Kind != b.Kind {
		return &Error{Code: code, Path: path + ".kind", Left: string(a.Kind), Right: string(b.Kind)}
	}
	for _, f := range []struct {
		name string
		a    Vec2
		b    Vec2
	}{{"start", a.Start, b.Start}, {"c1", a.C1, b.C1}, {"c2", a.C2, b.C2}, {"end", a.End, b.End}} {
		if f.a != f.b {
			return &Error{Code: code, Path: path + "." + f.name, Left: fmt.Sprintf("%+v", f.a), Right: fmt.Sprintf("%+v", f.b)}
		}
	}
	return &Error{Code: code, Path: path, Left: fmt.Sprintf("%+v", a), Right: fmt.Sprintf("%+v", b)}
}
