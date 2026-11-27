package terrainbake

// The raw structs deliberately mirror the three wire formats instead of
// sharing tags. This makes the interoperability surface explicit and makes
// missing-field diagnostics precise.

type rawCamera struct {
	TimeMS *int64   `json:"time_ms"`
	PosX   *int64   `json:"pos_x"`
	PosY   *int64   `json:"pos_y"`
	Yaw    *float64 `json:"yaw_deg"`
	Pitch  *float64 `json:"pitch_deg"`
	FOV    *float64 `json:"fov_deg"`
	Near   *float64 `json:"near"`
	Far    *float64 `json:"far"`
	Focus  *string  `json:"focus_block"`
}

type rawTruthSample struct {
	Block    *string `json:"block"`
	X        *int64  `json:"x"`
	Y        *int64  `json:"y"`
	U        *int64  `json:"u"`
	V        *int64  `json:"v"`
	DU       *int64  `json:"du"`
	DV       *int64  `json:"dv"`
	HeightMM *int64  `json:"height_mm"`
}

type rawSeamTruth struct {
	ID        *string `json:"id"`
	Axis      *string `json:"axis"`
	Start     *int64  `json:"start"`
	End       *int64  `json:"end"`
	Points    []int64 `json:"points"`
	HeightsMM []int64 `json:"heights_mm"`
}

type rawFractureTruth struct {
	ID          *string  `json:"id"`
	SeamID      *string  `json:"seam"`
	Kind        *string  `json:"kind"`
	At          *int64   `json:"at"`
	ToleranceUM *int64   `json:"tolerance_um"`
	AngleLimit  *float64 `json:"angle_limit_deg"`
	Expected    *string  `json:"expected_attribution"`
}

type rawTruthV1 struct {
	Vertices     []rawTruthSample   `json:"vertices"`
	MaxTriangles map[string]int     `json:"max_triangles"`
	Seams        []rawSeamTruth     `json:"seams"`
	Fractures    []rawFractureTruth `json:"fractures"`
}

type rawTruthV2 struct {
	Vertices     []rawTruthSample   `json:"vertices"`
	TriangleCaps map[string]int     `json:"triangle_caps"`
	Seams        []rawSeamTruth     `json:"seams"`
	Fractures    []rawFractureTruth `json:"fractures"`
}

type rawTruthV3 struct {
	Vertices     []rawTruthSample   `json:"vertices"`
	TriangleCaps map[string]int     `json:"triangle_limits"`
	Seams        []rawSeamTruth     `json:"seams"`
	Fractures    []rawFractureTruth `json:"fractures"`
}

type rawSeam struct {
	ID            *string  `json:"id"`
	A             *string  `json:"block_a"`
	B             *string  `json:"block_b"`
	Axis          *string  `json:"axis"`
	Coordinate    *int64   `json:"coordinate"`
	Start         *int64   `json:"start"`
	End           *int64   `json:"end"`
	SampleSpacing *int64   `json:"spacing"`
	AngleLimitDeg *float64 `json:"angle_limit_deg"`
}

type v1World struct {
	MinX     *int64  `json:"min_x"`
	MinY     *int64  `json:"min_y"`
	MaxX     *int64  `json:"max_x"`
	MaxY     *int64  `json:"max_y"`
	Boundary *string `json:"boundary"`
}

type v1Bounds struct {
	MinX     *int64  `json:"min_x"`
	MinY     *int64  `json:"min_y"`
	MaxX     *int64  `json:"max_x"`
	MaxY     *int64  `json:"max_y"`
	Boundary *string `json:"boundary"`
}

type v1Budget struct {
	MaxVertices  *int `json:"max_vertices"`
	MaxTriangles *int `json:"max_triangles"`
}

type v1Chunk struct {
	ID                *string   `json:"id"`
	Bounds            *v1Bounds `json:"bounds"`
	Budget            *v1Budget `json:"budget"`
	ChordTolPermille  *int64    `json:"chord_tol_permille"`
	ToleranceRounding *string   `json:"tolerance_rounding"`
	Neighbors         []string  `json:"neighbors"`
	Seams             []rawSeam `json:"seams"`
}

type v1Doc struct {
	SchemaVersion *int        `json:"schema_version"`
	Seed          *uint64     `json:"seed"`
	World         *v1World    `json:"world"`
	Chunks        []v1Chunk   `json:"chunks"`
	CameraTrack   []rawCamera `json:"camera_track"`
	Truth         *rawTruthV1 `json:"truth"`
}

type v2Origin struct {
	X *int64 `json:"x"`
	Y *int64 `json:"y"`
}

type v2Extent struct {
	W *int64 `json:"w"`
	H *int64 `json:"h"`
}

type v2World struct {
	MinX     *int64  `json:"min_x"`
	MinY     *int64  `json:"min_y"`
	MaxX     *int64  `json:"max_x"`
	MaxY     *int64  `json:"max_y"`
	Coverage *string `json:"coverage"`
}

type v2Chunk struct {
	ID             *string   `json:"id"`
	Origin         *v2Origin `json:"origin"`
	Extent         *v2Extent `json:"extent"`
	Coverage       *string   `json:"coverage"`
	VertexBudget   *int      `json:"vertex_budget"`
	TriangleBudget *int      `json:"triangle_budget"`
	ChordTolBP     *int64    `json:"chord_tolerance_bp"`
	ToleranceRound *string   `json:"rounding"`
}

type v2Doc struct {
	Version         *string     `json:"version"`
	Seed            *uint64     `json:"seed"`
	World           *v2World    `json:"world"`
	Chunks          []v2Chunk   `json:"chunks"`
	SeamConstraints []rawSeam   `json:"seam_constraints"`
	Camera          []rawCamera `json:"camera"`
	Truth           *rawTruthV2 `json:"truth"`
}

type v3BoundsRel struct {
	MinU       *int64  `json:"min_u"`
	MinV       *int64  `json:"min_v"`
	MaxU       *int64  `json:"max_u"`
	MaxV       *int64  `json:"max_v"`
	Convention *string `json:"convention"`
}

type v3Sampling struct {
	MaxVertices  *int `json:"max_vertices"`
	MaxTriangles *int `json:"max_triangles"`
}

type v3Block struct {
	ID                *string      `json:"id"`
	BoundsRel         *v3BoundsRel `json:"bounds_rel"`
	Sampling          *v3Sampling  `json:"sampling"`
	ChordTolPermille  *int64       `json:"chord_tol_permille"`
	ToleranceRounding *string      `json:"tolerance_rounding"`
	Adjacent          []string     `json:"adjacent"`
	Seams             []rawSeam    `json:"seams"`
}

type v3Camera struct {
	Samples []rawCamera `json:"samples"`
}

type v3Doc struct {
	Schema *string      `json:"schema"`
	Seed   *uint64      `json:"seed"`
	World  *v3BoundsRel `json:"world_bounds_rel"`
	Blocks []v3Block    `json:"blocks"`
	Camera *v3Camera    `json:"camera"`
	Truth  *rawTruthV3  `json:"truth"`
}
