package sample

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"cadpipe/internal/cad"
	"cadpipe/internal/render"
	"cadpipe/internal/tessellate"
)

type analyticObservation struct {
	tileID int
	line   string
	depth  float64
	probe  string
}

func buildTileExpectations(cam *render.Camera, scene *cad.Scene, truth Truth) TileExpectations {
	byTile := make(map[int]*TileExpectation)
	var observations []analyticObservation

	for _, point := range truth.SurfacePoints {
		patch := scene.Patch(point.PatchID)
		screen, ok := cam.Project(tessellate.Vertex{
			Position: cad.Vec3{X: point.X, Y: point.Y, Z: point.Z},
			UV:       cad.Vec2{U: point.U, V: point.V},
		})
		if !ok {
			continue
		}
		tileID := screenTile(cam, screen.ScreenX, screen.ScreenY)
		if tileID < 0 {
			continue
		}
		exp := byTile[tileID]
		if exp == nil {
			exp = &TileExpectation{TileID: tileID, MinDepthM: math.Inf(1), MaxDepthM: math.Inf(-1)}
			byTile[tileID] = exp
		}
		exp.SampleCount++
		if patch != nil && cad.PointInsideClip(patch.Clip, cad.Vec2{U: point.U, V: point.V}) {
			exp.InsideSampleCount++
		}
		exp.MinDepthM = math.Min(exp.MinDepthM, screen.Depth)
		exp.MaxDepthM = math.Max(exp.MaxDepthM, screen.Depth)
		line := fmt.Sprintf("S %s %.6f %.6f px %.4f %.4f d_nm %d",
			point.PatchID, point.U, point.V, screen.ScreenX, screen.ScreenY,
			int64(math.Round(screen.Depth*1e9)))
		observations = append(observations, analyticObservation{tileID: tileID, line: line, depth: screen.Depth})
	}

	for _, probe := range truth.ClipProbes {
		patch := scene.Patch(probe.PatchID)
		if patch == nil {
			continue
		}
		tileID, ok := render.ProbeTile(cam, patch, probe.U, probe.V)
		if !ok {
			continue
		}
		exp := byTile[tileID]
		if exp == nil {
			exp = &TileExpectation{TileID: tileID, MinDepthM: math.Inf(1), MaxDepthM: math.Inf(-1)}
			byTile[tileID] = exp
		}
		exp.ProbeIDs = append(exp.ProbeIDs, probe.ID)
		p := tessellate.Evaluate(patch, probe.U, probe.V)
		screen, _ := cam.Project(tessellate.Vertex{Position: p, UV: cad.Vec2{U: probe.U, V: probe.V}})
		line := fmt.Sprintf("P %s inside=%t px %.4f %.4f", probe.ID, probe.Inside, screen.ScreenX, screen.ScreenY)
		observations = append(observations, analyticObservation{tileID: tileID, line: line, probe: probe.ID})
	}

	ids := make([]int, 0, len(byTile))
	for id := range byTile {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	tiles := make([]TileExpectation, 0, len(ids))
	for _, id := range ids {
		exp := byTile[id]
		sort.Strings(exp.ProbeIDs)
		var lines []string
		for _, ob := range observations {
			if ob.tileID == id {
				lines = append(lines, ob.line)
			}
		}
		sort.Strings(lines)
		exp.ExpectedPixelSummary = cad.SHA256Hex([]byte(strings.Join(lines, "\n")))
		if math.IsInf(exp.MinDepthM, 1) {
			exp.MinDepthM = 0
			exp.MaxDepthM = 0
		}
		tiles = append(tiles, *exp)
	}
	return TileExpectations{Width: cam.Width, Height: cam.Height, TileSize: cam.TileWidth, Tiles: tiles}
}

func screenTile(cam *render.Camera, x, y float64) int {
	if x < 0 || x >= float64(cam.Width) || y < 0 || y >= float64(cam.Height) {
		return -1
	}
	tx := int(x) / cam.TileWidth
	ty := int(y) / cam.TileHeight
	return cam.TileID(tx, ty)
}
