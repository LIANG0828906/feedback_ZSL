package geom

import "fixedsampler/internal/model"

type ChunkGrid struct {
	Width     int
	Height    int
	ChunkSize int
	Cols      int
	Rows      int
}

func NewChunkGrid(width, height, chunkSize int) ChunkGrid {
	if chunkSize < 1 {
		chunkSize = 1
	}
	cols := (width + chunkSize - 1) / chunkSize
	rows := (height + chunkSize - 1) / chunkSize
	return ChunkGrid{Width: width, Height: height, ChunkSize: chunkSize, Cols: cols, Rows: rows}
}

func (g ChunkGrid) Count() int { return g.Cols * g.Rows }

func (g ChunkGrid) Index(cx, cy int) int { return cy*g.Cols + cx }

func (g ChunkGrid) Coords(index int) (int, int) {
	return index % g.Cols, index / g.Cols
}

func (g ChunkGrid) FullRect(cx, cy int) model.Rect {
	x0 := cx * g.ChunkSize
	y0 := cy * g.ChunkSize
	x1 := min(g.Width, x0+g.ChunkSize)
	y1 := min(g.Height, y0+g.ChunkSize)
	return model.Rect{X0: x0, Y0: y0, X1: x1, Y1: y1}
}

func FloorDiv(v, d int) int {
	if d <= 0 {
		panic("geom: positive divisor required")
	}
	q := v / d
	r := v % d
	if r != 0 && ((v < 0) != (d < 0)) {
		q--
	}
	return q
}

func MapFullRectToPlane(r model.Rect, subsampleX, subsampleY, cumulativeX, cumulativeY int) model.Rect {
	dx := max(1, subsampleX*cumulativeX)
	dy := max(1, subsampleY*cumulativeY)
	return model.Rect{
		X0: FloorDiv(r.X0, dx),
		Y0: FloorDiv(r.Y0, dy),
		X1: FloorDiv(r.X1, dx),
		Y1: FloorDiv(r.Y1, dy),
	}
}

func BoxSupport(output model.Rect, factorX, factorY int) model.Rect {
	if output.Empty() {
		return output
	}
	return model.Rect{
		X0: output.X0 * factorX,
		Y0: output.Y0 * factorY,
		X1: output.X1 * factorX,
		Y1: output.Y1 * factorY,
	}
}

func NearestSupport(output model.Rect, factorX, factorY int) model.Rect {
	if output.Empty() {
		return output
	}
	r := model.Rect{
		X0: output.X0*factorX + factorX/2,
		Y0: output.Y0*factorY + factorY/2,
		X1: 0,
		Y1: 0,
	}
	r.X1 = r.X0 + 1
	r.Y1 = r.Y0 + 1
	if output.Width() > 1 || output.Height() > 1 {
		endX := (output.X1-1)*factorX + factorX/2 + 1
		endY := (output.Y1-1)*factorY + factorY/2 + 1
		r.X1 = max(r.X1, endX)
		r.Y1 = max(r.Y1, endY)
		r.X0 = min(r.X0, output.X0*factorX)
		r.Y0 = min(r.Y0, output.Y0*factorY)
	}
	return r
}

func ChunksIntersectingRect(rect model.Rect, levelWidth, levelHeight, factorX, factorY, chunkSize int, subsampleX, subsampleY int) []int {
	_ = levelWidth
	_ = levelHeight
	fullX0 := rect.X0 * subsampleX * factorX
	fullY0 := rect.Y0 * subsampleY * factorY
	fullX1 := rect.X1 * subsampleX * factorX
	fullY1 := rect.Y1 * subsampleY * factorY
	cx0 := max(0, fullX0/chunkSize)
	cy0 := max(0, fullY0/chunkSize)
	cx1 := (fullX1 + chunkSize - 1) / chunkSize
	cy1 := (fullY1 + chunkSize - 1) / chunkSize
	var ids []int
	for cy := cy0; cy < cy1; cy++ {
		for cx := cx0; cx < cx1; cx++ {
			ids = append(ids, cy*ceilDiv(levelWidth, chunkSize)+cx)
		}
	}
	return ids
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }
