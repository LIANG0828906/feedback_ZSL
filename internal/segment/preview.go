package segment

import (
	"fmt"
	"math"
	"time"

	"maskpreview/internal/core"
)

type PreviewResult struct {
	Mask                  []uint8               `json:"-"`
	ProjectedMask         []uint8               `json:"-"`
	Width                 int                   `json:"width"`
	Height                int                   `json:"height"`
	Regions               []RegionStat          `json:"regions"`
	ProjectedRegions      []RegionStat          `json:"projected_regions"`
	SnapshotVersion       string                `json:"snapshot_version"`
	Quantization          core.QuantizationRule `json:"quantization"`
	DownsampleFactor      int                   `json:"downsample_factor"`
	ScaledSeeds           []core.Point          `json:"scaled_seeds"`
	GrowthIterations      int                   `json:"growth_iterations_used"`
	BudgetMS              int                   `json:"budget_ms"`
	ElapsedMS             float64               `json:"elapsed_ms"`
	WithinBudget          bool                  `json:"within_budget"`
	StatisticsPlaceholder bool                  `json:"statistics_placeholder"`
	Rationale             string                `json:"rationale"`
}

func Preview(img *core.Image, snap core.Snapshot, budgetMS int) (*PreviewResult, error) {
	if err := img.Validate(); err != nil {
		return nil, err
	}
	if err := snap.Validate(); err != nil {
		return nil, err
	}
	if budgetMS <= 0 {
		budgetMS = core.DefaultPreviewDelayMS
	}
	start := time.Now()
	targetDim := 48
	if budgetMS <= 20 {
		targetDim = 32
	}
	if budgetMS <= 5 {
		targetDim = 16
	}
	maxDim := img.Width
	if img.Height > maxDim {
		maxDim = img.Height
	}
	factor := (maxDim + targetDim - 1) / targetDim
	if factor < 2 {
		factor = 2
	}
	iterations := snap.Params.GrowthIterations
	if iterations > 12 {
		iterations = 12
	}
	if budgetMS <= 20 && iterations > 8 {
		iterations = 8
	}
	if budgetMS <= 5 && iterations > 4 {
		iterations = 4
	}

	scaled := downsample(img, factor)
	scaledSeeds := make([]core.Point, 0, len(snap.Seeds))
	for _, seed := range snap.Seeds {
		x := seed.X / factor
		y := seed.Y / factor
		if x < 0 {
			x = 0
		}
		if y < 0 {
			y = 0
		}
		if x >= scaled.Width {
			x = scaled.Width - 1
		}
		if y >= scaled.Height {
			y = scaled.Height - 1
		}
		scaledSeeds = append(scaledSeeds, core.Point{X: x, Y: y})
	}
	previewSnap := snap.Clone()
	previewSnap.Seeds = scaledSeeds
	exact, err := runKernel(scaled, previewSnap, iterations)
	if err != nil {
		return nil, err
	}
	projectedMask := upscaleMask(exact.Mask, scaled.Width, scaled.Height, factor, img.Width, img.Height)
	projectedRegions := projectRegions(exact.Regions, factor, img.Width, img.Height)
	elapsed := time.Since(start)
	reason := fmt.Sprintf(
		"declared=%dms targetMaxDim=%d maxDim=%d factor=ceil(maxDim/targetMaxDim)=%d iterations=%d; preview and exact share snapshot version, seed points (scaled only), and %d-bit quantization",
		budgetMS, targetDim, maxDim, factor, iterations, snap.Quantization.ChannelBits,
	)
	return &PreviewResult{
		Mask:                  exact.Mask,
		ProjectedMask:         projectedMask,
		Width:                 scaled.Width,
		Height:                scaled.Height,
		Regions:               exact.Regions,
		ProjectedRegions:      projectedRegions,
		SnapshotVersion:       snap.Version,
		Quantization:          snap.Quantization,
		DownsampleFactor:      factor,
		ScaledSeeds:           scaledSeeds,
		GrowthIterations:      exact.IterationsUsed,
		BudgetMS:              budgetMS,
		ElapsedMS:             float64(elapsed.Microseconds()) / 1000,
		WithinBudget:          elapsed <= time.Duration(budgetMS)*time.Millisecond,
		StatisticsPlaceholder: true,
		Rationale:             reason,
	}, nil
}

type PreviewComparison struct {
	OK                   bool          `json:"ok"`
	ExactRegionCount     int           `json:"exact_region_count"`
	PreviewRegionCount   int           `json:"preview_region_count"`
	RegionCountMismatch  int           `json:"region_count_mismatch"`
	CentroidDeviation    float64       `json:"centroid_deviation"`
	AreaRelative         float64       `json:"area_relative"`
	MaxCentroidLimit     float64       `json:"max_centroid_limit"`
	MaxAreaRelativeLimit float64       `json:"max_area_relative_limit"`
	FirstRegionID        int           `json:"first_region_id"`
	DifferenceX          int           `json:"difference_x"`
	DifferenceY          int           `json:"difference_y"`
	HasPixelDifference   bool          `json:"has_pixel_difference"`
	Matches              []RegionDelta `json:"matches,omitempty"`
}

func ComparePreview(exact *ExactResult, preview *PreviewResult, tol core.PreviewTolerance) (*PreviewComparison, error) {
	if exact == nil || preview == nil {
		return nil, fmt.Errorf("exact and preview results are required")
	}
	if exact.Snapshot.Version != preview.SnapshotVersion {
		return nil, core.VersionError("segment.compare_preview", preview.SnapshotVersion, exact.Snapshot.Version)
	}
	if exact.Snapshot.Quantization != preview.Quantization {
		return nil, &core.Error{
			Code:    core.CodeVersion,
			Op:      "segment.compare_preview",
			Field:   "quantization",
			Got:     preview.Quantization,
			Want:    exact.Snapshot.Quantization,
			Message: "preview and exact quantization rules differ",
		}
	}
	cmp := &PreviewComparison{
		ExactRegionCount:     len(exact.Regions),
		PreviewRegionCount:   len(preview.ProjectedRegions),
		MaxCentroidLimit:     tol.CentroidPx,
		MaxAreaRelativeLimit: tol.AreaRelative,
	}
	cmp.RegionCountMismatch = absInt(cmp.ExactRegionCount - cmp.PreviewRegionCount)
	if cmp.ExactRegionCount == cmp.PreviewRegionCount {
		cmp.Matches = MatchRegions(preview.ProjectedRegions, exact.Regions)
		for _, m := range cmp.Matches {
			if m.CentroidDeviation > cmp.CentroidDeviation {
				cmp.CentroidDeviation = m.CentroidDeviation
			}
			if m.AreaRelative > cmp.AreaRelative {
				cmp.AreaRelative = m.AreaRelative
			}
			if cmp.FirstRegionID == 0 {
				cmp.FirstRegionID = m.RegionID
			}
		}
	}
	cmp.DifferenceX, cmp.DifferenceY, cmp.HasPixelDifference = firstMaskDifference(exact, preview.ProjectedMask)
	if exact.LabelMask != nil && cmp.DifferenceX >= 0 && cmp.DifferenceY >= 0 {
		if cmp.DifferenceY < exactImageHeight(exact) && cmp.DifferenceX < exactImageWidth(exact) {
			id := exact.LabelMask[cmp.DifferenceY*exactImageWidth(exact)+cmp.DifferenceX]
			if id != 0 {
				cmp.FirstRegionID = id
			}
		}
	}
	if cmp.FirstRegionID == 0 && cmp.DifferenceX >= 0 && cmp.DifferenceY >= 0 {
		if id := regionAt(cmp.DifferenceX, cmp.DifferenceY, preview.ProjectedRegions); id != 0 {
			cmp.FirstRegionID = id
		}
	}
	if !cmp.HasPixelDifference && len(cmp.Matches) > 0 {
		first := cmp.Matches[0]
		cmp.FirstRegionID = first.RegionID
		for _, region := range exact.Regions {
			if region.ID == first.RegionID {
				cmp.DifferenceX = int(math.Floor(region.Centroid.X))
				cmp.DifferenceY = int(math.Floor(region.Centroid.Y))
				break
			}
		}
	}
	cmp.OK = cmp.RegionCountMismatch == 0 &&
		cmp.CentroidDeviation <= tol.CentroidPx &&
		cmp.AreaRelative <= tol.AreaRelative
	if cmp.OK {
		return cmp, nil
	}
	return cmp, &core.Error{
		Code:    core.CodeDiverge,
		Op:      "segment.compare_preview",
		Message: "preview exceeded declared consistency tolerance",
		Data: map[string]any{
			"region_count":       cmp.PreviewRegionCount,
			"exact_region_count": cmp.ExactRegionCount,
			"region_count_delta": cmp.RegionCountMismatch,
			"centroid_deviation": cmp.CentroidDeviation,
			"area_relative":      cmp.AreaRelative,
			"centroid_limit":     tol.CentroidPx,
			"area_limit":         tol.AreaRelative,
			"first_region_id":    cmp.FirstRegionID,
			"difference_x":       cmp.DifferenceX,
			"difference_y":       cmp.DifferenceY,
		},
	}
}

func regionAt(x, y int, regions []RegionStat) int {
	for _, region := range regions {
		if x >= region.BBox.X0 && x <= region.BBox.X1 && y >= region.BBox.Y0 && y <= region.BBox.Y1 {
			return region.ID
		}
	}
	bestID := 0
	best := math.Inf(1)
	for _, region := range regions {
		d := math.Hypot(region.Centroid.X-float64(x)-0.5, region.Centroid.Y-float64(y)-0.5)
		if d < best {
			best = d
			bestID = region.ID
		}
	}
	return bestID
}

func downsample(img *core.Image, factor int) *core.Image {
	w := (img.Width + factor - 1) / factor
	h := (img.Height + factor - 1) / factor
	out := &core.Image{ID: img.ID, Width: w, Height: h, Channels: 4, Pix: make([]uint8, w*h*4)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Point sampling is deliberate: averaging collapses the 18-level
			// gradient used by the low-contrast sample and would make the preview
			// diverge for reasons unrelated to the declared parameters. The
			// low-resolution path still uses the same color and gradient rules.
			sx := minInt(img.Width-1, x*factor+factor/2)
			sy := minInt(img.Height-1, y*factor+factor/2)
			srcOff := (sy*img.Width + sx) * 4
			off := (y*w + x) * 4
			out.Pix[off] = img.Pix[srcOff]
			out.Pix[off+1] = img.Pix[srcOff+1]
			out.Pix[off+2] = img.Pix[srcOff+2]
			out.Pix[off+3] = img.Pix[srcOff+3]
		}
	}
	return out
}

func upscaleMask(mask []uint8, srcW, srcH, factor, dstW, dstH int) []uint8 {
	out := make([]uint8, dstW*dstH)
	for y := 0; y < dstH; y++ {
		sy := y / factor
		if sy >= srcH {
			sy = srcH - 1
		}
		for x := 0; x < dstW; x++ {
			sx := x / factor
			if sx >= srcW {
				sx = srcW - 1
			}
			if mask[sy*srcW+sx] != 0 {
				out[y*dstW+x] = 1
			}
		}
	}
	return out
}

func projectRegions(regions []RegionStat, factor, width, height int) []RegionStat {
	out := make([]RegionStat, 0, len(regions))
	for _, r := range regions {
		proj := RegionStat{
			ID:       r.ID,
			Area:     r.Area * factor * factor,
			Centroid: core.PointF{X: r.Centroid.X * float64(factor), Y: r.Centroid.Y * float64(factor)},
			BBox: core.BBox{
				X0: r.BBox.X0 * factor,
				Y0: r.BBox.Y0 * factor,
				X1: minInt(width-1, (r.BBox.X1+1)*factor-1),
				Y1: minInt(height-1, (r.BBox.Y1+1)*factor-1),
			},
		}
		if len(r.Contour) > 0 {
			proj.Contour = make([]core.Point, len(r.Contour))
			for i, p := range r.Contour {
				proj.Contour[i] = core.Point{X: p.X * factor, Y: p.Y * factor}
			}
		}
		out = append(out, proj)
	}
	return out
}

func firstMaskDifference(exact *ExactResult, previewMask []uint8) (int, int, bool) {
	exactMask := exact.Mask
	n := len(exactMask)
	if len(previewMask) < n {
		n = len(previewMask)
	}
	width := exactImageWidth(exact)
	for i := 0; i < n; i++ {
		if exactMask[i] != previewMask[i] {
			if width <= 0 {
				return 0, 0, true
			}
			return i % width, i / width, true
		}
	}
	if len(exactMask) != len(previewMask) {
		return 0, 0, true
	}
	return -1, -1, false
}

func exactImageWidth(exact *ExactResult) int {
	if exact == nil {
		return 0
	}
	return exact.Width
}

func exactImageHeight(exact *ExactResult) int {
	w := exactImageWidth(exact)
	if w == 0 {
		return 0
	}
	return len(exact.LabelMask) / w
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
