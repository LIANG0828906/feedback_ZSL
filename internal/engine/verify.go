package engine

import (
	"context"
	"encoding/json"

	"terrainpatch/internal/model"
	"terrainpatch/internal/store"
)

type ThreeWayReport struct {
	Equal           bool            `json:"equal"`
	FirstDifference *Difference     `json:"firstDifference,omitempty"`
	CommitSequence  []string        `json:"commitSequence"`
	Draft           *Report         `json:"draft"`
	Recovery        *Report         `json:"recovery"`
	Full            *Report         `json:"full"`
	RecoveryInfo    *RecoveryResult `json:"recoveryInfo"`
	SQLitePath      string          `json:"sqlitePath,omitempty"`
}

func CloneProject(p *model.Project) *model.Project {
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	var out model.Project
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	out.Dir = p.Dir
	return &out
}

func VerificationEdits(p *model.Project) []EditSpec {
	center := p.Tile("T11")
	height := int64(52_000)
	if center != nil {
		gx, gy := center.Grid.Width/2, center.Grid.Height/2
		height = center.Grid.Heights[gy][gx] + 300
		if height > center.HeightMax {
			height = center.HeightMax
		}
	}
	return []EditSpec{
		{TileID: "T00", Field: "tolerance", IntValue: 100},
		{TileID: "T10", Field: "sampleBudget", IntValue: 120},
		{TileID: "T11", Field: "height", IntValue: height},
	}
}

func VerifyThreePaths(ctx context.Context, base *model.Project, edits []EditSpec, sqlitePath string, opts Options) (*ThreeWayReport, *model.AppError) {
	workers := opts.Workers
	if workers <= 0 {
		workers = 2
	}
	bufferCap := opts.VertexBufferCap
	if bufferCap <= 0 {
		bufferCap = 4096
	}
	memBudget := opts.MemoryBudgetBytes
	if memBudget <= 0 {
		memBudget = 16 << 20
	}

	draftProject := CloneProject(base)
	draftEngine := NewEngine(draftProject, Options{Workers: workers, VertexBufferCap: bufferCap, MemoryBudgetBytes: memBudget, Store: store.NewMemoryStore()})
	if _, err := draftEngine.Recover(ctx); err != nil {
		return nil, err
	}
	if _, err := draftEngine.RebuildAll(ctx, "draft-base"); err != nil {
		return nil, err
	}
	var draftReport *Report
	var err *model.AppError
	for _, edit := range edits {
		draftReport, err = draftEngine.ApplyEdit(ctx, edit)
		if err != nil {
			return nil, err
		}
	}
	if draftReport == nil {
		draftReport, err = draftEngine.RebuildAll(ctx, "draft-final")
		if err != nil {
			return nil, err
		}
	}

	fullProject := CloneProject(base)
	for _, edit := range edits {
		if perr := ApplyEditSpec(fullProject, edit); perr != nil {
			return nil, perr
		}
	}
	fullEngine := NewEngine(fullProject, Options{Workers: workers, VertexBufferCap: bufferCap, MemoryBudgetBytes: memBudget, Store: store.NewMemoryStore()})
	fullReport, ferr := fullEngine.RebuildAll(ctx, "full")
	if ferr != nil {
		return nil, ferr
	}

	if sqlitePath == "" {
		sqlitePath = ":memory:"
	}
	firstStore, serr := store.NewSQLite(sqlitePath)
	if serr != nil {
		return nil, model.NewError(model.CodeInternal, "open recovery sqlite failed: "+serr.Error())
	}
	recoveryProject := CloneProject(base)
	firstEngine := NewEngine(recoveryProject, Options{
		Workers: workers, VertexBufferCap: bufferCap, MemoryBudgetBytes: memBudget, Store: firstStore,
	})
	if _, rerr := firstEngine.Recover(ctx); rerr != nil {
		return nil, rerr
	}
	if _, rerr := firstEngine.RebuildAll(ctx, "recovery-base"); rerr != nil {
		return nil, rerr
	}
	var firstEdit EditSpec
	if len(edits) > 0 {
		firstEdit = edits[0]
		firstEngine.SetInterruptAfterCommits(1)
		_, _ = firstEngine.ApplyEdit(ctx, firstEdit) // intentionally interrupted after first durable commit
	}
	_ = firstStore.Close()

	secondStore, oerr := store.NewSQLite(sqlitePath)
	if oerr != nil {
		return nil, model.NewError(model.CodeInternal, "reopen recovery sqlite failed: "+oerr.Error())
	}
	defer secondStore.Close()
	recoveryEngine := NewEngine(recoveryProject, Options{Workers: workers, VertexBufferCap: bufferCap, MemoryBudgetBytes: memBudget, Store: secondStore})
	recoveryInfo, rerr := recoveryEngine.Recover(ctx)
	if rerr != nil {
		return nil, rerr
	}
	recoveryReport := recoveryEngine.SnapshotReport("recovery-recovered")
	if len(edits) > 0 {
		recoveryReport, rerr = recoveryEngine.RunImpact(ctx, firstEdit.TileID, firstEdit.Field, "recovery-realign")
		if rerr != nil {
			return nil, rerr
		}
		for _, edit := range edits[1:] {
			recoveryReport, rerr = recoveryEngine.ApplyEdit(ctx, edit)
			if rerr != nil {
				return nil, rerr
			}
		}
	} else {
		recoveryReport, rerr = recoveryEngine.RebuildAll(ctx, "recovery-full")
		if rerr != nil {
			return nil, rerr
		}
	}

	out := &ThreeWayReport{
		Equal: true, CommitSequence: fullReport.CommitSequence,
		Draft: draftReport, Recovery: recoveryReport, Full: fullReport,
		RecoveryInfo: recoveryInfo, SQLitePath: sqlitePath,
	}
	if d := CompareReports(fullReport, draftReport); d != nil {
		out.Equal = false
		out.FirstDifference = d
		return out, model.NewError(model.CodeDiff, "draft incremental differs from full rebuild").
			WithTile(d.TileID, d.Field).WithDetails(map[string]any{"difference": d})
	}
	if d := CompareReports(fullReport, recoveryReport); d != nil {
		out.Equal = false
		out.FirstDifference = d
		return out, model.NewError(model.CodeDiff, "checkpoint recovery differs from full rebuild").
			WithTile(d.TileID, d.Field).WithDetails(map[string]any{"difference": d})
	}
	return out, nil
}

func (e *Engine) RunImpact(ctx context.Context, tileID, field, kind string) (*Report, *model.AppError) {
	impact := e.g.Impact(tileID, field)
	return e.run(ctx, impact, kind)
}
