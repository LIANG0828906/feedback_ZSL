package pipeline

import (
	"meshdoctor/internal/errs"
	"meshdoctor/internal/mesh"
)

func CheckTopologyUnchanged(before, after mesh.TopologyStats) error {
	if before.Vertices == after.Vertices && before.Faces == after.Faces && before.Euler == after.Euler {
		return nil
	}
	err := errs.New(errs.ETOPO, "euler/topology changed during smoothing: %d -> %d", before.Euler, after.Euler)
	return errs.WithDetails(err, map[string]any{
		"before_vertices": before.Vertices,
		"after_vertices":  after.Vertices,
		"before_faces":    before.Faces,
		"after_faces":     after.Faces,
		"before_euler":    before.Euler,
		"after_euler":     after.Euler,
	})
}

func CheckVersion(stage, actualParamVersion, actualAlgoVersion string, params Params) error {
	if actualParamVersion == params.Version() && actualAlgoVersion == AlgoVersion {
		return nil
	}
	var err error = errs.New(errs.EVER, "version mismatch at stage %s", stage)
	err = errs.WithDetails(err, map[string]any{
		"stage":                  stage,
		"actual_param_version":   actualParamVersion,
		"expected_param_version": params.Version(),
		"actual_algo_version":    actualAlgoVersion,
		"expected_algo_version":  AlgoVersion,
	})
	return errs.InStage(err, stage)
}
