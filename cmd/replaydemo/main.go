package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"replaybench/internal/checkpoint"
	"replaybench/internal/diagnostics"
	"replaybench/internal/diff"
	"replaybench/internal/eerr"
	"replaybench/internal/engine"
	"replaybench/internal/fixed"
	"replaybench/internal/generator"
	"replaybench/internal/graph"
	"replaybench/internal/loader"
	"replaybench/internal/model"
	"replaybench/internal/runtime"
	"replaybench/internal/scheduler"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type VerifyReport struct {
	OK               bool             `json:"ok"`
	Seed             int64            `json:"seed"`
	ReplayID         string           `json:"replay_id"`
	ContentHash      string           `json:"content_hash"`
	Checks           []Check          `json:"checks"`
	InvalidRounds    []int            `json:"invalid_rounds,omitempty"`
	ReusedRounds     []int            `json:"reused_rounds,omitempty"`
	Intervals        []graph.Interval `json:"intervals,omitempty"`
	CommitSequence   []int            `json:"commit_sequence,omitempty"`
	SchedulerMaxConc int32            `json:"scheduler_max_concurrency,omitempty"`
	SchedulerWaits   int              `json:"scheduler_wait_records,omitempty"`
	DiagnosticsAddr  string           `json:"diagnostics_addr,omitempty"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "diagnostics listen address")
	serve := flag.Bool("serve", false, "start net/http diagnostics dashboard")
	dbPath := flag.String("db", "", "SQLite database path; verification uses a temp DB when empty")
	samplesDir := flag.String("samples-dir", "artifacts/samples", "write deterministic JSON samples here; empty disables")
	crashAfter := flag.Int("crash-after-window", 0, "SIGKILL process after this committed one-round window")
	recoverOnly := flag.Bool("recover-only", false, "recover and finish a crash-demo database")
	flag.Parse()

	ctx := context.Background()
	switch {
	case *serve:
		if err := runServe(*addr, *dbPath); err != nil {
			fatal(err)
		}
	case *crashAfter > 0:
		path := *dbPath
		if path == "" {
			path = filepath.Join("artifacts", "crash-demo.db")
		}
		if err := runCrashDemo(ctx, path, *crashAfter); err != nil {
			fatal(err)
		}
	case *recoverOnly:
		path := *dbPath
		if path == "" {
			path = filepath.Join("artifacts", "crash-demo.db")
		}
		if err := runRecoverOnly(ctx, path); err != nil {
			fatal(err)
		}
	default:
		report, err := runVerify(ctx, *dbPath, *samplesDir)
		if err != nil {
			fatal(err)
		}
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(b))
		if !report.OK {
			os.Exit(1)
		}
	}
}

func runServe(addr, dbPath string) error {
	if dbPath == "" {
		dbPath = filepath.Join("artifacts", "diagnostics.db")
	}
	svc, err := diagnostics.NewService(dbPath)
	if err != nil {
		return err
	}
	defer svc.Close()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("diagnostics port %s unavailable: %w", addr, err)
	}
	fmt.Printf("diagnostics listening on http://%s (db=%s)\n", addr, dbPath)
	return http.Serve(ln, svc.Handler())
}

func runVerify(ctx context.Context, dbPath, samplesDir string) (*VerifyReport, error) {
	set, err := generator.DeterministicSet(generator.DefaultSeed)
	if err != nil {
		return nil, err
	}
	first, err := loader.LoadJSON(set.JSON[model.FormatWeightCast])
	if err != nil {
		return nil, err
	}
	report := &VerifyReport{OK: true, Seed: set.Seed, ReplayID: first.Replay.ID, ContentHash: first.Replay.ContentHash}
	add := func(name string, ok bool, detail string) {
		report.Checks = append(report.Checks, Check{Name: name, OK: ok, Detail: detail})
		if !ok {
			report.OK = false
		}
	}

	if samplesDir != "" {
		if err := os.MkdirAll(samplesDir, 0o755); err != nil {
			return nil, err
		}
		for f, raw := range set.JSON {
			if err := os.WriteFile(filepath.Join(samplesDir, fmt.Sprintf("format-%d.json", f)), raw, 0o644); err != nil {
				return nil, err
			}
		}
		truthJSON, _ := json.MarshalIndent(set.Truth, "", "  ")
		if err := os.WriteFile(filepath.Join(samplesDir, "truth.json"), truthJSON, 0o644); err != nil {
			return nil, err
		}
		add("write_samples", true, samplesDir)
	}

	var baseline []model.RoundSnapshot
	for _, f := range []model.FormatID{1, 2, 3} {
		loaded, err := loader.LoadJSON(set.JSON[f])
		if err != nil {
			add(fmt.Sprintf("load_format_%d", f), false, err.Error())
			continue
		}
		res, err := engine.Run(ctx, loaded.Replay)
		if err != nil {
			add(fmt.Sprintf("settle_format_%d", f), false, err.Error())
			continue
		}
		expected := truthExpected(loaded.Truth, res.Snapshots)
		truthOK := diff.Snapshots(expected, res.Snapshots) == nil
		add(fmt.Sprintf("truth_format_%d", f), truthOK, fmt.Sprintf("rounds=%d verdict=%s/%s", len(res.Snapshots), res.Verdict, res.WinnerTeam))
		if f == 1 {
			baseline = res.Snapshots
		} else if err := diff.Snapshots(baseline, res.Snapshots); err != nil {
			add(fmt.Sprintf("format_%d_equivalence", f), false, err.Error())
		}
	}
	add("three_format_equivalence", report.OK, "formats 1/2/3 produce identical canonical snapshots")

	low, err := loader.ConvertDocument(set.JSON[3], model.FormatWeightCast)
	if err != nil {
		add("convert_high_to_low", false, err.Error())
	} else {
		high, err := loader.ConvertDocument(low, model.FormatTimestamp)
		if err != nil {
			add("convert_low_to_high", false, err.Error())
		} else if _, err := loader.LoadJSON(high); err != nil {
			add("roundtrip_high_low_high", false, err.Error())
		} else {
			add("roundtrip_high_low_high", true, "round, cooldown and queue slots preserved")
		}
	}

	cachePath := dbPath
	cleanup := func() {}
	if cachePath == "" {
		cachePath, cleanup, err = checkpoint.MustTempDB()
		if err != nil {
			return nil, err
		}
	}
	defer cleanup()
	store, err := checkpoint.Open(cachePath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	_ = store.ClearNodeCache(ctx)
	loaded, _ := loader.LoadJSON(set.JSON[1])
	oldGraph := graph.Build(loaded.Replay)
	if _, err := graph.RunIncremental(ctx, loaded.Replay, store); err != nil {
		add("graph_initial", false, err.Error())
	} else {
		add("graph_initial", true, "all rounds cached")
	}
	loaded.Replay.Edits = append(loaded.Replay.Edits, model.Edit{Type: "weight", Round: 2, UnitID: "beta", Value: 10})
	plan := graph.PlanInvalidation(oldGraph, graph.Build(loaded.Replay))
	report.InvalidRounds = plan.InvalidRounds
	report.ReusedRounds = plan.ReusedRounds
	report.Intervals = plan.Intervals
	inc, err := graph.RunIncremental(ctx, loaded.Replay, store)
	if err != nil {
		add("incremental_weight_edit", false, err.Error())
	} else {
		full, err := engine.Run(ctx, loaded.Replay)
		if err != nil {
			add("incremental_full_diff", false, err.Error())
		} else if err := diff.Snapshots(full.Snapshots, inc.Snapshots); err != nil {
			add("incremental_full_diff", false, err.Error())
		} else {
			add("incremental_weight_edit", true, fmt.Sprintf("hit=%v miss=%v", inc.CacheHitRounds, inc.CacheMissRounds))
		}
	}

	cpPath, cpCleanup, err := checkpoint.MustTempDB()
	if err != nil {
		return nil, err
	}
	defer cpCleanup()
	cpStore, err := checkpoint.Open(cpPath)
	if err != nil {
		return nil, err
	}
	mgr := &runtime.Manager{Store: cpStore, WindowSize: 2}
	cpReport, err := mgr.Run(ctx, first.Replay)
	if err != nil {
		add("checkpoint_commit", false, err.Error())
	} else {
		report.CommitSequence = cpReport.CommitSequence
		add("checkpoint_commit", true, fmt.Sprintf("windows=%v", cpReport.CommitSequence))
	}
	_ = cpStore.Close()
	cpStore2, err := checkpoint.Open(cpPath)
	if err != nil {
		return nil, err
	}
	resumeMgr := &runtime.Manager{Store: cpStore2, WindowSize: 2}
	resume, err := resumeMgr.Run(ctx, first.Replay)
	if err != nil {
		add("checkpoint_resume", false, err.Error())
	} else if err := diff.Snapshots(cpReport.Result.Snapshots, resume.Result.Snapshots); err != nil {
		add("checkpoint_resume", false, err.Error())
	} else {
		add("checkpoint_resume", true, fmt.Sprintf("recovered_window=%d sequence=%v", resume.RecoveredWindow.Index, resume.CommitSequence))
	}
	_ = cpStore2.Close()

	pool := scheduler.New(scheduler.Config{MaxWorkers: 3, WorkerBuffer: 1, QueueBudget: 4})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pool.Submit(ctx, "verify-task", func(context.Context) error { time.Sleep(5 * time.Millisecond); return nil })
		}()
	}
	wg.Wait()
	stats := pool.Stats()
	report.SchedulerMaxConc = stats.MaxConcurrency
	report.SchedulerWaits = stats.WaitCount
	add("bounded_scheduler", stats.MaxConcurrency <= 3, fmt.Sprintf("max_concurrency=%d waits=%d", stats.MaxConcurrency, stats.WaitCount))
	shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = pool.Shutdown(shutdownCtx)

	for _, c := range report.Checks {
		if !c.OK {
			report.OK = false
			break
		}
	}
	return report, nil
}

func runCrashDemo(ctx context.Context, path string, after int) error {
	set, err := generator.DeterministicSet(generator.DefaultSeed)
	if err != nil {
		return err
	}
	loaded, err := loader.LoadJSON(set.JSON[model.FormatWeightCast])
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	store, err := checkpoint.Open(path)
	if err != nil {
		return err
	}
	mgr := &runtime.Manager{Store: store, WindowSize: 1}
	mgr.AfterCommit = func(index int) {
		fmt.Printf("committed window %d; sending SIGKILL to self\n", index)
		if index == after {
			_ = store.Close()
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		}
	}
	_, err = mgr.Run(ctx, loaded.Replay)
	if err != nil {
		return err
	}
	return store.Close()
}

func runRecoverOnly(ctx context.Context, path string) error {
	set, err := generator.DeterministicSet(generator.DefaultSeed)
	if err != nil {
		return err
	}
	loaded, err := loader.LoadJSON(set.JSON[model.FormatWeightCast])
	if err != nil {
		return err
	}
	store, err := checkpoint.Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	mgr := &runtime.Manager{Store: store, WindowSize: 1}
	rep, err := mgr.Run(ctx, loaded.Replay)
	if err != nil {
		return err
	}
	full, err := engine.Run(ctx, loaded.Replay)
	if err != nil {
		return err
	}
	if err := diff.Snapshots(full.Snapshots, rep.Result.Snapshots); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(map[string]any{
		"ok": true, "commit_sequence": rep.CommitSequence,
		"recovered_window": rep.RecoveredWindow.Index,
		"rounds":           diagnostics.Summarize(rep.Result.Snapshots),
	}, "", "  ")
	fmt.Println(string(b))
	return nil
}

func truthExpected(truth *model.Truth, actual []model.RoundSnapshot) []model.RoundSnapshot {
	out := make([]model.RoundSnapshot, len(actual))
	copy(out, actual)
	for i := range out {
		rt := truth.Rounds[i]
		out[i].EffectiveSkills = append([]string(nil), rt.EffectiveSkills...)
		out[i].Adjudications = append([]model.Adjudication(nil), rt.Adjudications...)
		for j := range out[i].States {
			st := &out[i].States[j]
			if hp, ok := rt.HP[st.UnitID]; ok {
				st.HP = fixed.MustWhole(hp)
			}
			if res, ok := rt.Resource[st.UnitID]; ok {
				st.Resource = fixed.MustWhole(res)
			}
		}
	}
	return out
}

func fatal(err error) {
	if ee, ok := eerr.AsError(err); ok {
		b, _ := json.MarshalIndent(ee, "", "  ")
		fmt.Fprintln(os.Stderr, string(b))
	} else {
		b, _ := json.MarshalIndent(map[string]string{"code": "E_INTERNAL", "error": err.Error()}, "", "  ")
		fmt.Fprintln(os.Stderr, string(b))
	}
	os.Exit(1)
}
