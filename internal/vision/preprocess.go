package vision

import (
	"fmt"
	"image"
	"math"
	"sort"

	"ocrworkbench/internal/core"
)

type GrayImage struct {
	Width  int
	Height int
	Pixels []uint8
}

func ToGray(src image.Image) *GrayImage {
	bounds := src.Bounds()
	gray := &GrayImage{
		Width:  bounds.Dx(),
		Height: bounds.Dy(),
		Pixels: make([]uint8, bounds.Dx()*bounds.Dy()),
	}
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			r, g, b, _ := src.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			// Integer Rec.601 luma in 8-bit space.
			value := (19595*int(r) + 38470*int(g) + 7471*int(b) + 1<<15) >> 24
			gray.Pixels[y*gray.Width+x] = uint8(value)
		}
	}
	return gray
}

func gradientMap(gray *GrayImage) []uint8 {
	out := make([]uint8, gray.Width*gray.Height)
	for y := 0; y < gray.Height; y++ {
		y0, y1 := maxInt(0, y-1), minInt(gray.Height-1, y+1)
		for x := 0; x < gray.Width; x++ {
			x0, x1 := maxInt(0, x-1), minInt(gray.Width-1, x+1)
			gx := int(gray.Pixels[y*gray.Width+x1]) - int(gray.Pixels[y*gray.Width+x0])
			gy := int(gray.Pixels[y1*gray.Width+x]) - int(gray.Pixels[y0*gray.Width+x])
			if gx < 0 {
				gx = -gx
			}
			if gy < 0 {
				gy = -gy
			}
			value := gx + gy
			if value > 255 {
				value = 255
			}
			out[y*gray.Width+x] = uint8(value)
		}
	}
	return out
}

type Binarized struct {
	Gray      *GrayImage
	Mask      []bool
	Gradient  []uint8
	Threshold int
	Min       int
	Max       int
	Polarity  int // +1 means foreground is brighter than background.
}

func Binarize(gray *GrayImage, rect core.Rect, adaptiveRadius int) (*Binarized, error) {
	rect = core.Rect{
		X:      maxInt(0, rect.X),
		Y:      maxInt(0, rect.Y),
		Width:  minInt(gray.Width, rect.Right()) - maxInt(0, rect.X),
		Height: minInt(gray.Height, rect.Bottom()) - maxInt(0, rect.Y),
	}
	if rect.Empty() {
		return nil, fmt.Errorf("empty binarization region")
	}
	values := make([]uint8, 0, rect.Pixels())
	histogram := [256]int{}
	for y := rect.Y; y < rect.Bottom(); y++ {
		for x := rect.X; x < rect.Right(); x++ {
			v := gray.Pixels[y*gray.Width+x]
			histogram[v]++
			values = append(values, v)
		}
	}
	threshold := otsu(histogram, len(values))
	lowCount, highCount := 0, 0
	lowSum, highSum := 0, 0
	for value, count := range histogram {
		if value <= threshold {
			lowCount += count
			lowSum += value * count
		} else {
			highCount += count
			highSum += value * count
		}
	}
	classContrast := 0.0
	if lowCount > 0 && highCount > 0 {
		classContrast = math.Abs(float64(lowSum)/float64(lowCount) - float64(highSum)/float64(highCount))
	}
	// The border is background by construction. Its class determines polarity.
	borderSum, borderCount := 0, 0
	for x := rect.X; x < rect.Right(); x++ {
		borderSum += int(gray.Pixels[rect.Y*gray.Width+x])
		borderSum += int(gray.Pixels[(rect.Bottom()-1)*gray.Width+x])
		borderCount += 2
	}
	for y := rect.Y + 1; y < rect.Bottom()-1; y++ {
		borderSum += int(gray.Pixels[y*gray.Width+rect.X])
		borderSum += int(gray.Pixels[y*gray.Width+rect.Right()-1])
		borderCount += 2
	}
	backgroundMean := 0
	if borderCount > 0 {
		backgroundMean = borderSum / borderCount
	}
	polarity := 1
	if backgroundMean > threshold {
		polarity = -1
	}
	integral, integralSq := integralImages(gray)
	localRadius := adaptiveRadius
	if localRadius < 8 {
		localRadius = 20
	}
	mask := make([]bool, gray.Width*gray.Height)
	gradient := gradientMap(gray)
	for y := rect.Y; y < rect.Bottom(); y++ {
		for x := rect.X; x < rect.Right(); x++ {
			v := gray.Pixels[y*gray.Width+x]
			isForeground := (polarity > 0 && int(v) > threshold) || (polarity < 0 && int(v) < threshold)
			if classContrast < 10 {
				isForeground = false
			}
			localMean, localStd := windowStats(integral, integralSq, gray.Width, gray.Height, x, y, localRadius)
			difference := float64(int(v)) - localMean
			localThreshold := math.Max(7, 0.15*localStd)
			if polarity > 0 && difference >= localThreshold {
				isForeground = true
			}
			if polarity < 0 && -difference >= localThreshold {
				isForeground = true
			}
			mask[y*gray.Width+x] = isForeground
		}
	}
	minValue, maxValue := 255, 0
	for _, v := range values {
		if int(v) < minValue {
			minValue = int(v)
		}
		if int(v) > maxValue {
			maxValue = int(v)
		}
	}
	return &Binarized{
		Gray:      gray,
		Mask:      mask,
		Gradient:  gradient,
		Threshold: threshold,
		Min:       minValue,
		Max:       maxValue,
		Polarity:  polarity,
	}, nil
}

func otsu(histogram [256]int, total int) int {
	if total == 0 {
		return 127
	}
	sum := 0
	for i, count := range histogram {
		sum += i * count
	}
	sumB, weightB := 0, 0
	best, bestVariance := 127, -1.0
	for threshold := 0; threshold < 256; threshold++ {
		weightB += histogram[threshold]
		if weightB == 0 {
			continue
		}
		weightF := total - weightB
		if weightF == 0 {
			break
		}
		sumB += threshold * histogram[threshold]
		meanB := float64(sumB) / float64(weightB)
		meanF := float64(sum-sumB) / float64(weightF)
		variance := float64(weightB) * float64(weightF) * (meanB - meanF) * (meanB - meanF)
		if variance > bestVariance {
			bestVariance = variance
			best = threshold
		}
	}
	return best
}

func integralImages(gray *GrayImage) ([]uint64, []uint64) {
	stride := gray.Width + 1
	integral := make([]uint64, stride*(gray.Height+1))
	integralSq := make([]uint64, stride*(gray.Height+1))
	for y := 0; y < gray.Height; y++ {
		var rowSum, rowSq uint64
		for x := 0; x < gray.Width; x++ {
			v := uint64(gray.Pixels[y*gray.Width+x])
			rowSum += v
			rowSq += v * v
			index := (y+1)*stride + (x + 1)
			integral[index] = integral[index-stride] + rowSum
			integralSq[index] = integralSq[index-stride] + rowSq
		}
	}
	return integral, integralSq
}

func windowStats(integral, integralSq []uint64, width, height, x, y, radius int) (float64, float64) {
	x0, y0 := maxInt(0, x-radius), maxInt(0, y-radius)
	x1, y1 := minInt(width-1, x+radius), minInt(height-1, y+radius)
	stride := width + 1
	count := uint64((x1 - x0 + 1) * (y1 - y0 + 1))
	if count == 0 {
		return 0, 0
	}
	sum := rectSum(integral, stride, x0, y0, x1, y1)
	sumSq := rectSum(integralSq, stride, x0, y0, x1, y1)
	mean := float64(sum) / float64(count)
	variance := float64(sumSq)/float64(count) - mean*mean
	if variance < 0 {
		variance = 0
	}
	return mean, math.Sqrt(variance)
}

func rectSum(integral []uint64, stride, x0, y0, x1, y1 int) uint64 {
	return integral[(y1+1)*stride+(x1+1)] -
		integral[y0*stride+(x1+1)] -
		integral[(y1+1)*stride+x0] +
		integral[y0*stride+x0]
}

func DetectComponents(bin *Binarized, rect core.Rect, p core.DetectorParams, tileIndex int) []core.Component {
	rect = core.Rect{
		X:      maxInt(0, rect.X),
		Y:      maxInt(0, rect.Y),
		Width:  minInt(bin.Gray.Width, rect.Right()) - maxInt(0, rect.X),
		Height: minInt(bin.Gray.Height, rect.Bottom()) - maxInt(0, rect.Y),
	}
	if rect.Empty() {
		return nil
	}
	visited := make([]bool, bin.Gray.Width*bin.Gray.Height)
	components := make([]core.Component, 0)
	queueX := make([]int, 0, 256)
	queueY := make([]int, 0, 256)
	for y := rect.Y; y < rect.Bottom(); y++ {
		for x := rect.X; x < rect.Right(); x++ {
			index := y*bin.Gray.Width + x
			if visited[index] || !bin.Mask[index] {
				continue
			}
			visited[index] = true
			queueX = queueX[:0]
			queueY = queueY[:0]
			queueX = append(queueX, x)
			queueY = append(queueY, y)
			minX, minY := x, y
			maxX, maxY := x, y
			var pixels []core.Point
			graySum := 0
			gradientSum, maxGradient := 0, 0
			for head := 0; head < len(queueX); head++ {
				cx, cy := queueX[head], queueY[head]
				pixels = append(pixels, core.Point{X: float64(cx), Y: float64(cy)})
				graySum += int(bin.Gray.Pixels[cy*bin.Gray.Width+cx])
				gradientValue := int(bin.Gradient[cy*bin.Gray.Width+cx])
				gradientSum += gradientValue
				if gradientValue > maxGradient {
					maxGradient = gradientValue
				}
				minX, minY = minInt(minX, cx), minInt(minY, cy)
				maxX, maxY = maxInt(maxX, cx), maxInt(maxY, cy)
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						if dx == 0 && dy == 0 {
							continue
						}
						nx, ny := cx+dx, cy+dy
						if nx < rect.X || ny < rect.Y || nx >= rect.Right() || ny >= rect.Bottom() {
							continue
						}
						nIndex := ny*bin.Gray.Width + nx
						if visited[nIndex] || !bin.Mask[nIndex] {
							continue
						}
						visited[nIndex] = true
						queueX = append(queueX, nx)
						queueY = append(queueY, ny)
					}
				}
			}
			box := core.Rect{X: minX, Y: minY, Width: maxX - minX + 1, Height: maxY - minY + 1}
			if len(pixels) < p.MinComponentArea || len(pixels) < p.MinComponentPixels {
				continue
			}
			if box.Width < 2 || box.Height < 2 {
				continue
			}
			meanGradient := float64(gradientSum) / float64(len(pixels))
			if meanGradient < 1 && maxGradient < 3 {
				continue
			}
			minRun := estimateStrokeWidth(bin.Mask, bin.Gray.Width, box)
			components = append(components, core.Component{
				Rect:        box,
				PixelCount:  len(pixels),
				StrokeWidth: float64(minRun),
				Center: core.Point{
					X: float64(minX) + float64(box.Width-1)/2,
					Y: float64(minY) + float64(box.Height-1)/2,
				},
				SourceTile:   tileIndex,
				LocalPixels:  pixels,
				MeanGray:     float64(graySum) / float64(len(pixels)),
				MeanGradient: meanGradient,
				MaxGradient:  maxGradient,
				MinimumRun:   minRun,
			})
		}
	}
	sort.Slice(components, func(i, j int) bool {
		a, b := components[i], components[j]
		if a.Rect.Y != b.Rect.Y {
			return a.Rect.Y < b.Rect.Y
		}
		if a.Rect.X != b.Rect.X {
			return a.Rect.X < b.Rect.X
		}
		return a.PixelCount < b.PixelCount
	})
	for i := range components {
		components[i].ID = i
	}
	return components
}

func estimateStrokeWidth(mask []bool, width int, box core.Rect) int {
	runs := make([]int, 0, box.Width+box.Height)
	for y := box.Y; y < box.Bottom(); y++ {
		minRun := 0
		run := 0
		for x := box.X; x < box.Right(); x++ {
			if mask[y*width+x] {
				run++
			} else if run > 0 {
				if minRun == 0 || run < minRun {
					minRun = run
				}
				run = 0
			}
		}
		if run > 0 && (minRun == 0 || run < minRun) {
			minRun = run
		}
		if minRun > 0 {
			runs = append(runs, minRun)
		}
	}
	for x := box.X; x < box.Right(); x++ {
		minRun := 0
		run := 0
		for y := box.Y; y < box.Bottom(); y++ {
			if mask[y*width+x] {
				run++
			} else if run > 0 {
				if minRun == 0 || run < minRun {
					minRun = run
				}
				run = 0
			}
		}
		if run > 0 && (minRun == 0 || run < minRun) {
			minRun = run
		}
		if minRun > 0 {
			runs = append(runs, minRun)
		}
	}
	if len(runs) == 0 {
		return 1
	}
	sort.Ints(runs)
	return maxInt(1, runs[len(runs)/2])
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
