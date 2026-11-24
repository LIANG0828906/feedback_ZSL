package core

import "math"

func validateRange(field string, value, min, max float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < min || value > max {
		return WithField(NewError(CodePara, "parameter %s=%v is outside [%v,%v]", field, value, min, max), field)
	}
	return nil
}

func validateIntRange(field string, value, min, max int) error {
	if value < min || value > max {
		return WithField(NewError(CodePara, "parameter %s=%d is outside [%d,%d]", field, value, min, max), field)
	}
	return nil
}

func (p Params) Validate(width, height int) error {
	if err := validateIntRange("sample_cols", p.SampleCols, 4, 64); err != nil {
		return err
	}
	if err := validateIntRange("sample_rows", p.SampleRows, 1, 16); err != nil {
		return err
	}
	if err := validateRange("watermark_threshold", p.WatermarkThreshold, 0.05, 0.95); err != nil {
		return err
	}
	if err := validateRange("confidence_threshold", p.ConfidenceThreshold, 0.0, 1.0); err != nil {
		return err
	}
	if err := validateIntRange("search_radius", p.SearchRadius, 0, 12); err != nil {
		return err
	}
	if err := validateIntRange("alignment_tolerance", p.AlignmentTolerance, 0, 8); err != nil {
		return err
	}
	if err := validateIntRange("residual_window", p.ResidualWindow, 1, 16); err != nil {
		return err
	}
	if err := validateRange("residual_z_scale", p.ResidualZScale, 0.1, 100); err != nil {
		return err
	}
	maxBlock := width
	if height < maxBlock {
		maxBlock = height
	}
	if err := validateIntRange("block_size", p.BlockSize, 4, maxBlock); err != nil {
		return err
	}
	if p.BlockSize > width/2 || p.BlockSize > height/2 {
		return WithField(NewError(CodePara, "parameter block_size=%d must not exceed half frame dimensions %dx%d", p.BlockSize, width, height), "block_size")
	}
	if err := validateIntRange("noise_window", p.NoiseWindow, 1, 16); err != nil {
		return err
	}
	if err := validateRange("noise_z_threshold", p.NoiseZThreshold, 0.1, 20); err != nil {
		return err
	}
	if err := validateIntRange("edge_bins", p.EdgeBins, 2, 12); err != nil {
		return err
	}
	if err := validateRange("edge_anomaly_threshold", p.EdgeAnomalyThreshold, 0, 1); err != nil {
		return err
	}
	if err := validateRange("watermark_weight", p.WatermarkWeight, 0, 1); err != nil {
		return err
	}
	if err := validateRange("residual_weight", p.ResidualWeight, 0, 1); err != nil {
		return err
	}
	if err := validateRange("noise_weight", p.NoiseWeight, 0, 1); err != nil {
		return err
	}
	if p.WatermarkWeight+p.ResidualWeight+p.NoiseWeight <= 0 {
		return WithField(NewError(CodePara, "fusion weights must sum to a positive value"), "verdict_threshold")
	}
	if err := validateRange("verdict_threshold", p.VerdictThreshold, 0.01, 0.99); err != nil {
		return err
	}
	if err := validateIntRange("frame_tolerance", p.FrameTolerance, 0, 4); err != nil {
		return err
	}
	if err := validateIntRange("max_parallel", p.MaxParallel, 1, 64); err != nil {
		return err
	}
	if err := validateIntRange("max_pixels_per_worker", p.MaxPixelsPerWorker, width*height, 1<<30); err != nil {
		return err
	}
	if err := validateIntRange("batch_size", p.BatchSize, 1, 1024); err != nil {
		return err
	}
	return nil
}
