package grayrelease_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"grayrelease/internal/acceptance"
	"grayrelease/internal/checkpoint"
	"grayrelease/internal/engine"
	"grayrelease/internal/fault"
	"grayrelease/internal/incremental"
	"grayrelease/internal/loader"
	"grayrelease/internal/model"
	"grayrelease/internal/sample"
	"grayrelease/internal/scheduler"
)

func TestAcceptanceReport(t *testing.T) {
	report, err := acceptance.Run(t.TempDir())
	if err != nil {
		t.Fatalf("acceptance run failed: %v", err)
	}
	for _, check := range report.Checks {
		if !check.OK {
			t.Errorf("check %s failed: %s", check.Name, check.Detail)
		}
	}
	if !report.OK {
		t.Fatal("acceptance report is not OK")
	}
}

func TestParallelAdmissionIsDeterministic(t *testing.T) {
	project := sample.CanonicalProject()
	requests := sample.GeneratedRequests()
	config := acceptance.DemoConfig()

	one, err := scheduler.RouteParallelByTick(project, requests, withWorkers(config, 1), scheduler.Options{})
	if err != nil {
		t.Fatal(err)
	}
	eight, err := scheduler.RouteParallelByTick(project, requests, withWorkers(config, 8), scheduler.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Decisions) != len(eight.Decisions) {
		t.Fatalf("decision count changed: %d vs %d", len(one.Decisions), len(eight.Decisions))
	}
	for i := range one.Decisions {
		if !engine.EqualDecisions(one.Decisions[i], eight.Decisions[i]) {
			t.Fatalf("decision %d differs under worker budgets", i)
		}
	}

	config.RouteBufferPerService = 0
	backpressured, err := scheduler.RouteParallelByTick(project, requests, config, scheduler.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(backpressured.Backpressure) != len(requests) {
		t.Fatalf("zero buffer should reject every admission, got %d/%d", len(backpressured.Backpressure), len(requests))
	}
	for _, item := range backpressured.Backpressure {
		if item.Code != engine.CodeRouteBufferFull {
			t.Fatalf("unexpected code %s", item.Code)
		}
	}

	if _, err := scheduler.NewGate(0); err == nil {
		t.Fatal("zero worker gate must fail")
	} else if be, ok := model.AsBizError(err); !ok || be.Code != engine.CodeWorkerBudget {
		t.Fatalf("expected E_WORKER_BUDGET, got %v", err)
	}
}

func TestMirrorFailuresNeverChangeMainDecision(t *testing.T) {
	project := sample.CanonicalProject()
	baseRequests := sample.GeneratedRequests()
	config := acceptance.DemoConfig()
	config.MirrorConcurrencyPerService = 32
	config.ShadowBufferPerService = 128

	base, err := engine.Simulate(project, baseRequests, config)
	if err != nil {
		t.Fatal(err)
	}

	faultRequests := append([]model.Request{}, baseRequests...)
	rejectSet := map[string]bool{}
	timeoutSet := map[string]bool{}
	rejectCount := 0
	timeoutCount := 0
	for _, decision := range base.Decisions {
		if !decision.MirrorSelected {
			continue
		}
		if rejectCount < 10 {
			rejectSet[decision.RequestID] = true
			rejectCount++
		} else if timeoutCount < 10 {
			timeoutSet[decision.RequestID] = true
			timeoutCount++
		}
	}
	for i := range faultRequests {
		id := faultRequests[i].RequestID
		if rejectSet[id] {
			faultRequests[i].ShadowFault = "reject"
		}
		if timeoutSet[id] {
			faultRequests[i].ShadowFault = "timeout"
		}
	}

	faulty, err := engine.Simulate(project, faultRequests, config)
	if err != nil {
		t.Fatal(err)
	}
	baseByID := map[string]engine.Decision{}
	for _, decision := range base.Decisions {
		baseByID[decision.RequestID] = decision
	}
	sawRejected := false
	sawTimeout := false
	for _, decision := range faulty.Decisions {
		old := baseByID[decision.RequestID]
		if decision.Side != old.Side || decision.MainAllowed != old.MainAllowed ||
			decision.MainConclusion != old.MainConclusion || decision.SelectedRuleID != old.SelectedRuleID {
			t.Fatalf("main decision changed for %s after shadow fault", decision.RequestID)
		}
		if decision.ShadowOutcome == "rejected" {
			sawRejected = true
		}
		if decision.ShadowOutcome == "timeout" {
			sawTimeout = true
		}
	}
	if !sawRejected || !sawTimeout {
		t.Fatalf("expected both shadow outcomes, rejected=%v timeout=%v", sawRejected, sawTimeout)
	}
}

func TestShadowBufferFullCanDefer(t *testing.T) {
	project := sample.CanonicalProject()
	requests := sample.GeneratedRequests()
	config := acceptance.DemoConfig()
	config.MirrorConcurrencyPerService = 1
	config.ShadowBufferPerService = 0
	config.ShadowOnFull = "defer"
	result, err := engine.Simulate(project, requests, config)
	if err != nil {
		t.Fatal(err)
	}
	sawDeferred := false
	for _, decision := range result.Decisions {
		if decision.ShadowStatus == "deferred" {
			sawDeferred = true
			if decision.ShadowCode != engine.CodeShadowBufferFull || decision.Constraint == "" {
				t.Fatalf("deferred decision lacks stable code/constraint: %+v", decision)
			}
		}
		if !decision.MainAllowed {
			t.Fatal("shadow deferral changed main admission")
		}
	}
	if !sawDeferred {
		t.Fatal("expected deterministic deferred shadow requests")
	}
}

func TestCheckpointOrphanRecoveryAndVersionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := checkpoint.OpenStore(path, sample.EngineeringVersion)
	if err != nil {
		t.Fatal(err)
	}
	decision := &engine.Decision{RequestID: "orphan-req", Service: "checkout", Side: model.SideBaseline, MainAllowed: true}
	if err := store.WriteRouteMappings(5, []*engine.Decision{decision}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMirrorQueues(5, map[string]engine.BrokerState{"checkout": {NextSeq: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitTick(4); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := checkpoint.OpenStore(path, sample.EngineeringVersion)
	if err != nil {
		t.Fatal(err)
	}
	data, err := reopened.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if data.CommittedTick != 4 || data.DeletedFutureRoutes != 1 || data.DeletedFutureQueues != 1 {
		t.Fatalf("unexpected recovery data: %+v", data)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = checkpoint.OpenStore(path, "eng-different-snapshot")
	if be, ok := model.AsBizError(err); !ok || be.Code != checkpoint.CodeVersionMismatch {
		t.Fatalf("expected E_VERSION_MISMATCH, got %v", err)
	}
}

func TestFaultInjectionCodes(t *testing.T) {
	cases := map[string]string{
		"overlap":                 "E_RULE_OVERLAP",
		"bad-scale":               "E_SCALE_RANGE",
		"missing-service":         "E_SERVICE_MISSING",
		"ambiguous-scale":         "E_FIELD_AMBIGUOUS",
		"truncated-checkpoint":    "E_CKPT_TRUNCATED",
		"version-mismatch":        "E_VERSION_MISMATCH",
		"worker-budget-exhausted": scheduler.CodeWorkerBudgetExhausted,
	}
	for kind, expectedCode := range cases {
		t.Run(kind, func(t *testing.T) {
			bizErr := fault.Inject(kind, t.TempDir())
			if bizErr == nil {
				t.Fatalf("expected %s", expectedCode)
			}
			if bizErr.Code != expectedCode {
				t.Fatalf("expected %s, got %s: %v", expectedCode, bizErr.Code, bizErr)
			}
			if bizErr.Context == nil || len(bizErr.Context) == 0 {
				t.Fatal("error context should be locatable")
			}
		})
	}
}

func TestIncrementalClosureScope(t *testing.T) {
	project := sample.CanonicalProject()
	requests := sample.GeneratedRequests()
	manager, err := incremental.NewManager(project, requests, acceptance.DemoConfig())
	if err != nil {
		t.Fatal(err)
	}

	modulus := uint64(2)
	sampleReport, err := manager.Apply(incremental.Edit{
		ID: "test-sample", Kind: "mirror_sample", Service: "inventory",
		RuleID: "R-INV-CN", AtTick: 2, SampleModulus: &modulus,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sampleReport.EquivalentToFull {
		t.Fatal("sample edit is not equivalent to full recomputation")
	}
	if len(sampleReport.Closure.Services) != 1 || sampleReport.Closure.Services[0] != "inventory" {
		t.Fatalf("unexpected service closure: %v", sampleReport.Closure.Services)
	}
	if sampleReport.ReusedTicks == 0 || sampleReport.RecomputedRequests == 0 {
		t.Fatalf("expected reuse and recomputation, got reused=%d recomputed=%d",
			sampleReport.ReusedTicks, sampleReport.RecomputedRequests)
	}
	assertDecisionSources(t, sampleReport.Result)

	weight := 6500
	weightReport, err := manager.Apply(incremental.Edit{
		ID: "test-weight", Kind: "rule_weight", Service: "checkout",
		RuleID: "R-CHK-BETA", AtTick: 3, WeightBPS: &weight,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !weightReport.EquivalentToFull {
		t.Fatal("weight edit is not equivalent to full recomputation")
	}
	truth, err := sample.BuildTruth()
	if err != nil {
		t.Fatal(err)
	}
	if len(weightReport.Closure.StableKeys) != len(truth.MigrationTruth) {
		t.Fatalf("migration closure %v does not match truth count %d",
			weightReport.Closure.StableKeys, len(truth.MigrationTruth))
	}
}

func TestRecoveryScriptWithRealSIGKILL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping process recovery script in short mode")
	}
	cmd := exec.Command("bash", "scripts/recovery_check.sh")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("recovery script failed: %v\n%s", err, output)
	}
}

func assertDecisionSources(t *testing.T, result *engine.SimulationResult) {
	t.Helper()
	for _, decision := range result.Decisions {
		if decision.CacheState == "" || len(decision.InvalidationSources) == 0 {
			t.Fatalf("decision %s lacks cache state or invalidation source", decision.RequestID)
		}
	}
}

func TestOptionalSampleFieldRoundTrip(t *testing.T) {
	project := sample.CanonicalProject()
	modulus := uint64(3)
	project.Mirrors[0].Sample = &model.SamplePolicy{Present: true, Modulus: modulus}

	for _, version := range []string{model.SchemaV2, model.SchemaV3} {
		data, err := loader.MarshalProject(project, version)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := loader.LoadProject(data, loader.LoadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Mirrors[0].Sample == nil || !loaded.Mirrors[0].Sample.Present ||
			loaded.Mirrors[0].Sample.Modulus != modulus {
			t.Fatalf("%s optional sample field was not normalized: %+v", version, loaded.Mirrors[0].Sample)
		}
	}
}

func TestSnapshotVersionReplacementDiscardsCache(t *testing.T) {
	project := sample.CanonicalProject()
	requests := sample.GeneratedRequests()
	cache, err := incremental.NewSnapshotCache(project, requests, acceptance.DemoConfig())
	if err != nil {
		t.Fatal(err)
	}
	replaced := sample.CanonicalProject()
	replaced.EngineeringVersion = "eng-replaced-snapshot"
	report, err := cache.Replace(replaced, requests, acceptance.DemoConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !report.VersionChanged || !report.CacheDiscarded || cache.DiscardedCaches != 1 || cache.Manager == nil {
		t.Fatalf("snapshot replacement did not explicitly discard cache: %+v cache=%+v", report, cache)
	}
}

func withWorkers(config engine.Config, workers int) engine.Config {
	config.WorkerBudget = workers
	return config
}
