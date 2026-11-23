package engine

import (
	"math"
	"sort"

	"maskpipeline/internal/apperr"
	"maskpipeline/internal/model"
	"maskpipeline/internal/sample"
)

func Compare(preview *PreviewResult, exact *RunResult, tol sample.Tolerances) (*ConsistencyReport, error) {
	if preview == nil || exact == nil {
		return nil, apperr.New(apperr.CodeMeta, "compare", "preview and exact results are required")
	}
	report := &ConsistencyReport{}
	report.RegionCount = len(exact.Stats.Regions)
	report.RegionCountDiff = absInt(len(exact.Stats.Regions) - len(preview.Stats.Regions))
	exactByID := make(map[int]model.RegionStats, len(exact.Stats.Regions))
	previewByID := make(map[int]model.RegionStats, len(preview.Stats.Regions))
	for _, r := range exact.Stats.Regions {
		exactByID[r.ID] = r
	}
	for _, r := range preview.Stats.Regions {
		previewByID[r.ID] = r
	}
	firstRegion := 0
	regionsAligned := true
	ids := make([]int, 0, len(exactByID))
	for id := range exactByID {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		er := exactByID[id]
		pr, ok := previewByID[id]
		if !ok {
			regionsAligned = false
			if firstRegion == 0 {
				firstRegion = id
			}
			continue
		}
		dx := er.Centroid.X - pr.Centroid.X
		dy := er.Centroid.Y - pr.Centroid.Y
		delta := math.Hypot(dx, dy)
		if delta > report.MaxCentroidDeltaPx {
			report.MaxCentroidDeltaPx = delta
		}
		areaErr := math.Abs(float64(pr.Area)-float64(er.Area)) / math.Max(1, float64(er.Area))
		if areaErr > report.MaxAreaRelError {
			report.MaxAreaRelError = areaErr
		}
		if firstRegion == 0 && (delta > tol.CentroidPx || areaErr > tol.AreaRel) {
			firstRegion = id
		}
	}
	extraIDs := make([]int, 0)
	for id := range previewByID {
		if _, ok := exactByID[id]; !ok {
			extraIDs = append(extraIDs, id)
		}
	}
	sort.Ints(extraIDs)
	for _, id := range extraIDs {
		regionsAligned = false
		if firstRegion == 0 {
			firstRegion = id
		}
	}
	pixel := firstLabelDiff(preview.Labels, exact.Labels, exact.Width)
	if pixel != nil {
		report.FirstDiffPixel = pixel
	}
	report.FirstRegion = firstRegion
	report.WithinTolerance = report.RegionCountDiff <= tol.RegionCount &&
		report.MaxCentroidDeltaPx <= tol.CentroidPx &&
		report.MaxAreaRelError <= tol.AreaRel &&
		regionsAligned
	if report.WithinTolerance {
		return report, nil
	}
	details := map[string]any{
		"preview_region_count":  len(preview.Stats.Regions),
		"exact_region_count":    len(exact.Stats.Regions),
		"region_count_diff":     report.RegionCountDiff,
		"region_count_tol":      tol.RegionCount,
		"max_centroid_delta_px": report.MaxCentroidDeltaPx,
		"centroid_px_tol":       tol.CentroidPx,
		"max_area_rel_error":    report.MaxAreaRelError,
		"area_rel_tol":          tol.AreaRel,
		"first_region":          firstRegion,
	}
	if pixel != nil {
		details["first_diff_pixel"] = pixel
	}
	return report, apperr.New(apperr.CodeDiverge, "compare", "preview and exact results diverged").
		WithDetails(details)
}

func firstLabelDiff(a, b []byte, width int) *model.Point {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if width <= 0 {
				return &model.Point{X: i, Y: 0}
			}
			return &model.Point{X: i % width, Y: i / width}
		}
	}
	if len(a) != len(b) {
		if width <= 0 {
			return &model.Point{X: n, Y: 0}
		}
		return &model.Point{X: n % width, Y: n / width}
	}
	return nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
