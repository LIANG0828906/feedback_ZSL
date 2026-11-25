package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	ct "colortrace"
	"colortrace/review"
	"colortrace/sample"
	"colortrace/sqlitecheckpoint"
)

type report struct {
	OK                  bool                    `json:"ok"`
	Seed                int64                   `json:"seed"`
	TruthFingerprint    string                  `json:"truth_fingerprint"`
	Band                *ct.BandReport          `json:"band"`
	TruthMismatch       *ct.Mismatch            `json:"truth_mismatch,omitempty"`
	BoundChecks         int                     `json:"bound_checks"`
	RealtimeCommits     int                     `json:"realtime_commits"`
	RecoveryCommits     int                     `json:"recovery_commits"`
	RecoverySkipped     int                     `json:"recovery_skipped"`
	FullCommits         int                     `json:"full_commits"`
	PathMismatches      map[string]*ct.Mismatch `json:"path_mismatches"`
	ReviewOK            bool                    `json:"review_ok"`
	ReviewViolation     any                     `json:"review_violation,omitempty"`
	BudgetQueueWaits    int                     `json:"budget_queue_waits"`
	BudgetWaitSamples   []time.Duration         `json:"budget_wait_samples,omitempty"`
	BufferFlushes       int                     `json:"buffer_flushes"`
	MaxObservedParallel int                     `json:"max_observed_parallel"`
	InvalidationEvents  int                     `json:"invalidation_events"`
	ReusableNodes       int                     `json:"reusable_nodes"`
	ErrorCodes          map[string]bool         `json:"error_codes"`
	Artifacts           map[string]string       `json:"artifacts"`
}

func main() {
	outDir := flag.String("out", "artifacts/diagnostic", "diagnostic output directory")
	flag.Parse()
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal(err)
	}

	project, err := sample.Generate()
	if err != nil {
		fatal(err)
	}
	meta := ct.NewSnapshotMeta(project, project.Truth.Fingerprint)
	rep := &report{
		Seed: project.Seed, TruthFingerprint: project.Truth.Fingerprint,
		PathMismatches: map[string]*ct.Mismatch{}, ErrorCodes: map[string]bool{},
		Artifacts: map[string]string{},
	}

	writeJSON(rep, filepath.Join(*outDir, "project.json"), project)
	writeJSON(rep, filepath.Join(*outDir, "truth.json"), project.Truth)

	sequential, err := ct.ExecuteSequential(project)
	if err != nil {
		fatal(err)
	}
	rep.Band = sequential.Band
	rep.TruthMismatch = ct.VerifyTruthOutputs(project, sequential.Samples)
	bounds, err := ct.VerifyTruthBounds(project, sequential.Samples, project.Truth)
	if err != nil {
		fatal(err)
	}
	rep.BoundChecks = len(bounds)

	realtimePath := filepath.Join(*outDir, "realtime.sqlite")
	_ = os.Remove(realtimePath)
	realtimeStore, err := sqlitecheckpoint.Open(realtimePath, meta)
	if err != nil {
		fatal(err)
	}
	realtime, err := (&ct.Scheduler{
		Store:  realtimeStore,
		Budget: ct.Budget{MaxParallel: 2, PerWorkerSamples: 64, PendingTasks: 2},
	}).Run(context.Background(), project)
	if err != nil {
		fatal(err)
	}
	rep.RealtimeCommits = len(realtime.CommitSequence)
	reviewReport, err := review.RunStore(realtimeStore, project.Truth.Fingerprint)
	if err != nil {
		fatal(err)
	}
	rep.ReviewOK = reviewReport.OK
	if reviewReport.FirstViolation != nil {
		rep.ReviewViolation = reviewReport.FirstViolation
	}
	writeJSON(rep, filepath.Join(*outDir, "review.json"), reviewReport)
	writeJSON(rep, filepath.Join(*outDir, "commit-sequence.json"), realtime.CommitSequence)
	if err := realtimeStore.Close(); err != nil {
		fatal(err)
	}

	recoveryPath := filepath.Join(*outDir, "recovery.sqlite")
	_ = os.Remove(recoveryPath)
	recoveryStore, err := sqlitecheckpoint.Open(recoveryPath, meta)
	if err != nil {
		fatal(err)
	}
	cancelScheduler := &ct.Scheduler{
		Store: recoveryStore, CancelAfterCommits: 3,
		Budget: ct.Budget{MaxParallel: 1, PerWorkerSamples: 32, PendingTasks: 1},
	}
	if _, err := cancelScheduler.Run(context.Background(), project); !errors.Is(err, context.Canceled) {
		fatal(fmt.Errorf("expected cancel after checkpoint, got %w", err))
	}
	if err := recoveryStore.Close(); err != nil {
		fatal(err)
	}
	reopenedRecovery, err := sqlitecheckpoint.Open(recoveryPath, meta)
	if err != nil {
		fatal(err)
	}
	recovery, err := (&ct.Scheduler{
		Store:  reopenedRecovery,
		Budget: ct.Budget{MaxParallel: 2, PerWorkerSamples: 64, PendingTasks: 2},
	}).Run(context.Background(), project)
	if err != nil {
		fatal(err)
	}
	rep.RecoveryCommits = len(recovery.CommitSequence)
	rep.RecoverySkipped = len(recovery.Skipped)
	if err := reopenedRecovery.Close(); err != nil {
		fatal(err)
	}

	fullStore := ct.NewMemoryStore()
	full, err := (&ct.Scheduler{
		Store: fullStore, Force: true,
		Budget: ct.Budget{MaxParallel: 4, PerWorkerSamples: 128, PendingTasks: 8},
	}).Run(context.Background(), project)
	if err != nil {
		fatal(err)
	}
	rep.FullCommits = len(full.CommitSequence)

	if mismatch := ct.CompareResults(project, sequential, realtime.Result, realtime.CommitSequence, full.CommitSequence); mismatch != nil {
		rep.PathMismatches["sequential_vs_realtime_and_commit_log"] = mismatch
	}
	if mismatch := ct.CompareResults(project, recovery.Result, full.Result, recovery.CommitSequence, full.CommitSequence); mismatch != nil {
		rep.PathMismatches["recovery_vs_full"] = mismatch
	}

	runBudgetDemo(project, rep)
	runGraphDemo(project, rep)
	runErrorCodeChecks(project, rep)

	rep.OK = rep.TruthMismatch == nil && rep.ReviewOK && len(rep.PathMismatches) == 0 &&
		rep.RealtimeCommits == 8 && rep.RecoveryCommits == 8 && rep.FullCommits == 8 &&
		rep.RecoverySkipped == 3
	rep.Artifacts["report"] = filepath.Join(*outDir, "report.json")
	rep.Artifacts["project"] = filepath.Join(*outDir, "project.json")
	rep.Artifacts["truth"] = filepath.Join(*outDir, "truth.json")
	rep.Artifacts["review"] = filepath.Join(*outDir, "review.json")
	rep.Artifacts["commit_sequence"] = filepath.Join(*outDir, "commit-sequence.json")
	rep.Artifacts["realtime_sqlite"] = realtimePath
	rep.Artifacts["recovery_sqlite"] = recoveryPath
	writeJSON(rep, filepath.Join(*outDir, "report.json"), rep)

	fmt.Printf("colortrace diagnostic: ok=%v seed=%d truth=%s\n", rep.OK, rep.Seed, short(rep.TruthFingerprint))
	if rep.Band != nil {
		fmt.Printf("first band: stage=%d plane=%s coord=(%d,%d) chunk=%s jump=%d/%d operator=%s\n",
			rep.Band.Stage, rep.Band.Plane, rep.Band.Coord.X, rep.Band.Coord.Y,
			rep.Band.ChunkID, rep.Band.Jump, rep.Band.Tolerance, rep.Band.DominantOperator)
	}
	fmt.Printf("truth mismatch=%v bounds=%d review_ok=%v commits realtime/recovery/full=%d/%d/%d recovery_skipped=%d\n",
		rep.TruthMismatch, rep.BoundChecks, rep.ReviewOK, rep.RealtimeCommits, rep.RecoveryCommits, rep.FullCommits, rep.RecoverySkipped)
	fmt.Printf("budget waits=%d flushes=%d max_parallel=%d; dag invalid=%d reusable=%d\n",
		rep.BudgetQueueWaits, rep.BufferFlushes, rep.MaxObservedParallel, rep.InvalidationEvents, rep.ReusableNodes)
	for code, ok := range rep.ErrorCodes {
		fmt.Printf("error code %s covered=%v\n", code, ok)
	}
	if !rep.OK {
		os.Exit(1)
	}
}

func runBudgetDemo(project *ct.Project, rep *report) {
	store := ct.NewMemoryStore()
	scheduler := &ct.Scheduler{
		Store: store, Budget: ct.Budget{MaxParallel: 1, PerWorkerSamples: 1, PendingTasks: 1},
		Gate: func(context.Context, int, string) <-chan struct{} {
			release := make(chan struct{})
			go func() {
				time.Sleep(40 * time.Millisecond)
				close(release)
			}()
			return release
		},
	}
	result, err := scheduler.Run(context.Background(), project)
	if err != nil {
		fatal(err)
	}
	rep.BudgetQueueWaits = len(result.QueueWaits)
	rep.BufferFlushes = result.BufferFlushes
	rep.MaxObservedParallel = result.MaxObservedParallel
	for _, wait := range result.QueueWaits {
		rep.BudgetWaitSamples = append(rep.BudgetWaitSamples, wait.Waited)
	}
}

func runGraphDemo(project *ct.Project, rep *report) {
	graph, err := ct.BuildDependencyGraph(project)
	if err != nil {
		fatal(err)
	}
	edited := project.Stages[1]
	kernel := *edited.ChromaKernel
	kernel.NormScale = 192
	edited.ChromaKernel = &kernel
	plan, err := graph.PlanParameterEdit(project, edited)
	if err != nil {
		fatal(err)
	}
	rep.InvalidationEvents = len(plan.Invalidations)
	rep.ReusableNodes = len(plan.Reusable)
}

func runErrorCodeChecks(project *ct.Project, rep *report) {
	data, err := ct.EncodePlane(project.Planes[0])
	if err != nil {
		fatal(err)
	}
	_, decodeErr := ct.DecodePlane(data[:len(data)-5])
	rep.ErrorCodes[string(ct.EDecode)] = hasCode(decodeErr, ct.EDecode)

	orderErr := ct.StageTiming{Stage: 1, StartedAt: time.Now(), FinishedAt: time.Now().Add(-time.Millisecond)}.Validate()
	rep.ErrorCodes[string(ct.EOrder)] = hasCode(orderErr, ct.EOrder)

	_, ambigErr := ct.ClassifyAttribution(1, "Cb", "c00x00", ct.Coord{X: 1, Y: 1}, 1, 0, ct.OperatorBreakdown{
		ResampleNorm: ct.I64(1), AccumTrunc: ct.I64(1), BitRound: ct.I64(1), Chroma: ct.I64(1),
	}, 0.6)
	rep.ErrorCodes[string(ct.EAmbig)] = hasCode(ambigErr, ct.EAmbig)

	_, schemaErr := ct.LoadProject([]byte("{invalid"))
	rep.ErrorCodes[string(ct.ESchema)] = hasCode(schemaErr, ct.ESchema)

	machine := ct.NewChunkStateMachine()
	_, stateErr := machine.Transition(0, "c00x00", ct.StateCommitted)
	rep.ErrorCodes[string(ct.EState)] = hasCode(stateErr, ct.EState)

	_, paraErr := ct.ProcessChunk(func() *ct.Project {
		clone := *project
		stages := append([]ct.StageConfig{}, project.Stages...)
		stages[1].Bits = 2
		clone.Stages = stages
		return &clone
	}(), 1, nil, nil, ct.ChunkCoord{})
	rep.ErrorCodes[string(ct.EPara)] = hasCode(paraErr, ct.EPara)
}

func hasCode(err error, code ct.Code) bool {
	var ce *ct.Error
	return errors.As(err, &ce) && ce.Code == code
}

func writeJSON(rep *report, path string, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fatal(err)
	}
}

func short(fingerprint string) string {
	if len(fingerprint) <= 12 {
		return fingerprint
	}
	return fingerprint[:12]
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "diagnostic failed:", err)
	os.Exit(1)
}
