package workbench

import (
	"fmt"
	"math"
	"strings"
	"time"
)

func nowMS() float64 {
	return float64(time.Now().UnixNano()) / 1e6
}

// DecideScale creates an explainable strategy decision. The tiled path uses
// downsampled statistics for scheduling and exposure diagnostics, then
// refines radiance and color at native resolution per block. This preserves
// pixel fidelity while bounding live working memory.
func DecideScale(meta Metadata, cfg ProcessConfig) ScaleDecision {
	cfg = cfg.normalized()
	pixels := int64(meta.Width) * int64(meta.Height)
	effective := pixels * int64(meta.BandCount)
	estimatedMemory := int64(1024*1024) +
		pixels*int64(2*meta.BandCount+2*meta.ExposureCount+8+8+24+3+1)
	estimatedFullMS := float64(effective) * 0.000012
	memoryBudget := int64(cfg.MemoryBudgetMB) * 1024 * 1024

	tileArea := minInt(cfg.BlockPixels, cfg.PixelBufferLimit)
	adaptiveTile := false
	if pixels > 1_000_000 || effective > 4_000_000 {
		expanded := minInt(cfg.PixelBufferLimit, 65_536)
		if expanded > tileArea {
			tileArea = expanded
			adaptiveTile = true
		}
	}
	side := int(math.Sqrt(float64(tileArea)))
	if side < 8 {
		side = 8
	}
	if side > 512 {
		side = 512
	}
	tileW := minInt(side, maxInt(8, meta.Width))
	tileH := minInt(maxInt(8, tileArea/maxInt(1, tileW)), maxInt(8, meta.Height))
	if tileH > 512 {
		tileH = 512
	}
	downsample := cfg.DownsampleFactor
	if downsample < 1 {
		downsample = 1
	}

	selected := PathFull
	var reasons []string
	switch cfg.Path {
	case PathFull:
		selected = PathFull
		reasons = append(reasons, "operator forced the high-precision full-image path")
	case PathTiled:
		selected = PathTiled
		reasons = append(reasons, "operator forced the tiled path")
	default:
		switch {
		case effective > 4_000_000:
			selected = PathTiled
			reasons = append(reasons, fmt.Sprintf("effective band-pixels %d exceed the 4,000,000 full-frame threshold", effective))
		case estimatedMemory > memoryBudget*3/4:
			selected = PathTiled
			reasons = append(reasons, fmt.Sprintf("estimated working set %.1f MiB exceeds 75%% of the %d MiB budget", float64(estimatedMemory)/(1024*1024), cfg.MemoryBudgetMB))
		case estimatedFullMS > float64(cfg.TimeBudgetMS):
			selected = PathTiled
			reasons = append(reasons, fmt.Sprintf("estimated full pass %.1f ms exceeds the %d ms budget", estimatedFullMS, cfg.TimeBudgetMS))
		default:
			reasons = append(reasons, fmt.Sprintf("effective band-pixels %d, %.1f MiB working set and %.1f ms estimate fit the full-frame budget", effective, float64(estimatedMemory)/(1024*1024), estimatedFullMS))
		}
	}
	if selected == PathTiled {
		if cfg.Path == PathTiled || downsample == 1 {
			auto := int(math.Ceil(math.Sqrt(float64(effective) / 4_000_000)))
			if auto > 1 {
				downsample = clampInt(auto, 1, 8)
			}
		}
		reasons = append(reasons,
			fmt.Sprintf("tile geometry %dx%d (<= %d px/thread buffer)", tileW, tileH, cfg.PixelBufferLimit),
			fmt.Sprintf("downsample x%d supplies coarse exposure statistics; native-radiance refinement keeps final pixels within tolerance", downsample),
		)
		if adaptiveTile {
			reasons = append(reasons, fmt.Sprintf("tile area expanded to %d px to bound checkpoint count for %.1fM pixels", tileArea, float64(pixels)/1e6))
		}
	} else {
		downsample = 1
		reasons = append(reasons, "native full-frame processing selected; downsample stays at 1")
	}
	if estimatedMemory > memoryBudget*3/4 && selected == PathFull {
		reasons = append(reasons, "warning: forced full path operates above the preferred memory envelope")
	}
	return ScaleDecision{
		RequestedPath:       cfg.Path,
		SelectedPath:        selected,
		IsTiled:             selected == PathTiled,
		Rationale:           strings.Join(reasons, "; "),
		Reasons:             reasons,
		PixelCount:          int(pixels),
		EffectivePixels:     effective,
		TileWidth:           tileW,
		TileHeight:          tileH,
		BlockPixels:         tileW * tileH,
		DownsampleFactor:    downsample,
		EstimatedMemoryByte: estimatedMemory,
		MemoryBudgetByte:    memoryBudget,
		EstimatedFullMS:     estimatedFullMS,
		TimeBudgetMS:        cfg.TimeBudgetMS,
	}
}

// populateCoarseStats performs the actual downsampled exposure pass used by
// the tiled strategy. It supplies global exposure diagnostics and queue
// scheduling context without changing native-resolution pixel synthesis.
func populateCoarseStats(scene *Scene, scale *ScaleDecision, cfg ProcessConfig) {
	if scene == nil || scale == nil || !scale.IsTiled {
		return
	}
	step := maxInt(1, scale.DownsampleFactor)
	cfg = cfg.normalized()
	var sum, peak float64
	count := 0
	for y := step / 2; y < scene.Meta.Height; y += step {
		for x := step / 2; x < scene.Meta.Width; x += step {
			fused := fuseExposurePixel(scene, x, y, cfg)
			sum += fused
			peak = math.Max(peak, fused)
			count++
		}
	}
	if count > 0 {
		scale.CoarseSamples = count
		scale.CoarseMeanRadiance = sum / float64(count)
		scale.CoarsePeakRadiance = peak
	}
}

// CompareRenderResults verifies all emitted pixels, peak radiance and the
// observable out-of-gamut set. The first failure is reported as E_SCALE with
// the block id and three measured metrics.
func CompareRenderResults(a, b *RenderResult, blockW, blockH int, tolerance Tolerance) (*PathComparison, error) {
	if a == nil || b == nil {
		return nil, wbErr(CodeScale, "cannot compare nil render results")
	}
	if a.Width != b.Width || a.Height != b.Height {
		return nil, fieldErr(CodeScale, "dimensions", []int{b.Width, b.Height}, []int{a.Width, a.Height}, "render dimensions differ")
	}
	if len(a.FusedRadiance) != len(b.FusedRadiance) ||
		len(a.LinearRGB) != len(b.LinearRGB) {
		return nil, wbErr(CodeScale, "render buffers have different lengths")
	}
	if blockW <= 0 {
		blockW = 64
	}
	if blockH <= 0 {
		blockH = 64
	}
	report := &PathComparison{
		PathA:      a.Path,
		PathB:      b.Path,
		Consistent: true,
		Metrics: map[string]float64{
			"fusedMaxAbs":   0,
			"rgbMaxAbs":     0,
			"displayMaxAbs": 0,
			"gamutSetDiff":  0,
		},
	}
	var firstX, firstY = -1, -1
	for i := range a.FusedRadiance {
		d := math.Abs(a.FusedRadiance[i] - b.FusedRadiance[i])
		if d > report.Metrics["fusedMaxAbs"] {
			report.Metrics["fusedMaxAbs"] = d
		}
		if firstX < 0 && d > tolerance.PathPixel {
			firstX = i % a.Width
			firstY = i / a.Width
		}
	}
	for i := range a.LinearRGB {
		d := math.Abs(a.LinearRGB[i] - b.LinearRGB[i])
		if d > report.Metrics["rgbMaxAbs"] {
			report.Metrics["rgbMaxAbs"] = d
		}
		if firstX < 0 && d > tolerance.PathPixel {
			firstX = (i / 3) % a.Width
			firstY = (i / 3) / a.Width
		}
	}
	if len(a.DisplayRGB) != len(b.DisplayRGB) {
		report.Metrics["displayMaxAbs"] = 255
	} else {
		for i := range a.DisplayRGB {
			d := math.Abs(float64(a.DisplayRGB[i]) - float64(b.DisplayRGB[i]))
			if d > report.Metrics["displayMaxAbs"] {
				report.Metrics["displayMaxAbs"] = d
			}
			if firstX < 0 && d > tolerance.PathPixel {
				firstX = (i / 3) % a.Width
				firstY = (i / 3) / a.Width
			}
		}
	}
	if a.OutOfGamutHash != b.OutOfGamutHash || len(a.GamutMask) != len(b.GamutMask) {
		report.Metrics["gamutSetDiff"] = 1
		if firstX < 0 && len(a.GamutMask) == len(b.GamutMask) {
			for i := range a.GamutMask {
				if a.GamutMask[i] != b.GamutMask[i] {
					firstX = i % a.Width
					firstY = i / a.Width
					break
				}
			}
		}
	}
	report.PeakDiff = math.Abs(a.PeakRadiance - b.PeakRadiance)
	report.GamutSetMatch = report.Metrics["gamutSetDiff"] == 0
	ok := firstX < 0 &&
		report.PeakDiff <= math.Max(tolerance.PathPixel, a.PeakRadiance*tolerance.PeakRadianceRel) &&
		report.GamutSetMatch &&
		report.Metrics["displayMaxAbs"] <= tolerance.PathPixel
	if ok {
		return report, nil
	}
	report.Consistent = false
	coord := PixelCoord{X: maxInt(0, firstX), Y: maxInt(0, firstY)}
	report.FirstMismatch = &coord
	block := (coord.Y/blockH)*(maxInt(1, (a.Width+blockW-1)/blockW)) + coord.X/blockW
	report.FirstMismatchBlock = block
	e := fieldErr(CodeScale, "pathComparison", report.Metrics, fmt.Sprintf("<= %.3g", tolerance.PathPixel), "tiled and full paths exceed the declared pixel tolerance")
	e.Pixel = &coord
	e.Segment = block
	e.Metrics = report.Metrics
	return report, e
}
