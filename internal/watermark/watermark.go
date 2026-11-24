package watermark

import (
	"image"
	"image/color"
	"math"

	"integritykernel/internal/core"
)

func CellRect(region core.Rect, col, row, cols, rows int) image.Rectangle {
	x0 := region.X + int(math.Floor(float64(col)*float64(region.Width)/float64(cols)))
	x1 := region.X + int(math.Floor(float64(col+1)*float64(region.Width)/float64(cols)))
	y0 := region.Y + int(math.Floor(float64(row)*float64(region.Height)/float64(rows)))
	y1 := region.Y + int(math.Floor(float64(row+1)*float64(region.Height)/float64(rows)))
	return image.Rect(x0, y0, x1, y1)
}

func GridPoint(region core.Rect, col, row, cols, rows int) (int, int) {
	r := CellRect(region, col, row, cols, rows)
	return (r.Min.X + r.Max.X) / 2, (r.Min.Y + r.Max.Y) / 2
}

func Draw(img *image.NRGBA, frame int, region core.Rect, params core.Params) {
	cols := params.SampleCols
	rows := params.SampleRows
	for col := 0; col < cols; col++ {
		bit := (frame >> (cols - 1 - col)) & 1
		base := moduleColor(bit)
		for row := 0; row < rows; row++ {
			cell := CellRect(region, col, row, cols, rows)
			for y := cell.Min.Y; y < cell.Max.Y; y++ {
				for x := cell.Min.X; x < cell.Max.X; x++ {
					if x < 0 || y < 0 || x >= img.Rect.Dx() || y >= img.Rect.Dy() {
						continue
					}
					insetX := x == cell.Min.X || x == cell.Max.X-1
					insetY := y == cell.Min.Y || y == cell.Max.Y-1
					drawn := base
					if insetX || insetY {
						if bit == 1 {
							drawn = shade(base, -28)
						} else {
							drawn = shade(base, 34)
						}
					}
					img.SetNRGBA(x, y, color.NRGBA{R: drawn[0], G: drawn[1], B: drawn[2], A: drawn[3]})
				}
			}
		}
	}
}

func moduleColor(bit int) [4]uint8 {
	if bit == 1 {
		return [4]uint8{248, 248, 232, 255}
	}
	return [4]uint8{8, 10, 16, 255}
}

func shade(c [4]uint8, delta int) [4]uint8 {
	clamp := func(v int) uint8 {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v)
	}
	return [4]uint8{clamp(int(c[0]) + delta), clamp(int(c[1]) + delta), clamp(int(c[2]) + delta), 255}
}

func Decode(img *image.NRGBA, expectedFrame int, declared core.Rect, params core.Params) (*core.WatermarkEvidence, error) {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if !declared.InFrame(width, height) {
		frame := expectedFrame
		return nil, &core.Error{Code: core.CodeMark, Message: "watermark region is outside frame", Frame: &frame, Field: "watermark_region"}
	}

	type alignCandidate struct {
		dx, dy int
		score  float64
	}
	candidates := []alignCandidate{}
	maxScore := -1.0
	for dy := -params.SearchRadius; dy <= params.SearchRadius; dy++ {
		for dx := -params.SearchRadius; dx <= params.SearchRadius; dx++ {
			candidate := core.Rect{X: declared.X + dx, Y: declared.Y + dy, Width: declared.Width, Height: declared.Height}
			if !candidate.InFrame(width, height) {
				continue
			}
			score := alignmentScore(img, candidate, params)
			candidates = append(candidates, alignCandidate{dx: dx, dy: dy, score: score})
			if score > maxScore {
				maxScore = score
			}
		}
	}
	bestDX, bestDY := 0, 0
	bestDistance := 1 << 30
	for _, candidate := range candidates {
		if candidate.score < maxScore-0.018 {
			continue
		}
		distance := absInt(candidate.dx) + absInt(candidate.dy)
		if distance < bestDistance || (distance == bestDistance && (candidate.dy < bestDY || (candidate.dy == bestDY && candidate.dx < bestDX))) {
			bestDistance = distance
			bestDX, bestDY = candidate.dx, candidate.dy
		}
	}

	bestRegion := core.Rect{X: declared.X + bestDX, Y: declared.Y + bestDY, Width: declared.Width, Height: declared.Height}
	positions, columnLuma := samplePositions(img, bestRegion, params)
	decoded := 0
	confidenceSum := 0.0
	for col := 0; col < params.SampleCols; col++ {
		bit := 0
		if columnLuma[col] >= params.WatermarkThreshold {
			bit = 1
		}
		decoded = (decoded << 1) | bit
	}
	for _, pos := range positions {
		confidenceSum += pos.Confidence
	}
	overall := confidenceSum / float64(len(positions))
	contrast := moduleContrast(img, bestRegion, params)
	lowConfidence := 0
	for _, pos := range positions {
		if pos.Confidence < 0.35 {
			lowConfidence++
		}
	}
	missing := overall < params.ConfidenceThreshold ||
		contrast < 0.035 ||
		float64(lowConfidence)/float64(len(positions)) > 1.0/3.0
	misaligned := maxInt(absInt(bestDX), absInt(bestDY)) > params.AlignmentTolerance

	return &core.WatermarkEvidence{
		Frame:             expectedFrame,
		Region:            declared,
		BestRegion:        bestRegion,
		OffsetX:           bestDX,
		OffsetY:           bestDY,
		DecodedFrame:      decoded,
		ExpectedFrame:     expectedFrame,
		MatchesExpected:   decoded == expectedFrame && !missing,
		OverallConfidence: overall,
		Missing:           missing,
		Misaligned:        misaligned,
		Positions:         positions,
	}, nil
}

func moduleContrast(img *image.NRGBA, region core.Rect, params core.Params) float64 {
	sum := 0.0
	count := 0
	for col := 0; col < params.SampleCols; col++ {
		for row := 0; row < params.SampleRows; row++ {
			cell := CellRect(region, col, row, params.SampleCols, params.SampleRows)
			cx, cy := GridPoint(region, col, row, params.SampleCols, params.SampleRows)
			center := averageLuma(img, cx, cy, 1)
			edges := []float64{
				LumaAt(img, cell.Min.X, cy),
				LumaAt(img, cell.Max.X-1, cy),
				LumaAt(img, cx, cell.Min.Y),
				LumaAt(img, cx, cell.Max.Y-1),
			}
			edgeMean := 0.0
			for _, value := range edges {
				edgeMean += value
			}
			edgeMean /= float64(len(edges))
			sum += math.Abs(center - edgeMean)
			count++
		}
	}
	return sum / float64(count)
}

func alignmentScore(img *image.NRGBA, region core.Rect, params core.Params) float64 {
	positions, _ := samplePositions(img, region, params)
	sum := 0.0
	for _, pos := range positions {
		sum += pos.Confidence
	}
	return sum / float64(len(positions))
}

func samplePositions(img *image.NRGBA, region core.Rect, params core.Params) ([]core.PositionConfidence, []float64) {
	positions := make([]core.PositionConfidence, 0, params.SampleCols*params.SampleRows)
	columnLuma := make([]float64, params.SampleCols)
	for col := 0; col < params.SampleCols; col++ {
		rowSum := 0.0
		for row := 0; row < params.SampleRows; row++ {
			x, y := GridPoint(region, col, row, params.SampleCols, params.SampleRows)
			luma := averageLuma(img, x, y, 1)
			bit := 0
			if luma >= params.WatermarkThreshold {
				bit = 1
			}
			confidence := confidence(luma, params.WatermarkThreshold)
			positions = append(positions, core.PositionConfidence{
				X: x, Y: y, Column: col, Row: row,
				Luma: luma, Bit: bit, Confidence: confidence,
			})
			rowSum += luma
		}
		columnLuma[col] = rowSum / float64(params.SampleRows)
	}
	return positions, columnLuma
}

func confidence(luma, threshold float64) float64 {
	var c float64
	if luma >= threshold {
		if threshold >= 0.999 {
			c = 1
		} else {
			c = (luma - threshold) / (1 - threshold)
		}
	} else {
		if threshold <= 0.001 {
			c = 1
		} else {
			c = (threshold - luma) / threshold
		}
	}
	return clamp01(c)
}

func LumaAt(img *image.NRGBA, x, y int) float64 {
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x >= img.Rect.Dx() {
		x = img.Rect.Dx() - 1
	}
	if y >= img.Rect.Dy() {
		y = img.Rect.Dy() - 1
	}
	i := img.PixOffset(x, y)
	r := float64(img.Pix[i+0]) / 255.0
	g := float64(img.Pix[i+1]) / 255.0
	b := float64(img.Pix[i+2]) / 255.0
	return 0.2126*r + 0.7152*g + 0.0722*b
}

func averageLuma(img *image.NRGBA, centerX, centerY, radius int) float64 {
	sum := 0.0
	count := 0
	for y := centerY - radius; y <= centerY+radius; y++ {
		for x := centerX - radius; x <= centerX+radius; x++ {
			sum += LumaAt(img, x, y)
			count++
		}
	}
	return sum / float64(count)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
