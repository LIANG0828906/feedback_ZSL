package vision

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"

	"maskpipe/internal/model"
)

const AlgorithmVersion = "maskpipe-algo-1.0.0"

func ValidateParameters(p model.ParameterSet) error {
	if p.ColorTolerance < 1 || p.ColorTolerance > 220 {
		return model.NewError(model.CodePara, "color_tolerance", "must be in [1,220], got %v", p.ColorTolerance)
	}
	if p.GradientThreshold < 1 || p.GradientThreshold > 255 {
		return model.NewError(model.CodePara, "gradient_threshold", "must be in [1,255], got %v", p.GradientThreshold)
	}
	if p.StructureSize < 1 || p.StructureSize > 15 || p.StructureSize%2 == 0 {
		return model.NewError(model.CodePara, "structure_size", "must be an odd integer in [1,15], got %d", p.StructureSize)
	}
	if p.BinaryThreshold < 0 || p.BinaryThreshold > 1 {
		return model.NewError(model.CodePara, "binary_threshold", "must be in [0,1], got %v", p.BinaryThreshold)
	}
	if p.MinRegionArea < 0 {
		return model.NewError(model.CodePara, "min_region_area", "must be non-negative")
	}
	if p.MaxIterations < 1 {
		return model.NewError(model.CodePara, "max_iterations", "must be positive")
	}
	return nil
}

func WithVersion(p model.ParameterSet) model.ParameterSet {
	cp := p
	cp.ParamVersion = ParameterVersion(p)
	return cp
}

func ParameterVersion(p model.ParameterSet) string {
	p.ParamVersion = ""
	b, _ := json.Marshal(p)
	sum := sha1.Sum(b)
	return "pv-" + hex.EncodeToString(sum[:])[:12]
}
