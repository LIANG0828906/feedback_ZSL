package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"screenrelay/internal/checkpoint"
	"screenrelay/internal/engine"
	"screenrelay/internal/mapping"
	"screenrelay/internal/model"
	"screenrelay/internal/sample"
	"screenrelay/internal/schema"
	"screenrelay/internal/statemachine"
	"screenrelay/internal/topology"
	"screenrelay/internal/xconv"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail"`
}

type PathSummary struct {
	Name            string   `json:"name"`
	Windows         int      `json:"windows"`
	CommitSequence  []int    `json:"commit_sequence"`
	FinalScreenIDs  []string `json:"final_screen_ids"`
	FirstMismatch   string   `json:"first_mismatch,omitempty"`
	RecoveryInvalid int64    `json:"recovery_invalid_seq,omitempty"`
	RecoveryReason  string   `json:"recovery_reason,omitempty"`
}

type Report struct {
	OK          bool                 `json:"ok"`
	FixedSeed   int64                `json:"fixed_seed"`
	Checks      []Check              `json:"checks"`
	Paths       []PathSummary        `json:"paths"`
	Truth       *model.TruthManifest `json:"truth"`
	LossyCounts map[string]int       `json:"lossy_counts"`
	ArtifactDir string               `json:"artifact_dir,omitempty"`
}

func RunAll(ctx context.Context, sampleDir string, checkpointDir string) (*Report, error) {
	s, err := sample.Generate(sample.FixedSeed)
	if err != nil {
		return nil, err
	}
	report := &Report{FixedSeed: sample.FixedSeed, Truth: s.Truth, LossyCounts: map[string]int{}}
	add := func(name string, err error, detail string) {
		check := Check{Name: name, OK: err == nil, Detail: detail}
		if err != nil {
			if coded, ok := model.AsError(err); ok {
				check.Code = string(coded.Code)
			}
			if detail == "" {
				check.Detail = err.Error()
			}
		}
		report.Checks = append(report.Checks, check)
	}

	if sampleDir != "" {
		if err := writeSamples(sampleDir, s); err != nil {
			return nil, err
		}
		report.ArtifactDir = sampleDir
	}
	if checkpointDir == "" {
		checkpointDir = sampleDir
	}
	if err := os.MkdirAll(checkpointDir, 0o755); err != nil {
		return nil, err
	}

	loaded := map[int]*model.Project{}
	for _, major := range []int{1, 2, 3} {
		key := fmt.Sprintf("v%d", major)
		project, convertReport, err := schema.Load(s.Raw[key])
		if err != nil {
			add("load_"+key, err, "")
			continue
		}
		loaded[major] = project
		detail := fmt.Sprintf("source=%s screens=%d events=%d windows=%d fills=%d", project.SourceVersion, len(project.Screens), len(project.Events), len(project.Windows), len(convertReport.Fills))
		add("load_"+key, nil, detail)
	}

	fixture, err := sample.V1MissingDefaultsFixture(s)
	if err != nil {
		return nil, err
	}
	_, fillReport, err := schema.Load(fixture)
	if err == nil && len(fillReport.Fills) < 2 {
		err = fmt.Errorf("expected at least 2 default fills, got %d", len(fillReport.Fills))
	}
	add("v1_default_fills", err, fmt.Sprintf("fills=%d", len(fillReport.Fills)))

	for major, convertReport := range s.Reports {
		report.LossyCounts[major] = len(convertReport.Lossy)
	}
	add("v1_lossy_manifest", expect(len(s.Reports["v1"].Lossy) > 0), fmt.Sprintf("v1_lossy_items=%d", len(s.Reports["v1"].Lossy)))

	orderErr := checkMappingOrderIndependence(s.Project)
	add("mapping_order_independence", orderErr, "reverse and shuffled screen orders produce identical canonical JSON")

	full, err := engine.Run(ctx, s.Project, engine.Options{Budget: engine.DefaultBudget()})
	add("full_replay_run", err, summaryDetail(full))
	if err != nil {
		return report, err
	}

	realtimePath := filepath.Join(checkpointDir, "realtime.checkpoint")
	realtimeStore, err := checkpoint.Open(realtimePath)
	if err != nil {
		return nil, err
	}
	realtime, err := engine.Run(ctx, s.Project, engine.Options{Budget: engine.DefaultBudget(), Store: realtimeStore})
	_ = realtimeStore.Close()
	add("realtime_run", err, summaryDetail(realtime))
	if err != nil {
		return report, err
	}

	recoveryDB := filepath.Join(checkpointDir, "recovery.checkpoint")
	_ = os.Remove(recoveryDB)
	partialStore, err := checkpoint.Open(recoveryDB)
	if err != nil {
		return nil, err
	}
	partial, err := engine.Run(ctx, s.Project, engine.Options{Budget: engine.DefaultBudget(), Store: partialStore, StopAfter: 2})
	_ = partialStore.Close()
	add("recovery_interrupt_after_2", err, summaryDetail(partial))
	if err != nil {
		return report, err
	}
	resumeStore, err := checkpoint.Open(recoveryDB)
	if err != nil {
		return nil, err
	}
	recovered, recovery, err := engine.Resume(ctx, s.Project, resumeStore, engine.Options{Budget: engine.DefaultBudget(), Store: resumeStore})
	_ = resumeStore.Close()
	recoveryErr := err
	if recoveryErr == nil && recovery.InvalidSeq != 0 {
		recoveryErr = fmt.Errorf("unexpected invalid checkpoint seq=%d: %s", recovery.InvalidSeq, recovery.InvalidReason)
	}
	add("recovery_resume", recoveryErr, fmt.Sprintf("invalid_seq=%d reason=%s", recovery.InvalidSeq, recovery.InvalidReason))
	if recoveryErr != nil {
		return report, recoveryErr
	}

	add("compare_realtime_full", expect(engine.CompareResults(full, realtime) == ""), engine.CompareResults(full, realtime))
	add("compare_recovery_full", expect(engine.CompareResults(full, recovered) == ""), engine.CompareResults(full, recovered))

	report.Paths = []PathSummary{
		{Name: "full_replay", Windows: len(full.Windows), CommitSequence: full.CommitSequence, FinalScreenIDs: full.FinalScreenIDs},
		{Name: "realtime_checkpoint", Windows: len(realtime.Windows), CommitSequence: realtime.CommitSequence, FinalScreenIDs: realtime.FinalScreenIDs, FirstMismatch: engine.CompareResults(full, realtime)},
		{Name: "breakpoint_recovery", Windows: len(recovered.Windows), CommitSequence: recovered.CommitSequence, FinalScreenIDs: recovered.FinalScreenIDs, FirstMismatch: engine.CompareResults(full, recovered)},
	}

	add("join_replay", checkJoinReplay(full), "scr-e window-3 replays committed window-2")
	add("exit_no_packets", checkExit(full), "scr-c is absent from window-6 active and screen results")
	add("topology_decisions", checkDecisions(full), "leave and higher revision win deterministically")
	add("truth_manifest_windows", checkTruthWindows(s.Truth, full), "generator truth active/replay sets match committed output")
	add("error_bounds", checkErrorBounds(full), "all observed residuals are within integer bounds")
	add("budget_waves", checkBudget(full), "max parallelism and queued wave records are deterministic")

	runExceptionChecks(report, checkpointDir)
	report.OK = allOK(report.Checks)
	return report, nil
}

func expect(ok bool) error {
	if ok {
		return nil
	}
	return errors.New("assertion failed")
}

func allOK(checks []Check) bool {
	for _, check := range checks {
		if !check.OK {
			return false
		}
	}
	return true
}

func summaryDetail(r *engine.RunResult) string {
	if r == nil {
		return "nil result"
	}
	return fmt.Sprintf("windows=%d commits=%v finals=%v", len(r.Windows), r.CommitSequence, r.FinalScreenIDs)
}

func writeSamples(dir string, s *model.SampleSet) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for key, data := range s.Raw {
		if err := os.WriteFile(filepath.Join(dir, "session."+key+".json"), data, 0o644); err != nil {
			return err
		}
	}
	truth, err := json.MarshalIndent(s.Truth, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "truth.json"), truth, 0o644)
}

func checkMappingOrderIndependence(p *model.Project) error {
	window := p.Windows[0]
	normal := p.Screens
	reversed := append([]model.Screen(nil), normal...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	run := func(screens []model.Screen) (string, error) {
		results := []*mapping.Result{}
		for _, screen := range screens {
			result, err := mapping.MapScreen(mapping.Request{Region: window.Region, Screen: screen, TimeQuantum: 1}, window.Tick)
			if err != nil {
				return "", err
			}
			results = append(results, result)
		}
		sort.Slice(results, func(i, j int) bool { return results[i].ScreenID < results[j].ScreenID })
		canonical, err := json.Marshal(results)
		if err != nil {
			return "", err
		}
		return string(canonical), nil
	}
	a, err := run(normal)
	if err != nil {
		return err
	}
	b, err := run(reversed)
	if err != nil {
		return err
	}
	if a != b {
		return errors.New("mapping output changed with screen order")
	}
	return nil
}

func checkJoinReplay(r *engine.RunResult) error {
	if len(r.Windows) < 3 {
		return fmt.Errorf("expected >=3 windows, got %d", len(r.Windows))
	}
	w3 := r.Windows[2]
	for _, sr := range w3.ScreenResults {
		if sr.ScreenID == "scr-e" {
			if sr.Replay == nil || sr.Replay.SourceWindowSeq != 2 || len(sr.Replay.Packets) == 0 {
				return fmt.Errorf("scr-e replay=%+v", sr.Replay)
			}
			return nil
		}
	}
	return errors.New("scr-e missing from window 3")
}

func checkExit(r *engine.RunResult) error {
	last := r.Windows[len(r.Windows)-1]
	for _, id := range last.ActiveScreenIDs {
		if id == "scr-c" {
			return errors.New("scr-c still active after leave")
		}
	}
	for _, sr := range last.ScreenResults {
		if sr.ScreenID == "scr-c" && sr.Dispatch != nil {
			return errors.New("scr-c received packets after leave")
		}
	}
	return nil
}

func checkDecisions(r *engine.RunResult) error {
	foundLeave := false
	foundRevision := false
	for _, d := range r.Decisions {
		if d.ScreenID == "scr-f" && d.WinnerID == "evt-f-leave" {
			foundLeave = true
		}
		if d.ScreenID == "scr-c" && d.Tick == r.Windows[4].Tick && d.WinnerID == "evt-c-topo-new" {
			foundRevision = true
		}
	}
	if !foundLeave || !foundRevision {
		return fmt.Errorf("leave=%v revision=%v", foundLeave, foundRevision)
	}
	return nil
}

func checkTruthWindows(truth *model.TruthManifest, r *engine.RunResult) error {
	if truth == nil {
		return errors.New("missing truth manifest")
	}
	if len(truth.WindowTruths) != len(r.Windows) {
		return fmt.Errorf("truth windows %d != run windows %d", len(truth.WindowTruths), len(r.Windows))
	}
	for i, wt := range truth.WindowTruths {
		w := r.Windows[i]
		if strings.Join(wt.ActiveScreenIDs, ",") != strings.Join(w.ActiveScreenIDs, ",") {
			return fmt.Errorf("window %d active truth=%v actual=%v", wt.Seq, wt.ActiveScreenIDs, w.ActiveScreenIDs)
		}
		replayIDs := []string{}
		for _, sr := range w.ScreenResults {
			if sr.Replay != nil {
				replayIDs = append(replayIDs, sr.ScreenID)
			}
		}
		sort.Strings(replayIDs)
		sort.Strings(wt.ReplayScreenIDs)
		if strings.Join(wt.ReplayScreenIDs, ",") != strings.Join(replayIDs, ",") {
			return fmt.Errorf("window %d replay truth=%v actual=%v", wt.Seq, wt.ReplayScreenIDs, replayIDs)
		}
	}
	return nil
}

func checkErrorBounds(r *engine.RunResult) error {
	for _, w := range r.Windows {
		for _, sr := range w.ScreenResults {
			if sr.Mapping == nil {
				continue
			}
			e := sr.Mapping.Errors
			b := e.Bounds
			if e.CoordRemainderUnits > b.CoordRemainderUnits || e.ScaleRoundingNumerator > b.ScaleRoundingNumerator || e.ClipTruncationPixels > b.ClipTruncationPixels || e.TimeRemainderTicks > b.TimeRemainderTicks {
				return fmt.Errorf("window=%d screen=%s residual=%+v bounds=%+v", w.Seq, sr.ScreenID, e, b)
			}
		}
	}
	return nil
}

func checkBudget(r *engine.RunResult) error {
	queued := 0
	for _, w := range r.Windows {
		for _, b := range w.Budget {
			if b.MaxParallel != 2 {
				return fmt.Errorf("unexpected parallel budget %d", b.MaxParallel)
			}
			if b.Queued {
				queued++
				if b.Reason == "" {
					return errors.New("queued budget record lacks reason")
				}
			}
		}
	}
	if queued == 0 {
		return errors.New("expected deterministic queued records for >2 screens")
	}
	return nil
}

func runExceptionChecks(report *Report, dir string) {
	report.Checks = append(report.Checks, exceptionCheck("schema_unknown_version", model.ESchema, func() error {
		_, _, err := schema.Load([]byte(`{"schema_version":"9.9"}`))
		return err
	}))
	report.Checks = append(report.Checks, exceptionCheck("schema_missing_required", model.ESchema, func() error {
		_, _, err := schema.Load([]byte(`{"schema_version":3,"seed":1}`))
		return err
	}))
	report.Checks = append(report.Checks, exceptionCheck("decode_invalid_json", model.EDecode, func() error {
		_, _, err := schema.Load([]byte(`{"schema_version":3,`))
		return err
	}))
	report.Checks = append(report.Checks, exceptionCheck("overflow_checked_mul", model.EOvf, func() error {
		_, err := xconv.MulChecked(math.MaxInt64, 2, "probe.operand")
		return err
	}))
	report.Checks = append(report.Checks, exceptionCheck("parameter_unsupported_scale", model.EPara, func() error {
		screen := model.Screen{ID: "bad-scale", PixelW: 100, PixelH: 100, Scale: model.Scale100, SupportedScales: []model.ScaleTier{model.Scale100}, Chroma: model.Chroma420, SupportedChromas: []model.Chroma{model.Chroma420}}
		_, err := mapping.MapScreen(mapping.Request{Region: model.Rect{X: 0, Y: 0, W: 10 * model.InternalCoordUnit, H: 10 * model.InternalCoordUnit}, Screen: screen, Requested: model.Scale200}, 0)
		return err
	}))
	report.Checks = append(report.Checks, exceptionCheck("parameter_empty_clip", model.EPara, func() error {
		screen := model.Screen{ID: "offscreen", PixelW: 100, PixelH: 100, Scale: model.Scale100, SupportedScales: []model.ScaleTier{model.Scale100}, Chroma: model.Chroma420, SupportedChromas: []model.Chroma{model.Chroma420}}
		_, err := mapping.MapScreen(mapping.Request{Region: model.Rect{X: 9999 * model.InternalCoordUnit, Y: 9999 * model.InternalCoordUnit, W: 10 * model.InternalCoordUnit, H: 10 * model.InternalCoordUnit}, Screen: screen}, 0)
		return err
	}))
	report.Checks = append(report.Checks, exceptionCheck("conflict_same_revision_payload", model.EConflict, func() error {
		s1 := model.Screen{ID: "x", Revision: 7, PixelW: 1, PixelH: 1}
		s2 := s1
		s2.PixelW = 2
		events := []model.TopologyEvent{
			{ID: "a", Tick: 10, Kind: model.EventTopology, ScreenID: "x", Revision: 7, Screen: &s1},
			{ID: "b", Tick: 10, Kind: model.EventTopology, ScreenID: "x", Revision: 7, Screen: &s2},
		}
		_, _, err := topology.Adjudicate(events)
		return err
	}))
	report.Checks = append(report.Checks, exceptionCheck("state_illegal_transition", model.EState, func() error {
		m := statemachine.NewWindow(1)
		return m.Transition(statemachine.WindowCommitted)
	}))
	report.Checks = append(report.Checks, exceptionCheck("order_time_inversion", model.EOrder, func() error {
		s, err := sample.Generate(sample.FixedSeed)
		if err != nil {
			return err
		}
		p := *s.Project
		windows := append([]model.WindowInstruction(nil), p.Windows...)
		windows[1].Tick = windows[0].Tick - 1
		p.Windows = windows
		return engine.ValidateProject(&p)
	}))
	report.Checks = append(report.Checks, exceptionCheck("capability_conflict", model.ECap, func() error {
		screen := model.Screen{ID: "cap-bad", PixelW: 100, PixelH: 100, Scale: model.Scale100, SupportedScales: []model.ScaleTier{model.Scale100}, Chroma: model.Chroma444, SupportedChromas: []model.Chroma{model.Chroma420}}
		return mapping.ValidateCapabilities(screen)
	}))
	report.Checks = append(report.Checks, exceptionCheck("checkpoint_hash_corruption", model.EDecode, func() error {
		return checkCheckpointCorruption(dir)
	}))
	report.Checks = append(report.Checks, exceptionCheck("checkpoint_algorithm_version_mismatch", model.EDecode, func() error {
		return checkCheckpointVersionMismatch(dir, "algo")
	}))
	report.Checks = append(report.Checks, exceptionCheck("checkpoint_source_version_mismatch", model.EDecode, func() error {
		return checkCheckpointVersionMismatch(dir, "source")
	}))
	report.Checks = append(report.Checks, exceptionCheck("decode_checkpoint_file_corrupt", model.EDecode, func() error {
		path := filepath.Join(dir, "garbage.checkpoint")
		if err := os.WriteFile(path, []byte("{not-json"), 0o644); err != nil {
			return err
		}
		_, err := checkpoint.Open(path)
		return err
	}))
}

func exceptionCheck(name string, want model.Code, fn func() error) Check {
	err := fn()
	ok := err != nil && model.HasCode(err, want)
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	if !ok {
		detail = fmt.Sprintf("want %s; got: %v", want, err)
	}
	return Check{Name: name, OK: ok, Code: string(want), Detail: detail}
}

func checkCheckpointCorruption(dir string) error {
	path := filepath.Join(dir, "corrupt.checkpoint")
	_ = os.Remove(path)
	store, err := checkpoint.Open(path)
	if err != nil {
		return err
	}
	_, err = store.Commit(checkpoint.Commit{
		WindowSeq: 1, WindowID: "w1", Tick: 90, SourceVersion: "3.0",
		AlgorithmVersion: model.AlgorithmVersion, Payload: map[string]any{"hello": "world"},
	})
	_ = store.Close()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return model.NewError(model.EDecode, "test file not valid JSON: %v", err)
	}
	tables := doc["tables"].(map[string]any)
	cp := tables["checkpoint_windows"].(map[string]any)
	rows := cp["rows"].([]any)
	row := rows[0].(map[string]any)
	row["content_hash"] = "0000000000000000000000000000000000000000000000000000000000000000"
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return err
	}
	reopened, err := checkpoint.Open(path)
	if err != nil {
		return err
	}
	defer reopened.Close()
	recovery, err := reopened.LatestValid(model.AlgorithmVersion)
	if err != nil {
		return err
	}
	if recovery.InvalidSeq != 1 || recovery.Record != nil {
		return model.NewError(model.EDecode, "corrupt checkpoint was accepted: %+v", recovery)
	}
	return model.NewError(model.EDecode, "injected hash mismatch rejected at seq %d: %s", recovery.InvalidSeq, recovery.InvalidReason)
}

func checkCheckpointVersionMismatch(dir, kind string) error {
	path := filepath.Join(dir, "version-"+kind+".checkpoint")
	_ = os.WriteFile(path, nil, 0o644)
	store, err := checkpoint.Open(path)
	if err != nil {
		return err
	}
	commit := checkpoint.Commit{
		WindowSeq: 1, WindowID: "w1", Tick: 90, SourceVersion: "3.0",
		AlgorithmVersion: model.AlgorithmVersion, Payload: map[string]any{"hello": "version"},
	}
	wantSource := "3.0"
	if kind == "algo" {
		commit.AlgorithmVersion = "mapping-v0.broken"
	} else {
		commit.SourceVersion = "0.0"
		wantSource = "3.0"
	}
	if _, err := store.Commit(commit); err != nil {
		_ = store.Close()
		return err
	}
	_ = store.Close()
	reopened, err := checkpoint.Open(path)
	if err != nil {
		return err
	}
	defer reopened.Close()
	recovery, err := reopened.LatestValidVersions(model.AlgorithmVersion, wantSource)
	if err != nil {
		return err
	}
	if recovery.InvalidSeq != 1 || recovery.Record != nil {
		return model.NewError(model.EDecode, "%s mismatch checkpoint was accepted: %+v", kind, recovery)
	}
	return model.NewError(model.EDecode, "injected %s version mismatch rejected: %s", kind, recovery.InvalidReason)
}
