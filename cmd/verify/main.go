package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"feedback/maskworkbench/maskqa"
)

func main() {
	ctx := context.Background()
	set, err := maskqa.GenerateSet(maskqa.DefaultSeed)
	if err != nil {
		fatal("generate", err)
	}
	fmt.Printf("generated seed=%d samples=%d\n", set.Seed, len(set.Fixtures))

	cfg := maskqa.SchedulerConfig{MaxParallelism: 2, MaxPixelsPerWorker: 4096, QueueBudgetTiles: 2}
	allOK := true
	for _, fx := range set.Fixtures {
		fmt.Printf("\n[%s] %dx%d brush=%d strokes=%d truthDomains=%d segments=%d\n",
			fx.Name, fx.Project.Width, fx.Project.Height, fx.Project.BrushWidthPx,
			len(fx.Project.Strokes), len(fx.Truth.Domains), len(fx.Truth.Segments))
		for _, v := range []string{maskqa.SchemaV1, maskqa.SchemaV2, maskqa.SchemaV3} {
			p, err := maskqa.LoadProject(fx.Exports[v].Data)
			if err != nil {
				fatal("load "+v, err)
			}
			fmt.Printf("  schema %s -> source=%s strokes=%d residualDigest=%s\n", v, p.SourceVersion, len(p.Strokes), short(p.ResidualDigest))
		}

		eng, err := maskqa.NewEngine(fx.Project, fx.Truth, cfg, nil)
		if err != nil {
			fatal("engine", err)
		}
		res, err := eng.Run(ctx, maskqa.RunOptions{})
		if err != nil {
			fatal("verify", err)
		}
		if !res.Mask.Equal(fx.Truth.Mask) {
			tile, p, msg := maskqa.FirstMaskDifference(res.Mask, fx.Truth.Mask, eng.Tiles)
			fatal("pixel", fmt.Errorf("tile=%d point=%v %s", tile, p, msg))
		}
		fmt.Printf("  full-run: mask=EXACT maskHash=%s topology=OK direction=OK tiles=%d\n", short(res.Report.MaskHash), len(eng.Tiles))
		for _, d := range res.Report.Topology.Domains {
			fmt.Printf("    domain D%d area=%d holes=%d bbox=[%d,%d-%d,%d] ring=%.2f\n", d.DomainID, d.Area, d.Holes, d.MinX, d.MinY, d.MaxX, d.MaxY, d.RingWidthPx)
		}

		dir, err := os.MkdirTemp("", "maskqa-verify-paths-*")
		if err != nil {
			fatal("tempdir", err)
		}
		three, err := maskqa.RunThreePaths(ctx, fx.Project, fx.Truth, cfg, dir)
		if err != nil {
			fatal("three paths", err)
		}
		fmt.Printf("  three-paths: OK commits=%d maskHash=%s reportHash=%s recoveryBoundary=%d\n",
			three.Realtime.CommitCount, short(three.Realtime.MaskHash), short(three.Realtime.ReportHash),
			three.Recovery.Recovery.CommittedStrokeBoundary)
	}

	fx := set.Fixtures[0]
	dir, _ := os.MkdirTemp("", "maskqa-verify-edit-*")
	db := filepath.Join(dir, "edit.db")
	store, err := maskqa.OpenCheckpoint(db)
	if err != nil {
		fatal("checkpoint", err)
	}
	eng, _ := maskqa.NewEngine(fx.Project, fx.Truth, cfg, store)
	if _, err := eng.Run(ctx, maskqa.RunOptions{}); err != nil {
		fatal("baseline", err)
	}
	_ = store.Close()
	store2, err := maskqa.OpenCheckpoint(db)
	if err != nil {
		fatal("reopen", err)
	}
	edited := *fx.Project
	edited.Strokes = append([]maskqa.Stroke(nil), fx.Project.Strokes...)
	edited.Strokes[0].Start.X += 3
	plan, err := maskqa.PlanInvalidation(fx.Project, &edited, 1, eng.Tiles)
	if err != nil {
		fatal("invalidation", err)
	}
	incEng, _ := maskqa.NewEngine(&edited, fx.Truth, cfg, store2)
	inc, incErr := incEng.Run(ctx, maskqa.RunOptions{Invalidation: plan})
	if incErr != nil {
		if qe, ok := maskqa.AsError(incErr); !ok || (qe.Code != maskqa.CodeTOP && qe.Code != maskqa.CodeDIR) {
			fatal("incremental", incErr)
		}
	}
	fullEng, _ := maskqa.NewEngine(&edited, fx.Truth, cfg, nil)
	full, _ := fullEng.Run(ctx, maskqa.RunOptions{})
	if !inc.Mask.Equal(full.Mask) {
		fatal("incremental match", fmt.Errorf("incremental mask differs from full recompute"))
	}
	_ = store2.Close()
	fmt.Printf("\n[invalidation] stroke=1 dx=3 affected=%v recomputed=%v reused=%v incremental=full\n",
		plan.AffectedTiles, inc.RecomputedTiles, inc.ReusedTiles)

	if allOK {
		fmt.Println("\nVERIFY RESULT: ALL CHECKS PASSED")
	}
}

func short(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

func fatal(step string, err error) {
	if qe, ok := maskqa.AsError(err); ok {
		fmt.Fprintf(os.Stderr, "VERIFY FAILED at %s: %s path=%s code=%s details=%+v\n", step, qe.Error(), qe.Path, qe.Code, qe)
	} else {
		fmt.Fprintf(os.Stderr, "VERIFY FAILED at %s: %v\n", step, err)
	}
	os.Exit(1)
}
