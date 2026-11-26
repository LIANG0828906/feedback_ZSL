package maskqa

import (
	"image"
	"sync"
)

// Workbench is the stateful incremental QC component.
type Workbench struct {
	mu               sync.Mutex
	Project          *Project
	Options          SchedulerOptions
	Dir              string
	Store            *CheckpointStore
	Graph            *DependencyGraph
	Last             *ComputeResult
	LastInvalidation *InvalidationPlan
	RecoveryReport   RecoveryReport
}

// NewWorkbench opens the local checkpoint directory and builds the graph.
func NewWorkbench(project *Project, dir string, options SchedulerOptions) (*Workbench, error) {
	if options.Parallelism == 0 {
		options = DefaultSchedulerOptions()
	}
	var store *CheckpointStore
	var recovery RecoveryReport
	var err error
	if dir != "" {
		store, recovery, err = OpenCheckpointStore(dir)
		if err != nil {
			return nil, err
		}
	}
	return &Workbench{
		Project: project, Options: options, Dir: dir, Store: store,
		Graph: BuildDependencyGraph(project), RecoveryReport: recovery,
	}, nil
}

// Close releases checkpoint locks.
func (w *Workbench) Close() error {
	if w == nil || w.Store == nil {
		return nil
	}
	return w.Store.Close()
}

// InitializeFull performs and persists the first full calculation.
func (w *Workbench) InitializeFull() (*ComputeResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	result, err := ComputeFull(w.Project, w.Options, w.Store)
	w.Last = result
	w.Graph = BuildDependencyGraph(w.Project)
	return result, err
}

// Edit applies one mutation and performs precise incremental recomputation.
func (w *Workbench) Edit(edit Edit) (*ComputeResult, *InvalidationPlan, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	plan, err := ApplyEdit(w.Project, edit)
	if err != nil {
		return nil, nil, err
	}
	if w.Last == nil {
		full, fullErr := ComputeFull(w.Project, w.Options, w.Store)
		w.Last = full
		if fullErr != nil {
			return full, plan, fullErr
		}
	}

	invalidSet := map[TileCoord]bool{}
	for _, coord := range plan.InvalidTiles {
		invalidSet[coord] = true
	}
	prior := make([]TileResult, len(w.Last.Tiles))
	copy(prior, w.Last.Tiles)
	if w.Store != nil {
		for i := range prior {
			if invalidSet[prior[i].Coord] {
				continue
			}
			record, ok := w.Store.LatestTile(prior[i].Coord)
			if ok && len(record.TileAfter) > 0 {
				prior[i].Pixels = append([]byte(nil), record.TileAfter...)
			}
		}
	}

	result, computeErr := computeTiles(w.Project, w.Options, w.Store, invalidSet, prior)
	if result != nil {
		result.Invalidation = plan
	}
	w.Last = result
	w.LastInvalidation = plan
	w.Graph = BuildDependencyGraph(w.Project)
	return result, plan, computeErr
}

// Recover reconstructs the last committed result from local files.
func (w *Workbench) Recover() (*ComputeResult, RecoveryReport, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.Store == nil {
		return nil, w.RecoveryReport, decodeError("checkpoint_dir", "no checkpoint store configured")
	}
	if w.RecoveryReport.Action == RecoveryRebuild {
		result, err := ComputeFull(w.Project, w.Options, w.Store)
		w.Last = result
		return result, w.RecoveryReport, err
	}

	tiles := Tiles(w.Project.Width, w.Project.Height, w.Project.tileSize())
	mask := image.NewGray(image.Rect(0, 0, w.Project.Width, w.Project.Height))
	result := &ComputeResult{
		ProjectID: w.Project.ID, Width: w.Project.Width, Height: w.Project.Height,
		TileSize: w.Project.tileSize(), Budget: NewScheduler(w.Options).Snapshot(),
	}
	allRecords := w.Store.Records()
	recordsByTile := map[TileCoord][]StrokeCommitRecord{}
	for _, record := range allRecords {
		coord := TileCoord{TX: record.TileX, TY: record.TileY}
		recordsByTile[coord] = append(recordsByTile[coord], record)
	}

	for i, tile := range tiles {
		coord := tile.Coord
		records := recordsByTile[coord]
		var tileResult TileResult
		if latest, ok := w.Store.LatestTile(coord); ok && len(latest.TileAfter) == tile.PixelCount {
			pixels := append([]byte(nil), latest.TileAfter...)
			cacheKey := latest.TileCacheKey
			if cacheKey == "" {
				cacheKey = latest.CacheKey
			}
			ids := make([]string, 0, len(records))
			basis := make([]string, 0, len(records))
			for _, record := range records {
				ids = append(ids, record.StrokeID)
				basis = append(basis, OrderStrokes(w.Project.Strokes)[record.OrderIndex].Reason)
			}
			tileResult = TileResult{
				Coord: coord, CacheKey: cacheKey, InputHash: latest.InputHash,
				ContentHash: ContentDigest(pixels), StrokeIDs: ids, OrderBasis: basis,
				Pixels: pixels, Reused: true, StrokeCommits: len(records),
			}
		} else {
			computation, err := computeTile(w.Project, tile, OrderStrokes(w.Project.Strokes),
				make([]byte, tile.PixelCount))
			if err != nil {
				return nil, w.RecoveryReport, err
			}
			tileResult = computation.TileResult
		}
		result.Tiles = append(result.Tiles, tileResult)
		result.ReusedTiles = append(result.ReusedTiles, coord)
		copyTileToMask(mask, tile, tileResult.Pixels)
		result.CommitSequence = append(result.CommitSequence, CommitIdentity{
			LogicalSequence: i + 1, Tile: coord, CacheKey: tileResult.CacheKey,
			ContentHash: tileResult.ContentHash, StrokeCommits: tileResult.StrokeCommits,
		})
	}
	result.Accumulated = mask
	report, verifyErr := VerifyMask(w.Project, mask)
	result.Report = report
	w.Last = result
	return result, w.RecoveryReport, verifyErr
}

// RecoverOrRebuild is the restart boundary used by callers.
func (w *Workbench) RecoverOrRebuild() (*ComputeResult, error) {
	result, recovery, err := w.Recover()
	if recovery.Action == RecoveryRebuild && err == nil {
		return w.Last, nil
	}
	return result, err
}
