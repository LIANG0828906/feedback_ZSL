package pipeline

import (
	"fmt"

	"maskpipe/internal/model"
	"maskpipe/internal/vision"
)

type RegionDivergence struct {
	RegionID          int     `json:"region_id"`
	TruthID           int     `json:"truth_id,omitempty"`
	CentroidError     float64 `json:"centroid_error_px"`
	AreaRelativeError float64 `json:"area_relative_error"`
}

type PixelLocation struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type DivergenceReport struct {
	OK                     bool               `json:"ok"`
	RegionCountDelta       int                `json:"region_count_delta"`
	FullRegionCount        int                `json:"full_region_count"`
	PreviewRegionCount     int                `json:"preview_region_count"`
	MaxCentroidError       float64            `json:"max_centroid_error_px"`
	MaxAreaRelativeError   float64            `json:"max_area_relative_error"`
	PixelMismatchCount     int                `json:"pixel_mismatch_count"`
	PixelMismatchRatio     float64            `json:"pixel_mismatch_ratio"`
	FirstMismatchPixel     *PixelLocation     `json:"first_mismatch_pixel,omitempty"`
	MaxDifferencePixel     *PixelLocation     `json:"max_difference_pixel,omitempty"`
	FirstDivergentRegionID int                `json:"first_divergent_region_id"`
	Regions                []RegionDivergence `json:"regions,omitempty"`
	Error                  *model.Error       `json:"error,omitempty"`
}

func ComparePreview(fullMask, previewMask []byte, width, height int, fullRegions, previewRegions []model.Region, tol model.DivergenceTolerance) *DivergenceReport {
	report := &DivergenceReport{
		OK: true, FullRegionCount: len(fullRegions), PreviewRegionCount: len(previewRegions),
		FirstDivergentRegionID: -1,
	}
	report.RegionCountDelta = len(previewRegions) - len(fullRegions)

	pairs := vision.OptimalMatches(fullRegions, previewRegions)
	for _, pair := range pairs {
		full := fullRegions[pair[0]]
		preview := previewRegions[pair[1]]
		item := RegionDivergence{
			RegionID:          full.ID,
			CentroidError:     vision.CentroidDistance(full.Centroid, preview.Centroid),
			AreaRelativeError: vision.AreaRelativeError(preview.Area, full.Area),
		}
		report.Regions = append(report.Regions, item)
		if item.CentroidError > report.MaxCentroidError {
			report.MaxCentroidError = item.CentroidError
		}
		if item.AreaRelativeError > report.MaxAreaRelativeError {
			report.MaxAreaRelativeError = item.AreaRelativeError
		}
		if report.FirstDivergentRegionID == -1 &&
			(item.CentroidError > tol.CentroidPixels || item.AreaRelativeError > tol.AreaRelative) {
			report.FirstDivergentRegionID = full.ID
		}
	}
	if report.RegionCountDelta != 0 && report.FirstDivergentRegionID == -1 {
		if len(previewRegions) > len(fullRegions) {
			report.FirstDivergentRegionID = previewRegions[len(fullRegions)].ID
		} else if len(fullRegions) > 0 {
			report.FirstDivergentRegionID = fullRegions[minInt(len(fullRegions)-1, len(previewRegions))].ID
		}
	}

	total := width * height
	bestDensity := -1
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := y*width + x
			if fullMask[i] == previewMask[i] {
				continue
			}
			report.PixelMismatchCount++
			if report.FirstMismatchPixel == nil {
				report.FirstMismatchPixel = &PixelLocation{X: x, Y: y}
			}
			density := mismatchDensity(fullMask, previewMask, width, height, x, y)
			if density > bestDensity {
				bestDensity = density
				report.MaxDifferencePixel = &PixelLocation{X: x, Y: y}
			}
		}
	}
	if total > 0 {
		report.PixelMismatchRatio = float64(report.PixelMismatchCount) / float64(total)
	}

	if report.RegionCountDelta != 0 ||
		report.MaxCentroidError > tol.CentroidPixels ||
		report.MaxAreaRelativeError > tol.AreaRelative ||
		report.PixelMismatchRatio > tol.PixelMismatch {
		report.OK = false
		values := map[string]any{
			"region_count_delta":        report.RegionCountDelta,
			"max_centroid_error_px":     report.MaxCentroidError,
			"max_area_relative_error":   report.MaxAreaRelativeError,
			"pixel_mismatch_ratio":      report.PixelMismatchRatio,
			"first_divergent_region_id": report.FirstDivergentRegionID,
			"max_difference_pixel":      report.MaxDifferencePixel,
			"first_mismatch_pixel":      report.FirstMismatchPixel,
		}
		report.Error = model.WithDetails(model.NewError(model.CodeDiverge, "preview",
			"preview/full divergence: count_delta=%d centroid=%.3f area_rel=%.3f",
			report.RegionCountDelta, report.MaxCentroidError, report.MaxAreaRelativeError), values)
	}
	return report
}

func mismatchDensity(a, b []byte, w, h, cx, cy int) int {
	count := 0
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || x >= w || y < 0 || y >= h {
				continue
			}
			i := y*w + x
			if a[i] != b[i] {
				count++
			}
		}
	}
	return count
}

type PathComparison struct {
	Name                 string         `json:"name"`
	OK                   bool           `json:"ok"`
	MaskHash             string         `json:"mask_hash"`
	GeometryHash         string         `json:"geometry_hash"`
	SequenceLength       int            `json:"sequence_length"`
	FirstMaskMismatch    *PixelLocation `json:"first_mask_mismatch,omitempty"`
	FirstStageDifference string         `json:"first_stage_difference,omitempty"`
	Message              string         `json:"message,omitempty"`
}

type ThreeWayReport struct {
	OK          bool             `json:"ok"`
	Base        string           `json:"base"`
	Comparisons []PathComparison `json:"comparisons"`
}

func CompareThree(baseName string, base *Result, others map[string]*Result) *ThreeWayReport {
	report := &ThreeWayReport{OK: true, Base: baseName}
	for name, other := range others {
		cmp := PathComparison{Name: name, OK: true, MaskHash: other.MaskHash, GeometryHash: other.GeometryHash, SequenceLength: len(other.Sequence)}
		if len(base.Mask) != len(other.Mask) {
			cmp.OK = false
			cmp.Message = fmt.Sprintf("mask length %d != %d", len(base.Mask), len(other.Mask))
		} else {
			for i := range base.Mask {
				if base.Mask[i] != other.Mask[i] {
					cmp.OK = false
					x := i % base.Width
					y := i / base.Width
					cmp.FirstMaskMismatch = &PixelLocation{X: x, Y: y}
					break
				}
			}
		}
		if diff := firstSequenceDifference(base.Sequence, other.Sequence); diff != "" {
			cmp.OK = false
			cmp.FirstStageDifference = diff
			if cmp.Message == "" {
				cmp.Message = "stage sequence/hash mismatch at " + diff
			}
		}
		if base.GeometryHash != other.GeometryHash {
			cmp.OK = false
			cmp.Message = "geometry statistics differ"
		}
		if !cmp.OK {
			report.OK = false
		}
		report.Comparisons = append(report.Comparisons, cmp)
	}
	return report
}

func firstSequenceDifference(a, b []SequenceItem) string {
	if len(a) != len(b) {
		return fmt.Sprintf("sequence_length:%d!=%d", len(a), len(b))
	}
	for i := range a {
		if a[i].CanonicalOrder != b[i].CanonicalOrder ||
			a[i].ChunkX != b[i].ChunkX || a[i].ChunkY != b[i].ChunkY || a[i].Stage != b[i].Stage {
			return a[i].StageID()
		}
		if a[i].ContentHash != b[i].ContentHash {
			return a[i].StageID()
		}
	}
	return ""
}

func (s SequenceItem) StageID() string {
	return model.ChunkKey{ChunkCoord: model.ChunkCoord{X: s.ChunkX, Y: s.ChunkY}, Stage: s.Stage}.ID()
}
