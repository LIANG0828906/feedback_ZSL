package truthcheck

import (
	"math"

	"terrainnormalverify/internal/bake"
	"terrainnormalverify/internal/model"
	"terrainnormalverify/internal/seam"
)

type Result struct {
	PixelsChecked        int            `json:"pixelsChecked"`
	MaxNormalError       float64        `json:"maxNormalError"`
	MaxSlopeErrorDeg     float64        `json:"maxSlopeErrorDeg"`
	MaxSeamAngleErrorDeg float64        `json:"maxSeamAngleErrorDeg"`
	SeamsChecked         int            `json:"seamsChecked"`
	Attributions         map[string]int `json:"attributions"`
}

func Validate(project *model.Project, blocks map[int]*bake.BlockResult, seams []seam.SeamReport) (*Result, error) {
	result := &Result{Attributions: make(map[string]int)}
	expectedBySeam := make(map[string][]string)
	truthBySeam := make(map[string]model.SeamTruth)
	for _, st := range project.Truth.Seams {
		expectedBySeam[st.SeamID] = st.ExpectedCategories
		truthBySeam[st.SeamID] = st
	}
	for _, blockTruth := range project.Truth.Blocks {
		baked, ok := blocks[blockTruth.BlockID]
		if !ok {
			return nil, model.NewError(model.ErrBlock, blockTruth.BlockID, "truth.result", "missing baked block")
		}
		for _, pixel := range blockTruth.Pixels {
			actual := baked.Normals[baked.Index(pixel.X, pixel.Y)]
			normalError := math.Sqrt(
				(actual.X-pixel.Normal.X)*(actual.X-pixel.Normal.X) +
					(actual.Y-pixel.Normal.Y)*(actual.Y-pixel.Normal.Y) +
					(actual.Z-pixel.Normal.Z)*(actual.Z-pixel.Normal.Z))
			slopeError := math.Abs(baked.Slopes[baked.Index(pixel.X, pixel.Y)] - pixel.SlopeDeg)
			if normalError > result.MaxNormalError {
				result.MaxNormalError = normalError
			}
			if slopeError > result.MaxSlopeErrorDeg {
				result.MaxSlopeErrorDeg = slopeError
			}
			result.PixelsChecked++
			if normalError > pixel.Tolerance || slopeError > 1.0 {
				e := model.NewError(model.ErrDiff, blockTruth.BlockID, "truth.normal", "baked normal differs from analytic truth")
				e.Value = map[string]any{
					"x": pixel.X, "y": pixel.Y,
					"actual": actual, "expected": pixel.Normal,
					"normalError": normalError, "slopeErrorDeg": slopeError,
					"tolerance": pixel.Tolerance,
				}
				return result, e
			}
			color := model.ColorCode(actual)
			offset := baked.Index(pixel.X, pixel.Y) * 3
			if baked.Colors[offset] != color[0] || baked.Colors[offset+1] != color[1] || baked.Colors[offset+2] != color[2] {
				return nil, model.NewError(model.ErrDiff, blockTruth.BlockID, "truth.color", "normal color code is inconsistent")
			}
		}
	}
	for _, report := range seams {
		result.SeamsChecked++
		if truth, ok := truthBySeam[report.SeamID]; ok {
			errorDeg := math.Abs(report.MaxAngleDeg - truth.ExpectedAdjacentAngleDeg)
			if errorDeg > result.MaxSeamAngleErrorDeg {
				result.MaxSeamAngleErrorDeg = errorDeg
			}
			if errorDeg > truth.ToleranceDeg {
				e := model.NewError(model.ErrDiff, report.BlockA, "truth.seamAngle", "raw seam angle differs from analytic truth")
				e.Value = map[string]any{
					"seamId":       report.SeamID,
					"actual":       report.MaxAngleDeg,
					"expected":     truth.ExpectedAdjacentAngleDeg,
					"errorDeg":     errorDeg,
					"toleranceDeg": truth.ToleranceDeg,
				}
				return result, e
			}
		}
		if report.FinalMaxAngleDeg > report.ThresholdDeg+1e-9 {
			e := model.NewError(model.ErrSeam, report.BlockA, "truth.seam", "seam is not continuous")
			e.Value = map[string]any{
				"seamId": report.SeamID, "angleDeg": report.FinalMaxAngleDeg, "thresholdDeg": report.ThresholdDeg,
			}
			return result, e
		}
		for _, violation := range report.Violations {
			if violation.Attribution == "" {
				return nil, model.NewError(model.ErrSeam, report.BlockA, "truth.attribution", "seam violation has no attribution")
			}
			result.Attributions[violation.Attribution]++
			expected := expectedBySeam[report.SeamID]
			if len(expected) > 0 && !contains(expected, violation.Attribution) {
				e := model.NewError(model.ErrSeam, report.BlockA, "truth.attribution", "unexpected seam attribution category")
				e.Value = map[string]any{
					"seamId": report.SeamID, "actual": violation.Attribution, "expected": expected,
				}
				return result, e
			}
		}
	}
	return result, nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
