package model

import (
	"fmt"
	"math"
	"sort"
)

type Vec2 struct {
	U float64 `json:"u"`
	V float64 `json:"v"`
}

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (v Vec3) Add(o Vec3) Vec3    { return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z} }
func (v Vec3) Sub(o Vec3) Vec3    { return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z} }
func (v Vec3) Mul(s float64) Vec3 { return Vec3{v.X * s, v.Y * s, v.Z * s} }
func (v Vec3) Dot(o Vec3) float64 { return v.X*o.X + v.Y*o.Y + v.Z*o.Z }
func (v Vec3) Cross(o Vec3) Vec3 {
	return Vec3{
		X: v.Y*o.Z - v.Z*o.Y,
		Y: v.Z*o.X - v.X*o.Z,
		Z: v.X*o.Y - v.Y*o.X,
	}
}
func (v Vec3) Length() float64 { return math.Sqrt(v.Dot(v)) }
func (v Vec3) Lerp(o Vec3, t float64) Vec3 {
	return Vec3{v.X + (o.X-v.X)*t, v.Y + (o.Y-v.Y)*t, v.Z + (o.Z-v.Z)*t}
}
func (v Vec3) Normalize() Vec3 {
	l := v.Length()
	if l <= 1e-15 || math.IsNaN(l) || math.IsInf(l, 0) {
		return Vec3{}
	}
	return v.Mul(1 / l)
}
func (v Vec3) IsFinite() bool {
	return isFinite(v.X) && isFinite(v.Y) && isFinite(v.Z)
}
func (v Vec3) Max(o Vec3) Vec3 {
	return Vec3{math.Max(v.X, o.X), math.Max(v.Y, o.Y), math.Max(v.Z, o.Z)}
}
func (v Vec3) Min(o Vec3) Vec3 {
	return Vec3{math.Min(v.X, o.X), math.Min(v.Y, o.Y), math.Min(v.Z, o.Z)}
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

type BBox struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

func EmptyBBox() BBox {
	return BBox{
		Min: Vec3{math.Inf(1), math.Inf(1), math.Inf(1)},
		Max: Vec3{math.Inf(-1), math.Inf(-1), math.Inf(-1)},
	}
}

func (b *BBox) Add(p Vec3) {
	b.Min = b.Min.Min(p)
	b.Max = b.Max.Max(p)
}

func (b BBox) Empty() bool { return b.Min.X > b.Max.X }

func (b BBox) Center() Vec3 {
	return b.Min.Add(b.Max).Mul(0.5)
}

type ControlPoint struct {
	Row      int     `json:"row"`
	Col      int     `json:"col"`
	Position Vec3    `json:"position"`
	Weight   float64 `json:"weight"`
}

type SurfacePatch struct {
	ID        int            `json:"id"`
	Name      string         `json:"name"`
	UDegree   int            `json:"uDegree"`
	VDegree   int            `json:"vDegree"`
	UCount    int            `json:"uCount"`
	VCount    int            `json:"vCount"`
	Control   []ControlPoint `json:"controlPoints"`
	UKnots    []float64      `json:"uKnots"`
	VKnots    []float64      `json:"vKnots"`
	Trim      []Vec2         `json:"trimBoundary"`
	Tolerance float64        `json:"tolerance"`
}

func (p SurfacePatch) ControlAt(row, col int) (ControlPoint, bool) {
	if row < 0 || row >= p.VCount || col < 0 || col >= p.UCount {
		return ControlPoint{}, false
	}
	idx := row*p.UCount + col
	if idx < 0 || idx >= len(p.Control) {
		return ControlPoint{}, false
	}
	return p.Control[idx], true
}

func (p SurfacePatch) Domain() (u0, u1, v0, v1 float64) {
	if len(p.UKnots) == 0 || len(p.VKnots) == 0 {
		return 0, 1, 0, 1
	}
	return p.UKnots[p.UDegree], p.UKnots[p.UCount], p.VKnots[p.VDegree], p.VKnots[p.VCount]
}

type TruthSample struct {
	PatchID   int     `json:"patchId"`
	U         float64 `json:"u"`
	V         float64 `json:"v"`
	Position  Vec3    `json:"position"`
	Normal    Vec3    `json:"normal"`
	Curvature float64 `json:"curvature"`
}

type TrimTruth struct {
	PatchID int  `json:"patchId"`
	Point   Vec2 `json:"point"`
	Inside  bool `json:"inside"`
}

type BBoxTruth struct {
	PatchID int  `json:"patchId"`
	Box     BBox `json:"box"`
}

type GroundTruth struct {
	Samples    []TruthSample `json:"samples"`
	Bounds     []BBoxTruth   `json:"bounds"`
	TrimChecks []TrimTruth   `json:"trimChecks"`
}

type SubdivMesh struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Vertices []Vec3 `json:"vertices"`
	Normals  []Vec3 `json:"normals"`
	Indices  []int  `json:"indices"`
	Color    Color  `json:"color"`
}

type Color struct {
	R uint8 `json:"r"`
	G uint8 `json:"g"`
	B uint8 `json:"b"`
}

type Dataset struct {
	Seed      int64          `json:"seed"`
	Tolerance float64        `json:"tolerance"`
	AngleTol  float64        `json:"angleToleranceDeg"`
	Patches   []SurfacePatch `json:"patches"`
	Meshes    []SubdivMesh   `json:"meshes"`
	Truth     GroundTruth    `json:"truth"`
	Generator string         `json:"generator"`
	Generated string         `json:"generatedUtc"`
}

type Material struct {
	BaseColor   Color   `json:"baseColor"`
	Ambient     float64 `json:"ambient"`
	Diffuse     float64 `json:"diffuse"`
	Specular    float64 `json:"specular"`
	Shininess   float64 `json:"shininess"`
	WireOverlay bool    `json:"wireOverlay"`
}

type Camera struct {
	Eye    Vec3    `json:"eye"`
	Target Vec3    `json:"target"`
	Up     Vec3    `json:"up"`
	FovY   float64 `json:"fovY"`
	Near   float64 `json:"near"`
	Far    float64 `json:"far"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
}

type ClipPlane struct {
	Normal Vec3    `json:"normal"`
	Offset float64 `json:"offset"`
}

func (p ClipPlane) Distance(v Vec3) float64 { return p.Normal.Dot(v) + p.Offset }

type TileGrid struct {
	Cols   int `json:"cols"`
	Rows   int `json:"rows"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (g TileGrid) Count() int { return g.Cols * g.Rows }

func (g TileGrid) TileSize(id int) (w, h int) {
	col, row := id%g.Cols, id/g.Cols
	x0, x1 := g.XBounds(col), g.XBounds(col+1)
	y0, y1 := g.YBounds(row), g.YBounds(row+1)
	return x1 - x0, y1 - y0
}

func (g TileGrid) Rect(id int) (x0, y0, x1, y1 int) {
	col, row := id%g.Cols, id/g.Cols
	return g.XBounds(col), g.YBounds(row), g.XBounds(col + 1), g.YBounds(row + 1)
}

func (g TileGrid) XBounds(col int) int {
	return int(math.Round(float64(g.Width) * float64(col) / float64(g.Cols)))
}

func (g TileGrid) YBounds(row int) int {
	return int(math.Round(float64(g.Height) * float64(row) / float64(g.Rows)))
}

type RenderConfig struct {
	PreviewDensityCap int     `json:"previewDensityCap"`
	CurvatureScale    float64 `json:"curvatureScale"`
	NormalTolerance   float64 `json:"normalToleranceDeg"`
	MaxParallel       int     `json:"maxParallel"`
	PerTaskPixelBytes int64   `json:"perTaskPixelBytes"`
	PerTaskMeshBytes  int64   `json:"perTaskMeshBytes"`
	PixelBudget       int64   `json:"pixelBudgetBytes"`
	MeshBudget        int64   `json:"meshBudgetBytes"`
}

type PatchState struct {
	Source   SurfacePatch `json:"source"`
	Density  int          `json:"density"`
	Material Material     `json:"material"`
}

// SceneState is the mutable, versioned CAD scene. ContentHash deliberately
// excludes Version so a checkpoint can detect content drift.
type SceneState struct {
	Version          int64        `json:"version"`
	ContentHash      string       `json:"contentHash"`
	AlgorithmVersion string       `json:"algorithmVersion"`
	Patches          []PatchState `json:"patches"`
	Meshes           []SubdivMesh `json:"meshes"`
	Camera           Camera       `json:"camera"`
	Clip             ClipPlane    `json:"clipPlane"`
	Tiles            TileGrid     `json:"tiles"`
	Config           RenderConfig `json:"config"`
}

func (s SceneState) Clone() SceneState {
	s.Patches = append([]PatchState(nil), s.Patches...)
	for i := range s.Patches {
		p := &s.Patches[i]
		p.Source.Control = append([]ControlPoint(nil), p.Source.Control...)
		p.Source.UKnots = append([]float64(nil), p.Source.UKnots...)
		p.Source.VKnots = append([]float64(nil), p.Source.VKnots...)
		p.Source.Trim = append([]Vec2(nil), p.Source.Trim...)
	}
	s.Meshes = append([]SubdivMesh(nil), s.Meshes...)
	for i := range s.Meshes {
		s.Meshes[i].Vertices = append([]Vec3(nil), s.Meshes[i].Vertices...)
		s.Meshes[i].Normals = append([]Vec3(nil), s.Meshes[i].Normals...)
		s.Meshes[i].Indices = append([]int(nil), s.Meshes[i].Indices...)
	}
	return s
}

func (s SceneState) PatchByID(id int) (*PatchState, bool) {
	for i := range s.Patches {
		if s.Patches[i].Source.ID == id {
			return &s.Patches[i], true
		}
	}
	return nil, false
}

func SortTiles(ids []int) []int {
	out := append([]int(nil), ids...)
	sort.Ints(out)
	return out
}

func (p SurfacePatch) String() string {
	return fmt.Sprintf("patch %d (%s)", p.ID, p.Name)
}
