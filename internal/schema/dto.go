package schema

const (
	V1 = "cad.scene/v1"
	V2 = "cad.scene/v2"
	V3 = "cad.scene/v3"
)

type pointV1 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type uvV1 struct {
	U float64 `json:"u"`
	V float64 `json:"v"`
}

type clipV1 struct {
	UVPoints []uvV1 `json:"uv_points"`
}

type materialV1 struct {
	MaterialID string     `json:"material_id"`
	BaseColor  [3]float64 `json:"base_color"`
	Metallic   float64    `json:"metallic"`
	Roughness  float64    `json:"roughness"`
}

type patchV1 struct {
	PatchID         string      `json:"patch_id"`
	DegreeU         int         `json:"degree_u"`
	DegreeV         int         `json:"degree_v"`
	ControlPoints   [][]pointV1 `json:"control_points"`
	Weights         [][]float64 `json:"weights"`
	KnotsU          []float64   `json:"knots_u"`
	KnotsV          []float64   `json:"knots_v"`
	ClipBoundary    clipV1      `json:"clip_boundary"`
	SurfaceMaterial *materialV1 `json:"surface_material,omitempty"`
}

type envelopeV1 struct {
	Schema  string    `json:"schema"`
	SceneID string    `json:"scene_id"`
	Unit    string    `json:"unit"`
	Patches []patchV1 `json:"patches"`
}

type pointV2 struct {
	X float64 `json:"xMm"`
	Y float64 `json:"yMm"`
	Z float64 `json:"zMm"`
}

type deltaV2 struct {
	DX float64 `json:"dxMm"`
	DY float64 `json:"dyMm"`
	DZ float64 `json:"dzMm"`
}

type clipV2 struct {
	FlatUV []float64 `json:"flatUV"`
}

type patchV2 struct {
	PatchID         string      `json:"patchId"`
	DegreeU         int         `json:"degreeU"`
	DegreeV         int         `json:"degreeV"`
	ControlBaseMm   *pointV2    `json:"controlBaseMm"`
	ControlDeltasMm [][]deltaV2 `json:"controlDeltasMm"`
	WeightGrid      [][]float64 `json:"weightGrid"`
	UKnots          []float64   `json:"uKnots"`
	VKnots          []float64   `json:"vKnots"`
	Clip            clipV2      `json:"clip"`
}

type envelopeV2 struct {
	Schema     string    `json:"schema"`
	SceneID    string    `json:"sceneId"`
	LengthUnit string    `json:"lengthUnit"`
	Patches    []patchV2 `json:"patches"`
}

type pointV3 struct {
	X float64 `json:"X"`
	Y float64 `json:"Y"`
	Z float64 `json:"Z"`
}

type uvV3 struct {
	U float64 `json:"U"`
	V float64 `json:"V"`
}

type ringV3 struct {
	Vertices []uvV3 `json:"Vertices"`
}

type clipV3 struct {
	Rings []ringV3 `json:"Rings"`
}

type materialV3 struct {
	Name            string     `json:"Name"`
	ColorRGB        [3]float64 `json:"ColorRGB"`
	MetallicFactor  float64    `json:"MetallicFactor"`
	RoughnessFactor float64    `json:"RoughnessFactor"`
}

type subdivisionV3 struct {
	Density              int     `json:"Density"`
	CurvatureSensitivity float64 `json:"CurvatureSensitivity"`
	MaxDensity           int     `json:"MaxDensity"`
	Tolerance            float64 `json:"Tolerance,omitempty"`
}

type knotVectorsV3 struct {
	U []float64 `json:"U"`
	V []float64 `json:"V"`
}

type patchV3 struct {
	SurfaceID    string         `json:"SurfaceID"`
	UDegree      int            `json:"UDegree"`
	VDegree      int            `json:"VDegree"`
	ControlGridM [][]pointV3    `json:"ControlGridM"`
	Weights      [][]float64    `json:"Weights"`
	KnotVectors  knotVectorsV3  `json:"KnotVectors"`
	Clipping     clipV3         `json:"Clipping"`
	Material     *materialV3    `json:"Material,omitempty"`
	Subdivision  *subdivisionV3 `json:"Subdivision,omitempty"`
}

type envelopeV3 struct {
	Schema     string    `json:"schema"`
	SceneID    string    `json:"SceneID"`
	LengthUnit string    `json:"LengthUnit"`
	Surfaces   []patchV3 `json:"Surfaces"`
}
