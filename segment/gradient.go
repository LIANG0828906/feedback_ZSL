package segment

import (
	"iconflow/errs"
	"iconflow/model"
)

type QuantResidual struct {
	Index     int   `json:"index"`
	X         int   `json:"x"`
	Y         int   `json:"y"`
	OldValue  int   `json:"old_value"`
	Quotient  int64 `json:"quotient"`
	Remainder int64 `json:"remainder"`
}

type GradientResult struct {
	Coord           model.BlockCoord `json:"coord"`
	Gray            []int            `json:"gray"`
	Quantized       []int            `json:"quantized"`
	Magnitude       []int            `json:"magnitude"`
	Operator        string           `json:"operator"`
	OperatorVersion string           `json:"operator_version"`
	BufferPixels    int              `json:"buffer_pixels"`
	QuantResiduals  []QuantResidual  `json:"quant_residuals,omitempty"`
}

func lumaWeight(channel string) int {
	switch channel {
	case "R":
		return 299
	case "G":
		return 587
	case "B":
		return 114
	default:
		return 0
	}
}

func integerSqrt(v int64) int {
	if v <= 0 {
		return 0
	}
	x := v
	y := (x + 1) / 2
	for y < x {
		x = y
		y = (x + v/x) / 2
	}
	return int(x)
}

func ComputeGradient(img *model.Image, coord model.BlockCoord, order []string, quant model.QuantSpec, operatorVersion string, maxBufferPixels int) (*GradientResult, *errs.Error) {
	if img == nil || img.Raster == nil {
		return nil, errs.New(errs.EInternal, coord.Key(), "image raster is nil")
	}
	pixelCount := coord.Width * coord.Height
	if maxBufferPixels > 0 && pixelCount > maxBufferPixels {
		return nil, errs.New(errs.EBudget, coord.Key(), "block needs %d pixel slots, per-thread buffer limit is %d", pixelCount, maxBufferPixels)
	}
	if len(order) == 0 {
		order = img.Raster.Channels
	}
	if len(order) == 0 {
		order = []string{"R", "G", "B", "A"}
	}

	gray := make([]int, pixelCount)
	quantized := make([]int, pixelCount)
	residuals := []QuantResidual{}
	for by := 0; by < coord.Height; by++ {
		for bx := 0; bx < coord.Width; bx++ {
			globalX := coord.OriginX + bx
			globalY := coord.OriginY + by
			globalIndex := globalY*img.Raster.Width + globalX
			var weighted int64
			for storageCh, channelName := range order {
				if storageCh >= len(img.Raster.Planes) {
					break
				}
				weight := lumaWeight(channelName)
				weighted += int64(img.Raster.Planes[storageCh][globalIndex]) * int64(weight)
			}
			value := int(weighted / 1000)
			idx := by*coord.Width + bx
			gray[idx] = value
			if quant.Step > 1 {
				q := int64(value) / int64(quant.Step)
				rem := int64(value) % int64(quant.Step)
				value = int(q * int64(quant.Step))
				if rem != 0 {
					residuals = append(residuals, QuantResidual{
						Index: idx, X: bx, Y: by, OldValue: gray[idx],
						Quotient: q, Remainder: rem,
					})
				}
			}
			quantized[idx] = value
		}
	}

	operator := "sobel"
	centerWeight := 2
	if operatorVersion == "prewitt-v1" {
		operator = "prewitt"
		centerWeight = 1
	} else if operatorVersion != "" && operatorVersion != "sobel-v1" {
		return nil, errs.Schema("$.algorithm.operator_version", "unsupported gradient operator %q", operatorVersion)
	}

	magnitude := make([]int, pixelCount)
	for y := 0; y < coord.Height; y++ {
		for x := 0; x < coord.Width; x++ {
			var gx, gy int
			for ky := -1; ky <= 1; ky++ {
				for kx := -1; kx <= 1; kx++ {
					sampleX := clamp(x+kx, 0, coord.Width-1)
					sampleY := clamp(y+ky, 0, coord.Height-1)
					v := quantized[sampleY*coord.Width+sampleX]
					xKernel := kx
					yKernel := ky
					if kx == 0 {
						xKernel = 0
					} else if ky == 0 {
						xKernel = kx * centerWeight
					}
					if ky == 0 {
						yKernel = 0
					} else if kx == 0 {
						yKernel = ky * centerWeight
					}
					gx += v * xKernel
					gy += v * yKernel
				}
			}
			magnitude[y*coord.Width+x] = integerSqrt(int64(gx*gx + gy*gy))
		}
	}

	return &GradientResult{
		Coord: coord, Gray: gray, Quantized: quantized, Magnitude: magnitude,
		Operator: operator, OperatorVersion: operatorVersion,
		BufferPixels: pixelCount * 2, QuantResiduals: residuals,
	}, nil
}

func clamp(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}
