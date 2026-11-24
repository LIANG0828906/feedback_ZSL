package model

import "fmt"

const (
	MinFloodThreshold = 1.0
	MaxFloodThreshold = 255.0
	MinRegionArea     = 1
	MaxRegionArea     = 100000
	MinMergeCost      = 0.0
	MaxMergeCost      = 1000.0
	MinAreaDiff       = 0
	MaxAreaDiff       = 1000000
	MinSmoothKernel   = 0
	MaxSmoothKernel   = 5
	MinMorphRadius    = 0
	MaxMorphRadius    = 4
)

func ValidateParams(p Params) error {
	if p.FloodThreshold < MinFloodThreshold || p.FloodThreshold > MaxFloodThreshold {
		return (&Error{Code: CodePara}).WithField("flood_threshold", MinFloodThreshold, MaxFloodThreshold)
	}
	if p.MinRegionArea < MinRegionArea || p.MinRegionArea > MaxRegionArea {
		return (&Error{Code: CodePara}).WithField("min_region_area", MinRegionArea, MaxRegionArea)
	}
	if p.MergeCostCap < MinMergeCost || p.MergeCostCap > MaxMergeCost {
		return (&Error{Code: CodePara}).WithField("merge_cost_cap", MinMergeCost, MaxMergeCost)
	}
	if p.AreaDiffCap < MinAreaDiff || p.AreaDiffCap > MaxAreaDiff {
		return (&Error{Code: CodePara}).WithField("area_diff_cap", MinAreaDiff, MaxAreaDiff)
	}
	if p.SmoothKernel != 0 && p.SmoothKernel != 3 && p.SmoothKernel != 5 {
		return (&Error{Code: CodePara, Message: "smooth_kernel must be one of [0 3 5]"}).WithField("smooth_kernel", "0,3,5", "0,3,5")
	}
	if p.MorphRadius < MinMorphRadius || p.MorphRadius > MaxMorphRadius {
		return (&Error{Code: CodePara}).WithField("morph_radius", MinMorphRadius, MaxMorphRadius)
	}
	if p.GradientOperator != "sobel" && p.GradientOperator != "scharr" {
		return &Error{Code: CodePara, Field: "gradient_operator", Expected: []string{"sobel", "scharr"}, Actual: p.GradientOperator, Message: fmt.Sprintf("unsupported gradient operator %q", p.GradientOperator)}
	}
	return nil
}

func SameVersion(a, b Params) bool {
	return a.ParamVersion == b.ParamVersion && a.CanonicalHash() == b.CanonicalHash()
}

func VersionError(preview, exact Params) error {
	return &Error{
		Code:     CodeVer,
		Message:  fmt.Sprintf("parameter version mismatch: preview=v%d exact=v%d", preview.ParamVersion, exact.ParamVersion),
		Field:    "param_version",
		VersionA: fmt.Sprintf("v%d/%s/%s", preview.ParamVersion, preview.CanonicalHash(), AlgorithmVersion),
		VersionB: fmt.Sprintf("v%d/%s/%s", exact.ParamVersion, exact.CanonicalHash(), AlgorithmVersion),
	}
}

func DefaultTolerances() Tolerances {
	return Tolerances{
		MaskIoUMin:           0.93,
		MaskMismatchRatioMax: 0.08,
		CentroidPx:           2.5,
		AreaRel:              0.08,
		HoleCount:            0,
		PreviewRegionCount:   0,
		PreviewCentroidPx:    3.5,
		PreviewAreaRel:       0.14,
	}
}
