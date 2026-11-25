package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"

	"meshdoctor/internal/errs"
)

const AlgoVersion = "mesh-algo-1.0.0"

type Params struct {
	WeldTolerance         float64 `json:"weld_tolerance"`
	LaplacianWeight       float64 `json:"laplacian_weight"`
	SmoothIterations      int     `json:"smooth_iterations"`
	MaxParallelism        int     `json:"max_parallelism"`
	BufferMeshesPerWorker int     `json:"buffer_meshes_per_worker"`
}

func DefaultParams() Params {
	return Params{
		WeldTolerance:         1e-7,
		LaplacianWeight:       0.5,
		SmoothIterations:      2,
		MaxParallelism:        2,
		BufferMeshesPerWorker: 2,
	}
}

func (p Params) WithDefaults() Params {
	d := DefaultParams()
	if p.WeldTolerance == 0 {
		p.WeldTolerance = d.WeldTolerance
	}
	if p.LaplacianWeight == 0 {
		p.LaplacianWeight = d.LaplacianWeight
	}
	if p.SmoothIterations == 0 {
		p.SmoothIterations = d.SmoothIterations
	}
	if p.MaxParallelism <= 0 {
		p.MaxParallelism = d.MaxParallelism
	}
	if p.BufferMeshesPerWorker <= 0 {
		p.BufferMeshesPerWorker = d.BufferMeshesPerWorker
	}
	return p
}

func (p Params) ValidateSmooth() error {
	if math.IsNaN(p.LaplacianWeight) || p.LaplacianWeight < 0 || p.LaplacianWeight > 1 {
		return errs.New(errs.EPARA, "laplacian_weight must be in [0,1], got %v", p.LaplacianWeight)
	}
	if p.SmoothIterations < 0 || p.SmoothIterations > 1000 {
		return errs.New(errs.EPARA, "smooth_iterations must be in [0,1000], got %d", p.SmoothIterations)
	}
	return nil
}

func (p Params) ValidateWeld() error {
	if math.IsNaN(p.WeldTolerance) || p.WeldTolerance < 0 {
		return errs.New(errs.EPARA, "weld_tolerance must be finite and non-negative, got %v", p.WeldTolerance)
	}
	return nil
}

type parameterIdentity struct {
	WeldTolerance    float64 `json:"weld_tolerance"`
	LaplacianWeight  float64 `json:"laplacian_weight"`
	SmoothIterations int     `json:"smooth_iterations"`
}

func (p Params) Version() string {
	b, _ := json.Marshal(parameterIdentity{
		WeldTolerance: p.WeldTolerance, LaplacianWeight: p.LaplacianWeight, SmoothIterations: p.SmoothIterations,
	})
	sum := sha256.Sum256(b)
	return "pv-" + hex.EncodeToString(sum[:])[:16]
}

func (p Params) SnapshotJSON() string {
	b, _ := json.MarshalIndent(p, "", "  ")
	return string(b)
}
