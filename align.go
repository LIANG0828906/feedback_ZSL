package terrain

import (
	"fmt"
	"math"
	"sort"
)

type AlignedKnot struct {
	T      float64    `json:"t"`
	PointA GridPoint  `json:"point_a"`
	PointB GridPoint  `json:"point_b"`
	World  [3]float64 `json:"world_xyz"`
}

type AlignmentPlan struct {
	Knots  map[string][]AlignedKnot `json:"knots"`
	Forced map[int][]GridPoint      `json:"-"`
}

func PlanAlignment(p *Project) (*AlignmentPlan, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	base := map[int]map[string][]int{}
	for i := range p.Blocks {
		base[i] = map[string][]int{}
	}
	for _, c := range p.AlignmentConstraints {
		base[c.BlockA][c.SideA] = append([]int{}, c.SampleColumnA...)
		base[c.BlockB][c.SideB] = append([]int{}, c.SampleColumnB...)
	}

	sideKnots := map[int]map[string][]int{}
	for i, b := range p.Blocks {
		sideKnots[i] = map[string][]int{}
		for _, side := range []string{SideNorth, SideSouth, SideEast, SideWest} {
			sideKnots[i][side] = RefineBoundary1D(b.Grid, side, b.ErrorTolerance, base[i][side])
		}
	}

	forced := map[int][]GridPoint{}
	for i, b := range p.Blocks {
		var pts []GridPoint
		for _, side := range []string{SideNorth, SideSouth, SideEast, SideWest} {
			for _, pos := range sideKnots[i][side] {
				v := sidePoint(b.Grid, b.OriginX, b.OriginZ, side, pos)
				pts = append(pts, GridPoint{X: v.GridX, Y: v.GridY})
			}
		}
		forced[i] = uniquePoints(pts)
	}

	plan := &AlignmentPlan{Knots: map[string][]AlignedKnot{}, Forced: forced}
	for _, bnd := range p.SharedBoundaries {
		c, err := findConstraint(p, bnd.ConstraintID)
		if err != nil {
			return nil, err
		}
		ba, bb := p.Blocks[bnd.BlockA], p.Blocks[bnd.BlockB]
		lenA, lenB := edgeLength(ba.Grid, bnd.SideA), edgeLength(bb.Grid, bnd.SideB)
		pairs := map[[2]int]bool{}
		for k := range c.SampleColumnA {
			pairs[[2]int{c.SampleColumnA[k], c.SampleColumnB[k]}] = true
		}
		for _, pa := range sideKnots[bnd.BlockA][bnd.SideA] {
			pb := mapEdgePosition(pa, lenA, lenB)
			pairs[[2]int{pa, pb}] = true
		}
		for _, pb := range sideKnots[bnd.BlockB][bnd.SideB] {
			pa := mapEdgePosition(pb, lenB, lenA)
			pairs[[2]int{pa, pb}] = true
		}

		knots := make([]AlignedKnot, 0, len(pairs))
		for pair := range pairs {
			pa, pb := pair[0], pair[1]
			va := sidePoint(ba.Grid, ba.OriginX, ba.OriginZ, bnd.SideA, pa)
			vb := sidePoint(bb.Grid, bb.OriginX, bb.OriginZ, bnd.SideB, pb)
			forced[bnd.BlockB] = append(forced[bnd.BlockB], GridPoint{X: vb.GridX, Y: vb.GridY})
			forced[bnd.BlockA] = append(forced[bnd.BlockA], GridPoint{X: va.GridX, Y: va.GridY})
			t := float64(pa) / float64(lenA-1)
			knots = append(knots, AlignedKnot{T: t, PointA: GridPoint{X: va.GridX, Y: va.GridY}, PointB: GridPoint{X: vb.GridX, Y: vb.GridY}, World: [3]float64{va.X, va.Y, va.Z}})
		}
		sort.Slice(knots, func(i, j int) bool {
			if knots[i].T != knots[j].T {
				return knots[i].T < knots[j].T
			}
			return knots[i].PointA.Y < knots[j].PointA.Y
		})
		plan.Knots[bnd.ID] = knots
	}
	for i := range forced {
		plan.Forced[i] = uniquePoints(forced[i])
		sortPoints(plan.Forced[i])
	}
	return plan, nil
}

func findConstraint(p *Project, id string) (*AlignmentConstraint, error) {
	for i := range p.AlignmentConstraints {
		if p.AlignmentConstraints[i].ID == id {
			return &p.AlignmentConstraints[i], nil
		}
	}
	return nil, NewSchemaError("$.alignment_constraints", nil, "constraint %q not found", id)
}

func mapEdgePosition(pos, fromLen, toLen int) int {
	if fromLen <= 1 || toLen <= 1 {
		return 0
	}
	t := float64(pos) / float64(fromLen-1)
	mapped := int(math.Round(t * float64(toLen-1)))
	return max(0, min(toLen-1, mapped))
}

type CompiledProject struct {
	Meshes  map[int]*Mesh
	Reports map[string]*BoundaryReport
	Plan    *AlignmentPlan
}

func CompileProject(p *Project) (*CompiledProject, error) {
	plan, err := PlanAlignment(p)
	if err != nil {
		return nil, err
	}
	out := &CompiledProject{Meshes: map[int]*Mesh{}, Reports: map[string]*BoundaryReport{}, Plan: plan}
	for _, b := range p.Blocks {
		mesh, err := BuildBlockMesh(b, BuildOptions{ParameterVersion: p.ParameterVersion, ForcedPoints: plan.Forced[b.BlockNo]})
		if err != nil {
			return nil, err
		}
		out.Meshes[b.BlockNo] = mesh
	}
	if err := compileReports(p, out); err != nil {
		return nil, err
	}
	return out, nil
}

func compileReports(p *Project, out *CompiledProject) error {
	for _, bnd := range p.SharedBoundaries {
		c, err := findConstraint(p, bnd.ConstraintID)
		if err != nil {
			return err
		}
		ma, mb := out.Meshes[bnd.BlockA], out.Meshes[bnd.BlockB]
		if ma == nil || mb == nil {
			return fmt.Errorf("missing mesh for boundary %s", bnd.ID)
		}
		sa := ExtractBoundarySide(ma, p.Blocks[bnd.BlockA], bnd.SideA)
		sb := ExtractBoundarySide(mb, p.Blocks[bnd.BlockB], bnd.SideB)
		buffer := c.AlignmentBufferCells * math.Max(p.Blocks[bnd.BlockA].Grid.CellSize, p.Blocks[bnd.BlockB].Grid.CellSize)
		if buffer <= 1e-9 {
			buffer = 1e-6
		}
		rep := BuildBoundaryReport(BoundarySharedView{ID: bnd.ID, BlockA: bnd.BlockA, BlockB: bnd.BlockB}, sa, sb, c.MaxFoldAngleDeg,
			p.Blocks[bnd.BlockA].ErrorTolerance, p.Blocks[bnd.BlockB].ErrorTolerance, buffer)
		out.Reports[bnd.ID] = rep
	}
	return nil
}
