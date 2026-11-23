package ocr

import (
	"image"
	"math"
	"strings"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/fontlib"
	"ocrworkbench/internal/imaging"
)

type Recognizer struct {
	Library *fontlib.Library
	Split   domain.SplitParams
}

type colSpan struct {
	start int
	end   int
}

func NewRecognizer(templateVersion string, split domain.SplitParams) (*Recognizer, *domain.AppError) {
	if strings.TrimSpace(templateVersion) == "" {
		e := domain.Errorf(domain.CodeTmpl, "template library version is missing")
		e.Field = "template_version"
		return nil, e
	}
	library, appErr := fontlib.Load(templateVersion, split.TemplateWidth, split.TemplateHeight)
	if appErr != nil {
		return nil, appErr
	}
	return &Recognizer{Library: library, Split: split}, nil
}

func (r *Recognizer) RecognizeLine(gray *image.Gray, line domain.DetectedLine, threshold float64) (domain.LineRecognition, *domain.AppError) {
	if r == nil || r.Library == nil || len(r.Library.Templates) == 0 {
		return domain.LineRecognition{}, domain.Errorf(domain.CodeTmpl, "no usable templates are available")
	}
	if threshold < 0 || threshold > 1 {
		return domain.LineRecognition{}, domain.ParaError("confidence_threshold", "0..1", threshold)
	}
	pad := maxInt(2, int(math.Round(line.AvgStrokeWidth*r.Split.PaddingRatio*2)))
	cropRect := domain.Rect{
		X: maxInt(0, line.BBox.X-pad),
		Y: maxInt(0, line.BBox.Y-pad),
		W: minInt(gray.Bounds().Dx(), line.BBox.Right()+pad) - maxInt(0, line.BBox.X-pad),
		H: minInt(gray.Bounds().Dy(), line.BBox.Bottom()+pad) - maxInt(0, line.BBox.Y-pad),
	}
	if cropRect.W <= 0 || cropRect.H <= 0 {
		return emptyRecognition(r, threshold), nil
	}
	mask, width, height := cropAndBinarize(gray, cropRect)
	mask = cleanMask(mask, width, height, maxInt(2, int(math.Round(line.AvgStrokeWidth*line.AvgStrokeWidth*0.10))))
	if !hasInk(mask) {
		return emptyRecognition(r, threshold), nil
	}
	bestAngle, bestScore := 0.0, -1.0
	for angle := -r.Split.DeskewMaxDeg; angle <= r.Split.DeskewMaxDeg+1e-9; angle += r.Split.DeskewStepDeg {
		rotated, rw, rh := rotateMask(mask, width, height, angle)
		score := rowProjectionScore(rotated, rw, rh)
		if score > bestScore+1e-9 {
			bestScore = score
			bestAngle = angle
		}
	}
	rotated, rw, rh := rotateMask(mask, width, height, bestAngle)
	minX, minY, maxX, maxY := inkBounds(rotated, rw, rh)
	if maxX < minX || maxY < minY {
		return emptyRecognition(r, threshold), nil
	}
	rotated = cropMask(rotated, rw, rh, minX, minY, maxX-minX+1, maxY-minY+1)
	rw, rh = maxX-minX+1, maxY-minY+1
	projection := make([]int, rw)
	for x := 0; x < rw; x++ {
		for y := 0; y < rh; y++ {
			if rotated[y*rw+x] {
				projection[x]++
			}
		}
	}
	spans := make([]colSpan, 0, 32)
	inRun := false
	start := 0
	for x, v := range projection {
		if v > 0 && !inRun {
			inRun = true
			start = x
		}
		if v == 0 && inRun {
			inRun = false
			spans = append(spans, colSpan{start: start, end: x})
		}
	}
	if inRun {
		spans = append(spans, colSpan{start: start, end: rw})
	}
	spans = mergeSmallFragments(spans, rw, rh, rotated, line.AvgStrokeWidth*r.Split.MinGapRatio)
	if len(spans) == 0 {
		return emptyRecognition(r, threshold), nil
	}
	characters := make([]domain.CharResult, 0, len(spans))
	for i, sp := range spans {
		gw, gh, glyphMask, ox, oy := cropSpan(rotated, rw, rh, sp.start, sp.end)
		if gw <= 0 || gh <= 0 || !hasInk(glyphMask) {
			continue
		}
		normalized := fontlib.NormalizeMask(boolToFloat(glyphMask), gw, gh, r.Split.TemplateWidth, r.Split.TemplateHeight, 0.10)
		best, score, runner, runnerScore := r.Library.Match(normalized, r.Split.TemplateWidth, r.Split.TemplateHeight)
		if best.Pixels == 0 {
			continue
		}
		globalX := cropRect.X + minX + sp.start - int(math.Round((float64(rw)-float64(width))/2)) + ox
		globalY := cropRect.Y + minY + oy - int(math.Round((float64(rh)-float64(height))/2))
		characters = append(characters, domain.CharResult{
			Index:         i,
			Rune:          string(best.Rune),
			BBox:          domain.Rect{X: globalX, Y: globalY, W: gw, H: gh},
			Confidence:    domain.Round4(domain.Clamp01(score)),
			MatchScore:    domain.Round4(domain.Clamp01(score)),
			RunnerUp:      string(runner.Rune),
			RunnerUpScore: domain.Round4(domain.Clamp01(runnerScore)),
		})
	}
	if len(characters) == 0 {
		return emptyRecognition(r, threshold), nil
	}
	var builder strings.Builder
	confidence := 0.0
	for _, char := range characters {
		builder.WriteString(char.Rune)
		confidence += char.Confidence
	}
	confidence /= float64(len(characters))
	return domain.LineRecognition{
		Text:                builder.String(),
		Empty:               false,
		Confidence:          domain.Round4(domain.Clamp01(confidence)),
		Characters:          characters,
		DeskewDegrees:       domain.Round4(bestAngle),
		TemplateVersion:     r.Library.Version,
		SplitParams:         r.Split,
		SplitVersion:        domain.HashJSON(r.Split),
		Accepted:            domain.Clamp01(confidence) >= threshold,
		ConfidenceThreshold: threshold,
	}, nil
}

func emptyRecognition(r *Recognizer, threshold float64) domain.LineRecognition {
	return domain.LineRecognition{
		Text:                "",
		Empty:               true,
		Confidence:          0,
		Characters:          []domain.CharResult{},
		TemplateVersion:     r.Library.Version,
		SplitParams:         r.Split,
		SplitVersion:        domain.HashJSON(r.Split),
		Accepted:            false,
		ConfidenceThreshold: threshold,
	}
}

func cropAndBinarize(gray *image.Gray, rect domain.Rect) ([]bool, int, int) {
	sub := image.NewGray(image.Rect(0, 0, rect.W, rect.H))
	for y := 0; y < rect.H; y++ {
		copy(sub.Pix[y*rect.W:(y+1)*rect.W], gray.Pix[(rect.Y+y)*gray.Bounds().Dx()+rect.X:(rect.Y+y)*gray.Bounds().Dx()+rect.X+rect.W])
	}
	view := imaging.Binarize(sub)
	return view.Mask, rect.W, rect.H
}

func cleanMask(mask []bool, width, height, minArea int) []bool {
	if width <= 0 || height <= 0 {
		return mask
	}
	visited := make([]bool, len(mask))
	keep := make([]bool, len(mask))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			index := y*width + x
			if visited[index] || !mask[index] {
				continue
			}
			queue := []image.Point{{X: x, Y: y}}
			visited[index] = true
			component := make([]int, 0, 32)
			for head := 0; head < len(queue); head++ {
				p := queue[head]
				component = append(component, p.Y*width+p.X)
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						if dx == 0 && dy == 0 {
							continue
						}
						nx, ny := p.X+dx, p.Y+dy
						if nx < 0 || ny < 0 || nx >= width || ny >= height {
							continue
						}
						ni := ny*width + nx
						if !visited[ni] && mask[ni] {
							visited[ni] = true
							queue = append(queue, image.Pt(nx, ny))
						}
					}
				}
			}
			if len(component) >= minArea {
				for _, i := range component {
					keep[i] = true
				}
			}
		}
	}
	return keep
}

func hasInk(mask []bool) bool {
	for _, v := range mask {
		if v {
			return true
		}
	}
	return false
}

func rotateMask(src []bool, width, height int, degrees float64) ([]bool, int, int) {
	if math.Abs(degrees) < 1e-9 {
		return append([]bool(nil), src...), width, height
	}
	angle := degrees * math.Pi / 180
	cosA, sinA := math.Cos(angle), math.Sin(angle)
	dw := int(math.Ceil(math.Abs(float64(width)*cosA) + math.Abs(float64(height)*sinA)))
	dh := int(math.Ceil(math.Abs(float64(width)*sinA) + math.Abs(float64(height)*cosA)))
	dst := make([]bool, dw*dh)
	scx, scy := float64(width-1)/2, float64(height-1)/2
	dcx, dcy := float64(dw-1)/2, float64(dh-1)/2
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			dx, dy := float64(x)-dcx, float64(y)-dcy
			sx := dx*cosA + dy*sinA + scx
			sy := -dx*sinA + dy*cosA + scy
			ix, iy := int(math.Round(sx)), int(math.Round(sy))
			if ix < 0 || iy < 0 || ix >= width || iy >= height {
				continue
			}
			dst[y*dw+x] = src[iy*width+ix]
		}
	}
	return dst, dw, dh
}

func rowProjectionScore(mask []bool, width, height int) float64 {
	score := 0.0
	for y := 0; y < height; y++ {
		count := 0
		for x := 0; x < width; x++ {
			if mask[y*width+x] {
				count++
			}
		}
		score += float64(count * count)
	}
	return score
}

func inkBounds(mask []bool, width, height int) (int, int, int, int) {
	minX, minY := width, height
	maxX, maxY := -1, -1
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if !mask[y*width+x] {
				continue
			}
			if x < minX {
				minX = x
			}
			if y < minY {
				minY = y
			}
			if x > maxX {
				maxX = x
			}
			if y > maxY {
				maxY = y
			}
		}
	}
	return minX, minY, maxX, maxY
}

func cropMask(src []bool, width, height, x, y, w, h int) []bool {
	dst := make([]bool, w*h)
	for yy := 0; yy < h; yy++ {
		for xx := 0; xx < w; xx++ {
			sx, sy := x+xx, y+yy
			if sx >= 0 && sy >= 0 && sx < width && sy < height {
				dst[yy*w+xx] = src[sy*width+sx]
			}
		}
	}
	return dst
}

func mergeSmallFragments(spans []colSpan, width, height int, mask []bool, minGap float64) []colSpan {
	if len(spans) <= 1 || minGap <= 0 {
		return spans
	}
	merged := make([]colSpan, 0, len(spans))
	for _, sp := range spans {
		if len(merged) == 0 {
			merged = append(merged, sp)
			continue
		}
		gap := sp.start - merged[len(merged)-1].end
		if float64(gap) <= minGap {
			merged[len(merged)-1].end = sp.end
		} else {
			merged = append(merged, sp)
		}
	}
	return merged
}

func cropSpan(mask []bool, width, height, start, end int) (int, int, []bool, int, int) {
	minY, maxY := height, -1
	for y := 0; y < height; y++ {
		for x := start; x < end; x++ {
			if mask[y*width+x] {
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	if maxY < minY {
		return 0, 0, nil, 0, 0
	}
	w, h := end-start, maxY-minY+1
	out := make([]bool, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			out[y*w+x] = mask[(minY+y)*width+start+x]
		}
	}
	return w, h, out, start, minY
}

func boolToFloat(mask []bool) []float64 {
	out := make([]float64, len(mask))
	for i, v := range mask {
		if v {
			out[i] = 1
		}
	}
	return out
}
