package verifier

import (
	"context"

	"terrainpatch/internal/boundary"
	"terrainpatch/internal/engine"
	"terrainpatch/internal/loader"
	"terrainpatch/internal/model"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail any    `json:"detail,omitempty"`
	Code   string `json:"code,omitempty"`
}

type Report struct {
	SampleDir string                 `json:"sampleDir"`
	DBPath    string                 `json:"dbPath"`
	Checks    []Check                `json:"checks"`
	Base      *engine.Report         `json:"base,omitempty"`
	ThreeWay  *engine.ThreeWayReport `json:"threeWay,omitempty"`
	OK        bool                   `json:"ok"`
}

func VerifySample(ctx context.Context, dir, dbPath string) (*Report, *model.AppError) {
	p, err := loader.LoadProject(dir)
	if err != nil {
		return &Report{SampleDir: dir, DBPath: dbPath, OK: false, Checks: []Check{{Name: "load", OK: false, Code: err.Code, Detail: err}}}, err
	}
	out := &Report{SampleDir: dir, DBPath: dbPath, OK: true}

	eng := engine.NewEngine(p, engine.Options{Workers: 2, VertexBufferCap: 4096, MemoryBudgetBytes: 16 << 20})
	full, berr := eng.RebuildAll(ctx, "verify-full")
	if berr != nil {
		out.add(Check{Name: "triangulation-budget", OK: false, Code: berr.Code, Detail: berr})
		out.OK = false
		return out, berr
	}
	out.Base = full
	out.add(Check{Name: "triangulation-budget", OK: true, Detail: map[string]any{"tiles": len(full.Meshes)}})

	checkHeights(p, full, out)
	checkTriangleLimits(p, full, out)
	checkAlignedSequences(p, full, out)
	checkFaults(p, full, out)

	tw, terr := engine.VerifyThreePaths(ctx, p, engine.VerificationEdits(p), dbPath, engine.Options{Workers: 2, VertexBufferCap: 4096, MemoryBudgetBytes: 16 << 20})
	out.ThreeWay = tw
	if terr != nil {
		out.add(Check{Name: "three-path-consistency", OK: false, Code: terr.Code, Detail: terr})
		out.OK = false
	} else {
		out.add(Check{Name: "three-path-consistency", OK: true, Detail: map[string]any{
			"equal": true, "commitSequence": tw.CommitSequence,
			"recoveryReset": tw.RecoveryInfo.Reset, "recovered": tw.RecoveryInfo.Recovered,
		}})
	}
	return out, nil
}

func (r *Report) add(c Check) {
	r.Checks = append(r.Checks, c)
	if !c.OK {
		r.OK = false
	}
}

func checkHeights(p *model.Project, rep *engine.Report, out *Report) {
	bad := []any{}
	for _, h := range p.Truth.ExpectedHeights {
		m := rep.Meshes[h.TileID]
		if m == nil {
			bad = append(bad, map[string]any{"tile": h.TileID, "reason": "mesh missing"})
			continue
		}
		var found *model.Vertex
		for i := range m.Vertices {
			if m.Vertices[i].X == h.X && m.Vertices[i].Y == h.Y {
				found = &m.Vertices[i]
				break
			}
		}
		if found == nil {
			bad = append(bad, map[string]any{"tile": h.TileID, "field": "vertex", "x": h.X, "y": h.Y})
			continue
		}
		if found.Z != h.Z {
			bad = append(bad, map[string]any{"tile": h.TileID, "field": "z", "expected": h.Z, "actual": found.Z})
		}
	}
	out.add(Check{Name: "analytic-expected-heights", OK: len(bad) == 0, Detail: map[string]any{"checked": len(p.Truth.ExpectedHeights), "mismatches": bad}})
}

func checkTriangleLimits(p *model.Project, rep *engine.Report, out *Report) {
	bad := []any{}
	for _, lim := range p.Truth.TriangleLimits {
		m := rep.Meshes[lim.TileID]
		if m == nil {
			bad = append(bad, map[string]any{"tile": lim.TileID, "reason": "missing"})
			continue
		}
		if len(m.Triangles) > lim.MaxTriangles {
			bad = append(bad, map[string]any{"tile": lim.TileID, "actual": len(m.Triangles), "limit": lim.MaxTriangles})
		}
	}
	out.add(Check{Name: "declared-max-triangles", OK: len(bad) == 0, Detail: map[string]any{"limits": p.Truth.TriangleLimits, "violations": bad}})
}

func checkAlignedSequences(p *model.Project, rep *engine.Report, out *Report) {
	stats := map[string]boundary.Stat{}
	for _, s := range rep.Boundaries {
		stats[s.BoundaryID] = s
	}
	bad := []any{}
	for _, seq := range p.Truth.AlignedSequences {
		st, ok := stats[seq.BoundaryID]
		if !ok {
			bad = append(bad, map[string]any{"boundary": seq.BoundaryID, "reason": "stat missing"})
			continue
		}
		if st.AlignedCount != len(seq.Columns) || len(st.Aligned) != len(seq.Columns) {
			bad = append(bad, map[string]any{"boundary": seq.BoundaryID, "expected": len(seq.Columns), "actual": st.AlignedCount})
			continue
		}
		for i, c := range seq.Columns {
			a := st.Aligned[i]
			if a.X != c.X || a.Y != c.Y {
				bad = append(bad, map[string]any{"boundary": seq.BoundaryID, "index": i, "expected": c, "actual": a})
			}
		}
	}
	out.add(Check{Name: "shared-boundary-aligned-sequence", OK: len(bad) == 0, Detail: map[string]any{"sequences": len(p.Truth.AlignedSequences), "mismatches": bad}})
}

func checkFaults(p *model.Project, rep *engine.Report, out *Report) {
	stats := map[string]boundary.Stat{}
	for _, s := range rep.Boundaries {
		stats[s.BoundaryID] = s
	}
	bad := []any{}
	checked := 0
	for _, f := range p.Truth.Faults {
		checked++
		if b := p.Boundary(f.BoundaryID); b != nil {
			st, ok := stats[f.BoundaryID]
			if !ok {
				bad = append(bad, map[string]any{"fault": f.ID, "reason": "boundary stat missing"})
				continue
			}
			if st.Category != f.Category {
				bad = append(bad, map[string]any{"fault": f.ID, "field": "category", "expected": f.Category, "actual": st.Category, "code": st.Code})
			}
			if f.ExpectedCode != "" && st.Code != f.ExpectedCode {
				bad = append(bad, map[string]any{"fault": f.ID, "field": "code", "expected": f.ExpectedCode, "actual": st.Code})
			}
			if st.Position == nil || st.Position.X != f.X || st.Position.Y != f.Y {
				bad = append(bad, map[string]any{"fault": f.ID, "field": "position", "expected": [2]int64{f.X, f.Y}, "actual": st.Position})
			}
			continue
		}

		var probe *model.AttributionProbe
		for i := range p.Probes {
			if p.Probes[i].BoundaryID == f.BoundaryID {
				probe = &p.Probes[i]
				break
			}
		}
		if probe == nil {
			bad = append(bad, map[string]any{"fault": f.ID, "reason": "probe missing"})
			continue
		}
		st, perr := boundary.DiagnoseProbe(*probe)
		if st == nil {
			bad = append(bad, map[string]any{"fault": f.ID, "reason": perr})
			continue
		}
		if st.Category != f.Category {
			bad = append(bad, map[string]any{"fault": f.ID, "field": "probe.category", "expected": f.Category, "actual": st.Category, "code": st.Code})
		}
		if f.ExpectedCode != "" {
			if perr == nil || perr.Code != f.ExpectedCode {
				bad = append(bad, map[string]any{"fault": f.ID, "field": "probe.code", "expected": f.ExpectedCode, "actualError": perr, "statCode": st.Code})
			}
		}
		if st.Position == nil || st.Position.X != f.X || st.Position.Y != f.Y {
			bad = append(bad, map[string]any{"fault": f.ID, "field": "probe.position", "expected": [2]int64{f.X, f.Y}, "actual": st.Position})
		}
	}
	out.add(Check{Name: "fault-attribution-truth", OK: len(bad) == 0, Detail: map[string]any{"checked": checked, "mismatches": bad}})
}
