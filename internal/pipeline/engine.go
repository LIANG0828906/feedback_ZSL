// Package pipeline wires sampling, estimation, reconstruction, heatmaps,
// dependency invalidation, bounded concurrency and incremental rebuilding.
package pipeline

import (
	"fmt"
	"sync"

	"collectionrecon/internal/errors"
	"collectionrecon/internal/model"
)

// Limits declares the concurrency and per-worker pixel buffer ceilings.
type Limits struct {
	MaxWorkers      int
	MaxPixelBuffers int
}

// DefaultLimits is the conservative local configuration.
func DefaultLimits() Limits {
	return Limits{MaxWorkers: 2, MaxPixelBuffers: 4096}
}

// QueueEvent records why a zone task waited for a pixel buffer slot.
type QueueEvent struct {
	ZoneID int    `json:"zoneId"`
	Reason string `json:"reason"`
}

// ZoneFailure isolates a single zone's panic or error from other zones.
type ZoneFailure struct {
	ZoneID int
	Err    error
}

// Engine executes the pipeline with bounded resources.
type Engine struct {
	region      *model.Region
	limits      Limits
	pixelTokens chan struct{}
	queueLog    []QueueEvent
	mu          sync.Mutex
}

// NewEngine constructs an engine for one region instance.
func NewEngine(region *model.Region, limits Limits) *Engine {
	return &Engine{
		region:      region,
		limits:      limits,
		pixelTokens: make(chan struct{}, limits.MaxPixelBuffers),
	}
}

// QueueLog returns a copy of buffer-queue events recorded during execution.
func (e *Engine) QueueLog() []QueueEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := append([]QueueEvent(nil), e.queueLog...)
	return out
}

// ComputeZone runs the full deterministic chain for one zone and assembles its
// cached result. It is safe to call from worker goroutines.
func (e *Engine) ComputeZone(zoneID int) (model.ZoneResult, error) {
	region := e.region
	zone := findZone(region, zoneID)

	cells, err := SelectSampleCells(region, zoneID)
	if err != nil {
		return model.ZoneResult{}, err
	}
	est, err := EstimateDensity(region, zoneID, cells)
	if err != nil {
		return model.ZoneResult{}, err
	}
	points, err := ReconstructPoints(region, est)
	if err != nil {
		return model.ZoneResult{}, err
	}

	needed := heatTileCount(region, zoneID)
	tiles := make([]model.HeatTile, 0, needed)
	ix0 := int((zone.Bounds.MinX - region.BBox.MinX) / region.RasterCell)
	ix1 := int((zone.Bounds.MaxX - region.BBox.MinX) / region.RasterCell)
	for ix := ix0; ix < ix1; ix++ {
		for iy := 0; iy < region.RasterNY; iy++ {
			// Each tile claims one bounded slot; under parallel load a small
			// pool blocks here (with a logged reason) instead of dropping work.
			e.acquireBuffer(zoneID)
			tiles = append(tiles, HeatTileAt(region, zoneID, points, ix, iy))
			e.releaseBuffer()
		}
	}

	result := model.ZoneResult{
		ZoneID:       zoneID,
		CellIndices:  append([]int(nil), est.CellIndices...),
		Estimate:     est,
		Points:       points,
		PointDigest:  PointsDigest(points),
		HeatTiles:    tiles,
		HeatDigest:   HeatDigest(tiles),
		ContentHash:  ZoneContentHash(region, zoneID),
		ParamVersion: zone.ParamVersion,
		AlgoVersion:  AlgoVersion,
	}
	return result, nil
}

// acquireBuffer blocks until a pixel-buffer slot is free. When the pool is
// exhausted the wait is recorded with its reason rather than dropping work.
func (e *Engine) acquireBuffer(zoneID int) {
	select {
	case e.pixelTokens <- struct{}{}:
	default:
		e.mu.Lock()
		e.queueLog = append(e.queueLog, QueueEvent{
			ZoneID: zoneID,
			Reason: fmt.Sprintf("pixel buffer pool exhausted (%d/%d); blocking for slot",
				cap(e.pixelTokens), cap(e.pixelTokens)),
		})
		e.mu.Unlock()
		e.pixelTokens <- struct{}{}
	}
}

func (e *Engine) releaseBuffer() {
	<-e.pixelTokens
}

// RunParallel computes all zone ids with a fixed-size worker pool. A failing
// zone is reported and retried once; other zones and committed results stay
// untouched. Results are returned in ascending zone id regardless of timing.
func (e *Engine) RunParallel(zoneIDs []int) ([]model.ZoneResult, []ZoneFailure, error) {
	jobs := make(chan int)
	var wg sync.WaitGroup
	var failMu sync.Mutex
	failures := make([]ZoneFailure, 0)
	results := make(map[int]model.ZoneResult)
	var resMu sync.Mutex

	worker := func() {
		defer wg.Done()
		for zoneID := range jobs {
			result, err := e.computeWithRetry(zoneID)
			if err != nil {
				failMu.Lock()
				failures = append(failures, ZoneFailure{ZoneID: zoneID, Err: err})
				failMu.Unlock()
				continue
			}
			resMu.Lock()
			results[zoneID] = result
			resMu.Unlock()
		}
	}
	for w := 0; w < e.limits.MaxWorkers; w++ {
		wg.Add(1)
		go worker()
	}
	for _, id := range zoneIDs {
		jobs <- id
	}
	close(jobs)
	wg.Wait()

	if len(failures) > 0 {
		return nil, failures, errors.WithContext(errors.EZone,
			map[string]any{"failedZones": failures},
			"%d zone(s) failed and were isolated", len(failures))
	}
	ordered := make([]model.ZoneResult, 0, len(zoneIDs))
	for _, id := range zoneIDs {
		ordered = append(ordered, results[id])
	}
	return ordered, nil, nil
}

func (e *Engine) computeWithRetry(zoneID int) (result model.ZoneResult, err error) {
	for attempt := 0; attempt < 2; attempt++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("zone %d panic: %v", zoneID, r)
				}
			}()
			result, err = e.ComputeZone(zoneID)
		}()
		if err == nil {
			return result, nil
		}
	}
	return model.ZoneResult{}, err
}

func heatTileCount(region *model.Region, zoneID int) int {
	zone := findZone(region, zoneID)
	ix0 := int((zone.Bounds.MinX - region.BBox.MinX) / region.RasterCell)
	ix1 := int((zone.Bounds.MaxX - region.BBox.MinX) / region.RasterCell)
	return (ix1 - ix0) * region.RasterNY
}
