package fog

import (
	"fmt"
	"math"
)

// ValidateScenario checks the engineering contract for a loaded scenario.
func ValidateScenario(s *Scenario) error {
	if s == nil {
		return &Error{Code: ESchema, Field: "scenario", Message: "nil scenario"}
	}
	if s.FixedScale <= 0 {
		return &Error{Code: ESchema, Field: "fixed_scale", Message: "must be positive"}
	}
	if s.Width <= 0 || s.Height <= 0 {
		return &Error{Code: ESchema, Field: "viewport", Message: "width and height must be positive"}
	}
	if s.Recipe.TileSize <= 0 {
		return &Error{Code: ESchema, Field: "recipe.tile_size", Message: "must be positive"}
	}
	if s.Recipe.Steps <= 0 || s.Recipe.PreviewSteps <= 0 {
		return &Error{Code: ESchema, Field: "recipe.steps", Message: "step counts must be positive"}
	}
	if s.Recipe.ShadowSamples < 0 || s.Recipe.PreviewShadowSamples < 0 {
		return &Error{Code: ESchema, Field: "recipe.shadow_samples", Message: "must be non-negative"}
	}
	if s.Recipe.MaxParallelBlocks <= 0 || s.Recipe.RayBufferPerWorker <= 0 {
		return &Error{Code: ESchema, Field: "recipe.budget", Message: "parallel and buffer limits must be positive"}
	}

	seenMedia := map[string]bool{}
	for _, m := range s.Media {
		if m == nil {
			return &Error{Code: ESchema, Field: "media[]", Message: "nil medium"}
		}
		if m.ID == "" {
			return &Error{Code: ESchema, MediaID: m.ID, Field: "media[].id", Message: "medium id is required"}
		}
		if seenMedia[m.ID] {
			return &Error{Code: ESchema, MediaID: m.ID, Field: "media[].id", Message: "duplicate medium id"}
		}
		seenMedia[m.ID] = true
		if err := ValidateMedium(m, s.FixedScale); err != nil {
			return err
		}
	}

	if len(s.Cameras.Cameras) == 0 {
		return &Error{Code: ESchema, Field: "cameras", Message: "at least one camera is required"}
	}
	for i := 1; i < len(s.Cameras.Cameras); i++ {
		prev := s.Cameras.Cameras[i-1]
		cur := s.Cameras.Cameras[i]
		if !(cur.Time > prev.Time) {
			return &Error{
				Code: EOrder, Field: fmt.Sprintf("cameras[%d].time", i),
				Message: fmt.Sprintf("camera time %.6f is not strictly after cameras[%d].time %.6f", cur.Time, i-1, prev.Time),
			}
		}
	}
	for i, c := range s.Cameras.Cameras {
		if c.Forward.Len() == 0 || c.Right.Len() == 0 || c.Up.Len() == 0 {
			return &Error{Code: ESchema, Field: fmt.Sprintf("cameras[%d].basis", i), Message: "camera basis vectors must be non-zero"}
		}
		if c.FovY <= 0 || c.FovY >= 180 {
			return &Error{Code: ESchema, Field: fmt.Sprintf("cameras[%d].fov_y_degrees", i), Message: "fov must be in (0,180)"}
		}
	}
	return nil
}

func ValidateMedium(m *Medium, scale int64) error {
	axes := []struct {
		name string
		min  float64
		max  float64
		res  int
	}{
		{"x", m.Bounds.Min.X, m.Bounds.Max.X, m.Field.Resolution[0]},
		{"y", m.Bounds.Min.Y, m.Bounds.Max.Y, m.Field.Resolution[1]},
		{"z", m.Bounds.Min.Z, m.Bounds.Max.Z, m.Field.Resolution[2]},
	}
	for _, ax := range axes {
		if ax.min > ax.max {
			return &Error{
				Code: ESchema, MediaID: m.ID,
				Field:   "bounds." + ax.name,
				Message: fmt.Sprintf("minimum %.9f is greater than maximum %.9f", ax.min, ax.max),
			}
		}
		if ax.res <= 0 {
			return &Error{
				Code: ESchema, MediaID: m.ID,
				Field:   "density.resolution." + ax.name,
				Message: "voxel resolution must be positive",
			}
		}
		size := ax.max - ax.min
		if size <= 0 {
			return &Error{
				Code: ESchema, MediaID: m.ID,
				Field:   "bounds." + ax.name,
				Message: "bounds extent must be positive for a voxelized medium",
			}
		}
		if m.Field.VoxelSize <= 0 {
			return &Error{
				Code: ESchema, MediaID: m.ID,
				Field: "density.voxel_size", Message: "must be positive",
			}
		}
		expected := float64(ax.res) * m.Field.VoxelSize
		if math.Abs(size-expected) > 1e-6 {
			return &Error{
				Code: ESchema, MediaID: m.ID,
				Field:  "density.resolution." + ax.name,
				Actual: size / float64(ax.res), Expected: m.Field.VoxelSize,
				Message: fmt.Sprintf("bounds extent %.9f is incompatible with %d voxels of size %.9f", size, ax.res, m.Field.VoxelSize),
			}
		}
	}
	if len(m.Field.Values) != m.Field.VoxelCount() {
		return &Error{
			Code: ESchema, MediaID: m.ID, Field: "density.values",
			Actual: float64(len(m.Field.Values)), Expected: float64(m.Field.VoxelCount()),
			Message: "density value length does not match resolution product",
		}
	}
	if m.Absorption < 0 {
		return &Error{Code: ESchema, MediaID: m.ID, Field: "absorption", Actual: m.Absorption, Message: "must be non-negative"}
	}
	if m.Scattering < 0 {
		return &Error{Code: ESchema, MediaID: m.ID, Field: "scattering", Actual: m.Scattering, Message: "must be non-negative"}
	}
	if !fixedMultiple(m.Absorption, scale) {
		return &Error{Code: ESchema, MediaID: m.ID, Field: "absorption", Actual: m.Absorption, Message: "value is not representable at declared fixed scale"}
	}
	if !fixedMultiple(m.Scattering, scale) {
		return &Error{Code: ESchema, MediaID: m.ID, Field: "scattering", Actual: m.Scattering, Message: "value is not representable at declared fixed scale"}
	}
	if m.Anisotropy < -1 || m.Anisotropy > 1 {
		return &Error{Code: ESchema, MediaID: m.ID, Field: "anisotropy", Actual: m.Anisotropy, Message: "must be in [-1,1]"}
	}
	for i, v := range m.Field.Values {
		if v < m.Field.MinValue || v > m.Field.MaxValue {
			return &Error{
				Code: EDens, MediaID: m.ID, Field: fmt.Sprintf("density.values[%d]", i),
				Actual: float64(v), Expected: float64(m.Field.MaxValue),
				Message: "density voxel is outside declared value range",
			}
		}
	}
	return nil
}

// ApplyDensityEdit validates and applies an edit. An unchanged value is a
// successful no-op and callers must produce an empty invalidation manifest.
func ApplyDensityEdit(s *Scenario, edit DensityEdit) (DensityEdit, bool, error) {
	m, ok := s.Medium(edit.MediumID)
	if !ok {
		return DensityEdit{}, false, &Error{Code: EDens, MediaID: edit.MediumID, Field: "medium_id", Message: "unknown medium"}
	}
	if !m.Field.InBounds(edit.X, edit.Y, edit.Z) {
		return edit, false, &Error{
			Code: EDens, MediaID: m.ID,
			Field:  fmt.Sprintf("density.voxel[%d,%d,%d]", edit.X, edit.Y, edit.Z),
			Actual: float64(edit.X), Message: "voxel coordinate is outside density resolution",
		}
	}
	if edit.NewValue < m.Field.MinValue || edit.NewValue > m.Field.MaxValue {
		return edit, false, &Error{
			Code: EDens, MediaID: m.ID,
			Field:  fmt.Sprintf("density.values[%d,%d,%d]", edit.X, edit.Y, edit.Z),
			Actual: float64(edit.NewValue), Expected: float64(m.Field.MaxValue),
			Message: "edited density is outside declared range",
		}
	}
	idx := m.Field.Index(edit.X, edit.Y, edit.Z)
	old := m.Field.Values[idx]
	edit.OldValue = old
	if old == edit.NewValue {
		return edit, false, nil
	}
	m.Field.Values[idx] = edit.NewValue
	m.Rehash()
	return edit, true, nil
}
