// Package accept runs the local acceptance kernel and produces a report.
package accept

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"quotakernel/checkpoint"
	"quotakernel/engine"
	"quotakernel/gate"
	"quotakernel/generate"
	"quotakernel/incremental"
	"quotakernel/model"
	"quotakernel/runtime"
	"quotakernel/sampleio"
	"quotakernel/schema"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Data   any    `json:"data,omitempty"`
}

type RecoveryReport struct {
	CheckpointDir           string `json:"checkpointDir"`
	CommittedTick           int64  `json:"committedTick"`
	RecoveredFromTick       int64  `json:"recoveredFromTick"`
	OrphanEntityIgnored     string `json:"orphanEntityIgnored,omitempty"`
	TruthTicksCompared      int    `json:"truthTicksCompared"`
	VersionMismatchDetected bool   `json:"versionMismatchDetected"`
	FullRecomputeAfterReset bool   `json:"fullRecomputeAfterReset"`
}

type Report struct {
	OK                  bool                         `json:"ok"`
	Seed                int64                        `json:"seed"`
	Checks              []Check                      `json:"checks"`
	CriticalConversions []model.ConversionDiagnostic `json:"criticalConversions,omitempty"`
	IncrementalProofs   map[string]incremental.Proof `json:"incrementalProofs,omitempty"`
	RuntimeMetrics      *runtime.MetricsSnapshot     `json:"runtimeMetrics,omitempty"`
	Recovery            *RecoveryReport              `json:"recovery,omitempty"`
}

func VerifyDir(dir, checkpointDir string) (*Report, error) {
	loaded, err := sampleio.LoadBundle(dir)
	if err != nil {
		return nil, err
	}
	report := &Report{Seed: loaded.Seed, IncrementalProofs: map[string]incremental.Proof{}}
	report.CriticalConversions = loaded.Critical

	projects := map[string]*model.Project{}
	truths := map[string]*model.Truth{}
	for _, lp := range loaded.Projects {
		v1, err := sampleio.LoadVersion(dir, lp.Name, model.SchemaV1)
		if err != nil {
			return nil, err
		}
		v2, err := sampleio.LoadVersion(dir, lp.Name, model.SchemaV2)
		if err != nil {
			return nil, err
		}
		v3, err := sampleio.LoadVersion(dir, lp.Name, model.SchemaV3)
		if err != nil {
			return nil, err
		}
		projects[lp.Name] = v1.Project
		truths[lp.Name] = lp.Truth

		report.runCheck(fmt.Sprintf("schema-equivalence:%s", lp.Name), func() (any, error) {
			if err := jsonEqual(v1.Project, v2.Project); err != nil {
				return nil, fmt.Errorf("v1/v2: %w", err)
			}
			if err := jsonEqual(v1.Project, v3.Project); err != nil {
				return nil, fmt.Errorf("v1/v3: %w", err)
			}
			return map[string]any{"tenants": len(v1.Project.Tenants), "nodes": len(v1.Project.Nodes), "events": len(v1.Project.Events)}, nil
		})

		report.runCheck(fmt.Sprintf("truth-settlement:%s", lp.Name), func() (any, error) {
			result, err := engine.New(v1.Project).Run()
			if err != nil {
				return nil, err
			}
			if err := jsonEqual(result.Reports, lp.Truth.Ticks); err != nil {
				return nil, err
			}
			return map[string]any{"ticks": len(result.Reports), "fingerprint": engine.Fingerprint(v1.Project)}, nil
		})

		report.runCheck(fmt.Sprintf("order-independence:%s", lp.Name), func() (any, error) {
			hashes, err := checkOrderIndependence(v1.Project, loaded.Seed)
			if err != nil {
				return nil, err
			}
			return map[string]any{"shuffledRuns": len(hashes), "stateHash": hashes[0]}, nil
		})
	}

	report.runCheck("optional-burst-degradation", func() (any, error) {
		return checkBurstDegradation(projects)
	})
	report.runCheck("fixed-point-critical-cases", func() (any, error) {
		return checkCritical(loaded.Critical)
	})
	report.runCheck("gate-latency-and-determinism", func() (any, error) {
		return checkGateLatency(loaded.Seed)
	})
	report.runCheck("concurrent-ledger-hashes", func() (any, error) {
		return checkConcurrentLedgers(projects)
	})
	if main := projects["cluster-main"]; main != nil && truths["cluster-main"] != nil {
		truth := truths["cluster-main"]
		report.runCheck("concurrent-event-submission", func() (any, error) {
			return checkConcurrentSubmission(main, truth)
		})
	}
	report.runCheck("worker-backpressure", func() (any, error) {
		return checkWorkerBackpressure()
	})
	report.runCheck("tenant-queue-backpressure", func() (any, error) {
		return checkTenantQueueBackpressure()
	})
	report.runCheck("single-node-occupancy", func() (any, error) {
		return checkSingleNodeOccupancy()
	})

	if main := projects["cluster-main"]; main != nil {
		report.runCheck("incremental-invalidation", func() (any, error) {
			proofs, err := checkIncremental(main, report.IncrementalProofs)
			return proofs, err
		})
		report.runCheck("checkpoint-recovery", func() (any, error) {
			rec, metrics, err := checkRecovery(main, truths["cluster-main"].Ticks, checkpointDir)
			report.Recovery = rec
			if metrics != nil {
				report.RuntimeMetrics = metrics
			}
			return rec, err
		})
	}

	report.OK = true
	for _, c := range report.Checks {
		if !c.OK {
			report.OK = false
			break
		}
	}
	return report, nil
}

func (r *Report) runCheck(name string, fn func() (any, error)) {
	check := Check{Name: name}
	data, err := fn()
	if err != nil {
		check.OK = false
		check.Detail = err.Error()
	} else {
		check.OK = true
		check.Data = data
	}
	r.Checks = append(r.Checks, check)
}

func jsonEqual(a, b any) error {
	aj, err := canonicalJSON(a)
	if err != nil {
		return err
	}
	bj, err := canonicalJSON(b)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(aj, bj) {
		aa, _ := json.Marshal(aj)
		bb, _ := json.Marshal(bj)
		return fmt.Errorf("JSON mismatch\nA=%s\nB=%s", firstBytes(aa, 4000), firstBytes(bb, 4000))
	}
	return nil
}

func canonicalJSON(v any) (any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func firstBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}

func checkOrderIndependence(project *model.Project, seed int64) ([]string, error) {
	base, err := engine.New(project).Run()
	if err != nil {
		return nil, err
	}
	want, err := canonicalJSON(base.Reports)
	if err != nil {
		return nil, err
	}
	hashes := []string{base.Reports[len(base.Reports)-1].StateHash}
	for iteration := 0; iteration < 5; iteration++ {
		clone, err := incremental.CloneProject(project)
		if err != nil {
			return nil, err
		}
		rng := rand.New(rand.NewSource(seed + int64(iteration)))
		rng.Shuffle(len(clone.Events), func(i, j int) { clone.Events[i], clone.Events[j] = clone.Events[j], clone.Events[i] })
		result, err := engine.New(clone).Run()
		if err != nil {
			return nil, fmt.Errorf("shuffle %d: %w", iteration, err)
		}
		got, err := canonicalJSON(result.Reports)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(want, got) {
			return nil, fmt.Errorf("shuffle %d produced different reports", iteration)
		}
		hashes = append(hashes, result.Reports[len(result.Reports)-1].StateHash)
	}
	return hashes, nil
}

func checkBurstDegradation(projects map[string]*model.Project) (any, error) {
	main := projects["cluster-main"]
	if main == nil {
		return nil, errors.New("cluster-main missing")
	}
	doc := generate.V3WithoutBurst(main, "t-alpha")
	result, err := schema.LoadV3(doc)
	if err != nil {
		return nil, err
	}
	for _, tenant := range result.Project.Tenants {
		if tenant.ID != "t-alpha" {
			continue
		}
		if tenant.BurstFieldPresent || tenant.BurstEnabled {
			return nil, fmt.Errorf("missing burst field normalized as enabled: %+v", tenant)
		}
		if tenant.BurstDegradation != schema.BurstAbsentDegradation {
			return nil, fmt.Errorf("missing degradation annotation: %q", tenant.BurstDegradation)
		}
		return map[string]string{"tenant": tenant.ID, "degradation": tenant.BurstDegradation}, nil
	}
	return nil, errors.New("t-alpha not found in degraded v3")
}

func checkCritical(cases []model.ConversionDiagnostic) (any, error) {
	if len(cases) == 0 {
		return nil, errors.New("no critical conversion cases")
	}
	var within, detectedOverflow, detectedDrift bool
	for _, c := range cases {
		switch c.FieldName {
		case "capacity_kib_overflow_saturated":
			detectedOverflow = c.Check.Overflow && c.Check.Saturated && c.Actual == int64(^uint64(0)>>1)
		case "injected_quota_bytes_drift_probe":
			detectedDrift = !c.Check.WithinBound && c.Check.Diff > c.Check.Tolerance
		default:
			if !c.Check.WithinBound {
				return nil, fmt.Errorf("%s/%s tick %d exceeds tolerance: diff=%d tol=%d", c.TenantID, c.FieldName, c.Tick, c.Check.Diff, c.Check.Tolerance)
			}
			within = true
		}
	}
	if !within || !detectedOverflow || !detectedDrift {
		return nil, fmt.Errorf("critical coverage incomplete within=%v overflow=%v drift=%v", within, detectedOverflow, detectedDrift)
	}
	return map[string]any{"cases": len(cases), "overflowDetected": detectedOverflow, "driftDetected": detectedDrift}, nil
}

func checkGateLatency(seed int64) (any, error) {
	budget := time.Duration(model.DefaultRules().GateBudgetNs)
	inputs := make([]gate.Input, 96)
	for i := range inputs {
		inputs[i] = gate.Input{
			Tick: 1, EventID: fmt.Sprintf("lat-%03d", i), TenantID: fmt.Sprintf("lat-tenant-%d", i%7),
			NodeID: "lat-node", RequestedBytes: 4096, BudgetNs: budget.Nanoseconds(),
			NodeCapacityBytes: 1 << 40, TenantAllowedRemaining: 1 << 30, TenantAbsoluteMaxBytes: 1 << 30,
		}
	}
	degrees := []int{1, 2, 4, 8}
	var firstRecords []model.GateRecord
	var maxObserved int64
	var metrics runtime.MetricsSnapshot
	for _, workers := range degrees {
		decisions, m := runtime.RunGateBatch(gate.New(budget), inputs, workers)
		records := make([]model.GateRecord, len(decisions))
		for i, d := range decisions {
			records[i] = d.Record
			if d.ObservedLatencyNs > maxObserved {
				maxObserved = d.ObservedLatencyNs
			}
		}
		if workers == 4 {
			metrics = m
		}
		if firstRecords == nil {
			firstRecords = records
		} else if err := jsonEqual(firstRecords, records); err != nil {
			return nil, fmt.Errorf("worker degree %d changed gate records: %w", workers, err)
		}
	}
	if time.Duration(maxObserved) > budget {
		return nil, fmt.Errorf("observed gate latency %dns exceeds budget %dns", maxObserved, budget.Nanoseconds())
	}

	base := time.Unix(100, 0)
	calls := 0
	fakeNow := func() time.Time {
		calls++
		if calls == 1 {
			return base
		}
		return base.Add(budget + 5*time.Millisecond)
	}
	slowGate := &gate.FastGate{Budget: budget, Now: fakeNow}
	slow := slowGate.Decide(gate.Input{
		EventID: "slow", TenantID: "t", RequestedBytes: 1, BudgetNs: budget.Nanoseconds(),
		NodeCapacityBytes: 100, TenantAllowedRemaining: 100, TenantAbsoluteMaxBytes: 100,
	})
	if slow.Record.Conclusion != gate.ConclusionQueue || slow.Record.ReasonCode != model.CodeGateLatencyDegradedQueue {
		return nil, fmt.Errorf("latency overrun did not degrade to queue: %+v", slow.Record)
	}
	return map[string]any{
		"decisions": len(inputs), "workerDegrees": degrees, "maxObservedNs": maxObserved,
		"budgetNs": budget.Nanoseconds(), "forcedTimeoutCode": slow.Record.ReasonCode,
		"tenantObservations": len(metrics.Tenants),
		"metrics":            metrics,
	}, nil
}

func checkConcurrentLedgers(projects map[string]*model.Project) (any, error) {
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	hashes := map[string]string{}
	var mu sync.Mutex
	for name, project := range projects {
		for _, workers := range []int{1, 2, 4} {
			for repeat := 0; repeat < 2; repeat++ {
				wg.Add(1)
				go func(name string, workers, repeat int) {
					defer wg.Done()
					result, err := engine.New(project).Run()
					if err != nil {
						errs <- err
						return
					}
					last := result.Reports[len(result.Reports)-1].StateHash
					mu.Lock()
					if old, ok := hashes[name]; ok && old != last {
						errs <- fmt.Errorf("%s hash changed under concurrency", name)
					}
					hashes[name] = last
					mu.Unlock()
					_ = workers
				}(name, workers, repeat)
			}
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return hashes, nil
}

func checkConcurrentSubmission(project *model.Project, truth *model.Truth) (any, error) {
	pool := runtime.NewPool(project.Rules.WorkerBudget, len(project.Events))
	mailbox := &runtime.EventMailbox{}
	for _, event := range project.Events {
		event := event
		code := pool.Submit(event.TenantID, func() error {
			mailbox.Submit(event)
			return nil
		})
		if code != model.CodeAdmitted {
			pool.CloseAndWait()
			return nil, fmt.Errorf("event submission rejected: %s", code)
		}
	}
	pool.CloseAndWait()
	events := mailbox.Drain()
	if len(events) != len(project.Events) {
		return nil, fmt.Errorf("mailbox events=%d want %d", len(events), len(project.Events))
	}
	clone, err := incremental.CloneProject(project)
	if err != nil {
		return nil, err
	}
	clone.Events = events
	result, err := engine.New(clone).Run()
	if err != nil {
		return nil, err
	}
	if err := jsonEqual(result.Reports, truth.Ticks); err != nil {
		return nil, err
	}
	return map[string]any{"submittedEvents": len(events), "metrics": pool.Metrics.Snapshot()}, nil
}

func checkWorkerBackpressure() (any, error) {
	pool := runtime.NewPool(1, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	if code := pool.Submit("bp-tenant", func() error {
		close(started)
		<-release
		return nil
	}); code != model.CodeAdmitted {
		return nil, fmt.Errorf("blocking task rejected: %s", code)
	}
	<-started
	if code := pool.Submit("bp-tenant", func() error { return nil }); code != model.CodeAdmitted {
		return nil, fmt.Errorf("buffered task should be accepted, got %s", code)
	}
	code := pool.Submit("bp-tenant", func() error { return nil })
	if code != model.CodeBackpressureWorkers {
		close(release)
		pool.CloseAndWait()
		return nil, fmt.Errorf("expected worker backpressure, got %q", code)
	}
	close(release)
	pool.CloseAndWait()
	metrics := pool.Metrics.Snapshot()
	if metrics.Backpressure[model.CodeBackpressureWorkers] != 1 {
		return nil, fmt.Errorf("unexpected metrics: %+v", metrics.Backpressure)
	}
	return map[string]any{"code": code, "metrics": metrics}, nil
}

func checkTenantQueueBackpressure() (any, error) {
	project := queuePressureProject()
	result, err := engine.New(project).Run()
	if err != nil {
		return nil, err
	}
	var found bool
	for _, tick := range result.Reports {
		for _, rejection := range tick.Rejections {
			if rejection.Code == model.CodeBackpressureQueueFull && rejection.ConstraintID == model.ConstraintQueueBound {
				found = true
			}
		}
	}
	if !found {
		return nil, errors.New("tenant queue full business code not found")
	}
	return map[string]string{"code": model.CodeBackpressureQueueFull}, nil
}

func queuePressureProject() *model.Project {
	rules := model.DefaultRules()
	rules.MaxQueuePerTenant = 1
	return &model.Project{
		ProjectID: "queue-pressure", TickDurationMs: 100, Rules: rules,
		Nodes: []model.Node{{ID: "small-node", RackID: "r", CapacityBytes: 100, AllowedTenants: []string{"qp"}}},
		Tenants: []model.Tenant{{
			ID: "qp", RackID: "r", HomeNodeID: "small-node", Priority: 1, Weight: 1,
			QuotaBytes: 10_000, GuaranteeBytes: 0, BurstEnabled: false,
			WindowTicks: 10, MaxWaitTicks: 5,
		}},
		Events: []model.Event{
			{ID: "qp-1", Tick: 1, Kind: model.EventApply, TenantID: "qp", NodeID: "small-node", AmountBytes: 100},
			{ID: "qp-2", Tick: 1, Kind: model.EventApply, TenantID: "qp", NodeID: "small-node", AmountBytes: 100},
			{ID: "qp-3", Tick: 1, Kind: model.EventApply, TenantID: "qp", NodeID: "small-node", AmountBytes: 100},
		},
	}
}

func checkSingleNodeOccupancy() (any, error) {
	project := singleNodeProject()
	result, err := engine.New(project).Run()
	if err != nil {
		return nil, err
	}
	tick1 := result.Reports[0]
	nodeUsed := map[string]int64{}
	for _, n := range tick1.Nodes {
		nodeUsed[n.NodeID] = n.UsedBytes
	}
	if nodeUsed["sn-a"] != 1024*1024 || nodeUsed["sn-b"] != 0 {
		return nil, fmt.Errorf("tenant counted on two nodes: %+v", nodeUsed)
	}
	var queued bool
	for _, q := range tick1.Queue {
		if q.ConstraintID == model.ConstraintSingleNode {
			queued = true
		}
	}
	if !queued {
		return nil, errors.New("second-node request was not queued under C-TENANT-SINGLE-NODE")
	}
	return nodeUsed, nil
}

func singleNodeProject() *model.Project {
	oneMiB := 1024 * 1024
	return &model.Project{
		ProjectID: "single-node", TickDurationMs: 100, Rules: model.DefaultRules(),
		Nodes: []model.Node{
			{ID: "sn-a", RackID: "r", CapacityBytes: 10 * int64(oneMiB), AllowedTenants: []string{"sn-tenant"}},
			{ID: "sn-b", RackID: "r", CapacityBytes: 10 * int64(oneMiB), AllowedTenants: []string{"sn-tenant"}},
		},
		Tenants: []model.Tenant{{
			ID: "sn-tenant", RackID: "r", HomeNodeID: "sn-a", Priority: 1, Weight: 1,
			QuotaBytes: 100 * int64(oneMiB), GuaranteeBytes: 0, BurstEnabled: true, BurstCapBytes: 10 * int64(oneMiB),
			MaxWaitTicks: 2,
		}},
		Events: []model.Event{
			{ID: "sn-01-a", Tick: 1, Kind: model.EventApply, TenantID: "sn-tenant", NodeID: "sn-a", AmountBytes: int64(oneMiB)},
			{ID: "sn-02-b", Tick: 1, Kind: model.EventApply, TenantID: "sn-tenant", NodeID: "sn-b", AmountBytes: int64(oneMiB)},
		},
	}
}

func checkIncremental(project *model.Project, proofs map[string]incremental.Proof) (any, error) {
	manager, err := incremental.NewManager(project)
	if err != nil {
		return nil, err
	}
	edits := []incremental.Edit{
		{Kind: incremental.EditWeightEvent, Target: "evt-08-bravo-weight-up", NewWeight: 1},
		{Kind: incremental.EditTenantQuota, Target: "t-charlie", AtTick: 5, NewPPM: 250_000},
		{Kind: incremental.EditNodeCapacity, Target: "node-b", AtTick: 5, NewBytes: 200 * generate.MiB},
	}
	summaries := []map[string]any{}
	for _, edit := range edits {
		result, err := manager.Apply(edit)
		if err != nil {
			return nil, err
		}
		full, err := manager.FullRecompute(edit)
		if err != nil {
			return nil, err
		}
		if err := incremental.EqualReports(result.Reports, full); err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s:%s", edit.Kind, edit.Target)
		proofs[key] = result.Proof
		if len(result.Proof.ReusedTicks) == 0 {
			return nil, fmt.Errorf("%s did not reuse any unaffected ticks", key)
		}
		summaries = append(summaries, map[string]any{
			"edit": key, "recomputed": len(result.Proof.RecomputedTicks),
			"reused": len(result.Proof.ReusedTicks), "closure": result.Proof.Closure,
		})
	}
	return summaries, nil
}

func checkRecovery(project *model.Project, expected []model.TickReport, dir string) (*RecoveryReport, *runtime.MetricsSnapshot, error) {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "quotakernel-checkpoint")
	}
	_ = os.RemoveAll(dir)
	store := checkpoint.NewStore(dir)
	fingerprint := engine.Fingerprint(project)
	eng := engine.New(project)
	state := engine.NewState(project)

	for tick := model.Tick(1); tick <= 3; tick++ {
		if _, err := eng.RunFrom(state, tick); err != nil {
			return nil, nil, err
		}
		if _, err := store.Commit(state.Snapshot(fingerprint)); err != nil {
			return nil, nil, err
		}
	}
	if _, err := eng.RunFrom(state, 4); err != nil {
		return nil, nil, err
	}
	orphanName, _, err := store.WriteEntity(state.Snapshot(fingerprint))
	if err != nil {
		return nil, nil, err
	}

	snapshot, meta, err := store.Load(fingerprint)
	if err != nil {
		return nil, nil, err
	}
	if snapshot.CommittedTick != 3 || meta.CommittedTick != 3 {
		return nil, nil, fmt.Errorf("recovery read tick %d, want 3", snapshot.CommittedTick)
	}
	recovered, err := engine.StateFromSnapshot(project, snapshot)
	if err != nil {
		return nil, nil, err
	}
	startRecoveryTick := recovered.Tick
	remaining, err := eng.RunFrom(recovered, eng.Horizon())
	if err != nil {
		return nil, nil, err
	}
	if len(expected) < 4 {
		return nil, nil, fmt.Errorf("truth too short: %d", len(expected))
	}
	if err := jsonEqual(remaining.Reports, expected[3:]); err != nil {
		return nil, nil, fmt.Errorf("recovered reports mismatch: %w", err)
	}

	mismatchDir := filepath.Join(dir, "version-mismatch")
	badStore := checkpoint.NewStore(mismatchDir)
	if err := badStore.Ensure(); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(mismatchDir, "committed.json"), []byte(`{"formatVersion":999}`), 0o644); err != nil {
		return nil, nil, err
	}
	_, _, loadErr := badStore.Load(fingerprint)
	if !errors.Is(loadErr, checkpoint.ErrVersionMismatch) {
		return nil, nil, fmt.Errorf("expected version mismatch, got %v", loadErr)
	}
	if err := badStore.Reset(); err != nil {
		return nil, nil, err
	}
	full, err := engine.New(project).Run()
	if err != nil {
		return nil, nil, err
	}
	for _, report := range full.Reports {
		_ = report
	}

	metrics := runtime.NewMetrics().Snapshot()
	return &RecoveryReport{
		CheckpointDir: dir, CommittedTick: int64(snapshot.CommittedTick),
		RecoveredFromTick: int64(startRecoveryTick + 1), OrphanEntityIgnored: orphanName,
		TruthTicksCompared: len(remaining.Reports), VersionMismatchDetected: true,
		FullRecomputeAfterReset: true,
	}, &metrics, nil
}
