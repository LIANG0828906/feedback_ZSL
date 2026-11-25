package quality

import (
	"math"

	"qualitybench/internal/core"
	"qualitybench/internal/model"
)

const Version = "qb-quality-v1"

type Metrics struct {
	NoiseVariance float64 `json:"noise_variance"`
	EdgeSharpness float64 `json:"edge_sharpness"`
	ChromaShift   float64 `json:"chroma_shift"`
	LumaMean      float64 `json:"luma_mean"`
	CbMean        float64 `json:"cb_mean"`
	CrMean        float64 `json:"cr_mean"`
}

type Contribution struct {
	Step       int     `json:"step"`
	Category   string  `json:"category"`
	Score      float64 `json:"score"`
	Share      float64 `json:"share"`
	NoiseDelta float64 `json:"noise_delta"`
	EdgeLoss   float64 `json:"edge_loss"`
	ChromaDiff float64 `json:"chroma_diff"`
	LumaDiff   float64 `json:"luma_diff"`
}

type Attribution struct {
	Step       int     `json:"step"`
	Category   string  `json:"category"`
	Score      float64 `json:"score"`
	Share      float64 `json:"share"`
	TotalScore float64 `json:"total_score"`
}

type BlockEvaluation struct {
	Rect          core.Rect      `json:"rect"`
	Metrics       Metrics        `json:"metrics"`
	Steps         []Metrics      `json:"steps"`
	Contributions []Contribution `json:"contributions"`
	Attribution   Attribution    `json:"attribution"`
}

func EvaluateBlock(baseline *core.Frame, steps []*core.Frame, rect core.Rect, share float64, chain model.Chain) BlockEvaluation {
	eval := BlockEvaluation{Rect: rect, Steps: make([]Metrics, 0, len(steps)+1)}
	base := rawMetrics(baseline, rect)
	base.ChromaShift = 0
	eval.Steps = append(eval.Steps, base)
	prev := base
	var total float64
	for i, frame := range steps {
		cur := rawMetrics(frame, rect)
		cur.ChromaShift = chromaDistance(base, cur)
		eval.Steps = append(eval.Steps, cur)
		noiseDelta := math.Max(0, cur.NoiseVariance-prev.NoiseVariance)
		edgeLoss := math.Max(0, prev.EdgeSharpness-cur.EdgeSharpness)
		chromaDiff := chromaDistance(prev, cur)
		lumaDiff := math.Abs(cur.LumaMean - prev.LumaMean)

		noiseNorm := noiseDelta / math.Max(0.00035, base.NoiseVariance+0.00035)
		edgeNorm := edgeLoss / math.Max(0.0030, base.EdgeSharpness)
		chromaNorm := chromaDiff / 96.0
		lumaNorm := lumaDiff / 48.0
		score := 0.62*noiseNorm + 0.34*edgeNorm + 0.26*chromaNorm + 0.22*lumaNorm
		if score < 1e-7 {
			score = 0
		}
		category := "none"
		if i < len(chain.Steps) {
			category = chain.Steps[i].Type.Category()
		}
		eval.Contributions = append(eval.Contributions, Contribution{
			Step:       i + 1,
			Category:   category,
			Score:      score,
			NoiseDelta: noiseDelta,
			EdgeLoss:   edgeLoss,
			ChromaDiff: chromaDiff,
			LumaDiff:   lumaDiff,
		})
		total += score
		prev = cur
	}
	eval.Metrics = prev
	eval.Attribution = Attribution{Category: "none", TotalScore: total}
	if total > 1e-7 {
		for i := range eval.Contributions {
			c := &eval.Contributions[i]
			c.Share = c.Score / total
			if c.Score > 0 && c.Share > share {
				eval.Attribution = Attribution{
					Step:       c.Step,
					Category:   c.Category,
					Score:      c.Score,
					Share:      c.Share,
					TotalScore: total,
				}
				break
			}
		}
	}
	return eval
}

func rawMetrics(frame *core.Frame, rect core.Rect) Metrics {
	rect = rect.Clamp(frame.Width, frame.Height)
	if rect.Empty() {
		return Metrics{}
	}
	var noiseSum, edgeSum, lumaSum, cbSum, crSum float64
	var count float64
	for y := rect.Y; y < rect.Bottom(); y++ {
		for x := rect.X; x < rect.Right(); x++ {
			yy, cb, cr := ycc(frame, x, y)
			lumaSum += yy
			cbSum += cb
			crSum += cr

			avg := 0.0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					sx := clamp(x+dx, 0, frame.Width-1)
					sy := clamp(y+dy, 0, frame.Height-1)
					v, _, _ := ycc(frame, sx, sy)
					avg += v
				}
			}
			avg /= 9
			d := yy - avg
			noiseSum += d * d

			gx := -yccAt(frame, x-1, y-1) - 2*yccAt(frame, x-1, y) - yccAt(frame, x-1, y+1) +
				yccAt(frame, x+1, y-1) + 2*yccAt(frame, x+1, y) + yccAt(frame, x+1, y+1)
			gy := -yccAt(frame, x-1, y-1) - 2*yccAt(frame, x, y-1) - yccAt(frame, x+1, y-1) +
				yccAt(frame, x-1, y+1) + 2*yccAt(frame, x, y+1) + yccAt(frame, x+1, y+1)
			edgeSum += math.Abs(gx) + math.Abs(gy)
			count++
		}
	}
	return Metrics{
		NoiseVariance: noiseSum / count,
		EdgeSharpness: edgeSum / count / 255.0,
		LumaMean:      lumaSum / count,
		CbMean:        cbSum / count,
		CrMean:        crSum / count,
	}
}

func ycc(frame *core.Frame, x, y int) (float64, float64, float64) {
	c := frame.At(x, y)
	r := float64(c[0])
	g := float64(c[1])
	b := float64(c[2])
	yy := 0.299*r + 0.587*g + 0.114*b
	cb := 128 - 0.168736*r - 0.331264*g + 0.5*b
	cr := 128 + 0.5*r - 0.418688*g - 0.081312*b
	return yy, cb, cr
}

func yccAt(frame *core.Frame, x, y int) float64 {
	x = clamp(x, 0, frame.Width-1)
	y = clamp(y, 0, frame.Height-1)
	yy, _, _ := ycc(frame, x, y)
	return yy
}

func chromaDistance(a, b Metrics) float64 {
	return (math.Abs(a.CbMean-b.CbMean) + math.Abs(a.CrMean-b.CrMean)) / 2.0
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func MetricIntervals(m Metrics, tolerance float64) model.MetricInterval {
	return model.MetricInterval{
		NoiseVariance: model.Interval(m.NoiseVariance, tolerance),
		EdgeSharpness: model.Interval(m.EdgeSharpness, tolerance),
		ChromaShift:   model.Interval(m.ChromaShift, tolerance),
	}
}

func MetricsWithin(m Metrics, iv model.MetricInterval) bool {
	return model.IntervalContains(iv.NoiseVariance, m.NoiseVariance) &&
		model.IntervalContains(iv.EdgeSharpness, m.EdgeSharpness) &&
		model.IntervalContains(iv.ChromaShift, m.ChromaShift)
}

func AttributionMatches(actual Attribution, truth model.AttributionTruth) bool {
	if actual.Step != truth.Step || model.NormalizeCategory(actual.Category) != model.NormalizeCategory(truth.Category) {
		return false
	}
	return math.Abs(actual.Share-truth.Share) <= truth.Tolerance+1e-9
}
