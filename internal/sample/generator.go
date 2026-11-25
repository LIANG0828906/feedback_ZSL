package sample

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"

	"terrainpatch/internal/boundary"
	"terrainpatch/internal/mesh"
	"terrainpatch/internal/model"
)

const (
	GridN       = 17
	CellSize    = int64(1000)
	TileSpan    = int64(16000)
	Threshold   = int64(5_000_000)
	TightBudget = 40
	HighBudget  = 300
)

func Generate(dir string, seed int64) (*model.Project, *model.AppError) {
	_ = rand.New(rand.NewSource(seed)) // Seed documents deterministic construction; height is analytic.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, model.NewError(model.CodeInternal, err.Error())
	}

	origins := []struct {
		id string
		x  int64
		y  int64
	}{
		{"T00", 0, 0},
		{"T10", TileSpan, 0},
		{"T01", 0, TileSpan},
		{"T11", TileSpan, TileSpan},
	}

	p := &model.Project{
		SchemaVersion:          model.SchemaVersion,
		Seed:                   seed,
		FixedScale:             model.FixedScale,
		AngleThresholdMicroDeg: Threshold,
		Dir:                    dir,
	}
	grids := map[string]model.Grid{}
	decls := map[string]any{}

	for order, o := range origins {
		low := o.id == "T00"
		budget := HighBudget
		maxTriangles := 512
		if low {
			budget = TightBudget
			maxTriangles = 64
		}
		t := model.Tile{
			ID: o.id, Order: order, OriginX: o.x, OriginY: o.y,
			Coverage:     model.Coverage{MinX: o.x, MinY: o.y, MaxX: o.x + TileSpan, MaxY: o.y + TileSpan},
			SampleBudget: budget, Tolerance: 20, MaxTriangles: maxTriangles,
			ParamVersion: 1, HeightMin: 45_000, HeightMax: 100_000,
			Grid: makeGrid(o.x, o.y),
		}
		p.Tiles = append(p.Tiles, t)
		grids[t.ID] = t.Grid
		decls[t.ID] = tileDeclarationDTO(t)
	}

	p.Boundaries = []model.Boundary{
		makeBoundary("E01", "vertical", TileSpan, "T00", "right", "T10", "left", 2, 0),
		makeBoundary("E02", "horizontal", TileSpan, "T00", "bottom", "T01", "top", 2, 0),
		makeBoundary("E13", "horizontal", TileSpan, "T10", "bottom", "T11", "top", 4, TileSpan),
		makeBoundary("E23", "vertical", TileSpan, "T01", "right", "T11", "left", 4, TileSpan),
	}

	meshes := map[string]*model.Mesh{}
	for _, t := range p.Tiles {
		m, err := mesh.Build(t)
		if err != nil {
			return nil, err
		}
		meshes[t.ID] = m
		p.Truth.TriangleLimits = append(p.Truth.TriangleLimits, model.TriangleLimitTruth{
			TileID: t.ID, MaxTriangles: t.MaxTriangles, ActualTriangles: len(m.Triangles),
		})
	}

	p.Truth.ExpectedHeights = expectedHeights(p.Tiles)
	for _, b := range p.Boundaries {
		p.Truth.AlignedSequences = append(p.Truth.AlignedSequences, model.AlignedSequenceTruth{
			BoundaryID: b.ID, Columns: append([]model.BoundaryColumn{}, b.SideA.Columns...),
		})
		if (b.ID == "E01" || b.ID == "E02") && meshes[b.SideA.TileID] != nil {
			ta := *p.Tile(b.SideA.TileID)
			tb := *p.Tile(b.SideB.TileID)
			st, _ := boundary.Diagnose(b, ta, tb, meshes[ta.ID], meshes[tb.ID])
			if st != nil && st.Position != nil {
				p.Truth.Faults = append(p.Truth.Faults, model.FaultTruth{
					ID: "F-" + b.ID, BoundaryID: b.ID, Category: "undersampling",
					X: st.Position.X, Y: st.Position.Y,
					Note: "tight T00 budget leaves quadratic boundary curvature above tolerance",
				})
			}
		}
	}
	p.Probes = buildProbes()
	for _, probe := range p.Probes {
		p.Truth.Faults = append(p.Truth.Faults, probe.Truth)
	}

	if err := writeSamples(dir, p, grids, decls); err != nil {
		return nil, model.NewError(model.CodeInternal, err.Error())
	}
	return p, nil
}

func heightAt(x, y int64) int64 {
	fx := x / CellSize
	fy := y / CellSize
	dx := fx - 8
	dy := fy - 8
	return 50_000 + 120*fx + 80*fy + 35*dx*dx + 35*dy*dy
}

func makeGrid(ox, oy int64) model.Grid {
	g := model.Grid{Width: GridN, Height: GridN, CellSize: CellSize}
	g.Heights = make([][]int64, GridN)
	for y := 0; y < GridN; y++ {
		g.Heights[y] = make([]int64, GridN)
		for x := 0; x < GridN; x++ {
			g.Heights[y][x] = heightAt(ox+int64(x)*CellSize, oy+int64(y)*CellSize)
		}
	}
	return g
}

func tileDeclarationDTO(t model.Tile) any {
	return map[string]any{
		"id": t.ID, "order": t.Order, "originX": t.OriginX, "originY": t.OriginY,
		"coverage": t.Coverage, "sampleBudget": t.SampleBudget, "tolerance": t.Tolerance,
		"maxTriangles": t.MaxTriangles, "paramVersion": t.ParamVersion,
		"heightMin": t.HeightMin, "heightMax": t.HeightMax,
	}
}

func makeBoundary(id, axis string, value int64, tileA, edgeA, tileB, edgeB string, depth int, start int64) model.Boundary {
	n := 1 << depth
	step := TileSpan / int64(n)
	colsA := make([]model.BoundaryColumn, 0, n+1)
	colsB := make([]model.BoundaryColumn, 0, n+1)
	for i := 0; i <= n; i++ {
		var ca, cb model.BoundaryColumn
		if axis == "vertical" {
			y := start + int64(i)*step
			ca = model.BoundaryColumn{Order: i, X: value, Y: y}
			cb = ca
		} else {
			x := start + int64(i)*step
			ca = model.BoundaryColumn{Order: i, X: x, Y: value}
			cb = ca
		}
		colsA = append(colsA, ca)
		colsB = append(colsB, cb)
	}
	sig := model.DigestJSON(colsA)[:16]
	return model.Boundary{
		ID: id, Axis: axis, Value: value, AngleThresholdMicroDeg: Threshold,
		SideA:          model.BoundarySide{TileID: tileA, Edge: edgeA, Columns: colsA},
		SideB:          model.BoundarySide{TileID: tileB, Edge: edgeB, Columns: colsB},
		OrderSignature: sig,
	}
}

func expectedHeights(tiles []model.Tile) []model.ExpectedHeight {
	out := []model.ExpectedHeight{}
	points := [][2]int64{{0, 0}, {TileSpan / 2, TileSpan / 2}, {TileSpan, 0}, {0, TileSpan}, {TileSpan, TileSpan}}
	for _, t := range tiles {
		for _, q := range points {
			x := t.OriginX + q[0]
			y := t.OriginY + q[1]
			out = append(out, model.ExpectedHeight{TileID: t.ID, X: x, Y: y, Z: heightAt(x, y)})
		}
	}
	return out
}

func buildProbes() []model.AttributionProbe {
	ts := []int64{0, 4000, 8000, 12000, 16000}
	alignA := make([]model.ProbePoint, len(ts))
	alignB := make([]model.ProbePoint, len(ts))
	bOrder := []int64{0, 8000, 4000, 12000, 16000}
	for i, t := range ts {
		alignA[i] = model.ProbePoint{Order: i, X: 0, Y: t, Z: 0}
	}
	for i, t := range bOrder {
		alignB[i] = model.ProbePoint{Order: i, X: 0, Y: t, Z: 0}
	}
	alignRefs := flatReferences()

	kinkZ := []int64{0, 0, 0, 8000, 8000}
	kinkA := make([]model.ProbePoint, len(ts))
	kinkB := make([]model.ProbePoint, len(ts))
	for i, t := range ts {
		kinkA[i] = model.ProbePoint{Order: i, X: 32000, Y: t, Z: kinkZ[i]}
		kinkB[i] = model.ProbePoint{Order: i, X: 32000, Y: t, Z: kinkZ[i]}
	}
	kinkRefs := kinkReferences(kinkA)

	return []model.AttributionProbe{
		{
			ID: "P-ALIGN", BoundaryID: "P-ALIGN", Axis: "vertical",
			ToleranceA: 10, ToleranceB: 10, Threshold: 1_000_000,
			SideA: alignA, SideB: alignB, ReferenceA: alignRefs, ReferenceB: alignRefs,
			Truth: model.FaultTruth{ID: "F-P-ALIGN", BoundaryID: "P-ALIGN", Category: "alignment_mismatch", X: 0, Y: 4000},
		},
		{
			ID: "P-ATTR", BoundaryID: "P-ATTR", Axis: "vertical",
			ToleranceA: 10, ToleranceB: 10, Threshold: 1_000_000,
			SideA: kinkA, SideB: kinkB, ReferenceA: kinkRefs, ReferenceB: kinkRefs,
			Truth: model.FaultTruth{ID: "F-P-ATTR", BoundaryID: "P-ATTR", Category: "unclassified", ExpectedCode: model.CodeAttr, X: 32000, Y: 8000},
		},
	}
}

func flatReferences() []model.ProbeReference {
	out := []model.ProbeReference{}
	for y := int64(0); y <= 16000; y += 1000 {
		out = append(out, model.ProbeReference{X: 0, Y: y, Z: 0})
	}
	return out
}

func kinkReferences(points []model.ProbePoint) []model.ProbeReference {
	series := make([]boundary.PointZ, len(points))
	for i, p := range points {
		series[i] = boundary.PointZ{X: p.X, Y: p.Y, Z: p.Z}
	}
	out := []model.ProbeReference{}
	for y := int64(0); y <= 16000; y += 1000 {
		z, _ := boundary.SampleZ(series, "vertical", y)
		out = append(out, model.ProbeReference{X: 32000, Y: y, Z: z})
	}
	return out
}

func writeSamples(dir string, p *model.Project, grids map[string]model.Grid, decls map[string]any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tileIDs := make([]string, 0, len(p.Tiles))
	for _, t := range p.Tiles {
		tileIDs = append(tileIDs, t.ID)
	}
	files := map[string]any{
		"project.json": map[string]any{
			"schemaVersion": p.SchemaVersion, "seed": p.Seed, "fixedScale": p.FixedScale,
			"angleThresholdMicroDeg": p.AngleThresholdMicroDeg, "tileIds": tileIDs,
		},
		"declarations.json": map[string]any{"declarations": decls},
		"elevations.json":   map[string]any{"grids": grids},
		"boundaries.json":   map[string]any{"boundaries": p.Boundaries},
		"truth.json":        p.Truth,
		"probes.json":       map[string]any{"probes": p.Probes},
	}
	for name, v := range files {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}
