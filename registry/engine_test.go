package registry_test

import (
	"encoding/json"
	"testing"

	"serviceregistry/registry"
)

func boolPointer(value bool) *bool { return &value }

func testProject(instanceCount int) *registry.Project {
	project := &registry.Project{
		SchemaVersion:  registry.SchemaV1,
		ProjectVersion: "engine-test",
		TickMS:         100,
		Budgets: registry.Budgets{
			GlobalConcurrency: 20,
			NodeConcurrency:   20,
			RackConcurrency:   20,
			InstanceBuffer:    4,
			MaxDelayMS:        1000,
		},
		Services: []registry.Service{{
			ID:                     "svc",
			Name:                   "service",
			DefaultWeightScale:     registry.ScalePerMille,
			HalfOpenPolicy:         registry.HalfOpenExclude,
			HalfOpenFactorPermille: 500,
			AdmitInitial:           true,
		}},
	}
	for index := 0; index < instanceCount; index++ {
		project.Instances = append(project.Instances, registry.Instance{
			ID:               string(rune('a' + index)),
			ServiceID:        "svc",
			NodeID:           "node",
			RackID:           "rack",
			Address:          "127.0.0.1:" + string(rune('1'+index)),
			Weight:           1000,
			WeightScale:      registry.ScalePerMille,
			InitialHealth:    registry.HealthHealthy,
			LeaseTTLMS:       10_000,
			HeartbeatMS:      1000,
			ProbePeriodMS:    100,
			ProbeTimeoutMS:   1000,
			SuccessThreshold: 1,
			FailureThreshold: 1,
			Order:            index,
		})
	}
	return project
}

func register(tick int64, instanceID string) registry.Event {
	return registry.Event{AtMS: tick * 100, Kind: registry.EventRegister, InstanceID: instanceID, ServiceID: "svc"}
}

func heartbeat(tick int64, instanceID string) registry.Event {
	return registry.Event{AtMS: tick * 100, Kind: registry.EventHeartbeat, InstanceID: instanceID, ServiceID: "svc"}
}

func probeResult(tick int64, instanceID string, success bool) registry.Event {
	return registry.Event{AtMS: tick * 100, Kind: registry.EventProbeResult, InstanceID: instanceID, ServiceID: "svc", Success: boolPointer(success)}
}

func TestLeaseHeartbeatAndExpiry(t *testing.T) {
	project := testProject(1)
	project.Instances[0].LeaseTTLMS = 500
	project.Instances[0].FailureThreshold = 3
	project.Events = []registry.Event{
		register(0, "a"),
		probeResult(1, "a", false),
		heartbeat(2, "a"),
		probeResult(3, "a", false),
	}
	until := int64(7)
	run, err := registry.RunProject(project, registry.RunOptions{UntilTick: &until})
	if err != nil {
		t.Fatal(err)
	}
	step2 := run.Steps[2]
	if step2.Instances[0].LeaseExpiresAtMS != 700 {
		t.Fatalf("renewed expiry = %d, want 700", step2.Instances[0].LeaseExpiresAtMS)
	}
	if step2.Instances[0].RenewalCount != 1 {
		t.Fatalf("renewal count = %d, want 1", step2.Instances[0].RenewalCount)
	}
	step3 := run.Steps[3]
	if step3.Instances[0].ConsecutiveFail != 2 {
		t.Fatalf("heartbeat reset failure count to %d", step3.Instances[0].ConsecutiveFail)
	}
	step6 := run.Steps[6]
	if step6.Instances[0].Phase != registry.PhaseRegistered || step6.Instances[0].LeaseRemainingMS != 100 {
		t.Fatalf("lease remaining at tick 6 = %+v", step6.Instances[0])
	}
	step7 := run.Steps[7]
	if step7.Instances[0].Phase != registry.PhaseRemoval || step7.Instances[0].RemovalReason != registry.CodeExcludeLeaseExpired {
		t.Fatalf("expired state = %+v", step7.Instances[0])
	}
	if step7.Instances[0].LeaseRemainingMS != 0 {
		t.Fatalf("expired lease remaining = %d", step7.Instances[0].LeaseRemainingMS)
	}
}

func TestHeartbeatAtExactExpiryIsRejected(t *testing.T) {
	project := testProject(1)
	project.Instances[0].LeaseTTLMS = 500
	project.Events = []registry.Event{
		register(0, "a"),
		heartbeat(5, "a"),
	}
	run, err := registry.RunProject(project, registry.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	step := run.Steps[5]
	if step.Instances[0].Phase != registry.PhaseRemoval {
		t.Fatalf("phase = %s, want removal", step.Instances[0].Phase)
	}
	if step.Instances[0].RenewalCount != 0 {
		t.Fatalf("renewal count = %d", step.Instances[0].RenewalCount)
	}
	if step.Events[0].Accepted {
		t.Fatal("heartbeat at expiry was accepted")
	}
}

func TestProbeSuppressionAndTimeout(t *testing.T) {
	project := testProject(1)
	project.Instances[0].ProbeTimeoutMS = 250
	project.Events = []registry.Event{register(0, "a")}
	until := int64(3)
	run, err := registry.RunProject(project, registry.RunOptions{UntilTick: &until})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Steps[0].DueProbes) != 1 {
		t.Fatalf("tick0 due probes = %d", len(run.Steps[0].DueProbes))
	}
	if len(run.Steps[1].SuppressedProbes) != 1 || run.Steps[1].SuppressedProbes[0].Reason != registry.CodeProbeInFlight {
		t.Fatalf("tick1 suppression = %+v", run.Steps[1].SuppressedProbes)
	}
	if len(run.Steps[2].SuppressedProbes) != 1 {
		t.Fatalf("tick2 suppression = %+v", run.Steps[2].SuppressedProbes)
	}
	if len(run.Steps[3].TimedOutProbes) != 1 {
		t.Fatalf("tick3 timeouts = %+v", run.Steps[3].TimedOutProbes)
	}
	if run.Steps[3].Instances[0].Health != registry.HealthUnhealthy {
		t.Fatalf("health after timeout = %s", run.Steps[3].Instances[0].Health)
	}
	if len(run.Steps[3].DueProbes) != 1 {
		t.Fatalf("new probe was not dispatched after timeout: %+v", run.Steps[3].DueProbes)
	}
}

func TestFourHealthStates(t *testing.T) {
	project := testProject(1)
	project.Events = []registry.Event{
		register(0, "a"),
		probeResult(1, "a", false),
		probeResult(2, "a", true),
		probeResult(3, "a", true),
	}
	run, err := registry.RunProject(project, registry.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []registry.HealthState{
		registry.HealthHealthy,
		registry.HealthUnhealthy,
		registry.HealthHalfOpen,
		registry.HealthHealthy,
	}
	for tick, health := range want {
		if got := run.Steps[tick].Instances[0].Health; got != health {
			t.Fatalf("tick %d health = %s, want %s", tick, got, health)
		}
	}
}

func TestConcurrencyDeferralAndMaxDelayDrop(t *testing.T) {
	project := testProject(3)
	project.Budgets.GlobalConcurrency = 1
	project.Budgets.NodeConcurrency = 1
	project.Budgets.RackConcurrency = 1
	project.Budgets.MaxDelayMS = 0
	project.Events = []registry.Event{
		register(0, "a"),
		register(0, "b"),
		register(0, "c"),
	}
	until := int64(1)
	run, err := registry.RunProject(project, registry.RunOptions{UntilTick: &until})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Steps[0].DeferredProbes) != 2 {
		t.Fatalf("deferred = %+v", run.Steps[0].DeferredProbes)
	}
	if len(run.Steps[0].DeferredProbes[0].Violations) == 0 {
		t.Fatal("deferred probe has no violated constraint")
	}
	if len(run.Steps[1].DroppedProbes) != 2 {
		t.Fatalf("dropped at tick1 = %+v", run.Steps[1].DroppedProbes)
	}
	for _, dropped := range run.Steps[1].DroppedProbes {
		if dropped.Reason != registry.CodeProbeMaxDelay {
			t.Fatalf("drop reason = %s", dropped.Reason)
		}
	}
}

func TestPerInstanceBufferDrop(t *testing.T) {
	project := testProject(2)
	project.Budgets.GlobalConcurrency = 1
	project.Budgets.InstanceBuffer = 1
	project.Budgets.MaxDelayMS = 5000
	project.Instances[1].ProbePeriodMS = 100
	project.Events = []registry.Event{
		register(0, "a"),
		register(0, "b"),
	}
	until := int64(1)
	run, err := registry.RunProject(project, registry.RunOptions{UntilTick: &until})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, dropped := range run.Steps[1].DroppedProbes {
		if dropped.InstanceID == "b" && dropped.Reason == registry.CodeProbeBufferFull {
			found = true
			if len(dropped.Violations) == 0 || dropped.Violations[0] != registry.ConstraintInstance {
				t.Fatalf("buffer drop violations = %v", dropped.Violations)
			}
		}
	}
	if !found {
		t.Fatalf("no buffer drop: %+v", run.Steps[1].DroppedProbes)
	}
}

func TestNodeAndRackBudgetConstraints(t *testing.T) {
	tests := []struct {
		name       string
		nodeLimit  int
		rackLimit  int
		constraint string
	}{
		{name: "node", nodeLimit: 1, rackLimit: 2, constraint: registry.ConstraintNode},
		{name: "rack", nodeLimit: 2, rackLimit: 1, constraint: registry.ConstraintRack},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			project := testProject(2)
			project.Budgets.GlobalConcurrency = 4
			project.Budgets.NodeConcurrency = test.nodeLimit
			project.Budgets.RackConcurrency = test.rackLimit
			project.Budgets.MaxDelayMS = 5000
			project.Events = []registry.Event{
				register(0, "a"),
				register(0, "b"),
			}
			run, err := registry.RunProject(project, registry.RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(run.Steps[0].DeferredProbes) != 1 {
				t.Fatalf("deferred = %+v", run.Steps[0].DeferredProbes)
			}
			violations := run.Steps[0].DeferredProbes[0].Violations
			found := false
			for _, violation := range violations {
				if violation == test.constraint {
					found = true
				}
			}
			if !found {
				t.Fatalf("violations = %v, want %s", violations, test.constraint)
			}
		})
	}
}

func TestCatalogScaleEquivalence(t *testing.T) {
	project := testProject(2)
	project.Instances[0].Weight = 500
	project.Instances[1].Weight = 900
	project.Events = []registry.Event{
		register(0, "a"),
		register(0, "b"),
		{
			AtMS:        0,
			Kind:        registry.EventQuery,
			ServiceID:   "svc",
			WeightScale: registry.ScalePerMille,
			QueryID:     "mile",
		},
		{
			AtMS:        0,
			Kind:        registry.EventQuery,
			ServiceID:   "svc",
			WeightScale: registry.ScalePer10K,
			QueryID:     "ten-k",
		},
	}
	run, err := registry.RunProject(project, registry.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	queries := run.Steps[0].Queries
	if len(queries) != 2 {
		t.Fatalf("queries = %d", len(queries))
	}
	mileJSON, _ := json.Marshal(queries[0].Catalog.Candidates)
	tenKJSON, _ := json.Marshal(queries[1].Catalog.Candidates)
	if string(mileJSON) == string(tenKJSON) {
		t.Fatal("display scales did not change displayed weights")
	}
	if queries[0].Catalog.Candidates[0].InstanceID != queries[1].Catalog.Candidates[0].InstanceID ||
		queries[0].Catalog.Candidates[1].InstanceID != queries[1].Catalog.Candidates[1].InstanceID {
		t.Fatal("candidate order depends on display scale")
	}
	if queries[0].Catalog.Candidates[0].DisplayWeight != 900 || queries[1].Catalog.Candidates[0].DisplayWeight != 9000 {
		t.Fatalf("display weights = %d/%d", queries[0].Catalog.Candidates[0].DisplayWeight, queries[1].Catalog.Candidates[0].DisplayWeight)
	}
}

func TestCatalogHealthExclusionAndHalfOpenDemotion(t *testing.T) {
	project := testProject(1)
	project.Services[0].HalfOpenPolicy = registry.HalfOpenDemote
	project.Services[0].HalfOpenFactorPermille = 500
	project.Instances[0].Weight = 1000
	project.Events = []registry.Event{
		register(0, "a"),
		probeResult(1, "a", false),
		probeResult(2, "a", true),
	}
	until := int64(2)
	run, err := registry.RunProject(project, registry.RunOptions{UntilTick: &until})
	if err != nil {
		t.Fatal(err)
	}
	if run.Steps[1].Instances[0].Health != registry.HealthUnhealthy {
		t.Fatalf("tick1 health = %s", run.Steps[1].Instances[0].Health)
	}
	unhealthyCatalog := run.Steps[1].Catalogs[0]
	if len(unhealthyCatalog.Candidates) != 0 || len(unhealthyCatalog.Excluded) != 1 ||
		unhealthyCatalog.Excluded[0].Reason != registry.CodeExcludeUnhealthy {
		t.Fatalf("unhealthy catalog = %+v", unhealthyCatalog)
	}
	if run.Steps[2].Instances[0].Health != registry.HealthHalfOpen {
		t.Fatalf("tick2 health = %s", run.Steps[2].Instances[0].Health)
	}
	halfOpenCatalog := run.Steps[2].Catalogs[0]
	if len(halfOpenCatalog.Candidates) != 1 {
		t.Fatalf("half-open candidates = %+v", halfOpenCatalog)
	}
	candidate := halfOpenCatalog.Candidates[0]
	if candidate.Tier != 2 || candidate.EffectiveWeightPPM != 500_000 ||
		len(candidate.Basis) != 1 || candidate.Basis[0] != registry.CodeHalfOpenDemoted {
		t.Fatalf("half-open candidate = %+v", candidate)
	}
	project.Services[0].HalfOpenPolicy = registry.HalfOpenExclude
	run, err = registry.RunProject(project, registry.RunOptions{UntilTick: &until})
	if err != nil {
		t.Fatal(err)
	}
	excludedCatalog := run.Steps[2].Catalogs[0]
	if len(excludedCatalog.Candidates) != 0 || len(excludedCatalog.Excluded) != 1 ||
		excludedCatalog.Excluded[0].Reason != registry.CodeExcludeHalfOpen {
		t.Fatalf("half-open exclude catalog = %+v", excludedCatalog)
	}
}
