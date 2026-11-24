package terrain

import (
	"fmt"
	"math"
)

type blockSpec struct {
	w, h    int
	budget  int
	tol     float64
	feature string
}

func DefaultSpecs() []blockSpec {
	return []blockSpec{
		{17, 13, 130, 0.18, "rolling"},
		{25, 13, 180, 0.14, "ridge"},
		{17, 21, 175, 0.16, "basin"},
		{25, 21, 230, 0.12, "peak"},
	}
}

func GenerateSample(seed int64) *Project {
	specs := DefaultSpecs()
	blocks := make([]Block, 0, len(specs))
	origins := [][2]float64{{0, 0}, {16, 0}, {0, 12}, {16, 12}}

	for i, spec := range specs {
		g := Grid{Width: spec.w, Height: spec.h, CellSize: 1, Heights: make([]float64, spec.w*spec.h)}
		for y := 0; y < spec.h; y++ {
			for x := 0; x < spec.w; x++ {
				worldX := origins[i][0] + float64(x)
				worldZ := origins[i][1] + float64(y)
				g.Heights[gridIndex(g, x, y)] = round6(sampleAnalyticHeight(seed, i, spec.feature, spec.w, spec.h, x, y, worldX, worldZ))
			}
		}
		b := Block{
			BlockNo:        i,
			ID:             fmt.Sprintf("block-%02d", i),
			Feature:        spec.feature,
			Grid:           g,
			OriginX:        origins[i][0],
			OriginZ:        origins[i][1],
			CoverageXZ:     [4]float64{origins[i][0], origins[i][1], origins[i][0] + float64(spec.w-1), origins[i][1] + float64(spec.h-1)},
			SampleBudget:   spec.budget,
			ErrorTolerance: spec.tol,
		}
		b.RequiredPoints = []GridPoint{
			{X: 2, Y: 2},
			{X: spec.w / 2, Y: 3},
			{X: spec.w - 3, Y: spec.h / 2},
			{X: spec.w / 2, Y: spec.h - 3},
			{X: 3, Y: spec.h - 3},
			{X: spec.w / 2, Y: spec.h / 2},
		}
		blocks = append(blocks, b)
	}

	constraints := []AlignmentConstraint{
		makeConstraint("align-0-1", 0, 1, SideEast, SideWest, blocks, 7, 24),
		makeConstraint("align-0-2", 0, 2, SideSouth, SideNorth, blocks, 5, 18),
		makeConstraint("align-1-3", 1, 3, SideSouth, SideNorth, blocks, 7, 32),
		makeConstraint("align-2-3", 2, 3, SideEast, SideWest, blocks, 7, 24),
	}
	boundaries := []SharedBoundary{
		{ID: "edge-0-1", BlockA: 0, BlockB: 1, SideA: SideEast, SideB: SideWest, ConstraintID: "align-0-1"},
		{ID: "edge-0-2", BlockA: 0, BlockB: 2, SideA: SideSouth, SideB: SideNorth, ConstraintID: "align-0-2"},
		{ID: "edge-1-3", BlockA: 1, BlockB: 3, SideA: SideSouth, SideB: SideNorth, ConstraintID: "align-1-3"},
		{ID: "edge-2-3", BlockA: 2, BlockB: 3, SideA: SideEast, SideB: SideWest, ConstraintID: "align-2-3"},
	}

	truth := Truth{
		VertexChecks:     generateVertexTruth(blocks),
		BlockErrorBounds: []BlockErrorTruth{{BlockNo: 0, DeclaredMaxHeightError: 2.25}, {BlockNo: 1, DeclaredMaxHeightError: 2.5}, {BlockNo: 2, DeclaredMaxHeightError: 2.5}, {BlockNo: 3, DeclaredMaxHeightError: 2.75}},
		BoundaryChecks: []BoundaryTruth{
			{BoundaryID: "edge-0-1", MaxAllowedFoldAngle: constraints[0].MaxFoldAngleDeg},
			{BoundaryID: "edge-0-2", MaxAllowedFoldAngle: constraints[1].MaxFoldAngleDeg},
			{BoundaryID: "edge-1-3", MaxAllowedFoldAngle: constraints[2].MaxFoldAngleDeg},
			{BoundaryID: "edge-2-3", MaxAllowedFoldAngle: constraints[3].MaxFoldAngleDeg},
		},
		FaultCases: generateFaultCases(),
	}

	return &Project{
		SchemaVersion:        FormatVersion,
		ProjectID:            fmt.Sprintf("deterministic-terrain-seed-%d", seed),
		Seed:                 seed,
		AlgorithmVersion:     AlgorithmVersion,
		ParameterVersion:     DefaultParamVer,
		Blocks:               blocks,
		SharedBoundaries:     boundaries,
		AlignmentConstraints: constraints,
		Truth:                truth,
	}
}

func makeConstraint(id string, a, b int, sideA, sideB string, blocks []Block, knots int, maxAngle float64) AlignmentConstraint {
	edgeA := edgeLength(blocks[a].Grid, sideA)
	edgeB := edgeLength(blocks[b].Grid, sideB)
	colA := evenColumn(edgeA, knots)
	colB := evenColumn(edgeB, knots)
	return AlignmentConstraint{
		ID:                   id,
		BlockA:               a,
		BlockB:               b,
		SideA:                sideA,
		SideB:                sideB,
		SampleColumnA:        colA,
		SampleColumnB:        colB,
		MaxFoldAngleDeg:      maxAngle,
		AlignmentBufferCells: 0.35,
	}
}

func evenColumn(edgeLength, count int) []int {
	if count < 2 {
		count = 2
	}
	out := make([]int, count)
	for i := 0; i < count; i++ {
		pos := int(math.Round(float64(i) * float64(edgeLength-1) / float64(count-1)))
		if i == count-1 {
			pos = edgeLength - 1
		}
		out[i] = pos
	}
	return out
}

func generateVertexTruth(blocks []Block) []VertexTruth {
	var out []VertexTruth
	for _, b := range blocks {
		for _, p := range b.RequiredPoints {
			out = append(out, VertexTruth{
				BlockNo:        b.BlockNo,
				GridX:          p.X,
				GridY:          p.Y,
				ExpectedHeight: gridHeight(b.Grid, p.X, p.Y),
				Tolerance:      b.ErrorTolerance,
			})
		}
	}
	return out
}

func sampleAnalyticHeight(seed int64, blockNo int, feature string, w, h, gx, gy int, x, z float64) float64 {
	s := float64(seed%1000) * 0.001
	base := 1.9 * math.Sin(x*0.21+s*2*math.Pi) * math.Cos(z*0.17-s)
	base += 1.05 * math.Sin((x+z)*0.29+s*2.0)
	base += 0.55 * math.Cos(math.Hypot(x-18.0, z-16.0)*0.42+s)
	base += 0.22 * math.Sin(x*0.83+s) * math.Sin(z*0.71)

	fx := float64(gx) / float64(w-1)
	fy := float64(gy) / float64(h-1)
	fade := math.Sin(math.Pi*fx) * math.Sin(math.Pi*fy)
	dx := float64(gx-w/2) / float64(w)
	dy := float64(gy-h/2) / float64(h)
	d2 := dx*dx + dy*dy
	switch feature {
	case "ridge":
		base += 2.4 * math.Exp(-dx*dx*34.0) * (0.65 + 0.35*math.Cos(float64(gy)*0.5)) * fade
	case "basin":
		base -= 2.2 * math.Exp(-d2*18.0) * fade
	case "peak":
		base += 3.0 * math.Exp(-d2*24.0) * fade
	case "rolling":
		base += 0.85 * math.Sin(float64(gx)*0.42) * math.Cos(float64(gy)*0.36) * fade
	}
	return base
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func generateFaultCases() []FaultCase {
	okLeft := BoundarySide{BlockNo: 0, Side: SideEast, Points: []BoundaryPoint{
		{T: 0, X: 16, Y: 1.0, Z: 0}, {T: 0.5, X: 16, Y: 1.2, Z: 5}, {T: 1, X: 16, Y: 1.0, Z: 10},
	}, Tangents: [][3]float64{{-1, -0.1, 0}, {-1, -0.2, 0}, {-1, -0.1, 0}}}
	okRight := BoundarySide{BlockNo: 1, Side: SideWest, Points: []BoundaryPoint{
		{T: 0, X: 16, Y: 1.0, Z: 0}, {T: 0.5, X: 16, Y: 1.2, Z: 5}, {T: 1, X: 16, Y: 1.0, Z: 10},
	}, Tangents: [][3]float64{{1, 0.1, 0}, {1, 0.2, 0}, {1, 0.1, 0}}}

	underLeft := okLeft
	underLeft.MaxHeightError = 0.86
	underLeft.Tangents = [][3]float64{{-1, 1.15, 0}, {-1, 1.35, 0}, {-1, 1.1, 0}}
	underRight := okRight
	underRight.MaxHeightError = 0.08
	underRight.Tangents = [][3]float64{{1, 0.15, 0}, {1, 0.1, 0}, {1, 0.12, 0}}

	alignLeft := okLeft
	alignRight := BoundarySide{BlockNo: 1, Side: SideWest, Points: []BoundaryPoint{
		{T: 1, X: 16, Y: 1.0, Z: 10}, {T: 0.5, X: 16, Y: 1.2, Z: 5}, {T: 0, X: 16, Y: 1.0, Z: 0},
	}, Tangents: [][3]float64{{1, 0.1, 0}, {1, 0.2, 0}, {1, 0.1, 0}}}

	unavailLeft := okLeft
	unavailRight := BoundarySide{BlockNo: 1, Side: SideWest, Points: []BoundaryPoint{
		{T: 0, X: 18.4, Y: 1.0, Z: 0.8}, {T: 0.5, X: 18.4, Y: 1.2, Z: 5.8}, {T: 1, X: 18.4, Y: 1.0, Z: 10.8},
	}, Tangents: [][3]float64{{1, 0.1, 0}, {1, 0.2, 0}, {1, 0.1, 0}}}

	return []FaultCase{
		{ID: "fault-ok", BoundaryID: "edge-0-1", BlockA: 0, BlockB: 1, Left: okLeft, Right: okRight, FoldLimitDeg: 20, HeightTolerance: 0.15, AlignmentBuffer: 0.25, ExpectedCategory: "ok"},
		{ID: "fault-undersample", BoundaryID: "edge-0-1", BlockA: 0, BlockB: 1, Left: underLeft, Right: underRight, FoldLimitDeg: 20, HeightTolerance: 0.15, AlignmentBuffer: 0.25, ExpectedCategory: CategoryUndersample},
		{ID: "fault-alignment-order", BoundaryID: "edge-0-1", BlockA: 0, BlockB: 1, Left: alignLeft, Right: alignRight, FoldLimitDeg: 20, HeightTolerance: 0.15, AlignmentBuffer: 0.25, ExpectedCategory: CategoryAlignment},
		{ID: "fault-align-unavailable", BoundaryID: "edge-0-1", BlockA: 0, BlockB: 1, Left: unavailLeft, Right: unavailRight, FoldLimitDeg: 20, HeightTolerance: 0.15, AlignmentBuffer: 0.25, ExpectedCategory: CategoryAlignUnavailable, ExpectedCode: CodeAlign},
	}
}

const (
	CategoryOK               = "ok"
	CategoryUndersample      = "undersample"
	CategoryAlignment        = "alignment_mismatch"
	CategoryMixed            = "mixed"
	CategoryAlignUnavailable = "alignment_unavailable"
	CategoryNaturalFold      = "natural_sharp_fold"
)
