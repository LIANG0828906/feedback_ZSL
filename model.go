package terrainbake

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
)

const (
	// SchemaVersion is the on-disk project schema version.
	SchemaVersion = 1
	// WeightQuantum is the fixed quantization used by material weights. For
	// every texel, weights must be integers in [0, WeightQuantum] and sum to
	// exactly WeightQuantum.
	WeightQuantum = 1000
)

// Vec3 is a three-component vector. Normals returned by the baking core are
// unit length in tangent space; the +Z axis is surface up.
type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// Block is one independently schedulable terrain tile.
type Block struct {
	ID          string    `json:"id"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	OriginX     int       `json:"origin_x"`
	OriginY     int       `json:"origin_y"`
	Elevation   []float64 `json:"heights"`
	Materials   []string  `json:"material_ids"`
	Weights     []int     `json:"weights"`
	Description string    `json:"description,omitempty"`
}

// Material is a named layer. NormalScale and RotationDeg are deterministic
// per-layer parameters used to derive and combine layer normals.
type Material struct {
	ID          string  `json:"id"`
	NormalScale float64 `json:"normal_scale"`
	RotationDeg float64 `json:"rotation_deg"`
}

// Edge identifies a block edge.
type Edge string

const (
	EdgeLeft   Edge = "left"
	EdgeRight  Edge = "right"
	EdgeTop    Edge = "top"
	EdgeBottom Edge = "bottom"
)

// Seam is a bilateral normal-continuity constraint on two matching edges.
// ReverseB is needed when the natural traversal of edge B runs opposite to
// edge A in world space.
type Seam struct {
	ID            string  `json:"id"`
	BlockA        string  `json:"block_a"`
	BlockB        string  `json:"block_b"`
	EdgeA         Edge    `json:"edge_a"`
	EdgeB         Edge    `json:"edge_b"`
	ReverseB      bool    `json:"reverse_b"`
	MaxAngleDeg   float64 `json:"max_angle_deg"`
	MaxIterations int     `json:"max_iterations"`
}

// TruthPixel is an analytic sample used by the independent verification pass.
type TruthPixel struct {
	BlockID  string  `json:"block_id"`
	X        int     `json:"x"`
	Y        int     `json:"y"`
	Normal   Vec3    `json:"normal"`
	SlopeDeg float64 `json:"slope_deg"`
}

// TruthSeam records an expected seam angle at one edge pair.
type TruthSeam struct {
	SeamID   string  `json:"seam_id"`
	Index    int     `json:"index"`
	AngleDeg float64 `json:"angle_deg"`
}

// Project is the complete, self-contained terrain baking input. Heights are
// stored in the same project file as weights, seams and truths so local runs
// never depend on external services.
type Project struct {
	SchemaVersion int          `json:"schema_version"`
	Name          string       `json:"name"`
	Seed          int64        `json:"seed"`
	Blocks        []Block      `json:"blocks"`
	Materials     []Material   `json:"materials"`
	Seams         []Seam       `json:"seams"`
	Truth         []TruthPixel `json:"truth"`
	SeamTruth     []TruthSeam  `json:"seam_truth"`
}

// BakeParams controls the per-block normal calculation.
type BakeParams struct {
	Operator string  `json:"operator"`
	Scale    float64 `json:"scale"`
}

// DefaultBakeParams returns the canonical deterministic configuration.
func DefaultBakeParams() BakeParams {
	return BakeParams{Operator: "sobel", Scale: 2.5}
}

// SeamStats is emitted for every block. MaxAngleDeg is the maximum actual
// angle over all constrained pairs involving the block, not a threshold.
type SeamStats struct {
	BlockID     string  `json:"block_id"`
	Violations  int     `json:"violations"`
	MaxAngleDeg float64 `json:"max_angle_deg"`
	Constrained int     `json:"constrained"`
}

// InvalidationItem documents exactly why a block entered the dirty set.
// Chain starts at the edited object/block and ends at BlockID.
type InvalidationItem struct {
	BlockID string   `json:"block_id"`
	Reason  string   `json:"reason"`
	Chain   []string `json:"chain"`
}

// QueueEvent records why a task was not admitted immediately.
type QueueEvent struct {
	BlockID string `json:"block_id"`
	Reason  string `json:"reason"`
}

// CommitEvent records the stable final submission order. Reused blocks still
// appear in the canonical snapshot order.
type CommitEvent struct {
	BlockID string `json:"block_id"`
	Kind    string `json:"kind"` // baked | reused
}

// BlockOutput is the in-memory result for one block.
type BlockOutput struct {
	BlockID      string
	Width        int
	Height       int
	Normals      []Vec3
	SlopeDeg     []float64
	NormalPNG    []byte
	SlopePNG     []byte
	NormalDigest string
	SlopeDigest  string
	DataDigest   string
	ContentHash  string
	Seam         SeamStats
}

// BakeResult describes one successful or partially successful scheduler run.
type BakeResult struct {
	AlgorithmVersion string               `json:"algorithm_version"`
	ParamsVersion    string               `json:"params_version"`
	ContentHash      string               `json:"content_hash"`
	Invalidation     []InvalidationItem   `json:"invalidation,omitempty"`
	Queue            []QueueEvent         `json:"queue,omitempty"`
	CommitSequence   []CommitEvent        `json:"commit_sequence"`
	Baked            []string             `json:"baked"`
	Reused           []string             `json:"reused"`
	Failed           map[string]string    `json:"failed,omitempty"`
	SeamStats        map[string]SeamStats `json:"seam_stats"`
	Paused           bool                 `json:"paused"`
	SnapshotReset    bool                 `json:"snapshot_reset"`
	SnapshotResetWhy string               `json:"snapshot_reset_reason,omitempty"`
	TruthChecked     int                  `json:"truth_checked"`
}

// ProjectHash computes the global content hash used by checkpoint validation.
// It is stable over map-free canonical project JSON.
func ProjectHash(p *Project) string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ParamsHash identifies output-affecting parameters. Worker counts and memory
// budgets intentionally do not participate.
func ParamsHash(p BakeParams) string {
	b, _ := json.Marshal(struct {
		Operator string  `json:"operator"`
		Scale    float64 `json:"scale"`
		Quantum  int     `json:"quantum"`
	}{p.Operator, p.Scale, WeightQuantum})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// BlockContentHash binds a block's own data, layers, adjacent seam
// definitions and immediate neighbors. It is used to reject partial cache
// reuse after edits or restart.
func BlockContentHash(p *Project, blockID string, params BakeParams, algVersion string) string {
	block, ok := FindBlock(p, blockID)
	if !ok {
		return ""
	}
	mats := make([]Material, 0, len(block.Materials))
	for _, id := range block.Materials {
		if m, ok := FindMaterial(p, id); ok {
			mats = append(mats, *m)
		}
	}
	seams := seamsForBlock(p, blockID)
	type neighbor struct {
		ID     string    `json:"id"`
		Height []float64 `json:"heights"`
		Mats   []string  `json:"materials"`
		Weight []int     `json:"weights"`
	}
	neighbors := make([]neighbor, 0, len(seams))
	for _, s := range seams {
		other := s.BlockA
		if other == blockID {
			other = s.BlockB
		}
		if b, ok := FindBlock(p, other); ok {
			neighbors = append(neighbors, neighbor{b.ID, b.Elevation, b.Materials, b.Weights})
		}
	}
	payload := struct {
		Algorithm string     `json:"algorithm"`
		Params    BakeParams `json:"params"`
		Quantum   int        `json:"quantum"`
		Block     Block      `json:"block"`
		Materials []Material `json:"materials"`
		Seams     []Seam     `json:"seams"`
		Neighbors []neighbor `json:"neighbors"`
	}{algVersion, params, WeightQuantum, *block, mats, seams, neighbors}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func FindBlock(p *Project, id string) (*Block, bool) {
	for i := range p.Blocks {
		if p.Blocks[i].ID == id {
			return &p.Blocks[i], true
		}
	}
	return nil, false
}

func FindMaterial(p *Project, id string) (*Material, bool) {
	for i := range p.Materials {
		if p.Materials[i].ID == id {
			return &p.Materials[i], true
		}
	}
	return nil, false
}

func seamsForBlock(p *Project, id string) []Seam {
	out := make([]Seam, 0)
	for _, s := range p.Seams {
		if s.BlockA == id || s.BlockB == id {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func canonicalBlockIDs(p *Project) []string {
	ids := make([]string, 0, len(p.Blocks))
	for _, b := range p.Blocks {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	return ids
}

func validFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func normalize(v Vec3) (Vec3, bool) {
	l := math.Sqrt(v.X*v.X + v.Y*v.Y + v.Z*v.Z)
	if !validFinite(l) || l < 1e-15 {
		return Vec3{}, false
	}
	return Vec3{v.X / l, v.Y / l, v.Z / l}, true
}

func angleDeg(a, b Vec3) float64 {
	d := a.X*b.X + a.Y*b.Y + a.Z*b.Z
	d = math.Max(-1, math.Min(1, d))
	return math.Acos(d) * 180 / math.Pi
}

func encodeNormal(v Vec3) (uint8, uint8, uint8) {
	r := uint8(math.Round(clamp01(v.X*0.5+0.5) * 255))
	g := uint8(math.Round(clamp01(v.Y*0.5+0.5) * 255))
	b := uint8(math.Round(clamp01(v.Z*0.5+0.5) * 255))
	return r, g, b
}

func digestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
