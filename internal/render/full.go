package render

import (
	"context"
	"fmt"
	"math"
	"time"

	"cadlab/internal/model"
)

func RenderFull(ctx context.Context, scene *Scene, path string) (*Frame, error) {
	start := time.Now()
	if err := validateScene(scene); err != nil {
		return nil, err
	}
	tiles := make([]TileResult, 0, scene.Tiles.Count())
	for id := 0; id < scene.Tiles.Count(); id++ {
		t, err := RenderTile(scene, id, RenderOptions{Context: ctx, FailTile: -1})
		if err != nil {
			return nil, err
		}
		t.Path = path
		tiles = append(tiles, t)
	}
	frame := NewFrame(scene, path, time.Since(start), tiles)
	frame.Rendered = len(tiles)
	return frame, nil
}

func validateScene(s *Scene) *model.Diagnostic {
	if s == nil {
		return model.Diag(model.CodeStage, "missing render scene")
	}
	if s.Camera.Width <= 0 || s.Camera.Height <= 0 {
		return model.Diag(model.CodePara, "invalid camera viewport")
	}
	if s.Tiles.Cols <= 0 || s.Tiles.Rows <= 0 || s.Tiles.Width != s.Camera.Width || s.Tiles.Height != s.Camera.Height {
		return model.Diag(model.CodePara, "tile grid does not match camera viewport")
	}
	if s.Camera.FovY <= 0 || s.Camera.FovY >= 180 || s.Camera.Near <= 0 || s.Camera.Far <= s.Camera.Near {
		return model.Diag(model.CodePara, "invalid camera intrinsics")
	}
	return nil
}

func CompareFrames(a, b *Frame) *model.Diagnostic {
	if a == nil || b == nil {
		return model.Diag(model.CodeDiff, "missing frame for comparison")
	}
	if a.Version != b.Version {
		return &model.Diagnostic{
			Code: model.CodeVersion, Message: "frame versions differ",
			PatchID: -1, TileID: -1, VersionA: a.Version, VersionB: b.Version,
			Field: "snapshotVersion",
		}
	}
	if a.Width != b.Width || a.Height != b.Height {
		return model.Diag(model.CodeDiff, "frame dimensions differ")
	}
	if a.Image == nil || b.Image == nil {
		return model.Diag(model.CodeDiff, "frame image missing")
	}
	for y := 0; y < a.Height; y++ {
		for x := 0; x < a.Width; x++ {
			o := y*a.Image.Stride + x*4
			if a.Image.Pix[o] != b.Image.Pix[o] ||
				a.Image.Pix[o+1] != b.Image.Pix[o+1] ||
				a.Image.Pix[o+2] != b.Image.Pix[o+2] {
				tile := tileAt(a.Grid, x, y)
				return &model.Diagnostic{
					Code: model.CodeDiff, Message: "pixel mismatch",
					PatchID: -1, TileID: tile, Field: "RGBA",
					FirstPixel: &model.PixelPoint{X: x, Y: y},
				}
			}
			d := math.Abs(float64(a.Depth[y*a.Width+x]) - float64(b.Depth[y*a.Width+x]))
			if d > 1e-7 && !(math.IsInf(float64(a.Depth[y*a.Width+x]), 1) && math.IsInf(float64(b.Depth[y*a.Width+x]), 1)) {
				tile := tileAt(a.Grid, x, y)
				return &model.Diagnostic{
					Code: model.CodeDiff, Message: "depth mismatch",
					PatchID: -1, TileID: tile, Field: "depth",
					FirstPixel: &model.PixelPoint{X: x, Y: y},
				}
			}
		}
	}
	for id := 0; id < a.Grid.Count(); id++ {
		ta, okA := a.TileByID(id)
		tb, okB := b.TileByID(id)
		if !okA || !okB {
			return model.DiagTile(model.CodeDiff, -1, id, "tile", "missing tile statistic")
		}
		if ta.Stats.SurfacePixels != tb.Stats.SurfacePixels {
			return model.DiagTile(model.CodeDiff, -1, id, "coverage", "silhouette coverage differs")
		}
		if ta.Stats.Coverage != tb.Stats.Coverage {
			return model.DiagTile(model.CodeDiff, -1, id, "coverage", "silhouette coverage ratio differs")
		}
		if ta.Stats.LuminanceBins != tb.Stats.LuminanceBins {
			return model.DiagTile(model.CodeDiff, -1, id, "luminanceBins", "shading histogram differs")
		}
	}
	return nil
}

func tileAt(g model.TileGrid, x, y int) int {
	col := x * g.Cols / g.Width
	row := y * g.Rows / g.Height
	if col >= g.Cols {
		col = g.Cols - 1
	}
	if row >= g.Rows {
		row = g.Rows - 1
	}
	return row*g.Cols + col
}

func DiffSummary(a, b *Frame) (string, *model.Diagnostic) {
	diag := CompareFrames(a, b)
	if diag == nil {
		return fmt.Sprintf("identical: %d pixels, %d tiles", a.Width*a.Height, a.Grid.Count()), nil
	}
	return diag.Error(), diag
}
