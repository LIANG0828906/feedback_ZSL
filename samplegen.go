package spectralrender

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

func GenerateSamples(outDir string, seed int64) (*SampleManifest, error) {
	scene, err := BuildDeterministicScene(seed)
	if err != nil {
		return nil, err
	}
	sceneHash := HashScene(scene)
	referenceParams := DefaultRenderParams()
	referenceParams.Version = 1
	referenceParams.SampleCount = len(scene.Spectrum.Wavelengths)
	referenceParams.Integration = IntegrationTrapezoid
	referenceParams.MaxParallel = 4
	referenceParams.MemoryBudgetBytes = 64 << 20
	referenceFrame, _, rerr := Render(scene, sceneHash, referenceParams, RenderOptions{})
	if rerr != nil {
		return nil, fmt.Errorf("reference render failed: %w", rerr)
	}

	defaultParams := DefaultRenderParams()
	defaultParams.Version = referenceParams.Version
	defaultFrame, _, derr := Render(scene, sceneHash, defaultParams, RenderOptions{})
	if derr != nil {
		return nil, fmt.Errorf("default render failed: %w", derr)
	}
	comparisonFrames := []*Frame{defaultFrame}
	for _, method := range []IntegrationMethod{IntegrationRectangle, IntegrationSimpson} {
		p := defaultParams
		p.Integration = method
		frame, _, ferr := Render(scene, sceneHash, p, RenderOptions{})
		if ferr != nil {
			return nil, fmt.Errorf("%s render failed: %w", method, ferr)
		}
		comparisonFrames = append(comparisonFrames, frame)
	}

	truthPixels := [][2]int{{16, 32}, {48, 32}, {79, 32}, {12, 12}, {48, 20}, {84, 50}}
	truth := &TruthManifest{
		SchemaVersion: truthSchema,
		SceneHash:     sceneHash,
		Reference: TruthReference{
			SampleCount:    referenceParams.SampleCount,
			Integration:    string(referenceParams.Integration),
			Bounces:        referenceParams.Bounces,
			IndirectWeight: referenceParams.IndirectWeight,
		},
	}
	maxTolerance := 5e-4
	maxPeakTolerance := 5e-4
	maxChromaBound := 2e-3
	for _, coord := range truthPixels {
		x, y := coord[0], coord[1]
		idx := y*referenceFrame.Width + x
		expected := RGB{
			referenceFrame.LinearRGB[idx*3],
			referenceFrame.LinearRGB[idx*3+1],
			referenceFrame.LinearRGB[idx*3+2],
		}
		xyz := RGB{
			referenceFrame.XYZ[idx*3],
			referenceFrame.XYZ[idx*3+1],
			referenceFrame.XYZ[idx*3+2],
		}
		xy := chromaticity([3]float64{xyz[0], xyz[1], xyz[2]})
		rgbErr := 0.0
		peakErr := 0.0
		chromaErr := 0.0
		for _, frame := range comparisonFrames {
			got := RGB{
				frame.LinearRGB[idx*3],
				frame.LinearRGB[idx*3+1],
				frame.LinearRGB[idx*3+2],
			}
			if e := maxAbsDiff(expected, got); e > rgbErr {
				rgbErr = e
			}
			if e := math.Abs(referenceFrame.PeakRadiance[idx] - frame.PeakRadiance[idx]); e > peakErr {
				peakErr = e
			}
			gotXYZ := RGB{
				frame.XYZ[idx*3],
				frame.XYZ[idx*3+1],
				frame.XYZ[idx*3+2],
			}
			gotXY := chromaticity([3]float64{gotXYZ[0], gotXYZ[1], gotXYZ[2]})
			if e := math.Hypot(xy[0]-gotXY[0], xy[1]-gotXY[1]); e > chromaErr {
				chromaErr = e
			}
		}
		tol := math.Max(5e-5, 2*rgbErr+1e-6)
		peak := referenceFrame.PeakRadiance[idx]
		peakTol := math.Max(5e-5, 2*peakErr+1e-6)
		chromaBound := math.Max(5e-4, 2*chromaErr+1e-6)
		if tol > maxTolerance {
			maxTolerance = tol
		}
		if peakTol > maxPeakTolerance {
			maxPeakTolerance = peakTol
		}
		if chromaBound > maxChromaBound {
			maxChromaBound = chromaBound
		}
		truth.Pixels = append(truth.Pixels, TruthPixel{
			X: x, Y: y,
			LinearRGB:         expected,
			Tolerance:         tol,
			PeakRadiance:      peak,
			PeakTolerance:     peakTol,
			ChromaticityXY:    xy,
			ChromaticityBound: chromaBound,
		})
	}
	truth.Bounds = TruthBounds{
		LinearRGBAbs:    maxTolerance,
		PeakRadianceRel: maxPeakTolerance,
		ChromaticityXY:  maxChromaBound,
	}
	sceneBytes, err := json.MarshalIndent(scene, "", "  ")
	if err != nil {
		return nil, err
	}
	sceneBytes = append(sceneBytes, '\n')
	cmfDoc := CMFDocument{
		SchemaVersion: 1,
		StartNM:       scene.Spectrum.StartNM,
		EndNM:         scene.Spectrum.EndNM,
		StepNM:        scene.Spectrum.StepNM,
		Wavelengths:   scene.Spectrum.Wavelengths,
		CMF:           scene.Spectrum.CMF,
		WhitePoint:    scene.Spectrum.WhitePoint,
		TargetGamut:   scene.Spectrum.TargetGamut,
	}
	cmfBytes, err := json.MarshalIndent(cmfDoc, "", "  ")
	if err != nil {
		return nil, err
	}
	cmfBytes = append(cmfBytes, '\n')
	truthBytes, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, err
	}
	truthBytes = append(truthBytes, '\n')
	manifest := &SampleManifest{
		SchemaVersion: 1,
		Generator:     "spectralrender-samplegen",
		Seed:          seed,
		SceneFile:     "scene.json",
		CMFFile:       "cmf.json",
		TruthFile:     "truth.json",
		SceneHash:     sceneHash,
		CMFHash:       sha256HexBytes(cmfBytes),
		TruthHash:     sha256HexBytes(truthBytes),
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	outputs := []struct {
		name string
		data []byte
	}{
		{"scene.json", sceneBytes},
		{"cmf.json", cmfBytes},
		{"truth.json", truthBytes},
		{"manifest.json", manifestBytes},
	}
	for _, out := range outputs {
		path := filepath.Join(outDir, out.name)
		if err := os.WriteFile(path, out.data, 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
	}
	return manifest, nil
}

func BuildDeterministicScene(seed int64) (*Scene, error) {
	const width, height = 96, 64
	const wlStart, wlEnd = 360.0, 830.0
	wavelengths := make([]float64, 0, int(wlEnd-wlStart)+1)
	cmfX := make([]float64, 0, cap(wavelengths))
	cmfY := make([]float64, 0, cap(wavelengths))
	cmfZ := make([]float64, 0, cap(wavelengths))
	for wl := wlStart; wl <= wlEnd+1e-9; wl += 1 {
		wavelengths = append(wavelengths, wl)
		x, y, z := cie1931Approx(wl)
		cmfX = append(cmfX, x)
		cmfY = append(cmfY, y)
		cmfZ = append(cmfZ, z)
	}
	white := [3]float64{0.95047, 1.0, 1.08883}
	baseMatrix := [3][3]float64{
		{3.2406, -1.5372, -0.4986},
		{-0.9689, 1.8758, 0.0415},
		{0.0557, -0.2040, 1.0570},
	}
	var matrix [3][3]float64
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			matrix[r][c] = baseMatrix[r][c] * white[c]
		}
	}
	basis450 := gaussianTable(wavelengths, 450, 34)
	basis540 := gaussianTable(wavelengths, 540, 43)
	basis630 := gaussianTable(wavelengths, 630, 57)
	coefficients := [][3]int64{
		{1450, 1020, 760},
		{860, 1780, 1020},
		{720, 980, 1930},
	}
	materials := make([]Material, 0, 3)
	for i := range coefficients {
		coeffs := make([]int64, 3)
		for j := 0; j < 3; j++ {
			coeffs[j] = coefficients[i][j]
			coeffs[j] += (seed % 7) * int64(i+1) * int64(j+1)
		}
		materials = append(materials, Material{
			ID: fmt.Sprintf("mat-%d", i),
			Basis: []BasisFunction{
				{Name: "basis-450", Values: basis450},
				{Name: "basis-540", Values: basis540},
				{Name: "basis-630", Values: basis630},
			},
			Coefficients:    coeffs,
			CoefficientBits: 12,
			CoefficientUnit: 4095,
		})
	}

	lights := []Light{
		{ID: "key", Direction: [3]float64{-0.45, 0.55, 0.80}, Intensity: lightTable(wavelengths, 0.0068, 505, 0.70, 655, 0.35)},
		{ID: "fill", Direction: [3]float64{0.62, -0.18, 0.72}, Intensity: lightTable(wavelengths, 0.0044, 470, 0.45, 590, 0.30)},
		{ID: "rim", Direction: [3]float64{0.14, 0.82, 0.42}, Intensity: lightTable(wavelengths, 0.0028, 575, 0.62, 700, 0.18)},
	}
	scene := &Scene{
		SchemaVersion: sceneSchema,
		Seed:          seed,
		Width:         width, Height: height,
		Quantization: Quantization{
			VertexBits: 10, VertexUnit: 1023,
			CoefficientBits: 12, CoefficientUnit: 4095,
		},
		Spectrum: Spectrum{
			StartNM: wlStart, EndNM: wlEnd, StepNM: 1,
			Wavelengths: wavelengths,
			CMF:         CMF{X: cmfX, Y: cmfY, Z: cmfZ},
			WhitePoint:  white,
			XYZToRGB:    matrix,
			TargetGamut: "linear-srgb-rec709-d65",
		},
		Materials:     materials,
		Lights:        lights,
		BackgroundRGB: RGB{0.004, 0.006, 0.010},
	}

	meshSpecs := []struct {
		id       string
		material string
		verts    [][2]float64
		normals  [][3]float64
		albedo   [][3]int64
		rough    []int64
		metal    []int64
		tris     [][3]int
		depth    float64
	}{
		{
			id: "panel-left", material: "mat-0",
			verts:   [][2]float64{{-0.90, -0.72}, {-0.38, -0.72}, {-0.38, 0.72}, {-0.90, 0.72}},
			normals: [][3]float64{{-0.12, 0, 1}, {0.10, 0.04, 1}, {0.08, 0.10, 1}, {-0.10, 0.06, 1}},
			albedo:  [][3]int64{{900, 620, 520}, {830, 560, 470}, {960, 680, 560}, {880, 590, 500}},
			rough:   []int64{520, 470, 430, 500},
			metal:   []int64{90, 120, 150, 110},
			tris:    [][3]int{{0, 1, 2}, {0, 2, 3}},
			depth:   0.15,
		},
		{
			id: "shard-center", material: "mat-1",
			verts:   [][2]float64{{-0.18, -0.76}, {0.18, -0.76}, {0.0, 0.80}},
			normals: [][3]float64{{-0.28, -0.10, 1}, {0.30, -0.08, 1}, {0.02, 0.32, 1}},
			albedo:  [][3]int64{{420, 850, 560}, {520, 930, 620}, {380, 800, 520}},
			rough:   []int64{380, 410, 350},
			metal:   []int64{180, 210, 160},
			tris:    [][3]int{{0, 1, 2}},
			depth:   0.05,
		},
		{
			id: "panel-right", material: "mat-2",
			verts:   [][2]float64{{0.38, -0.72}, {0.90, -0.72}, {0.90, 0.72}, {0.38, 0.72}},
			normals: [][3]float64{{-0.08, -0.04, 1}, {0.12, 0.02, 1}, {0.10, 0.12, 1}, {-0.06, 0.10, 1}},
			albedo:  [][3]int64{{650, 650, 950}, {700, 680, 980}, {610, 620, 920}, {680, 660, 940}},
			rough:   []int64{620, 570, 530, 590},
			metal:   []int64{250, 280, 300, 260},
			tris:    [][3]int{{0, 1, 2}, {0, 2, 3}},
			depth:   0.20,
		},
	}
	for _, spec := range meshSpecs {
		mesh := Mesh{ID: spec.id, MaterialID: spec.material}
		for i := range spec.verts {
			mesh.Vertices = append(mesh.Vertices, Vertex{
				Position:  [3]float64{spec.verts[i][0], spec.verts[i][1], spec.depth},
				Normal:    spec.normals[i],
				Albedo:    spec.albedo[i],
				Roughness: spec.rough[i],
				Metallic:  spec.metal[i],
			})
		}
		mesh.Triangles = spec.tris
		scene.Meshes = append(scene.Meshes, mesh)
	}
	return scene, nil
}

func cie1931Approx(wl float64) (float64, float64, float64) {
	g := func(mu, sigma float64) float64 {
		d := (wl - mu) / sigma
		return math.Exp(-0.5 * d * d)
	}
	x := 1.056*g(599.8, 37.9) + 0.362*g(442.0, 16.0) - 0.065*g(501.1, 20.4)
	y := 0.821*g(568.8, 46.9) + 0.286*g(530.9, 16.3)
	z := 1.217*g(437.0, 11.8) + 0.681*g(459.0, 26.0)
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if z < 0 {
		z = 0
	}
	return x, y, z
}

func gaussianTable(wavelengths []float64, center, sigma float64) []float64 {
	out := make([]float64, len(wavelengths))
	for i, wl := range wavelengths {
		d := (wl - center) / sigma
		out[i] = math.Exp(-0.5 * d * d)
	}
	return out
}

func lightTable(wavelengths []float64, base, center1, weight1, center2, weight2 float64) []float64 {
	out := make([]float64, len(wavelengths))
	for i, wl := range wavelengths {
		d1 := (wl - center1) / 110
		d2 := (wl - center2) / 95
		out[i] = base * (0.58 + weight1*math.Exp(-0.5*d1*d1) + weight2*math.Exp(-0.5*d2*d2))
	}
	return out
}
