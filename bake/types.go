package bake

import (
	"encoding/json"

	"terrain-bake/project"
)

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type Bounds struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

type Instance struct {
	Position Vec3    `json:"position"`
	Yaw      float64 `json:"yaw"`
	Scale    float64 `json:"scale"`
	Variant  uint32  `json:"variant"`
}

type SeamStat struct {
	SeamID          string  `json:"seamId"`
	A               string  `json:"a"`
	B               string  `json:"b"`
	EdgeA           string  `json:"edgeA"`
	EdgeB           string  `json:"edgeB"`
	Samples         int     `json:"samples"`
	MaxAngleDeg     float64 `json:"maxAngleDeg"`
	MaxMaterialDiff int     `json:"maxMaterialDiff"`
	IterationsUsed  int     `json:"iterationsUsed"`
	Violations      int     `json:"violations"`
}

type SeamStats struct {
	ConstraintViolations int        `json:"constraintViolations"`
	MaxNormalAngleDeg    float64    `json:"maxNormalAngleDeg"`
	MaxMaterialDiff      int        `json:"maxMaterialDiff"`
	PerSeam              []SeamStat `json:"perSeam"`
}

// Artifact is a self-contained per-block render batch. It deliberately does
// not contain job timing, cache keys or generation numbers, so incremental and
// full bakes can be compared byte-for-byte.
type Artifact struct {
	BlockID         string     `json:"blockId"`
	Positions       []Vec3     `json:"positions"`
	Normals         []Vec3     `json:"normals"`
	Indices         []uint32   `json:"indices"`
	MaterialWeights []uint16   `json:"materialWeights"`
	MaterialRatios  []float64  `json:"materialRatios"`
	Instances       []Instance `json:"instances"`
	Bounds          Bounds     `json:"bounds"`
	Seams           SeamStats  `json:"seams"`
}

func (a *Artifact) CanonicalBytes() []byte {
	b, _ := json.Marshal(a)
	return b
}

func (a *Artifact) Hash() string {
	return project.HashBytes(a.CanonicalBytes())
}

type TruthMismatch struct {
	Block     string  `json:"block"`
	Kind      string  `json:"kind"`
	Expected  float64 `json:"expected"`
	Actual    float64 `json:"actual"`
	Tolerance float64 `json:"tolerance"`
	Layer     int     `json:"layer,omitempty"`
}

type TruthReport struct {
	Checks     int             `json:"checks"`
	Mismatches []TruthMismatch `json:"mismatches"`
	OK         bool            `json:"ok"`
}
