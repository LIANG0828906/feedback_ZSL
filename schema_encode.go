package terrainbake

import (
	"encoding/json"
	"fmt"
)

type outBounds struct {
	MinX     int64  `json:"min_x"`
	MinY     int64  `json:"min_y"`
	MaxX     int64  `json:"max_x"`
	MaxY     int64  `json:"max_y"`
	Boundary string `json:"boundary"`
}

type outSeam struct {
	ID            string  `json:"id"`
	A             string  `json:"block_a"`
	B             string  `json:"block_b"`
	Axis          string  `json:"axis"`
	Coordinate    int64   `json:"coordinate"`
	Start         int64   `json:"start"`
	End           int64   `json:"end"`
	SampleSpacing int64   `json:"spacing"`
	AngleLimitDeg float64 `json:"angle_limit_deg"`
}

type outCamera struct {
	TimeMS int64   `json:"time_ms"`
	PosX   int64   `json:"pos_x"`
	PosY   int64   `json:"pos_y"`
	Yaw    float64 `json:"yaw_deg"`
	Pitch  float64 `json:"pitch_deg"`
	FOV    float64 `json:"fov_deg"`
	Near   float64 `json:"near"`
	Far    float64 `json:"far"`
	Focus  string  `json:"focus_block"`
}

type outTruthSample struct {
	Block    string `json:"block"`
	X        *int64 `json:"x,omitempty"`
	Y        *int64 `json:"y,omitempty"`
	U        *int64 `json:"u,omitempty"`
	V        *int64 `json:"v,omitempty"`
	DU       *int64 `json:"du,omitempty"`
	DV       *int64 `json:"dv,omitempty"`
	HeightMM int64  `json:"height_mm"`
}

type outSeamTruth struct {
	ID        string  `json:"id"`
	Axis      string  `json:"axis"`
	Start     int64   `json:"start"`
	End       int64   `json:"end"`
	Points    []int64 `json:"points"`
	HeightsMM []int64 `json:"heights_mm"`
}

type outFractureTruth struct {
	ID          string  `json:"id"`
	Seam        string  `json:"seam"`
	Kind        string  `json:"kind"`
	At          int64   `json:"at"`
	ToleranceUM int64   `json:"tolerance_um"`
	AngleLimit  float64 `json:"angle_limit_deg"`
	Expected    string  `json:"expected_attribution"`
}

type outTruthV1 struct {
	Vertices     []outTruthSample   `json:"vertices"`
	MaxTriangles map[string]int     `json:"max_triangles"`
	Seams        []outSeamTruth     `json:"seams"`
	Fractures    []outFractureTruth `json:"fractures"`
}

type outTruthV2 struct {
	Vertices     []outTruthSample   `json:"vertices"`
	TriangleCaps map[string]int     `json:"triangle_caps"`
	Seams        []outSeamTruth     `json:"seams"`
	Fractures    []outFractureTruth `json:"fractures"`
}

type outTruthV3 struct {
	Vertices     []outTruthSample   `json:"vertices"`
	TriangleCaps map[string]int     `json:"triangle_limits"`
	Seams        []outSeamTruth     `json:"seams"`
	Fractures    []outFractureTruth `json:"fractures"`
}

type outV1Chunk struct {
	ID                string    `json:"id"`
	Bounds            outBounds `json:"bounds"`
	Budget            Budget    `json:"budget"`
	ChordTolPermille  int64     `json:"chord_tol_permille"`
	ToleranceRounding string    `json:"tolerance_rounding"`
	Neighbors         []string  `json:"neighbors"`
	Seams             []outSeam `json:"seams,omitempty"`
}

type outV1 struct {
	SchemaVersion int          `json:"schema_version"`
	Seed          uint64       `json:"seed"`
	World         outBounds    `json:"world"`
	Chunks        []outV1Chunk `json:"chunks"`
	CameraTrack   []outCamera  `json:"camera_track"`
	Truth         outTruthV1   `json:"truth"`
}

type outV2Origin struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type outV2Extent struct {
	W int64 `json:"w"`
	H int64 `json:"h"`
}

type outV2Chunk struct {
	ID             string      `json:"id"`
	Origin         outV2Origin `json:"origin"`
	Extent         outV2Extent `json:"extent"`
	Coverage       string      `json:"coverage"`
	VertexBudget   int         `json:"vertex_budget"`
	TriangleBudget int         `json:"triangle_budget"`
	ChordTolBP     int64       `json:"chord_tolerance_bp"`
	Rounding       string      `json:"rounding"`
}

type outV2World struct {
	MinX     int64  `json:"min_x"`
	MinY     int64  `json:"min_y"`
	MaxX     int64  `json:"max_x"`
	MaxY     int64  `json:"max_y"`
	Coverage string `json:"coverage"`
}

type outV2 struct {
	Version         string       `json:"version"`
	Seed            uint64       `json:"seed"`
	World           outV2World   `json:"world"`
	Chunks          []outV2Chunk `json:"chunks"`
	SeamConstraints []outSeam    `json:"seam_constraints"`
	Camera          []outCamera  `json:"camera"`
	Truth           outTruthV2   `json:"truth"`
}

type outV3Bounds struct {
	MinU       int64  `json:"min_u"`
	MinV       int64  `json:"min_v"`
	MaxU       int64  `json:"max_u"`
	MaxV       int64  `json:"max_v"`
	Convention string `json:"convention"`
}

type outV3Sampling struct {
	MaxVertices  int `json:"max_vertices"`
	MaxTriangles int `json:"max_triangles"`
}

type outV3Block struct {
	ID                string        `json:"id"`
	BoundsRel         outV3Bounds   `json:"bounds_rel"`
	Sampling          outV3Sampling `json:"sampling"`
	ChordTolPermille  int64         `json:"chord_tol_permille"`
	ToleranceRounding string        `json:"tolerance_rounding"`
	Adjacent          []string      `json:"adjacent"`
	Seams             []outSeam     `json:"seams,omitempty"`
}

type outV3Camera struct {
	Samples []outCamera `json:"samples"`
}

type outV3 struct {
	Schema string       `json:"schema"`
	Seed   uint64       `json:"seed"`
	World  outV3Bounds  `json:"world_bounds_rel"`
	Blocks []outV3Block `json:"blocks"`
	Camera outV3Camera  `json:"camera"`
	Truth  outTruthV3   `json:"truth"`
}

// EncodeProject serializes the internal model into the requested wire
// version. The output is deterministic (map keys are sorted by encoding/json).
func EncodeProject(p *Project, target SchemaVersion) ([]byte, error) {
	if p == nil {
		return nil, errSchema("$", "nil project")
	}
	if !IsKnownSchemaVersion(target) {
		return nil, errSchema("schema_version", fmt.Sprintf("unknown target version %d", int(target)))
	}
	if err := p.ValidateTopology(); err != nil {
		return nil, err
	}
	switch target {
	case SchemaV1:
		return encodeV1(p)
	case SchemaV2:
		return encodeV2(p)
	case SchemaV3:
		return encodeV3(p)
	default:
		return nil, errSchema("schema_version", "unsupported target")
	}
}

func encodeBoundsClosed(rect Rect, rule string) (outBounds, error) {
	minX, maxX, err := HalfOpenToBoundary("rect.x", rect.MinX, rect.MaxX, rule)
	if err != nil {
		return outBounds{}, err
	}
	minY, maxY, err := HalfOpenToBoundary("rect.y", rect.MinY, rect.MaxY, rule)
	if err != nil {
		return outBounds{}, err
	}
	return outBounds{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY, Boundary: rule}, nil
}

func encodeV1(p *Project) ([]byte, error) {
	world, err := encodeBoundsClosed(p.World.Rect, "[]")
	if err != nil {
		return nil, err
	}
	seamsByBlock := groupSeamsByBlock(p.Seams)
	doc := outV1{
		SchemaVersion: 1,
		Seed:          p.World.Seed,
		World:         world,
		CameraTrack:   encodeCameras(p.Camera),
		Truth:         makeTruthV1(p),
	}
	for _, b := range p.Blocks {
		tol, _, err := UMToTolerance(b.ToleranceUM, "permille", "half_up")
		if err != nil {
			return nil, err
		}
		bounds, err := encodeBoundsClosed(b.Rect, "[]")
		if err != nil {
			return nil, err
		}
		doc.Chunks = append(doc.Chunks, outV1Chunk{
			ID:                b.ID,
			Bounds:            bounds,
			Budget:            b.Budget,
			ChordTolPermille:  tol,
			ToleranceRounding: "half_up",
			Neighbors:         append([]string(nil), b.Neighbors...),
			Seams:             encodeSeams(seamsByBlock[b.ID]),
		})
	}
	return json.MarshalIndent(doc, "", "  ")
}

func encodeV2(p *Project) ([]byte, error) {
	doc := outV2{
		Version: "terrain/v2",
		Seed:    p.World.Seed,
		World: outV2World{
			MinX: p.World.Rect.MinX, MinY: p.World.Rect.MinY,
			MaxX: p.World.Rect.MaxX, MaxY: p.World.Rect.MaxY,
			Coverage: "[)",
		},
		SeamConstraints: encodeSeams(p.Seams),
		Camera:          encodeCameras(p.Camera),
		Truth:           makeTruthV2(p),
	}
	for _, b := range p.Blocks {
		tol, _, err := UMToTolerance(b.ToleranceUM, "basis_points", "floor")
		if err != nil {
			return nil, err
		}
		doc.Chunks = append(doc.Chunks, outV2Chunk{
			ID:             b.ID,
			Origin:         outV2Origin{X: b.Rect.MinX, Y: b.Rect.MinY},
			Extent:         outV2Extent{W: b.Rect.Width(), H: b.Rect.Height()},
			Coverage:       "[)",
			VertexBudget:   b.Budget.MaxVertices,
			TriangleBudget: b.Budget.MaxTriangles,
			ChordTolBP:     tol,
			Rounding:       "floor",
		})
	}
	return json.MarshalIndent(doc, "", "  ")
}

func encodeV3(p *Project) ([]byte, error) {
	worldRule := "(]"
	worldMinX, worldMaxX, err := HalfOpenToBoundary("world.u", p.World.Rect.MinX, p.World.Rect.MaxX, worldRule)
	if err != nil {
		return nil, err
	}
	worldMinY, worldMaxY, err := HalfOpenToBoundary("world.v", p.World.Rect.MinY, p.World.Rect.MaxY, worldRule)
	if err != nil {
		return nil, err
	}
	origin := Point{X: p.World.Rect.MinX, Y: p.World.Rect.MinY}
	seamsByBlock := groupSeamsByBlock(p.Seams)
	doc := outV3{
		Schema: "terrain/v3",
		Seed:   p.World.Seed,
		World: outV3Bounds{
			MinU: worldMinX, MinV: worldMinY, MaxU: worldMaxX, MaxV: worldMaxY,
			Convention: worldRule,
		},
		Camera: outV3Camera{Samples: encodeCameras(p.Camera)},
		Truth:  makeTruthV3(p, origin),
	}
	for _, b := range p.Blocks {
		tol, _, err := UMToTolerance(b.ToleranceUM, "permille", "ceil")
		if err != nil {
			return nil, err
		}
		declMinX, declMaxX, err := HalfOpenToBoundary("block.u", b.Rect.MinX, b.Rect.MaxX, worldRule)
		if err != nil {
			return nil, err
		}
		declMinY, declMaxY, err := HalfOpenToBoundary("block.v", b.Rect.MinY, b.Rect.MaxY, worldRule)
		if err != nil {
			return nil, err
		}
		relMinX, err := SubChecked("block.min_u", declMinX, origin.X)
		if err != nil {
			return nil, err
		}
		relMinY, err := SubChecked("block.min_v", declMinY, origin.Y)
		if err != nil {
			return nil, err
		}
		relMaxX, err := SubChecked("block.max_u", declMaxX, origin.X)
		if err != nil {
			return nil, err
		}
		relMaxY, err := SubChecked("block.max_v", declMaxY, origin.Y)
		if err != nil {
			return nil, err
		}
		doc.Blocks = append(doc.Blocks, outV3Block{
			ID: b.ID,
			BoundsRel: outV3Bounds{
				MinU: relMinX, MinV: relMinY, MaxU: relMaxX, MaxV: relMaxY,
				Convention: worldRule,
			},
			Sampling: outV3Sampling{
				MaxVertices: b.Budget.MaxVertices, MaxTriangles: b.Budget.MaxTriangles,
			},
			ChordTolPermille:  tol,
			ToleranceRounding: "ceil",
			Adjacent:          append([]string(nil), b.Neighbors...),
			Seams:             encodeSeams(seamsByBlock[b.ID]),
		})
	}
	return json.MarshalIndent(doc, "", "  ")
}

func encodeSeams(in []SeamSpec) []outSeam {
	out := make([]outSeam, 0, len(in))
	for _, s := range in {
		out = append(out, outSeam{
			ID: s.ID, A: s.A, B: s.B, Axis: string(s.Axis),
			Coordinate: s.Coordinate, Start: s.Start, End: s.End,
			SampleSpacing: s.SampleSpacing, AngleLimitDeg: s.AngleLimitDeg,
		})
	}
	return out
}

func groupSeamsByBlock(seams []SeamSpec) map[string][]SeamSpec {
	out := map[string][]SeamSpec{}
	for _, s := range seams {
		out[s.A] = append(out[s.A], s)
		out[s.B] = append(out[s.B], s)
	}
	return out
}

func encodeCameras(in []CameraSample) []outCamera {
	out := make([]outCamera, 0, len(in))
	for _, c := range in {
		out = append(out, outCamera{
			TimeMS: c.TimeMS, PosX: c.Position.X, PosY: c.Position.Y,
			Yaw: c.YawDeg, Pitch: c.PitchDeg, FOV: c.FOVDeg,
			Near: c.Near, Far: c.Far, Focus: c.Focus,
		})
	}
	return out
}

func makeTruthV1(p *Project) outTruthV1 {
	out := outTruthV1{MaxTriangles: copyIntMap(p.Truth.MaxTriangles)}
	for _, s := range p.Truth.Samples {
		x, y := s.X, s.Y
		out.Vertices = append(out.Vertices, outTruthSample{
			Block: s.BlockID, X: &x, Y: &y, HeightMM: s.HeightMM,
		})
	}
	out.Seams = makeSeamTruth(p.Truth.Seams)
	out.Fractures = makeFractures(p.Truth.Fractures)
	return out
}

func makeTruthV2(p *Project) outTruthV2 {
	out := outTruthV2{TriangleCaps: copyIntMap(p.Truth.MaxTriangles)}
	for _, s := range p.Truth.Samples {
		b, _ := p.BlockByID(s.BlockID)
		u, _ := SubChecked("truth.u", s.X, b.Rect.MinX)
		v, _ := SubChecked("truth.v", s.Y, b.Rect.MinY)
		out.Vertices = append(out.Vertices, outTruthSample{
			Block: s.BlockID, U: &u, V: &v, HeightMM: s.HeightMM,
		})
	}
	out.Seams = makeSeamTruth(p.Truth.Seams)
	out.Fractures = makeFractures(p.Truth.Fractures)
	return out
}

func makeTruthV3(p *Project, origin Point) outTruthV3 {
	out := outTruthV3{TriangleCaps: copyIntMap(p.Truth.MaxTriangles)}
	for _, s := range p.Truth.Samples {
		b, _ := p.BlockByID(s.BlockID)
		du, _ := SubChecked("truth.du", s.X, b.Rect.MinX)
		dv, _ := SubChecked("truth.dv", s.Y, b.Rect.MinY)
		out.Vertices = append(out.Vertices, outTruthSample{
			Block: s.BlockID, DU: &du, DV: &dv, HeightMM: s.HeightMM,
		})
	}
	out.Seams = makeSeamTruth(p.Truth.Seams)
	out.Fractures = makeFractures(p.Truth.Fractures)
	return out
}

func makeSeamTruth(in []SeamTruth) []outSeamTruth {
	out := make([]outSeamTruth, 0, len(in))
	for _, s := range in {
		out = append(out, outSeamTruth{
			ID: s.SeamID, Axis: string(s.Axis), Start: s.Start, End: s.End,
			Points:    append([]int64(nil), s.Points...),
			HeightsMM: append([]int64(nil), s.HeightsMM...),
		})
	}
	return out
}

func makeFractures(in []FractureTruth) []outFractureTruth {
	out := make([]outFractureTruth, 0, len(in))
	for _, f := range in {
		out = append(out, outFractureTruth{
			ID: f.ID, Seam: f.SeamID, Kind: f.Kind, At: f.At,
			ToleranceUM: f.ToleranceUM, AngleLimit: f.AngleLimit, Expected: f.Expected,
		})
	}
	return out
}

func copyIntMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ConvertSchemaVersion is the explicit upgrade/downgrade entry point. It
// returns the normalized intermediate project as well as bytes in the target
// version, making field-level conversion traces available to callers.
func ConvertSchemaVersion(data []byte, target SchemaVersion) ([]byte, *Project, error) {
	p, err := LoadProject(data)
	if err != nil {
		return nil, nil, err
	}
	if !IsKnownSchemaVersion(target) {
		return nil, nil, errSchema("schema_version", fmt.Sprintf("unknown target version %d", int(target)))
	}
	out, err := EncodeProject(p, target)
	if err != nil {
		return nil, nil, err
	}
	return out, p, nil
}
