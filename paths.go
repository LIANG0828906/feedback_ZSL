package fog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

type PathReport struct {
	Mode             string            `json:"mode"`
	DBPath           string            `json:"db_path,omitempty"`
	Tiles            []TileResult      `json:"tiles"`
	Sequence         []TileCoord       `json:"sequence"`
	ReusedTiles      []TileCoord       `json:"reused_tiles,omitempty"`
	RecomputedTiles  []TileCoord       `json:"recomputed_tiles,omitempty"`
	StaleCheckpoints []StaleCheckpoint `json:"stale_checkpoints,omitempty"`
	Scheduler        *SchedulerReport  `json:"scheduler,omitempty"`
}

type ThreeWayReport struct {
	Realtime  PathReport `json:"realtime"`
	Resume    PathReport `json:"resume"`
	Full      PathReport `json:"full"`
	Equal     bool       `json:"equal"`
	FirstDiff *Error     `json:"first_diff,omitempty"`
}

func RenderFreshPath(ctx context.Context, scene *Scene, store *CheckpointStore, scheduler *Scheduler, mode string, options IntegrationOptions) (PathReport, error) {
	tiles := TileGrid(scene.Viewport, scene.Recipe)
	report := scheduler.Run(ctx, scene, tiles, options)
	if first := firstSchedulerFailure(report.Failures); first != nil {
		return PathReport{Mode: mode, Scheduler: &report}, first
	}
	ordered := OrderedTiles(report.Tiles)
	for _, tile := range ordered {
		tile := tile
		if err := store.CommitTile(ctx, tile); err != nil {
			return PathReport{Mode: mode}, err
		}
	}
	sequence, err := store.CheckpointSequence(ctx)
	if err != nil {
		return PathReport{Mode: mode}, err
	}
	return PathReport{Mode: mode, DBPath: store.Path(), Tiles: ordered, Sequence: sequence, RecomputedTiles: tiles, Scheduler: &report}, nil
}

func ResumePath(ctx context.Context, scene *Scene, store *CheckpointStore, scheduler *Scheduler, options IntegrationOptions) (PathReport, error) {
	allTiles := TileGrid(scene.Viewport, scene.Recipe)
	loaded := map[string]TileResult{}
	reused := []TileCoord{}
	missing := []TileCoord{}
	stale := []StaleCheckpoint{}
	for _, coord := range allTiles {
		result, staleReason, err := store.LoadValidTile(ctx, scene, coord)
		if err != nil {
			return PathReport{Mode: "resume"}, err
		}
		if staleReason != nil {
			stale = append(stale, *staleReason)
		}
		if result != nil {
			loaded[tileKey(coord)] = *result
			reused = append(reused, coord)
		} else {
			missing = append(missing, coord)
		}
	}

	var schedulerReport SchedulerReport
	if len(missing) > 0 {
		schedulerReport = scheduler.Run(ctx, scene, missing, options)
		if first := firstSchedulerFailure(schedulerReport.Failures); first != nil {
			return PathReport{Mode: "resume", StaleCheckpoints: stale, Scheduler: &schedulerReport}, first
		}
		for _, result := range schedulerReport.Tiles {
			loaded[tileKey(result.Tile)] = result
		}
	}

	ordered := make([]TileResult, 0, len(allTiles))
	for _, coord := range allTiles {
		result, ok := loaded[tileKey(coord)]
		if !ok {
			e := newError(CodeBlock, "tile %v is missing after resume scheduling", coord)
			e.Tile = &coord
			return PathReport{Mode: "resume"}, e
		}
		ordered = append(ordered, result)
	}
	ordered = OrderedTiles(ordered)
	for _, tile := range ordered {
		tile := tile
		if err := store.CommitTile(ctx, tile); err != nil {
			return PathReport{Mode: "resume"}, err
		}
	}
	sequence, err := store.CheckpointSequence(ctx)
	if err != nil {
		return PathReport{Mode: "resume"}, err
	}
	return PathReport{
		Mode: "resume", DBPath: store.Path(), Tiles: ordered, Sequence: sequence,
		ReusedTiles: reused, RecomputedTiles: missing, StaleCheckpoints: stale,
		Scheduler: &schedulerReport,
	}, nil
}

func RunThreeWayConsistency(ctx context.Context, scene *Scene, dir string, config SchedulerConfig) (*ThreeWayReport, error) {
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	options := IntegrationOptions{}
	realtimeStore, err := OpenCheckpointStore(filepath.Join(dir, "realtime.db"))
	if err != nil {
		return nil, err
	}
	realtimeScheduler := NewScheduler(config)
	realtime, err := RenderFreshPath(ctx, scene, realtimeStore, realtimeScheduler, "realtime", options)
	_ = realtimeStore.Close()
	if err != nil {
		return nil, err
	}

	fullStore, err := OpenCheckpointStore(filepath.Join(dir, "full-recompute.db"))
	if err != nil {
		return nil, err
	}
	fullScheduler := NewScheduler(config)
	full, err := RenderFreshPath(ctx, scene, fullStore, fullScheduler, "full_recompute", options)
	_ = fullStore.Close()
	if err != nil {
		return nil, err
	}

	resumePath := filepath.Join(dir, "resume.db")
	seedStore, err := OpenCheckpointStore(resumePath)
	if err != nil {
		return nil, err
	}
	seedScheduler := NewScheduler(config)
	_, err = RenderFreshPath(ctx, scene, seedStore, seedScheduler, "checkpoint_seed", options)
	_ = seedStore.Close()
	if err != nil {
		return nil, err
	}
	staleTile := TileCoord{X: 1, Y: 1}
	if !tileInGrid(staleTile, scene) {
		staleTile = TileCoord{X: 0, Y: 0}
	}
	if err := CorruptCheckpointForTest(resumePath, staleTile, "content_hash", "stale-hash-before-restart"); err != nil {
		return nil, err
	}
	resumeStore, err := OpenCheckpointStore(resumePath)
	if err != nil {
		return nil, err
	}
	resumeScheduler := NewScheduler(config)
	resume, err := ResumePath(ctx, scene, resumeStore, resumeScheduler, options)
	_ = resumeStore.Close()
	if err != nil {
		return nil, err
	}

	report := &ThreeWayReport{Realtime: realtime, Resume: resume, Full: full}
	report.FirstDiff = CompareThreeWay(report, scene.Tolerances.CheckpointPixel)
	report.Equal = report.FirstDiff == nil
	return report, nil
}

func CompareThreeWay(report *ThreeWayReport, pixelTolerance float64) *Error {
	if !equalTileCoords(report.Realtime.Sequence, report.Resume.Sequence) {
		e := diffError(TileCoord{}, "commit_sequence", report.Realtime.Sequence, report.Resume.Sequence, report.Full.Sequence, "realtime and resume commit sequences differ")
		return e
	}
	if !equalTileCoords(report.Realtime.Sequence, report.Full.Sequence) {
		e := diffError(TileCoord{}, "commit_sequence", report.Realtime.Sequence, report.Resume.Sequence, report.Full.Sequence, "realtime and full commit sequences differ")
		return e
	}
	rt := tileMap(report.Realtime.Tiles)
	rs := tileMap(report.Resume.Tiles)
	fu := tileMap(report.Full.Tiles)
	for _, coord := range report.Realtime.Sequence {
		a, okA := rt[tileKey(coord)]
		b, okB := rs[tileKey(coord)]
		c, okC := fu[tileKey(coord)]
		if !okA || !okB || !okC {
			return diffError(coord, "tile_presence", okA, okB, okC, "tile missing from one path")
		}
		if e := CompareTileResults(coord, &a, &b, pixelTolerance); e != nil {
			e.Value = map[string]any{"realtime": e.Value, "resume": nil, "full": nil}
			return e
		}
		if e := CompareTileResults(coord, &a, &c, pixelTolerance); e != nil {
			return e
		}
	}
	return nil
}

func CompareTileResults(coord TileCoord, expected, actual *TileResult, pixelTolerance float64) *Error {
	if expected.CacheKey != actual.CacheKey {
		return diffError(coord, "cache_key", expected.CacheKey, actual.CacheKey, nil, "cache key mismatch")
	}
	if expected.ContentHash != actual.ContentHash {
		return diffError(coord, "content_hash", expected.ContentHash, actual.ContentHash, nil, "content hash mismatch")
	}
	if expected.RecipeVersion != actual.RecipeVersion {
		return diffError(coord, "recipe_version", expected.RecipeVersion, actual.RecipeVersion, nil, "recipe version mismatch")
	}
	if expected.AlgoVersion != actual.AlgoVersion {
		return diffError(coord, "algo_version", expected.AlgoVersion, actual.AlgoVersion, nil, "algorithm version mismatch")
	}
	if expected.RaySegmentDigest != actual.RaySegmentDigest {
		return diffError(coord, "ray_segment_digest", expected.RaySegmentDigest, actual.RaySegmentDigest, nil, "ray segment digest mismatch")
	}
	if floatAbs(expected.Transmittance-actual.Transmittance) > 1e-12 {
		return diffError(coord, "transmittance", expected.Transmittance, actual.Transmittance, nil, "tile transmittance mismatch")
	}
	if floatAbs(expected.Scattering-actual.Scattering) > 1e-12 {
		return diffError(coord, "scattering", expected.Scattering, actual.Scattering, nil, "tile scattering mismatch")
	}
	if len(expected.Pixels) != len(actual.Pixels) {
		return diffError(coord, "pixels.length", len(expected.Pixels), len(actual.Pixels), nil, "pixel count mismatch")
	}
	actualPixels := map[string]RenderPixel{}
	for _, pixel := range actual.Pixels {
		actualPixels[fmt.Sprintf("%d,%d", pixel.X, pixel.Y)] = pixel
	}
	for _, ep := range expected.Pixels {
		ap, ok := actualPixels[fmt.Sprintf("%d,%d", ep.X, ep.Y)]
		if !ok {
			e := diffError(coord, "pixel.coordinate", ep, nil, nil, "pixel missing")
			e.Pixel = &PixelCoord{X: ep.X, Y: ep.Y}
			return e
		}
		if ep.X != ap.X || ep.Y != ap.Y {
			e := diffError(coord, "pixel.coordinate", [2]int{ep.X, ep.Y}, [2]int{ap.X, ap.Y}, nil, "pixel coordinate mismatch")
			e.Pixel = &PixelCoord{X: ep.X, Y: ep.Y}
			return e
		}
		for channel := 0; channel < 3; channel++ {
			delta := floatAbs(ep.Radiance[channel] - ap.Radiance[channel])
			if delta > pixelTolerance {
				e := diffError(coord, fmt.Sprintf("pixel(%d,%d).radiance[%d]", ep.X, ep.Y, channel), ep.Radiance[channel], ap.Radiance[channel], nil, "pixel radiance mismatch")
				e.Pixel = &PixelCoord{X: ep.X, Y: ep.Y}
				e.Differences = [3]float64{
					floatAbs(ep.Radiance[0] - ap.Radiance[0]),
					floatAbs(ep.Radiance[1] - ap.Radiance[1]),
					floatAbs(ep.Radiance[2] - ap.Radiance[2]),
				}
				return e
			}
		}
		if ep.Color != ap.Color {
			e := diffError(coord, fmt.Sprintf("pixel(%d,%d).color", ep.X, ep.Y), ep.Color, ap.Color, nil, "pixel byte color mismatch")
			e.Pixel = &PixelCoord{X: ep.X, Y: ep.Y}
			e.Differences = [3]float64{
				floatAbs(float64(int(ep.Color[0]) - int(ap.Color[0]))),
				floatAbs(float64(int(ep.Color[1]) - int(ap.Color[1]))),
				floatAbs(float64(int(ep.Color[2]) - int(ap.Color[2]))),
			}
			return e
		}
	}
	return nil
}

func diffError(tile TileCoord, field string, expected, actual, third any, message string) *Error {
	e := newError(CodeDiff, "%s: field=%s", message, field)
	e.Tile = &tile
	e.FieldName = field
	e.Field = field
	e.Value = map[string]any{"expected_or_realtime": expected, "actual_or_resume": actual, "full": third}
	return e
}

func firstSchedulerFailure(failures map[string]*Error) *Error {
	for _, err := range failures {
		return err
	}
	return nil
}

func tileMap(tiles []TileResult) map[string]TileResult {
	out := map[string]TileResult{}
	for _, tile := range tiles {
		out[tileKey(tile.Tile)] = tile
	}
	return out
}

func equalTileCoords(a, b []TileCoord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func tileInGrid(tile TileCoord, scene *Scene) bool {
	for _, candidate := range TileGrid(scene.Viewport, scene.Recipe) {
		if candidate == tile {
			return true
		}
	}
	return false
}

func ensureDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

func floatAbs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
