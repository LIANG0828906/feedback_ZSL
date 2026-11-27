package terrainbake

import (
	"fmt"
	"math"
	"sort"
)

const (
	DefaultSeed        uint64 = 20261127
	DefaultWorldWidth  int64  = 384
	DefaultWorldHeight int64  = 256
	DefaultBlockSize   int64  = 128
	DefaultSeamSpacing int64  = 8
	GeneratedBy               = "terrainbake/deterministic-generator-v1"
)

// Generator owns the analytic height construction. Every schema and truth
// record is derived from this one function, so tests never need an external
// fixture or a recorded raster.
type Generator struct {
	Seed uint64
}

func NewGenerator(seed uint64) *Generator {
	return &Generator{Seed: seed}
}

func (g *Generator) phase() float64 {
	return float64(g.Seed%100003) / 97.0
}

// BaseHeightMM is the shared, analytic terrain height. Heights are integer
// millimetres. Shared block boundaries always use this function, which makes
// the alignment profile independent of block-local edits.
func (g *Generator) BaseHeightMM(x, y int64) int64 {
	if g == nil {
		g = NewGenerator(DefaultSeed)
	}
	p := g.phase()
	fx := float64(x)
	fy := float64(y)
	h := 850.0
	h += 310.0 * math.Sin(fx*0.0041+fy*0.0013+p*0.01)
	h += 245.0 * math.Cos(fy*0.0053-fx*0.0009+p*0.017)
	h += 115.0 * math.Sin((fx+fy)*0.0027+p*0.031)
	h += 47.0 * math.Cos((fx-fy)*0.0101+p*0.007)
	return int64(math.Round(h))
}

// HeightMM returns the block-local field. The edit bump is faded to zero at
// coverage edges. It demonstrates interior edits while keeping the declared
// shared-boundary truth invariant.
func (g *Generator) HeightMM(blockID string, rect Rect, editDeltaMM int64, x, y int64) int64 {
	h := g.BaseHeightMM(x, y)
	if editDeltaMM == 0 {
		return h
	}
	if !rect.Contains(Point{X: x, Y: y}) {
		return h
	}
	const fade = int64(12)
	dx := min64(x-rect.MinX, rect.MaxX-1-x)
	dy := min64(y-rect.MinY, rect.MaxY-1-y)
	d := min64(dx, dy)
	if d <= 0 {
		return h
	}
	if d > fade {
		d = fade
	}
	return h + editDeltaMM*d/fade
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Project builds the canonical six-block sample. Three serializations are
// produced on demand by the schema encoder; this internal project is the
// authoritative content.
func (g *Generator) Project() *Project {
	if g == nil {
		g = NewGenerator(DefaultSeed)
	}
	world := World{
		Rect: Rect{MinX: 0, MinY: 0, MaxX: DefaultWorldWidth, MaxY: DefaultWorldHeight},
		Seed: g.Seed,
		Name: "deterministic-open-world-ridge",
	}
	cols := int(DefaultWorldWidth / DefaultBlockSize)
	rows := int(DefaultWorldHeight / DefaultBlockSize)
	tolerances := []int64{
		2000, 3000, 4000,
		2000, 3000, 2000,
	}
	budgets := []Budget{
		{MaxVertices: 3200, MaxTriangles: 4200},
		{MaxVertices: 3000, MaxTriangles: 4000},
		{MaxVertices: 3600, MaxTriangles: 4800},
		{MaxVertices: 3300, MaxTriangles: 4400},
		{MaxVertices: 3100, MaxTriangles: 4100},
		{MaxVertices: 3500, MaxTriangles: 4600},
	}
	blocks := make([]BlockSpec, 0, cols*rows)
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			idx := row*cols + col
			o := Point{X: int64(col) * DefaultBlockSize, Y: int64(row) * DefaultBlockSize}
			sz := Size{W: DefaultBlockSize, H: DefaultBlockSize}
			blocks = append(blocks, BlockSpec{
				ID:                 fmt.Sprintf("B%02d", idx),
				Origin:             o,
				Size:               sz,
				Rect:               RectFromOriginSize(o, sz),
				Budget:             budgets[idx],
				ToleranceUM:        tolerances[idx],
				ToleranceScale:     "permille",
				ToleranceRounding:  "half_up",
				DeclaredNeighbors:  true,
				SourceCoverageRule: "[min,max)",
			})
		}
	}

	seams := make([]SeamSpec, 0)
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			idx := row*cols + col
			a := blocks[idx]
			if col+1 < cols {
				b := blocks[idx+1]
				seams = append(seams, SeamSpec{
					ID:            EdgeID(a.ID, b.ID),
					A:             a.ID,
					B:             b.ID,
					Axis:          AxisVertical,
					Coordinate:    a.Rect.MaxX,
					Start:         a.Rect.MinY,
					End:           a.Rect.MaxY,
					SampleSpacing: DefaultSeamSpacing,
					AngleLimitDeg: 42,
				})
			}
			if row+1 < rows {
				b := blocks[idx+cols]
				seams = append(seams, SeamSpec{
					ID:            EdgeID(a.ID, b.ID),
					A:             a.ID,
					B:             b.ID,
					Axis:          AxisHorizontal,
					Coordinate:    a.Rect.MaxY,
					Start:         a.Rect.MinX,
					End:           a.Rect.MaxX,
					SampleSpacing: DefaultSeamSpacing,
					AngleLimitDeg: 38,
				})
			}
		}
	}
	sort.Slice(seams, func(i, j int) bool { return seams[i].ID < seams[j].ID })
	for i := range blocks {
		for _, s := range seams {
			if s.A == blocks[i].ID {
				blocks[i].Neighbors = append(blocks[i].Neighbors, s.B)
			} else if s.B == blocks[i].ID {
				blocks[i].Neighbors = append(blocks[i].Neighbors, s.A)
			}
		}
		blocks[i].Neighbors = SortedUniqueStrings(blocks[i].Neighbors)
	}

	p := &Project{
		SourceVersion: SchemaV3,
		World:         world,
		Blocks:        blocks,
		Seams:         seams,
		Camera:        g.cameraTrack(blocks),
		GeneratedBy:   GeneratedBy,
		Truth: Truth{
			MaxTriangles: map[string]int{},
			Fractures:    []FractureTruth{},
		},
	}
	g.fillTruth(p)
	return p
}

func (g *Generator) cameraTrack(blocks []BlockSpec) []CameraSample {
	out := make([]CameraSample, 0, 9)
	for i := 0; i < 9; i++ {
		t := int64(i) * 750
		u := float64(i) / 8.0
		x := int64(math.Round(-40 + u*460))
		y := int64(math.Round(80 + 45*math.Sin(u*math.Pi*2)))
		focus := blocks[i%len(blocks)].ID
		out = append(out, CameraSample{
			TimeMS:   t,
			Position: Point{X: x, Y: y},
			YawDeg:   math.Round(18 + u*70),
			PitchDeg: math.Round(-12 + 7*math.Sin(u*math.Pi)),
			FOVDeg:   58 + math.Round(u*6),
			Near:     1,
			Far:      900,
			Focus:    focus,
		})
	}
	return out
}

func (g *Generator) fillTruth(p *Project) {
	for i, b := range p.Blocks {
		p.Truth.MaxTriangles[b.ID] = b.Budget.MaxTriangles
		// These points lie on the forced boundary lattice. The mesh builder
		// guarantees they are actual vertices, so the truth is directly
		// comparable with emitted vertex heights.
		pts := []Point{
			{X: b.Rect.MinX + 16 + int64(i%3)*8, Y: b.Rect.MinY},
			{X: b.Rect.MinX, Y: b.Rect.MinY + 48},
			{X: b.Rect.MaxX, Y: b.Rect.MinY + 80},
			{X: b.Rect.MaxX - 16, Y: b.Rect.MaxY},
		}
		for _, pt := range pts {
			p.Truth.Samples = append(p.Truth.Samples, TruthSample{
				BlockID:  b.ID,
				X:        pt.X,
				Y:        pt.Y,
				HeightMM: g.BaseHeightMM(pt.X, pt.Y),
			})
		}
	}
	for _, s := range p.Seams {
		points := seamPoints(s.Start, s.End, s.SampleSpacing)
		heights := make([]int64, len(points))
		for i, q := range points {
			if s.Axis == AxisVertical {
				heights[i] = g.BaseHeightMM(s.Coordinate, q)
			} else {
				heights[i] = g.BaseHeightMM(q, s.Coordinate)
			}
		}
		p.Truth.Seams = append(p.Truth.Seams, SeamTruth{
			SeamID:    s.ID,
			Axis:      s.Axis,
			Start:     s.Start,
			End:       s.End,
			Points:    points,
			HeightsMM: heights,
		})
	}
	if len(p.Seams) > 0 {
		s := p.Seams[0]
		mid := s.Start + (s.End-s.Start)/2
		p.Truth.Fractures = append(p.Truth.Fractures, FractureTruth{
			ID:          "F:undersampling:0",
			SeamID:      s.ID,
			Kind:        "undersampling",
			At:          mid,
			ToleranceUM: 1000,
			AngleLimit:  0.25,
			Expected:    "undersampling",
		})
	}
	if len(p.Seams) > 1 {
		s := p.Seams[1]
		mid := s.Start + (s.End-s.Start)/2
		p.Truth.Fractures = append(p.Truth.Fractures, FractureTruth{
			ID:          "F:alignment:1",
			SeamID:      s.ID,
			Kind:        "alignment_mismatch",
			At:          mid,
			ToleranceUM: 12000,
			AngleLimit:  42,
			Expected:    "alignment_mismatch",
		})
	}
}

func seamPoints(start, end, spacing int64) []int64 {
	if spacing <= 0 {
		spacing = 1
	}
	out := make([]int64, 0, (end-start)/spacing+2)
	for q := start; q <= end; q += spacing {
		out = append(out, q)
	}
	if out[len(out)-1] != end {
		out = append(out, end)
	}
	return out
}

// SeamSpecsFromBlocks reconstructs the unique shared edges from block
// coverage. It is used for schema versions that omit adjacency tables.
func SeamSpecsFromBlocks(blocks []BlockSpec) []SeamSpec {
	var seams []SeamSpec
	for i := 0; i < len(blocks); i++ {
		for j := i + 1; j < len(blocks); j++ {
			a, b := blocks[i], blocks[j]
			if a.Rect.MaxX == b.Rect.MinX || b.Rect.MaxX == a.Rect.MinX {
				var left, right BlockSpec
				if a.Rect.MinX < b.Rect.MinX {
					left, right = a, b
				} else {
					left, right = b, a
				}
				start := max64(left.Rect.MinY, right.Rect.MinY)
				end := min64(left.Rect.MaxY, right.Rect.MaxY)
				if end > start {
					seams = append(seams, SeamSpec{
						ID:            EdgeID(left.ID, right.ID),
						A:             left.ID,
						B:             right.ID,
						Axis:          AxisVertical,
						Coordinate:    left.Rect.MaxX,
						Start:         start,
						End:           end,
						SampleSpacing: DefaultSeamSpacing,
						AngleLimitDeg: 42,
					})
				}
			}
			if a.Rect.MaxY == b.Rect.MinY || b.Rect.MaxY == a.Rect.MinY {
				var bottom, top BlockSpec
				if a.Rect.MinY < b.Rect.MinY {
					bottom, top = a, b
				} else {
					bottom, top = b, a
				}
				start := max64(bottom.Rect.MinX, top.Rect.MinX)
				end := min64(bottom.Rect.MaxX, top.Rect.MaxX)
				if end > start {
					seams = append(seams, SeamSpec{
						ID:            EdgeID(bottom.ID, top.ID),
						A:             bottom.ID,
						B:             top.ID,
						Axis:          AxisHorizontal,
						Coordinate:    bottom.Rect.MaxY,
						Start:         start,
						End:           end,
						SampleSpacing: DefaultSeamSpacing,
						AngleLimitDeg: 38,
					})
				}
			}
		}
	}
	sort.Slice(seams, func(i, j int) bool { return seams[i].ID < seams[j].ID })
	for i := range blocks {
		for _, s := range seams {
			if s.A == blocks[i].ID {
				blocks[i].Neighbors = append(blocks[i].Neighbors, s.B)
			} else if s.B == blocks[i].ID {
				blocks[i].Neighbors = append(blocks[i].Neighbors, s.A)
			}
		}
		blocks[i].Neighbors = SortedUniqueStrings(blocks[i].Neighbors)
	}
	return seams
}
