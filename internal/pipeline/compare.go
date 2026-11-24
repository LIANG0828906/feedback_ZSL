package pipeline

import (
	"fmt"
	"math"
	"reflect"

	"cadpipe/internal/cad"
	"cadpipe/internal/render"
)

func CompareTileMaps(want, got map[int]*render.TileResult, tileCount int) error {
	for id := 0; id < tileCount; id++ {
		w := want[id]
		g := got[id]
		if w == nil || g == nil {
			missing := "want"
			if g == nil {
				missing = "got"
			}
			return cad.NewError(cad.CodeDiff, "tile %d missing in %s", id, missing).
				WithTilePixel(id, 0, 0).WithField("tile")
		}
		if w.ContentHash != g.ContentHash {
			return diffError(id, "content_hash", 0, 0, w.ContentHash, g.ContentHash)
		}
		if w.Coverage != g.Coverage {
			return diffError(id, "coverage", 0, 0, w.Coverage, g.Coverage)
		}
		if w.OutlineCoverage != g.OutlineCoverage {
			return diffError(id, "outline_coverage", 0, 0, w.OutlineCoverage, g.OutlineCoverage)
		}
		if !reflect.DeepEqual(w.LightStats, g.LightStats) {
			return diffError(id, "light_stats", 0, 0, w.LightStats, g.LightStats)
		}
		if len(w.Color) != len(g.Color) {
			return diffError(id, "color_length", 0, 0, len(w.Color), len(g.Color))
		}
		tileX, tileY := 0, 0
		if g != nil {
			tileX, tileY = g.X*g.Width, g.Y*g.Height
		}
		for i := 0; i < len(w.Color); i++ {
			if w.Color[i] != g.Color[i] {
				pixel := i / 4
				lx := pixel % g.Width
				ly := pixel / g.Width
				channel := []string{"r", "g", "b", "a"}[i%4]
				err := diffError(id, "color."+channel, tileX+lx, tileY+ly, w.Color[i], g.Color[i])
				err.Pixel = &[2]int{lx, ly}
				err.Details["global_pixel"] = [2]int{tileX + lx, tileY + ly}
				return err
			}
		}
		if len(w.Depth) != len(g.Depth) {
			return diffError(id, "depth_length", 0, 0, len(w.Depth), len(g.Depth))
		}
		for i := 0; i < len(w.Depth); i++ {
			if math.Float64bits(w.Depth[i]) != math.Float64bits(g.Depth[i]) {
				lx := i % g.Width
				ly := i / g.Width
				err := diffError(id, "depth", tileX+lx, tileY+ly, w.Depth[i], g.Depth[i])
				err.Pixel = &[2]int{lx, ly}
				err.Details["global_pixel"] = [2]int{tileX + lx, tileY + ly}
				return err
			}
		}
		if w.ColorDigest != g.ColorDigest {
			return diffError(id, "color_digest", 0, 0, w.ColorDigest, g.ColorDigest)
		}
		if w.DepthDigest != g.DepthDigest {
			return diffError(id, "depth_digest", 0, 0, w.DepthDigest, g.DepthDigest)
		}
	}
	return nil
}

func diffError(tileID int, field string, x, y int, want, got any) *cad.Error {
	e := cad.NewError(cad.CodeDiff, "pixel/stat mismatch at tile %d field %s: want=%v got=%v",
		tileID, field, want, got)
	e.TileID = tileID
	e.Field = field
	e.Pixel = &[2]int{x, y}
	e.Details = map[string]any{"want": want, "got": got}
	if x == 0 && y == 0 && (field == "coverage" || field == "content_hash" || field == "color_digest" || field == "depth_digest" || field == "outline_coverage" || field == "light_stats") {
		e.Pixel = nil
	}
	return e
}

func (p *Pipeline) CompareWith(other *Pipeline) error {
	if p.Camera.TileCount() != other.Camera.TileCount() {
		return cad.NewError(cad.CodeDiff, "tile count differs: %d vs %d", p.Camera.TileCount(), other.Camera.TileCount()).
			WithField("tile_count")
	}
	if err := CompareTileMaps(other.Cache, p.Cache, p.Camera.TileCount()); err != nil {
		if ce, ok := cad.AsError(err); ok {
			ce.Message = fmt.Sprintf("incremental/full comparison failed: %s", ce.Message)
		}
		return err
	}
	return nil
}
