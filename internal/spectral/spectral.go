// Package spectral implements deterministic wavelength resampling and
// color-matching integration. It contains no rendering state.
package spectral

import (
	"fmt"
	"math"
	"sort"

	"spectralforge/internal/domain"
	"spectralforge/internal/errs"
)

type XYZ struct {
	X float64
	Y float64
	Z float64
}

func (x XYZ) Add(y XYZ) XYZ {
	return XYZ{X: x.X + y.X, Y: x.Y + y.Y, Z: x.Z + y.Z}
}

func (x XYZ) Mul(v float64) XYZ {
	return XYZ{X: x.X * v, Y: x.Y * v, Z: x.Z * v}
}

type SamplePoint struct {
	Wavelength float64
	Weight     float64
}

// IntegrationSamples returns strictly increasing sample wavelengths and their
// quadrature weights. Trapezoid uses N nodes; midpoint uses N bins.
func IntegrationSamples(wavelengths []float64, steps int, method string) ([]SamplePoint, error) {
	if len(wavelengths) < 2 {
		return nil, errs.Schema("spectral.wavelengths", "at least two samples are required")
	}
	if steps < 2 || steps > 4096 {
		return nil, errs.Para("render_config.wavelength_steps", "[2,4096]", "step count %d is out of range", steps)
	}
	lo, hi := wavelengths[0], wavelengths[len(wavelengths)-1]
	if !(hi > lo) {
		return nil, errs.Para("spectral.wavelengths", "strictly increasing", "invalid wavelength interval")
	}
	points := make([]SamplePoint, steps)
	switch method {
	case "trapezoid":
		d := (hi - lo) / float64(steps-1)
		for i := 0; i < steps; i++ {
			points[i] = SamplePoint{
				Wavelength: lo + float64(i)*d,
				Weight:     d,
			}
			if i == 0 || i == steps-1 {
				points[i].Weight = d / 2
			}
		}
	case "midpoint":
		d := (hi - lo) / float64(steps)
		for i := 0; i < steps; i++ {
			points[i] = SamplePoint{
				Wavelength: lo + (float64(i)+0.5)*d,
				Weight:     d,
			}
		}
	default:
		return nil, errs.Para("render_config.integration", `["trapezoid","midpoint"]`,
			"unsupported integration method %q", method)
	}
	return points, nil
}

// Interp linearly interpolates a table sampled at xs. Values outside the table
// are held at the endpoint values.
func Interp(xs, ys []float64, x float64) float64 {
	n := len(xs)
	if n == 0 || len(ys) < n {
		return 0
	}
	if x <= xs[0] {
		return ys[0]
	}
	if x >= xs[n-1] {
		return ys[n-1]
	}
	i := sort.SearchFloat64s(xs, x)
	if i < n && xs[i] == x {
		return ys[i]
	}
	i--
	if i < 0 {
		return ys[0]
	}
	if i >= n-1 {
		return ys[n-1]
	}
	t := (x - xs[i]) / (xs[i+1] - xs[i])
	return ys[i] + t*(ys[i+1]-ys[i])
}

type LightKernel struct {
	ID        string
	Direction [3]float64
	White     XYZ
	Basis     []XYZ
}

// Kernel precomputes every wavelength-dependent product except the per-pixel
// reflectance tint and BRDF factors. The integration loop is still explicit:
// light intensity * basis reflectance * CMF * quadrature weight.
type Kernel struct {
	Points           []SamplePoint
	Lights           []LightKernel
	Average          LightKernel
	BasisCount       int
	Wavelengths      []float64
	BasisValues      [][]float64
	LightIntensities [][]float64
	AverageIntensity []float64
	WhitePoint       [3]float64
	Gamut            domain.Gamut
}

func BuildKernel(s *domain.SpectralData, c domain.RenderConfig) (*Kernel, error) {
	points, err := IntegrationSamples(s.Wavelengths, c.WavelengthSteps, c.Integration)
	if err != nil {
		return nil, err
	}
	k := &Kernel{
		Points:      points,
		BasisCount:  len(s.Basis),
		Wavelengths: append([]float64(nil), s.Wavelengths...),
		WhitePoint:  s.WhitePoint,
		Gamut:       s.Gamut,
	}
	k.BasisValues = make([][]float64, len(s.Basis))
	for i := range s.Basis {
		k.BasisValues[i] = append([]float64(nil), s.Basis[i].Values...)
	}
	for i := range s.Lights {
		lk := integrateLight(s, s.Lights[i].ID, s.Lights[i].Direction, s.Lights[i].Intensity, points)
		k.Lights = append(k.Lights, lk)
		k.LightIntensities = append(k.LightIntensities, append([]float64(nil), s.Lights[i].Intensity...))
	}
	avg := make([]float64, len(s.Wavelengths))
	for i := range avg {
		sum := 0.0
		for j := range s.Lights {
			sum += s.Lights[j].Intensity[i]
		}
		avg[i] = sum / float64(len(s.Lights))
	}
	k.Average = integrateLight(s, "average", [3]float64{0, 0, 1}, avg, points)
	k.AverageIntensity = avg
	return k, nil
}

func integrateLight(s *domain.SpectralData, id string, direction [3]float64, intensity []float64, points []SamplePoint) LightKernel {
	lk := LightKernel{
		ID:        id,
		Direction: direction,
		Basis:     make([]XYZ, len(s.Basis)),
	}
	for _, point := range points {
		li := Interp(s.Wavelengths, intensity, point.Wavelength)
		cx := Interp(s.Wavelengths, s.CMF.X, point.Wavelength)
		cy := Interp(s.Wavelengths, s.CMF.Y, point.Wavelength)
		cz := Interp(s.Wavelengths, s.CMF.Z, point.Wavelength)
		scale := point.Weight * li
		lk.White.X += scale * cx
		lk.White.Y += scale * cy
		lk.White.Z += scale * cz
		for b := range s.Basis {
			bv := Interp(s.Wavelengths, s.Basis[b].Values, point.Wavelength)
			term := scale * bv
			lk.Basis[b].X += term * cx
			lk.Basis[b].Y += term * cy
			lk.Basis[b].Z += term * cz
		}
	}
	return lk
}

// IntegrateProducts is a direct, unoptimized integration used by tests and
// diagnostics. It computes sum(light * reflectance * CMF * weight).
func IntegrateProducts(s *domain.SpectralData, lightIndex int, points []SamplePoint, reflectance []float64) (XYZ, error) {
	if lightIndex < 0 || lightIndex >= len(s.Lights) {
		return XYZ{}, errs.Para("spectral.lights", fmt.Sprintf("[0,%d]", len(s.Lights)-1), "light index out of range")
	}
	var out XYZ
	for _, p := range points {
		li := Interp(s.Wavelengths, s.Lights[lightIndex].Intensity, p.Wavelength)
		rv := Interp(s.Wavelengths, reflectance, p.Wavelength)
		scale := p.Weight * li * rv
		out.X += scale * Interp(s.Wavelengths, s.CMF.X, p.Wavelength)
		out.Y += scale * Interp(s.Wavelengths, s.CMF.Y, p.Wavelength)
		out.Z += scale * Interp(s.Wavelengths, s.CMF.Z, p.Wavelength)
	}
	return out, nil
}

func NormalizeToLinearRGB(xyz XYZ, white [3]float64, gamut domain.Gamut) [3]float64 {
	n := [3]float64{xyz.X / white[0], xyz.Y / white[1], xyz.Z / white[2]}
	var rgb [3]float64
	for r := 0; r < 3; r++ {
		rgb[r] = gamut.XYZToRGB[r][0]*n[0] + gamut.XYZToRGB[r][1]*n[1] + gamut.XYZToRGB[r][2]*n[2]
	}
	return rgb
}

func Chromaticity(xyz XYZ) [2]float64 {
	sum := xyz.X + xyz.Y + xyz.Z
	if math.Abs(sum) < 1e-15 {
		return [2]float64{}
	}
	return [2]float64{xyz.X / sum, xyz.Y / sum}
}

func Luminance(rgb [3]float64) float64 {
	return domain.Luminance(rgb)
}
