package cad

import (
	"math"
	"sort"
)

// Length is an exact canonical CAD length in nanometres. Schema boundaries may
// use millimetres or metres; conversion through Length keeps unit round trips
// exact for ordinary decimal CAD coordinates.
type Length int64

func Millimetre(v float64) Length {
	return Length(math.Round(v * 1_000_000))
}

func Metre(v float64) Length {
	return Length(math.Round(v * 1_000_000_000))
}

func (l Length) Nanometres() int64 { return int64(l) }
func (l Length) Metres() float64   { return float64(l) / 1_000_000_000 }
func (l Length) Millimetres() float64 {
	return float64(l) / 1_000_000
}

type Vec2 struct {
	U float64 `json:"u"`
	V float64 `json:"v"`
}

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type Point3 struct {
	X Length `json:"x_nm"`
	Y Length `json:"y_nm"`
	Z Length `json:"z_nm"`
}

func (p Point3) Metres() Vec3 {
	return Vec3{X: p.X.Metres(), Y: p.Y.Metres(), Z: p.Z.Metres()}
}

type RGB struct {
	R float64 `json:"r"`
	G float64 `json:"g"`
	B float64 `json:"b"`
}

type Material struct {
	Name      string  `json:"name"`
	Color     RGB     `json:"color"`
	Metallic  float64 `json:"metallic"`
	Roughness float64 `json:"roughness"`
}

type Subdivision struct {
	Density              int     `json:"density"`
	CurvatureSensitivity float64 `json:"curvature_sensitivity"`
	MaxDensity           int     `json:"max_density"`
	Tolerance            float64 `json:"tolerance,omitempty"`
}

func DefaultSubdivision() Subdivision {
	return Subdivision{Density: 8, CurvatureSensitivity: 0.5, MaxDensity: 32, Tolerance: 0}
}

type ClipBoundary struct {
	Loops [][]Vec2 `json:"loops"`
}

func (c ClipBoundary) MainLoop() []Vec2 {
	if len(c.Loops) == 0 {
		return nil
	}
	return c.Loops[0]
}

type Patch struct {
	ID            string       `json:"id"`
	SourceSchema  string       `json:"source_schema,omitempty"`
	DegreeU       int          `json:"degree_u"`
	DegreeV       int          `json:"degree_v"`
	ControlPoints [][]Point3   `json:"control_points"`
	Weights       [][]float64  `json:"weights"`
	KnotsU        []float64    `json:"knots_u"`
	KnotsV        []float64    `json:"knots_v"`
	Clip          ClipBoundary `json:"clip"`
	Material      *Material    `json:"material,omitempty"`
	Subdivision   *Subdivision `json:"subdivision,omitempty"`
}

type Scene struct {
	Schema        string   `json:"schema"`
	SchemaVersion int      `json:"schema_version"`
	SceneID       string   `json:"scene_id"`
	SourceUnit    string   `json:"source_unit,omitempty"`
	Patches       []*Patch `json:"patches"`
}

func (s *Scene) Patch(id string) *Patch {
	for _, p := range s.Patches {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (s *Scene) SortedPatches() []*Patch {
	out := append([]*Patch(nil), s.Patches...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type ClipProbe struct {
	ID      string  `json:"id"`
	PatchID string  `json:"patch_id"`
	U       float64 `json:"u"`
	V       float64 `json:"v"`
	Inside  bool    `json:"inside"`
}

type Bounds3 struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

func (b Bounds3) Contains(p Vec3, eps float64) bool {
	return p.X >= b.Min.X-eps && p.X <= b.Max.X+eps &&
		p.Y >= b.Min.Y-eps && p.Y <= b.Max.Y+eps &&
		p.Z >= b.Min.Z-eps && p.Z <= b.Max.Z+eps
}
