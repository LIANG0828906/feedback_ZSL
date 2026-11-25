package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"credrot/internal/anomaly"
	"credrot/internal/bizerr"
	"credrot/internal/checkpoint"
	"credrot/internal/engine"
	"credrot/internal/generator"
	"credrot/internal/hashutil"
	"credrot/internal/model"
	"credrot/internal/project"
)

type Check struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Details string `json:"details,omitempty"`
}

type Report struct {
	Seed         int64   `json:"seed"`
	Manifest     string  `json:"manifest_version"`
	Checks       []Check `json:"checks"`
	ParallelRuns int     `json:"parallel_runs"`
	Backpressure int     `json:"backpressure_events"`
	ReusedCells  int     `json:"reused_validity_cells"`
	CacheReuse   bool    `json:"cache_reuse_observed"`
	CheckpointOK bool    `json:"checkpoint_recovery_ok"`
	AnomaliesOK  bool    `json:"anomaly_codes_ok"`
}

func Run(seed int64, workDir string) (*Report, error) {
	if workDir == "" {
		var err error
		workDir, err = os.MkdirTemp("", "credrot-verify-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(workDir)
	} else if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, err
	}
	report := &Report{Seed: seed}

	bundle, err := generator.Generate(seed)
	if err != nil {
		return nil, err
	}
	report.Manifest = bundle.Manifest
	second, err := generator.Generate(seed)
	if err != nil {
		return nil, err
	}
	report.add("deterministic_generator", equalGenerated(bundle, second), "same seed produced byte-identical samples")

	loaded := make(map[string]*model.Project)
	for _, name := range []string{"project_v1.json", "project_v2.json", "project_v3.json"} {
		loadedValue, err := project.Load(bundle.Files[name])
		if err != nil {
			return nil, err
		}
		loaded[name] = loadedValue.Project
	}
	converged := reflect.DeepEqual(loaded["project_v1.json"], loaded["project_v2.json"]) &&
		reflect.DeepEqual(loaded["project_v2.json"], loaded["project_v3.json"]) &&
		reflect.DeepEqual(loaded["project_v3.json"], bundle.Project)
	report.add("three_schema_convergence", converged, "V1/V2/V3 canonical projects are deeply equal")
	if !converged {
		return report, fmt.Errorf("schema convergence failed")
	}
	p := loaded["project_v3.json"]

	solver, err := engine.NewSolver(p)
	if err != nil {
		return nil, err
	}
	validity, err := solver.ValidityAll()
	if err != nil {
		return nil, err
	}
	report.add("validity_oracle", reflect.DeepEqual(validity, bundle.Oracle.Validity), "every tick/subscription cell matches oracle")
	if !reflect.DeepEqual(validity, bundle.Oracle.Validity) {
		return report, firstDiff("validity", validity, bundle.Oracle.Validity)
	}
	propagation, err := engine.Propagate(p)
	if err != nil {
		return nil, err
	}
	report.add("propagation_oracle", reflect.DeepEqual(propagation, bundle.Oracle.Propagation), "arrival batches match oracle")
	if !reflect.DeepEqual(propagation, bundle.Oracle.Propagation) {
		return report, firstDiff("propagation", propagation, bundle.Oracle.Propagation)
	}
	schedule, err := engine.Schedule(p, propagation)
	if err != nil {
		return nil, err
	}
	report.add("schedule_oracle", reflect.DeepEqual(schedule, bundle.Oracle.Schedule), "retry/cap schedule matches oracle")
	if !reflect.DeepEqual(schedule, bundle.Oracle.Schedule) {
		return report, firstDiff("schedule", schedule, bundle.Oracle.Schedule)
	}
	report.add("schedule_coverage", schedule.CoverageMatch, "actual reload + terminal failures cover the full suspended set")
	gates, err := engine.GateAll(p, bundle.Oracle.Gates.AtTick, bundle.Oracle.GateRuntime)
	if err != nil {
		return nil, err
	}
	report.add("gating_oracle", reflect.DeepEqual(gates, bundle.Oracle.Gates), "allow/fallback/reject decisions match oracle")
	if !reflect.DeepEqual(gates, bundle.Oracle.Gates) {
		return report, firstDiff("gates", gates, bundle.Oracle.Gates)
	}

	var baselineDigest string
	var baselineBackpressure int
	for _, workers := range []int{1, 2, 4, 8} {
		for _, buffer := range []int{1, 2, 10} {
			parallel, err := engine.SolveParallel(context.Background(), p, engine.ParallelOptions{
				Workers:                      workers,
				PendingBufferPerSubscription: buffer,
			})
			if err != nil {
				return nil, err
			}
			report.ParallelRuns++
			baselineBackpressure += len(parallel.Backpressure)
			if baselineDigest == "" {
				baselineDigest = parallel.Digest
			} else if parallel.Digest != baselineDigest {
				report.add("parallel_invariance", false, "parallel digest changed across workers/buffers")
				return report, fmt.Errorf("parallel result changed for workers=%d buffer=%d", workers, buffer)
			}
			if !reflect.DeepEqual(parallel.Validity, validity) {
				report.add("parallel_invariance", false, "parallel validity differs from sequential")
				return report, firstDiff("parallel validity", parallel.Validity, validity)
			}
			if !reflect.DeepEqual(parallel.Propagation, propagation) {
				report.add("parallel_invariance", false, "parallel propagation differs from sequential")
				return report, firstDiff("parallel propagation", parallel.Propagation, propagation)
			}
		}
	}
	report.Backpressure = baselineBackpressure
	report.add("parallel_invariance", true, "workers 1/2/4/8 and buffers 1/2/10 produce identical semantic digests")
	report.add("parallel_backpressure", baselineBackpressure > 0, "bounded buffers and worker slots emit deterministic backpressure events")

	oldSnapshot, err := engine.BuildSnapshot(p)
	if err != nil {
		return nil, err
	}
	sourceBefore, err := hashutil.JSONSHA256(oldSnapshot.Project)
	if err != nil {
		return nil, err
	}
	edited := p.Clone()
	edited.Credentials[0].TTLMillis += 2000
	edited.Sort()
	incremental, err := engine.Incremental(oldSnapshot, edited)
	if err != nil {
		return nil, err
	}
	editedSolver, err := engine.NewSolver(edited)
	if err != nil {
		return nil, err
	}
	fullValidity, err := editedSolver.ValidityAll()
	if err != nil {
		return nil, err
	}
	fullPropagation, err := engine.Propagate(edited)
	if err != nil {
		return nil, err
	}
	fullSchedule, err := engine.Schedule(edited, fullPropagation)
	if err != nil {
		return nil, err
	}
	report.ReusedCells = incremental.ReusedValidity
	report.CacheReuse = incremental.ReusedValidity > 0
	incrementalMatches := reflect.DeepEqual(incremental.Snapshot.Validity, fullValidity) &&
		reflect.DeepEqual(incremental.Snapshot.Propagation, fullPropagation) &&
		reflect.DeepEqual(incremental.Snapshot.Schedule, fullSchedule)
	report.add("incremental_equivalence", incrementalMatches, "incremental cache result equals full recomputation")
	if !incrementalMatches {
		return report, firstDiff("incremental validity", incremental.Snapshot.Validity, fullValidity)
	}
	report.add("incremental_reuse", incremental.ReusedValidity > 0 && len(incremental.Invalidated) > 0,
		fmt.Sprintf("reused=%d invalidated=%d", incremental.ReusedValidity, len(incremental.Invalidated)))

	reloadEdited := edited.Clone()
	reloadEdited.Reload.MaxPerBatch = 2
	reloadEdited.Sort()
	reloadIncremental, err := engine.Incremental(incremental.Snapshot, reloadEdited)
	if err != nil {
		return nil, err
	}
	reloadSolver, err := engine.NewSolver(reloadEdited)
	if err != nil {
		return nil, err
	}
	reloadValidity, err := reloadSolver.ValidityAll()
	if err != nil {
		return nil, err
	}
	reloadPropagation, err := engine.Propagate(reloadEdited)
	if err != nil {
		return nil, err
	}
	reloadSchedule, err := engine.Schedule(reloadEdited, reloadPropagation)
	if err != nil {
		return nil, err
	}
	reloadMatches := reflect.DeepEqual(reloadIncremental.Snapshot.Validity, reloadValidity) &&
		reflect.DeepEqual(reloadIncremental.Snapshot.Propagation, reloadPropagation) &&
		reflect.DeepEqual(reloadIncremental.Snapshot.Schedule, reloadSchedule)
	report.add("incremental_reload_conditions", reloadMatches && reloadIncremental.ScheduleRecomputed &&
		reloadIncremental.ReusedValidity == len(reloadValidity),
		"reload-cap edit recomputes the schedule while reusing every validity cell")
	sourceAfter, err := hashutil.JSONSHA256(oldSnapshot.Project)
	if err != nil {
		return nil, err
	}
	report.add("source_immutability", sourceBefore == sourceAfter, "incremental edits did not mutate cached source")

	checkpointOK, err := verifyCheckpoint(workDir, bundle.Manifest)
	if err != nil {
		return report, err
	}
	report.CheckpointOK = checkpointOK
	report.add("checkpoint_recovery", checkpointOK, "two-phase commits recover only completed states and rebuild on manifest mismatch")

	anomaliesOK, details, err := verifyAnomalies(p)
	if err != nil {
		return report, err
	}
	report.AnomaliesOK = anomaliesOK
	report.add("anomaly_codes", anomaliesOK, details)
	return report, nil
}

func (r *Report) add(name string, ok bool, details string) {
	r.Checks = append(r.Checks, Check{Name: name, OK: ok, Details: details})
}

func equalGenerated(a, b *generator.Bundle) bool {
	if !reflect.DeepEqual(a.Project, b.Project) || !reflect.DeepEqual(a.Oracle, b.Oracle) {
		return false
	}
	if len(a.Files) != len(b.Files) {
		return false
	}
	for name, data := range a.Files {
		if !reflect.DeepEqual(data, b.Files[name]) {
			return false
		}
	}
	return true
}

func firstDiff(label string, got, want any) error {
	gotJSON, _ := jsonMarshal(got)
	wantJSON, _ := jsonMarshal(want)
	max := 900
	if len(gotJSON) < max {
		max = len(gotJSON)
	}
	return fmt.Errorf("%s mismatch\n got: %.900s\nwant: %.900s", label, gotJSON, wantJSON)
}

func jsonMarshal(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func verifyCheckpoint(workDir, manifest string) (bool, error) {
	dir := filepath.Join(workDir, "checkpoint")
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	path := filepath.Join(dir, "rotation.checkpoint")
	store, state, info, err := checkpoint.OpenOrRebuild(path, manifest, checkpoint.OpenOptions{})
	if err != nil {
		return false, err
	}
	if state.CommittedTick != -1 || !info.Rebuilt && info.CommitsSeen != 0 {
		store.Close()
		return false, fmt.Errorf("fresh checkpoint state is invalid: %s", checkpoint.Describe(state))
	}
	state1 := checkpointState(manifest, 1)
	if err := store.Commit(state1); err != nil {
		store.Close()
		return false, err
	}
	state2 := checkpointState(manifest, 2)
	if err := store.CommitWithCrash(state2, checkpoint.CrashAfterState); err != nil {
		store.Close()
		return false, err
	}
	store.Close()
	store, recovered, info, err := checkpoint.Open(path, manifest, checkpoint.OpenOptions{})
	if err != nil {
		return false, err
	}
	if recovered.CommittedTick != 1 || !info.IgnoredDanglingState {
		store.Close()
		return false, fmt.Errorf("dangling state was accepted: tick=%d info=%+v", recovered.CommittedTick, info)
	}
	if err := store.Commit(state2); err != nil {
		store.Close()
		return false, err
	}
	state3 := checkpointState(manifest, 3)
	if err := store.CommitWithCrash(state3, checkpoint.CrashDuringCommit); err != nil {
		store.Close()
		return false, err
	}
	store.Close()
	store, recovered, info, err = checkpoint.Open(path, manifest, checkpoint.OpenOptions{})
	if err != nil {
		return false, err
	}
	if recovered.CommittedTick != 2 || !info.TruncatedTail || !info.Repaired {
		store.Close()
		return false, fmt.Errorf("partial commit was accepted: tick=%d info=%+v", recovered.CommittedTick, info)
	}
	state4 := checkpointState(manifest, 4)
	if err := store.Commit(state4); err != nil {
		store.Close()
		return false, err
	}
	store.Close()
	store, recovered, _, err = checkpoint.Open(path, manifest, checkpoint.OpenOptions{})
	if err != nil {
		return false, err
	}
	if recovered.CommittedTick != 4 {
		store.Close()
		return false, fmt.Errorf("completed commit not recovered: tick=%d", recovered.CommittedTick)
	}
	store.Close()

	truncatedPath := filepath.Join(dir, "truncated.checkpoint")
	truncated, _, _, err := checkpoint.Open(truncatedPath, manifest, checkpoint.OpenOptions{})
	if err != nil {
		return false, err
	}
	if err := truncated.Commit(checkpointState(manifest, 1)); err != nil {
		truncated.Close()
		return false, err
	}
	truncated.Close()
	file, err := os.OpenFile(truncatedPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := file.WriteString(`{"seq":4,"kind":"commit","payload":{`); err != nil {
		file.Close()
		return false, err
	}
	file.Close()
	if _, err := checkpoint.Verify(truncatedPath, manifest); !bizerr.IsCode(err, bizerr.CodeCheckpointTruncated) {
		return false, fmt.Errorf("strict truncation check returned %v", err)
	}

	otherPath := filepath.Join(dir, "manifest-mismatch.checkpoint")
	other, _, _, err := checkpoint.Open(otherPath, manifest, checkpoint.OpenOptions{})
	if err != nil {
		return false, err
	}
	if err := other.Commit(checkpointState(manifest, 1)); err != nil {
		other.Close()
		return false, err
	}
	other.Close()
	if _, _, _, err := checkpoint.Open(otherPath, "other-manifest", checkpoint.OpenOptions{}); !bizerr.IsCode(err, bizerr.CodeManifestMismatch) {
		return false, fmt.Errorf("manifest mismatch check returned %v", err)
	}
	other, rebuiltState, rebuiltInfo, err := checkpoint.OpenOrRebuild(otherPath, "other-manifest", checkpoint.OpenOptions{})
	if err != nil {
		return false, err
	}
	defer other.Close()
	if !rebuiltInfo.Rebuilt || rebuiltState.ManifestVersion != "other-manifest" || rebuiltState.CommittedTick != -1 {
		return false, fmt.Errorf("manifest rebuild did not start from an empty state: %+v %+v", rebuiltInfo, rebuiltState)
	}
	return true, nil
}

func checkpointState(manifest string, tick int64) checkpoint.State {
	state := *checkpoint.EmptyState(manifest)
	state.CommittedTick = tick
	state.Subscriptions = []checkpoint.CommittedSubscription{{
		SubscriptionID: "sub-api",
		CredentialIDs:  []string{"cred-sub-api-v2"},
		Versions:       []uint64{2},
		Source:         fmt.Sprintf("commit:%d", tick),
	}}
	state.Pending = []checkpoint.PendingCredential{{
		CredentialID:    "cred-sub-api-v2",
		SourceVersion:   1,
		TargetVersion:   2,
		ReloadBatch:     1,
		SubscriptionIDs: []string{"sub-api", "sub-billing"},
	}}
	state.Batches = []checkpoint.BatchProgress{{
		Batch:         1,
		Planned:       []string{"sub-api", "sub-billing"},
		Actual:        []string{"sub-api"},
		Failed:        []string{},
		Attempts:      1,
		Confirmed:     true,
		CompletedTick: tick,
	}}
	return state
}

func verifyAnomalies(p *model.Project) (bool, string, error) {
	before, err := hashutil.JSONSHA256(p)
	if err != nil {
		return false, "", err
	}
	cases := []struct {
		kind anomaly.Kind
		code string
	}{
		{anomaly.DuplicateCredentialRef, bizerr.CodeDuplicateRef},
		{anomaly.NegativeTTL, bizerr.CodeNegativeTTL},
		{anomaly.BatchLimitZero, bizerr.CodeBatchLimit},
		{anomaly.SubscriptionCycle, bizerr.CodeSubCycle},
		{anomaly.MissingCredential, bizerr.CodeMissingCredential},
		{anomaly.DuplicateCredential, bizerr.CodeDuplicateCredential},
	}
	seen := make([]string, 0, len(cases))
	for _, tc := range cases {
		mutated, err := anomaly.Inject(p, tc.kind)
		if err != nil {
			return false, "", err
		}
		err = project.Validate(mutated)
		if !bizerr.IsCode(err, tc.code) {
			return false, "", fmt.Errorf("anomaly %s returned %v, want %s", tc.kind, err, tc.code)
		}
		seen = append(seen, string(tc.kind)+"="+tc.code)
	}
	after, err := hashutil.JSONSHA256(p)
	if err != nil {
		return false, "", err
	}
	if before != after {
		return false, "", fmt.Errorf("anomaly injection mutated source project")
	}
	combined := engine.EvaluateGate(engine.GateConditions{
		SubscriptionID:     "combined",
		AtTick:             6,
		Requested:          &model.Credential{ID: "requested-v1", Version: 1},
		Committed:          &model.Credential{ID: "committed-v2", Version: 2},
		RequestedBatch:     9,
		LastCommittedBatch: 2,
		CheckBatchGap:      true,
		WatermarkKnown:     false,
		RequireWatermark:   true,
	})
	if combined.Code != bizerr.CodeVersionRegression {
		return false, "", fmt.Errorf("combined anomaly precedence returned %s", combined.Code)
	}
	seen = append(seen, "combined=E_VERSION_REGRESSION")
	return true, strings.Join(seen, ", "), nil
}
