package spectralrender

import (
	"encoding/json"
	"fmt"
	"math"
)

type IntegrationMethod string

const (
	IntegrationRectangle IntegrationMethod = "rectangle"
	IntegrationTrapezoid IntegrationMethod = "trapezoid"
	IntegrationSimpson   IntegrationMethod = "simpson"
)

func (m IntegrationMethod) Valid() bool {
	switch m {
	case IntegrationRectangle, IntegrationTrapezoid, IntegrationSimpson:
		return true
	default:
		return false
	}
}

func ParseIntegration(s string) (IntegrationMethod, error) {
	m := IntegrationMethod(s)
	if !m.Valid() {
		return "", paraError("integration", "must be one of rectangle, trapezoid, simpson")
	}
	return m, nil
}

func SampleGrid(start, end float64, count int) ([]float64, *Error) {
	if count < 4 || count > 4097 {
		return nil, paraError("sample_count", "must be in [4,4097]")
	}
	if !isFinite(start) || !isFinite(end) || end <= start {
		return nil, paraError("wavelength_range", "must be finite with end > start")
	}
	out := make([]float64, count)
	step := (end - start) / float64(count-1)
	for i := range out {
		out[i] = start + step*float64(i)
	}
	out[count-1] = end
	return out, nil
}

// IntegrationWeights returns the quadrature weights for a uniform sample grid.
// Simpson is composed on the leading odd-sized part for even sample counts and
// the final interval is integrated with a trapezoid.
func IntegrationWeights(wavelengths []float64, method IntegrationMethod) ([]float64, *Error) {
	if len(wavelengths) < 2 {
		return nil, paraError("sample_count", "at least two wavelength samples are required")
	}
	if !method.Valid() {
		return nil, paraError("integration", "unsupported integration method")
	}
	n := len(wavelengths)
	weights := make([]float64, n)
	switch method {
	case IntegrationRectangle:
		for i := 0; i < n-1; i++ {
			weights[i] = wavelengths[i+1] - wavelengths[i]
		}
	case IntegrationTrapezoid:
		for i := 0; i < n-1; i++ {
			h := wavelengths[i+1] - wavelengths[i]
			weights[i] += h / 2
			weights[i+1] += h / 2
		}
	case IntegrationSimpson:
		if n < 3 {
			return IntegrationWeights(wavelengths, IntegrationTrapezoid)
		}
		last := n - 1
		even := n%2 == 0
		if even {
			last = n - 2
		}
		for i := 0; i < last; i += 2 {
			h := wavelengths[i+1] - wavelengths[i]
			weights[i] += h / 3
			weights[i+1] += 4 * h / 3
			weights[i+2] += h / 3
		}
		if even {
			h := wavelengths[n-1] - wavelengths[n-2]
			weights[n-2] += h / 2
			weights[n-1] += h / 2
		}
	}
	return weights, nil
}

func SampleCountForScene(scene *Scene, count int) ([]float64, []float64, *Error) {
	w, err := SampleGrid(scene.Spectrum.StartNM, scene.Spectrum.EndNM, count)
	if err != nil {
		return nil, nil, err
	}
	weights, err := IntegrationWeights(w, IntegrationTrapezoid)
	return w, weights, err
}

func spectrumHash(scene *Scene) string {
	b, err := json.Marshal(scene.Spectrum)
	if err != nil {
		return ""
	}
	return hashBytes(b)
}

func SamplingVersion(scene *Scene, count int, method IntegrationMethod) (string, *Error) {
	if count < 4 || count > 4097 {
		return "", paraError("sample_count", "must be in [4,4097]")
	}
	if !method.Valid() {
		return "", paraError("integration", "unsupported integration method")
	}
	payload := fmt.Sprintf("%s|%d|%s|%.17g|%.17g", spectrumHash(scene), count, method,
		scene.Spectrum.StartNM, scene.Spectrum.EndNM)
	return hashString(AlgorithmVersion, payload), nil
}

func luminosityFromXYZ(xyz [3]float64) float64 { return xyz[1] }

func chromaticity(xyz [3]float64) [2]float64 {
	sum := xyz[0] + xyz[1] + xyz[2]
	if sum <= 0 || !isFinite(sum) {
		return [2]float64{0, 0}
	}
	return [2]float64{xyz[0] / sum, xyz[1] / sum}
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

func finiteOr(v, fallback float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fallback
	}
	return v
}
