// Package sample creates a deterministic scene with analytic truth.
package sample

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"spectralforge/internal/domain"
	"spectralforge/internal/errs"
	"spectralforge/internal/geom"
	"spectralforge/internal/quant"
	"spectralforge/internal/shading"
	"spectralforge/internal/spectral"
)

type objectSpec struct {
	meshID     string
	name       string
	coeffs     []int64
	baseAlbedo [3]float64
	roughness  float64
	metallic   float64
}

// Generate writes scene.json, spectral.json, truth.json and render-config.json.
// All arithmetic that produces truth is done here, never by the renderer.
func Generate(dir string, seed int64) (*domain.SampleBundle, error) {
	scene, spectralData := build(seed)
	config := domain.DefaultConfig()
	bundle := &domain.SampleBundle{
		Scene:    scene,
		Spectral: spectralData,
		Config:   &config,
	}
	if err := bundle.FinishHashes(); err != nil {
		return nil, errs.Decode("sample", "cannot hash generated scene: %v", err)
	}

	truth, err := buildTruth(bundle)
	if err != nil {
		return nil, err
	}
	bundle.Truth = truth
	if err := domain.ValidateBundle(bundle); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errs.IO("sample_dir", err)
	}
	files := []struct {
		name string
		v    any
	}{
		{domain.SceneFileName, scene},
		{domain.SpectralFileName, spectralData},
		{domain.TruthFileName, truth},
		{domain.ConfigFileName, config},
	}
	for _, f := range files {
		data, err := json.MarshalIndent(f.v, "", "  ")
		if err != nil {
			return nil, errs.Decode(f.name, "cannot encode: %v", err)
		}
		data = append(data, '\n')
		if err := os.WriteFile(filepath.Join(dir, f.name), data, 0o644); err != nil {
			return nil, errs.IO(f.name, err)
		}
	}
	manifest := struct {
		Seed         int64    `json:"seed"`
		ContentHash  string   `json:"content_hash"`
		SceneHash    string   `json:"scene_hash"`
		SpectralHash string   `json:"spectral_hash"`
		Files        []string `json:"files"`
	}{
		Seed:         seed,
		ContentHash:  bundle.ContentHash,
		SceneHash:    bundle.SceneHash,
		SpectralHash: bundle.SpectralHash,
		Files:        []string{domain.SceneFileName, domain.SpectralFileName, domain.TruthFileName, domain.ConfigFileName},
	}
	data, _ := json.MarshalIndent(manifest, "", "  ")
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		return nil, errs.IO("manifest.json", err)
	}
	return bundle, nil
}

func build(seed int64) (*domain.Scene, *domain.SpectralData) {
	wavelengths := make([]float64, 0, 21)
	for w := 400.0; w <= 700.0001; w += 15 {
		wavelengths = append(wavelengths, w)
	}
	cmf := domain.ColorMatching{}
	for _, w := range wavelengths {
		cmf.X = append(cmf.X, ciedeX(w))
		cmf.Y = append(cmf.Y, ciedeY(w))
		cmf.Z = append(cmf.Z, ciedeZ(w))
	}
	white := integrateCMF(wavelengths, cmf)
	gamut := domain.Gamut{
		Name: "sRGB-linear",
		RGBToXYZ: [3][3]float64{
			{0.4124564, 0.3575761, 0.1804375},
			{0.2126729, 0.7151522, 0.0721750},
			{0.0193339, 0.1191920, 0.9503041},
		},
		XYZToRGB: [3][3]float64{
			{3.2404542, -1.5371385, -0.4985314},
			{-0.9692660, 1.8760108, 0.0415560},
			{0.0556434, -0.2040259, 1.0572252},
		},
	}
	basis := []domain.BasisFunction{
		{ID: "broad", Values: sampleValues(wavelengths, func(w float64) float64 {
			return 0.46 + 0.08*math.Cos((w-550)/105)
		})},
		{ID: "blue", Values: sampleGaussian(wavelengths, 455, 43)},
		{ID: "green", Values: sampleGaussian(wavelengths, 555, 57)},
		{ID: "red", Values: sampleGaussian(wavelengths, 645, 51)},
	}
	lights := []domain.Light{
		{ID: "key", Direction: normalize([3]float64{-0.35, 0.55, 0.75}), Intensity: sampleValues(wavelengths, func(w float64) float64 {
			return 1.08 + 0.07*math.Cos((w-530)/90)
		})},
		{ID: "fill", Direction: normalize([3]float64{0.45, 0.20, 0.86}), Intensity: sampleValues(wavelengths, func(w float64) float64 {
			return 0.46 + 0.07*math.Sin((w-430)/75)
		})},
		{ID: "rim", Direction: normalize([3]float64{0.2, -0.65, 0.73}), Intensity: sampleValues(wavelengths, func(w float64) float64 {
			return 0.22 + 0.04*math.Cos((w-620)/65)
		})},
	}
	spectralData := &domain.SpectralData{
		SchemaVersion: domain.SchemaVersion,
		Wavelengths:   wavelengths,
		CMF:           cmf,
		WhitePoint:    white,
		Gamut:         gamut,
		Basis:         basis,
		Lights:        lights,
	}

	specs := []objectSpec{
		{
			meshID: "mesh_vermilion", name: "Vermilion Panel",
			coeffs: []int64{1100, 70, 1420, 760}, baseAlbedo: [3]float64{0.92, 0.25, 0.13},
			roughness: 0.38, metallic: 0.08,
		},
		{
			meshID: "mesh_jade", name: "Jade Wedge",
			coeffs: []int64{980, 1540, 820, 120}, baseAlbedo: [3]float64{0.17, 0.82, 0.36},
			roughness: 0.48, metallic: 0.04,
		},
		{
			meshID: "mesh_cobalt", name: "Cobalt Tile",
			coeffs: []int64{920, 2020, 700, 180}, baseAlbedo: [3]float64{0.14, 0.32, 0.94},
			roughness: 0.31, metallic: 0.12,
		},
		{
			meshID: "mesh_gold", name: "Gold Inlay",
			coeffs: []int64{1230, 280, 1010, 1820}, baseAlbedo: [3]float64{0.95, 0.64, 0.19},
			roughness: 0.23, metallic: 0.72,
		},
	}
	meshes := []domain.Mesh{
		quadMesh(specs[0], 22, 18, 116, 86, 1.0),
		triangleMesh(specs[1], [3][2]float64{{128, 16}, {240, 31}, {151, 95}}, 2.0),
		quadMesh(specs[2], 28, 101, 124, 149, 3.0),
		triangleMesh(specs[3], [3][2]float64{{139, 97}, {242, 111}, {188, 151}}, 4.0),
	}
	objects := make([]domain.ObjectSpectrum, len(specs))
	for i := range specs {
		objects[i] = domain.ObjectSpectrum{
			MeshID:       specs[i].meshID,
			BasisBits:    12,
			BasisSigned:  true,
			Coefficients: append([]int64(nil), specs[i].coeffs...),
		}
	}
	scene := &domain.Scene{
		SchemaVersion: domain.SchemaVersion,
		Seed:          seed,
		Width:         256,
		Height:        160,
		Background:    [3]float64{0.012, 0.016, 0.024},
		Meshes:        meshes,
		Objects:       objects,
	}
	return scene, spectralData
}

func quadMesh(spec objectSpec, x0, y0, x1, y1, z float64) domain.Mesh {
	positions := [][2]float64{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}}
	return meshFromPositions(spec, positions, []int{0, 1, 2, 0, 2, 3}, z)
}

func triangleMesh(spec objectSpec, positions [3][2]float64, z float64) domain.Mesh {
	return meshFromPositions(spec, positions[:], []int{0, 1, 2}, z)
}

func meshFromPositions(spec objectSpec, positions [][2]float64, indices []int, z float64) domain.Mesh {
	vertices := make([]domain.Vertex, len(positions))
	albedoDelta := [][3]float64{
		{-0.04, 0.02, 0.01},
		{0.03, -0.02, 0.02},
		{0.01, 0.03, -0.03},
		{0.02, -0.01, 0.04},
	}
	roughDelta := []float64{-0.035, 0.025, -0.015, 0.045}
	metalDelta := []float64{-0.02, 0.03, 0.01, -0.03}
	normalDelta := [][3]float64{
		{-0.03, -0.02, 0},
		{0.02, -0.04, 0},
		{0.04, 0.03, 0},
		{-0.02, 0.04, 0},
	}
	for i, p := range positions {
		albedo := [3]float64{
			clamp01(spec.baseAlbedo[0] + albedoDelta[i][0]),
			clamp01(spec.baseAlbedo[1] + albedoDelta[i][1]),
			clamp01(spec.baseAlbedo[2] + albedoDelta[i][2]),
		}
		vertices[i] = domain.Vertex{
			Position:  [3]float64{p[0], p[1], z},
			Normal:    normalize([3]float64{normalDelta[i][0], normalDelta[i][1], 1}),
			Albedo:    albedo,
			Roughness: clamp01(spec.roughness + roughDelta[i]),
			Metallic:  clamp01(spec.metallic + metalDelta[i]),
		}
	}
	return domain.Mesh{
		ID:               spec.meshID,
		Name:             spec.name,
		VertexCount:      len(vertices),
		MaterialChannels: 5,
		Vertices:         vertices,
		Indices:          indices,
	}
}

func buildTruth(b *domain.SampleBundle) (*domain.Truth, error) {
	kernel, err := spectral.BuildKernel(b.Spectral, *b.Config)
	if err != nil {
		return nil, err
	}
	materials, err := quant.BuildSet(b.Scene, b.Config.Quantization, quant.NewCache())
	if err != nil {
		return nil, err
	}
	truth := &domain.Truth{
		SceneHash: b.ContentHash,
		Source:    "analytic_generator_v1",
		Tolerance: 1e-6,
	}
	for mi := range b.Scene.Meshes {
		x, y, ok := findInteriorPixel(b.Scene, &b.Scene.Meshes[mi])
		if !ok {
			return nil, errs.New(errs.CodeVerify, "generator cannot find an interior pixel for mesh %s", b.Scene.Meshes[mi].ID)
		}
		hit, ok := geom.Trace(b.Scene, float64(x)+0.5, float64(y)+0.5)
		if !ok || hit.ObjectID != b.Scene.Meshes[mi].ID {
			return nil, errs.New(errs.CodeVerify, "generator truth trace missed mesh %s", b.Scene.Meshes[mi].ID)
		}
		mat := materials.Objects[hit.ObjectID]
		attrs := shading.Interpolate(mat, hit.Indices, hit.Bary)
		xyz, peak := shading.Shade(attrs, mat, kernel, *b.Config)
		rgb := shading.LinearRGB(xyz, kernel)
		normalized := spectral.XYZ{
			X: xyz.X / b.Spectral.WhitePoint[0],
			Y: xyz.Y / b.Spectral.WhitePoint[1],
			Z: xyz.Z / b.Spectral.WhitePoint[2],
		}
		chroma := spectral.Chromaticity(normalized)
		truth.Points = append(truth.Points, domain.TruthPoint{
			X:                    x,
			Y:                    y,
			ObjectID:             hit.ObjectID,
			ExpectedLinearRGB:    rgb,
			ExpectedPeakRadiance: peak,
			ExpectedChromaticity: chroma,
		})
		if peak > truth.PeakRadiance {
			truth.PeakRadiance = peak
		}
	}
	return truth, nil
}

func findInteriorPixel(scene *domain.Scene, mesh *domain.Mesh) (int, int, bool) {
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, v := range mesh.Vertices {
		minX = math.Min(minX, v.Position[0])
		minY = math.Min(minY, v.Position[1])
		maxX = math.Max(maxX, v.Position[0])
		maxY = math.Max(maxY, v.Position[1])
	}
	cx, cy := int((minX+maxX)/2), int((minY+maxY)/2)
	for radius := 0; radius < max(scene.Width, scene.Height); radius++ {
		for dy := -radius; dy <= radius; dy++ {
			for dx := -radius; dx <= radius; dx++ {
				x, y := cx+dx, cy+dy
				if x < 0 || y < 0 || x >= scene.Width || y >= scene.Height {
					continue
				}
				hit, ok := geom.Trace(scene, float64(x)+0.5, float64(y)+0.5)
				if ok && hit.ObjectID == mesh.ID {
					return x, y, true
				}
			}
		}
	}
	return 0, 0, false
}

func integrateCMF(wavelengths []float64, cmf domain.ColorMatching) [3]float64 {
	var out [3]float64
	for i := 0; i < len(wavelengths); i++ {
		w := 0.0
		if i > 0 {
			w += (wavelengths[i] - wavelengths[i-1]) / 2
		}
		if i+1 < len(wavelengths) {
			w += (wavelengths[i+1] - wavelengths[i]) / 2
		}
		out[0] += w * cmf.X[i]
		out[1] += w * cmf.Y[i]
		out[2] += w * cmf.Z[i]
	}
	return out
}

func sampleValues(wavelengths []float64, f func(float64) float64) []float64 {
	out := make([]float64, len(wavelengths))
	for i, w := range wavelengths {
		out[i] = clamp01(f(w))
	}
	return out
}

func sampleGaussian(wavelengths []float64, mean, sigma float64) []float64 {
	return sampleValues(wavelengths, func(w float64) float64 {
		return math.Exp(-0.5 * math.Pow((w-mean)/sigma, 2))
	})
}

func ciedeX(w float64) float64 {
	return clamp01(1.056*gauss(w, 599.8, 37.9) + 0.362*gauss(w, 442.0, 16.0) - 0.065*gauss(w, 501.1, 20.4))
}

func ciedeY(w float64) float64 {
	return clamp01(0.821*gauss(w, 568.8, 46.9) + 0.286*gauss(w, 530.9, 16.3))
}

func ciedeZ(w float64) float64 {
	return clamp01(1.217*gauss(w, 437.0, 11.8) + 0.681*gauss(w, 459.0, 26.0))
}

func gauss(x, mean, sigma float64) float64 {
	return math.Exp(-0.5 * math.Pow((x-mean)/sigma, 2))
}

func normalize(v [3]float64) [3]float64 {
	n := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
	return [3]float64{v[0] / n, v[1] / n, v[2] / n}
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func Summary(b *domain.SampleBundle) string {
	return fmt.Sprintf("%dx%d, %d meshes, %d truth points, content=%s",
		b.Scene.Width, b.Scene.Height, len(b.Scene.Meshes), len(b.Truth.Points), b.ContentHash[:12])
}
