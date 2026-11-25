package engine

import (
	"context"
	"math"

	"uvenhance/internal/yuv"
)

type BlockErrorReport struct {
	Block                       string  `json:"block"`
	CenterX                     int     `json:"center_x"`
	CenterY                     int     `json:"center_y"`
	Samples                     int     `json:"samples"`
	ObservedChromaSecondDiff    float64 `json:"observed_chroma_second_difference"`
	ObservedChromaNeighborDiff  float64 `json:"observed_chroma_neighbor_difference"`
	ObservedLumaNeighborDiff    float64 `json:"observed_luma_neighbor_difference"`
	ChromaUpsampleError         float64 `json:"chroma_upsample_interpolation_error"`
	LumaSharpenContrastResidual float64 `json:"luma_sharpen_contrast_residual"`
	ChromaGainQuantizationError float64 `json:"chroma_gain_quantization_error"`
	InterleaveRoundingResidual  float64 `json:"interleave_rounding_residual"`
	CumulativeBoundQuantUnits   float64 `json:"cumulative_bound_quant_units"`
	CumulativeBoundNormalized   float64 `json:"cumulative_bound_normalized"`
	YBoundQuantUnits            float64 `json:"y_bound_quant_units"`
	UBoundQuantUnits            float64 `json:"u_bound_quant_units"`
	VBoundQuantUnits            float64 `json:"v_bound_quant_units"`
}

type ErrorReport struct {
	SampleID                     string             `json:"sample_id"`
	BitDepth                     int                `json:"bit_depth"`
	QuantMin                     uint16             `json:"quant_min"`
	QuantMax                     uint16             `json:"quant_max"`
	QuantStep                    float64            `json:"quantization_step"`
	ChromaSampleRatio            float64            `json:"chroma_sample_ratio_per_axis"`
	ChromaAreaRatio              float64            `json:"chroma_area_ratio"`
	InterpolationOrder           int                `json:"interpolation_order"`
	Upsample                     string             `json:"upsample"`
	ChromaAlignment              string             `json:"chroma_alignment"`
	UsedTruthDeviation           bool               `json:"used_truth_deviation"`
	BoundUsesObservedDifferences bool               `json:"bound_uses_observed_differences"`
	Basis                        []string           `json:"basis"`
	Formulas                     map[string]string  `json:"formulas"`
	Blocks                       []BlockErrorReport `json:"blocks"`
}

func BuildErrorReport(ctx context.Context, sample *yuv.Sample, params yuv.Params, result *RunResult) (*ErrorReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bitDepth := sample.Metadata.BitDepth
	fullRange := float64(sample.Metadata.QuantMax - sample.Metadata.QuantMin)
	levels := math.Pow(2, float64(bitDepth)) - 1
	quantStep := fullRange / levels
	ratio := 0.5
	order := yuv.InterpolationOrder(sample.Metadata.Upsample)
	report := &ErrorReport{
		SampleID: sample.Metadata.ID, BitDepth: bitDepth,
		QuantMin: sample.Metadata.QuantMin, QuantMax: sample.Metadata.QuantMax,
		QuantStep: quantStep, ChromaSampleRatio: ratio, ChromaAreaRatio: ratio * ratio,
		InterpolationOrder: order, Upsample: sample.Metadata.Upsample,
		ChromaAlignment: sample.Metadata.ChromaAlignment, UsedTruthDeviation: false,
		BoundUsesObservedDifferences: false,
		Basis: []string{
			"declared 4:2:0 sampling ratio (0.5 per axis, 0.25 area)",
			"declared interpolation order and chroma alignment",
			"quantization step derived from bit depth and declared quant range",
			"declared filter parameters only; neighboring-plane differences are diagnostics and never feed the bound",
		},
		Formulas: map[string]string{
			"chroma_upsample_interpolation_error": "full_range * r^(order+1), r=0.5",
			"luma_sharpen_contrast_residual":      "quant_step + abs(sharpen_amount)*full_range",
			"chroma_gain_quantization_error":      "quant_step*(1+max(abs(U.gain),abs(V.gain))) + max(abs(offset))*full_range",
			"interleave_rounding_residual":        "quant_step*(1+interpolation_order)",
			"cumulative_bound":                    "sum of the four nonnegative terms; channel bounds omit irrelevant terms",
		},
	}

	for _, block := range sample.Metadata.Blocks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		br := BlockErrorReport{Block: block.Name, CenterX: block.CenterX, CenterY: block.CenterY}
		br.Samples = block.Rect.W * block.Rect.H
		br.ObservedChromaSecondDiff = math.Max(secondDiffInRect(sample.U, block.Rect), secondDiffInRect(sample.V, block.Rect))
		br.ObservedChromaNeighborDiff = math.Max(neighborDiffInRect(sample.U, block.Rect), neighborDiffInRect(sample.V, block.Rect))
		br.ObservedLumaNeighborDiff = neighborDiffInRect(sample.Y, block.Rect)

		br.ChromaUpsampleError = fullRange * math.Pow(ratio, float64(order+1))
		br.LumaSharpenContrastResidual = quantStep + math.Abs(params.Luma.SharpenAmount)*fullRange
		maxGain := math.Max(math.Abs(params.ChromaU.Gain), math.Abs(params.ChromaV.Gain))
		maxOffset := math.Max(math.Abs(params.ChromaU.Offset), math.Abs(params.ChromaV.Offset))
		br.ChromaGainQuantizationError = quantStep*(1+maxGain) + maxOffset*fullRange
		br.InterleaveRoundingResidual = quantStep * float64(1+order)
		br.CumulativeBoundQuantUnits = br.ChromaUpsampleError + br.LumaSharpenContrastResidual +
			br.ChromaGainQuantizationError + br.InterleaveRoundingResidual
		if fullRange > 0 {
			br.CumulativeBoundNormalized = math.Min(1, br.CumulativeBoundQuantUnits/fullRange)
		}
		br.YBoundQuantUnits = br.LumaSharpenContrastResidual + br.InterleaveRoundingResidual
		br.UBoundQuantUnits = br.ChromaUpsampleError + br.ChromaGainQuantizationError + br.InterleaveRoundingResidual
		br.VBoundQuantUnits = br.UBoundQuantUnits
		roundReport(&br)
		report.Blocks = append(report.Blocks, br)
	}
	return report, nil
}

func secondDiffInRect(p *yuv.Plane, rect yuv.Rect) float64 {
	x0, y0 := max(1, rect.X/2), max(1, rect.Y/2)
	x1, y1 := min(p.Width-2, (rect.X+rect.W-1)/2), min(p.Height-2, (rect.Y+rect.H-1)/2)
	var best float64
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			c := int64(p.At(x, y))
			d := math.Abs(float64(int64(p.At(x-1, y)) - 2*c + int64(p.At(x+1, y))))
			dv := math.Abs(float64(int64(p.At(x, y-1)) - 2*c + int64(p.At(x, y+1))))
			best = math.Max(best, math.Max(d, dv))
		}
	}
	return best
}

func neighborDiffInRect(p *yuv.Plane, rect yuv.Rect) float64 {
	scaleX, scaleY := 1, 1
	if p.Name != "Y" {
		scaleX, scaleY = 2, 2
	}
	x0, y0 := max(0, rect.X/scaleX), max(0, rect.Y/scaleY)
	x1, y1 := min(p.Width-1, (rect.X+rect.W-1)/scaleX), min(p.Height-1, (rect.Y+rect.H-1)/scaleY)
	var best float64
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			c := int64(p.At(x, y))
			if x+1 <= x1 {
				best = math.Max(best, math.Abs(float64(c-int64(p.At(x+1, y)))))
			}
			if y+1 <= y1 {
				best = math.Max(best, math.Abs(float64(c-int64(p.At(x, y+1)))))
			}
		}
	}
	return best
}

func roundReport(r *BlockErrorReport) {
	values := []*float64{
		&r.ObservedChromaSecondDiff, &r.ObservedChromaNeighborDiff, &r.ObservedLumaNeighborDiff,
		&r.ChromaUpsampleError, &r.LumaSharpenContrastResidual, &r.ChromaGainQuantizationError,
		&r.InterleaveRoundingResidual, &r.CumulativeBoundQuantUnits, &r.CumulativeBoundNormalized,
		&r.YBoundQuantUnits, &r.UBoundQuantUnits, &r.VBoundQuantUnits,
	}
	for _, v := range values {
		*v = math.Round(*v*1e6) / 1e6
	}
}
