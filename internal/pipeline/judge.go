package pipeline

import (
	"math"

	"iconbench/internal/imaging"
	"iconbench/internal/model"
)

func CheckVersions(preview, exact model.Params) error {
	if model.SameVersion(preview, exact) {
		return nil
	}
	return model.VersionError(preview, exact)
}

func JudgePreviewExact(imageID string, previewVersion int64, previewMask, exactMask []byte, width, height int, tol model.Tolerances) (model.Judgment, *model.Error) {
	previewLabels, previewStats := imaging.ConnectedComponents(previewMask, width, height)
	exactLabels, exactStats := imaging.ConnectedComponents(exactMask, width, height)
	report := imaging.MatchRegions(imageID, previewStats, exactStats, previewLabels, exactLabels, width, height, tol)

	metrics := imaging.MaskMetrics(previewMask, exactMask)
	metrics.RegionCountExpected = len(exactStats)
	metrics.RegionCountActual = len(previewStats)
	metrics.RegionCountDelta = len(previewStats) - len(exactStats)
	metrics.MaxCentroidErrorPx = report.MaxCentroidErrorPx
	metrics.MaxAreaRelativeError = report.MaxAreaRelativeError

	firstRegion := firstBadRegion(exactStats, report, tol)
	diffX, diffY, hasDiff := imaging.FirstMaskDifference(previewMask, exactMask, width, height)

	countBad := absInt(metrics.RegionCountDelta) > tol.PreviewRegionCount
	centroidBad := metrics.MaxCentroidErrorPx > tol.PreviewCentroidPx
	areaBad := metrics.MaxAreaRelativeError > tol.PreviewAreaRel
	judgment := model.Judgment{
		ParamVersion: previewVersion, ImageID: imageID, Tolerances: tol, Metrics: metrics,
		OK:            !(countBad || centroidBad || areaBad),
		FirstRegionID: firstRegion, DiffX: diffX, DiffY: diffY,
	}
	if hasDiff {
		judgment.DiffX = diffX
		judgment.DiffY = diffY
	} else {
		judgment.DiffX = -1
		judgment.DiffY = -1
	}
	if !judgment.OK {
		judgment.Code = model.CodeDiverge
		return judgment, &model.Error{
			Code:          model.CodeDiverge,
			Message:       "rough preview diverged from exact recomputation beyond declared tolerance",
			FirstRegionID: firstRegion,
			DiffX:         diffX, DiffY: diffY,
			Metrics:  &metrics,
			Expected: map[string]any{"region_count": metrics.RegionCountExpected, "centroid_px_max": tol.PreviewCentroidPx, "area_rel_max": tol.PreviewAreaRel},
			Actual:   map[string]any{"region_count": metrics.RegionCountActual, "centroid_px_max": metrics.MaxCentroidErrorPx, "area_rel_max": metrics.MaxAreaRelativeError},
		}
	}
	return judgment, nil
}

func firstBadRegion(exactStats []model.RegionStat, report *imaging.RegionReport, tol model.Tolerances) string {
	byTruth := map[string]imaging.RegionDeviation{}
	for _, d := range report.Deviations {
		byTruth[d.TruthID] = d
	}
	for _, ts := range exactStats {
		d, ok := byTruth[ts.ID]
		if !ok {
			return ts.ID
		}
		if d.CentroidDistance > tol.PreviewCentroidPx || d.AreaRelativeError > tol.PreviewAreaRel {
			return ts.ID
		}
	}
	if len(report.Missing) > 0 {
		return report.Missing[0].ID
	}
	if len(report.Extra) > 0 {
		return report.Extra[0].ID
	}
	return ""
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// TighteningPoint is one point on a tolerance-history curve.
type TighteningPoint struct {
	Tolerances model.Tolerances `json:"tolerances"`
	Judgment   model.Judgment   `json:"judgment"`
	// ReportedStat is min(actual worst deviation, current ceiling). It is the
	// monotone non-increasing statistic retained during componentwise tightening.
	ReportedStat float64 `json:"reported_stat"`
}

// EvaluateToleranceCurve evaluates one fixed preview/exact pair under a sequence
// of progressively tighter tolerances. Raw metrics are pair-determined; the
// retained reported ceiling statistic is guaranteed not to increase.
func EvaluateToleranceCurve(imageID string, version int64, previewMask, exactMask []byte, w, h int, sequence []model.Tolerances) []TighteningPoint {
	var out []TighteningPoint
	previous := math.Inf(1)
	for _, tol := range sequence {
		j, _ := JudgePreviewExact(imageID, version, previewMask, exactMask, w, h, tol)
		rawWorst := math.Max(j.Metrics.MaxCentroidErrorPx, j.Metrics.MaxAreaRelativeError*100)
		reported := math.Min(rawWorst, math.Min(tol.PreviewCentroidPx, tol.PreviewAreaRel*100))
		if math.IsInf(previous, 1) {
			previous = reported
		}
		if reported > previous {
			reported = previous
		}
		out = append(out, TighteningPoint{Tolerances: tol, Judgment: j, ReportedStat: reported})
		previous = reported
	}
	return out
}
