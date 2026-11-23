package spatialpick

const (
	FormatVersion = 1

	GeometryAABB   = "aabb"
	GeometrySphere = "sphere"
	GeometryMesh   = "mesh"

	StepTranslate = "translate"
	StepRotate    = "rotate"
	StepScale     = "scale"
)

type Camera struct {
	ViewportWidth  float64 `json:"viewport_width"`
	ViewportHeight float64 `json:"viewport_height"`
	WorldHeight    float64 `json:"world_height"`
	Eye            Vec3    `json:"eye"`
	Right          Vec3    `json:"right"`
	Up             Vec3    `json:"up"`
	Forward        Vec3    `json:"forward"`
}

type Geometry struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Min       Vec3     `json:"min"`
	Max       Vec3     `json:"max"`
	Center    Vec3     `json:"center"`
	Radius    float64  `json:"radius"`
	Vertices  []Vec3   `json:"vertices"`
	Triangles [][3]int `json:"triangles"`
}

type Node struct {
	ID            string      `json:"id"`
	ParentID      string      `json:"parent_id"`
	LocalMatrix   [16]float64 `json:"local_matrix"`
	GeometryIndex int         `json:"geometry_index"`
}

type RayQuery struct {
	ID     string `json:"id"`
	Screen Vec2   `json:"screen"`
}

type BoxQuery struct {
	ID   string `json:"id"`
	Rect Rect   `json:"rect"`
}

type PointQuery struct {
	ID    string `json:"id"`
	Point Vec3   `json:"point"`
}

type PivotStep struct {
	Kind  string `json:"kind"`
	Value Vec3   `json:"value"`
}

type EditBatch struct {
	ID        string      `json:"id"`
	Selection []string    `json:"selection"`
	Pivot     Vec3        `json:"pivot"`
	Steps     []PivotStep `json:"steps"`
}

type Scene struct {
	FormatVersion int          `json:"format_version"`
	SceneVersion  string       `json:"scene_version"`
	Precision     int          `json:"precision"`
	Camera        Camera       `json:"camera"`
	Geometries    []Geometry   `json:"geometries"`
	Nodes         []Node       `json:"nodes"`
	RayQueries    []RayQuery   `json:"ray_queries"`
	BoxQueries    []BoxQuery   `json:"box_queries"`
	PointQueries  []PointQuery `json:"point_queries"`
	EditSequence  []EditBatch  `json:"edit_sequence"`
	ContentHash   string       `json:"content_hash"`
}

type MatrixManifest struct {
	NodeID      string `json:"node_id"`
	LocalDigest string `json:"local_digest"`
}

type HitTruth struct {
	NodeID     string  `json:"node_id"`
	GeometryID string  `json:"geometry_id"`
	T          float64 `json:"t"`
	Point      Vec3    `json:"point"`
}

type GeometryHitTruth struct {
	NodeID     string  `json:"node_id"`
	GeometryID string  `json:"geometry_id"`
	Hit        bool    `json:"hit"`
	T          float64 `json:"t,omitempty"`
	Point      Vec3    `json:"point,omitempty"`
}

type RayTruth struct {
	QueryID     string             `json:"query_id"`
	ErrorCode   string             `json:"error_code"`
	First       *HitTruth          `json:"first,omitempty"`
	Geometries  []GeometryHitTruth `json:"geometries"`
	Ambiguities []Ambiguity        `json:"ambiguities,omitempty"`
}

type BoxTruth struct {
	QueryID     string      `json:"query_id"`
	ErrorCode   string      `json:"error_code"`
	Hits        []string    `json:"hits"`
	Ambiguities []Ambiguity `json:"ambiguities,omitempty"`
}

type PointTruth struct {
	QueryID     string      `json:"query_id"`
	ErrorCode   string      `json:"error_code"`
	Hits        []string    `json:"hits"`
	Ambiguities []Ambiguity `json:"ambiguities,omitempty"`
}

type NodeMatrixTruth struct {
	NodeID string      `json:"node_id"`
	Matrix [16]float64 `json:"matrix"`
}

type OperationTruth struct {
	BatchID  string            `json:"batch_id"`
	Matrices []NodeMatrixTruth `json:"matrices"`
}

type Truth struct {
	FormatVersion     int              `json:"format_version"`
	SceneVersion      string           `json:"scene_version"`
	Precision         int              `json:"precision"`
	ContentHash       string           `json:"content_hash"`
	TransformManifest []MatrixManifest `json:"transform_manifest"`
	RayResults        []RayTruth       `json:"ray_results"`
	BoxResults        []BoxTruth       `json:"box_results"`
	PointResults      []PointTruth     `json:"point_results"`
	OperationResults  []OperationTruth `json:"operation_results"`
	FinalRayResults   []RayTruth       `json:"final_ray_results"`
	FinalBoxResults   []BoxTruth       `json:"final_box_results"`
	FinalPointResults []PointTruth     `json:"final_point_results"`
}

type PickHit struct {
	NodeID     string  `json:"node_id"`
	GeometryID string  `json:"geometry_id"`
	T          float64 `json:"t"`
	Point      Vec3    `json:"point"`
	Depth      int     `json:"depth"`
}

type GeometryHit struct {
	NodeID     string  `json:"node_id"`
	GeometryID string  `json:"geometry_id"`
	Hit        bool    `json:"hit"`
	T          float64 `json:"t,omitempty"`
	Point      Vec3    `json:"point,omitempty"`
	Bound      float64 `json:"bound"`
}

type RayPickResult struct {
	QueryID     string        `json:"query_id"`
	ErrorCode   string        `json:"error_code"`
	First       *PickHit      `json:"first,omitempty"`
	Geometries  []GeometryHit `json:"geometries"`
	ErrorBound  float64       `json:"error_bound"`
	Ambiguities []Ambiguity   `json:"ambiguities,omitempty"`
}

type BoxSelectResult struct {
	QueryID     string      `json:"query_id"`
	ErrorCode   string      `json:"error_code"`
	Hits        []string    `json:"hits"`
	ErrorBound  float64     `json:"error_bound"`
	Ambiguities []Ambiguity `json:"ambiguities,omitempty"`
}

type PointSelectResult struct {
	QueryID     string      `json:"query_id"`
	ErrorCode   string      `json:"error_code"`
	Hits        []string    `json:"hits"`
	ErrorBound  float64     `json:"error_bound"`
	Ambiguities []Ambiguity `json:"ambiguities,omitempty"`
}

type NodeResult struct {
	NodeID     string      `json:"node_id"`
	Depth      int         `json:"depth"`
	WorldRaw   [16]int64   `json:"world_raw"`
	World      [16]float64 `json:"world"`
	AABB       AABB        `json:"aabb"`
	Sphere     Sphere      `json:"sphere"`
	Version    uint64      `json:"version"`
	GeometryID string      `json:"geometry_id,omitempty"`
	ErrorBound float64     `json:"error_bound"`
}

type ResultBundle struct {
	SceneVersion string              `json:"scene_version"`
	Revision     uint64              `json:"revision"`
	Precision    int                 `json:"precision"`
	Nodes        []NodeResult        `json:"nodes"`
	Rays         []RayPickResult     `json:"rays"`
	Boxes        []BoxSelectResult   `json:"boxes"`
	Points       []PointSelectResult `json:"points"`
}
