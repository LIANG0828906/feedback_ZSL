package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"spectralforge/internal/errs"
)

const (
	SceneFileName    = "scene.json"
	SpectralFileName = "spectral.json"
	TruthFileName    = "truth.json"
	ConfigFileName   = "render-config.json"
)

func LoadBundle(dir string) (*SampleBundle, error) {
	sceneBytes, err := readFile(filepath.Join(dir, SceneFileName), "scene")
	if err != nil {
		return nil, err
	}
	spectralBytes, err := readFile(filepath.Join(dir, SpectralFileName), "spectral")
	if err != nil {
		return nil, err
	}
	truthBytes, err := readFile(filepath.Join(dir, TruthFileName), "truth")
	if err != nil {
		return nil, err
	}
	configBytes, err := readFile(filepath.Join(dir, ConfigFileName), "render_config")
	if err != nil {
		return nil, err
	}

	scene, err := DecodeScene(sceneBytes)
	if err != nil {
		return nil, err
	}
	spectral, err := DecodeSpectral(spectralBytes)
	if err != nil {
		return nil, err
	}
	truth, err := DecodeTruth(truthBytes)
	if err != nil {
		return nil, err
	}
	config, err := DecodeConfig(configBytes)
	if err != nil {
		return nil, err
	}

	bundle := &SampleBundle{
		Scene:    scene,
		Spectral: spectral,
		Truth:    truth,
		Config:   config,
	}
	if err := bundle.FinishHashes(); err != nil {
		return nil, errs.Decode("sample", "cannot hash sample: %v", err)
	}
	if err := ValidateBundle(bundle); err != nil {
		return nil, err
	}
	return bundle, nil
}

func readFile(path, field string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errs.Decode(field, "cannot read %s: %v", path, err)
	}
	return data, nil
}

func DecodeScene(data []byte) (*Scene, error) {
	raw, err := decodeObject(data, "scene")
	if err != nil {
		return nil, err
	}
	if err := requireFields(raw, "scene", "schema_version", "seed", "width", "height", "background", "meshes", "objects"); err != nil {
		return nil, err
	}
	if err := requireFixedArray(raw["background"], "scene.background", 3); err != nil {
		return nil, err
	}
	meshes, err := objectArray(raw["meshes"], "scene.meshes")
	if err != nil {
		return nil, err
	}
	for i, mesh := range meshes {
		path := fmt.Sprintf("scene.meshes[%d]", i)
		if err := requireFields(mesh, path, "id", "name", "vertex_count", "material_channels", "vertices", "indices"); err != nil {
			return nil, err
		}
		vertices, err := objectArray(mesh["vertices"], path+".vertices")
		if err != nil {
			return nil, err
		}
		for j, vertex := range vertices {
			vpath := fmt.Sprintf("%s.vertices[%d]", path, j)
			if err := requireFields(vertex, vpath, "position", "normal", "albedo", "roughness", "metallic"); err != nil {
				return nil, err
			}
			if err := requireFixedArray(vertex["position"], vpath+".position", 3); err != nil {
				return nil, err
			}
			if err := requireFixedArray(vertex["normal"], vpath+".normal", 3); err != nil {
				return nil, err
			}
			if err := requireFixedArray(vertex["albedo"], vpath+".albedo", 3); err != nil {
				return nil, err
			}
		}
	}
	objects, err := objectArray(raw["objects"], "scene.objects")
	if err != nil {
		return nil, err
	}
	for i, obj := range objects {
		path := fmt.Sprintf("scene.objects[%d]", i)
		if err := requireFields(obj, path, "mesh_id", "basis_bits", "basis_signed", "coefficients"); err != nil {
			return nil, err
		}
	}

	var scene Scene
	if err := json.Unmarshal(data, &scene); err != nil {
		return nil, errs.Decode("scene", "invalid scene JSON: %v", err)
	}
	if err := ValidateScene(&scene); err != nil {
		return nil, err
	}
	return &scene, nil
}

func DecodeSpectral(data []byte) (*SpectralData, error) {
	raw, err := decodeObject(data, "spectral")
	if err != nil {
		return nil, err
	}
	if err := requireFields(raw, "spectral", "schema_version", "wavelengths", "cmf", "white_point", "gamut", "basis", "lights"); err != nil {
		return nil, err
	}
	if err := requireFixedArray(raw["white_point"], "spectral.white_point", 3); err != nil {
		return nil, err
	}
	cmf, err := decodeObject(raw["cmf"], "spectral.cmf")
	if err != nil {
		return nil, err
	}
	if err := requireFields(cmf, "spectral.cmf", "x", "y", "z"); err != nil {
		return nil, err
	}
	gamut, err := decodeObject(raw["gamut"], "spectral.gamut")
	if err != nil {
		return nil, err
	}
	if err := requireFields(gamut, "spectral.gamut", "name", "rgb_to_xyz", "xyz_to_rgb"); err != nil {
		return nil, err
	}
	if err := requireMatrix(gamut["rgb_to_xyz"], "spectral.gamut.rgb_to_xyz"); err != nil {
		return nil, err
	}
	if err := requireMatrix(gamut["xyz_to_rgb"], "spectral.gamut.xyz_to_rgb"); err != nil {
		return nil, err
	}
	basis, err := objectArray(raw["basis"], "spectral.basis")
	if err != nil {
		return nil, err
	}
	for i, b := range basis {
		path := fmt.Sprintf("spectral.basis[%d]", i)
		if err := requireFields(b, path, "id", "values"); err != nil {
			return nil, err
		}
	}
	lights, err := objectArray(raw["lights"], "spectral.lights")
	if err != nil {
		return nil, err
	}
	for i, light := range lights {
		path := fmt.Sprintf("spectral.lights[%d]", i)
		if err := requireFields(light, path, "id", "direction", "intensity"); err != nil {
			return nil, err
		}
		if err := requireFixedArray(light["direction"], path+".direction", 3); err != nil {
			return nil, err
		}
	}

	var spectral SpectralData
	if err := json.Unmarshal(data, &spectral); err != nil {
		return nil, errs.Decode("spectral", "invalid spectral JSON: %v", err)
	}
	if err := ValidateSpectral(&spectral); err != nil {
		return nil, err
	}
	return &spectral, nil
}

func DecodeTruth(data []byte) (*Truth, error) {
	raw, err := decodeObject(data, "truth")
	if err != nil {
		return nil, err
	}
	if err := requireFields(raw, "truth", "scene_hash", "source", "tolerance", "points", "peak_radiance"); err != nil {
		return nil, err
	}
	points, err := objectArray(raw["points"], "truth.points")
	if err != nil {
		return nil, err
	}
	for i, point := range points {
		path := fmt.Sprintf("truth.points[%d]", i)
		if err := requireFields(point, path, "x", "y", "object_id", "expected_linear_rgb", "expected_peak_radiance", "expected_chromaticity"); err != nil {
			return nil, err
		}
		if err := requireFixedArray(point["expected_linear_rgb"], path+".expected_linear_rgb", 3); err != nil {
			return nil, err
		}
		if err := requireFixedArray(point["expected_chromaticity"], path+".expected_chromaticity", 2); err != nil {
			return nil, err
		}
	}
	var truth Truth
	if err := json.Unmarshal(data, &truth); err != nil {
		return nil, errs.Decode("truth", "invalid truth JSON: %v", err)
	}
	if err := ValidateTruth(&truth); err != nil {
		return nil, err
	}
	return &truth, nil
}

func DecodeConfig(data []byte) (*RenderConfig, error) {
	raw, err := decodeObject(data, "render_config")
	if err != nil {
		return nil, err
	}
	if err := requireFields(raw, "render_config",
		"wavelength_steps", "integration", "tile_size", "max_workers",
		"per_thread_pixel_buffer", "queue_budget", "indirect_bounces",
		"exposure", "tone_curve", "gamma", "seam_threshold",
		"quantization", "preview_tolerance", "color_tolerance", "max_retries"); err != nil {
		return nil, err
	}
	quant, err := decodeObject(raw["quantization"], "render_config.quantization")
	if err != nil {
		return nil, err
	}
	if err := requireFields(quant, "render_config.quantization", "albedo_bits", "roughness_bits", "metallic_bits", "basis_bits"); err != nil {
		return nil, err
	}
	var config RenderConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, errs.Decode("render_config", "invalid render config JSON: %v", err)
	}
	if err := ValidateConfig(&config); err != nil {
		return nil, err
	}
	return &config, nil
}

func ValidateBundle(b *SampleBundle) error {
	if b.Scene == nil || b.Spectral == nil || b.Truth == nil || b.Config == nil {
		return errs.Schema("sample", "scene, spectral, truth and render config are required")
	}
	basisCount := len(b.Spectral.Basis)
	for i := range b.Scene.Objects {
		obj := &b.Scene.Objects[i]
		if len(obj.Coefficients) != basisCount {
			return errs.Para(fmt.Sprintf("scene.objects[%d].coefficients", i),
				fmt.Sprintf("[%d,%d]", basisCount, basisCount),
				"coefficient count %d does not match basis count %d", len(obj.Coefficients), basisCount)
		}
	}
	if b.Truth.SceneHash != b.ContentHash {
		return errs.Schema("truth.scene_hash", "truth scene hash does not match generated scene content")
	}
	if len(b.Truth.Points) == 0 {
		return errs.Schema("truth.points", "at least one analytic truth point is required")
	}
	for i, p := range b.Truth.Points {
		if p.X < 0 || p.X >= b.Scene.Width || p.Y < 0 || p.Y >= b.Scene.Height {
			return errs.Para(fmt.Sprintf("truth.points[%d]", i),
				fmt.Sprintf("[0,%d]x[0,%d]", b.Scene.Width-1, b.Scene.Height-1),
				"truth pixel (%d,%d) is outside the screen", p.X, p.Y)
		}
		if !objectExists(b.Scene, p.ObjectID) {
			return errs.Schema(fmt.Sprintf("truth.points[%d].object_id", i), "unknown mesh id %q", p.ObjectID)
		}
	}
	return nil
}

func ValidateScene(s *Scene) error {
	if s.SchemaVersion != SchemaVersion {
		return errs.Para("scene.schema_version", fmt.Sprintf("[%d,%d]", SchemaVersion, SchemaVersion),
			"unsupported schema version %d", s.SchemaVersion)
	}
	if s.Width < 1 || s.Width > 16384 {
		return errs.Para("scene.width", "[1,16384]", "width %d is out of range", s.Width)
	}
	if s.Height < 1 || s.Height > 16384 {
		return errs.Para("scene.height", "[1,16384]", "height %d is out of range", s.Height)
	}
	if !finite3(s.Background) {
		return errs.Para("scene.background", "finite numbers", "background contains a non-finite value")
	}
	if len(s.Meshes) == 0 {
		return errs.Schema("scene.meshes", "at least one mesh is required")
	}
	meshIDs := make(map[string]bool, len(s.Meshes))
	for i := range s.Meshes {
		m := &s.Meshes[i]
		path := fmt.Sprintf("scene.meshes[%d]", i)
		if strings.TrimSpace(m.ID) == "" {
			return errs.Schema(path+".id", "mesh id is required")
		}
		if meshIDs[m.ID] {
			return errs.Schema(path+".id", "duplicate mesh id %q", m.ID)
		}
		meshIDs[m.ID] = true
		if m.VertexCount != len(m.Vertices) {
			return errs.Para(path+".vertex_count", fmt.Sprintf("[%d,%d]", len(m.Vertices), len(m.Vertices)),
				"declared vertex count %d does not match %d vertices", m.VertexCount, len(m.Vertices))
		}
		if m.MaterialChannels != 5 {
			return errs.Para(path+".material_channels", "[5,5]",
				"material channels %d cannot represent albedo(3)+roughness(1)+metallic(1)", m.MaterialChannels)
		}
		for j := range m.Vertices {
			v := &m.Vertices[j]
			vpath := fmt.Sprintf("%s.vertices[%d]", path, j)
			if !finite3(v.Position) {
				return errs.Para(vpath+".position", "finite numbers", "position contains a non-finite value")
			}
			if !finite3(v.Normal) || norm3(v.Normal) < 1e-9 {
				return errs.Para(vpath+".normal", "finite, length>0", "normal must be finite and non-zero")
			}
			for c := 0; c < 3; c++ {
				if v.Albedo[c] < 0 || v.Albedo[c] > 1 || math.IsNaN(v.Albedo[c]) {
					return errs.Para(fmt.Sprintf("%s.albedo[%d]", vpath, c), "[0,1]",
						"albedo[%d]=%g is outside [0,1]", c, v.Albedo[c])
				}
			}
			if err := unitRange(v.Roughness, vpath+".roughness"); err != nil {
				return err
			}
			if err := unitRange(v.Metallic, vpath+".metallic"); err != nil {
				return err
			}
		}
		if len(m.Indices) == 0 || len(m.Indices)%3 != 0 {
			return errs.Para(path+".indices", "[3,3N]", "triangle index count %d is not a positive multiple of 3", len(m.Indices))
		}
		for j, index := range m.Indices {
			if index < 0 || index >= len(m.Vertices) {
				return errs.Para(fmt.Sprintf("%s.indices[%d]", path, j),
					fmt.Sprintf("[0,%d]", len(m.Vertices)-1),
					"vertex index %d is out of range", index)
			}
		}
	}
	objectMeshIDs := make(map[string]bool, len(s.Objects))
	for i := range s.Objects {
		o := &s.Objects[i]
		path := fmt.Sprintf("scene.objects[%d]", i)
		if !meshIDs[o.MeshID] {
			return errs.Schema(path+".mesh_id", "mesh_id %q does not reference a mesh", o.MeshID)
		}
		if objectMeshIDs[o.MeshID] {
			return errs.Schema(path+".mesh_id", "duplicate spectral object for mesh %q", o.MeshID)
		}
		objectMeshIDs[o.MeshID] = true
		if o.BasisBits < 2 || o.BasisBits > 32 {
			return errs.Para(path+".basis_bits", "[2,32]", "basis bit depth %d is out of range", o.BasisBits)
		}
		min, max := coefficientBounds(o.BasisBits, o.BasisSigned)
		for j, coeff := range o.Coefficients {
			if coeff < min || coeff > max {
				return errs.Para(fmt.Sprintf("%s.coefficients[%d]", path, j),
					fmt.Sprintf("[%d,%d]", min, max),
					"coefficient %d exceeds declared %d-bit range", coeff, o.BasisBits)
			}
		}
	}
	for meshID := range meshIDs {
		if !objectMeshIDs[meshID] {
			return errs.Schema("scene.objects", "mesh %q has no spectral object", meshID)
		}
	}
	return nil
}

func ValidateSpectral(s *SpectralData) error {
	if s.SchemaVersion != SchemaVersion {
		return errs.Para("spectral.schema_version", fmt.Sprintf("[%d,%d]", SchemaVersion, SchemaVersion),
			"unsupported schema version %d", s.SchemaVersion)
	}
	n := len(s.Wavelengths)
	if n < 2 {
		return errs.Schema("spectral.wavelengths", "at least two wavelength samples are required")
	}
	if len(s.CMF.X) != n || len(s.CMF.Y) != n || len(s.CMF.Z) != n {
		return errs.Para("spectral.cmf", fmt.Sprintf("[%d,%d]", n, n),
			"color matching table must align point-by-point with %d wavelengths", n)
	}
	for i := range s.Wavelengths {
		w := s.Wavelengths[i]
		if math.IsNaN(w) || math.IsInf(w, 0) {
			return errs.Para(fmt.Sprintf("spectral.wavelengths[%d]", i), "finite numbers", "wavelength is not finite")
		}
		if i > 0 && w <= s.Wavelengths[i-1] {
			return errs.Para(fmt.Sprintf("spectral.wavelengths[%d]", i),
				fmt.Sprintf("(%.12g, +inf)", s.Wavelengths[i-1]),
				"wavelengths must be strictly increasing")
		}
		if s.CMF.X[i] < 0 || s.CMF.Y[i] < 0 || s.CMF.Z[i] < 0 {
			return errs.Para(fmt.Sprintf("spectral.cmf[%d]", i), "[0,+inf)", "color matching functions must be non-negative")
		}
	}
	for i := 0; i < 3; i++ {
		if s.WhitePoint[i] <= 0 || math.IsNaN(s.WhitePoint[i]) || math.IsInf(s.WhitePoint[i], 0) {
			return errs.Para(fmt.Sprintf("spectral.white_point[%d]", i), "(0,+inf)", "white point component must be positive and finite")
		}
	}
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			if math.IsNaN(s.Gamut.RGBToXYZ[r][c]) || math.IsInf(s.Gamut.RGBToXYZ[r][c], 0) ||
				math.IsNaN(s.Gamut.XYZToRGB[r][c]) || math.IsInf(s.Gamut.XYZToRGB[r][c], 0) {
				return errs.Para("spectral.gamut", "finite numbers", "gamut matrix contains a non-finite value")
			}
		}
	}
	if err := checkInverse(s.Gamut, 2e-4); err != nil {
		return err
	}
	if len(s.Basis) == 0 {
		return errs.Schema("spectral.basis", "at least one basis function is required")
	}
	ids := make(map[string]bool, len(s.Basis))
	for i := range s.Basis {
		b := &s.Basis[i]
		path := fmt.Sprintf("spectral.basis[%d]", i)
		if strings.TrimSpace(b.ID) == "" {
			return errs.Schema(path+".id", "basis id is required")
		}
		if ids[b.ID] {
			return errs.Schema(path+".id", "duplicate basis id %q", b.ID)
		}
		ids[b.ID] = true
		if len(b.Values) != n {
			return errs.Para(path+".values", fmt.Sprintf("[%d,%d]", n, n),
				"basis sample count %d does not match wavelength count %d", len(b.Values), n)
		}
		for j, v := range b.Values {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
				return errs.Para(fmt.Sprintf("%s.values[%d]", path, j), "[0,1]",
					"basis value %g is outside [0,1]", v)
			}
		}
	}
	if len(s.Lights) == 0 {
		return errs.Schema("spectral.lights", "at least one light is required")
	}
	lightIDs := make(map[string]bool, len(s.Lights))
	for i := range s.Lights {
		l := &s.Lights[i]
		path := fmt.Sprintf("spectral.lights[%d]", i)
		if strings.TrimSpace(l.ID) == "" {
			return errs.Schema(path+".id", "light id is required")
		}
		if lightIDs[l.ID] {
			return errs.Schema(path+".id", "duplicate light id %q", l.ID)
		}
		lightIDs[l.ID] = true
		if !finite3(l.Direction) || norm3(l.Direction) < 1e-9 {
			return errs.Para(path+".direction", "finite, length>0", "light direction must be finite and non-zero")
		}
		if len(l.Intensity) != n {
			return errs.Para(path+".intensity", fmt.Sprintf("[%d,%d]", n, n),
				"light intensity count %d does not match wavelength count %d", len(l.Intensity), n)
		}
		for j, v := range l.Intensity {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				return errs.Para(fmt.Sprintf("%s.intensity[%d]", path, j), "[0,+inf)",
					"light intensity must be non-negative and finite")
			}
		}
	}
	return nil
}

func ValidateTruth(t *Truth) error {
	if strings.TrimSpace(t.SceneHash) == "" {
		return errs.Schema("truth.scene_hash", "scene hash is required")
	}
	if t.Source != "analytic_generator_v1" {
		return errs.Schema("truth.source", "truth must originate from analytic_generator_v1")
	}
	if t.Tolerance <= 0 || t.Tolerance > 1 {
		return errs.Para("truth.tolerance", "(0,1]", "truth tolerance must be in (0,1]")
	}
	if t.PeakRadiance < 0 || math.IsNaN(t.PeakRadiance) || math.IsInf(t.PeakRadiance, 0) {
		return errs.Para("truth.peak_radiance", "[0,+inf)", "peak radiance must be finite and non-negative")
	}
	for i, p := range t.Points {
		path := fmt.Sprintf("truth.points[%d]", i)
		if p.X < 0 || p.Y < 0 {
			return errs.Para(path, "[0,+inf)x[0,+inf)", "pixel coordinates must be non-negative")
		}
		if !finite3(p.ExpectedLinearRGB) {
			return errs.Para(path+".expected_linear_rgb", "finite numbers", "expected RGB contains a non-finite value")
		}
		if p.ExpectedPeakRadiance < 0 || math.IsNaN(p.ExpectedPeakRadiance) || math.IsInf(p.ExpectedPeakRadiance, 0) {
			return errs.Para(path+".expected_peak_radiance", "[0,+inf)", "expected peak radiance must be finite and non-negative")
		}
		if p.ExpectedChromaticity[0] < 0 || p.ExpectedChromaticity[0] > 1 ||
			p.ExpectedChromaticity[1] < 0 || p.ExpectedChromaticity[1] > 1 ||
			p.ExpectedChromaticity[0]+p.ExpectedChromaticity[1] > 1.000001 {
			return errs.Para(path+".expected_chromaticity", "[0,1] and x+y<=1", "invalid chromaticity coordinates")
		}
	}
	return nil
}

func ValidateConfig(c *RenderConfig) error {
	if c.WavelengthSteps < 2 || c.WavelengthSteps > 4096 {
		return errs.Para("render_config.wavelength_steps", "[2,4096]",
			"wavelength steps %d is out of range", c.WavelengthSteps)
	}
	switch c.Integration {
	case "trapezoid", "midpoint":
	default:
		return errs.Para("render_config.integration", `["trapezoid","midpoint"]`,
			"unsupported integration method %q", c.Integration)
	}
	if c.TileSize < 4 || c.TileSize > 1024 {
		return errs.Para("render_config.tile_size", "[4,1024]", "tile size %d is out of range", c.TileSize)
	}
	if c.MaxWorkers < 1 || c.MaxWorkers > 256 {
		return errs.Para("render_config.max_workers", "[1,256]", "worker count %d is out of range", c.MaxWorkers)
	}
	if c.PerThreadPixelBuffer < c.TileSize*c.TileSize || c.PerThreadPixelBuffer > 16*1024*1024 {
		return errs.Para("render_config.per_thread_pixel_buffer",
			fmt.Sprintf("[%d,%d]", c.TileSize*c.TileSize, 16*1024*1024),
			"per-thread buffer %d cannot hold a %dx%d tile", c.PerThreadPixelBuffer, c.TileSize, c.TileSize)
	}
	if c.QueueBudget < 1 || c.QueueBudget > 65536 {
		return errs.Para("render_config.queue_budget", "[1,65536]", "queue budget %d is out of range", c.QueueBudget)
	}
	if c.IndirectBounces < 0 || c.IndirectBounces > 16 {
		return errs.Para("render_config.indirect_bounces", "[0,16]", "indirect bounce count %d is out of range", c.IndirectBounces)
	}
	if math.IsNaN(c.Exposure) || math.IsInf(c.Exposure, 0) || c.Exposure < -20 || c.Exposure > 20 {
		return errs.Para("render_config.exposure", "[-20,20]", "exposure %g is out of range", c.Exposure)
	}
	switch c.ToneCurve {
	case "linear", "reinhard", "aces":
	default:
		return errs.Para("render_config.tone_curve", `["linear","reinhard","aces"]`,
			"unsupported tone curve %q", c.ToneCurve)
	}
	if math.IsNaN(c.Gamma) || c.Gamma < 0.1 || c.Gamma > 10 {
		return errs.Para("render_config.gamma", "[0.1,10]", "gamma %g is out of range", c.Gamma)
	}
	if math.IsNaN(c.SeamThreshold) || c.SeamThreshold < 0 || c.SeamThreshold > 1 {
		return errs.Para("render_config.seam_threshold", "[0,1]", "seam threshold %g is out of range", c.SeamThreshold)
	}
	if err := validateBits("render_config.quantization.albedo_bits", c.Quantization.AlbedoBits); err != nil {
		return err
	}
	if err := validateBits("render_config.quantization.roughness_bits", c.Quantization.RoughnessBits); err != nil {
		return err
	}
	if err := validateBits("render_config.quantization.metallic_bits", c.Quantization.MetallicBits); err != nil {
		return err
	}
	if err := validateBits("render_config.quantization.basis_bits", c.Quantization.BasisBits); err != nil {
		return err
	}
	if c.PreviewTolerance < 0 || c.PreviewTolerance > 1 || math.IsNaN(c.PreviewTolerance) {
		return errs.Para("render_config.preview_tolerance", "[0,1]", "preview tolerance is out of range")
	}
	if c.ColorTolerance < 0 || c.ColorTolerance > 1 || math.IsNaN(c.ColorTolerance) {
		return errs.Para("render_config.color_tolerance", "[0,1]", "color tolerance is out of range")
	}
	if c.MaxRetries < 0 || c.MaxRetries > 16 {
		return errs.Para("render_config.max_retries", "[0,16]", "max retries is out of range")
	}
	if c.InjectedGamut != nil {
		if c.InjectedGamut.Channel != "r" && c.InjectedGamut.Channel != "g" && c.InjectedGamut.Channel != "b" {
			return errs.Para("render_config.injected_gamut.channel", `["r","g","b"]`, "invalid injected gamut channel")
		}
		if math.IsNaN(c.InjectedGamut.Value) {
			return errs.Para("render_config.injected_gamut.value", "finite numbers", "injected gamut value must be finite")
		}
	}
	return nil
}

func validateBits(field string, bits int) error {
	if bits < 2 || bits > 24 {
		return errs.Para(field, "[2,24]", "bit depth %d is out of range", bits)
	}
	return nil
}

func unitRange(v float64, field string) error {
	if math.IsNaN(v) || v < 0 || v > 1 {
		return errs.Para(field, "[0,1]", "value %g is outside [0,1]", v)
	}
	return nil
}

func coefficientBounds(bits int, signed bool) (int64, int64) {
	if signed {
		limit := int64(1) << (bits - 1)
		return -limit, limit - 1
	}
	return 0, (int64(1) << bits) - 1
}

func decodeObject(data []byte, field string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, errs.Decode(field, "invalid JSON: %v", err)
	}
	if obj == nil {
		return nil, errs.Decode(field, "expected a JSON object")
	}
	return obj, nil
}

func requireFields(obj map[string]json.RawMessage, path string, fields ...string) error {
	for _, field := range fields {
		raw, ok := obj[field]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			return errs.Schema(path+"."+field, "required field is missing")
		}
	}
	return nil
}

func objectArray(raw json.RawMessage, path string) ([]map[string]json.RawMessage, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, errs.Decode(path, "expected an array: %v", err)
	}
	out := make([]map[string]json.RawMessage, len(arr))
	for i := range arr {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(arr[i], &obj); err != nil || obj == nil {
			return nil, errs.Decode(fmt.Sprintf("%s[%d]", path, i), "expected an object")
		}
		out[i] = obj
	}
	return out, nil
}

func requireFixedArray(raw json.RawMessage, path string, n int) error {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return errs.Decode(path, "expected an array: %v", err)
	}
	if len(arr) != n {
		return errs.Schema(path, "expected %d elements, got %d", n, len(arr))
	}
	return nil
}

func requireMatrix(raw json.RawMessage, path string) error {
	var rows [][]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return errs.Decode(path, "expected a matrix: %v", err)
	}
	if len(rows) != 3 {
		return errs.Schema(path, "expected 3 matrix rows, got %d", len(rows))
	}
	for i := range rows {
		if len(rows[i]) != 3 {
			return errs.Schema(fmt.Sprintf("%s[%d]", path, i), "expected 3 matrix columns, got %d", len(rows[i]))
		}
	}
	return nil
}

func checkInverse(g Gamut, tolerance float64) error {
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			sum := 0.0
			for k := 0; k < 3; k++ {
				sum += g.RGBToXYZ[r][k] * g.XYZToRGB[k][c]
			}
			want := 0.0
			if r == c {
				want = 1
			}
			if math.Abs(sum-want) > tolerance {
				return errs.Para("spectral.gamut", fmt.Sprintf("inverse residual <= %g", tolerance),
					"rgb_to_xyz and xyz_to_rgb are not inverses at [%d][%d] (residual %g)", r, c, sum-want)
			}
		}
	}
	return nil
}

func finite3(v [3]float64) bool {
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}

func norm3(v [3]float64) float64 {
	return math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
}

func objectExists(s *Scene, meshID string) bool {
	for _, m := range s.Meshes {
		if m.ID == meshID {
			return true
		}
	}
	return false
}
