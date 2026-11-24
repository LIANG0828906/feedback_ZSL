package core

import (
	"fmt"
	"math"
)

func ValidateDetectorParams(p DetectorParams) error {
	checks := []struct {
		field string
		value float64
		low   float64
		high  float64
	}{
		{"tile_width", float64(p.TileWidth), 64, 4096},
		{"tile_height", float64(p.TileHeight), 64, 4096},
		{"band", float64(p.Band), 0, 2048},
		{"merge_iou", p.MergeIoU, 0, 1},
		{"merge_stroke_ratio", p.MergeStrokeRatio, 0, 1},
		{"min_component_area", float64(p.MinComponentArea), 1, 1000000},
		{"stroke_ratio_tolerance", p.StrokeRatioTolerance, 0, 10},
		{"min_component_pixels", float64(p.MinComponentPixels), 1, 1000000},
		{"adaptive_radius", float64(p.AdaptiveRadius), 1, 256},
		{"max_workers", float64(p.MaxWorkers), 1, 64},
		{"buffer_bytes_per_worker", float64(p.BufferBytesPerWorker), 1 << 16, 1 << 34},
	}
	for _, check := range checks {
		if math.IsNaN(check.value) || math.IsInf(check.value, 0) ||
			check.value < check.low || check.value > check.high {
			return FieldError(CodePara, check.field,
				fmt.Sprintf("[%g,%g]", check.low, check.high),
				"detector parameter is outside its legal interval")
		}
	}
	if RequiredBand(p) > p.Band {
		tileIndex := 0
		boundary := fmt.Sprintf("tile(0,0) vertical boundary x=%d", p.TileWidth)
		if p.TileWidth >= p.TileHeight {
			tileIndex = 1
			boundary = fmt.Sprintf("tile(0,0)/tile(0,1) vertical boundary x=%d", p.TileWidth)
		}
		return &Error{
			Code:      CodeBand,
			Field:     "band",
			TileIndex: tileIndex,
			Boundary:  boundary,
			Message:   fmt.Sprintf("band %d is smaller than the required overlap %d derived from merge_iou", p.Band, RequiredBand(p)),
		}
	}
	return nil
}

func ValidateSegmentParams(p SegmentParams) error {
	checks := []struct {
		field string
		value float64
		low   float64
		high  float64
	}{
		{"min_char_gap", p.MinCharGap, 0, 64},
		{"max_char_width", p.MaxCharWidth, 4, 1024},
		{"min_char_area", float64(p.MinCharArea), 1, 1000000},
		{"match_threshold", p.MatchThreshold, 0, 1},
		{"margin_scale", p.MarginScale, 0.01, 100},
	}
	for _, check := range checks {
		if math.IsNaN(check.value) || math.IsInf(check.value, 0) ||
			check.value < check.low || check.value > check.high {
			return FieldError(CodePara, check.field,
				fmt.Sprintf("[%g,%g]", check.low, check.high),
				"segmentation parameter is outside its legal interval")
		}
	}
	return nil
}
