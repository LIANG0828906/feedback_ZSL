// Command demo is the local, offline demonstration entry point. It loads one
// logical project in all three schema dialects, prints per-tick gate and
// cold-start verdicts, runs an incremental edit and demonstrates checkpoint
// recovery against the reference truth. No network or container is used.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"coldkernel/internal/checkpoint"
	"coldkernel/internal/engine"
	"coldkernel/internal/fixedpoint"
	"coldkernel/internal/incremental"
	"coldkernel/internal/model"
	"coldkernel/internal/oracle"
	"coldkernel/internal/runner"
	"coldkernel/internal/sample"
	"coldkernel/internal/testutil"
)

func main() {
	seed := flag.Uint64("seed", 20261126, "deterministic generator seed")
	ticks := flag.Int("ticks", 6, "number of ticks to print in detail")
	cpDir := flag.String("checkpoint-dir", "testdata/checkpoints", "checkpoint directory")
	inject := flag.Bool("inject", false, "run the exception-injection probes")
	flag.Parse()
	if *inject {
		triple, err := sample.Build(*seed)
		if err != nil {
			fmt.Fprintf(os.Stderr, "demo: %v\n", err)
			os.Exit(1)
		}
		if err := runInjection(triple.RawV1); err != nil {
			fmt.Fprintf(os.Stderr, "demo: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(*seed, *ticks, *cpDir); err != nil {
		fmt.Fprintf(os.Stderr, "demo: %v\n", err)
		os.Exit(1)
	}
}

func run(seed uint64, showTicks int, cpDir string) error {
	triple, err := sample.Build(seed)
	if err != nil {
		return err
	}
	projects := []*model.Project{triple.ProjectV1, triple.ProjectV2, triple.ProjectV3}

	fmt.Println("== 1. Three-schema loading ==")
	testutil.AssertProjectsEqual(&testingT{}, projects...)
	for _, p := range projects {
		fmt.Printf("schema %s: functions=%d requests=%d horizon=%d ticks\n",
			p.SchemaVersion, len(p.Functions), len(p.Requests), p.HorizonTicks)
	}

	fmt.Println("\n== 2. Per-tick gate / queue / cold-start (schema v1) ==")
	ref := oracle.Simulate(triple.ProjectV1)
	limit := showTicks
	if int64(limit) > ref.Ticks[len(ref.Ticks)-1].Tick+1 {
		limit = len(ref.Ticks)
	}
	for ti := 0; ti < limit; ti++ {
		printTick(triple.ProjectV1, ref.Ticks[ti])
	}

	fmt.Println("\n== 3. Cross-version reference equality ==")
	for _, p := range projects[1:] {
		got := oracle.Simulate(p)
		if err := testutil.AssertRunsEqual("cross-version", ref, got); err != nil {
			return err
		}
		fmt.Printf("schema %s matches v1 tick-by-tick\n", p.SchemaVersion)
	}

	fmt.Println("\n== 4. Parallel engine determinism ==")
	for _, cfg := range []engine.Config{
		{Workers: 1, FuncBuffer: 8, SubmitBatch: 1},
		{Workers: 4, FuncBuffer: 8, SubmitBatch: 2},
	} {
		got, timings, err := engine.Run(triple.ProjectV1, cfg)
		if err != nil {
			return err
		}
		if err := testutil.AssertRunsEqual("engine", ref, got); err != nil {
			return err
		}
		fmt.Printf("workers=%d buffer=%d: identical verdicts (%d timing diagnostics)\n",
			cfg.Workers, cfg.FuncBuffer, len(timings))
	}

	fmt.Println("\n== 4b. Fixed-point critical-case tolerance ==")
	critical := fixedpoint.CriticalCases(triple.ProjectV1.ConversionWidth,
		triple.ProjectV1.TickMs, triple.ProjectV1.FixedTolerance)
	for _, c := range critical {
		mark := "OK"
		if !c.WithinTolerance() {
			mark = "OUT_OF_TOLERANCE"
		}
		fmt.Printf("  %-28s got=%d want=%d err=%d tol=%d %s\n",
			c.Name, c.Got, c.Want, c.Error, c.Tolerance, mark)
	}
	if !fixedpoint.AllWithinTolerance(critical) {
		return fmt.Errorf("fixed-point tolerance breached")
	}

	fmt.Println("\n== 5. Incremental edit (raise fn-b limit at tick 12) ==")
	inc := incremental.NewEngine(sample.CloneProject(triple.ProjectV1))
	if _, err := inc.Baseline(); err != nil {
		return err
	}
	inc.ApplyLimit("fn-b", triple.ProjectV1.Functions[1].ConcurrencyLimit+2)
	incRes, stats, err := inc.Run([]incremental.Edit{{
		Kind: incremental.EditLimit, FunctionID: "fn-b", FromTick: 12,
	}})
	if err != nil {
		return err
	}
	edited := sample.CloneProject(triple.ProjectV1)
	edited.Functions[1].ConcurrencyLimit += 2
	full := oracle.Simulate(edited)
	if err := testutil.AssertRunsEqual("incremental", full, incRes); err != nil {
		return err
	}
	fmt.Printf("recomputed cells=%d reused cells=%d, matches full recompute\n",
		stats.Recomputed, stats.Reused)

	fmt.Println("\n== 6. Checkpoint commit & recovery ==")
	if err := os.MkdirAll(cpDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(cpDir, "demo.db")
	_ = os.Remove(path)
	store, err := checkpoint.Open(path, triple.ProjectV1.SourceVersion+"#demo")
	if err != nil {
		return err
	}
	r, err := runner.New(triple.ProjectV1, store)
	if err != nil {
		return err
	}
	if _, err := r.Run(context.Background()); err != nil {
		return err
	}
	last, schema, source, err := store.LatestTick()
	if err != nil {
		return err
	}
	fmt.Printf("committed through tick %d (%s, %s)\n", last, schema, source)

	r2, err := runner.New(triple.ProjectV1, store)
	if err != nil {
		return err
	}
	fmt.Printf("restart resumes at tick %d (horizon=%d)\n",
		r2.StartTick(), triple.ProjectV1.HorizonTicks)
	states, _, err := store.Restore(last)
	if err != nil {
		return err
	}
	for _, fn := range triple.ProjectV1.Functions {
		want := ref.Ticks[last].Functions[fn.ID]
		got := states[fn.ID]
		if got.ColdCount != want.ColdCount || got.ReuseCount != want.ReuseCount ||
			got.BudgetConsumed != want.BudgetConsumed {
			return fmt.Errorf("recovery mismatch on %s", fn.ID)
		}
	}
	fmt.Println("recovered cold/reuse/budget metrics match the reference truth")
	if err := store.Close(); err != nil {
		return err
	}

	fmt.Println("\nAll local acceptance checks completed successfully.")
	return nil
}

func printTick(p *model.Project, tr *model.TickResult) {
	fmt.Printf("tick %d: admit=%s queue=%s reject=%s\n",
		tr.Tick, ids(tr.Admitted), ids(tr.Queued), ids(tr.Rejected))
	for _, fn := range sortedStates(tr) {
		decl, _ := p.FunctionByID(fn.FunctionID)
		freeSlots := decl.ConcurrencyLimit - fn.Occupied
		fmt.Printf("    %s occupied=%d free_slots=%d warm=%d inflight=%d cold=%d reuse=%d budget_used=%d budget_left=%d queue=%d\n",
			fn.FunctionID, fn.Occupied, freeSlots, fn.WarmIdle, fn.InFlightCold,
			fn.ColdCount, fn.ReuseCount, fn.BudgetConsumed, fn.BudgetRemaining, fn.QueueLen)
	}
}

func ids(os []*model.RequestOutcome) string {
	if len(os) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(os))
	for _, o := range os {
		tag := string(o.Decision)
		if o.ColdStarted {
			tag += "+cold"
		}
		if o.ReusedWarm {
			tag += "+warm"
		}
		parts = append(parts, o.RequestID+":"+tag)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func sortedStates(tr *model.TickResult) []*model.FunctionTickState {
	out := make([]*model.FunctionTickState, 0, len(tr.Functions))
	for _, st := range tr.Functions {
		out = append(out, st)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].FunctionID > out[j].FunctionID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// testingT adapts the demo to the testutil equality helper without pulling
// in *testing.T; it panics with a descriptive message on inequality.
type testingT struct{}

func (testingT) Helper()                    {}
func (testingT) Fatalf(format string, a ...any) {
	panic(fmt.Sprintf(format, a...))
}
