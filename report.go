package lightprobe

import (
	"math"
	"sort"
)

type ChannelErrorReport struct {
	Channel           int     `json:"channel"`
	EstimatedRadiance float64 `json:"estimated_radiance"`
	ExpectedRadiance  float64 `json:"expected_radiance"`
	AbsoluteError     float64 `json:"absolute_error"`
	LowerBound        float64 `json:"lower_bound"`
	UpperBound        float64 `json:"upper_bound"`
}

type QueryReport struct {
	QueryID                  string               `json:"query_id"`
	PrimaryRoomID            string               `json:"primary_room_id"`
	NeighborRoomID           string               `json:"neighbor_room_id,omitempty"`
	SHQuantResidualMax       float64              `json:"sh_quant_residual_max"`
	CountRoundingErrorMax    float64              `json:"count_rounding_error_max"`
	WeightNormalizationError float64              `json:"weight_normalization_error"`
	NormalizedSumError       float64              `json:"normalized_sum_error"`
	BlendTruncationError     float64              `json:"blend_truncation_error"`
	SHUpper                  float64              `json:"sh_upper"`
	CountUpper               float64              `json:"count_upper"`
	WeightUpper              float64              `json:"weight_upper"`
	BlendUpper               float64              `json:"blend_upper"`
	AccumulatedUpper         float64              `json:"accumulated_upper"`
	ActualErrorMax           float64              `json:"actual_error_max"`
	Tolerance                float64              `json:"tolerance"`
	WithinTolerance          bool                 `json:"within_tolerance"`
	Certified                bool                 `json:"certified"`
	Uncertain                bool                 `json:"uncertain"`
	Channels                 []ChannelErrorReport `json:"channels"`
}

func EvaluateQueries(scene *Scene, results map[string]ProbeResult, params Params) ([]QueryResult, error) {
	truth := map[string][3]float64{}
	for _, t := range scene.Truths {
		truth[t.QueryID] = t.Expected
	}
	out := make([]QueryResult, 0, len(scene.Queries))
	for _, q := range scene.Queries {
		qr, err := InterpolateQuery(scene, q, results, params)
		if err != nil {
			return nil, err
		}
		expected := truth[q.ID]
		qr.Report = BuildReport(qr, expected, results, params)
		qr.Uncertain = qr.Report.Uncertain
		for ch := 0; ch < 3; ch++ {
			qr.Lower[ch] = math.Max(0, qr.Radiance[ch]-qr.Report.AccumulatedUpper)
			qr.Upper[ch] = math.Min(1, qr.Radiance[ch]+qr.Report.AccumulatedUpper)
		}
		qr.WithinTolerance = qr.Report.WithinTolerance
		out = append(out, qr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Query.ID < out[j].Query.ID })
	return out, nil
}

func BuildReport(qr QueryResult, expected [3]float64, results map[string]ProbeResult, params Params) QueryReport {
	rep := QueryReport{
		QueryID:        qr.Query.ID,
		PrimaryRoomID:  qr.Query.PrimaryRoomID,
		NeighborRoomID: qr.Query.NeighborRoomID,
		Tolerance:      params.Tolerance,
	}

	primarySH, primaryCount, primarySHUpper, primaryCountUpper := cornerBoundsWithResults(qr.Primary, results, params)
	rep.SHQuantResidualMax = primarySH
	rep.CountRoundingErrorMax = primaryCount
	rep.WeightNormalizationError = qr.Primary.NormalizationError
	rep.NormalizedSumError = qr.Primary.NormalizedSumError
	rep.WeightUpper = weightInterpolationUpper(qr.Primary, params)

	shUpper := primarySHUpper
	countUpper := primaryCountUpper
	weightUpper := (1 - qr.BlendRatioQuantized) * rep.WeightUpper

	if qr.Neighbor != nil {
		nSH, nCount, nSHUpper, nCountUpper := cornerBoundsWithResults(*qr.Neighbor, results, params)
		beta := qr.BlendRatioQuantized
		rep.SHQuantResidualMax = math.Max(rep.SHQuantResidualMax, nSH)
		rep.CountRoundingErrorMax = math.Max(rep.CountRoundingErrorMax, nCount)
		rep.WeightNormalizationError = math.Max(rep.WeightNormalizationError, qr.Neighbor.NormalizationError)
		rep.NormalizedSumError = math.Max(rep.NormalizedSumError, qr.Neighbor.NormalizedSumError)
		shUpper = (1-beta)*shUpper + beta*nSHUpper
		countUpper = (1-beta)*countUpper + beta*nCountUpper
		weightUpper += beta * weightInterpolationUpper(*qr.Neighbor, params)
		rep.BlendTruncationError = qr.BlendTruncationError
		rep.BlendUpper = 1/float64(params.BlendScale) + 1/float64(params.FixedScale)
	}

	rep.SHUpper = shUpper
	rep.CountUpper = countUpper
	rep.WeightUpper = weightUpper
	rep.AccumulatedUpper = rep.SHUpper + rep.CountUpper + rep.WeightUpper + rep.BlendUpper

	rep.Channels = make([]ChannelErrorReport, 3)
	for ch := 0; ch < 3; ch++ {
		absErr := math.Abs(qr.Radiance[ch] - expected[ch])
		rep.ActualErrorMax = math.Max(rep.ActualErrorMax, absErr)
		rep.Channels[ch] = ChannelErrorReport{
			Channel: ch, EstimatedRadiance: qr.Radiance[ch], ExpectedRadiance: expected[ch],
			AbsoluteError: absErr,
			LowerBound:    math.Max(0, qr.Radiance[ch]-rep.AccumulatedUpper),
			UpperBound:    math.Min(1, qr.Radiance[ch]+rep.AccumulatedUpper),
		}
	}
	rep.WithinTolerance = rep.ActualErrorMax <= params.Tolerance+1e-12
	rep.Certified = rep.AccumulatedUpper <= params.Tolerance+1e-12
	rep.Uncertain = !rep.WithinTolerance || !rep.Certified
	return rep
}

func cornerBoundsWithResults(interp Interpolation, results map[string]ProbeResult, params Params) (shResidualMax, countRoundingMax, shUpper, countUpper float64) {
	for _, term := range interp.Terms {
		r := results[term.ProbeID]
		w := term.NormalizedWeight
		shResidualMax = math.Max(shResidualMax, r.SHQuantResidualMax)
		countRoundingMax = math.Max(countRoundingMax, r.CountRoundingError)
		shUpper += w * probeSHUpper(params)
		countUpper += w * probeCountUpper(r.SampleCount, params)
	}
	return shResidualMax, countRoundingMax, shUpper, countUpper
}

func probeSHUpper(params Params) float64 {
	return params.SHCondition/(2*float64(params.FixedScale)) + 1/float64(params.FixedScale)
}

func probeCountUpper(sampleCount int, params Params) float64 {
	n := float64(sampleCount)
	lambda := float64(params.SampleLambda)
	den := n + lambda
	if den <= 0.5 {
		return 1 / float64(params.FixedScale)
	}
	finite := 0.5 * lambda / ((den - 0.5) * (den - 0.5))
	return finite + 1/float64(params.FixedScale)
}

func weightInterpolationUpper(interp Interpolation, params Params) float64 {
	scale := float64(params.WeightScale)
	s := float64(interp.WeightSumFixed) / scale
	delta := math.Abs(1 - s)
	if s <= 1e-12 {
		return 1
	}
	return 2*delta/s + 1/float64(params.FixedScale)
}
