package audiofp

import "math"

type Template struct {
	Category    string      `json:"category"`
	Tone        ToneParams  `json:"tone"`
	Fingerprint Fingerprint `json:"fingerprint"`
}

func BuildTemplates(categories []Category, sampleRate int, params Params) ([]Template, error) {
	if err := params.Validate(); err != nil {
		return nil, err
	}
	out := make([]Template, 0, len(categories))
	buffer := make([]float64, params.FrameLength)
	for _, category := range categories {
		for i := range buffer {
			buffer[i] = SynthesizeToneSample(category.Tone, float64(i)/float64(sampleRate))
		}
		fp := ExtractFrameBuffer(buffer, -1, 0, sampleRate, params)
		out = append(out, Template{Category: category.Name, Tone: category.Tone, Fingerprint: fp})
	}
	return out, nil
}

// FingerprintSimilarity returns a deterministic [0,1] shape similarity.
func FingerprintSimilarity(a, b Fingerprint) float64 {
	if a.BandCount != b.BandCount || len(a.Bands) != len(b.Bands) || len(a.Bands) == 0 {
		return 0
	}
	var dot, normA, normB, absDiff float64
	var sumA float64
	for i := range a.Bands {
		x := float64(a.Bands[i])
		y := float64(b.Bands[i])
		dot += x * y
		normA += x * x
		normB += y * y
		absDiff += math.Abs(x - y)
		sumA += x
	}
	cosine := 0.0
	if normA > 0 && normB > 0 {
		cosine = dot / (math.Sqrt(normA) * math.Sqrt(normB))
	}
	meanAgreement := 1.0 - absDiff/float64(len(a.Bands)*255.0)
	intersection := 0.0
	var minSum, maxSum float64
	for i := range a.Bands {
		x := float64(a.Bands[i])
		y := float64(b.Bands[i])
		minSum += math.Min(x, y)
		maxSum += math.Max(x, y)
	}
	if maxSum > 0 {
		intersection = minSum / maxSum
	}
	score := 0.50*cosine + 0.25*meanAgreement + 0.25*intersection
	if sumA == 0 {
		score = 0
	}
	return math.Max(0, math.Min(1, score))
}
