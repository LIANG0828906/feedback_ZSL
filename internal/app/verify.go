package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"battlekernel/internal/checkpoint"
	"battlekernel/internal/convert"
	"battlekernel/internal/engine"
	"battlekernel/internal/errs"
	"battlekernel/internal/loader"
	"battlekernel/internal/model"
	"battlekernel/internal/pipeline"
	"battlekernel/internal/review"
	"battlekernel/internal/sample"
	"battlekernel/internal/sched"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Code   string `json:"code,omitempty"`
	Field  string `json:"field,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type ProjectReport struct {
	ProjectID               string                 `json:"project_id"`
	SemanticHash            string                 `json:"semantic_hash"`
	TruthVerified           bool                   `json:"truth_verified"`
	RealtimeCompare         pipeline.CompareReport `json:"realtime_compare"`
	ResumeCompare           pipeline.CompareReport `json:"resume_compare"`
	FailureRetryCompare     pipeline.CompareReport `json:"failure_retry_compare"`
	SnapshotRecoveryCompare pipeline.CompareReport `json:"snapshot_recovery_compare"`
	Review                  review.Report          `json:"review"`
	DBPath                  string                 `json:"db_path"`
	Checks                  []Check                `json:"checks"`
}

type VerificationReport struct {
	OK                  bool                        `json:"ok"`
	StartedAt           string                      `json:"started_at"`
	FinishedAt          string                      `json:"finished_at"`
	Seed                int64                       `json:"seed"`
	SampleCount         int                         `json:"sample_count"`
	WindowSize          int                         `json:"window_size"`
	OutDir              string                      `json:"out_dir"`
	Checks              []Check                     `json:"checks"`
	Projects            []*ProjectReport            `json:"projects"`
	Scheduler           sched.Metrics               `json:"scheduler"`
	BufferScheduler     sched.Metrics               `json:"buffer_scheduler"`
	Dependencies        []model.Dependency          `json:"dependencies"`
	WeightInvalidation  *pipeline.IncrementalReport `json:"weight_invalidation,omitempty"`
	ReorderInvalidation *pipeline.IncrementalReport `json:"reorder_invalidation,omitempty"`
	NoOpInvalidation    *pipeline.IncrementalReport `json:"no_op_invalidation,omitempty"`
}

type Options struct {
	OutDir          string
	Seed            int64
	SampleCount     int
	WindowSize      int
	Parallel        int
	BufferPerWorker int
}

func DefaultOptions() Options {
	return Options{OutDir: "out", Seed: sample.DefaultSeed, SampleCount: 3, WindowSize: 2, Parallel: 2, BufferPerWorker: 1}
}

func RunVerification(ctx context.Context, opts Options) (*VerificationReport, error) {
	started := time.Now().UTC()
	if opts.OutDir == "" {
		opts.OutDir = "out"
	}
	if opts.Seed == 0 {
		opts.Seed = sample.DefaultSeed
	}
	if opts.SampleCount <= 0 {
		opts.SampleCount = 3
	}
	windowSize := pipeline.DefaultWindowSize
	if opts.WindowSize > 0 {
		windowSize = opts.WindowSize
	}
	if opts.Parallel <= 0 {
		opts.Parallel = 2
	}
	if opts.BufferPerWorker <= 0 {
		opts.BufferPerWorker = 1
	}

	report := &VerificationReport{StartedAt: started.Format(time.RFC3339Nano), Seed: opts.Seed, SampleCount: opts.SampleCount, WindowSize: windowSize, OutDir: opts.OutDir, OK: true}
	for _, dir := range []string{"", "samples", "data", "reports"} {
		if err := os.MkdirAll(filepath.Join(opts.OutDir, dir), 0o755); err != nil {
			return nil, err
		}
	}

	suite, err := sample.GenerateSuite(opts.Seed, opts.SampleCount)
	if err != nil {
		return nil, err
	}
	fingerprintData, err := json.MarshalIndent(suite.FingerprintFile(), "", "  ")
	if err != nil {
		return nil, err
	}
	fingerprintPath := filepath.Join(opts.OutDir, "truth-fingerprints.json")
	if err := os.WriteFile(fingerprintPath, fingerprintData, 0o644); err != nil {
		return nil, err
	}

	for _, s := range suite.Samples {
		base := filepath.Join(opts.OutDir, "samples", s.Project.ID)
		for formatNo, data := range s.Documents {
			if err := os.WriteFile(fmt.Sprintf("%s.format%d.json", base, formatNo), data, 0o644); err != nil {
				return nil, err
			}
		}
		if err := os.WriteFile(base+".truth.json", s.TruthJSON, 0o644); err != nil {
			return nil, err
		}
	}

	report.Checks = append(report.Checks, verifyFormats(suite)...)
	report.Checks = append(report.Checks, verifyConversions()...)
	report.Checks = append(report.Checks, verifyExceptions(suite)...)
	report.Dependencies = engine.BuildDependencies(suite.Samples[0].Project)

	pool := sched.NewPool(opts.Parallel, opts.BufferPerWorker)
	futures := make([]sched.Future, len(suite.Samples))
	projectReports := make([]*ProjectReport, len(suite.Samples))
	for i, sampleItem := range suite.Samples {
		i := i
		s := sampleItem
		future, err := pool.Submit(ctx, "parallel-battle-verification", func() error {
			pr, err := verifyProject(ctx, opts, windowSize, fingerprintPath, s)
			if err != nil {
				return err
			}
			projectReports[i] = pr
			return nil
		})
		if err != nil {
			return nil, err
		}
		futures[i] = future
	}
	var projectErr error
	for _, future := range futures {
		if err := future.Wait(); err != nil && projectErr == nil {
			projectErr = err
		}
	}
	pool.Close()
	report.Scheduler = pool.Metrics()
	if projectErr != nil {
		return nil, projectErr
	}
	report.Projects = projectReports

	incReports, incChecks, err := verifyInvalidation(ctx, opts, windowSize, suite)
	if err != nil {
		return nil, err
	}
	report.Checks = append(report.Checks, incChecks...)
	report.WeightInvalidation = incReports.weight
	report.ReorderInvalidation = incReports.reorder
	report.NoOpInvalidation = incReports.noop

	schedCheck, bufferMetrics := verifySchedulerBlocking()
	report.Checks = append(report.Checks, schedCheck)
	report.BufferScheduler = bufferMetrics

	for _, check := range report.Checks {
		if !check.OK {
			report.OK = false
		}
	}
	for _, pr := range report.Projects {
		if pr == nil || !pr.TruthVerified || !pr.RealtimeCompare.Equal || !pr.ResumeCompare.Equal || !pr.FailureRetryCompare.Equal || !pr.SnapshotRecoveryCompare.Equal || !pr.Review.OK {
			report.OK = false
		}
		for _, check := range pr.Checks {
			if !check.OK {
				report.OK = false
			}
		}
	}

	report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	reportPath := filepath.Join(opts.OutDir, "reports", "verification-report.json")
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(reportPath, data, 0o644); err != nil {
		return nil, err
	}
	return report, nil
}

func verifyFormats(suite *sample.Suite) []Check {
	var checks []Check
	for _, s := range suite.Samples {
		set, err := loader.LoadSet(s.Documents[1], s.Documents[2], s.Documents[3], s.TruthJSON)
		if err != nil {
			checks = append(checks, Check{Name: "load-set:" + s.Project.ID, OK: false, Code: string(errs.CodeOf(err)), Detail: err.Error()})
			continue
		}
		hash, _ := loader.SemanticHash(set.V1)
		checks = append(checks, Check{Name: "load-set:" + s.Project.ID, OK: true, Detail: "formats=" + set.V1.SourceFormat + "," + set.V2.SourceFormat + "," + set.V3.SourceFormat + " semantic_hash=" + hash[:12]})
	}
	return checks
}

func verifyConversions() []Check {
	ts, canonical, err := convertRoundTrip(1760000002350, 1, 1760000000000, 1000)
	if err != nil {
		return []Check{{Name: "integer-roundtrip", OK: false, Code: string(errs.CodeOf(err)), Detail: err.Error()}}
	}
	ok := ts == 3 && canonical == 1760000002000
	checks := []Check{{Name: "integer-roundtrip", OK: ok, Detail: fmt.Sprintf("floor ms -> round=%d canonical_ms=%d", ts, canonical)}}
	_, err = convert.RoundIndexToMillis(maxInt64, 1, 1760000000000, 1000, "probe.round")
	code := errs.CodeOf(err)
	checks = append(checks, Check{Name: "overflow-e_ovf", OK: code == errs.EOVF, Code: string(code), Field: "probe.atMilliseconds", Detail: fmt.Sprint(err)})
	return checks
}

const maxInt64 = int64(^uint64(0) >> 1)

func verifyExceptions(suite *sample.Suite) []Check {
	s := suite.Samples[0]
	var checks []Check

	corrupt := append([]byte(nil), s.Documents[1]...)
	corrupt[len(corrupt)-2] = '{'
	_, err := loader.LoadDocument(corrupt)
	checks = append(checks, exceptionCheck("corrupt-engineering-e_decode", errs.EDecode, err))

	_, err = loader.LoadDocument([]byte(`{"format":99}`))
	checks = append(checks, exceptionCheck("unknown-format-e_schema", errs.ESchema, err))

	var raw map[string]any
	_ = json.Unmarshal(s.Documents[1], &raw)
	delete(raw, "battle_id")
	missing, _ := json.Marshal(raw)
	_, err = loader.LoadDocument(missing)
	checks = append(checks, exceptionCheck("missing-required-e_schema", errs.ESchema, err))

	var w2 map[string]any
	_ = json.Unmarshal(s.Documents[2], &w2)
	tps := w2["timePoints"].([]any)
	tp0 := tps[0].(map[string]any)
	cmds := tp0["commands"].([]any)
	cmd := cmds[2].(map[string]any)
	cmd["conditionBit"] = 1
	contradiction, _ := json.Marshal(w2)
	_, err = loader.LoadDocument(contradiction)
	checks = append(checks, exceptionCheck("flag-contradiction-e_flag", errs.EFlag, err))

	var w1 map[string]any
	_ = json.Unmarshal(s.Documents[1], &w1)
	units := w1["units"].([]any)
	unit0 := units[0].(map[string]any)
	unit0["recast"] = 101
	badCool, _ := json.Marshal(w1)
	_, err = loader.LoadDocument(badCool)
	checks = append(checks, exceptionCheck("cooldown-bound-e_cool", errs.ECool, err))

	coolProject := suite.Samples[0].Project
	states := engine.InitialStateMap(coolProject)
	badState := states["u1"]
	badState.CooldownRemaining = loader.MaxCooldownRemaining + 1
	states["u1"] = badState
	_, _, coolErr := engine.SettleRound(coolProject, coolProject.Rounds[0], states)
	coolCheck := exceptionCheck("runtime-cooldown-e_cool-round-isolated", errs.ECool, coolErr)
	if coolCheck.OK {
		if ke, ok := coolErr.(*errs.Error); !ok || ke.Round != 1 || ke.UnitID != "u1" {
			coolCheck.OK = false
			coolCheck.Detail = "E_COOL missing round/unit isolation: " + coolCheck.Detail
		}
	}
	checks = append(checks, coolCheck)

	return checks
}

func exceptionCheck(name string, want errs.Code, err error) Check {
	got := errs.CodeOf(err)
	return Check{Name: name, OK: got == want, Code: string(got), Detail: fmt.Sprint(err)}
}

func verifyProject(ctx context.Context, opts Options, windowSize int, fingerprintPath string, s *sample.Sample) (*ProjectReport, error) {
	p := s.Project
	pr := &ProjectReport{ProjectID: p.ID, DBPath: filepath.Join(opts.OutDir, "data", p.ID+".realtime.db")}
	hash, err := loader.SemanticHash(p)
	if err != nil {
		return nil, err
	}
	pr.SemanticHash = hash

	full, err := pipeline.FullRecompute(p, pipeline.RunOptions{WindowSize: windowSize})
	if err != nil {
		return nil, err
	}
	if err := verifyTruth(p, full.Rounds); err != nil {
		pr.TruthVerified = false
		pr.Checks = append(pr.Checks, Check{Name: "truth", OK: false, Code: string(errs.CodeOf(err)), Detail: err.Error()})
	} else {
		pr.TruthVerified = true
		pr.Checks = append(pr.Checks, Check{Name: "truth", OK: true, Detail: "all order, condition, cooldown, resource and winner fingerprints match"})
	}

	_ = os.Remove(pr.DBPath)
	store, err := checkpoint.Open(pr.DBPath)
	if err != nil {
		return nil, err
	}
	realtime, err := pipeline.RunRealtime(ctx, p, store, pipeline.RunOptions{WindowSize: windowSize})
	if err != nil {
		store.Close()
		return nil, err
	}
	pr.RealtimeCompare = pipeline.CompareReports(p.ID, full, realtime)
	if err := store.Close(); err != nil {
		return nil, err
	}

	breakDB := filepath.Join(opts.OutDir, "data", p.ID+".breakpoint.db")
	_ = removeDBFiles(breakDB)
	breakStore, err := checkpoint.Open(breakDB)
	if err != nil {
		return nil, err
	}
	if _, err := pipeline.RunRealtime(ctx, p, breakStore, pipeline.RunOptions{WindowSize: windowSize, StopAfterWindows: 1}); err != nil {
		breakStore.Close()
		return nil, err
	}
	if err := breakStore.Close(); err != nil {
		return nil, err
	}
	resumeStore, err := checkpoint.Open(breakDB)
	if err != nil {
		return nil, err
	}
	resume, err := pipeline.RunResume(ctx, p, resumeStore, pipeline.RunOptions{WindowSize: windowSize})
	if err != nil {
		resumeStore.Close()
		return nil, err
	}
	pr.ResumeCompare = pipeline.CompareReports(p.ID, full, resume)
	if err := resumeStore.Close(); err != nil {
		return nil, err
	}

	failDB := filepath.Join(opts.OutDir, "data", p.ID+".failure.db")
	_ = removeDBFiles(failDB)
	failStore, err := checkpoint.Open(failDB)
	if err != nil {
		return nil, err
	}
	_, failErr := pipeline.RunRealtime(ctx, p, failStore, pipeline.RunOptions{WindowSize: windowSize, InjectFailureRound: 3, InjectFailureCode: errs.EState, InjectFailureMessage: "injected settlement panic boundary"})
	failOK := errs.IsCode(failErr, errs.EState)
	retry, retryErr := pipeline.RunResume(ctx, p, failStore, pipeline.RunOptions{WindowSize: windowSize})
	if retryErr != nil {
		failStore.Close()
		return nil, retryErr
	}
	pr.FailureRetryCompare = pipeline.CompareReports(p.ID, full, retry)
	pr.Checks = append(pr.Checks, Check{Name: "single-round-failure-isolated", OK: failOK && pr.FailureRetryCompare.Equal, Code: string(errs.CodeOf(failErr)), Detail: fmt.Sprint(failErr)})
	if err := failStore.Close(); err != nil {
		return nil, err
	}

	snapshotDB := filepath.Join(opts.OutDir, "data", p.ID+".snapshot-corrupt.db")
	_ = removeDBFiles(snapshotDB)
	snapshotStore, err := checkpoint.Open(snapshotDB)
	if err != nil {
		return nil, err
	}
	if _, err := pipeline.RunRealtime(ctx, p, snapshotStore, pipeline.RunOptions{WindowSize: windowSize}); err != nil {
		snapshotStore.Close()
		return nil, err
	}
	if err := snapshotStore.Close(); err != nil {
		return nil, err
	}
	if err := corruptSnapshotVersion(snapshotDB, p.ID); err != nil {
		return nil, err
	}
	recoveryStore, err := checkpoint.Open(snapshotDB)
	if err != nil {
		return nil, err
	}
	recovered, err := pipeline.RunResume(ctx, p, recoveryStore, pipeline.RunOptions{WindowSize: windowSize})
	if err != nil {
		recoveryStore.Close()
		return nil, err
	}
	pr.SnapshotRecoveryCompare = pipeline.CompareReports(p.ID, full, recovered)
	pr.Checks = append(pr.Checks, Check{Name: "snapshot-version-mismatch-full-reset", OK: pr.SnapshotRecoveryCompare.Equal, Code: "E_CHECKPOINT", Detail: "bad algorithm_version causes whole project snapshot reset and full recompute"})
	if err := recoveryStore.Close(); err != nil {
		return nil, err
	}

	reviewReport, err := review.ReviewDatabase(ctx, pr.DBPath, fingerprintPath, p.ID)
	if err != nil {
		return nil, err
	}
	pr.Review = *reviewReport
	return pr, nil
}

type incReports struct {
	weight  *pipeline.IncrementalReport
	reorder *pipeline.IncrementalReport
	noop    *pipeline.IncrementalReport
}

func verifyInvalidation(ctx context.Context, opts Options, windowSize int, suite *sample.Suite) (incReports, []Check, error) {
	first := suite.Samples[0].Project
	var checks []Check

	weightProject := cloneProject(first)
	weightProject.WeightEdits = append(weightProject.WeightEdits, model.WeightEdit{UnitID: "u4", Weight: 99, FromRound: 3, Reason: "verification weight edit"})
	weightReport, weightCompare, err := runIncrementalCase(ctx, opts, windowSize, "weight", weightProject, "u4", 3)
	if err != nil {
		return incReports{}, nil, err
	}
	checks = append(checks, Check{Name: "weight-edit-invalidation", OK: weightReport.FirstChangedRound == 3 && len(weightReport.Invalidations) > 0 && containsRound(weightReport.ReusedRounds, 1) && containsRound(weightReport.ReusedRounds, 2) && weightCompare.Equal, Detail: fmt.Sprintf("first=%d invalidations=%d compare=%v", weightReport.FirstChangedRound, len(weightReport.Invalidations), weightCompare.Equal)})

	reorderProject := cloneProject(first)
	reorderProject.Reorders = append(reorderProject.Reorders, model.ReorderInstruction{Offset: 1, ID: "ro-insert-u2-after3", UnitID: "u2", AfterRound: 3, NewTieOrdinal: 0, Reason: "inserted verification reorder"})
	reorderReport, reorderCompare, err := runIncrementalCase(ctx, opts, windowSize, "reorder", reorderProject, "u2", 4)
	if err != nil {
		return incReports{}, nil, err
	}
	checks = append(checks, Check{Name: "insert-reorder-invalidation", OK: !reorderReport.NoOp && reorderReport.FirstChangedRound == 4 && reorderCompare.Equal, Detail: fmt.Sprintf("first=%d invalidations=%d compare=%v", reorderReport.FirstChangedRound, len(reorderReport.Invalidations), reorderCompare.Equal)})

	noopProject := cloneProject(first)
	noopProject.WeightEdits = append(noopProject.WeightEdits, model.WeightEdit{UnitID: "u1", Weight: first.Units[0].BaseWeight, FromRound: 3, Reason: "same value no-op"})
	noopReport, noopCompare, err := runIncrementalCase(ctx, opts, windowSize, "noop", noopProject, "u1", 3)
	if err != nil {
		return incReports{}, nil, err
	}
	checks = append(checks, Check{Name: "ineffective-edit-reuses-all", OK: noopReport.NoOp && len(noopReport.Invalidations) == 0 && noopCompare.Equal, Detail: "no semantic change; persistent snapshots retained"})

	return incReports{weight: weightReport, reorder: reorderReport, noop: noopReport}, checks, nil
}

func runIncrementalCase(ctx context.Context, opts Options, windowSize int, name string, edited *model.Project, target string, start int64) (*pipeline.IncrementalReport, pipeline.CompareReport, error) {
	dbPath := filepath.Join(opts.OutDir, "data", "incremental-"+name+".db")
	_ = removeDBFiles(dbPath)
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, pipeline.CompareReport{}, err
	}
	defer store.Close()
	baseline := cloneProject(edited)
	baseline.WeightEdits = nil
	baseline.Reorders = nil
	// Restore the generator's original reorder for incremental baselines.
	for _, u := range edited.Units {
		_ = u
	}
	baseline.Reorders = originalReorders(edited.ID)
	if _, err := pipeline.RunRealtime(ctx, baseline, store, pipeline.RunOptions{WindowSize: windowSize}); err != nil {
		return nil, pipeline.CompareReport{}, err
	}
	inc, err := pipeline.ApplyIncremental(ctx, edited, store, windowSize, target, start, name+" mutation")
	if err != nil {
		return nil, pipeline.CompareReport{}, err
	}
	full, err := pipeline.FullRecompute(edited, pipeline.RunOptions{WindowSize: windowSize})
	if err != nil {
		return nil, pipeline.CompareReport{}, err
	}
	actual := &pipeline.RunReport{Path: "incremental", ProjectID: edited.ID, Rounds: inc.Results, Windows: pipeline.WindowRecordsFromResults(edited, inc.Results, windowSize)}
	cmp := pipeline.CompareReports(edited.ID, full, actual)
	return inc, cmp, nil
}

func originalReorders(projectID string) []model.ReorderInstruction {
	return []model.ReorderInstruction{{Offset: 0, ID: "ro-u3-after-r2", UnitID: "u3", AfterRound: 2, NewTieOrdinal: 0, Reason: "formation moves Oracle ahead of equal-weight Skirmisher after round 2"}}
}

func verifySchedulerBlocking() (Check, sched.Metrics) {
	pool := sched.NewPool(1, 1)
	release := make(chan struct{})
	started := make(chan struct{})
	f1, err := pool.Submit(context.Background(), "blocking-worker", func() error {
		close(started)
		<-release
		return nil
	})
	if err != nil {
		return Check{Name: "bounded-buffer-blocking", OK: false, Detail: err.Error()}, pool.Metrics()
	}
	<-started
	_, err = pool.Submit(context.Background(), "fill-thread-buffer", func() error { return nil })
	if err != nil {
		return Check{Name: "bounded-buffer-blocking", OK: false, Detail: err.Error()}, pool.Metrics()
	}
	type submitResult struct {
		f   sched.Future
		err error
	}
	resultCh := make(chan submitResult, 1)
	go func() {
		f, err := pool.Submit(context.Background(), "buffer-exhausted-should-wait", func() error { return nil })
		resultCh <- submitResult{f: f, err: err}
	}()
	time.Sleep(30 * time.Millisecond)
	close(release)
	res := <-resultCh
	if res.err != nil {
		return Check{Name: "bounded-buffer-blocking", OK: false, Detail: res.err.Error()}, pool.Metrics()
	}
	if err := f1.Wait(); err != nil {
		return Check{Name: "bounded-buffer-blocking", OK: false, Detail: err.Error()}, pool.Metrics()
	}
	if err := res.f.Wait(); err != nil {
		return Check{Name: "bounded-buffer-blocking", OK: false, Detail: err.Error()}, pool.Metrics()
	}
	metrics := pool.Metrics()
	pool.Close()
	ok := metrics.WaitEvents > 0
	return Check{Name: "bounded-buffer-blocking", OK: ok, Detail: fmt.Sprintf("wait_events=%d max_wait_ms=%d reason=%s", metrics.WaitEvents, metrics.MaxWaitMS, "buffer-exhausted-should-wait")}, metrics
}

func verifyTruth(p *model.Project, results []model.RoundResult) error {
	return engine.VerifyTruth(p, results)
}

func containsRound(rounds []int64, target int64) bool {
	for _, r := range rounds {
		if r == target {
			return true
		}
	}
	return false
}

func cloneProject(p *model.Project) *model.Project {
	data, _ := json.Marshal(p)
	var out model.Project
	_ = json.Unmarshal(data, &out)
	out.Truth = p.Truth
	out.Dependencies = p.Dependencies
	return &out
}

func removeDBFiles(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func corruptSnapshotVersion(dbPath, projectID string) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`UPDATE windows SET algorithm_version='tampered-algorithm-v0' WHERE project_id=?`, projectID)
	return err
}

func convertRoundTrip(ts, start, epoch, tick int64) (int64, int64, error) {
	return convert.CanonicalRoundTrip(ts, start, epoch, tick, convert.RoundFloor, "probe.atMilliseconds")
}
