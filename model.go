package silhouette

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	FixedScale int64 = 1_000_000

	VersionLegacy   = 1
	VersionRelative = 2
	VersionCurrent  = 3

	AlgorithmVersion    = "silhouette-sample/1.0.0"
	GeometryVersion     = "tile-projection/1.0.0"
	CurveMetaVersion    = "curve-meta/1.0.0"
	BoundaryRule        = "min-tile-id-half-open"
	MinTolerance        = int64(1)
	MaxSubdivisionDepth = 12
)

var (
	Epsilon = int64(0)
)

type Vec2 struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type Vec3 struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	Z int64 `json:"z"`
}

func (v Vec2) Add(o Vec2) Vec2 { return Vec2{X: v.X + o.X, Y: v.Y + o.Y} }
func (v Vec2) Sub(o Vec2) Vec2 { return Vec2{X: v.X - o.X, Y: v.Y - o.Y} }

func (v Vec2) String() string {
	return fmt.Sprintf("(%d,%d)", v.X, v.Y)
}

type ControlGrid struct {
	Name     string `json:"name"`
	Rows     int    `json:"rows"`
	Cols     int    `json:"cols"`
	Controls []Vec3 `json:"controls"`
}

type CurveSegment struct {
	ID              string  `json:"id"`
	Weight          int64   `json:"weight"`
	Controls        [4]Vec2 `json:"controls"`
	Closed          bool    `json:"closed"`
	SurfaceRefs     []int   `json:"surface_refs"`
	MetadataVersion string  `json:"metadata_version"`
}

type Tile struct {
	ID    string `json:"id"`
	Min   Vec2   `json:"min"`
	Max   Vec2   `json:"max"`
	Order int    `json:"order"`
}

type Budget struct {
	MaxVertices  int    `json:"max_vertices"`
	ParamVersion string `json:"param_version"`
}

type Tolerance struct {
	Value        int64  `json:"value"`
	Unit         string `json:"unit"`
	ParamVersion string `json:"param_version"`
}

type Scene struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Seed          int64             `json:"seed"`
	SourceVersion int               `json:"source_version"`
	Surface       ControlGrid       `json:"surface"`
	Segments      []CurveSegment    `json:"segments"`
	Tiles         []Tile            `json:"tiles"`
	Budget        Budget            `json:"budget"`
	Tolerance     Tolerance         `json:"tolerance"`
	Traces        []ConversionTrace `json:"traces"`
}

func (s *Scene) Clone() *Scene {
	if s == nil {
		return nil
	}
	out := *s
	out.Surface.Controls = append([]Vec3(nil), s.Surface.Controls...)
	out.Segments = make([]CurveSegment, len(s.Segments))
	for i, seg := range s.Segments {
		out.Segments[i] = seg
		out.Segments[i].SurfaceRefs = append([]int(nil), seg.SurfaceRefs...)
	}
	out.Tiles = append([]Tile(nil), s.Tiles...)
	out.Traces = append([]ConversionTrace(nil), s.Traces...)
	return &out
}

func (s *Scene) Normalize() {
	sort.SliceStable(s.Segments, func(i, j int) bool { return s.Segments[i].ID < s.Segments[j].ID })
	sort.SliceStable(s.Tiles, func(i, j int) bool {
		if s.Tiles[i].Order == s.Tiles[j].Order {
			return s.Tiles[i].ID < s.Tiles[j].ID
		}
		return s.Tiles[i].Order < s.Tiles[j].Order
	})
	for i := range s.Tiles {
		if s.Tiles[i].Order == 0 {
			s.Tiles[i].Order = i
		}
	}
	for i := range s.Segments {
		if s.Segments[i].MetadataVersion == "" {
			s.Segments[i].MetadataVersion = CurveMetaVersion
		}
		sort.Ints(s.Segments[i].SurfaceRefs)
	}
	SortTraces(s.Traces)
}

func (s *Scene) Segment(id string) (*CurveSegment, bool) {
	for i := range s.Segments {
		if s.Segments[i].ID == id {
			return &s.Segments[i], true
		}
	}
	return nil, false
}

func (s *Scene) Tile(id string) (*Tile, bool) {
	for i := range s.Tiles {
		if s.Tiles[i].ID == id {
			return &s.Tiles[i], true
		}
	}
	return nil, false
}

func (s *Scene) Validate() error {
	if s == nil {
		return NewDomainError(CodeSchema, "$", "scene is nil")
	}
	if s.SourceVersion < 1 || s.SourceVersion > 3 {
		return NewDomainError(CodeSchema, "$.schema_version", "unknown schema version")
	}
	if s.Surface.Rows <= 0 || s.Surface.Cols <= 0 {
		return NewDomainError(CodeSchema, "$.surface", "control grid dimensions must be positive")
	}
	if len(s.Surface.Controls) != s.Surface.Rows*s.Surface.Cols {
		return NewDomainError(CodeSchema, "$.surface.controls", "control count does not match rows*cols")
	}
	if len(s.Segments) == 0 {
		return NewDomainError(CodeSchema, "$.segments", "at least one outline segment is required")
	}
	if len(s.Tiles) == 0 {
		return NewDomainError(CodeSchema, "$.tiles", "at least one tile is required")
	}
	if s.Budget.MaxVertices <= 0 {
		return NewDomainError(CodeSchema, "$.budget.max_vertices", "sample budget must be positive")
	}
	if s.Tolerance.Value < MinTolerance {
		return (&DomainError{Code: CodeTolerance, Path: "$.tolerance", Message: "tolerance is below precision floor"}).
			WithDetail("segment_id", "scene").
			WithDetail("measured", strconv.FormatInt(s.Tolerance.Value, 10)).
			WithDetail("minimum", strconv.FormatInt(MinTolerance, 10))
	}
	ids := make(map[string]struct{}, len(s.Segments))
	for i := range s.Segments {
		seg := &s.Segments[i]
		if strings.TrimSpace(seg.ID) == "" {
			return NewDomainError(CodeSchema, "$.segments["+strconv.Itoa(i)+"].id", "segment id is required")
		}
		if _, ok := ids[seg.ID]; ok {
			return NewDomainError(CodeSchema, "$.segments["+strconv.Itoa(i)+"].id", "duplicate segment id")
		}
		ids[seg.ID] = struct{}{}
		if seg.Weight <= 0 {
			return NewDomainError(CodeSchema, "$.segments["+strconv.Itoa(i)+"].weight", "segment weight must be positive")
		}
	}
	tileIDs := make(map[string]struct{}, len(s.Tiles))
	for i := range s.Tiles {
		t := &s.Tiles[i]
		if strings.TrimSpace(t.ID) == "" {
			return NewDomainError(CodeSchema, "$.tiles["+strconv.Itoa(i)+"].id", "tile id is required")
		}
		if _, ok := tileIDs[t.ID]; ok {
			return NewDomainError(CodeSchema, "$.tiles["+strconv.Itoa(i)+"].id", "duplicate tile id")
		}
		tileIDs[t.ID] = struct{}{}
		if t.Max.X < t.Min.X || t.Max.Y < t.Min.Y {
			return NewDomainError(CodeSchema, "$.tiles["+strconv.Itoa(i)+"]", "tile bounds are inverted")
		}
	}
	return nil
}
