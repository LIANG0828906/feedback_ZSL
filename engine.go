package spectralrender

import (
	"fmt"
	"math"
)

// SpectralEngine precomputes the wavelength integration for every
// material/light pair. Per-pixel work then only evaluates geometric shading and
// indirect bounces, while the expensive CMF integral remains deterministic.
type SpectralEngine struct {
	scene            *Scene
	params           RenderParams
	wavelengths      []float64
	weights          []float64
	materialIndex    map[string]int
	materialLightXYZ [][]RGB
	samplingVersion  string
	materialKey      string
	quantized        *QuantizedInput
	whiteScale       RGB
}

func NewSpectralEngine(scene *Scene, p RenderParams) (*SpectralEngine, *Error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	q, err := QuantizeScene(scene, p)
	if err != nil {
		return nil, err
	}
	w, err := SampleGrid(scene.Spectrum.StartNM, scene.Spectrum.EndNM, p.SampleCount)
	if err != nil {
		return nil, err
	}
	weights, err := IntegrationWeights(w, p.Integration)
	if err != nil {
		return nil, err
	}
	sv, err := SamplingVersion(scene, p.SampleCount, p.Integration)
	if err != nil {
		return nil, err
	}
	e := &SpectralEngine{
		scene:            scene,
		params:           p,
		wavelengths:      w,
		weights:          weights,
		materialIndex:    make(map[string]int, len(scene.Materials)),
		materialLightXYZ: make([][]RGB, len(scene.Materials)),
		samplingVersion:  sv,
		materialKey:      q.Report.MaterialCacheKey,
		quantized:        q,
	}
	white := matrixVector(scene.Spectrum.XYZToRGB, [3]float64{1, 1, 1})
	e.whiteScale = RGB{white[0], white[1], white[2]}
	for i, m := range scene.Materials {
		e.materialIndex[m.ID] = i
	}
	for mi, mat := range scene.Materials {
		coeffs := q.Coefficients[mat.ID]
		perLight := make([]RGB, len(scene.Lights))
		for li, light := range scene.Lights {
			var xyz RGB
			for si, wl := range w {
				refl := 0.0
				for bi, basis := range mat.Basis {
					refl += coeffs[bi] * interpolateTable(scene.Spectrum.Wavelengths, basis.Values, wl)
				}
				if refl < 0 {
					refl = 0
				}
				intensity := interpolateTable(scene.Spectrum.Wavelengths, light.Intensity, wl)
				cx := interpolateTable(scene.Spectrum.Wavelengths, scene.Spectrum.CMF.X, wl)
				cy := interpolateTable(scene.Spectrum.Wavelengths, scene.Spectrum.CMF.Y, wl)
				cz := interpolateTable(scene.Spectrum.Wavelengths, scene.Spectrum.CMF.Z, wl)
				factor := weights[si] * intensity * refl
				xyz[0] += factor * cx
				xyz[1] += factor * cy
				xyz[2] += factor * cz
			}
			perLight[li] = xyz
		}
		e.materialLightXYZ[mi] = perLight
	}
	return e, nil
}

func (e *SpectralEngine) WhiteNormalizedRGB(xyz RGB) RGB {
	n := RGB{
		xyz[0] / e.scene.Spectrum.WhitePoint[0],
		xyz[1] / e.scene.Spectrum.WhitePoint[1],
		xyz[2] / e.scene.Spectrum.WhitePoint[2],
	}
	v := matrixVector(e.scene.Spectrum.XYZToRGB, n)
	return RGB{v[0] / e.whiteScale[0], v[1] / e.whiteScale[1], v[2] / e.whiteScale[2]}
}

func (e *SpectralEngine) shade(meshIndex int, lightIndex int, n [3]float64) RGB {
	mesh := e.scene.Meshes[meshIndex]
	matIdx := e.materialIndex[mesh.MaterialID]
	light := e.scene.Lights[lightIndex]
	ld := normalized(light.Direction)
	ndotl := dot(normalized(n), ld)
	if ndotl < 0 {
		ndotl = 0
	}
	xyz := e.materialLightXYZ[matIdx][lightIndex]
	return xyz.Mul(ndotl)
}

func (e *SpectralEngine) materialValue(meshIndex int) (RGB, float64, float64) {
	// This helper is only used for documentation/tests. Pixel rendering
	// interpolates the quantized vertex values directly.
	v := e.quantized.Vertices[meshIndex][0]
	return v.Albedo, v.Roughness, v.Metallic
}

func (e *SpectralEngine) debugString() string {
	return fmt.Sprintf("samples=%d method=%s material=%s", e.params.SampleCount, e.params.Integration, e.materialKey)
}

func sanitizeXYZ(xyz RGB) RGB {
	return RGB{
		finiteOr(xyz[0], 0),
		finiteOr(xyz[1], 0),
		finiteOr(xyz[2], 0),
	}
}

func maxAbsDiff(a, b RGB) float64 {
	return math.Max(math.Abs(a[0]-b[0]), math.Max(math.Abs(a[1]-b[1]), math.Abs(a[2]-b[2])))
}
