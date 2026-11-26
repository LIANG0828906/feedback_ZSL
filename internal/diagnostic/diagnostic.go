// Package diagnostic packages the acceptance checks used by the CLI and
// local HTTP workbench.
package diagnostic

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"quota-cluster/internal/bizerror"
	"quota-cluster/internal/checkpoint"
	"quota-cluster/internal/engine"
	"quota-cluster/internal/incremental"
	"quota-cluster/internal/model"
	"quota-cluster/internal/projectio"
	"quota-cluster/internal/sample"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail"`
}

type IncrementalSummary struct {
	Edit            model.Edit               `json:"edit"`
	StartTick       int64                    `json:"start_tick"`
	Dependencies    []string                 `json:"dependencies"`
	RecomputedTicks int                      `json:"recomputed_ticks"`
	ReusedTicks     int                      `json:"reused_ticks"`
	FullEqual       bool                     `json:"full_equal"`
	Provenance      []incremental.Provenance `json:"provenance"`
}

type Report struct {
	Seed               int64                 `json:"seed"`
	OK                 bool                  `json:"ok"`
	ProjectID          string                `json:"project_id"`
	Checks             []Check               `json:"checks"`
	DecisionCount      int                   `json:"decision_count"`
	QueuePeak          int                   `json:"queue_peak"`
	CriticalCases      []sample.CriticalCase `json:"critical_cases"`
	Incremental        []IncrementalSummary  `json:"incremental"`
	CheckpointPath     string                `json:"checkpoint_path,omitempty"`
	CheckpointTick     int64                 `json:"checkpoint_tick,omitempty"`
	StableBusinessCode string                `json:"stable_business_code_example"`
}

func Verify(seed int64, checkpointPath string) (*Report, *sample.Bundle, error) {
	bundle, err := sample.Generate(seed)
	if err != nil {
		return nil, nil, err
	}
	report := &Report{Seed: seed, ProjectID: bundle.Project.ProjectID, CriticalCases: bundle.CriticalCases}
	add := func(name string, ok bool, detail string) {
		report.Checks = append(report.Checks, Check{Name: name, OK: ok, Detail: detail})
	}

	again, err := sample.Generate(seed)
	detOK := err == nil && string(again.V1) == string(bundle.V1) && string(again.V2) == string(bundle.V2) && string(again.V3) == string(bundle.V3) && string(again.TruthJSON) == string(bundle.TruthJSON)
	add("deterministic_seed_byte_for_byte", detOK, fmt.Sprintf("seed=%d regenerated three schemas and truth", seed))

	projects := make([]*model.Project, 0, 3)
	equivalent := true
	for i, doc := range [][]byte{bundle.V1, bundle.V2, bundle.V3} {
		p, loadErr := projectio.Load(doc)
		if loadErr != nil {
			equivalent = false
			add(fmt.Sprintf("load_schema_v%d", i+1), false, loadErr.Error())
			continue
		}
		p.SourceSchema = ""
		want := *bundle.Project
		want.SourceSchema = ""
		if !reflect.DeepEqual(p, &want) {
			equivalent = false
			add(fmt.Sprintf("load_schema_v%d", i+1), false, "canonical model mismatch")
		}
		projects = append(projects, p)
	}
	add("three_schema_canonical_equivalence", equivalent, "flat/nested, byte/KiB, tick/ms, permille/bp normalized")

	truthOK := true
	if len(projects) == 3 {
		for i, p := range projects {
			r, runErr := engine.Run(p, engine.Options{Workers: bundle.Project.Policy.WorkerBudget, BatchSize: 2})
			if runErr != nil || !reflect.DeepEqual(r.Ticks, bundle.Truth.Ticks) || !reflect.DeepEqual(r.Decisions, bundle.Truth.Decisions) {
				truthOK = false
				add(fmt.Sprintf("schema_v%d_truth", i+1), false, fmt.Sprintf("err=%v", runErr))
			}
		}
	}
	add("ledger_and_gate_match_truth", truthOK, fmt.Sprintf("%d decisions across %d ticks", len(bundle.Truth.Decisions), len(bundle.Truth.Ticks)))

	parallelOK := true
	first, err := engine.Run(bundle.Project, engine.Options{Workers: 1, BatchSize: 1})
	if err != nil {
		parallelOK = false
	} else {
		for _, opts := range []engine.Options{{Workers: 2, BatchSize: 1}, {Workers: 4, BatchSize: 2}, {Workers: 8, BatchSize: 3}, {Workers: 16, BatchSize: 16}} {
			r, runErr := engine.Run(bundle.Project, opts)
			if runErr != nil || !reflect.DeepEqual(first.Ticks, r.Ticks) || !reflect.DeepEqual(first.Decisions, r.Decisions) {
				parallelOK = false
				break
			}
		}
	}
	add("parallel_batch_conclusion_stability", parallelOK, "workers 1/2/4/8/16 and batch sizes 1/2/3/16 produce identical reports")

	ambiguous := `{"schema_version":"quota.project.v1","nodes":[{"id":"n","capacity_bytes":1,"capacity_kib":1}]}`
	_, ambErr := projectio.Load([]byte(ambiguous))
	be, ambOK := bizerror.As(ambErr)
	if ambOK && be.Code == bizerror.CodeFieldAmbiguous {
		report.StableBusinessCode = be.Code
		add("ambiguous_field_rejected", true, be.Error())
	} else {
		add("ambiguous_field_rejected", false, fmt.Sprintf("err=%v", ambErr))
	}

	backpressureOK := checkBackpressure()
	add("worker_budget_backpressure_code", backpressureOK, bizerror.CodeBackpressureWorkers)

	incOK, summaries, incDetail := checkIncremental(bundle.Project)
	report.Incremental = summaries
	add("incremental_invalidation_equivalence", incOK, incDetail)

	cpOK, cpPath, cpTick, cpDetail := checkCheckpoint(bundle, checkpointPath)
	report.CheckpointPath = cpPath
	report.CheckpointTick = cpTick
	add("sqlite_checkpoint_recovery", cpOK, cpDetail)

	report.DecisionCount = len(bundle.Truth.Decisions)
	for _, tick := range bundle.Truth.Ticks {
		if len(tick.Queue) > report.QueuePeak {
			report.QueuePeak = len(tick.Queue)
		}
	}

	report.OK = true
	for _, c := range report.Checks {
		if !c.OK {
			report.OK = false
			break
		}
	}
	return report, bundle, nil
}

func checkBackpressure() bool {
	exec := engine.NewBoundedExecutor(1, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	if err := exec.TrySubmit(func() { close(started); <-release }); err != nil {
		return false
	}
	<-started
	if err := exec.TrySubmit(func() {}); err != nil {
		return false
	}
	err := exec.TrySubmit(func() {})
	close(release)
	exec.CloseAndWait()
	be, ok := bizerror.As(err)
	return ok && be.Code == bizerror.CodeBackpressureWorkers
}

func checkIncremental(p *model.Project) (bool, []IncrementalSummary, string) {
	svc, err := incremental.NewService(p, engine.Options{Workers: 3, BatchSize: 2})
	if err != nil {
		return false, nil, err.Error()
	}
	edits := []model.Edit{
		{Kind: model.EditNodeCapacity, TargetID: "node-a", AtTick: 2, Value: p.Nodes[0].CapacityBytes + sample.MiB*256},
		{Kind: model.EditTenantQuota, TargetID: "tenant-alpha", AtTick: 1, Value: sample.MiB * 3000},
		{Kind: model.EditTenantGuarantee, TargetID: "tenant-beta", AtTick: 2, Value: sample.MiB * 250},
		{Kind: model.EditWeightEvent, EventSequence: 4, Value: 180},
	}
	summaries := []IncrementalSummary{}
	for _, edit := range edits {
		res, err := svc.Apply(edit)
		if err != nil || !res.FullEqual {
			return false, summaries, fmt.Sprintf("edit %+v: %v", edit, err)
		}
		summaries = append(summaries, IncrementalSummary{Edit: res.Edit, StartTick: res.StartTick, Dependencies: res.Dependencies, RecomputedTicks: res.RecomputedTicks, ReusedTicks: res.ReusedTicks, FullEqual: res.FullEqual, Provenance: res.Provenance})
	}
	return true, summaries, "node, quota, guarantee, and weight edits match full recomputation"
}

func checkCheckpoint(bundle *sample.Bundle, path string) (bool, string, int64, string) {
	run, err := engine.RunWithStates(bundle.Project, engine.Options{Workers: 4, BatchSize: 2})
	if err != nil {
		return false, path, 0, err.Error()
	}
	if path == "" {
		path = filepath.Join(os.TempDir(), "quota-cluster-checkpoint.sqlite")
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	last := run.Report.Ticks[len(run.Report.Ticks)-1]
	hash := incremental.Digest(bundle.Project)
	cp := &checkpoint.Checkpoint{
		FormatVersion: model.FormatVersion, ProjectID: bundle.Project.ProjectID, ProjectHash: hash,
		CommittedTick: bundle.Project.LogicalTicks,
		TenantStates:  mapsToTenantSlice(last.TenantStates),
		NodeStates:    mapsToNodeSlice(last.NodeStates),
		Queue:         last.Queue, Decisions: run.Report.Decisions,
		EngineState: run.States[bundle.Project.LogicalTicks],
	}
	if err := checkpoint.SaveCheckpoint(path, cp, checkpoint.SaveOptions{ProjectHash: hash, State: cp.EngineState}); err != nil {
		return false, path, 0, err.Error()
	}
	loaded, err := checkpoint.LoadCheckpoint(path, hash)
	if err != nil {
		return false, path, 0, err.Error()
	}
	if loaded.CommittedTick != cp.CommittedTick || len(loaded.TenantStates) != len(cp.TenantStates) || !reflect.DeepEqual(loaded.EngineState, cp.EngineState) {
		return false, path, loaded.CommittedTick, "loaded checkpoint state mismatch"
	}
	return true, path, loaded.CommittedTick, fmt.Sprintf("SQLite checkpoint tick=%d tenants=%d nodes=%d", loaded.CommittedTick, len(loaded.TenantStates), len(loaded.NodeStates))
}

func mapsToTenantSlice(in map[string]model.TenantSnapshot) []model.TenantSnapshot {
	out := make([]model.TenantSnapshot, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

func mapsToNodeSlice(in map[string]model.NodeSnapshot) []model.NodeSnapshot {
	out := make([]model.NodeSnapshot, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}
