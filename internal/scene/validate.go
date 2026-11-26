package scene

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"

	"volumekernel/internal/model"
)

const (
	MaxDensityResolution = 128
	MaxCoefficient       = 4096.0
)

func LoadJSON(data []byte) (model.Scene, error) {
	var s model.Scene
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&s); err != nil {
		e := model.DecodeError("scene JSON is truncated or malformed").WithCause(err)
		if id := truncatedMediumID(data); id != "" {
			e.MediumID = id
			e.Field = "media"
		}
		return model.Scene{}, e
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return model.Scene{}, model.DecodeError("scene JSON has trailing values")
		}
		return model.Scene{}, model.DecodeError("scene JSON is truncated or malformed").WithCause(err)
	}
	if err := Validate(s); err != nil {
		return model.Scene{}, err
	}
	return s, nil
}

func Validate(s model.Scene) error {
	if s.ID == "" {
		return model.ParamError("", "id", "scene id must not be empty")
	}
	if s.FixedScale <= 0 {
		return model.ParamError("", "fixed_scale", "fixed scale must be positive")
	}
	if s.CoefficientScale <= 0 {
		return model.ParamError("", "coefficient_scale", "coefficient scale must be positive")
	}
	if s.Viewport.Width <= 0 || s.Viewport.Height <= 0 {
		return model.ParamError("", "viewport", "viewport must be positive")
	}
	if s.Recipe.Version == "" {
		return model.ParamError("", "recipe.version", "recipe version must not be empty")
	}
	if s.Recipe.Steps <= 0 || s.Recipe.PreviewSteps <= 0 || s.Recipe.PreviewSteps > s.Recipe.Steps {
		return model.ParamError("", "recipe.steps", "steps and preview_steps must be positive and preview_steps <= steps")
	}
	if s.Recipe.ShadowSamples < 0 || s.Recipe.PreviewShadowSamples < 0 || s.Recipe.ShadowBudget < 0 {
		return model.ParamError("", "recipe.shadow_samples", "shadow sample values must be non-negative")
	}
	if s.Recipe.BlockWidth <= 0 || s.Recipe.BlockHeight <= 0 {
		return model.ParamError("", "recipe.block_width", "block dimensions must be positive")
	}
	if s.Recipe.MaxParallelBlocks <= 0 || s.Recipe.MaxRaysPerBlock <= 0 {
		return model.ParamError("", "recipe.max_parallel_blocks", "parallel and ray buffer limits must be positive")
	}
	if s.Recipe.PixelTolerance < 0 || !isFinite(s.Recipe.PixelTolerance) {
		return model.ParamError("", "recipe.pixel_tolerance", "pixel tolerance must be finite and non-negative")
	}
	if s.Recipe.OverlapMode != "ordered-composite" {
		return model.ParamError("", "recipe.overlap_mode", "unsupported overlap mode %q", s.Recipe.OverlapMode)
	}
	if s.Recipe.Phase != model.PhaseIsotropic && s.Recipe.Phase != model.PhaseHG {
		return model.ParamError("", "recipe.phase", "unsupported phase type %q", s.Recipe.Phase)
	}
	if s.Light.Direction.Normalize().Length() == 0 {
		return model.ParamError("", "light.direction", "light direction must be non-zero")
	}
	if s.Light.Radiance < 0 || !isFinite(s.Light.Radiance) {
		return model.ParamError("", "light.radiance", "light radiance must be finite and non-negative")
	}
	if s.Light.ShadowTau < 0 || s.Light.ShadowProfileGain < 0 {
		return model.ParamError("", "light.shadow_tau", "shadow tau and profile gain must be non-negative")
	}
	ids := make(map[string]struct{}, len(s.Media))
	for _, m := range s.Media {
		if _, exists := ids[m.ID]; exists {
			return model.ParamError(m.ID, "id", "duplicate medium id")
		}
		ids[m.ID] = struct{}{}
		if err := validateMedium(s, m); err != nil {
			return err
		}
	}
	if len(s.CameraTrack.Samples) == 0 {
		return model.OrderError("camera track must contain at least one sample")
	}
	prev := math.Inf(-1)
	for i, c := range s.CameraTrack.Samples {
		if c.ID == "" {
			return model.ParamError("", "camera.id", "camera sample %d has empty id", i)
		}
		if !c.Position.IsFinite() || !c.Target.IsFinite() || !c.Up.IsFinite() {
			return model.ParamError(c.ID, "camera.position", "camera vectors must be finite")
		}
		if !isFinite(c.Time) || c.Time < prev {
			e := model.OrderError("camera sample %d (%s) time %.6f is before predecessor %.6f", i, c.ID, c.Time, prev)
			e.CameraID = c.ID
			return e
		}
		if c.Target.Sub(c.Position).Length() < 1e-12 {
			return model.ParamError(c.ID, "camera.target", "target must differ from position")
		}
		if c.Up.Length() < 1e-12 {
			return model.ParamError(c.ID, "camera.up", "up vector must be non-zero")
		}
		if c.FOVDeg <= 0 || c.FOVDeg >= 180 || !isFinite(c.FOVDeg) {
			return model.ParamError(c.ID, "camera.fov_deg", "field of view must be in (0,180)")
		}
		if c.Aspect <= 0 || !isFinite(c.Aspect) {
			return model.ParamError(c.ID, "camera.aspect", "aspect must be positive")
		}
		prev = c.Time
	}
	return nil
}

func validateMedium(s model.Scene, m model.Medium) error {
	if m.ID == "" {
		return model.ParamError("", "media.id", "medium id must not be empty")
	}
	if !m.Bounds.Valid() {
		return model.ParamError(m.ID, "bounds", "minimum corner must not exceed maximum corner")
	}
	if err := validateCoefficient(s, m, "absorption", m.Absorption); err != nil {
		return err
	}
	if err := validateCoefficient(s, m, "scattering", m.Scattering); err != nil {
		return err
	}
	if !isFinite(m.Anisotropy) || m.Anisotropy < -0.999 || m.Anisotropy > 0.999 {
		return model.ParamError(m.ID, "anisotropy", "anisotropy must be finite and in [-0.999,0.999]")
	}
	d := m.Density
	if d.NX <= 0 || d.NY <= 0 || d.NZ <= 0 || d.NX > MaxDensityResolution || d.NY > MaxDensityResolution || d.NZ > MaxDensityResolution {
		return model.DensityError(m.ID, "density.resolution", "resolution (%d,%d,%d) must be within 1..%d", d.NX, d.NY, d.NZ, MaxDensityResolution)
	}
	if d.Len() != len(d.Values) {
		return model.DensityError(m.ID, "density.values", "density values length %d does not match resolution %dx%dx%d", len(d.Values), d.NX, d.NY, d.NZ)
	}
	ext := m.Bounds.Extent()
	voxelTicks := []float64{
		ext.X * float64(s.FixedScale) / float64(d.NX),
		ext.Y * float64(s.FixedScale) / float64(d.NY),
		ext.Z * float64(s.FixedScale) / float64(d.NZ),
	}
	for axis, ticks := range voxelTicks {
		if ticks < 1 {
			return model.DensityError(m.ID, "density.resolution", "axis %d voxel size is below the declared fixed-point tick", axis)
		}
	}
	if ext.X <= 0 || ext.Y <= 0 || ext.Z <= 0 {
		return model.DensityError(m.ID, "density.resolution", "density volume must have non-zero extent")
	}
	return nil
}

func validateCoefficient(s model.Scene, m model.Medium, field string, v float64) error {
	if !isFinite(v) || v < 0 || v > MaxCoefficient {
		return model.ParamError(m.ID, field, "coefficient %v must be finite and within [0,%.0f]", v, MaxCoefficient)
	}
	if !model.IsQuantized(v, s.CoefficientScale) {
		return model.ParamError(m.ID, field, "coefficient %v is not representable at scale %d", v, s.CoefficientScale)
	}
	return nil
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

type InjectionKind string

const (
	InjectDecode InjectionKind = "decode"
	InjectOrder  InjectionKind = "order"
	InjectDens   InjectionKind = "dens"
	InjectBlock  InjectionKind = "block"
)

func Inject(s model.Scene, kind InjectionKind) (model.Scene, error) {
	s = cloneScene(s)
	switch kind {
	case InjectDecode:
		e := model.DecodeError("injected truncated medium payload")
		if len(s.Media) > 0 {
			e.MediumID = s.Media[0].ID
			e.Field = "media"
		}
		return model.Scene{}, e
	case InjectOrder:
		if len(s.CameraTrack.Samples) < 2 {
			return s, model.OrderError("injected camera order requires at least two samples")
		}
		s.CameraTrack.Samples[0].Time, s.CameraTrack.Samples[1].Time = s.CameraTrack.Samples[1].Time, s.CameraTrack.Samples[0].Time
		if err := Validate(s); err != nil {
			return s, err
		}
		return s, model.OrderError("injected camera time inversion was not detected")
	case InjectDens:
		if len(s.Media) == 0 {
			return s, model.DensityError("", "density.resolution", "injected density failure needs a medium")
		}
		s.Media[0].Density.NX = MaxDensityResolution + 1
		if err := Validate(s); err != nil {
			return s, err
		}
		return s, model.DensityError(s.Media[0].ID, "density.resolution", "injected density resolution overflow")
	case InjectBlock:
		return s, model.BlockError(1, 0, fmt.Errorf("injected single-block failure"))
	default:
		return s, model.ParamError("", "injection", "unknown injection %q", kind)
	}
}

func cloneScene(s model.Scene) model.Scene {
	cp := s
	cp.Media = append([]model.Medium(nil), s.Media...)
	for i := range cp.Media {
		cp.Media[i].Density.Values = append([]uint8(nil), s.Media[i].Density.Values...)
	}
	cp.CameraTrack.Samples = append([]model.CameraSample(nil), s.CameraTrack.Samples...)
	return cp
}

var mediumIDPattern = regexp.MustCompile(`"id"\s*:\s*"([^"]+)"`)

func truncatedMediumID(data []byte) string {
	media := bytes.Index(data, []byte(`"media"`))
	if media < 0 {
		return ""
	}
	m := mediumIDPattern.FindStringSubmatch(string(data[media:]))
	if len(m) == 2 {
		return m[1]
	}
	return ""
}
