package samplegen

import (
	"fmt"

	"fixedsampler/internal/fixedpoint"
	"fixedsampler/internal/model"
)

func analyticErrorBounds(p *model.Project) []model.TruthErrorBound {
	bits := map[string]int{}
	for _, plane := range p.Planes {
		bits[plane.Name] = plane.BitDepth
	}
	cum := [4]int64{}
	var out []model.TruthErrorBound
	for seq, stage := range p.Config {
		seq++
		gainNum, gainDen := truthStageGain(stage, bits)
		for i := range cum {
			if cum[i] > 0 {
				cum[i] = ceilBound(cum[i]*gainNum, gainDen)
			}
		}
		bases := [4]string{
			"cumulative fixed-point propagation; no new bound at this stage",
			"cumulative fixed-point propagation; no new bound at this stage",
			"no color conversion stage has contributed a residual yet",
			"no bit-depth stage has contributed a residual yet",
		}
		switch stage.Type {
		case "resample":
			maxBits := 0
			for _, name := range stage.Planes {
				maxBits = max(maxBits, bits[name])
			}
			taps := int64(stage.Taps)
			if taps == 0 {
				if stage.Kernel == "nearest" {
					taps = 1
				} else {
					taps = int64(stage.FactorX * stage.FactorY)
				}
			}
			vMax := fixedpoint.MaxCode(maxBits) * p.ValueScale
			weightBound := ceilBound(vMax*taps, stage.FixedScale)
			cum[0] += weightBound
			cum[1]++
			basis := fmt.Sprintf("Vmax=%d FX, taps=%d, weight_scale=%d, truncation divisor=%d", vMax, taps, stage.FixedScale, stage.FixedScale)
			bases[0] = basis
			bases[1] = basis
		case "color":
			cum[2]++
			bases[2] = fmt.Sprintf("matrix_scale=%d, one floor division per component", stage.Matrix.Scale)
			firstBits := bits[stage.InputNames[0]]
			for _, name := range stage.OutputNames {
				bits[name] = firstBits
			}
		case "quantize":
			cum[3] += (p.ValueScale + 1) / 2
			for _, name := range stage.Planes {
				bits[name] = stage.BitDepth
			}
			bases[3] = fmt.Sprintf("value_scale=%d, nearest rounding <= half output code", p.ValueScale)
		}
		categories := []string{
			"resample_weight_normalization",
			"downsample_accumulation_truncation",
			"color_conversion_quantization",
			"bit_depth_rounding",
		}
		for i, category := range categories {
			out = append(out, model.TruthErrorBound{
				StageSeq: seq, Category: category, CumulativeBoundFX: cum[i],
				Basis: bases[i],
			})
		}
	}
	return out
}

func truthStageGain(stage model.StageConfig, bits map[string]int) (int64, int64) {
	switch stage.Type {
	case "resample":
		return 1, 1
	case "color":
		var worst int64 = 1
		for _, row := range stage.Matrix.Rows {
			var sum int64
			for _, v := range row {
				sum += fixedpoint.Abs(v)
			}
			worst = max(worst, sum)
		}
		if worst < stage.Matrix.Scale {
			worst = stage.Matrix.Scale
		}
		return worst, stage.Matrix.Scale
	case "quantize":
		return 1, 1
	default:
		return 1, 1
	}
}

func ceilBound(a, b int64) int64 {
	if b <= 0 || a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}
