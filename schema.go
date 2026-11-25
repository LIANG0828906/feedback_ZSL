package vectorcore

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
)

type schemaProbe struct {
	Camel *int `json:"schemaVersion"`
	Snake *int `json:"schema_version"`
}

// LoadSceneFile loads a scene from a local JSON file.
func LoadSceneFile(path string) (*Scene, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Code: CodeIO, Op: "readScene", Field: path, Keyframe: -1, TimeIndex: -1, Err: err}
	}
	return LoadSceneBytes(data)
}

// LoadScene loads and migrates a scene from JSON.
func LoadScene(r io.Reader) (*Scene, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, &Error{Code: CodeDecode, Op: "loadScene", Keyframe: -1, TimeIndex: -1, Err: err}
	}
	return LoadSceneBytes(data)
}

// LoadSceneBytes detects the declared schema version, maps it to the internal
// integer model and preserves the source version on the result.
func LoadSceneBytes(data []byte) (*Scene, error) {
	var probe schemaProbe
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, &Error{Code: CodeDecode, Op: "loadScene", Field: "json", Keyframe: -1, TimeIndex: -1, Err: err}
	}
	if probe.Camel == nil && probe.Snake == nil {
		return nil, &Error{Code: CodeSchema, Op: "loadScene", Field: "schemaVersion", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("required field is missing")}
	}
	if probe.Camel != nil && probe.Snake != nil && *probe.Camel != *probe.Snake {
		return nil, &Error{Code: CodeSchema, Op: "loadScene", Field: "schemaVersion", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("schemaVersion and schema_version disagree")}
	}
	version := 0
	if probe.Camel != nil {
		version = *probe.Camel
	} else {
		version = *probe.Snake
	}
	var (
		scene *Scene
		err   error
	)
	switch version {
	case 1:
		scene, err = loadV1(data)
	case 2:
		scene, err = loadV2(data)
	case 3:
		scene, err = loadV3(data)
	default:
		return nil, &Error{Code: CodeVersion, Op: "loadScene", SourceVersion: version, Field: "schemaVersion", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("unsupported schema version %d", version)}
	}
	if err != nil {
		return nil, err
	}
	if err := scene.Validate(); err != nil {
		if e, ok := err.(*Error); ok && e.SourceVersion == 0 {
			e.SourceVersion = version
		}
		return nil, err
	}
	return scene, nil
}

func decodeVersion(data []byte, dst any, version int) error {
	if err := json.Unmarshal(data, dst); err != nil {
		return &Error{Code: CodeDecode, Op: "decode", SourceVersion: version, Keyframe: -1, TimeIndex: -1, Err: err}
	}
	return nil
}

func requiredString(p *string, field string, version int) (string, error) {
	if p == nil {
		return "", &Error{Code: CodeSchema, Op: "decode", SourceVersion: version, Field: field, Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("required field is missing")}
	}
	if *p == "" {
		return "", &Error{Code: CodeSchema, Op: "decode", SourceVersion: version, Field: field, Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("required field is empty")}
	}
	return *p, nil
}

func requiredInt(p *int64, field string, version int) (int64, error) {
	if p == nil {
		return 0, &Error{Code: CodeSchema, Op: "decode", SourceVersion: version, Field: field, Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("required field is missing")}
	}
	return *p, nil
}

func requiredBool(p *bool, field string, version int) (bool, error) {
	if p == nil {
		return false, &Error{Code: CodeSchema, Op: "decode", SourceVersion: version, Field: field, Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("required field is missing")}
	}
	return *p, nil
}

func timeConversionError(err error, code Code, field string, version int, pathID string, keyframe, timeIndex int) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Op: "convertTime", SourceVersion: version, Field: field, PathID: pathID,
		Keyframe: keyframe, TimeIndex: timeIndex, Err: err}
}

func sortedUniqueTimes(times []int64) bool {
	if !sort.SliceIsSorted(times, func(i, j int) bool { return times[i] < times[j] }) {
		return false
	}
	for i := 1; i < len(times); i++ {
		if times[i] == times[i-1] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// v1: flat camelCase fields, frame time, absolute points, hex color.
// ---------------------------------------------------------------------------

type v1Point struct {
	X *int64 `json:"x"`
	Y *int64 `json:"y"`
}

type v1Segment struct {
	Kind *string  `json:"kind"`
	C1   *v1Point `json:"c1"`
	C2   *v1Point `json:"c2"`
	To   *v1Point `json:"to"`
}

type v1Frame struct {
	Frame    *int64      `json:"frame"`
	Start    *v1Point    `json:"start"`
	Segments []v1Segment `json:"segments"`
}

type v1Path struct {
	PathID      *string   `json:"pathId"`
	Fill        *bool     `json:"fill"`
	StrokeWidth *int64    `json:"strokeWidth"`
	StrokeJoin  *string   `json:"strokeJoin"`
	Color       *string   `json:"color"`
	Opacity     *int64    `json:"opacity"`
	BlendMode   *string   `json:"blendMode"`
	Frames      []v1Frame `json:"frames"`
}

type v1Scene struct {
	SchemaVersion *int     `json:"schemaVersion"`
	SceneID       *string  `json:"sceneId"`
	FrameRate     *int64   `json:"frameRate"`
	Rounding      *string  `json:"rounding"`
	SampleFrames  []int64  `json:"sampleFrames"`
	Paths         []v1Path `json:"paths"`
}

func loadV1(data []byte) (*Scene, error) {
	var raw v1Scene
	if err := decodeVersion(data, &raw, 1); err != nil {
		return nil, err
	}
	sceneID, err := requiredString(raw.SceneID, "sceneId", 1)
	if err != nil {
		return nil, err
	}
	fps, err := requiredInt(raw.FrameRate, "frameRate", 1)
	if err != nil {
		return nil, err
	}
	rounding, err := parseRoundingField(raw.Rounding, "rounding", 1)
	if err != nil {
		return nil, err
	}
	if len(raw.SampleFrames) == 0 {
		return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 1, Field: "sampleFrames", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("required list is empty")}
	}
	samples := make([]int64, len(raw.SampleFrames))
	for i, frame := range raw.SampleFrames {
		if frame < 0 {
			return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 1, Field: fmt.Sprintf("sampleFrames[%d]", i), Keyframe: -1, TimeIndex: i}
		}
		ticks, convErr := FrameToTicks(frame, fps, rounding)
		if convErr != nil {
			return nil, timeConversionError(convErr, CodeOverflow, fmt.Sprintf("sampleFrames[%d]", i), 1, "", -1, i)
		}
		samples[i] = ticks
	}
	if !sortedUniqueTimes(samples) {
		return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 1, Field: "sampleFrames", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("sample times must be strictly increasing")}
	}
	scene := &Scene{ID: sceneID, SourceVersion: 1, FPS: fps, Rounding: rounding, SampleTimes: samples}
	for pi, rp := range raw.Paths {
		pathID, err := requiredString(rp.PathID, fmt.Sprintf("paths[%d].pathId", pi), 1)
		if err != nil {
			return nil, err
		}
		fill, err := requiredBool(rp.Fill, fmt.Sprintf("paths[%d].fill", pi), 1)
		if err != nil {
			return nil, err
		}
		colorHex, err := requiredString(rp.Color, fmt.Sprintf("paths[%d].color", pi), 1)
		if err != nil {
			return nil, err
		}
		color, colorErr := ParseHexColor(colorHex)
		if colorErr != nil {
			return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 1, Field: fmt.Sprintf("paths[%d].color", pi), PathID: pathID, Keyframe: -1, TimeIndex: -1, Err: colorErr}
		}
		opacity, err := requiredInt(rp.Opacity, fmt.Sprintf("paths[%d].opacity", pi), 1)
		if err != nil {
			return nil, err
		}
		width := int64(0)
		if rp.StrokeWidth != nil {
			width = *rp.StrokeWidth
		}
		join := JoinRound
		if rp.StrokeJoin != nil {
			join = JoinMode(*rp.StrokeJoin)
		}
		blend := BlendNormal
		if rp.BlendMode != nil {
			blend = BlendMode(*rp.BlendMode)
		}
		if len(rp.Frames) == 0 {
			return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 1, PathID: pathID, Field: fmt.Sprintf("paths[%d].frames", pi), Keyframe: -1, TimeIndex: -1}
		}
		path := Path{ID: pathID, Paint: Paint{Fill: fill, StrokeWidth: width, Join: join, Color: color, Opacity: int(opacity), Blend: blend}}
		for ki, rf := range rp.Frames {
			frame, err := requiredInt(rf.Frame, fmt.Sprintf("paths[%d].frames[%d].frame", pi, ki), 1)
			if err != nil {
				if e, ok := err.(*Error); ok {
					e.PathID, e.Keyframe = pathID, ki
				}
				return nil, err
			}
			if frame < 0 {
				return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 1, Field: fmt.Sprintf("paths[%d].frames[%d].frame", pi, ki), PathID: pathID, Keyframe: ki, TimeIndex: -1}
			}
			ticks, convErr := FrameToTicks(frame, fps, rounding)
			if convErr != nil {
				return nil, timeConversionError(convErr, CodeOverflow, fmt.Sprintf("paths[%d].frames[%d].frame", pi, ki), 1, pathID, ki, -1)
			}
			contour, err := decodeV1Contour(rf, fmt.Sprintf("paths[%d].frames[%d]", pi, ki), 1, pathID, ki)
			if err != nil {
				return nil, err
			}
			path.Keyframes = append(path.Keyframes, Keyframe{Time: ticks, Contour: contour})
		}
		scene.Paths = append(scene.Paths, path)
	}
	return scene, nil
}

func parseRoundingField(value *string, field string, version int) (Rounding, error) {
	if value == nil {
		return RoundNearest, nil
	}
	r, err := ParseRounding(*value)
	if err != nil {
		return "", &Error{Code: CodeSchema, Op: "decode", SourceVersion: version, Field: field, Keyframe: -1, TimeIndex: -1, Err: err}
	}
	return r, nil
}

func v1PointValue(p *v1Point, field string, version int, pathID string, keyframe int) (Point, error) {
	if p == nil {
		return Point{}, &Error{Code: CodeSchema, Op: "decode", SourceVersion: version, Field: field, PathID: pathID, Keyframe: keyframe, TimeIndex: -1,
			Err: fmt.Errorf("required point is missing")}
	}
	x, err := requiredInt(p.X, field+".x", version)
	if err != nil {
		if e, ok := err.(*Error); ok {
			e.PathID, e.Keyframe = pathID, keyframe
		}
		return Point{}, err
	}
	y, err := requiredInt(p.Y, field+".y", version)
	if err != nil {
		if e, ok := err.(*Error); ok {
			e.PathID, e.Keyframe = pathID, keyframe
		}
		return Point{}, err
	}
	return Point{X: x, Y: y}, nil
}

func decodeV1Contour(raw v1Frame, prefix string, version int, pathID string, keyframe int) (Contour, error) {
	start, err := v1PointValue(raw.Start, prefix+".start", version, pathID, keyframe)
	if err != nil {
		return Contour{}, err
	}
	if len(raw.Segments) == 0 {
		return Contour{}, &Error{Code: CodeSchema, Op: "decode", SourceVersion: version, Field: prefix + ".segments", PathID: pathID, Keyframe: keyframe, TimeIndex: -1,
			Err: fmt.Errorf("required list is empty")}
	}
	contour := Contour{Start: start}
	for si, rs := range raw.Segments {
		kind, err := requiredString(rs.Kind, fmt.Sprintf("%s.segments[%d].kind", prefix, si), version)
		if err != nil {
			if e, ok := err.(*Error); ok {
				e.PathID, e.Keyframe = pathID, keyframe
			}
			return Contour{}, err
		}
		c1, err := v1PointValue(rs.C1, fmt.Sprintf("%s.segments[%d].c1", prefix, si), version, pathID, keyframe)
		if err != nil {
			return Contour{}, err
		}
		to, err := v1PointValue(rs.To, fmt.Sprintf("%s.segments[%d].to", prefix, si), version, pathID, keyframe)
		if err != nil {
			return Contour{}, err
		}
		seg := Segment{Kind: SegmentKind(kind), C1: c1, To: to}
		if seg.Kind == SegmentCubic {
			c2, err := v1PointValue(rs.C2, fmt.Sprintf("%s.segments[%d].c2", prefix, si), version, pathID, keyframe)
			if err != nil {
				return Contour{}, err
			}
			seg.C2 = c2
		} else if seg.Kind != SegmentQuad {
			return Contour{}, &Error{Code: CodeSchema, Op: "decode", SourceVersion: version, Field: fmt.Sprintf("%s.segments[%d].kind", prefix, si), PathID: pathID, Keyframe: keyframe, TimeIndex: -1,
				Err: fmt.Errorf("unknown segment kind %q", kind)}
		}
		contour.Segments = append(contour.Segments, seg)
	}
	return contour, nil
}

// ---------------------------------------------------------------------------
// v2: snake_case, millisecond time, base+delta coordinates, optional style.
// ---------------------------------------------------------------------------

type v2Point struct {
	X *int64 `json:"x"`
	Y *int64 `json:"y"`
}

type v2Delta struct {
	DX *int64 `json:"dx"`
	DY *int64 `json:"dy"`
}

type v2Segment struct {
	Kind *string  `json:"kind"`
	C1   *v2Delta `json:"c1"`
	C2   *v2Delta `json:"c2"`
	To   *v2Delta `json:"to"`
}

type v2Shape struct {
	Base     *v2Point    `json:"base"`
	Segments []v2Segment `json:"segments"`
}

type v2Track struct {
	At    *int64   `json:"at"`
	Shape *v2Shape `json:"shape"`
}

type v2Stroke struct {
	Width *int64  `json:"width"`
	Join  *string `json:"join"`
}

type v2Paint struct {
	Fill    *bool     `json:"fill"`
	Color   []int64   `json:"color"`
	Opacity *int64    `json:"opacity"`
	Blend   *string   `json:"blend"`
	Stroke  *v2Stroke `json:"stroke"`
}

type v2Path struct {
	PathID *string   `json:"path_id"`
	Paint  *v2Paint  `json:"paint"`
	Tracks []v2Track `json:"tracks"`
}

type v2Timeline struct {
	Unit     *string `json:"unit"`
	FPS      *int64  `json:"fps"`
	Rounding *string `json:"rounding"`
	Samples  []int64 `json:"samples"`
}

type v2Scene struct {
	SchemaVersion *int        `json:"schema_version"`
	ID            *string     `json:"id"`
	Timeline      *v2Timeline `json:"timeline"`
	Paths         []v2Path    `json:"paths"`
}

func loadV2(data []byte) (*Scene, error) {
	var raw v2Scene
	if err := decodeVersion(data, &raw, 2); err != nil {
		return nil, err
	}
	sceneID, err := requiredString(raw.ID, "id", 2)
	if err != nil {
		return nil, err
	}
	if raw.Timeline == nil {
		return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, Field: "timeline", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("required object is missing")}
	}
	unit, err := requiredString(raw.Timeline.Unit, "timeline.unit", 2)
	if err != nil {
		return nil, err
	}
	if unit != "ms" && unit != "millisecond" && unit != "milliseconds" {
		return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, Field: "timeline.unit", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("v2 supports millisecond time only, got %q", unit)}
	}
	fps, err := requiredInt(raw.Timeline.FPS, "timeline.fps", 2)
	if err != nil {
		return nil, err
	}
	rounding, err := parseRoundingField(raw.Timeline.Rounding, "timeline.rounding", 2)
	if err != nil {
		return nil, err
	}
	if len(raw.Timeline.Samples) == 0 {
		return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, Field: "timeline.samples", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("required list is empty")}
	}
	samples := make([]int64, len(raw.Timeline.Samples))
	for i, ms := range raw.Timeline.Samples {
		if ms < 0 {
			return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, Field: fmt.Sprintf("timeline.samples[%d]", i), Keyframe: -1, TimeIndex: i}
		}
		ticks, convErr := MillisToTicks(ms, rounding)
		if convErr != nil {
			return nil, timeConversionError(convErr, CodeOverflow, fmt.Sprintf("timeline.samples[%d]", i), 2, "", -1, i)
		}
		samples[i] = ticks
	}
	if !sortedUniqueTimes(samples) {
		return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, Field: "timeline.samples", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("sample times must be strictly increasing")}
	}
	scene := &Scene{ID: sceneID, SourceVersion: 2, FPS: fps, Rounding: rounding, SampleTimes: samples}
	for pi, rp := range raw.Paths {
		pathID, err := requiredString(rp.PathID, fmt.Sprintf("paths[%d].path_id", pi), 2)
		if err != nil {
			return nil, err
		}
		if rp.Paint == nil {
			return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, PathID: pathID, Field: fmt.Sprintf("paths[%d].paint", pi), Keyframe: -1, TimeIndex: -1,
				Err: fmt.Errorf("required object is missing")}
		}
		fill, err := requiredBool(rp.Paint.Fill, fmt.Sprintf("paths[%d].paint.fill", pi), 2)
		if err != nil {
			return nil, err
		}
		opacity, err := requiredInt(rp.Paint.Opacity, fmt.Sprintf("paths[%d].paint.opacity", pi), 2)
		if err != nil {
			return nil, err
		}
		if len(rp.Paint.Color) != 4 {
			return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, PathID: pathID, Field: fmt.Sprintf("paths[%d].paint.color", pi), Keyframe: -1, TimeIndex: -1,
				Err: fmt.Errorf("expected [r,g,b,a]")}
		}
		color := RGBA{}
		for i, v := range rp.Paint.Color {
			if v < 0 || v > 255 {
				return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, PathID: pathID, Field: fmt.Sprintf("paths[%d].paint.color[%d]", pi, i), Keyframe: -1, TimeIndex: -1}
			}
		}
		color.R, color.G, color.B, color.A = uint8(rp.Paint.Color[0]), uint8(rp.Paint.Color[1]), uint8(rp.Paint.Color[2]), uint8(rp.Paint.Color[3])
		width := int64(0)
		join := JoinRound
		if rp.Paint.Stroke != nil {
			if rp.Paint.Stroke.Width != nil {
				width = *rp.Paint.Stroke.Width
			}
			if rp.Paint.Stroke.Join != nil {
				join = JoinMode(*rp.Paint.Stroke.Join)
			}
		}
		blend := BlendSourceOver
		if rp.Paint.Blend != nil {
			blend = BlendMode(*rp.Paint.Blend)
		}
		if len(rp.Tracks) == 0 {
			return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, PathID: pathID, Field: fmt.Sprintf("paths[%d].tracks", pi), Keyframe: -1, TimeIndex: -1}
		}
		path := Path{ID: pathID, Paint: Paint{Fill: fill, StrokeWidth: width, Join: join, Color: color, Opacity: int(opacity), Blend: blend}}
		for ki, rt := range rp.Tracks {
			ms, err := requiredInt(rt.At, fmt.Sprintf("paths[%d].tracks[%d].at", pi, ki), 2)
			if err != nil {
				if e, ok := err.(*Error); ok {
					e.PathID, e.Keyframe = pathID, ki
				}
				return nil, err
			}
			if ms < 0 {
				return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, Field: fmt.Sprintf("paths[%d].tracks[%d].at", pi, ki), PathID: pathID, Keyframe: ki, TimeIndex: -1}
			}
			ticks, convErr := MillisToTicks(ms, rounding)
			if convErr != nil {
				return nil, timeConversionError(convErr, CodeOverflow, fmt.Sprintf("paths[%d].tracks[%d].at", pi, ki), 2, pathID, ki, -1)
			}
			contour, err := decodeV2Contour(rt, fmt.Sprintf("paths[%d].tracks[%d]", pi, ki), pathID, ki)
			if err != nil {
				return nil, err
			}
			path.Keyframes = append(path.Keyframes, Keyframe{Time: ticks, Contour: contour})
		}
		scene.Paths = append(scene.Paths, path)
	}
	return scene, nil
}

func v2PointValue(p *v2Point, field, pathID string, keyframe int) (Point, error) {
	if p == nil {
		return Point{}, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, Field: field, PathID: pathID, Keyframe: keyframe, TimeIndex: -1,
			Err: fmt.Errorf("required point is missing")}
	}
	x, err := requiredInt(p.X, field+".x", 2)
	if err != nil {
		return Point{}, err
	}
	y, err := requiredInt(p.Y, field+".y", 2)
	if err != nil {
		return Point{}, err
	}
	return Point{X: x, Y: y}, nil
}

func v2DeltaValue(p *v2Delta, field string, base Point, pathID string, keyframe int) (Point, error) {
	if p == nil {
		return Point{}, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, Field: field, PathID: pathID, Keyframe: keyframe, TimeIndex: -1,
			Err: fmt.Errorf("required delta is missing")}
	}
	dx, err := requiredInt(p.DX, field+".dx", 2)
	if err != nil {
		return Point{}, err
	}
	dy, err := requiredInt(p.DY, field+".dy", 2)
	if err != nil {
		return Point{}, err
	}
	x, ok := checkedAdd(base.X, dx)
	if !ok {
		return Point{}, &Error{Code: CodeOverflow, Op: "decode", SourceVersion: 2, Field: field + ".dx", PathID: pathID, Keyframe: keyframe, TimeIndex: -1,
			Err: fmt.Errorf("absolute coordinate overflow")}
	}
	y, ok := checkedAdd(base.Y, dy)
	if !ok {
		return Point{}, &Error{Code: CodeOverflow, Op: "decode", SourceVersion: 2, Field: field + ".dy", PathID: pathID, Keyframe: keyframe, TimeIndex: -1,
			Err: fmt.Errorf("absolute coordinate overflow")}
	}
	return Point{X: x, Y: y}, nil
}

func decodeV2Contour(raw v2Track, prefix, pathID string, keyframe int) (Contour, error) {
	if raw.Shape == nil {
		return Contour{}, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, Field: prefix + ".shape", PathID: pathID, Keyframe: keyframe, TimeIndex: -1,
			Err: fmt.Errorf("required object is missing")}
	}
	base, err := v2PointValue(raw.Shape.Base, prefix+".shape.base", pathID, keyframe)
	if err != nil {
		return Contour{}, err
	}
	if len(raw.Shape.Segments) == 0 {
		return Contour{}, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, Field: prefix + ".shape.segments", PathID: pathID, Keyframe: keyframe, TimeIndex: -1,
			Err: fmt.Errorf("required list is empty")}
	}
	contour := Contour{Start: base}
	for si, rs := range raw.Shape.Segments {
		kind, err := requiredString(rs.Kind, fmt.Sprintf("%s.shape.segments[%d].kind", prefix, si), 2)
		if err != nil {
			return Contour{}, err
		}
		c1, err := v2DeltaValue(rs.C1, fmt.Sprintf("%s.shape.segments[%d].c1", prefix, si), base, pathID, keyframe)
		if err != nil {
			return Contour{}, err
		}
		to, err := v2DeltaValue(rs.To, fmt.Sprintf("%s.shape.segments[%d].to", prefix, si), base, pathID, keyframe)
		if err != nil {
			return Contour{}, err
		}
		seg := Segment{Kind: SegmentKind(kind), C1: c1, To: to}
		if seg.Kind == SegmentCubic {
			c2, err := v2DeltaValue(rs.C2, fmt.Sprintf("%s.shape.segments[%d].c2", prefix, si), base, pathID, keyframe)
			if err != nil {
				return Contour{}, err
			}
			seg.C2 = c2
		} else if seg.Kind != SegmentQuad {
			return Contour{}, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 2, Field: fmt.Sprintf("%s.shape.segments[%d].kind", prefix, si), PathID: pathID, Keyframe: keyframe, TimeIndex: -1,
				Err: fmt.Errorf("unknown segment kind %q", kind)}
		}
		contour.Segments = append(contour.Segments, seg)
	}
	return contour, nil
}

// ---------------------------------------------------------------------------
// v3: nested timeline, frame time, absolute points and command spelling.
// ---------------------------------------------------------------------------

type v3Point struct {
	X *int64 `json:"x"`
	Y *int64 `json:"y"`
}

type v3Command struct {
	Op *string  `json:"op"`
	C1 *v3Point `json:"c1"`
	C2 *v3Point `json:"c2"`
	To *v3Point `json:"to"`
}

type v3Geometry struct {
	Start    *v3Point    `json:"start"`
	Commands []v3Command `json:"commands"`
}

type v3Key struct {
	Tick     *int64      `json:"tick"`
	Geometry *v3Geometry `json:"geometry"`
}

type v3Color struct {
	R *int64 `json:"r"`
	G *int64 `json:"g"`
	B *int64 `json:"b"`
	A *int64 `json:"a"`
}

type v3Render struct {
	Fill        *bool    `json:"fill"`
	StrokeWidth *int64   `json:"strokeWidth"`
	Join        *string  `json:"join"`
	Color       *v3Color `json:"color"`
	Opacity     *int64   `json:"opacity"`
	BlendMode   *string  `json:"blendMode"`
}

type v3Path struct {
	ID     *string   `json:"id"`
	Render *v3Render `json:"render"`
	Keys   []v3Key   `json:"keys"`
}

type v3Timeline struct {
	Unit     *string `json:"unit"`
	FPS      *int64  `json:"fps"`
	Rounding *string `json:"rounding"`
	Samples  []int64 `json:"samples"`
}

type v3Scene struct {
	SchemaVersion *int        `json:"schemaVersion"`
	SceneID       *string     `json:"sceneId"`
	Timeline      *v3Timeline `json:"timeline"`
	Paths         []v3Path    `json:"paths"`
}

func loadV3(data []byte) (*Scene, error) {
	var raw v3Scene
	if err := decodeVersion(data, &raw, 3); err != nil {
		return nil, err
	}
	sceneID, err := requiredString(raw.SceneID, "sceneId", 3)
	if err != nil {
		return nil, err
	}
	if raw.Timeline == nil {
		return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 3, Field: "timeline", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("required object is missing")}
	}
	unit, err := requiredString(raw.Timeline.Unit, "timeline.unit", 3)
	if err != nil {
		return nil, err
	}
	if unit != "frame" && unit != "frames" {
		return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 3, Field: "timeline.unit", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("v3 supports frame time only, got %q", unit)}
	}
	fps, err := requiredInt(raw.Timeline.FPS, "timeline.fps", 3)
	if err != nil {
		return nil, err
	}
	rounding, err := parseRoundingField(raw.Timeline.Rounding, "timeline.rounding", 3)
	if err != nil {
		return nil, err
	}
	if len(raw.Timeline.Samples) == 0 {
		return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 3, Field: "timeline.samples", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("required list is empty")}
	}
	samples := make([]int64, len(raw.Timeline.Samples))
	for i, frame := range raw.Timeline.Samples {
		if frame < 0 {
			return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 3, Field: fmt.Sprintf("timeline.samples[%d]", i), Keyframe: -1, TimeIndex: i}
		}
		ticks, convErr := FrameToTicks(frame, fps, rounding)
		if convErr != nil {
			return nil, timeConversionError(convErr, CodeOverflow, fmt.Sprintf("timeline.samples[%d]", i), 3, "", -1, i)
		}
		samples[i] = ticks
	}
	if !sortedUniqueTimes(samples) {
		return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 3, Field: "timeline.samples", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("sample times must be strictly increasing")}
	}
	scene := &Scene{ID: sceneID, SourceVersion: 3, FPS: fps, Rounding: rounding, SampleTimes: samples}
	for pi, rp := range raw.Paths {
		pathID, err := requiredString(rp.ID, fmt.Sprintf("paths[%d].id", pi), 3)
		if err != nil {
			return nil, err
		}
		if rp.Render == nil {
			return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 3, PathID: pathID, Field: fmt.Sprintf("paths[%d].render", pi), Keyframe: -1, TimeIndex: -1,
				Err: fmt.Errorf("required object is missing")}
		}
		fill, err := requiredBool(rp.Render.Fill, fmt.Sprintf("paths[%d].render.fill", pi), 3)
		if err != nil {
			return nil, err
		}
		opacity, err := requiredInt(rp.Render.Opacity, fmt.Sprintf("paths[%d].render.opacity", pi), 3)
		if err != nil {
			return nil, err
		}
		if rp.Render.Color == nil {
			return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 3, PathID: pathID, Field: fmt.Sprintf("paths[%d].render.color", pi), Keyframe: -1, TimeIndex: -1}
		}
		channels := []*int64{rp.Render.Color.R, rp.Render.Color.G, rp.Render.Color.B, rp.Render.Color.A}
		var color RGBA
		vals := []*uint8{&color.R, &color.G, &color.B, &color.A}
		for i, channel := range channels {
			v, err := requiredInt(channel, fmt.Sprintf("paths[%d].render.color.%s", pi, []string{"r", "g", "b", "a"}[i]), 3)
			if err != nil {
				return nil, err
			}
			if v < 0 || v > 255 {
				return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 3, PathID: pathID, Field: fmt.Sprintf("paths[%d].render.color.%s", pi, []string{"r", "g", "b", "a"}[i]), Keyframe: -1, TimeIndex: -1,
					Err: fmt.Errorf("must be in [0,255]")}
			}
			*vals[i] = uint8(v)
		}
		width := int64(0)
		if rp.Render.StrokeWidth != nil {
			width = *rp.Render.StrokeWidth
		}
		join := JoinRound
		if rp.Render.Join != nil {
			join = JoinMode(*rp.Render.Join)
		}
		blend := BlendNormal
		if rp.Render.BlendMode != nil {
			blend = BlendMode(*rp.Render.BlendMode)
		}
		if len(rp.Keys) == 0 {
			return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 3, PathID: pathID, Field: fmt.Sprintf("paths[%d].keys", pi), Keyframe: -1, TimeIndex: -1}
		}
		path := Path{ID: pathID, Paint: Paint{Fill: fill, StrokeWidth: width, Join: join, Color: color, Opacity: int(opacity), Blend: blend}}
		for ki, rk := range rp.Keys {
			frame, err := requiredInt(rk.Tick, fmt.Sprintf("paths[%d].keys[%d].tick", pi, ki), 3)
			if err != nil {
				if e, ok := err.(*Error); ok {
					e.PathID, e.Keyframe = pathID, ki
				}
				return nil, err
			}
			if frame < 0 {
				return nil, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 3, Field: fmt.Sprintf("paths[%d].keys[%d].tick", pi, ki), PathID: pathID, Keyframe: ki, TimeIndex: -1}
			}
			ticks, convErr := FrameToTicks(frame, fps, rounding)
			if convErr != nil {
				return nil, timeConversionError(convErr, CodeOverflow, fmt.Sprintf("paths[%d].keys[%d].tick", pi, ki), 3, pathID, ki, -1)
			}
			contour, err := decodeV3Contour(rk, fmt.Sprintf("paths[%d].keys[%d]", pi, ki), pathID, ki)
			if err != nil {
				return nil, err
			}
			path.Keyframes = append(path.Keyframes, Keyframe{Time: ticks, Contour: contour})
		}
		scene.Paths = append(scene.Paths, path)
	}
	return scene, nil
}

func v3PointValue(p *v3Point, field, pathID string, keyframe int) (Point, error) {
	if p == nil {
		return Point{}, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 3, Field: field, PathID: pathID, Keyframe: keyframe, TimeIndex: -1,
			Err: fmt.Errorf("required point is missing")}
	}
	x, err := requiredInt(p.X, field+".x", 3)
	if err != nil {
		return Point{}, err
	}
	y, err := requiredInt(p.Y, field+".y", 3)
	if err != nil {
		return Point{}, err
	}
	return Point{X: x, Y: y}, nil
}

func decodeV3Contour(raw v3Key, prefix, pathID string, keyframe int) (Contour, error) {
	if raw.Geometry == nil {
		return Contour{}, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 3, Field: prefix + ".geometry", PathID: pathID, Keyframe: keyframe, TimeIndex: -1}
	}
	start, err := v3PointValue(raw.Geometry.Start, prefix+".geometry.start", pathID, keyframe)
	if err != nil {
		return Contour{}, err
	}
	if len(raw.Geometry.Commands) == 0 {
		return Contour{}, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 3, Field: prefix + ".geometry.commands", PathID: pathID, Keyframe: keyframe, TimeIndex: -1}
	}
	contour := Contour{Start: start}
	for si, rc := range raw.Geometry.Commands {
		op, err := requiredString(rc.Op, fmt.Sprintf("%s.geometry.commands[%d].op", prefix, si), 3)
		if err != nil {
			return Contour{}, err
		}
		kind := SegmentKind("")
		switch op {
		case "Q", "quad":
			kind = SegmentQuad
		case "C", "cubic":
			kind = SegmentCubic
		default:
			return Contour{}, &Error{Code: CodeSchema, Op: "decode", SourceVersion: 3, Field: fmt.Sprintf("%s.geometry.commands[%d].op", prefix, si), PathID: pathID, Keyframe: keyframe, TimeIndex: -1,
				Err: fmt.Errorf("unknown command %q", op)}
		}
		c1, err := v3PointValue(rc.C1, fmt.Sprintf("%s.geometry.commands[%d].c1", prefix, si), pathID, keyframe)
		if err != nil {
			return Contour{}, err
		}
		to, err := v3PointValue(rc.To, fmt.Sprintf("%s.geometry.commands[%d].to", prefix, si), pathID, keyframe)
		if err != nil {
			return Contour{}, err
		}
		seg := Segment{Kind: kind, C1: c1, To: to}
		if kind == SegmentCubic {
			c2, err := v3PointValue(rc.C2, fmt.Sprintf("%s.geometry.commands[%d].c2", prefix, si), pathID, keyframe)
			if err != nil {
				return Contour{}, err
			}
			seg.C2 = c2
		}
		contour.Segments = append(contour.Segments, seg)
	}
	return contour, nil
}
