package colortrace

import "fmt"

func ChunkCoordAt(x, y, chunkSize int) ChunkCoord {
	if chunkSize <= 0 {
		chunkSize = 1
	}
	return ChunkCoord{X: x / chunkSize, Y: y / chunkSize}
}

func ChunkRegion(chunk ChunkCoord, width, height, chunkSize int) Region {
	if chunkSize <= 0 {
		chunkSize = width
	}
	r := Region{
		MinX: chunk.X * chunkSize,
		MinY: chunk.Y * chunkSize,
		MaxX: (chunk.X + 1) * chunkSize,
		MaxY: (chunk.Y + 1) * chunkSize,
	}
	if r.MaxX > width {
		r.MaxX = width
	}
	if r.MaxY > height {
		r.MaxY = height
	}
	if r.MinX > width {
		r.MinX = width
	}
	if r.MinY > height {
		r.MinY = height
	}
	return r
}

func ChunkGrid(width, height, chunkSize int) (cols, rows int) {
	if chunkSize <= 0 {
		chunkSize = 1
	}
	cols = (width + chunkSize - 1) / chunkSize
	rows = (height + chunkSize - 1) / chunkSize
	return cols, rows
}

func AllChunks(width, height, chunkSize int) []ChunkCoord {
	cols, rows := ChunkGrid(width, height, chunkSize)
	out := make([]ChunkCoord, 0, cols*rows)
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			out = append(out, ChunkCoord{X: x, Y: y})
		}
	}
	return out
}

func PlaneAt(sample *Sample, name string) (*PlaneSample, error) {
	plane := sample.Planes[name]
	if plane == nil {
		return nil, schemaError(fmt.Sprintf("planes.%s", name), "plane missing in stage %d", sample.Stage)
	}
	return plane, nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func planeIndex(width, x, y int) int { return y*width + x }
