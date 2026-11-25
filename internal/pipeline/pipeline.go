package pipeline

import (
	"math"

	"ms-workbench/internal/apperr"
	"ms-workbench/internal/color"
	"ms-workbench/internal/scene"
)

type FusedChunk struct {
	Data         []float64
	PeakRadiance float64
}

type BoundaryPoint struct {
	X         int        `json:"x"`
	Y         int        `json:"y"`
	RawRGB    scene.Vec3 `json:"rawRGB"`
	MappedRGB scene.Vec3 `json:"mappedRGB"`
}

type ChromaOutput struct {
	LinearRGB    []float64
	RawRGB       []float64
	OutOfGamut   []int
	Boundary     []BoundaryPoint
	PeakRadiance float64
}

type ChunkResult struct {
	Fused  *FusedChunk
	Chroma *ChromaOutput
}

type PatchMeasurement struct {
	Name     string     `json:"name"`
	X        int        `json:"x"`
	Y        int        `json:"y"`
	Expected scene.Vec3 `json:"expected"`
	Actual   scene.Vec3 `json:"actual"`
	Delta    float64    `json:"delta"`
	Pass     bool       `json:"pass"`
}

func ValidateRegistration(s *scene.Scene, settings scene.RenderSettings) error {
	if len(s.Bands) == 0 {
		return apperr.New(apperr.ESCHEMA, "scene has no bands")
	}
	ref := s.Bands[0]
	for _, b := range s.Bands[1:] {
		mag := math.Hypot(b.OffsetX, b.OffsetY)
		if mag > settings.MaxRegistrationOffset+1e-12 {
			return &apperr.Error{
				Code:     apperr.EREG,
				Message:  "registration offset exceeds configurable correction range",
				BandPair: ref.ID + "/" + b.ID,
				OffsetX:  b.OffsetX,
				OffsetY:  b.OffsetY,
				Measured: mag,
				Limit:    settings.MaxRegistrationOffset,
			}
		}
	}
	return nil
}

func FuseChunk(s *scene.Scene, rect scene.Rect, settings scene.RenderSettings) (*FusedChunk, error) {
	if err := ValidateRegistration(s, settings); err != nil {
		return nil, err
	}
	return fuseChunkValidated(s, rect, settings), nil
}

func fuseChunkValidated(s *scene.Scene, rect scene.Rect, settings scene.RenderSettings) *FusedChunk {
	bandCount := len(s.Bands)
	out := &FusedChunk{Data: make([]float64, rect.W*rect.H*bandCount)}
	maxCode := s.MaxCode()
	for bi := range s.Bands {
		b := &s.Bands[bi]
		for y := 0; y < rect.H; y++ {
			for x := 0; x < rect.W; x++ {
				gx := float64(rect.X + x)
				gy := float64(rect.Y + y)
				radiance, ok := fusePixel(s, b, gx, gy, settings, maxCode)
				local := y*rect.W + x
				if ok {
					detail := sampleGrid(b.Detail, s.Width, s.Height, gx+b.OffsetX, gy+b.OffsetY, 1)
					radiance += detail * settings.DetailGain
				}
				out.Data[local*bandCount+bi] = radiance
				if radiance > out.PeakRadiance {
					out.PeakRadiance = radiance
				}
			}
		}
	}
	return out
}

func fusePixel(s *scene.Scene, b *scene.Band, x, y float64, settings scene.RenderSettings, maxCode float64) (float64, bool) {
	sx := x + b.OffsetX
	sy := y + b.OffsetY
	var weighted, weightSum float64
	var bestValue, bestWeight float64
	for _, e := range b.Exposures {
		code := sampleGrid(toFloatCodes(e.Codes), s.Width, s.Height, sx, sy, settings.InterpolationOrder)
		if code < 0 {
			code = 0
		} else if code > maxCode {
			code = maxCode
		}
		span := maxCode - s.Response.BlackLevel
		if span <= 0 {
			continue
		}
		norm := (code - s.Response.BlackLevel) / span
		if norm < 0 {
			norm = 0
		} else if norm > 1 {
			norm = 1
		}
		shape := math.Pow(math.Abs(2*norm-1), settings.FusionPower)
		weight := math.Exp(-settings.FusionStrength * shape)
		if norm <= 0.01 || norm >= 0.99 {
			weight *= 0.0005
		}
		linear := math.Pow(norm, s.Response.InverseGamma)
		radiance := linear * s.Response.FullWellRadiance * 1_000_000 / float64(e.DurationUS)
		weighted += weight * radiance
		weightSum += weight
		if weight > bestWeight {
			bestWeight, bestValue = weight, radiance
		}
	}
	if weightSum <= 0 {
		return bestValue, bestWeight > 0
	}
	return weighted / weightSum, true
}

func ChromaChunk(s *scene.Scene, rect scene.Rect, fused *FusedChunk, settings scene.RenderSettings) *ChromaOutput {
	bandCount := len(s.Bands)
	responses := make([]scene.Vec3, bandCount)
	for i := range s.Bands {
		responses[i] = s.Bands[i].ResponseXYZ
	}
	out := &ChromaOutput{
		LinearRGB:    make([]float64, rect.W*rect.H*3),
		RawRGB:       make([]float64, rect.W*rect.H*3),
		PeakRadiance: fused.PeakRadiance,
	}
	radiance := make([]float64, bandCount)
	for y := 0; y < rect.H; y++ {
		for x := 0; x < rect.W; x++ {
			local := y*rect.W + x
			for bi := 0; bi < bandCount; bi++ {
				radiance[bi] = fused.Data[local*bandCount+bi]
			}
			mapped, raw, oog := color.ReconstructLinearRGB(radiance, responses, s.WhitePoint, settings.GamutIntent)
			for c := 0; c < 3; c++ {
				out.LinearRGB[local*3+c] = mapped[c]
				out.RawRGB[local*3+c] = raw[c]
			}
			if oog {
				out.OutOfGamut = append(out.OutOfGamut, local)
				out.Boundary = append(out.Boundary, BoundaryPoint{
					X:         rect.X + x,
					Y:         rect.Y + y,
					RawRGB:    raw,
					MappedRGB: mapped,
				})
			}
		}
	}
	return out
}

func ProcessChunk(s *scene.Scene, rect scene.Rect, settings scene.RenderSettings) (*ChunkResult, error) {
	fused, err := FuseChunk(s, rect, settings)
	if err != nil {
		return nil, err
	}
	return &ChunkResult{Fused: fused, Chroma: ChromaChunk(s, rect, fused, settings)}, nil
}

func sampleGrid(grid []float64, w, h int, x, y float64, order int) float64 {
	if len(grid) == 0 {
		return 0
	}
	if order <= 0 {
		ix := int(math.Round(x))
		iy := int(math.Round(y))
		return gridAt(grid, w, h, ix, iy)
	}
	if order > 5 {
		order = 5
	}
	n := order + 1
	startX := int(math.Floor(x)) - order/2
	startY := int(math.Floor(y)) - order/2
	var sum float64
	for jy := 0; jy < n; jy++ {
		wy := lagrangeWeight(y-float64(startY), jy, n)
		if math.Abs(wy) < 1e-18 {
			continue
		}
		for jx := 0; jx < n; jx++ {
			wx := lagrangeWeight(x-float64(startX), jx, n)
			if math.Abs(wx) < 1e-18 {
				continue
			}
			sum += wx * wy * gridAt(grid, w, h, startX+jx, startY+jy)
		}
	}
	return sum
}

func lagrangeWeight(pos float64, j, n int) float64 {
	w := 1.0
	for k := 0; k < n; k++ {
		if k == j {
			continue
		}
		w *= (pos - float64(k)) / float64(j-k)
	}
	return w
}

func gridAt(grid []float64, w, h, x, y int) float64 {
	if x < 0 || y < 0 || x >= w || y >= h {
		return 0
	}
	return grid[y*w+x]
}

func toFloatCodes(codes []uint16) []float64 {
	out := make([]float64, len(codes))
	for i, c := range codes {
		out[i] = float64(c)
	}
	return out
}

func MeasurePatches(s *scene.Scene, rgb []float64) []PatchMeasurement {
	out := make([]PatchMeasurement, len(s.Patches))
	for i, p := range s.Patches {
		x := p.Rect.X + p.Rect.W/2
		y := p.Rect.Y + p.Rect.H/2
		expected := p.ExpectedLinearRGB
		if candidate, ok := p.ExpectedByIntent[s.Settings.GamutIntent]; ok {
			expected = candidate
		}
		actual := scene.Vec3{
			rgb[(y*s.Width+x)*3],
			rgb[(y*s.Width+x)*3+1],
			rgb[(y*s.Width+x)*3+2],
		}
		delta := 0.0
		for c := 0; c < 3; c++ {
			d := math.Abs(actual[c] - expected[c])
			if d > delta {
				delta = d
			}
		}
		out[i] = PatchMeasurement{
			Name:     p.Name,
			X:        x,
			Y:        y,
			Expected: expected,
			Actual:   actual,
			Delta:    delta,
			Pass:     delta <= s.Truth.PatchTolerance,
		}
	}
	return out
}

func (c *ChunkResult) Clone() *ChunkResult {
	if c == nil {
		return nil
	}
	return &ChunkResult{
		Fused: &FusedChunk{
			Data:         append([]float64(nil), c.Fused.Data...),
			PeakRadiance: c.Fused.PeakRadiance,
		},
		Chroma: &ChromaOutput{
			LinearRGB:    append([]float64(nil), c.Chroma.LinearRGB...),
			RawRGB:       append([]float64(nil), c.Chroma.RawRGB...),
			OutOfGamut:   append([]int(nil), c.Chroma.OutOfGamut...),
			Boundary:     append([]BoundaryPoint(nil), c.Chroma.Boundary...),
			PeakRadiance: c.Chroma.PeakRadiance,
		},
	}
}
