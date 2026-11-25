package iconmask

import "context"

// RefineBlockwise performs the same alternating close/open rounds independently
// on fixed blocks. A halo wider than the maximum possible round-to-round
// propagation is read for each block; only the block core is committed. This is
// the actual block iteration used by the consistency paths, not a post-hoc crop.
func RefineBlockwise(sample ImageSample, cfg RepairConfig) (*RepairResult, *Error) {
	return RefineBlockwiseParallel(context.Background(), sample, cfg, DefaultParallelConfig())
}

func RefineBlockwiseParallel(
	ctx context.Context,
	sample ImageSample,
	cfg RepairConfig,
	parallelCfg ParallelConfig,
) (*RepairResult, *Error) {
	reference := sample.Truth.ReferenceMask
	segments := sample.Truth.Segments
	if cfg.Shape == "" {
		cfg.Shape = ShapeSquare
	}
	if cfg.BlockSize <= 0 {
		return nil, parameterError(sample.ID, "blockSize", "block size must be a positive integer", "positive integer")
	}
	table, err := ResolveRadii(sample.ID, segments, cfg.Radius)
	if err != nil {
		return nil, err
	}
	edge, err := LocateEdgeBand(sample.ID, sample.InitialMask, segments, EdgeConfig{
		Shape: cfg.Shape, RadiusBandMin: cfg.Radius.Min,
	})
	if err != nil {
		return nil, err
	}
	result := &RepairResult{
		ImageID:     sample.ID,
		Mask:        sample.InitialMask.Clone(),
		EdgeBand:    edge.Band,
		EdgeEmpty:   edge.Empty,
		RadiusTable: table,
		Status:      "ready",
	}
	if edge.Empty {
		result.Status = "empty_edge_band"
		result.Success = false
		result.Stats = AnalyzeMask(result.Mask)
		result.Accuracy = ComputeAccuracy(reference, result.Mask)
		return result, nil
	}
	// The global pass establishes the deterministic convergence budget and
	// per-round telemetry. The pixel result below is rebuilt block by block.
	global, globalErr := RefineSample(sample, cfg)
	if globalErr != nil {
		return global, globalErr
	}
	result.Rounds = global.Rounds
	result.Converged = global.Converged
	result.Success = global.Success
	result.RoundMasks = global.RoundMasks
	rounds := len(global.Rounds)
	assignment := assignBandToSegments(edge.Band, segments)
	halo := cfg.Radius.Max*maxInt(1, rounds) + cfg.Radius.Min + 2
	final := NewMask(sample.Width, sample.Height)
	rects := blockRects(sample.Width, sample.Height, cfg.BlockSize)
	tasks := make([]ParallelTask, len(rects))
	for i, core := range rects {
		read := expandRect(core, halo, sample.Width, sample.Height)
		tasks[i] = ParallelTask{
			Index:      i,
			Block:      Coord{X: core.X / cfg.BlockSize, Y: core.Y / cfg.BlockSize},
			Rect:       core,
			PixelCount: read.Width * read.Height,
		}
	}
	schedule, runErr := RunParallel(ctx, tasks, parallelCfg, func(ctx context.Context, task ParallelTask, buffer *PixelBuffer) error {
		core := task.Rect
		read := expandRect(core, halo, sample.Width, sample.Height)
		local := cropMask(sample.InitialMask, read)
		localBand := cropMask(edge.Band, read)
		localAssignment := cropInts(assignment, sample.Width, read)
		for round := 0; round < rounds; round++ {
			closeResult := local.Clone()
			if _, morphErr := applySegmentedMorphology(
				sample.ID, closeResult, local, localBand, localAssignment,
				segments, table, cfg.Shape, true,
			); morphErr != nil {
				return morphErr
			}
			openResult := closeResult.Clone()
			if _, morphErr := applySegmentedMorphology(
				sample.ID, openResult, closeResult, localBand, localAssignment,
				segments, table, cfg.Shape, false,
			); morphErr != nil {
				return morphErr
			}
			local = openResult
		}
		if len(buffer.Data) < core.Width*core.Height {
			return parameterError(sample.ID, "pixelBufferBudget", "worker buffer is too small for block", "buffer >= block area")
		}
		for cy := 0; cy < core.Height; cy++ {
			for cx := 0; cx < core.Width; cx++ {
				value := local.At(cx+core.X-read.X, cy+core.Y-read.Y)
				buffer.Data[cy*core.Width+cx] = value
				final.Set(core.X+cx, core.Y+cy, value)
			}
		}
		return nil
	})
	if runErr != nil {
		if domainErr, ok := runErr.(*Error); ok {
			return nil, domainErr
		}
		return nil, &Error{Code: CodeState, ImageID: sample.ID, Field: "parallel", Message: runErr.Error()}
	}
	result.BlockSchedule = &schedule
	result.Mask = final
	result.OutsideChanged = outsideBandChanges(sample.InitialMask, final, edge.Band)
	result.Stats = AnalyzeMask(final)
	result.Accuracy = ComputeAccuracy(reference, final)
	result.Status = "converged"
	if result.OutsideChanged != 0 {
		result.Status = "outside_band_mutation"
		result.Success = false
		return result, &Error{
			Code: CodeState, ImageID: sample.ID, Field: "edgeBand",
			Message:   "blockwise repair changed pixels outside the declared edge band",
			Remaining: result.OutsideChanged,
		}
	}
	return result, nil
}

func expandRect(rect Rect, margin, width, height int) Rect {
	x0 := maxInt(0, rect.X-margin)
	y0 := maxInt(0, rect.Y-margin)
	x1 := minInt(width, rect.X+rect.Width+margin)
	y1 := minInt(height, rect.Y+rect.Height+margin)
	return Rect{X: x0, Y: y0, Width: x1 - x0, Height: y1 - y0}
}

func cropInts(values []int, sourceWidth int, rect Rect) []int {
	out := make([]int, rect.Width*rect.Height)
	for y := 0; y < rect.Height; y++ {
		for x := 0; x < rect.Width; x++ {
			out[y*rect.Width+x] = values[(rect.Y+y)*sourceWidth+rect.X+x]
		}
	}
	return out
}
