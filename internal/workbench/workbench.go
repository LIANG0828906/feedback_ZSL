package workbench

import (
	"context"
	"fmt"

	"ballistic/internal/kernel"
	"ballistic/internal/snapshot"
)

const DefaultFrameID = "diagnostic-frame"

type Workbench struct {
	Seed      int64
	FrameID   string
	Scene     *kernel.Scene
	Camera    kernel.Camera
	Pool      *kernel.BoundedPool
	Store     *snapshot.Store
	FailTiles map[string]bool

	Report  *kernel.PredictionReport
	Mapping *kernel.Mapping
	Results map[string]*kernel.TileResult
	Frame   []byte

	LastPlan        *InvalidationPlan
	Comparison      *kernel.FrameComparison
	ReusedTileIDs   []string
	RenderedTileIDs []string
}

func New(seed int64, store *snapshot.Store, frameID string) (*Workbench, error) {
	sample, err := kernel.GenerateSampleSet(seed)
	if err != nil {
		return nil, err
	}
	if frameID == "" {
		frameID = DefaultFrameID
	}
	wb := &Workbench{
		Seed:      seed,
		FrameID:   frameID,
		Scene:     sample.Scene,
		Camera:    sample.Camera,
		Pool:      kernel.NewBoundedPool(2, 256*1024),
		Store:     store,
		FailTiles: map[string]bool{},
	}
	if err := wb.Rebuild(); err != nil {
		return nil, err
	}
	return wb, nil
}

func FromScene(scene *kernel.Scene, camera kernel.Camera, store *snapshot.Store, frameID string) (*Workbench, error) {
	if frameID == "" {
		frameID = DefaultFrameID
	}
	wb := &Workbench{
		Seed:      scene.Seed,
		FrameID:   frameID,
		Scene:     scene,
		Camera:    camera,
		Pool:      kernel.NewBoundedPool(2, 256*1024),
		Store:     store,
		FailTiles: map[string]bool{},
	}
	if err := wb.Rebuild(); err != nil {
		return nil, err
	}
	return wb, nil
}

func (w *Workbench) Rebuild() error {
	report, err := kernel.Predict(w.Scene)
	if err != nil {
		return err
	}
	mapping, err := kernel.BuildMapping(w.Scene, report, w.Camera)
	if err != nil {
		return err
	}
	w.Report = report
	w.Mapping = mapping
	return nil
}

func (w *Workbench) Key(tileID string) snapshot.Key {
	return snapshot.Key{
		FrameID:               w.FrameID,
		TileID:                tileID,
		ContentHash:           w.Mapping.TileHash(tileID),
		SceneStructureVersion: fmt.Sprintf("structure-v%d", w.Scene.StructureVersion),
		SemanticVersionKey:    w.SemanticVersionKey(),
		CameraVersion:         w.Camera.CameraVersionKey(),
		AlgorithmVersion:      kernel.AlgorithmVersion,
	}
}

func (w *Workbench) SemanticVersionKey() string {
	v := w.Scene.Versions
	return fmt.Sprintf("ep%d-n%d-r%d-g%d-t%d-m%d",
		v.Endpoints, v.Normals, v.Restitutions, v.Gravity, v.TimeStep, v.Materials)
}

func (w *Workbench) RunFull(ctx context.Context) error {
	if err := w.Rebuild(); err != nil {
		return err
	}
	results, err := w.renderTiles(ctx, w.Mapping.TileOrder)
	if err != nil {
		return err
	}
	w.Results = results
	if w.Store != nil {
		for order, tileID := range w.Mapping.TileOrder {
			if err := w.commit(ctx, tileID, order, results[tileID]); err != nil {
				return err
			}
		}
	}
	frame, err := kernel.AssembleFrame(w.Mapping, results)
	if err != nil {
		return err
	}
	w.Frame = frame
	w.RenderedTileIDs = append([]string{}, w.Mapping.TileOrder...)
	w.ReusedTileIDs = nil
	return nil
}

func (w *Workbench) RunResume(ctx context.Context) error {
	if err := w.Rebuild(); err != nil {
		return err
	}
	return w.runWithInvalidSet(ctx, map[string]bool{})
}

func (w *Workbench) RunIncremental(ctx context.Context, invalid map[string]bool) error {
	if err := w.Rebuild(); err != nil {
		return err
	}
	return w.runWithInvalidSet(ctx, invalid)
}

func (w *Workbench) runWithInvalidSet(ctx context.Context, invalid map[string]bool) error {
	if w.Results == nil {
		w.Results = map[string]*kernel.TileResult{}
	}
	reused := []string{}
	toRender := []string{}
	for _, tileID := range w.Mapping.TileOrder {
		if invalid[tileID] {
			toRender = append(toRender, tileID)
			continue
		}
		if w.Store != nil {
			loaded, err := w.Store.LoadValidTile(ctx, w.Key(tileID), w.Camera)
			if err != nil {
				return err
			}
			if loaded != nil {
				w.Results[tileID] = loaded
				reused = append(reused, tileID)
				continue
			}
		}
		toRender = append(toRender, tileID)
	}

	rendered, err := w.renderTiles(ctx, toRender)
	if err != nil {
		return err
	}
	for tileID, result := range rendered {
		w.Results[tileID] = result
	}
	w.refreshResultMetrics()
	if w.Store != nil {
		for order, tileID := range w.Mapping.TileOrder {
			result := w.Results[tileID]
			if result == nil {
				return fmt.Errorf("tile %s has no result", tileID)
			}
			if !containsString(toRender, tileID) {
				continue
			}
			if err := w.commit(ctx, tileID, order, result); err != nil {
				return err
			}
		}
	}
	frame, err := kernel.AssembleFrame(w.Mapping, w.Results)
	if err != nil {
		return err
	}
	w.Frame = frame
	w.ReusedTileIDs = reused
	w.RenderedTileIDs = append([]string{}, toRender...)
	return nil
}

func (w *Workbench) refreshResultMetrics() {
	total := w.Mapping.TotalPolylinePixels
	for tileID, result := range w.Results {
		tm := w.Mapping.Tiles[tileID]
		if tm == nil || result == nil {
			continue
		}
		result.ContentHash = tm.ContentHash
		result.CoverageBp = tm.CoverageBp
		result.ObstacleCoverageBp = tm.ObstacleCoverageBp
		result.PolylineCoverageBp = tm.PolylineCoverageBp
		result.ObstaclePixelCount = tm.ObstaclePixelCount
		result.PolylinePixelCount = tm.PolylinePixelCount
		if total > 0 {
			result.PolylineShareBp = tm.PolylinePixelCount * 10000 / total
		}
	}
}

func (w *Workbench) renderTiles(ctx context.Context, tileIDs []string) (map[string]*kernel.TileResult, error) {
	jobs := make([]kernel.TileJob, 0, len(tileIDs))
	for _, tileID := range tileIDs {
		id := tileID
		tm := w.Mapping.Tiles[id]
		estimate := w.Camera.TileW*w.Camera.TileH*12 + 4096
		if tm != nil {
			estimate += (len(tm.SegmentIDs) + len(tm.RequestIDs) + len(tm.BounceMarkers)) * 128
		}
		jobs = append(jobs, kernel.TileJob{
			TileID:        id,
			EstimateBytes: estimate,
			Run: func(_ []byte) (*kernel.TileResult, error) {
				return kernel.RenderTile(w.Mapping, id, w.FailTiles)
			},
		})
	}
	jobResults := w.Pool.Run(ctx, jobs)
	results := map[string]*kernel.TileResult{}
	for _, jobResult := range jobResults {
		if jobResult.Err != nil {
			return nil, jobResult.Err
		}
		results[jobResult.TileID] = jobResult.Result
	}
	return results, nil
}

func (w *Workbench) commit(ctx context.Context, tileID string, order int, result *kernel.TileResult) error {
	if w.Store == nil {
		return nil
	}
	key := w.Key(tileID)
	return w.Store.CommitTile(ctx, snapshot.CommitInput{
		Key:                key,
		CommitOrder:        order,
		ColorDigest:        result.ColorDigest,
		DepthDigest:        result.DepthDigest,
		CoverageBp:         result.CoverageBp,
		ObstacleCoverageBp: result.ObstacleCoverageBp,
		PolylineCoverageBp: result.PolylineCoverageBp,
		PolylineShareBp:    result.PolylineShareBp,
		BounceCount:        result.BounceCount,
		FirstHitCount:      result.FirstHitMarkerCount,
		Color:              result.Color,
		Depth:              result.Depth,
	})
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
