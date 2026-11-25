package acceptance

import (
	"os"
	"path/filepath"
	"reflect"

	"grayrelease/internal/checkpoint"
	"grayrelease/internal/durable"
	"grayrelease/internal/engine"
	"grayrelease/internal/incremental"
	"grayrelease/internal/loader"
	"grayrelease/internal/model"
	"grayrelease/internal/sample"
	"grayrelease/internal/scheduler"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type SchemaLoad struct {
	Project  *model.Project  `json:"project"`
	Requests []model.Request `json:"requests"`
}

type Report struct {
	OK                   bool                     `json:"ok"`
	SampleDir            string                   `json:"sample_dir"`
	Checks               []Check                  `json:"checks"`
	Loads                map[string]SchemaLoad    `json:"loads"`
	Truth                *sample.ParseTruth       `json:"truth"`
	Simulation           *engine.SimulationResult `json:"simulation"`
	SchemaDecisionCounts map[string]int           `json:"schema_decision_counts"`
	ParallelWorkers1     int32                    `json:"parallel_peak_workers_1"`
	ParallelWorkers8     int32                    `json:"parallel_peak_workers_8"`
	BufferBackpressure   int                      `json:"buffer_backpressure_count"`
	WorkerBudgetCode     string                   `json:"worker_budget_code"`
	SampleEdit           *incremental.Report      `json:"sample_edit"`
	WeightEdit           *incremental.Report      `json:"weight_edit"`
	CheckpointTick       int                      `json:"checkpoint_tick"`
}

func DemoConfig() engine.Config {
	return engine.Config{
		WorkerBudget: 4, RouteBufferPerService: 32,
		MirrorConcurrencyPerService: 1, ShadowBufferPerService: 2,
		ShadowOnFull: "drop", ShadowLatencyTicks: 1, ShadowTimeoutTicks: 2,
	}
}

func Run(sampleDir string) (*Report, error) {
	if err := sample.WriteSet(sampleDir); err != nil {
		return nil, err
	}
	report := &Report{SampleDir: sampleDir, Loads: map[string]SchemaLoad{}, SchemaDecisionCounts: map[string]int{}}
	add := func(name string, ok bool, detail string) {
		report.Checks = append(report.Checks, Check{Name: name, OK: ok, Detail: detail})
	}

	truth, err := sample.BuildTruth()
	if err != nil {
		return nil, err
	}
	report.Truth = truth

	versions := []struct{ version, suffix string }{
		{model.SchemaV1, "v1"}, {model.SchemaV2, "v2"}, {model.SchemaV3, "v3"},
	}
	var canonical *model.Project
	var canonicalReqs []model.Request
	schemaResults := map[string]*engine.SimulationResult{}
	for _, item := range versions {
		project, err := loader.LoadProjectFile(filepath.Join(sampleDir, "project-"+item.suffix+".json"))
		if err != nil {
			add("load-"+item.suffix, false, err.Error())
			return report, err
		}
		requests, err := loader.LoadRequestsFile(filepath.Join(sampleDir, "requests-"+item.suffix+".json"), item.version, project.TickIntervalMS)
		if err != nil {
			add("requests-"+item.suffix, false, err.Error())
			return report, err
		}
		report.Loads[item.version] = SchemaLoad{Project: project, Requests: requests}
		if canonical == nil {
			canonical = project
			canonicalReqs = requests
		} else {
			if !projectsEquivalent(canonical, project) {
				add("schema-equivalence-"+item.suffix, false, "normalized projects differ")
			}
			if !requestsEquivalent(canonicalReqs, requests) {
				add("request-equivalence-"+item.suffix, false, "normalized requests differ")
			}
		}
		result, err := engine.Simulate(project, requests, DemoConfig())
		if err != nil {
			return nil, err
		}
		schemaResults[item.version] = result
		report.SchemaDecisionCounts[item.version] = len(result.Decisions)
	}
	add("three-schema-load-equivalence", projectsAndRequestsAllEqual(report), "services, rule order, scales and normalized ticks match")

	baseResult := schemaResults[model.SchemaV2]
	report.Simulation = baseResult
	schemaDecisionsMatch := true
	for version, result := range schemaResults {
		if !sameDecisionSet(schemaResults[model.SchemaV1], result) {
			schemaDecisionsMatch = false
			add("schema-decisions-"+version, false, "decision mismatch versus v1")
		}
	}
	add("three-schema-decision-equivalence", schemaDecisionsMatch, "same request has identical side, rule, mirror and main conclusion under all schemas")
	add("decision-count", len(baseResult.Decisions) == truth.RequestCount, "all requests produced one route decision")
	add("main-never-blocked-by-shadow", allMainAllowed(baseResult), "mirror drops/deferrals leave main_allowed=true")
	add("mirror-caps", mirrorCapsRespected(baseResult, DemoConfig()), "active shadow calls never exceed declared per-service concurrency")
	add("stable-assignment", stableAssignments(baseResult), "same rule key keeps one side while weight is unchanged")
	add("zero-full-edges", zeroAndFullEdges(baseResult), "weight 0 is baseline-only and weight 10000 is canary-only")
	add("migration-truth", migrationTruthMatches(truth), "enumerated weight migration keys equal generator truth")

	parallel1, err := scheduler.RouteParallelByTick(canonical, canonicalReqs, withWorkers(DemoConfig(), 1), scheduler.Options{})
	if err != nil {
		return nil, err
	}
	parallel8, err := scheduler.RouteParallelByTick(canonical, canonicalReqs, withWorkers(DemoConfig(), 8), scheduler.Options{})
	if err != nil {
		return nil, err
	}
	report.ParallelWorkers1 = parallel1.PeakActive
	report.ParallelWorkers8 = parallel8.PeakActive
	add("parallel-determinism", parallelDecisionsEqual(parallel1.Decisions, parallel8.Decisions), "worker budget 1 and 8 produce identical ordered decisions")

	zeroBufferConfig := withWorkers(DemoConfig(), 2)
	zeroBufferConfig.RouteBufferPerService = 0
	backpressure, err := scheduler.RouteParallel(canonical, canonicalReqs, zeroBufferConfig)
	if err != nil {
		return nil, err
	}
	report.BufferBackpressure = len(backpressure.Backpressure)
	add("route-buffer-backpressure", len(backpressure.Backpressure) == len(canonicalReqs), "zero per-service buffer returns E_ROUTE_BUFFER_FULL instead of silent queueing")
	_, workerErr := scheduler.RouteParallel(canonical, canonicalReqs, withWorkers(DemoConfig(), 0))
	if be, ok := model.AsBizError(workerErr); ok {
		report.WorkerBudgetCode = be.Code
	}
	add("worker-budget-code", report.WorkerBudgetCode == engine.CodeWorkerBudget, "non-positive worker budget returns E_WORKER_BUDGET")

	manager, err := incremental.NewManager(canonical, canonicalReqs, DemoConfig())
	if err != nil {
		return nil, err
	}
	modulus := uint64(2)
	sampleEdit := incremental.Edit{
		ID: "edit-inventory-sample-at-tick-2", Kind: "mirror_sample", Service: "inventory",
		RuleID: "R-INV-CN", AtTick: 2, SampleModulus: &modulus,
	}
	sampleReport, err := manager.Apply(sampleEdit)
	if err != nil {
		return nil, err
	}
	report.SampleEdit = sampleReport
	add("incremental-sample-equivalence", sampleReport.EquivalentToFull, "service-local sample edit matches full timeline recomputation")

	weight := 6500
	weightEdit := incremental.Edit{
		ID: "edit-checkout-weight-at-tick-3", Kind: "rule_weight", Service: "checkout",
		RuleID: "R-CHK-BETA", AtTick: 3, WeightBPS: &weight,
	}
	weightReport, err := manager.Apply(weightEdit)
	if err != nil {
		return nil, err
	}
	report.WeightEdit = weightReport
	add("incremental-weight-equivalence", weightReport.EquivalentToFull, "weight edit and stable-key closure match full recomputation")

	tick, err := checkpointRoundtrip(sampleDir, canonical, canonicalReqs)
	if err != nil {
		return nil, err
	}
	report.CheckpointTick = tick
	add("checkpoint-roundtrip", tick >= sample.TickCount, "durable runner committed through drain interval and reopened")

	report.OK = true
	for _, check := range report.Checks {
		if !check.OK {
			report.OK = false
		}
	}
	return report, nil
}

func projectsAndRequestsAllEqual(report *Report) bool {
	base := report.Loads[model.SchemaV1]
	for version, load := range report.Loads {
		if version == model.SchemaV1 {
			continue
		}
		if !projectsEquivalent(base.Project, load.Project) || !requestsEquivalent(base.Requests, load.Requests) {
			return false
		}
	}
	return true
}

func projectsEquivalent(a, b *model.Project) bool {
	if a.EngineeringVersion != b.EngineeringVersion || a.TickIntervalMS != b.TickIntervalMS || a.Name != b.Name {
		return false
	}
	if !reflect.DeepEqual(a.Services, b.Services) || !rulesEquivalent(a.Rules, b.Rules) {
		return false
	}
	return mirrorPoliciesEquivalent(a.Mirrors, b.Mirrors)
}

func rulesEquivalent(a, b []model.Rule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		left, right := a[i], b[i]
		if left.ID != right.ID || left.RuleNo != right.RuleNo || left.Service != right.Service ||
			left.Priority != right.Priority || left.Order != right.Order || left.WeightBPS != right.WeightBPS {
			return false
		}
		if !reflect.DeepEqual(left.Match.Labels, right.Match.Labels) {
			return false
		}
	}
	return true
}

func mirrorPoliciesEquivalent(a, b []model.MirrorPolicy) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Service != b[i].Service || a[i].RuleID != b[i].RuleID || a[i].RatioBPS != b[i].RatioBPS {
			return false
		}
		if (a[i].Sample == nil) != (b[i].Sample == nil) {
			return false
		}
		if a[i].Sample != nil && (a[i].Sample.Modulus != b[i].Sample.Modulus || a[i].Sample.Remainder != b[i].Sample.Remainder) {
			return false
		}
	}
	return true
}

func requestsEquivalent(a, b []model.Request) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Tick != b[i].Tick || a[i].RequestID != b[i].RequestID || a[i].Service != b[i].Service ||
			a[i].Key != b[i].Key || a[i].ShadowFault != b[i].ShadowFault || !reflect.DeepEqual(a[i].Labels, b[i].Labels) {
			return false
		}
	}
	return true
}

func allMainAllowed(result *engine.SimulationResult) bool {
	for _, decision := range result.Decisions {
		if !decision.MainAllowed || decision.MainConclusion != "allow:"+decision.Side {
			return false
		}
	}
	return true
}

func mirrorCapsRespected(result *engine.SimulationResult, config engine.Config) bool {
	for _, tick := range result.Reports {
		for service, report := range tick.Services {
			if report.ActiveOccupancy > config.MirrorConcurrencyPerService {
				return false
			}
			if report.QueueOccupancy > config.ShadowBufferPerService {
				return false
			}
			_ = service
		}
	}
	return true
}

func stableAssignments(result *engine.SimulationResult) bool {
	sides := map[string]string{}
	for _, decision := range result.Decisions {
		if decision.SelectedRuleID == "" {
			continue
		}
		key := decision.SelectedRuleID + "|" + decision.Key
		if side, ok := sides[key]; ok && side != decision.Side {
			return false
		}
		sides[key] = decision.Side
	}
	return true
}

func zeroAndFullEdges(result *engine.SimulationResult) bool {
	for _, decision := range result.Decisions {
		switch decision.SelectedRuleID {
		case "R-PAY-DEFAULT", "R-LED-AUDIT":
			if decision.Side != model.SideBaseline || decision.Bucket < 0 && decision.WeightBPS == 0 {
				if decision.WeightBPS == 0 && decision.Side != model.SideBaseline {
					return false
				}
			}
		case "R-PAY-VIP":
			if decision.Side != model.SideCanary {
				return false
			}
		}
		if decision.WeightBPS == 0 && decision.Side != model.SideBaseline {
			return false
		}
		if decision.WeightBPS == model.CanonicalBasis && decision.Side != model.SideCanary {
			return false
		}
	}
	return true
}

func migrationTruthMatches(truth *sample.ParseTruth) bool {
	project := sample.CanonicalProject()
	changed := sample.CanonicalProject()
	for i := range changed.Rules {
		if changed.Rules[i].ID == "R-CHK-BETA" {
			changed.Rules[i].WeightBPS = 6500
		}
	}
	keys := sample.KeyUniverse()
	migrations, err := engine.EnumerateMigrations(project, changed, "checkout", "R-CHK-BETA", keys)
	if err != nil || len(migrations) != len(truth.MigrationTruth) {
		return false
	}
	for i := range migrations {
		if migrations[i].Key != truth.MigrationTruth[i].Key || migrations[i].OldSide != truth.MigrationTruth[i].OldSide ||
			migrations[i].NewSide != truth.MigrationTruth[i].NewSide {
			return false
		}
	}
	return true
}

func parallelDecisionsEqual(a, b []engine.Decision) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !engine.EqualDecisions(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sameDecisionSet(a, b *engine.SimulationResult) bool {
	if a == nil || b == nil || len(a.Decisions) != len(b.Decisions) {
		return false
	}
	bMap := map[string]engine.Decision{}
	for _, decision := range b.Decisions {
		bMap[decision.RequestID] = decision
	}
	for _, decision := range a.Decisions {
		other, ok := bMap[decision.RequestID]
		if !ok || !engine.EqualDecisions(decision, other) {
			return false
		}
	}
	return true
}

func withWorkers(config engine.Config, workers int) engine.Config {
	config.WorkerBudget = workers
	return config
}

func checkpointRoundtrip(sampleDir string, project *model.Project, requests []model.Request) (int, error) {
	path := filepath.Join(sampleDir, "acceptance-checkpoint.db")
	_ = os.Remove(path)
	store, err := checkpoint.OpenStore(path, project.EngineeringVersion)
	if err != nil {
		return -1, err
	}
	runner := durable.NewRunner(project, requests, DemoConfig(), store)
	if _, err := runner.RunUntil(sample.TickCount+1, nil); err != nil {
		_ = store.Close()
		return -1, err
	}
	tick, commitErr := store.CommittedTick()
	_ = store.Close()
	if commitErr != nil {
		return -1, commitErr
	}
	reopened, err := checkpoint.OpenStore(path, project.EngineeringVersion)
	if err != nil {
		return -1, err
	}
	reopenedTick, err := reopened.CommittedTick()
	if err != nil {
		_ = reopened.Close()
		return -1, err
	}
	if reopenedTick != tick {
		_ = reopened.Close()
		return -1, model.NewError(checkpoint.CodeCorrupt, "checkpoint tick changed across reopen").
			With("before_close", tick).With("after_reopen", reopenedTick)
	}
	if err := reopened.Close(); err != nil {
		return -1, err
	}
	_ = os.Remove(path)
	return reopenedTick, nil
}
