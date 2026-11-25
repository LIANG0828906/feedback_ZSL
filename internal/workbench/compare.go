package workbench

import (
	"context"
	"encoding/json"
	"errors"

	"ballistic/internal/kernel"
	"ballistic/internal/snapshot"
)

var ErrForcedStop = errors.New("forced termination boundary after durable tile commits")

func CloneScene(scene *kernel.Scene) (*kernel.Scene, error) {
	data, err := json.Marshal(scene)
	if err != nil {
		return nil, err
	}
	var clone kernel.Scene
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func (w *Workbench) FreshFullRun(ctx context.Context) (*Workbench, error) {
	clone, err := CloneScene(w.Scene)
	if err != nil {
		return nil, err
	}
	freshStore, err := snapshot.Open(":memory:")
	if err != nil {
		return nil, err
	}
	full, err := FromScene(clone, w.Camera, freshStore, "full-reference")
	if err != nil {
		_ = freshStore.Close()
		return nil, err
	}
	if err := full.RunFull(ctx); err != nil {
		_ = freshStore.Close()
		return nil, err
	}
	return full, nil
}

func CompareRuns(full, incremental *Workbench) kernel.FrameComparison {
	pixel := kernel.ComparePixels(incremental.Frame, full.Frame, full.Camera)
	tiles := kernel.CompareTileResults(full.Results, incremental.Results, full.Mapping.TileOrder)

	pixel.TileMetricsEqual = tiles.TileMetricsEqual
	pixel.HashEqual = tiles.HashEqual
	pixel.HitStatsEqual = tiles.HitStatsEqual
	if tiles.FirstMismatchTile != "" && pixel.FirstMismatchTile == "" {
		pixel.FirstMismatchTile = tiles.FirstMismatchTile
	}
	if tiles.FirstMetricMismatch != "" {
		pixel.FirstMetricMismatch = tiles.FirstMetricMismatch
	}
	if !tiles.PixelEqual {
		pixel.PixelEqual = false
	}
	return pixel
}

func (w *Workbench) RunFullThenStopAfter(ctx context.Context, commitCount int) (int, error) {
	if err := w.Rebuild(); err != nil {
		return 0, err
	}
	results, err := w.renderTiles(ctx, w.Mapping.TileOrder)
	if err != nil {
		return 0, err
	}
	w.Results = results
	committed := 0
	if w.Store != nil {
		for order, tileID := range w.Mapping.TileOrder {
			if committed >= commitCount {
				break
			}
			if err := w.commit(ctx, tileID, order, results[tileID]); err != nil {
				return committed, err
			}
			committed++
		}
	}
	frame, assembleErr := kernel.AssembleFrame(w.Mapping, results)
	if assembleErr == nil {
		w.Frame = frame
	}
	return committed, ErrForcedStop
}
