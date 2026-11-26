package rebuild

import (
	"sort"

	"acousticfield/internal/acoustic"
)

type Boundary struct {
	Side       string
	NeighborID string
	Position   acoustic.Point
}

func ChunkOrder(level *acoustic.Level) []acoustic.Chunk {
	chunks := append([]acoustic.Chunk(nil), level.Chunks...)
	sort.Slice(chunks, func(i, j int) bool {
		if chunks[i].Row != chunks[j].Row {
			return chunks[i].Row < chunks[j].Row
		}
		if chunks[i].Col != chunks[j].Col {
			return chunks[i].Col < chunks[j].Col
		}
		return chunks[i].ID < chunks[j].ID
	})
	return chunks
}

func Boundaries(chunk acoustic.Chunk, chunks []acoustic.Chunk) []Boundary {
	out := []Boundary{}
	for _, other := range chunks {
		if other.ID == chunk.ID {
			continue
		}
		c, o := chunk.Bounds, other.Bounds
		if c.Max.X == o.Min.X {
			if lo, hi, ok := overlap(c.Min.Y, c.Max.Y, o.Min.Y, o.Max.Y); ok {
				out = append(out, Boundary{"east", other.ID, acoustic.Point{X: c.Max.X, Y: midpoint(lo, hi)}})
			}
		}
		if c.Min.X == o.Max.X {
			if lo, hi, ok := overlap(c.Min.Y, c.Max.Y, o.Min.Y, o.Max.Y); ok {
				out = append(out, Boundary{"west", other.ID, acoustic.Point{X: c.Min.X, Y: midpoint(lo, hi)}})
			}
		}
		if c.Max.Y == o.Min.Y {
			if lo, hi, ok := overlap(c.Min.X, c.Max.X, o.Min.X, o.Max.X); ok {
				out = append(out, Boundary{"north", other.ID, acoustic.Point{X: midpoint(lo, hi), Y: c.Max.Y}})
			}
		}
		if c.Min.Y == o.Max.Y {
			if lo, hi, ok := overlap(c.Min.X, c.Max.X, o.Min.X, o.Max.X); ok {
				out = append(out, Boundary{"south", other.ID, acoustic.Point{X: midpoint(lo, hi), Y: c.Min.Y}})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Side != out[j].Side {
			return out[i].Side < out[j].Side
		}
		return out[i].NeighborID < out[j].NeighborID
	})
	return out
}

func overlap(a0, a1, b0, b1 int64) (int64, int64, bool) {
	lo := max64(a0, b0)
	hi := min64(a1, b1)
	return lo, hi, hi > lo
}

func midpoint(a, b int64) int64 { return a + (b-a)/2 }

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func sumBands(results []acoustic.ChunkResult, bandCount int, pick func(acoustic.ChunkResult) []int64) []int64 {
	total := make([]int64, bandCount)
	for _, r := range results {
		v := pick(r)
		for b := 0; b < bandCount && b < len(v); b++ {
			total[b] += v[b]
		}
	}
	return total
}
