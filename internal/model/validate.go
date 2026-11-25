package model

import "edgerefine/internal/errs"

func ValidateParams(p Params, width, height int) error {
	if p.TileSize < 8 || p.TileSize > 256 {
		return errs.Para("tile_size", p.TileSize, [2]any{8, 256})
	}
	if width%p.TileSize != 0 {
		return errs.Para("width", width, [2]any{"multiple of tile_size", p.TileSize})
	}
	if height%p.TileSize != 0 {
		return errs.Para("height", height, [2]any{"multiple of tile_size", p.TileSize})
	}
	if p.IterBudget < 1 || p.IterBudget > 64 {
		return errs.Para("iter_budget", p.IterBudget, [2]any{1, 64})
	}
	if p.Radius < 0 || p.Radius > 12 {
		return errs.Para("radius", p.Radius, [2]any{0, 12})
	}
	if p.PreviewDelayMS < 1 || p.PreviewDelayMS > 5000 {
		return errs.Para("preview_delay_ms", p.PreviewDelayMS, [2]any{1, 5000})
	}
	if p.BitDepth != 8 {
		return errs.Para("bit_depth", p.BitDepth, [2]any{8, 8})
	}
	if p.BGQuantile <= 0 || p.BGQuantile >= 0.5 {
		return errs.Para("bg_quantile", p.BGQuantile, [2]any{"(0,0.5)", nil})
	}
	if p.FGQuantile <= 0.5 || p.FGQuantile >= 1 {
		return errs.Para("fg_quantile", p.FGQuantile, [2]any{"(0.5,1)", nil})
	}
	if p.ConvergeTolerance <= 0 || p.ConvergeTolerance > 10 {
		return errs.Para("converge_tolerance_px", p.ConvergeTolerance, [2]any{"(0,10]", nil})
	}
	if p.EdgeTolerance <= 0 || p.EdgeUpperBound < p.EdgeTolerance {
		return errs.Para("edge_tolerance_px", p.EdgeTolerance, [2]any{"(0,edge_upper_bound]", nil})
	}
	return nil
}

func ValidateHistogram(h []int, bitDepth int) error {
	want := 1 << bitDepth
	if bitDepth != 8 || len(h) != want {
		return errs.Para("histogram", len(h), [2]any{want, want})
	}
	sum := 0
	for _, v := range h {
		if v < 0 {
			return errs.Para("histogram", "negative bin", [2]any{0, "non-negative"})
		}
		sum += v
	}
	if sum == 0 {
		return errs.Para("histogram", "empty", [2]any{"non-empty tile histogram", nil})
	}
	return nil
}
