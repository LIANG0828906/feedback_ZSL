package mesh

import (
	"math/bits"

	"terrainpatch/internal/model"
)

func PreferredDepth(t model.Tile) (int, *model.AppError) {
	if t.Grid.Width < 2 || t.Grid.Height < 2 {
		return 0, model.NewError(model.CodeMeta, "grid must contain at least 2x2 vertices").WithTile(t.ID, "grid")
	}
	if t.Grid.Width != t.Grid.Height {
		return 0, model.NewError(model.CodeMeta, "recursive sample grid must be square").WithTile(t.ID, "grid.width")
	}
	cells := t.Grid.Width - 1
	if cells&(cells-1) != 0 {
		return 0, model.NewError(model.CodeMeta, "grid cell count must be a power of two").WithTile(t.ID, "grid.width")
	}
	if t.SampleBudget <= 0 {
		return 0, model.NewError(model.CodePara, "sampleBudget must be positive").WithTile(t.ID, "sampleBudget").WithActual(t.SampleBudget)
	}
	maxGeomDepth := bits.TrailingZeros64(uint64(cells))
	depth := 0
	for depth < maxGeomDepth {
		next := depth + 1
		n := 1 << next
		vertices := (n + 1) * (n + 1)
		if vertices > t.SampleBudget {
			break
		}
		depth = next
	}
	return depth, nil
}

func Build(tile model.Tile) (*model.Mesh, *model.AppError) {
	maxBudgetDepth, err := PreferredDepth(tile)
	if err != nil {
		return nil, err
	}
	if tile.Tolerance < 0 {
		return nil, model.NewError(model.CodePara, "tolerance must be non-negative").WithTile(tile.ID, "tolerance").WithActual(tile.Tolerance)
	}
	if tile.MaxTriangles <= 0 {
		return nil, model.NewError(model.CodeMeta, "maxTriangles must be positive").WithTile(tile.ID, "maxTriangles").WithActual(tile.MaxTriangles)
	}

	var selected *model.Mesh
	for depth := 0; depth <= maxBudgetDepth; depth++ {
		candidate := buildDepth(tile, depth)
		errs := evaluateErrors(tile, candidate)
		candidate.TriangleErrors = errs
		for _, te := range errs {
			if te.MaxAbsError > candidate.MaxError {
				candidate.MaxError = te.MaxAbsError
			}
		}
		selected = candidate
		if candidate.MaxError <= tile.Tolerance {
			break
		}
	}

	if len(selected.Triangles) > tile.MaxTriangles {
		return nil, model.NewError(
			model.CodeBudget,
			"triangle count exceeds declared maxTriangles",
		).WithTile(tile.ID, "maxTriangles").
			WithActual(len(selected.Triangles)).
			WithLimit(tile.MaxTriangles).
			WithDetails(map[string]any{
				"depth":        selected.Depth,
				"vertices":     len(selected.Vertices),
				"sampleBudget": tile.SampleBudget,
			})
	}
	return selected, nil
}

func buildDepth(tile model.Tile, depth int) *model.Mesh {
	n := 1 << depth
	cellCount := tile.Grid.Width - 1
	gridStep := cellCount / n
	stride := n + 1

	vertices := make([]model.Vertex, 0, stride*stride)
	for iy := 0; iy <= n; iy++ {
		gy := iy * gridStep
		for ix := 0; ix <= n; ix++ {
			gx := ix * gridStep
			vertices = append(vertices, model.Vertex{
				ID: iy*stride + ix,
				X:  tile.OriginX + int64(gx)*tile.Grid.CellSize,
				Y:  tile.OriginY + int64(gy)*tile.Grid.CellSize,
				Z:  tile.Grid.Heights[gy][gx],
			})
		}
	}

	triangles := make([]model.Triangle, 0, 2*n*n)
	id := 0
	for iy := 0; iy < n; iy++ {
		for ix := 0; ix < n; ix++ {
			v00 := iy*stride + ix
			v10 := v00 + 1
			v01 := (iy+1)*stride + ix
			v11 := v01 + 1
			triangles = append(triangles,
				model.Triangle{ID: id, V0: v00, V1: v10, V2: v11},
				model.Triangle{ID: id + 1, V0: v00, V1: v11, V2: v01},
			)
			id += 2
		}
	}

	return &model.Mesh{
		TileID:       tile.ID,
		ParamVersion: tile.ParamVersion,
		Algorithm:    model.AlgorithmVersion,
		Depth:        depth,
		Vertices:     vertices,
		Triangles:    triangles,
	}
}

type point struct {
	x, y int64
}

func cross(ax, ay, bx, by int64) int64 {
	return ax*by - ay*bx
}

func evaluateErrors(tile model.Tile, m *model.Mesh) []model.TriangleError {
	byID := make(map[int]model.Vertex, len(m.Vertices))
	for _, v := range m.Vertices {
		byID[v.ID] = v
	}
	out := make([]model.TriangleError, 0, len(m.Triangles))
	for _, tri := range m.Triangles {
		a := byID[tri.V0]
		b := byID[tri.V1]
		c := byID[tri.V2]
		denSigned := cross(b.X-a.X, b.Y-a.Y, c.X-a.X, c.Y-a.Y)
		orientation := int64(1)
		den := denSigned
		if denSigned < 0 {
			orientation = -1
			den = -denSigned
		}
		te := model.TriangleError{TriangleID: tri.ID}
		if den == 0 {
			te.MaxAbsError = -1
			out = append(out, te)
			continue
		}

		minGX, maxGX := gridIndex(tile.OriginX, tile.Grid.CellSize, min3(a.X, b.X, c.X)), gridIndex(tile.OriginX, tile.Grid.CellSize, max3(a.X, b.X, c.X))
		minGY, maxGY := gridIndex(tile.OriginY, tile.Grid.CellSize, min3(a.Y, b.Y, c.Y)), gridIndex(tile.OriginY, tile.Grid.CellSize, max3(a.Y, b.Y, c.Y))
		for gy := minGY; gy <= maxGY; gy++ {
			for gx := minGX; gx <= maxGX; gx++ {
				px := tile.OriginX + int64(gx)*tile.Grid.CellSize
				py := tile.OriginY + int64(gy)*tile.Grid.CellSize
				pa := cross(b.X-px, b.Y-py, c.X-px, c.Y-py) * orientation
				pb := cross(c.X-px, c.Y-py, a.X-px, a.Y-py) * orientation
				pc := cross(a.X-px, a.Y-py, b.X-px, b.Y-py) * orientation
				if pa < 0 || pb < 0 || pc < 0 {
					continue
				}
				h := tile.Grid.Heights[gy][gx]
				weighted := pa*a.Z + pb*b.Z + pc*c.Z
				diff := h*den - weighted
				if diff < 0 {
					diff = -diff
				}
				err := (diff + den - 1) / den
				te.SampleCount++
				te.L1Error += err
				if err > te.MaxAbsError {
					te.MaxAbsError = err
				}
			}
		}
		out = append(out, te)
	}
	return out
}

func gridIndex(origin, cellSize, coord int64) int {
	if cellSize == 0 {
		return 0
	}
	return int((coord - origin) / cellSize)
}

func min3(a, b, c int64) int64 {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

func max3(a, b, c int64) int64 {
	if a > b {
		if a > c {
			return a
		}
		return c
	}
	if b > c {
		return b
	}
	return c
}
