package analyze

import (
	"math"
	"sort"

	"gameintegrity/internal/model"
)

type Neighbor struct {
	Index  int
	Pixels []byte
}

type Input struct {
	Frame       []byte
	Previous    []Neighbor
	Next        []Neighbor
	Width       int
	Height      int
	Channels    int
	BlockSize   int
	Shapes      []model.ShapeBox
	BlockMetric []model.BlockMetric
}

type Metrics struct {
	BlockResidual      []float64
	BlockNoise         []float64
	ResidualEnergy     float64
	ResidualEnergyNext float64
	ResidualScore      float64
	NoiseMean          float64
	NoiseDispersion    float64
	BBoxShiftPixels    float64
}

func Compute(in Input) Metrics {
	blocks := model.AllBlocks(in.Width, in.Height, in.BlockSize)
	m := Metrics{
		BlockResidual: make([]float64, len(blocks)),
		BlockNoise:    make([]float64, len(blocks)),
	}
	if len(blocks) == 0 {
		return m
	}
	for i, b := range blocks {
		m.BlockNoise[i] = blockNoiseVariance(in.Frame, in.Width, in.Channels, b)
	}
	m.NoiseMean = mean(m.BlockNoise)
	m.NoiseDispersion = stddev(m.BlockNoise)
	if len(in.Previous) > 0 {
		all := make([]float64, len(blocks))
		valid := 0
		for _, prev := range in.Previous {
			if len(prev.Pixels) != len(in.Frame) {
				continue
			}
			valid++
			for i, b := range blocks {
				all[i] += blockResidual(in.Frame, prev.Pixels, in.Width, in.Channels, b)
			}
		}
		if valid > 0 {
			for i := range all {
				all[i] /= float64(valid)
				m.BlockResidual[i] += all[i]
			}
			m.ResidualEnergy = mean(all)
		}
	}
	if len(in.Next) > 0 {
		all := make([]float64, len(blocks))
		valid := 0
		for _, next := range in.Next {
			if len(next.Pixels) != len(in.Frame) {
				continue
			}
			valid++
			for i, b := range blocks {
				if valid == 1 {
					all[i] = 0
				}
				all[i] += blockResidual(in.Frame, next.Pixels, in.Width, in.Channels, b)
			}
		}
		if valid > 0 {
			for i := range all {
				all[i] /= float64(valid)
			}
			m.ResidualEnergyNext = mean(all)
		}
	}
	switch {
	case len(in.Previous) > 0 && len(in.Next) > 0:
		m.ResidualScore = math.Min(m.ResidualEnergy, m.ResidualEnergyNext)
	case len(in.Previous) > 0:
		m.ResidualScore = m.ResidualEnergy
	case len(in.Next) > 0:
		m.ResidualScore = m.ResidualEnergyNext
	}
	m.BBoxShiftPixels = bboxShift(in.Frame, in.Width, in.Height, in.Channels, in.Shapes)
	for i := range in.BlockMetric {
		in.BlockMetric[i].Residual = m.BlockResidual[i]
		in.BlockMetric[i].NoiseVariance = m.BlockNoise[i]
	}
	return m
}

func BlockNoiseVariances(pixels []byte, width, height, channels, blockSize int) []float64 {
	blocks := model.AllBlocks(width, height, blockSize)
	out := make([]float64, len(blocks))
	for i, b := range blocks {
		out[i] = blockNoiseVariance(pixels, width, channels, b)
	}
	return out
}

func blockNoiseVariance(pixels []byte, width, channels int, b model.BlockRef) float64 {
	count := 0
	sum, sumSq := 0.0, 0.0
	for y := b.Y; y < b.Y+b.H; y++ {
		for x := b.X; x < b.X+b.W; x++ {
			off := (y*width + x) * channels
			lum := luminance(pixels[off : off+channels])
			sum += lum
			sumSq += lum * lum
			count++
		}
	}
	if count <= 1 {
		return 0
	}
	n := float64(count)
	variance := (sumSq - sum*sum/n) / n
	if variance < 0 {
		variance = 0
	}
	return variance / (255 * 255)
}

func blockResidual(a, b []byte, width, channels int, block model.BlockRef) float64 {
	count := 0
	sum := 0.0
	for y := block.Y; y < block.Y+block.H; y++ {
		row := y * width * channels
		for x := block.X; x < block.X+block.W; x++ {
			off := row + x*channels
			for c := 0; c < channels; c++ {
				d := int(a[off+c]) - int(b[off+c])
				sum += float64(d * d)
				count++
			}
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count) / (255 * 255)
}

func luminance(pixel []byte) float64 {
	if len(pixel) == 0 {
		return 0
	}
	if len(pixel) < 3 {
		return float64(pixel[0])
	}
	return 0.299*float64(pixel[0]) + 0.587*float64(pixel[1]) + 0.114*float64(pixel[2])
}

// bboxShift detects the exact palette-colored shapes and compares their bounding
// rectangles to the parsed truth. A missing shape receives the frame diagonal as
// its shift, which guarantees a high-confidence replacement signal.
func bboxShift(pixels []byte, width, height, channels int, expected []model.ShapeBox) float64 {
	if len(expected) == 0 {
		return 0
	}
	maxShift := 0.0
	for _, shape := range expected {
		minX, minY := width, height
		maxX, maxY := -1, -1
		count := 0
		c := [3]byte{byte(shape.Color[0]), byte(shape.Color[1]), byte(shape.Color[2])}
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				off := (y*width + x) * channels
				if channels < 3 || pixels[off] != c[0] || pixels[off+1] != c[1] || pixels[off+2] != c[2] {
					continue
				}
				count++
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
		minimum := 3
		if shape.W*shape.H >= 32 {
			minimum = 6
		}
		if count < minimum || maxX < minX || maxY < minY {
			shift := math.Hypot(float64(width), float64(height))
			if shift > maxShift {
				maxShift = shift
			}
			continue
		}
		dx := absInt((minX - shape.X))
		dy := absInt((minY - shape.Y))
		dw := absInt((maxX - minX + 1) - shape.W)
		dh := absInt((maxY - minY + 1) - shape.H)
		shift := float64(maxInt(maxInt(dx, dy), maxInt(dw, dh)))
		if shift > maxShift {
			maxShift = shift
		}
	}
	return maxShift
}

func Classify(frame *model.FrameResult, cfg model.Config) {
	if frame == nil {
		return
	}
	frame.Reasons = nil
	strongBBox := frame.BBoxShiftPixels > cfg.BBoxShiftThreshold
	noiseScore := math.Max(frame.NoiseDispersion, frame.NoiseMean)
	strongNoise := noiseScore >= cfg.NoiseThreshold
	strongResidual := frame.ResidualScore >= cfg.ResidualThreshold
	changedRatio := 0.0
	if len(frame.Blocks) > 0 {
		changedRatio = float64(frame.ChangedBlockCount) / float64(len(frame.Blocks))
	}
	hashJump := changedRatio >= cfg.HashJumpRatio
	frame.SummaryJump = hashJump
	if strongBBox {
		frame.Reasons = append(frame.Reasons, "shape bounding box shifted")
	}
	if strongNoise {
		frame.Reasons = append(frame.Reasons, "block noise dispersion exceeded threshold")
	}
	if strongResidual {
		frame.Reasons = append(frame.Reasons, "temporal residual score exceeded threshold")
	}
	if hashJump {
		frame.Reasons = append(frame.Reasons, "block summary jump")
	}
	replaced := strongBBox || (strongNoise && (strongResidual || hashJump)) || (strongResidual && hashJump)
	switch {
	case replaced:
		frame.Verdict = model.VerdictReplaced
	case strongResidual || strongNoise:
		frame.Verdict = model.VerdictSuspect
	default:
		frame.Verdict = model.VerdictIntact
	}
	confidence := 0.1
	if strongBBox {
		confidence += 0.4
	}
	if strongNoise {
		confidence += 0.25
	}
	if strongResidual {
		confidence += 0.2
	}
	if hashJump {
		confidence += 0.1
	}
	if frame.Verdict == model.VerdictIntact {
		confidence = 1 - math.Min(0.9, confidence-0.1)
	}
	frame.Confidence = math.Round(math.Max(0, math.Min(1, confidence))*1000) / 1000
	frame.AbnormalBlocks = AbnormalBlockRefs(frame, cfg)
}

func AbnormalBlockRefs(frame *model.FrameResult, cfg model.Config) []model.BlockRef {
	if frame == nil {
		return nil
	}
	out := make([]model.BlockRef, 0)
	for i := range frame.Blocks {
		b := &frame.Blocks[i]
		if b.Residual >= cfg.ResidualThreshold || b.NoiseVariance >= cfg.NoiseThreshold {
			out = append(out, model.BlockRef{Col: b.Col, Row: b.Row, X: b.X, Y: b.Y, W: b.W, H: b.H})
		}
	}
	if frame.Verdict == model.VerdictReplaced && len(out) == 0 && len(frame.Blocks) > 0 {
		b := frame.Blocks[0]
		out = append(out, model.BlockRef{Col: b.Col, Row: b.Row, X: b.X, Y: b.Y, W: b.W, H: b.H})
	}
	return out
}

// AbnormalBlockCountAt is used by the monotonicity contract tests. As the
// residual threshold increases, the count can only stay equal or decrease.
func AbnormalBlockCountAt(blocks []model.BlockMetric, residualThreshold float64) int {
	count := 0
	for i := range blocks {
		if blocks[i].Residual >= residualThreshold {
			count++
		}
	}
	return count
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func stddev(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	avg := mean(v)
	sum := 0.0
	for _, x := range v {
		d := x - avg
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(v)))
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

func SortedReasons(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
