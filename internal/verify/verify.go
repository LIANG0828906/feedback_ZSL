package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync/atomic"
	"time"

	"retargetlab/internal/engine"
	"retargetlab/internal/fsm"
	"retargetlab/internal/model"
	"retargetlab/internal/solver"
	"retargetlab/internal/store"
)

type Check struct {
	Name     string         `json:"name"`
	Passed   bool           `json:"passed"`
	Duration string         `json:"duration"`
	Detail   map[string]any `json:"detail,omitempty"`
	Error    string         `json:"error,omitempty"`
}

type Report struct {
	Passed     bool    `json:"passed"`
	Target     string  `json:"target"`
	FrameCount int     `json:"frameCount"`
	Checks     []Check `json:"checks"`
}

func Run(ctx context.Context, ws model.Workspace, targetName, tempDir string) Report {
	freshDir, err := os.MkdirTemp(tempDir, "retargetlab-verify-")
	if err == nil {
		defer os.RemoveAll(freshDir)
		tempDir = freshDir
	}
	report := Report{Target: targetName, FrameCount: len(ws.Clip.Frames)}
	target, ok := ws.TargetByName(targetName)
	if !ok {
		report.Checks = append(report.Checks, Check{Name: "target_exists", Passed: false, Error: "target not found"})
		return report
	}
	truth, hasTruth := ws.TruthByTarget(targetName)

	validation := runCheck("workspace_validation", func() (map[string]any, error) {
		return map[string]any{"joints": len(target.Skeleton.Joints), "frames": len(ws.Clip.Frames)}, model.ValidateWorkspace(ws)
	})
	report.Checks = append(report.Checks, validation)
	if !validation.Passed {
		return report
	}

	fullEngine := mustEngine(target, ws, 4, nil)
	report.Checks = append(report.Checks, runCheck("full_solve_and_truth", func() (map[string]any, error) {
		if _, err := fullEngine.SolveAll(ctx); err != nil {
			return nil, err
		}
		results := fullEngine.CommittedResults()
		frames := sortedResults(results)
		if hasTruth {
			if !reflect.DeepEqual(truth.Contacts, ws.Clip.Contacts) {
				return nil, fmt.Errorf("truth contact intervals differ from generated clip")
			}
			if err := solver.CompareTruthWithSource(ws.Source, target.Skeleton, truth, frames); err != nil {
				return nil, err
			}
		}
		return map[string]any{"frames": len(frames), "truth": hasTruth}, nil
	}))

	report.Checks = append(report.Checks, runCheck("mapping_edit_incremental_vs_full", func() (map[string]any, error) {
		e := mustEngine(target, ws, 4, nil)
		rule := findRule(ws.Mapping, "upperarm_l")
		if rule < 0 {
			return nil, model.Err(model.E_SCHEMA, "verify", "upperarm_l mapping rule not found")
		}
		replacement := ws.Mapping.Rules[rule]
		replacement.Source = "forearm_l"
		inv, err := e.ApplyMappingRule(rule, replacement)
		if err != nil {
			return nil, err
		}
		if _, err := e.SolveAll(ctx); err != nil {
			return nil, err
		}
		compare, err := e.CompareWithFull(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"invalidatedFrames": inv.Frames, "comparedFrames": compare.FramesCompared}, nil
	}))

	report.Checks = append(report.Checks, runCheck("scale_edit_cache_reuse", func() (map[string]any, error) {
		e := mustEngine(target, ws, 4, nil)
		half := len(ws.Clip.Frames) / 2
		if _, err := e.SolveRange(ctx, 0, half-1); err != nil {
			return nil, err
		}
		inv, err := e.SetScale("gesture", 1.08)
		if err != nil {
			return nil, err
		}
		run, err := e.SolveAll(ctx)
		if err != nil {
			return nil, err
		}
		if run.CacheHits != half {
			return nil, fmt.Errorf("expected %d cache hits, got %d", half, run.CacheHits)
		}
		if run.CacheMisses != len(ws.Clip.Frames)-half {
			return nil, fmt.Errorf("expected %d cache misses, got %d", len(ws.Clip.Frames)-half, run.CacheMisses)
		}
		fresh := mustEngine(target, ws, 1, nil)
		if _, err := fresh.SetScale("gesture", 1.08); err != nil {
			return nil, err
		}
		if _, err := fresh.SolveRange(ctx, half, len(ws.Clip.Frames)-1); err != nil {
			return nil, err
		}
		left := e.CommittedResults()
		right := fresh.CommittedResults()
		for frame := half; frame < len(ws.Clip.Frames); frame++ {
			if err := solver.CompareFrameResults(left[frame], right[frame]); err != nil {
				return nil, err
			}
		}
		return map[string]any{
			"invalidatedFrames": inv.Frames, "cacheHits": run.CacheHits,
			"cacheMisses": run.CacheMisses, "comparedFrames": len(ws.Clip.Frames) - half,
		}, nil
	}))

	report.Checks = append(report.Checks, runCheck("parallel_serial_equivalence", func() (map[string]any, error) {
		serial := mustEngine(target, ws, 1, nil)
		parallel := mustEngine(target, ws, 4, nil)
		for _, e := range []*engine.Engine{serial, parallel} {
			rule := findRule(ws.Mapping, "upperarm_r")
			replacement := ws.Mapping.Rules[rule]
			replacement.Source = "forearm_r"
			if _, err := e.ApplyMappingRule(rule, replacement); err != nil {
				return nil, err
			}
			if _, err := e.SetScale("stride", 1.03); err != nil {
				return nil, err
			}
			if _, err := e.SolveAll(ctx); err != nil {
				return nil, err
			}
		}
		l, r := serial.CommittedResults(), parallel.CommittedResults()
		for frame := range ws.Clip.Frames {
			if err := solver.CompareFrameResults(l[frame], r[frame]); err != nil {
				return nil, err
			}
		}
		sw, pw := serial.StateSnapshot().Windows, parallel.StateSnapshot().Windows
		if len(sw) != len(pw) {
			return nil, fmt.Errorf("window count differs: %d vs %d", len(sw), len(pw))
		}
		for i := range sw {
			if sw[i].ContentHash != pw[i].ContentHash || sw[i].StartFrame != pw[i].StartFrame || sw[i].EndFrame != pw[i].EndFrame {
				return nil, fmt.Errorf("window %d commit sequence differs", i)
			}
		}
		return map[string]any{"frames": len(ws.Clip.Frames), "windows": len(sw)}, nil
	}))

	report.Checks = append(report.Checks, runCheck("sqlite_recovery", func() (map[string]any, error) {
		dbPath := filepath.Join(tempDir, "recovery.db")
		db, err := store.Open(dbPath)
		if err != nil {
			return nil, err
		}
		e := mustEngine(target, ws, 3, db)
		prefix := len(ws.Clip.Frames) * 3 / 4
		if _, err := e.SolveRange(ctx, 0, prefix-1); err != nil {
			_ = db.Close()
			return nil, err
		}
		if err := db.Close(); err != nil {
			return nil, err
		}
		db2, err := store.Open(dbPath)
		if err != nil {
			return nil, err
		}
		defer db2.Close()
		e2 := mustEngine(target, ws, 3, db2)
		rec, err := e2.Recover(ctx)
		if err != nil {
			return nil, err
		}
		if rec.LoadedFrames != prefix || rec.Invalidated {
			return nil, fmt.Errorf("unexpected recovery: %+v", rec)
		}
		if _, err := e2.SolveAll(ctx); err != nil {
			return nil, err
		}
		compare, err := e2.CompareWithFull(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"loadedFrames": rec.LoadedFrames, "loadedWindows": rec.LoadedWindows,
			"comparedFrames": compare.FramesCompared,
		}, nil
	}))

	report.Checks = append(report.Checks, runCheck("version_mismatch_wipes_all", func() (map[string]any, error) {
		dbPath := filepath.Join(tempDir, "version.db")
		db, err := store.Open(dbPath)
		if err != nil {
			return nil, err
		}
		e := mustEngine(target, ws, 2, db)
		if _, err := e.SolveRange(ctx, 0, min(7, len(ws.Clip.Frames)-1)); err != nil {
			_ = db.Close()
			return nil, err
		}
		if err := db.Close(); err != nil {
			return nil, err
		}
		db2, err := store.Open(dbPath)
		if err != nil {
			return nil, err
		}
		defer db2.Close()
		badParams := solver.DefaultParams()
		badParams.ParamVersion = 999
		targetCopy := target
		bad, err := engine.New(engine.Config{
			Source: ws.Source, Clip: ws.Clip, Target: targetCopy, Mapping: ws.Mapping,
			Params: badParams, Store: db2, WindowSize: 4, MaxWorkers: 2,
		})
		if err != nil {
			return nil, err
		}
		rec, err := bad.Recover(ctx)
		if err != nil {
			return nil, err
		}
		if !rec.Invalidated || rec.LoadedFrames != 0 {
			return nil, fmt.Errorf("version mismatch was not rejected: %+v", rec)
		}
		count, err := db2.Count(ctx)
		if err != nil {
			return nil, err
		}
		if count != 0 {
			return nil, fmt.Errorf("%d stale windows survived invalidation", count)
		}
		if _, err := bad.SolveAll(ctx); err != nil {
			return nil, err
		}
		compare, err := bad.CompareWithFull(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"invalidated": rec.Invalidated, "reason": rec.Reason,
			"remainingWindows": count, "recomputedFrames": compare.FramesCompared,
		}, nil
	}))

	report.Checks = append(report.Checks, runCheck("failure_retry_isolated", func() (map[string]any, error) {
		var fail atomic.Bool
		fail.Store(true)
		e, err := engine.New(engine.Config{
			Source: ws.Source, Clip: ws.Clip, Target: target, Mapping: ws.Mapping,
			Params: solver.DefaultParams(), MaxWorkers: 4, WindowSize: 4,
			FrameFault: func(frame int) error {
				if frame == 5 && fail.Load() {
					return errors.New("verification-injected fault")
				}
				return nil
			},
		})
		if err != nil {
			return nil, err
		}
		if _, err := e.SolveAll(ctx); err != nil {
			return nil, err
		}
		if state, _ := e.State(5); state != fsm.Failed {
			return nil, fmt.Errorf("frame 5 state=%s", state)
		}
		for _, frame := range []int{0, 1, 2, 3, 4, 6, 7} {
			if state, _ := e.State(frame); state != fsm.Committed {
				return nil, fmt.Errorf("frame %d polluted: %s", frame, state)
			}
		}
		fail.Store(false)
		if _, err := e.RetryFrame(ctx, 5); err != nil {
			return nil, err
		}
		if state, _ := e.State(5); state != fsm.Committed {
			return nil, fmt.Errorf("retried frame state=%s", state)
		}
		return map[string]any{"failedFrame": 5, "retry": "committed"}, nil
	}))

	for _, check := range report.Checks {
		if !check.Passed {
			report.Passed = false
			return report
		}
	}
	report.Passed = true
	return report
}

func mustEngine(target model.TargetVariant, ws model.Workspace, workers int, db *store.Store) *engine.Engine {
	e, err := engine.New(engine.Config{
		Source: ws.Source, Clip: ws.Clip, Target: target, Mapping: ws.Mapping,
		Params: solver.DefaultParams(), Store: db, WindowSize: 4, MaxWorkers: workers,
		QueuePerWorker: 2, BufferPerWorker: 2,
	})
	if err != nil {
		panic(err)
	}
	return e
}

func runCheck(name string, fn func() (map[string]any, error)) Check {
	start := time.Now()
	detail, err := fn()
	c := Check{Name: name, Passed: err == nil, Duration: time.Since(start).String(), Detail: detail}
	if err != nil {
		var ce *model.CodedError
		if errors.As(err, &ce) {
			c.Error = fmt.Sprintf("%s %s", ce.Code, ce.Error())
		} else {
			c.Error = err.Error()
		}
	}
	return c
}

func findRule(m model.Mapping, target string) int {
	for i, r := range m.Rules {
		for _, name := range r.Targets {
			if name == target {
				return i
			}
		}
	}
	return -1
}

func sortedResults(results map[int]solver.FrameResult) []solver.FrameResult {
	keys := make([]int, 0, len(results))
	for k := range results {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	out := make([]solver.FrameResult, 0, len(keys))
	for _, k := range keys {
		out = append(out, results[k])
	}
	return out
}
