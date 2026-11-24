package verifier

import (
	"fmt"

	"fixedsampler/internal/model"
)

func locatePoint(rows map[int]PersistedRow, seq int, plane string, x, y int) *model.DiffLocation {
	loc := &model.DiffLocation{StageSeq: seq}
	for _, idx := range sortedRowIndices(rows) {
		chunk := rows[idx].Artifact
		level := findLevel(chunk, seq)
		if level == nil {
			continue
		}
		for _, tile := range level.Tiles {
			if tile.Plane == plane && tile.Rect.Contains(x, y) {
				loc.ChunkIndex = chunk.ChunkIndex
				loc.ChunkX = chunk.ChunkX
				loc.ChunkY = chunk.ChunkY
				return loc
			}
		}
	}
	return loc
}

func checkBoundary(rows map[int]PersistedRow, boundary model.BlockBoundary, rp *refPlane) *model.DiffLocation {
	c := boundary.Coordinate
	switch boundary.Axis {
	case "x":
		if c <= 0 || c >= rp.width {
			loc := locatePoint(rows, boundary.AfterSeq, boundary.Plane, min(c, rp.width-1), 0)
			loc.Field = "block_boundaries." + boundary.ID + ".coordinate"
			loc.Expected = fmt.Sprintf("0 < x < %d", rp.width)
			loc.Actual = fmt.Sprint(c)
			return loc
		}
		var target int64
		maxDiff := int64(-1)
		maxCoord := -1
		for k := 1; k < rp.width; k++ {
			var sum int64
			for y := 0; y < rp.height; y++ {
				a := rp.values[y*rp.width+k-1]
				b := rp.values[y*rp.width+k]
				sum += abs64(b - a)
			}
			if sum > maxDiff {
				maxDiff = sum
				maxCoord = k
			}
			if k == c {
				target = sum
			}
		}
		if target == 0 || target < maxDiff {
			loc := locatePoint(rows, boundary.AfterSeq, boundary.Plane, c, rp.height/2)
			loc.Field = "block_boundaries." + boundary.ID + ".coordinate"
			loc.StageSeq = boundary.AfterSeq
			loc.Expected = fmt.Sprintf("tied/strongest_x_edge including=%d", c)
			loc.Actual = fmt.Sprintf("first_x_edge=%d max_diff=%d target_diff=%d", maxCoord, maxDiff, target)
			return loc
		}
	case "y":
		if c <= 0 || c >= rp.height {
			loc := locatePoint(rows, boundary.AfterSeq, boundary.Plane, 0, min(c, rp.height-1))
			loc.Field = "block_boundaries." + boundary.ID + ".coordinate"
			loc.Expected = fmt.Sprintf("0 < y < %d", rp.height)
			loc.Actual = fmt.Sprint(c)
			return loc
		}
		var target int64
		maxDiff := int64(-1)
		maxCoord := -1
		for k := 1; k < rp.height; k++ {
			var sum int64
			for x := 0; x < rp.width; x++ {
				a := rp.values[(k-1)*rp.width+x]
				b := rp.values[k*rp.width+x]
				sum += abs64(b - a)
			}
			if sum > maxDiff {
				maxDiff = sum
				maxCoord = k
			}
			if k == c {
				target = sum
			}
		}
		if target == 0 || target < maxDiff {
			loc := locatePoint(rows, boundary.AfterSeq, boundary.Plane, rp.width/2, c)
			loc.Field = "block_boundaries." + boundary.ID + ".coordinate"
			loc.StageSeq = boundary.AfterSeq
			loc.Expected = fmt.Sprintf("tied/strongest_y_edge including=%d", c)
			loc.Actual = fmt.Sprintf("first_y_edge=%d max_diff=%d target_diff=%d", maxCoord, maxDiff, target)
			return loc
		}
	default:
		loc := &model.DiffLocation{StageSeq: boundary.AfterSeq, Field: "block_boundaries." + boundary.ID + ".axis"}
		loc.Expected = "x|y"
		loc.Actual = boundary.Axis
		return loc
	}
	return nil
}
