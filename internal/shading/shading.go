// Package shading contains the pure per-pixel spectral BRDF approximation used
// by both the deterministic generator (for analytic truth) and the renderer.
package shading

import (
	"math"

	"spectralforge/internal/domain"
	"spectralforge/internal/quant"
	"spectralforge/internal/spectral"
)

type Attributes struct {
	Albedo    [3]float64
	Roughness float64
	Metallic  float64
	Normal    [3]float64
}

func Interpolate(mat *quant.Material, indices [3]int, bary [3]float64) Attributes {
	var a Attributes
	for k := 0; k < 3; k++ {
		v := mat.Vertices[indices[k]]
		for c := 0; c < 3; c++ {
			a.Albedo[c] += bary[k] * v.Albedo[c]
			a.Normal[c] += bary[k] * v.Normal[c]
		}
		a.Roughness += bary[k] * v.Roughness
		a.Metallic += bary[k] * v.Metallic
	}
	return a
}

// Shade returns the reflected XYZ and the maximum spectral radiance over the
// quadrature samples.
func Shade(attrs Attributes, mat *quant.Material, kernel *spectral.Kernel, cfg domain.RenderConfig) (spectral.XYZ, float64) {
	tint := domain.Luminance(attrs.Albedo)
	metal := clamp01(attrs.Metallic)
	rough := math.Max(0.02, clamp01(attrs.Roughness))
	normal := normalize(attrs.Normal)
	view := [3]float64{0, 0, 1}
	diffuseWeight := (1 - metal) * tint
	f0 := 0.04*(1-metal) + tint*metal
	shininess := math.Max(2, 2/(rough*rough*rough*rough+1e-4)-2)

	var out spectral.XYZ
	for i := range kernel.Lights {
		l := &kernel.Lights[i]
		lightDir := normalize(l.Direction)
		ndotl := math.Max(0, dot(normal, lightDir))
		if ndotl == 0 {
			continue
		}
		half := normalize([3]float64{lightDir[0] + view[0], lightDir[1] + view[1], lightDir[2] + view[2]})
		specScalar := f0 * math.Pow(math.Max(0, dot(normal, half)), shininess)
		refl := basisXYZ(l.Basis, mat.Coefficients)
		contrib := refl.Mul(diffuseWeight).Add(l.White.Mul(specScalar))
		out = out.Add(contrib.Mul(ndotl))
	}

	avgRefl := basisXYZ(kernel.Average.Basis, mat.Coefficients)
	power := 1.0
	for b := 0; b < cfg.IndirectBounces; b++ {
		factor := 0.12 * tint * (1 - metal) * power
		out = out.Add(avgRefl.Mul(factor))
		power *= 0.35 * tint * (1 - metal)
	}
	return out, peakRadiance(attrs, mat, kernel, cfg, diffuseWeight, f0, shininess)
}

func LinearRGB(xyz spectral.XYZ, kernel *spectral.Kernel) [3]float64 {
	return spectral.NormalizeToLinearRGB(xyz, kernel.WhitePoint, kernel.Gamut)
}

func peakRadiance(attrs Attributes, mat *quant.Material, kernel *spectral.Kernel, cfg domain.RenderConfig, diffuseWeight, f0, shininess float64) float64 {
	tint := domain.Luminance(attrs.Albedo)
	metal := clamp01(attrs.Metallic)
	normal := normalize(attrs.Normal)
	view := [3]float64{0, 0, 1}
	peak := 0.0
	for _, p := range kernel.Points {
		rho := clamp01(evalReflectance(mat, kernel, p.Wavelength))
		radiance := 0.0
		for i := range kernel.Lights {
			l := &kernel.Lights[i]
			lightDir := normalize(l.Direction)
			ndotl := math.Max(0, dot(normal, lightDir))
			if ndotl == 0 {
				continue
			}
			half := normalize([3]float64{lightDir[0] + view[0], lightDir[1] + view[1], lightDir[2] + view[2]})
			specScalar := f0 * math.Pow(math.Max(0, dot(normal, half)), shininess)
			intensity := spectral.Interp(kernel.Wavelengths, kernel.LightIntensities[i], p.Wavelength)
			radiance += ndotl * intensity * (diffuseWeight*rho + specScalar)
		}
		power := 1.0
		for b := 0; b < cfg.IndirectBounces; b++ {
			avgIntensity := spectral.Interp(kernel.Wavelengths, kernel.AverageIntensity, p.Wavelength)
			radiance += avgIntensity * rho * 0.12 * tint * (1 - metal) * power
			power *= 0.35 * tint * (1 - metal)
		}
		if radiance > peak {
			peak = radiance
		}
	}
	return peak
}

func evalReflectance(mat *quant.Material, kernel *spectral.Kernel, wavelength float64) float64 {
	value := 0.0
	n := len(mat.Coefficients)
	if len(kernel.BasisValues) < n {
		n = len(kernel.BasisValues)
	}
	for i := 0; i < n; i++ {
		value += mat.Coefficients[i] * spectral.Interp(kernel.Wavelengths, kernel.BasisValues[i], wavelength)
	}
	return value
}

func basisXYZ(basis []spectral.XYZ, coeff []float64) spectral.XYZ {
	var out spectral.XYZ
	n := len(basis)
	if len(coeff) < n {
		n = len(coeff)
	}
	for i := 0; i < n; i++ {
		out.X += basis[i].X * coeff[i]
		out.Y += basis[i].Y * coeff[i]
		out.Z += basis[i].Z * coeff[i]
	}
	return out
}

func normalize(v [3]float64) [3]float64 {
	n := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
	if n < 1e-15 {
		return [3]float64{0, 0, 1}
	}
	return [3]float64{v[0] / n, v[1] / n, v[2] / n}
}

func dot(a, b [3]float64) float64 {
	return a[0]*b[0] + a[1]*b[1] + a[2]*b[2]
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(0, math.Min(1, v))
}
