package silhouette

import (
	"encoding/json"
	"fmt"
	"strconv"
)

const (
	DefaultSeed      int64 = 20261127
	DefaultTolerance int64 = 50_000
	DefaultBudget          = 90
)

type Rational2 struct {
	XNum int64 `json:"x_num"`
	XDen int64 `json:"x_den"`
	YNum int64 `json:"y_num"`
	YDen int64 `json:"y_den"`
}

type SegmentTruth struct {
	ID          string    `json:"id"`
	Weight      int64     `json:"weight"`
	Start       Vec2      `json:"start"`
	End         Vec2      `json:"end"`
	Midpoint    Rational2 `json:"midpoint"`
	ControlMin  Vec2      `json:"control_min"`
	ControlMax  Vec2      `json:"control_max"`
	SurfaceRefs []int     `json:"surface_refs"`
}

type TruthCheck struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

// TruthManifest is constructed only from analytic generator inputs. No field in
// this structure is populated from sampled vertices or assembled tiles.
type TruthManifest struct {
	Source         string         `json:"source"`
	Seed           int64          `json:"seed"`
	SceneID        string         `json:"scene_id"`
	Version        int            `json:"version"`
	InternalUnit   string         `json:"internal_unit"`
	GridRows       int            `json:"grid_rows"`
	GridCols       int            `json:"grid_cols"`
	SurfacePoints  []Vec3         `json:"surface_points"`
	ToleranceFixed int64          `json:"tolerance_fixed"`
	Budget         int            `json:"budget"`
	Segments       []SegmentTruth `json:"segments"`
	Tiles          []Tile         `json:"tiles"`
	Checks         []TruthCheck   `json:"checks"`
}

type GeneratedSample struct {
	Version int             `json:"version"`
	Schema  json.RawMessage `json:"schema"`
	Truth   TruthManifest   `json:"truth"`
	Scene   *Scene          `json:"scene"`
}

type xorshift64 struct {
	state uint64
}

func newXorShift(seed int64) *xorshift64 {
	s := uint64(seed)
	if s == 0 {
		s = 0x9e3779b97f4a7c15
	}
	return &xorshift64{state: s}
}

func (x *xorshift64) next() uint64 {
	v := x.state
	v ^= v << 13
	v ^= v >> 7
	v ^= v << 17
	x.state = v
	return v
}

func (x *xorshift64) between(min, max int64) int64 {
	if max <= min {
		return min
	}
	span := uint64(max - min + 1)
	return min + int64(x.next()%span)
}

func logicalPoint(x, y int64) Vec2 {
	return Vec2{X: x * FixedScale, Y: y * FixedScale}
}

func logicalPoint3(x, y, z int64) Vec3 {
	return Vec3{X: x * FixedScale, Y: y * FixedScale, Z: z * FixedScale}
}

// GenerateScene deterministically creates the canonical internal scene. The
// canonical source version is v3; Generate serializes the same canonical values
// into the requested schema.
func GenerateScene(seed int64) *Scene {
	rng := newXorShift(seed)
	controls := make([]Vec3, 0, 16)
	for row := 0; row < 4; row++ {
		for col := 0; col < 4; col++ {
			x := int64(20 + col*145 + int(rng.between(-7, 7)))
			y := int64(20 + row*145 + int(rng.between(-7, 7)))
			z := int64(18 + row*22 + col*31 + int(rng.between(-5, 5)))
			controls = append(controls, logicalPoint3(x, y, z))
		}
	}

	segments := []CurveSegment{
		{
			ID: "seg-0", Weight: 100, Closed: false, SurfaceRefs: []int{0, 1, 4, 5},
			Controls: [4]Vec2{
				logicalPoint(40, 82), logicalPoint(142, 505),
				logicalPoint(251, -36), logicalPoint(326, 304),
			},
		},
		{
			ID: "seg-1", Weight: 80, Closed: false, SurfaceRefs: []int{6, 7, 10, 11},
			Controls: [4]Vec2{
				logicalPoint(326, 304), logicalPoint(360, 618),
				logicalPoint(514, 30), logicalPoint(602, 358),
			},
		},
		{
			ID: "seg-2", Weight: 60, Closed: false, SurfaceRefs: []int{2, 3, 8, 9},
			Controls: [4]Vec2{
				logicalPoint(36, 302), logicalPoint(120, 18),
				logicalPoint(224, 576), logicalPoint(326, 304),
			},
		},
	}
	for i := range segments {
		segments[i].MetadataVersion = CurveMetaVersion
	}

	tiles := []Tile{
		{ID: "tile-0", Min: logicalPoint(0, 0), Max: logicalPoint(320, 320), Order: 0},
		{ID: "tile-1", Min: logicalPoint(320, 0), Max: logicalPoint(640, 320), Order: 1},
		{ID: "tile-2", Min: logicalPoint(0, 320), Max: logicalPoint(320, 640), Order: 2},
		{ID: "tile-3", Min: logicalPoint(320, 320), Max: logicalPoint(640, 640), Order: 3},
	}

	scene := &Scene{
		ID:            fmt.Sprintf("surface-%d", seed),
		Name:          fmt.Sprintf("deterministic-cad-surface-%d", seed),
		Seed:          seed,
		SourceVersion: VersionCurrent,
		Surface:       ControlGrid{Name: "surface-grid", Rows: 4, Cols: 4, Controls: controls},
		Segments:      segments,
		Tiles:         tiles,
		Budget:        Budget{MaxVertices: DefaultBudget, ParamVersion: "budget-v1"},
		Tolerance:     Tolerance{Value: DefaultTolerance, Unit: "micro_px", ParamVersion: "tolerance-v1"},
		Traces:        []ConversionTrace{{Stage: "generate", Path: "$", Action: "analytic_control_points", Note: "truth is generator-owned"}},
	}
	scene.Normalize()
	return scene
}

func GenerateTruth(scene *Scene, version int) TruthManifest {
	truth := TruthManifest{
		Source:         "analytic-generator/v1",
		Seed:           scene.Seed,
		SceneID:        scene.ID,
		Version:        version,
		InternalUnit:   "micro_logical (= micro_pixel, FixedScale=1000000)",
		GridRows:       scene.Surface.Rows,
		GridCols:       scene.Surface.Cols,
		SurfacePoints:  append([]Vec3(nil), scene.Surface.Controls...),
		ToleranceFixed: scene.Tolerance.Value,
		Budget:         scene.Budget.MaxVertices,
		Tiles:          append([]Tile(nil), scene.Tiles...),
	}
	for _, seg := range scene.Segments {
		start := seg.Controls[0]
		end := seg.Controls[3]
		min := seg.Controls[0]
		max := seg.Controls[0]
		for _, p := range seg.Controls[1:] {
			if p.X < min.X {
				min.X = p.X
			}
			if p.Y < min.Y {
				min.Y = p.Y
			}
			if p.X > max.X {
				max.X = p.X
			}
			if p.Y > max.Y {
				max.Y = p.Y
			}
		}
		truth.Segments = append(truth.Segments, SegmentTruth{
			ID: seg.ID, Weight: seg.Weight, Start: start, End: end,
			Midpoint: Rational2{
				XNum: start.X + 3*seg.Controls[1].X + 3*seg.Controls[2].X + end.X,
				XDen: 8,
				YNum: start.Y + 3*seg.Controls[1].Y + 3*seg.Controls[2].Y + end.Y,
				YDen: 8,
			},
			ControlMin: min, ControlMax: max,
			SurfaceRefs: append([]int(nil), seg.SurfaceRefs...),
		})
	}
	truth.Checks = []TruthCheck{
		{Name: "fixed_scale", Path: "truth.internal_unit", Value: strconv.FormatInt(FixedScale, 10)},
		{Name: "surface_control_count", Path: "truth.surface_points", Value: strconv.Itoa(len(truth.SurfacePoints))},
		{Name: "segment_count", Path: "truth.segments", Value: strconv.Itoa(len(truth.Segments))},
		{Name: "tile_count", Path: "truth.tiles", Value: strconv.Itoa(len(truth.Tiles))},
		{Name: "tolerance_internal", Path: "truth.tolerance_fixed", Value: strconv.FormatInt(truth.ToleranceFixed, 10)},
		{Name: "budget", Path: "truth.budget", Value: strconv.Itoa(truth.Budget)},
	}
	for _, seg := range truth.Segments {
		truth.Checks = append(truth.Checks,
			TruthCheck{Name: "segment_start", Path: "truth.segment." + seg.ID + ".start", Value: seg.Start.String()},
			TruthCheck{Name: "segment_end", Path: "truth.segment." + seg.ID + ".end", Value: seg.End.String()},
		)
	}
	return truth
}

// Generate serializes the canonical scene into one of the three declared
// schemas and returns the matching analytic truth manifest.
func Generate(seed int64, version int) (*GeneratedSample, error) {
	scene := GenerateScene(seed)
	result, err := Export(scene, version)
	if err != nil {
		return nil, err
	}
	return &GeneratedSample{
		Version: version,
		Schema:  json.RawMessage(result.Data),
		Truth:   GenerateTruth(scene, version),
		Scene:   scene,
	}, nil
}
