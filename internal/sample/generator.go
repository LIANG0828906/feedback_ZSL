package sample

import (
	"math/rand"

	"cadpipe/internal/cad"
	"cadpipe/internal/render"
	"cadpipe/internal/schema"
)

const (
	DefaultSeed    int64 = 420261125
	ViewportWidth        = 192
	ViewportHeight       = 128
	TileSize             = 16
	SampleGrid           = 9
)

type SurfacePointTruth struct {
	PatchID string  `json:"patch_id"`
	U       float64 `json:"u"`
	V       float64 `json:"v"`
	X       float64 `json:"x_m"`
	Y       float64 `json:"y_m"`
	Z       float64 `json:"z_m"`
}

type BoundsTruth struct {
	PatchID string      `json:"patch_id"`
	Bounds  cad.Bounds3 `json:"bounds_m"`
}

type ReferenceMesh struct {
	PatchID   string     `json:"patch_id"`
	SegmentsU int        `json:"segments_u"`
	SegmentsV int        `json:"segments_v"`
	Positions []cad.Vec3 `json:"positions_m"`
	Triangles [][3]int   `json:"triangles"`
}

type TileExpectation struct {
	TileID               int      `json:"tile_id"`
	SampleCount          int      `json:"analytic_sample_count"`
	InsideSampleCount    int      `json:"inside_sample_count"`
	ProbeIDs             []string `json:"probe_ids"`
	MinDepthM            float64  `json:"min_depth_m"`
	MaxDepthM            float64  `json:"max_depth_m"`
	ExpectedPixelSummary string   `json:"expected_pixel_summary_sha256"`
}

type TileExpectations struct {
	Width    int               `json:"viewport_width"`
	Height   int               `json:"viewport_height"`
	TileSize int               `json:"tile_size"`
	Tiles    []TileExpectation `json:"tiles"`
}

type Truth struct {
	Seed          int64               `json:"seed"`
	SurfacePoints []SurfacePointTruth `json:"surface_points"`
	Bounds        []BoundsTruth       `json:"bounds"`
	ClipProbes    []cad.ClipProbe     `json:"clip_probes"`
}

type Bundle struct {
	Seed         int64
	Scene        *cad.Scene
	Camera       *render.Camera
	SchemaV1     []byte
	SchemaV2     []byte
	SchemaV3     []byte
	Truth        Truth
	Meshes       []ReferenceMesh
	Tiles        TileExpectations
	TranscriptV1 *schema.Transcript
}

type patchSpec struct {
	id        string
	originX   float64
	originY   float64
	width     float64
	height    float64
	amplitude float64
	color     cad.RGB
	roughness float64
	metallic  float64
}

func Generate(seed int64) (*Bundle, error) {
	rng := rand.New(rand.NewSource(seed))
	specs := []patchSpec{
		{id: "surface-alpha", originX: -72, originY: -15, width: 42, height: 30, amplitude: float64(6 + rng.Intn(3)), color: cad.RGB{R: 0.30, G: 0.62, B: 0.95}, roughness: 0.42, metallic: 0.18},
		{id: "surface-beta", originX: -21, originY: -15, width: 42, height: 30, amplitude: float64(8 + rng.Intn(3)), color: cad.RGB{R: 0.95, G: 0.55, B: 0.28}, roughness: 0.58, metallic: 0.05},
		{id: "surface-gamma", originX: 30, originY: -15, width: 42, height: 30, amplitude: float64(7 + rng.Intn(4)), color: cad.RGB{R: 0.42, G: 0.82, B: 0.48}, roughness: 0.36, metallic: 0.28},
	}

	scene := &cad.Scene{Schema: schema.V3, SchemaVersion: 3, SceneID: "deterministic-cad-sample", SourceUnit: "mm"}
	truth := Truth{Seed: seed}
	for _, spec := range specs {
		patch := buildPatch(spec)
		scene.Patches = append(scene.Patches, patch)
		truth.Bounds = append(truth.Bounds, BoundsTruth{PatchID: spec.id, Bounds: analyticBounds(spec)})
		truth.ClipProbes = append(truth.ClipProbes, buildProbes(spec.id)...)
		for i := 0; i < SampleGrid; i++ {
			u := float64(i) / (SampleGrid - 1)
			for j := 0; j < SampleGrid; j++ {
				v := float64(j) / (SampleGrid - 1)
				p := analyticPoint(spec, u, v)
				truth.SurfacePoints = append(truth.SurfacePoints, SurfacePointTruth{
					PatchID: spec.id, U: u, V: v, X: p.X, Y: p.Y, Z: p.Z,
				})
			}
		}
	}

	v1, loss1, err := schema.Export(scene, schema.V1)
	if err != nil {
		return nil, err
	}
	v2, loss2, err := schema.Export(scene, schema.V2)
	if err != nil {
		return nil, err
	}
	v3, _, err := schema.Export(scene, schema.V3)
	if err != nil {
		return nil, err
	}
	_ = loss1
	_ = loss2

	loaded1, err := schema.Load(v1)
	if err != nil {
		return nil, err
	}
	cam := render.FitCamera(scene, ViewportWidth, ViewportHeight, TileSize)
	cam.Version = 1
	meshes := buildReferenceMeshes(specs)
	tiles := buildTileExpectations(cam, scene, truth)

	return &Bundle{
		Seed: seed, Scene: scene, Camera: cam,
		SchemaV1: v1, SchemaV2: v2, SchemaV3: v3,
		Truth: truth, Meshes: meshes, Tiles: tiles,
		TranscriptV1: loaded1.Transcript,
	}, nil
}

func buildPatch(spec patchSpec) *cad.Patch {
	points := make([][]cad.Point3, 3)
	weights := make([][]float64, 3)
	for i := 0; i < 3; i++ {
		points[i] = make([]cad.Point3, 3)
		weights[i] = make([]float64, 3)
		for j := 0; j < 3; j++ {
			x := spec.originX + spec.width*float64(i)/2
			y := spec.originY + spec.height*float64(j)/2
			z := 0.0
			if i == 1 && j == 1 {
				z = 4 * spec.amplitude
			}
			points[i][j] = cad.Point3{X: cad.Millimetre(x), Y: cad.Millimetre(y), Z: cad.Millimetre(z)}
			weights[i][j] = 1
		}
	}
	return &cad.Patch{
		ID: spec.id, SourceSchema: schema.V3, DegreeU: 2, DegreeV: 2,
		ControlPoints: points, Weights: weights,
		KnotsU: []float64{0, 0, 0, 1, 1, 1},
		KnotsV: []float64{0, 0, 0, 1, 1, 1},
		Clip: cad.ClipBoundary{Loops: [][]cad.Vec2{{
			{U: 0.18, V: 0.18}, {U: 0.82, V: 0.18},
			{U: 0.82, V: 0.82}, {U: 0.18, V: 0.82},
		}}},
		Material: &cad.Material{
			Name: spec.id + "-material", Color: spec.color,
			Metallic: spec.metallic, Roughness: spec.roughness,
		},
		Subdivision: &cad.Subdivision{Density: 8, CurvatureSensitivity: 0.6, MaxDensity: 32},
	}
}

func analyticPoint(spec patchSpec, u, v float64) cad.Vec3 {
	xMM := spec.originX + spec.width*u
	yMM := spec.originY + spec.height*v
	zMM := 16 * spec.amplitude * u * (1 - u) * v * (1 - v)
	return cad.Vec3{X: xMM / 1000, Y: yMM / 1000, Z: zMM / 1000}
}

func analyticBounds(spec patchSpec) cad.Bounds3 {
	return cad.Bounds3{
		Min: cad.Vec3{X: spec.originX / 1000, Y: spec.originY / 1000, Z: 0},
		Max: cad.Vec3{
			X: (spec.originX + spec.width) / 1000,
			Y: (spec.originY + spec.height) / 1000,
			Z: spec.amplitude / 1000,
		},
	}
}

func buildProbes(patchID string) []cad.ClipProbe {
	values := []struct {
		id     string
		u, v   float64
		inside bool
	}{
		{"center", 0.5, 0.5, true},
		{"lower-left-inside", 0.25, 0.25, true},
		{"upper-right-inside", 0.75, 0.75, true},
		{"corner-out", 0.05, 0.05, false},
		{"left-out", 0.08, 0.5, false},
		{"right-out", 0.92, 0.5, false},
		{"bottom-out", 0.5, 0.08, false},
		{"top-out", 0.5, 0.92, false},
	}
	out := make([]cad.ClipProbe, 0, len(values))
	for _, q := range values {
		out = append(out, cad.ClipProbe{
			ID: patchID + "-" + q.id, PatchID: patchID, U: q.u, V: q.v, Inside: q.inside,
		})
	}
	return out
}

func buildReferenceMeshes(specs []patchSpec) []ReferenceMesh {
	out := make([]ReferenceMesh, 0, len(specs))
	for _, spec := range specs {
		mesh := ReferenceMesh{PatchID: spec.id, SegmentsU: 8, SegmentsV: 8}
		for i := 0; i <= 8; i++ {
			u := float64(i) / 8
			for j := 0; j <= 8; j++ {
				v := float64(j) / 8
				mesh.Positions = append(mesh.Positions, analyticPoint(spec, u, v))
			}
		}
		row := 9
		for i := 0; i < 8; i++ {
			for j := 0; j < 8; j++ {
				a := i*row + j
				b := (i+1)*row + j
				c := i*row + j + 1
				d := (i+1)*row + j + 1
				mesh.Triangles = append(mesh.Triangles, [3]int{a, b, d}, [3]int{a, d, c})
			}
		}
		out = append(out, mesh)
	}
	return out
}
