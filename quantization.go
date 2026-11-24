package spectralrender

import (
	"encoding/json"
	"fmt"
	"math"
)

type RenderVertex struct {
	Albedo    RGB
	Roughness float64
	Metallic  float64
}

type QuantSample struct {
	Path   string  `json:"path"`
	Kind   string  `json:"kind"`
	Bits   int     `json:"bits"`
	Unit   int64   `json:"unit"`
	Before float64 `json:"before"`
	After  float64 `json:"after"`
	Error  float64 `json:"error"`
}

type QuantStats struct {
	Count   int     `json:"count"`
	MaxAbs  float64 `json:"max_abs"`
	MeanAbs float64 `json:"mean_abs"`
	RMSE    float64 `json:"rmse"`
}

type QuantizationReport struct {
	Effective           EffectiveQuantization `json:"effective"`
	VertexStats         QuantStats            `json:"vertex_stats"`
	CoefficientStats    QuantStats            `json:"coefficient_stats"`
	OverallStats        QuantStats            `json:"overall_stats"`
	Samples             []QuantSample         `json:"samples"`
	MaterialContentHash string                `json:"material_content_hash"`
	MaterialCacheKey    string                `json:"material_cache_key"`
}

type QuantizedInput struct {
	Vertices     [][]RenderVertex
	Coefficients map[string][]float64
	Report       QuantizationReport
}

func effectiveQuantization(scene *Scene, p RenderParams) EffectiveQuantization {
	q := EffectiveQuantization{
		VertexBits:      scene.Quantization.VertexBits,
		VertexUnit:      scene.Quantization.VertexUnit,
		CoefficientBits: scene.Quantization.CoefficientBits,
		CoefficientUnit: scene.Quantization.CoefficientUnit,
	}
	if p.VertexBitsOverride > 0 {
		q.VertexBits = p.VertexBitsOverride
		q.VertexUnit = quantizedUnit(p.VertexBitsOverride)
	}
	if p.CoefficientBitsOverride > 0 {
		q.CoefficientBits = p.CoefficientBitsOverride
		q.CoefficientUnit = quantizedUnit(p.CoefficientBitsOverride)
	}
	return q
}

func MaterialContentHash(scene *Scene) string {
	type materialContent struct {
		Quantization Quantization `json:"quantization"`
		Materials    []Material   `json:"materials"`
		Meshes       []Mesh       `json:"meshes"`
	}
	b, err := json.Marshal(materialContent{
		Quantization: scene.Quantization,
		Materials:    scene.Materials,
		Meshes:       scene.Meshes,
	})
	if err != nil {
		return ""
	}
	return hashBytes(b)
}

// MaterialCacheKey combines the scene content hash with the effective
// quantization parameters. Content and quantization are both inputs: changing
// either one yields a different key.
func MaterialCacheKey(scene *Scene, sceneHash string, p RenderParams) string {
	q := effectiveQuantization(scene, p)
	params := fmt.Sprintf("vertex:%d/%d;coeff:%d/%d", q.VertexBits, q.VertexUnit,
		q.CoefficientBits, q.CoefficientUnit)
	return hashString(AlgorithmVersion, sceneHash, MaterialContentHash(scene), params)
}

type quantAccumulator struct {
	samples []QuantSample
	sumAbs  float64
	sumSq   float64
	maxAbs  float64
}

func (a *quantAccumulator) add(path, kind string, bits int, unit int64, before, after float64) {
	err := math.Abs(after - before)
	a.samples = append(a.samples, QuantSample{
		Path: path, Kind: kind, Bits: bits, Unit: unit,
		Before: before, After: after, Error: after - before,
	})
	a.sumAbs += err
	a.sumSq += err * err
	if err > a.maxAbs {
		a.maxAbs = err
	}
}

func (a *quantAccumulator) stats() QuantStats {
	n := len(a.samples)
	if n == 0 {
		return QuantStats{}
	}
	return QuantStats{
		Count: n, MaxAbs: a.maxAbs,
		MeanAbs: a.sumAbs / float64(n),
		RMSE:    math.Sqrt(a.sumSq / float64(n)),
	}
}

func QuantizeScene(scene *Scene, p RenderParams) (*QuantizedInput, *Error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	eff := effectiveQuantization(scene, p)
	if eff.VertexUnit <= 0 || eff.CoefficientUnit <= 0 {
		return nil, paraError("quantization", "effective quantization units must be positive")
	}
	input := &QuantizedInput{
		Vertices:     make([][]RenderVertex, len(scene.Meshes)),
		Coefficients: make(map[string][]float64, len(scene.Materials)),
	}
	var vs, cs quantAccumulator
	for mi, mesh := range scene.Meshes {
		verts := make([]RenderVertex, len(mesh.Vertices))
		for vi, v := range mesh.Vertices {
			var rv RenderVertex
			for c := 0; c < 3; c++ {
				before := float64(v.Albedo[c]) / float64(scene.Quantization.VertexUnit)
				after := requantize(before, eff.VertexBits, scene.Quantization.VertexBits, eff.VertexUnit)
				rv.Albedo[c] = after
				vs.add(fmt.Sprintf("meshes[%d].vertices[%d].albedo[%d]", mi, vi, c),
					"vertex", eff.VertexBits, eff.VertexUnit, before, after)
			}
			before := float64(v.Roughness) / float64(scene.Quantization.VertexUnit)
			rv.Roughness = requantize(before, eff.VertexBits, scene.Quantization.VertexBits, eff.VertexUnit)
			vs.add(fmt.Sprintf("meshes[%d].vertices[%d].roughness", mi, vi),
				"vertex", eff.VertexBits, eff.VertexUnit, before, rv.Roughness)

			before = float64(v.Metallic) / float64(scene.Quantization.VertexUnit)
			rv.Metallic = requantize(before, eff.VertexBits, scene.Quantization.VertexBits, eff.VertexUnit)
			vs.add(fmt.Sprintf("meshes[%d].vertices[%d].metallic", mi, vi),
				"vertex", eff.VertexBits, eff.VertexUnit, before, rv.Metallic)
			verts[vi] = rv
		}
		input.Vertices[mi] = verts
	}
	for _, mat := range scene.Materials {
		unit := mat.CoefficientUnit
		bits := mat.CoefficientBits
		if p.CoefficientBitsOverride > 0 {
			unit = eff.CoefficientUnit
			bits = eff.CoefficientBits
		}
		coeffs := make([]float64, len(mat.Coefficients))
		for i, c := range mat.Coefficients {
			before := float64(c) / float64(mat.CoefficientUnit)
			after := requantize(before, bits, mat.CoefficientBits, unit)
			coeffs[i] = after
			cs.add(fmt.Sprintf("materials[%s].coefficients[%d]", mat.ID, i),
				"coefficient", bits, unit, before, after)
		}
		input.Coefficients[mat.ID] = coeffs
	}
	all := append(append([]QuantSample{}, vs.samples...), cs.samples...)
	var overall quantAccumulator
	overall.samples = all
	for _, sample := range all {
		e := math.Abs(sample.Error)
		overall.sumAbs += e
		overall.sumSq += e * e
		if e > overall.maxAbs {
			overall.maxAbs = e
		}
	}
	input.Report = QuantizationReport{
		Effective:           eff,
		VertexStats:         vs.stats(),
		CoefficientStats:    cs.stats(),
		OverallStats:        overall.stats(),
		MaterialContentHash: MaterialContentHash(scene),
		MaterialCacheKey:    MaterialCacheKey(scene, HashScene(scene), p),
	}
	input.Report.Samples = all
	return input, nil
}

func requantize(value float64, targetBits, sourceBits int, targetUnit int64) float64 {
	if targetBits >= sourceBits {
		return value
	}
	q := roundToUnit(value, targetUnit)
	if q < 0 {
		q = 0
	}
	if q > targetUnit {
		q = targetUnit
	}
	return float64(q) / float64(targetUnit)
}
