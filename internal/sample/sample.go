// Package sample renders deterministic projects in all three schema
// dialects and loads them through the real loader.
package sample

import (
	"encoding/json"

	"coldkernel/internal/gen"
	"coldkernel/internal/loader"
	"coldkernel/internal/model"
)

// Triple holds the raw snapshots and loaded projects for one logical project
// expressed in all three schema versions.
type Triple struct {
	Spec     *gen.Spec
	RawV1    []byte
	RawV2    []byte
	RawV3    []byte
	ProjectV1 *model.Project
	ProjectV2 *model.Project
	ProjectV3 *model.Project
}

// Build generates a deterministic triple from a seed.
func Build(seed uint64) (*Triple, error) {
	spec := gen.Generate(seed)
	raw1, err := json.Marshal(gen.BuildV1(spec))
	if err != nil {
		return nil, err
	}
	raw2, err := json.Marshal(gen.BuildV2(spec))
	if err != nil {
		return nil, err
	}
	raw3, err := json.Marshal(gen.BuildV3(spec))
	if err != nil {
		return nil, err
	}
	p1, err := loader.Load(raw1)
	if err != nil {
		return nil, err
	}
	p2, err := loader.Load(raw2)
	if err != nil {
		return nil, err
	}
	p3, err := loader.Load(raw3)
	if err != nil {
		return nil, err
	}
	return &Triple{
		Spec: spec, RawV1: raw1, RawV2: raw2, RawV3: raw3,
		ProjectV1: p1, ProjectV2: p2, ProjectV3: p3,
	}, nil
}
