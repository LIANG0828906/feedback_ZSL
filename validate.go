package fog

import "fmt"

func ValidateScene(scene *Scene) error {
	if scene == nil {
		return schemaError("scene", nil, "scene is missing")
	}
	if scene.SchemaVersion == "" {
		return schemaError("schema_version", scene.SchemaVersion, "schema version is required")
	}
	if scene.FixedScale != FixedScale {
		return schemaError("fixed_scale", scene.FixedScale, "fixed scale must be %d", FixedScale)
	}
	if scene.Viewport.Width <= 0 || scene.Viewport.Height <= 0 || scene.Viewport.Width > 512 || scene.Viewport.Height > 512 {
		return schemaError("viewport", scene.Viewport, "viewport dimensions must be in (0,512]")
	}
	if len(scene.Media) == 0 {
		return schemaError("media", len(scene.Media), "at least one medium is required")
	}

	seen := map[string]bool{}
	for i, medium := range scene.Media {
		if medium == nil {
			return schemaError(fmt.Sprintf("media[%d]", i), nil, "medium is missing")
		}
		if medium.ID == "" {
			return schemaError(fmt.Sprintf("media[%d].id", i), medium.ID, "medium id is required")
		}
		if seen[medium.ID] {
			return schemaError(fmt.Sprintf("media[%d].id", i), medium.ID, "duplicate medium id")
		}
		seen[medium.ID] = true
		if err := validateMedium(medium, i); err != nil {
			return err
		}
	}

	if err := validateCamera(&scene.CameraTrack); err != nil {
		return err
	}
	if scene.ActiveCamera < 0 || scene.ActiveCamera >= len(scene.CameraTrack.Samples) {
		return schemaError("active_camera", scene.ActiveCamera, "active camera index is out of range")
	}
	if err := ValidateRecipe(&scene.Recipe); err != nil {
		return err
	}
	if err := validateTolerances(scene.Tolerances); err != nil {
		return err
	}
	return validateLight(scene.Light)
}

func validateMedium(medium *Medium, index int) error {
	prefix := fmt.Sprintf("media[%d]", index)
	min := medium.Bounds.Min
	max := medium.Bounds.Max
	if min.X > max.X || min.Y > max.Y || min.Z > max.Z {
		return schemaError(prefix+".bounds", medium.Bounds, "bounds minimum corner must not exceed maximum corner")
	}
	if min.X == max.X || min.Y == max.Y || min.Z == max.Z {
		return schemaError(prefix+".bounds", medium.Bounds, "bounds must have positive volume")
	}
	maxCoeff := FixedFromFloat(MaxCoefficient)
	if medium.Absorption < 0 || medium.Absorption > maxCoeff {
		e := schemaError(prefix+".absorption", medium.Absorption.Float64(), "absorption must be in [0,%v] on fixed scale %d", MaxCoefficient, FixedScale)
		e.MediumID = medium.ID
		return e
	}
	if medium.Scattering < 0 || medium.Scattering > maxCoeff {
		e := schemaError(prefix+".scattering", medium.Scattering.Float64(), "scattering must be in [0,%v] on fixed scale %d", MaxCoefficient, FixedScale)
		e.MediumID = medium.ID
		return e
	}
	if medium.Anisotropy < -1 || medium.Anisotropy > 1 {
		e := schemaError(prefix+".anisotropy", medium.Anisotropy, "anisotropy must be in [-1,1]")
		e.MediumID = medium.ID
		return e
	}
	if medium.DensityScale < 0 || !finiteFloat(medium.DensityScale) {
		e := schemaError(prefix+".density_scale", medium.DensityScale, "density scale must be finite and non-negative")
		e.MediumID = medium.ID
		return e
	}
	if medium.Density == nil {
		e := schemaError(prefix+".density", nil, "integer density field is required")
		e.MediumID = medium.ID
		return e
	}
	return validateDensity(medium)
}

func validateDensity(medium *Medium) error {
	d := medium.Density
	if d.MediumID != medium.ID {
		e := schemaError("density.medium_id", d.MediumID, "density field medium id must match %q", medium.ID)
		e.MediumID = medium.ID
		return e
	}
	for axis, n := range d.Resolution {
		if n <= 0 || n > 128 {
			e := schemaError(fmt.Sprintf("density.resolution[%d]", axis), n, "density resolution must be in [1,128]")
			e.MediumID = medium.ID
			return e
		}
	}
	count := d.Resolution[0] * d.Resolution[1] * d.Resolution[2]
	if len(d.Values) != count {
		e := schemaError("density.values.length", len(d.Values), "expected %d integer voxels", count)
		e.MediumID = medium.ID
		return e
	}
	sizes := [3]Fixed{d.VoxelSize.X, d.VoxelSize.Y, d.VoxelSize.Z}
	mins := [3]Fixed{medium.Bounds.Min.X, medium.Bounds.Min.Y, medium.Bounds.Min.Z}
	maxs := [3]Fixed{medium.Bounds.Max.X, medium.Bounds.Max.Y, medium.Bounds.Max.Z}
	for axis := 0; axis < 3; axis++ {
		if sizes[axis] <= 0 {
			e := schemaError(fmt.Sprintf("density.voxel_size[%d]", axis), int64(sizes[axis]), "voxel size must be positive fixed-point value")
			e.MediumID = medium.ID
			return e
		}
		dimension := maxs[axis] - mins[axis]
		expected := Fixed(int64(d.Resolution[axis]) * int64(sizes[axis]))
		if dimension != expected {
			e := schemaError(fmt.Sprintf("density.resolution[%d]", axis), d.Resolution[axis], "resolution*voxel_size=%v must equal bounds dimension %v", expected.Float64(), dimension.Float64())
			e.MediumID = medium.ID
			return e
		}
	}
	for i, value := range d.Values {
		if value < 0 || value > MaxDensityVoxel {
			e := newError(CodeDensity, "density voxel %d is outside [0,%d]", i, MaxDensityVoxel)
			e.Field = fmt.Sprintf("media.%s.density.values[%d]", medium.ID, i)
			e.Value = value
			e.MediumID = medium.ID
			return e
		}
	}
	return nil
}

func validateCamera(track *CameraTrack) error {
	if track.ID == "" {
		return schemaError("camera_track.id", track.ID, "camera track id is required")
	}
	if track.Version < 1 {
		return schemaError("camera_track.version", track.Version, "camera parameter version must be positive")
	}
	if len(track.Samples) == 0 {
		return schemaError("camera_track.samples", 0, "at least one camera sample is required")
	}
	var previous int64
	for i, sample := range track.Samples {
		if i > 0 && sample.LogicalTime <= previous {
			e := newError(CodeOrder, "camera logical times must be strictly increasing at sample %d", i)
			e.Field = fmt.Sprintf("camera_track.samples[%d].logical_time", i)
			e.Value = sample.LogicalTime
			return e
		}
		previous = sample.LogicalTime
		if !finiteVec3(sample.Position) || !finiteVec3(sample.Forward) || !finiteVec3(sample.Up) {
			return schemaError(fmt.Sprintf("camera_track.samples[%d].transform", i), sample, "camera position and orientation must be finite")
		}
		if mathAbs(sample.Forward.Len()-1) > 1e-6 || mathAbs(sample.Up.Len()-1) > 1e-6 {
			return schemaError(fmt.Sprintf("camera_track.samples[%d].orientation", i), sample, "forward and up must be normalized")
		}
		if sample.FovYDegrees < 10 || sample.FovYDegrees > 120 {
			return schemaError(fmt.Sprintf("camera_track.samples[%d].fov_y_degrees", i), sample.FovYDegrees, "fov must be in [10,120]")
		}
		if sample.Near <= 0 || sample.Far <= sample.Near {
			return schemaError(fmt.Sprintf("camera_track.samples[%d].clip", i), [2]int64{int64(sample.Near), int64(sample.Far)}, "near/far clip interval is invalid")
		}
	}
	return nil
}

func ValidateRecipe(recipe *Recipe) error {
	if recipe == nil {
		return schemaError("recipe", nil, "recipe is required")
	}
	if recipe.ID == "" {
		return schemaError("recipe.id", recipe.ID, "recipe id is required")
	}
	if recipe.Version < 1 {
		return schemaError("recipe.version", recipe.Version, "recipe version must be positive")
	}
	positive := map[string]int{
		"tile_width":              recipe.TileWidth,
		"tile_height":             recipe.TileHeight,
		"max_steps_per_ray":       recipe.MaxStepsPerRay,
		"shadow_samples_per_step": recipe.ShadowSamplesPerStep,
		"shadow_budget_per_ray":   recipe.ShadowBudgetPerRay,
		"preview_steps_per_ray":   recipe.PreviewStepsPerRay,
		"preview_shadow_budget":   recipe.PreviewShadowBudget,
	}
	for field, value := range positive {
		if value <= 0 {
			return schemaError("recipe."+field, value, "recipe budget must be a positive integer")
		}
	}
	if recipe.PreviewStepsPerRay > recipe.MaxStepsPerRay {
		return schemaError("recipe.preview_steps_per_ray", recipe.PreviewStepsPerRay, "preview step budget cannot exceed exact budget %d", recipe.MaxStepsPerRay)
	}
	if recipe.PreviewShadowBudget > recipe.ShadowBudgetPerRay {
		return schemaError("recipe.preview_shadow_budget", recipe.PreviewShadowBudget, "preview shadow budget cannot exceed exact budget %d", recipe.ShadowBudgetPerRay)
	}
	if recipe.PhaseFunction != PhaseIsotropic && recipe.PhaseFunction != PhaseHenyeyGreenstein {
		return schemaError("recipe.phase_function", recipe.PhaseFunction, "unsupported phase function")
	}
	if recipe.PreviewDelayMS <= 0 {
		return schemaError("recipe.preview_delay_ms", recipe.PreviewDelayMS, "preview delay budget must be positive")
	}
	return nil
}

func validateTolerances(t Tolerances) error {
	values := map[string]float64{
		"segment_transmittance": t.SegmentTransmittance,
		"segment_scattering":    t.SegmentScattering,
		"preview_pixel":         t.PreviewPixel,
		"checkpoint_pixel":      t.CheckpointPixel,
		"convergence":           t.Convergence,
	}
	for field, value := range values {
		if value <= 0 || !finiteFloat(value) {
			return schemaError("tolerances."+field, value, "tolerance must be positive and finite")
		}
	}
	return nil
}

func validateLight(light Light) error {
	if !finiteVec3(light.Direction) || light.Direction.Len() == 0 {
		return schemaError("light.direction", light.Direction, "light direction must be a non-zero finite vector")
	}
	if light.Intensity < 0 || !finiteFloat(light.Intensity) {
		return schemaError("light.intensity", light.Intensity, "light intensity must be non-negative")
	}
	for i, c := range light.Color {
		if c < 0 || !finiteFloat(c) {
			return schemaError(fmt.Sprintf("light.color[%d]", i), c, "light color must be non-negative")
		}
	}
	return nil
}

func mathAbs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
