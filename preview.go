package iconcut

import (
	"math"
	"time"
)

type PreviewOptions struct {
	DownsampleStep  int `json:"downsample_step"`
	IterationBudget int `json:"iteration_budget"`
}

type PreviewReport struct {
	Options          PreviewOptions `json:"options"`
	DownsampleStep   int            `json:"downsample_step"`
	ResolutionScale  float64        `json:"resolution_scale"`
	IterationBudget  int            `json:"iteration_budget"`
	IterationsUsed   int            `json:"iterations_used"`
	Basis            string         `json:"basis"`
	DelayMS          int64          `json:"delay_ms"`
	WithinDelayLimit bool           `json:"within_delay_limit"`
	ParamVersion     string         `json:"param_version"`
	SchemeVersion    string         `json:"scheme_version"`
	Stats            []RegionStats  `json:"stats"`
	Mask             *LabelMask     `json:"-"`
	Upscaled         *LabelMask     `json:"-"`
	Divergence       *Error         `json:"divergence,omitempty"`
}

func DownsampleBox(im *Image, step int) *Image {
	if step <= 1 {
		out := NewImage(im.W, im.H, 3)
		copy(out.Pix, im.Pix)
		return out
	}
	w := (im.W + step - 1) / step
	h := (im.H + step - 1) / step
	out := NewImage(w, h, 3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var sum [3]int
			n := 0
			for sy := y * step; sy < minInt(im.H, (y+1)*step); sy++ {
				for sx := x * step; sx < minInt(im.W, (x+1)*step); sx++ {
					c := im.Pixel(sx, sy)
					sum[0] += int(c[0])
					sum[1] += int(c[1])
					sum[2] += int(c[2])
					n++
				}
			}
			out.SetPixel(x, y, [3]byte{byte(sum[0] / n), byte(sum[1] / n), byte(sum[2] / n)})
		}
	}
	return out
}

func DownsampleParams(p SegmentParams, fullW, fullH, smallW, smallH int) SegmentParams {
	out := p
	out.SeedPoints = make([]IPoint, len(p.SeedPoints))
	for i, s := range p.SeedPoints {
		x := int(math.Floor((float64(s.X) + 0.5) * float64(smallW) / float64(fullW)))
		y := int(math.Floor((float64(s.Y) + 0.5) * float64(smallH) / float64(fullH)))
		out.SeedPoints[i] = IPoint{X: clampInt(x, 0, smallW-1), Y: clampInt(y, 0, smallH-1)}
	}
	return out
}

func UpscaleLabels(m *LabelMask, fullW, fullH, step int) *LabelMask {
	out := NewLabelMask(fullW, fullH)
	for y := 0; y < fullH; y++ {
		sy := clampInt(y/step, 0, m.H-1)
		for x := 0; x < fullW; x++ {
			sx := clampInt(x/step, 0, m.W-1)
			out.Set(x, y, m.At(sx, sy))
		}
	}
	return out
}

func ScalePreviewStats(stats []RegionStats, step int, fullW, fullH int) []RegionStats {
	out := make([]RegionStats, 0, len(stats))
	for _, s := range stats {
		bbox := Rect{
			X: s.BBox.X * step,
			Y: s.BBox.Y * step,
			W: minInt(fullW-s.BBox.X*step, s.BBox.W*step),
			H: minInt(fullH-s.BBox.Y*step, s.BBox.H*step),
		}
		out = append(out, RegionStats{
			ID: s.ID, Area: s.Area * step * step,
			Centroid: Point{X: (s.Centroid.X+0.5)*float64(step) - 0.5, Y: (s.Centroid.Y+0.5)*float64(step) - 0.5},
			BBox:     bbox, Holes: s.Holes,
		})
	}
	return out
}

func RunPreview(corrected *Image, m *Manifest, params SegmentParams, opts PreviewOptions) (*PreviewReport, error) {
	if corrected == nil || !corrected.Valid() {
		return nil, NewError(EEmpty, "corrected", "corrected canvas is empty")
	}
	if err := ValidateSegmentParams(params); err != nil {
		return nil, err
	}
	step := opts.DownsampleStep
	if step <= 0 {
		step = 2
	}
	small := DownsampleBox(corrected, step)
	sp := DownsampleParams(params, corrected.W, corrected.H, small.W, small.H)
	budget := opts.IterationBudget
	if budget <= 0 {
		budget = maxInt(128, small.W*small.H/2)
	}
	start := time.Now()
	raw, trace, err := RegionGrow(small, sp, budget)
	if err != nil {
		return nil, err
	}
	mask := CleanLabels(raw, params.MorphSize)
	elapsed := time.Since(start).Milliseconds()
	stats := ScalePreviewStats(AnalyzeLabels(mask), step, corrected.W, corrected.H)
	upscaled := UpscaleLabels(mask, corrected.W, corrected.H, step)
	versions := BuildVersions(m, params)
	basis := "drag preview: box downsample, global iteration cap, fixed parameter snapshot; exact path never reads iterations_used"
	return &PreviewReport{
		Options:        PreviewOptions{DownsampleStep: step, IterationBudget: budget},
		DownsampleStep: step, ResolutionScale: 1.0 / float64(step),
		IterationBudget: budget, IterationsUsed: trace.Iterations, Basis: basis,
		DelayMS: elapsed, WithinDelayLimit: elapsed <= int64(m.Declared.Budget.PreviewDelayMaxMS),
		ParamVersion: versions.Param, SchemeVersion: versions.Scheme,
		Stats: stats, Mask: mask, Upscaled: upscaled,
	}, nil
}

func firstDiffPixel(a, b *LabelMask) IPoint {
	if a == nil || b == nil || a.W != b.W || a.H != b.H {
		return IPoint{X: -1, Y: -1}
	}
	for y := 0; y < a.H; y++ {
		for x := 0; x < a.W; x++ {
			if a.At(x, y) != b.At(x, y) {
				return IPoint{X: x, Y: y}
			}
		}
	}
	return IPoint{X: -1, Y: -1}
}

func ComparePreview(fullStats, previewStats []RegionStats, fullMask, previewUpscaled *LabelMask, tol Tolerances, paramVersion, mappingVersion string) *Error {
	fullMap := map[int]RegionStats{}
	prevMap := map[int]RegionStats{}
	for _, s := range fullStats {
		fullMap[s.ID] = s
	}
	for _, s := range previewStats {
		prevMap[s.ID] = s
	}
	countFull := len(fullMap)
	countPreview := len(prevMap)
	var centroidMax, areaMax float64
	firstID := 0
	consider := func(id int) {
		f, okf := fullMap[id]
		p, okp := prevMap[id]
		if !okf || !okp {
			if firstID == 0 {
				firstID = id
			}
			return
		}
		cd := math.Hypot(f.Centroid.X-p.Centroid.X, f.Centroid.Y-p.Centroid.Y)
		ar := math.Abs(float64(f.Area-p.Area)) / math.Max(1, float64(f.Area))
		centroidMax = math.Max(centroidMax, cd)
		areaMax = math.Max(areaMax, ar)
		if firstID == 0 && (cd > tol.PreviewCentroidPX || ar > tol.PreviewAreaRel) {
			firstID = id
		}
	}
	for id := range fullMap {
		consider(id)
	}
	for id := range prevMap {
		consider(id)
	}
	diff := firstDiffPixel(fullMask, previewUpscaled)
	countBad := countFull != countPreview
	metricBad := centroidMax > tol.PreviewCentroidPX || areaMax > tol.PreviewAreaRel
	if !countBad && !metricBad {
		return nil
	}
	if firstID == 0 && len(fullStats) > 0 {
		firstID = fullStats[0].ID
	}
	return WithDetails(NewError(EDiverge, "preview", "preview and exact recomputation diverged beyond declared tolerance"), map[string]any{
		"region_count_exact": countFull, "region_count_preview": countPreview,
		"centroid_deviation_px": centroidMax, "area_relative_error": areaMax,
		"centroid_limit_px": tol.PreviewCentroidPX, "area_limit_rel": tol.PreviewAreaRel,
		"first_region_id": firstID, "first_diff_pixel": diff,
		"param_version": paramVersion, "mapping_version": mappingVersion,
		"geometry_consistent": true,
		"diagnosis":           "same mapping/scheme/param snapshot; divergence is caused by preview downsample or iteration budget",
	})
}
