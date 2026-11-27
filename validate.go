package volumefog

import (
	"math"
)

// ValidateScene enforces the declared fixed-point and schema contract.
func ValidateScene(scene *Scene) error {
	if scene == nil {
		return schemaErr("", "scene", "scene is nil")
	}
	if scene.Schema != SchemaVersion {
		return schemaErr("", "schema", "unsupported schema version")
	}
	if scene.FixedScale != FixedScale {
		return schemaErr("", "fixed_scale", "fixed scale must be 1024")
	}
	if scene.CoefficientScale != CoeffScale {
		return schemaErr("", "coefficient_scale", "coefficient scale must be 1000000")
	}
	if scene.ChunkSize <= 0 || scene.ChunkSize > 256 {
		return schemaErr("", "chunk_size", "chunk size must be in [1,256]")
	}
	if len(scene.Media) == 0 {
		return schemaErr("", "media", "at least one medium is required")
	}
	if len(scene.Cameras) == 0 {
		return schemaErr("", "cameras", "at least one camera sample is required")
	}
	ids := make(map[string]bool)
	for i := range scene.Media {
		m := &scene.Media[i]
		if m.ID == "" {
			return schemaErr("<unknown>", "id", "medium id is required")
		}
		if ids[m.ID] {
			return schemaErr(m.ID, "id", "duplicate medium id")
		}
		ids[m.ID] = true
		for a := 0; a < 3; a++ {
			if m.MinQ[a] > m.MaxQ[a] {
				return schemaErr(m.ID, "bounds", "minimum corner is greater than maximum corner")
			}
		}
		ext := m.ExtentQ()
		for a := 0; a < 3; a++ {
			if ext[a] <= 0 {
				return schemaErr(m.ID, "bounds", "medium has a zero or negative extent")
			}
			if m.DensityRes[a] <= 0 || m.DensityRes[a] > 256 {
				return schemaErr(m.ID, "density_res", "density resolution must be in [1,256]")
			}
			if ext[a]%int64(m.DensityRes[a]) != 0 {
				return schemaErr(m.ID, "density_res", "bounding extent is not divisible by density resolution")
			}
			voxel := ext[a] / int64(m.DensityRes[a])
			if voxel < MinVoxelQ || voxel > MaxVoxelQ {
				return schemaErr(m.ID, "density_res", "voxel size is outside the declared fixed-point range")
			}
		}
		want := m.VoxelCount()
		if len(m.Density) != want {
			return schemaErr(m.ID, "density", "density field length does not match density_res")
		}
		for j, v := range m.Density {
			if v < 0 || v > MaxDensity {
				return codeErr(E_DENS, m.ID, densityField(j), "density voxel is outside [0,255]")
			}
		}
		raw := m.DensityRawCopy()
		if got := DensityChecksum(raw); got != m.DensityChecksum {
			e := codeErr(E_DECODE, m.ID, "density_checksum", "density payload checksum mismatch")
			e.Cause = "stored checksum does not match decoded payload"
			return e
		}
		if m.AbsorptionQ < 0 || m.AbsorptionQ > 16*CoeffScale {
			return schemaErr(m.ID, "absorption_q", "absorption coefficient is negative or outside fixed-point range")
		}
		if m.ScatteringQ < 0 || m.ScatteringQ > 16*CoeffScale {
			return schemaErr(m.ID, "scattering_q", "scattering coefficient is negative or outside fixed-point range")
		}
		if m.AnisotropyQ < -CoeffScale || m.AnisotropyQ > CoeffScale {
			return schemaErr(m.ID, "anisotropy_q", "anisotropy is outside [-1,1] fixed-point range")
		}
	}
	for i := range scene.Cameras {
		c := &scene.Cameras[i]
		if c.TimeUS < 0 {
			return schemaErr(cameraID(i), "time_us", "camera time must be non-negative")
		}
		if i > 0 && c.TimeUS <= scene.Cameras[i-1].TimeUS {
			return &FogError{Code: E_ORDER, Message: "camera sample times must be strictly increasing",
				MediaID: cameraID(i), Field: "time_us"}
		}
		if c.ViewWidthQ <= 0 || c.ViewHeightQ <= 0 {
			return schemaErr(cameraID(i), "view_size_q", "view dimensions must be positive")
		}
		if c.FovDeg <= 0 || c.FovDeg >= 180 || math.IsNaN(c.FovDeg) {
			return schemaErr(cameraID(i), "fov_deg", "fov must be in (0,180)")
		}
		if c.NearQ < 0 || c.FarQ <= c.NearQ {
			return schemaErr(cameraID(i), "far_q", "far plane must be greater than near plane")
		}
		if c.Width <= 0 || c.Height <= 0 || c.Width > 4096 || c.Height > 4096 {
			return schemaErr(cameraID(i), "width", "viewport dimensions must be in [1,4096]")
		}
	}
	if scene.ActiveCamera < 0 || scene.ActiveCamera >= len(scene.Cameras) {
		return schemaErr("", "active_camera", "active camera index is out of range")
	}
	r := scene.Recipe
	if r.Steps <= 0 || r.Steps > 8192 {
		return schemaErr("", "recipe.steps", "step budget must be in [1,8192]")
	}
	if r.PreviewSteps <= 0 || r.PreviewSteps > 8192 {
		return schemaErr("", "recipe.preview_steps", "preview step budget must be in [1,8192]")
	}
	if r.PhaseType != "isotropic" && r.PhaseType != "hg" {
		return schemaErr("", "recipe.phase_type", "phase type must be isotropic or hg")
	}
	if r.ShadowSamples < 0 || r.ShadowSamples > 4096 {
		return schemaErr("", "recipe.shadow_samples", "shadow samples must be in [0,4096]")
	}
	if r.PreviewShadowSamples < 0 || r.PreviewShadowSamples > r.ShadowSamples {
		return schemaErr("", "recipe.preview_shadow_samples", "preview shadow samples are out of range")
	}
	if r.TransmittanceCutoff <= 0 || r.TransmittanceCutoff > 1 || math.IsNaN(r.TransmittanceCutoff) {
		return schemaErr("", "recipe.transmittance_cutoff", "transmittance cutoff must be in (0,1]")
	}
	if r.MaxParallel <= 0 || r.MaxParallel > 256 {
		return schemaErr("", "recipe.max_parallel", "max parallel chunks must be in [1,256]")
	}
	if r.RayBuffer <= 0 || r.RayBuffer > 4096 {
		return schemaErr("", "recipe.ray_buffer", "ray buffer must be in [1,4096]")
	}
	if r.ShadowBudget <= 0 || r.ShadowBudget > 1<<20 {
		return schemaErr("", "recipe.shadow_budget", "shadow budget must be positive")
	}
	if r.PreviewLatencyMS <= 0 {
		return schemaErr("", "recipe.preview_latency_ms", "preview latency cap must be positive")
	}
	ln := math.Sqrt(r.LightDir[0]*r.LightDir[0] + r.LightDir[1]*r.LightDir[1] + r.LightDir[2]*r.LightDir[2])
	if ln < 1e-9 {
		return schemaErr("", "recipe.light_dir", "light direction must be non-zero")
	}
	for _, v := range r.AmbientColor {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return schemaErr("", "recipe.ambient_color", "ambient color must be finite and non-negative")
		}
	}
	return nil
}

func densityField(index int) string {
	return "density[" + itoa(index) + "]"
}

func cameraID(i int) string { return "camera[" + itoa(i) + "]" }

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
