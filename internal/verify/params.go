package verify

import (
	"encoding/json"

	"screenverify/internal/model"
	"screenverify/internal/rverr"
)

func (p Params) Validate(width, height, frameCount int) error {
	if p.GridX < 1 || p.GridX > 64 {
		return rverr.Field(rverr.EPara, "grid_x", "grid_x must be in [1,64]")
	}
	if p.GridY < 1 || p.GridY > 64 {
		return rverr.Field(rverr.EPara, "grid_y", "grid_y must be in [1,64]")
	}
	if p.GridX*p.GridY < 32 {
		return rverr.Field(rverr.EPara, "grid_x", "grid_x*grid_y must provide at least 32 sampling points")
	}
	if !validUnit(p.WatermarkThreshold) {
		return rverr.Field(rverr.EPara, "watermark_threshold", "watermark_threshold must be in [0,1]")
	}
	if p.ResidualWindow < 1 || p.ResidualWindow > 16 || p.ResidualWindow > frameCount {
		return rverr.Field(rverr.EPara, "residual_window", "residual_window is out of bounds")
	}
	if p.BlockSize <= 0 {
		return rverr.Field(rverr.EPara, "block_size", "block_size must be positive")
	}
	if width%p.BlockSize != 0 || height%p.BlockSize != 0 {
		return rverr.Field(rverr.EPara, "block_size", "block_size must divide frame width and height")
	}
	if !validUnit(p.TamperThreshold) {
		return rverr.Field(rverr.EPara, "tamper_threshold", "tamper_threshold must be in [0,1]")
	}
	if p.ResidualZThreshold < 0 || p.ResidualZThreshold > 20 {
		return rverr.Field(rverr.EPara, "residual_z_threshold", "residual_z_threshold must be in [0,20]")
	}
	if p.NoiseZThreshold < 0 || p.NoiseZThreshold > 100 {
		return rverr.Field(rverr.EPara, "noise_z_threshold", "noise_z_threshold must be in [0,100]")
	}
	if !validUnit(p.EdgeThreshold) {
		return rverr.Field(rverr.EPara, "edge_threshold", "edge_threshold must be in [0,1]")
	}
	if p.FrameTolerance < 0 || p.FrameTolerance > frameCount {
		return rverr.Field(rverr.EPara, "frame_tolerance", "frame_tolerance is out of bounds")
	}
	if p.BatchFrames <= 0 || p.BatchFrames > frameCount {
		return rverr.Field(rverr.EPara, "batch_frames", "batch_frames is out of bounds")
	}
	if p.WatermarkRegion != nil {
		r := *p.WatermarkRegion
		if r.X < 0 || r.Y < 0 || r.Width <= 0 || r.Height <= 0 {
			return rverr.Field(rverr.EPara, "watermark_region", "watermark_region has invalid geometry")
		}
	}
	return nil
}

func (r RuntimeConfig) Validate(width, height int) error {
	if r.Parallelism < 1 || r.Parallelism > 64 {
		return rverr.Field(rverr.EPara, "parallelism", "parallelism must be in [1,64]")
	}
	if r.BufferPixelsPerThread < int64(width*height) {
		return rverr.Field(rverr.EPara, "buffer_pixels_per_thread", "buffer limit must hold at least one frame pixel buffer")
	}
	return nil
}

func validUnit(v float64) bool {
	return v >= 0 && v <= 1
}

func ParamVersion(p Params) string {
	key, _, _ := model.HashJSON(p)
	return key[:16]
}

func relevantParams(t EvidenceType, p Params) map[string]any {
	switch t {
	case EvWatermark:
		return map[string]any{
			"grid_x":              p.GridX,
			"grid_y":              p.GridY,
			"watermark_threshold": p.WatermarkThreshold,
			"watermark_region":    p.WatermarkRegion,
		}
	case EvResidual:
		return map[string]any{"residual_window": p.ResidualWindow}
	case EvNoise:
		return map[string]any{"block_size": p.BlockSize}
	case EvEdge:
		return map[string]any{"bins": 8}
	case EvTimeline:
		return map[string]any{"continuity": "plus_one_v1"}
	case EvDecision:
		return map[string]any{
			"watermark_threshold":  p.WatermarkThreshold,
			"tamper_threshold":     p.TamperThreshold,
			"residual_z_threshold": p.ResidualZThreshold,
			"noise_z_threshold":    p.NoiseZThreshold,
			"edge_threshold":       p.EdgeThreshold,
		}
	default:
		return map[string]any{"all": p}
	}
}

func canonicalKeyPayload(contentHashes []string, t EvidenceType, p Params, upstream []string) []byte {
	payload := map[string]any{
		"algorithm":      model.AlgorithmVersion,
		"content_hashes": contentHashes,
		"evidence_type":  t,
		"params":         relevantParams(t, p),
		"upstream_keys":  upstream,
	}
	b, _ := json.Marshal(payload)
	return b
}

func CacheKey(contentHashes []string, t EvidenceType, p Params, upstream []string) string {
	return model.HashBytes("evidence-cache-key-v1:", canonicalKeyPayload(contentHashes, t, p, upstream))
}

func SourceKey(frame *model.Frame) string {
	return "source:" + frame.ContentHash
}
