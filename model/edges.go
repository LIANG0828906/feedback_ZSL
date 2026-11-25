package model

import (
	"fmt"
	"math"
	"sort"
)

const edgeTolerance = 1e-7

// BuildSharedEdges derives all positive-length chunk adjacencies and verifies
// that both chunks carry the same height sequence along the shared boundary.
func BuildSharedEdges(p *Project) ([]SharedEdge, error) {
	var edges []SharedEdge
	for i := 0; i < len(p.Chunks); i++ {
		for j := i + 1; j < len(p.Chunks); j++ {
			a, b := p.Chunks[i], p.Chunks[j]
			axis, pos, start, end, ok := sharedBoundary(a.Bounds, b.Bounds)
			if !ok {
				continue
			}
			edge := SharedEdge{
				ID:       EdgeID(a.ID, b.ID),
				ChunkA:   a.ID,
				ChunkB:   b.ID,
				Axis:     axis,
				Position: pos,
				Start:    start,
				End:      end,
			}
			positions := commonEdgePositions(a, b, axis, start, end)
			for _, s := range positions {
				ha := interpolateChunkEdge(a, axis, pos, s)
				hb := interpolateChunkEdge(b, axis, pos, s)
				edge.HeightsA = append(edge.HeightsA, ha)
				edge.HeightsB = append(edge.HeightsB, hb)
				if math.Abs(ha-hb) > edgeTolerance {
					edge.Consistent = false
					return edges, NewError(CodeSeam,
						"shared edge height mismatch at %s: %.9f vs %.9f", edge.ID, ha, hb).
						WithEdge(edge.ID).
						WithField("chunks.heights").
						WithWorld(Vec2{X: edgePositionX(axis, pos, s), Z: edgePositionZ(axis, pos, s)})
				}
			}
			edge.Consistent = true
			edges = append(edges, edge)
		}
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
	return edges, nil
}

func sharedBoundary(a, b Bounds) (axis string, position, start, end float64, ok bool) {
	const eps = 1e-9
	if math.Abs(a.MaxX-b.MinX) < eps || math.Abs(b.MaxX-a.MinX) < eps {
		start = math.Max(a.MinZ, b.MinZ)
		end = math.Min(a.MaxZ, b.MaxZ)
		if end-start <= eps {
			return "", 0, 0, 0, false
		}
		if math.Abs(a.MaxX-b.MinX) < eps {
			return "x", a.MaxX, start, end, true
		}
		return "x", b.MaxX, start, end, true
	}
	if math.Abs(a.MaxZ-b.MinZ) < eps || math.Abs(b.MaxZ-a.MinZ) < eps {
		start = math.Max(a.MinX, b.MinX)
		end = math.Min(a.MaxX, b.MaxX)
		if end-start <= eps {
			return "", 0, 0, 0, false
		}
		if math.Abs(a.MaxZ-b.MinZ) < eps {
			return "z", a.MaxZ, start, end, true
		}
		return "z", b.MaxZ, start, end, true
	}
	return "", 0, 0, 0, false
}

func commonEdgePositions(a, b Chunk, axis string, start, end float64) []float64 {
	set := make(map[uint64]float64)
	add := func(v float64) {
		key := math.Float64bits(math.Round(v*1e9) / 1e9)
		if v >= start-1e-9 && v <= end+1e-9 {
			set[key] = math.Round(v*1e9) / 1e9
		}
	}
	if axis == "x" {
		for z := 0; z < a.ResZ; z++ {
			add(a.Bounds.MinZ + float64(z)/float64(a.ResZ-1)*a.Bounds.Depth())
		}
		for z := 0; z < b.ResZ; z++ {
			add(b.Bounds.MinZ + float64(z)/float64(b.ResZ-1)*b.Bounds.Depth())
		}
	} else {
		for x := 0; x < a.ResX; x++ {
			add(a.Bounds.MinX + float64(x)/float64(a.ResX-1)*a.Bounds.Width())
		}
		for x := 0; x < b.ResX; x++ {
			add(b.Bounds.MinX + float64(x)/float64(b.ResX-1)*b.Bounds.Width())
		}
	}
	out := make([]float64, 0, len(set))
	for _, v := range set {
		out = append(out, v)
	}
	sort.Float64s(out)
	return out
}

func interpolateChunkEdge(c Chunk, axis string, position, along float64) float64 {
	if axis == "x" {
		u := (along - c.Bounds.MinZ) / c.Bounds.Depth() * float64(c.ResZ-1)
		x := 0
		switch {
		case math.Abs(position-c.Bounds.MinX) < 1e-9:
			x = 0
		case math.Abs(position-c.Bounds.MaxX) < 1e-9:
			x = c.ResX - 1
		}
		return bilinearColumn(c, x, u)
	}
	u := (along - c.Bounds.MinX) / c.Bounds.Width() * float64(c.ResX-1)
	z := 0
	switch {
	case math.Abs(position-c.Bounds.MinZ) < 1e-9:
		z = 0
	case math.Abs(position-c.Bounds.MaxZ) < 1e-9:
		z = c.ResZ - 1
	}
	return bilinearRow(c, z, u)
}

func bilinearColumn(c Chunk, x int, u float64) float64 {
	v := clamp(u, 0, float64(c.ResZ-1))
	z0 := int(math.Floor(v))
	z1 := minInt(z0+1, c.ResZ-1)
	t := v - float64(z0)
	h0 := c.Heights[z0*c.ResX+x]
	h1 := c.Heights[z1*c.ResX+x]
	return h0 + (h1-h0)*t
}

func bilinearRow(c Chunk, z int, u float64) float64 {
	v := clamp(u, 0, float64(c.ResX-1))
	x0 := int(math.Floor(v))
	x1 := minInt(x0+1, c.ResX-1)
	t := v - float64(x0)
	h0 := c.Heights[z*c.ResX+x0]
	h1 := c.Heights[z*c.ResX+x1]
	return h0 + (h1-h0)*t
}

func edgePositionX(axis string, position, along float64) float64 {
	if axis == "x" {
		return position
	}
	return along
}

func edgePositionZ(axis string, position, along float64) float64 {
	if axis == "z" {
		return position
	}
	return along
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// FormatWorld formats a world position for diagnostics.
func FormatWorld(x, z float64) string {
	return fmt.Sprintf("(%.6f, %.6f)", x, z)
}
