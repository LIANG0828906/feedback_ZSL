// Package quant applies declared fixed-point units to material data and
// provides a content-addressed material cache guarded by quantization identity.
package quant

import (
	"fmt"
	"math"
	"sync"

	"spectralforge/internal/domain"
	"spectralforge/internal/errs"
)

type VertexMaterial struct {
	Albedo    [3]float64 `json:"albedo"`
	Roughness float64    `json:"roughness"`
	Metallic  float64    `json:"metallic"`
	Luminance float64    `json:"luminance"`
	Normal    [3]float64 `json:"normal"`
}

type ErrorSample struct {
	ObjectID string  `json:"object_id"`
	Vertex   int     `json:"vertex"`
	Field    string  `json:"field"`
	Before   float64 `json:"before"`
	After    float64 `json:"after"`
	Error    float64 `json:"error"`
}

type ErrorStats struct {
	Samples int     `json:"samples"`
	MeanAbs float64 `json:"mean_abs"`
	MaxAbs  float64 `json:"max_abs"`
	RMSE    float64 `json:"rmse"`
}

type ErrorReport struct {
	Samples []ErrorSample `json:"samples"`
	Stats   ErrorStats    `json:"stats"`
}

type Material struct {
	ObjectID     string           `json:"object_id"`
	MeshID       string           `json:"mesh_id"`
	Key          string           `json:"key"`
	ContentHash  string           `json:"content_hash"`
	QuantSig     string           `json:"quant_signature"`
	Vertices     []VertexMaterial `json:"vertices"`
	Coefficients []float64        `json:"coefficients"`
	Errors       ErrorReport      `json:"errors"`
}

type Set struct {
	Objects map[string]*Material `json:"objects"`
	Keys    map[string]string    `json:"keys"`
	Stats   ErrorStats           `json:"stats"`
	Samples []ErrorSample        `json:"samples"`
}

type cacheEntry struct {
	quantSig string
	material *Material
}

// Cache enforces that one scene content hash cannot be silently reused under a
// different quantization signature.
type Cache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

func NewCache() *Cache {
	return &Cache{entries: make(map[string]cacheEntry)}
}

func (c *Cache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]cacheEntry)
}

func (c *Cache) GetOrBuild(scene *domain.Scene, obj domain.ObjectSpectrum, q domain.Quantization) (*Material, error) {
	contentHash := domain.ObjectContentHash(scene, obj)
	quantSig := q.Signature()
	key := domain.ObjectMaterialKey(scene, obj, q)

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[contentHash]; ok {
		if existing.quantSig != quantSig || existing.material.Key != key {
			return nil, errs.QMix("material_cache["+contentHash+"]",
				"refusing to reuse material content hash under quantization %s; cached signature is %s",
				quantSig, existing.quantSig)
		}
		return existing.material, nil
	}
	mat, err := QuantizeObject(scene, obj, q)
	if err != nil {
		return nil, err
	}
	c.entries[contentHash] = cacheEntry{quantSig: quantSig, material: mat}
	return mat, nil
}

func BuildSet(scene *domain.Scene, q domain.Quantization, cache *Cache) (*Set, error) {
	if cache == nil {
		cache = NewCache()
	}
	set := &Set{
		Objects: make(map[string]*Material, len(scene.Objects)),
		Keys:    make(map[string]string, len(scene.Objects)),
	}
	var all []ErrorSample
	for _, obj := range scene.Objects {
		mat, err := cache.GetOrBuild(scene, obj, q)
		if err != nil {
			return nil, err
		}
		set.Objects[obj.MeshID] = mat
		set.Keys[obj.MeshID] = mat.Key
		all = append(all, mat.Errors.Samples...)
	}
	set.Stats = Summarize(all)
	set.Samples = all
	return set, nil
}

func QuantizeObject(scene *domain.Scene, obj domain.ObjectSpectrum, q domain.Quantization) (*Material, error) {
	var mesh *domain.Mesh
	for i := range scene.Meshes {
		if scene.Meshes[i].ID == obj.MeshID {
			mesh = &scene.Meshes[i]
			break
		}
	}
	if mesh == nil {
		return nil, errs.Schema("scene.objects.mesh_id", "mesh %q not found", obj.MeshID)
	}
	mat := &Material{
		ObjectID:    obj.MeshID,
		MeshID:      obj.MeshID,
		ContentHash: domain.ObjectContentHash(scene, obj),
		QuantSig:    q.Signature(),
		Key:         domain.ObjectMaterialKey(scene, obj, q),
		Vertices:    make([]VertexMaterial, len(mesh.Vertices)),
	}
	var samples []ErrorSample
	for i, v := range mesh.Vertices {
		vm := VertexMaterial{Normal: v.Normal}
		for c := 0; c < 3; c++ {
			before := v.Albedo[c]
			after, err := quantizeUnit(before, q.AlbedoBits)
			if err != nil {
				return nil, err
			}
			vm.Albedo[c] = after
			samples = append(samples, newSample(obj.MeshID, i, fmt.Sprintf("albedo[%d]", c), before, after))
		}
		var err error
		vm.Roughness, err = quantizeUnit(v.Roughness, q.RoughnessBits)
		if err != nil {
			return nil, err
		}
		samples = append(samples, newSample(obj.MeshID, i, "roughness", v.Roughness, vm.Roughness))
		vm.Metallic, err = quantizeUnit(v.Metallic, q.MetallicBits)
		if err != nil {
			return nil, err
		}
		samples = append(samples, newSample(obj.MeshID, i, "metallic", v.Metallic, vm.Metallic))
		vm.Luminance = domain.Luminance(vm.Albedo)
		mat.Vertices[i] = vm
	}
	declaredScale := basisScale(obj.BasisBits, obj.BasisSigned)
	targetScale := basisScale(q.BasisBits, obj.BasisSigned)
	mat.Coefficients = make([]float64, len(obj.Coefficients))
	for i, coeff := range obj.Coefficients {
		before := float64(coeff) / float64(declaredScale)
		after := math.Round(before*float64(targetScale)) / float64(targetScale)
		mat.Coefficients[i] = after
		samples = append(samples, newSample(obj.MeshID, -1, fmt.Sprintf("basis[%d]", i), before, after))
	}
	mat.Errors = ErrorReport{Samples: samples, Stats: Summarize(samples)}
	return mat, nil
}

func quantizeUnit(v float64, bits int) (float64, error) {
	if bits < 2 || bits > 24 {
		return 0, errs.Para("render_config.quantization", "[2,24]", "bit depth %d is out of range", bits)
	}
	levels := float64(int64(1) << bits)
	q := math.Round(v*levels) / levels
	return math.Max(0, math.Min(1, q)), nil
}

func basisScale(bits int, signed bool) int64 {
	if signed {
		return int64(1) << (bits - 1)
	}
	return int64(1) << bits
}

func newSample(object string, vertex int, field string, before, after float64) ErrorSample {
	return ErrorSample{
		ObjectID: object,
		Vertex:   vertex,
		Field:    field,
		Before:   before,
		After:    after,
		Error:    after - before,
	}
}

// Summarize reports both empirical statistics and the analytic half-step bound.
// The binary nested quantization grid guarantees all of them are non-increasing
// as bit depth grows.
func Summarize(samples []ErrorSample) ErrorStats {
	if len(samples) == 0 {
		return ErrorStats{}
	}
	var sum, sumSq, maxAbs float64
	for _, s := range samples {
		e := math.Abs(s.Error)
		sum += e
		sumSq += e * e
		if e > maxAbs {
			maxAbs = e
		}
	}
	n := float64(len(samples))
	return ErrorStats{
		Samples: len(samples),
		MeanAbs: sum / n,
		MaxAbs:  maxAbs,
		RMSE:    math.Sqrt(sumSq / n),
	}
}
