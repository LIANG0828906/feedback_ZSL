package vectorcore

import (
	"encoding/json"
	"fmt"
)

// EncodeScene emits the internal integer scene using one of the supported
// public schema versions. Version conversion is deliberately lossless for
// values representable in the target time unit.
func EncodeScene(scene *Scene, version int) ([]byte, error) {
	if err := scene.Validate(); err != nil {
		return nil, err
	}
	switch version {
	case 1:
		raw, err := encodeV1(scene)
		if err != nil {
			return nil, err
		}
		return json.MarshalIndent(raw, "", "  ")
	case 2:
		raw, err := encodeV2(scene)
		if err != nil {
			return nil, err
		}
		return json.MarshalIndent(raw, "", "  ")
	case 3:
		raw, err := encodeV3(scene)
		if err != nil {
			return nil, err
		}
		return json.MarshalIndent(raw, "", "  ")
	default:
		return nil, &Error{Code: CodeVersion, Op: "encodeScene", SourceVersion: version, Field: "schemaVersion", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("unsupported schema version %d", version)}
	}
}

type encV1Point struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

func encV1PointValue(p Point) encV1Point { return encV1Point{X: p.X, Y: p.Y} }

type encV1Segment struct {
	Kind string      `json:"kind"`
	C1   encV1Point  `json:"c1"`
	C2   *encV1Point `json:"c2,omitempty"`
	To   encV1Point  `json:"to"`
}

type encV1Frame struct {
	Frame    int64          `json:"frame"`
	Start    encV1Point     `json:"start"`
	Segments []encV1Segment `json:"segments"`
}

type encV1Path struct {
	PathID      string       `json:"pathId"`
	Fill        bool         `json:"fill"`
	StrokeWidth int64        `json:"strokeWidth"`
	StrokeJoin  string       `json:"strokeJoin,omitempty"`
	Color       string       `json:"color"`
	Opacity     int          `json:"opacity"`
	BlendMode   string       `json:"blendMode,omitempty"`
	Frames      []encV1Frame `json:"frames"`
}

type encV1Scene struct {
	SchemaVersion int         `json:"schemaVersion"`
	SceneID       string      `json:"sceneId"`
	FrameRate     int64       `json:"frameRate"`
	Rounding      Rounding    `json:"rounding"`
	SampleFrames  []int64     `json:"sampleFrames"`
	Paths         []encV1Path `json:"paths"`
}

func encodeV1(scene *Scene) (*encV1Scene, error) {
	out := &encV1Scene{SchemaVersion: 1, SceneID: scene.ID, FrameRate: scene.FPS, Rounding: scene.Rounding}
	for i, t := range scene.SampleTimes {
		frame, err := TicksToFrame(t, scene.FPS, scene.Rounding)
		if err != nil {
			return nil, &Error{Code: CodeOverflow, Op: "encode.v1", SourceVersion: 1, Field: fmt.Sprintf("sampleFrames[%d]", i), Keyframe: -1, TimeIndex: i, Err: err}
		}
		out.SampleFrames = append(out.SampleFrames, frame)
	}
	for pi, p := range scene.Paths {
		ep := encV1Path{PathID: p.ID, Fill: p.Paint.Fill, StrokeWidth: p.Paint.StrokeWidth,
			StrokeJoin: string(p.Paint.Join), Color: p.Paint.Color.Hex(), Opacity: p.Paint.Opacity, BlendMode: string(p.Paint.Blend)}
		for ki, k := range p.Keyframes {
			frame, err := TicksToFrame(k.Time, scene.FPS, scene.Rounding)
			if err != nil {
				return nil, &Error{Code: CodeOverflow, Op: "encode.v1", SourceVersion: 1, PathID: p.ID, Field: fmt.Sprintf("paths[%d].frames[%d].frame", pi, ki), Keyframe: ki, TimeIndex: -1, Err: err}
			}
			ef := encV1Frame{Frame: frame, Start: encV1PointValue(k.Contour.Start)}
			for _, seg := range k.Contour.Segments {
				es := encV1Segment{Kind: string(seg.Kind), C1: encV1PointValue(seg.C1), To: encV1PointValue(seg.To)}
				if seg.Kind == SegmentCubic {
					c2 := encV1PointValue(seg.C2)
					es.C2 = &c2
				}
				ef.Segments = append(ef.Segments, es)
			}
			ep.Frames = append(ep.Frames, ef)
		}
		out.Paths = append(out.Paths, ep)
	}
	return out, nil
}

type encV2Point struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type encV2Delta struct {
	DX int64 `json:"dx"`
	DY int64 `json:"dy"`
}

type encV2Segment struct {
	Kind string      `json:"kind"`
	C1   encV2Delta  `json:"c1"`
	C2   *encV2Delta `json:"c2,omitempty"`
	To   encV2Delta  `json:"to"`
}

type encV2Shape struct {
	Base     encV2Point     `json:"base"`
	Segments []encV2Segment `json:"segments"`
}

type encV2Track struct {
	At    int64      `json:"at"`
	Shape encV2Shape `json:"shape"`
}

type encV2Stroke struct {
	Width int64  `json:"width"`
	Join  string `json:"join"`
}

type encV2Paint struct {
	Fill    bool         `json:"fill"`
	Color   []int64      `json:"color"`
	Opacity int          `json:"opacity"`
	Blend   string       `json:"blend"`
	Stroke  *encV2Stroke `json:"stroke,omitempty"`
}

type encV2Path struct {
	PathID string       `json:"path_id"`
	Paint  encV2Paint   `json:"paint"`
	Tracks []encV2Track `json:"tracks"`
}

type encV2Timeline struct {
	Unit     string   `json:"unit"`
	FPS      int64    `json:"fps"`
	Rounding Rounding `json:"rounding"`
	Samples  []int64  `json:"samples"`
}

type encV2Scene struct {
	SchemaVersion int           `json:"schema_version"`
	ID            string        `json:"id"`
	Timeline      encV2Timeline `json:"timeline"`
	Paths         []encV2Path   `json:"paths"`
}

func encodeV2(scene *Scene) (*encV2Scene, error) {
	out := &encV2Scene{SchemaVersion: 2, ID: scene.ID, Timeline: encV2Timeline{Unit: "ms", FPS: scene.FPS, Rounding: scene.Rounding}}
	for i, t := range scene.SampleTimes {
		ms, err := TicksToMillis(t, scene.Rounding)
		if err != nil {
			return nil, &Error{Code: CodeOverflow, Op: "encode.v2", SourceVersion: 2, Field: fmt.Sprintf("timeline.samples[%d]", i), Keyframe: -1, TimeIndex: i, Err: err}
		}
		out.Timeline.Samples = append(out.Timeline.Samples, ms)
	}
	for pi, p := range scene.Paths {
		ep := encV2Path{PathID: p.ID, Paint: encV2Paint{
			Fill: p.Paint.Fill, Color: []int64{int64(p.Paint.Color.R), int64(p.Paint.Color.G), int64(p.Paint.Color.B), int64(p.Paint.Color.A)},
			Opacity: p.Paint.Opacity, Blend: string(p.Paint.Blend),
		}}
		if p.Paint.StrokeWidth > 0 {
			ep.Paint.Stroke = &encV2Stroke{Width: p.Paint.StrokeWidth, Join: string(p.Paint.Join)}
		}
		for ki, k := range p.Keyframes {
			ms, err := TicksToMillis(k.Time, scene.Rounding)
			if err != nil {
				return nil, &Error{Code: CodeOverflow, Op: "encode.v2", SourceVersion: 2, PathID: p.ID, Field: fmt.Sprintf("paths[%d].tracks[%d].at", pi, ki), Keyframe: ki, TimeIndex: -1, Err: err}
			}
			shape := encV2Shape{Base: encV2Point{X: k.Contour.Start.X, Y: k.Contour.Start.Y}}
			for si, seg := range k.Contour.Segments {
				c1, err := relativePoint(seg.C1, k.Contour.Start)
				if err != nil {
					return nil, &Error{Code: CodeOverflow, Op: "encode.v2", SourceVersion: 2, PathID: p.ID, Field: fmt.Sprintf("paths[%d].tracks[%d].shape.segments[%d].c1", pi, ki, si), Keyframe: ki, TimeIndex: -1, Err: err}
				}
				to, err := relativePoint(seg.To, k.Contour.Start)
				if err != nil {
					return nil, &Error{Code: CodeOverflow, Op: "encode.v2", SourceVersion: 2, PathID: p.ID, Field: fmt.Sprintf("paths[%d].tracks[%d].shape.segments[%d].to", pi, ki, si), Keyframe: ki, TimeIndex: -1, Err: err}
				}
				es := encV2Segment{Kind: string(seg.Kind), C1: c1, To: to}
				if seg.Kind == SegmentCubic {
					c2, err := relativePoint(seg.C2, k.Contour.Start)
					if err != nil {
						return nil, &Error{Code: CodeOverflow, Op: "encode.v2", SourceVersion: 2, PathID: p.ID, Field: fmt.Sprintf("paths[%d].tracks[%d].shape.segments[%d].c2", pi, ki, si), Keyframe: ki, TimeIndex: -1, Err: err}
					}
					es.C2 = &c2
				}
				shape.Segments = append(shape.Segments, es)
			}
			ep.Tracks = append(ep.Tracks, encV2Track{At: ms, Shape: shape})
		}
		out.Paths = append(out.Paths, ep)
	}
	return out, nil
}

func relativePoint(p, base Point) (encV2Delta, error) {
	x, ok := checkedSub(p.X, base.X)
	if !ok {
		return encV2Delta{}, fmt.Errorf("x coordinate overflow")
	}
	y, ok := checkedSub(p.Y, base.Y)
	if !ok {
		return encV2Delta{}, fmt.Errorf("y coordinate overflow")
	}
	return encV2Delta{DX: x, DY: y}, nil
}

type encV3Point struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type encV3Command struct {
	Op string      `json:"op"`
	C1 encV3Point  `json:"c1"`
	C2 *encV3Point `json:"c2,omitempty"`
	To encV3Point  `json:"to"`
}

type encV3Geometry struct {
	Start    encV3Point     `json:"start"`
	Commands []encV3Command `json:"commands"`
}

type encV3Key struct {
	Tick     int64         `json:"tick"`
	Geometry encV3Geometry `json:"geometry"`
}

type encV3Color struct {
	R int `json:"r"`
	G int `json:"g"`
	B int `json:"b"`
	A int `json:"a"`
}

type encV3Render struct {
	Fill        bool       `json:"fill"`
	StrokeWidth int64      `json:"strokeWidth"`
	Join        string     `json:"join,omitempty"`
	Color       encV3Color `json:"color"`
	Opacity     int        `json:"opacity"`
	BlendMode   string     `json:"blendMode,omitempty"`
}

type encV3Path struct {
	ID     string      `json:"id"`
	Render encV3Render `json:"render"`
	Keys   []encV3Key  `json:"keys"`
}

type encV3Timeline struct {
	Unit     string   `json:"unit"`
	FPS      int64    `json:"fps"`
	Rounding Rounding `json:"rounding"`
	Samples  []int64  `json:"samples"`
}

type encV3Scene struct {
	SchemaVersion int           `json:"schemaVersion"`
	SceneID       string        `json:"sceneId"`
	Timeline      encV3Timeline `json:"timeline"`
	Paths         []encV3Path   `json:"paths"`
}

func encodeV3(scene *Scene) (*encV3Scene, error) {
	out := &encV3Scene{SchemaVersion: 3, SceneID: scene.ID, Timeline: encV3Timeline{Unit: "frame", FPS: scene.FPS, Rounding: scene.Rounding}}
	for i, t := range scene.SampleTimes {
		frame, err := TicksToFrame(t, scene.FPS, scene.Rounding)
		if err != nil {
			return nil, &Error{Code: CodeOverflow, Op: "encode.v3", SourceVersion: 3, Field: fmt.Sprintf("timeline.samples[%d]", i), Keyframe: -1, TimeIndex: i, Err: err}
		}
		out.Timeline.Samples = append(out.Timeline.Samples, frame)
	}
	for pi, p := range scene.Paths {
		ep := encV3Path{ID: p.ID, Render: encV3Render{
			Fill: p.Paint.Fill, StrokeWidth: p.Paint.StrokeWidth, Join: string(p.Paint.Join),
			Color:   encV3Color{R: int(p.Paint.Color.R), G: int(p.Paint.Color.G), B: int(p.Paint.Color.B), A: int(p.Paint.Color.A)},
			Opacity: p.Paint.Opacity, BlendMode: string(p.Paint.Blend),
		}}
		for ki, k := range p.Keyframes {
			frame, err := TicksToFrame(k.Time, scene.FPS, scene.Rounding)
			if err != nil {
				return nil, &Error{Code: CodeOverflow, Op: "encode.v3", SourceVersion: 3, PathID: p.ID, Field: fmt.Sprintf("paths[%d].keys[%d].tick", pi, ki), Keyframe: ki, TimeIndex: -1, Err: err}
			}
			geom := encV3Geometry{Start: encV3Point{X: k.Contour.Start.X, Y: k.Contour.Start.Y}}
			for _, seg := range k.Contour.Segments {
				cmd := encV3Command{C1: encV3Point{X: seg.C1.X, Y: seg.C1.Y}, To: encV3Point{X: seg.To.X, Y: seg.To.Y}}
				if seg.Kind == SegmentQuad {
					cmd.Op = "Q"
				} else {
					cmd.Op = "C"
					c2 := encV3Point{X: seg.C2.X, Y: seg.C2.Y}
					cmd.C2 = &c2
				}
				geom.Commands = append(geom.Commands, cmd)
			}
			ep.Keys = append(ep.Keys, encV3Key{Tick: frame, Geometry: geom})
		}
		out.Paths = append(out.Paths, ep)
	}
	return out, nil
}
