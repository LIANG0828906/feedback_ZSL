package maskqa

import (
	"fmt"
	"math"
)

func ValidateDataset(d *Dataset) error {
	if d == nil {
		return schemaError("dataset", "root", "dataset is nil")
	}
	if d.SchemaVersion != SchemaVersion {
		return schemaError("schema_version", "dataset", "want %q got %q", SchemaVersion, d.SchemaVersion)
	}
	if d.Params.RadiusMin <= 0 || d.Params.RadiusMax <= 0 {
		return schemaError("radius_band", "dataset", "radius endpoints must be positive integers: [%d,%d]", d.Params.RadiusMin, d.Params.RadiusMax)
	}
	if d.Params.RadiusMin > d.Params.RadiusMax {
		return schemaError("radius_band.min", "dataset", "minimum %d exceeds maximum %d", d.Params.RadiusMin, d.Params.RadiusMax)
	}
	if d.RadiusBand[0] != d.Params.RadiusMin || d.RadiusBand[1] != d.Params.RadiusMax {
		return schemaError("radius_band", "dataset", "declared band %v does not match params [%d,%d]", d.RadiusBand, d.Params.RadiusMin, d.Params.RadiusMax)
	}
	if d.Params.MaxIterations <= 0 {
		return schemaError("iteration_limit", "dataset", "must be positive, got %d", d.Params.MaxIterations)
	}
	if len(d.Samples) == 0 {
		return schemaError("samples", "dataset", "at least one sample is required")
	}
	seen := map[string]bool{}
	for _, s := range d.Samples {
		if s == nil {
			return schemaError("samples[]", "dataset", "sample is nil")
		}
		if s.ID == "" {
			return schemaError("samples[].id", fmt.Sprintf("index-%d", len(seen)), "sample id is empty")
		}
		if seen[s.ID] {
			return schemaError("samples[].id", s.ID, "duplicate sample id")
		}
		seen[s.ID] = true
		if s.Width <= 0 || s.Height <= 0 || s.Width > 4096 || s.Height > 4096 {
			return schemaError("width/height", s.ID, "illegal dimensions %dx%d", s.Width, s.Height)
		}
		if s.Raster == nil || s.Raster.Width != s.Width || s.Raster.Height != s.Height || len(s.Raster.RGBA) != s.Width*s.Height*4 {
			return schemaError("raster", s.ID, "raster dimensions or RGBA length mismatch")
		}
		if s.Rough == nil || s.Rough.Width != s.Width || s.Rough.Height != s.Height || len(s.Rough.Data) != s.Width*s.Height {
			return schemaError("rough_mask", s.ID, "rough mask dimensions or length mismatch")
		}
		if s.Truth == nil || s.Truth.Mask == nil || s.Truth.Mask.Width != s.Width || s.Truth.Mask.Height != s.Height || len(s.Truth.Mask.Data) != s.Width*s.Height {
			return schemaError("truth.mask", s.ID, "truth mask dimensions or length mismatch")
		}
		if err := validateComponents(s); err != nil {
			return err
		}
	}
	return nil
}

func validateComponents(s *Sample) error {
	seen := map[string]bool{}
	truthPixels := 0
	for i, c := range s.Truth.Components {
		oid := s.ID + "/component:" + c.ID
		if c.ID == "" {
			oid = fmt.Sprintf("%s/component[%d]", s.ID, i)
			return schemaError("truth.components[].id", oid, "component id is empty")
		}
		if seen[c.ID] {
			return schemaError("truth.components[].id", oid, "duplicate component id")
		}
		seen[c.ID] = true
		if c.Area <= 0 || c.Area > s.Width*s.Height {
			return schemaError("truth.components[].area", oid, "illegal area %d", c.Area)
		}
		if c.Holes < 0 {
			return schemaError("truth.components[].holes", oid, "holes must be non-negative")
		}
		b := c.Bounds
		if b.Width <= 0 || b.Height <= 0 || b.X < 0 || b.Y < 0 || b.Right() > s.Width || b.Bottom() > s.Height || b.Width > s.Width || b.Height > s.Height {
			return schemaError("truth.components[].bounds", oid, "illegal bounds %+v for %dx%d", b, s.Width, s.Height)
		}
		if c.CentroidX < float64(b.X) || c.CentroidX >= float64(b.Right()) || c.CentroidY < float64(b.Y) || c.CentroidY >= float64(b.Bottom()) {
			return schemaError("truth.components[].centroid", oid, "centroid (%.3f,%.3f) outside bounds %+v", c.CentroidX, c.CentroidY, b)
		}
		truthPixels += c.Area
	}
	actualArea := 0
	for _, v := range s.Truth.Mask.Data {
		if v != 0 {
			actualArea++
		}
	}
	if truthPixels != actualArea {
		return schemaError("truth.components[].area", s.ID, "component area sum %d != truth mask pixels %d", truthPixels, actualArea)
	}
	recomputed := ComputeComponents(s.Truth.Mask)
	if len(recomputed) != len(s.Truth.Components) {
		return schemaError("truth.components", s.ID, "component count %d != recomputed %d", len(s.Truth.Components), len(recomputed))
	}
	for i := range recomputed {
		got := s.Truth.Components[i]
		want := recomputed[i]
		if got.Area != want.Area || got.Bounds != want.Bounds || got.Holes != want.Holes || math.Abs(got.CentroidX-want.CentroidX) > 1e-6 || math.Abs(got.CentroidY-want.CentroidY) > 1e-6 {
			return schemaError("truth.components", s.ID, "component %s does not match parsed truth", got.ID)
		}
	}
	return nil
}

func (p Params) Validate() error {
	if p.SchemaVersion != SchemaVersion {
		return paraError("schema_version", SchemaVersion, SchemaVersion, p.SchemaVersion, "unsupported schema version")
	}
	if p.BlockSize < 8 || p.BlockSize > 256 {
		return paraError("block_size", 8, 256, p.BlockSize, "block size outside supported interval")
	}
	if err := p.ValidateGradient(); err != nil {
		return err
	}
	if p.QuantilePermille < 1 || p.QuantilePermille > 999 {
		return paraError("quantile_permille", 1, 999, p.QuantilePermille, "quantile scale must be in permille")
	}
	if p.EdgeBandWidth < 1 || p.EdgeBandWidth > 12 {
		return paraError("edge_band_width", 1, 12, p.EdgeBandWidth, "edge band width outside supported interval")
	}
	if p.CurvatureBins < 1 || p.CurvatureBins > 8 {
		return paraError("curvature_bins", 1, 8, p.CurvatureBins, "curvature segment bins outside supported interval")
	}
	if p.CurvatureWindow < 1 || p.CurvatureWindow > 8 {
		return paraError("curvature_window", 1, 8, p.CurvatureWindow, "curvature window outside supported interval")
	}
	if p.RadiusMin < 1 || p.RadiusMin > 12 {
		return paraError("radius_min", 1, 12, p.RadiusMin, "radius minimum outside supported interval")
	}
	if p.RadiusMax < p.RadiusMin || p.RadiusMax > 12 {
		return paraError("radius_max", p.RadiusMin, 12, p.RadiusMax, "radius maximum must be >= min and <= 12")
	}
	if p.MaxIterations < 1 || p.MaxIterations > 30 {
		return paraError("max_iterations", 1, 30, p.MaxIterations, "iteration limit outside supported interval")
	}
	if p.ChangeThreshold < 0 || p.ChangeThreshold > 1_000_000 {
		return paraError("change_threshold", 0, 1_000_000, p.ChangeThreshold, "convergence threshold outside supported interval")
	}
	if p.ComposeThresholdPermille < 1 || p.ComposeThresholdPermille > 999 {
		return paraError("compose_threshold_permille", 1, 999, p.ComposeThresholdPermille, "composition threshold outside supported interval")
	}
	if p.ComposeOpenWeight < 1 || p.ComposeOpenWeight > 100 || p.ComposeAuxWeight < 0 || p.ComposeAuxWeight > 100 {
		return paraError("compose_weights", [2]int{1, 0}, [2]int{100, 100}, [2]int{p.ComposeOpenWeight, p.ComposeAuxWeight}, "composition weights outside supported interval")
	}
	if p.ComposeOpenWeight <= 2*p.ComposeAuxWeight {
		return paraError("compose_open_weight", 2*p.ComposeAuxWeight+1, 100, p.ComposeOpenWeight, "open mask must be authoritative over close and rough votes")
	}
	if p.TraceScale < 1000 || p.TraceScale > 10_000_000_000 {
		return paraError("trace_scale", 1000, 10_000_000_000, p.TraceScale, "trace scale outside supported interval")
	}
	if p.MaxParallelBlocks < 1 || p.MaxParallelBlocks > 8 {
		return paraError("max_parallel_blocks", 1, 8, p.MaxParallelBlocks, "parallelism outside supported interval")
	}
	if p.ThreadPixelBufferLimit < p.BlockSize*p.BlockSize || p.ThreadPixelBufferLimit > 4_194_304 {
		return paraError("thread_pixel_buffer_limit", p.BlockSize*p.BlockSize, 4_194_304, p.ThreadPixelBufferLimit, "per-thread pixel buffer outside supported interval")
	}
	if p.QueuedPixelBudget < p.BlockSize*p.BlockSize || p.QueuedPixelBudget > 16_777_216 {
		return paraError("queued_pixel_budget", p.BlockSize*p.BlockSize, 16_777_216, p.QueuedPixelBudget, "queue pixel budget outside supported interval")
	}
	return nil
}

func (p Params) ValidateGradient() error {
	if p.OperatorSize != 3 && p.OperatorSize != 5 {
		return paraError("gradient_operator_size", 3, 5, p.OperatorSize, "only odd operator sizes 3 and 5 are declared")
	}
	validOperator := false
	for _, op := range []string{"sobel3", "prewitt3", "sobel5"} {
		if p.Operator == op {
			validOperator = true
		}
	}
	if !validOperator {
		return paraError("gradient_operator", "sobel3|prewitt3|sobel5", "sobel3|prewitt3|sobel5", p.Operator, "unknown gradient operator")
	}
	if (p.Operator == "sobel3" || p.Operator == "prewitt3") && p.OperatorSize != 3 {
		return paraError("gradient_operator_size", 3, 3, p.OperatorSize, "operator %s requires size 3", p.Operator)
	}
	if p.Operator == "sobel5" && p.OperatorSize != 5 {
		return paraError("gradient_operator_size", 5, 5, p.OperatorSize, "operator sobel5 requires size 5")
	}
	if p.FixedScale < 16 || p.FixedScale > 1<<20 {
		return paraError("fixed_scale", 16, 1<<20, p.FixedScale, "fixed point scale outside declared interval")
	}
	if p.LumaWeightR+p.LumaWeightG+p.LumaWeightB != 1000 || p.LumaWeightR < 0 || p.LumaWeightG < 0 || p.LumaWeightB < 0 {
		return paraError("luma_weights", "sum=1000 and non-negative", "sum=1000", [3]int{p.LumaWeightR, p.LumaWeightG, p.LumaWeightB}, "luma weights must be declared permille weights")
	}
	return nil
}
