package hckernel

import (
	"fmt"
	"math/rand"
)

const DefaultSeed int64 = 42

type Generator struct {
	Seed int64
	rng  *rand.Rand
}

func NewGenerator(seed int64) *Generator {
	return &Generator{Seed: seed, rng: rand.New(rand.NewSource(seed))}
}

// BuildProject returns the hand-arranged but seed-tagged deterministic fixture.
// The arrangement is intentionally dense enough to hit every scheduler branch.
func (g *Generator) BuildProject() *Project {
	variant := g.rng.Intn(1000)
	return &Project{
		ProjectID:         "health-seed-42",
		ProjectVersion:    fmt.Sprintf("engineering-%d.%03d", g.Seed, variant),
		Seed:              g.Seed,
		StepMs:            100,
		Rounds:            14,
		MaxDeferMs:        250,
		GlobalConcurrency: 3,
		Racks: []Rack{
			{ID: "rack-a", Concurrency: 2},
			{ID: "rack-b", Concurrency: 2},
			{ID: "rack-c", Concurrency: 1},
		},
		Workers:        4,
		BufferCapacity: 8,
		SlotPriority:   PriorityDeferredFirst,
		CountPolicy: CountPolicy{
			SuccessReachedAction: ThresholdReset,
			FailureReachedAction: ThresholdKeep,
			ResetOppositeStreak:  true,
			HalfOpenSuccessLimit: 1,
			HalfOpenFailureLimit: 1,
			HalfOpenDelayMs:      200,
		},
		Instances: []Instance{
			{ID: "be-01", NodeID: "node-a1", RackID: "rack-a", Protocol: "HTTP", Address: "10.42.0.11:8080", IntervalMs: 200, TimeoutMs: 100, BackoffMs: 100, RetryLimit: 1, SuccessThreshold: 2, FailureThreshold: 2, InitialState: StateHealthy, WeightPPM: 500000, ActiveProbe: true},
			{ID: "be-02", NodeID: "node-a2", RackID: "rack-a", Protocol: "HTTP", Address: "10.42.0.12:8080", IntervalMs: 400, TimeoutMs: 100, BackoffMs: 200, RetryLimit: 1, SuccessThreshold: 1, FailureThreshold: 2, InitialState: StateHealthy, WeightPPM: 300000, ActiveProbe: true},
			{ID: "be-03", NodeID: "node-b1", RackID: "rack-b", Protocol: "TCP", Address: "10.42.1.11:5432", IntervalMs: 200, TimeoutMs: 100, BackoffMs: 100, RetryLimit: 1, SuccessThreshold: 2, FailureThreshold: 3, InitialState: StateInitial, WeightPPM: 400000, ActiveProbe: true},
			{ID: "be-04", NodeID: "node-b2", RackID: "rack-b", Protocol: "GRPC", Address: "10.42.1.12:9090", IntervalMs: 300, TimeoutMs: 200, BackoffMs: 100, RetryLimit: 1, SuccessThreshold: 1, FailureThreshold: 2, InitialState: StateHealthy, WeightPPM: 250000, ActiveProbe: true},
			{ID: "be-05", NodeID: "node-c1", RackID: "rack-c", Protocol: "HTTP", Address: "10.42.2.11:8080", IntervalMs: 200, TimeoutMs: 400, BackoffMs: 100, RetryLimit: 1, SuccessThreshold: 2, FailureThreshold: 2, InitialState: StateUnhealthy, WeightPPM: 150000, ActiveProbe: true},
			{ID: "be-06", NodeID: "node-c2", RackID: "rack-c", Protocol: "TCP", Address: "10.42.2.12:6379", IntervalMs: 100, TimeoutMs: 100, BackoffMs: 100, RetryLimit: 0, SuccessThreshold: 3, FailureThreshold: 3, InitialState: StateInitial, WeightPPM: 100000, ActiveProbe: true},
		},
	}
}

func resultEvent(id string, at int64, instance string, result ProbeResultKind) Event {
	return Event{ID: id, AtMs: at, Type: EventProbeResult, InstanceID: instance, Result: result}
}

// BuildEvents returns scripted injections and topology changes aligned to the
// declared 100ms logical round step.
func (g *Generator) BuildEvents() []Event {
	return []Event{
		resultEvent("EVT-001", 100, "be-01", ResultSuccess),
		resultEvent("EVT-002", 100, "be-02", ResultSuccess),
		resultEvent("EVT-003", 100, "be-03", ResultSuccess),
		resultEvent("EVT-004", 200, "be-04", ResultSuccess),
		resultEvent("EVT-005", 200, "be-06", ResultSuccess),
		resultEvent("EVT-006", 300, "be-01", ResultSuccess),
		resultEvent("EVT-007", 300, "be-03", ResultSuccess),
		resultEvent("EVT-008", 500, "be-01", ResultFailure),
		resultEvent("EVT-009", 500, "be-03", ResultSuccess),
		resultEvent("EVT-010", 600, "be-02", ResultFailure),
		resultEvent("EVT-011", 700, "be-01", ResultFailure),
		resultEvent("EVT-012", 700, "be-06", ResultSuccess),
		resultEvent("EVT-013", 700, "be-04", ResultFailure),
		resultEvent("EVT-014", 900, "be-02", ResultFailure),
		resultEvent("EVT-015", 800, "be-03", ResultSuccess),
		resultEvent("EVT-016", 900, "be-04", ResultFailure),
		resultEvent("EVT-017", 1000, "be-01", ResultSuccess),
		resultEvent("EVT-018", 1000, "be-03", ResultSuccess),
		resultEvent("EVT-019", 1000, "be-05", ResultSuccess),
		resultEvent("EVT-020", 1100, "be-06", ResultSuccess),
		resultEvent("EVT-021", 1300, "be-02", ResultSuccess),
		resultEvent("EVT-022", 1200, "be-01", ResultSuccess),
		resultEvent("EVT-023", 1200, "be-03", ResultSuccess),
		resultEvent("EVT-024", 1300, "be-04", ResultSuccess),
		{
			ID: "EVT-026", AtMs: 1300, Type: EventInstanceAdd,
			Instance: &Instance{
				ID: "be-07", NodeID: "node-b3", RackID: "rack-b", Protocol: "HTTP",
				Address: "10.42.1.13:8080", IntervalMs: 200, TimeoutMs: 100, BackoffMs: 100,
				RetryLimit: 1, SuccessThreshold: 1, FailureThreshold: 2,
				InitialState: StateInitial, WeightPPM: 200000, ActiveProbe: true,
			},
		},
		{ID: "EVT-027", AtMs: 1300, Type: EventRackMove, InstanceID: "be-06", ToRackID: "rack-b"},
		{ID: "EVT-028", AtMs: 1300, Type: EventInstanceRemove, InstanceID: "be-02"},
	}
}

func (g *Generator) BuildEdits() []Event {
	return []Event{
		{ID: "EDT-001", AtMs: 300, Type: EventConfigEdit, InstanceID: "be-03", Edit: &Edit{Field: "success_threshold", NewValue: 1}},
		{ID: "EDT-002", AtMs: 200, Type: EventConfigEdit, InstanceID: "be-06", Edit: &Edit{Field: "interval_ms", NewValue: 250}},
		{ID: "EDT-003", AtMs: 1300, Type: EventConfigEdit, InstanceID: "be-04", Edit: &Edit{Field: "weight_ppm", NewValue: int64(900000)}},
		{ID: "EDT-004", AtMs: 1100, Type: EventConfigEdit, InstanceID: "be-06", Edit: &Edit{Field: "rack_id", NewValue: "rack-a"}},
	}
}
