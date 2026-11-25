package engine_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/liang/collector-backpressure/bpderr"
	"github.com/liang/collector-backpressure/engine"
	"github.com/liang/collector-backpressure/model"
	"github.com/liang/collector-backpressure/sample"
	"github.com/liang/collector-backpressure/schema"
	"github.com/liang/collector-backpressure/testsupport"
)

func runSample(t *testing.T, workers int) *engine.Simulation {
	t.Helper()
	budget := engine.DefaultBudget()
	budget.Workers = workers
	runner := newRunner(t, budget)
	sim, err := runner.Run(context.Background(), sample.GenerateProject(sample.DefaultSeed))
	if err != nil {
		t.Fatal(err)
	}
	return sim
}

func newRunner(t *testing.T, budget engine.Budget) *engine.Runner {
	t.Helper()
	runner, err := engine.NewRunner(budget)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func hasCode(err error, code bpderr.Code) bool {
	biz, ok := bpderr.As(err)
	return ok && biz.Code == code
}

func findPartition(t *testing.T, tick model.TickReport, id string) model.PartitionReport {
	t.Helper()
	for _, p := range tick.Partitions {
		if p.PartitionID == id {
			return p
		}
	}
	t.Fatalf("partition %s not found", id)
	return model.PartitionReport{}
}

func TestSampleBackpressureInvariantsAndEvents(t *testing.T) {
	sim := runSample(t, 4)
	if len(sim.Result.Ticks) != 14 {
		t.Fatalf("expected 14 tick reports, got %d", len(sim.Result.Ticks))
	}

	var sawBufferPending, sawSlow, sawQueueCode, sawPartitionDrop bool
	for _, tick := range sim.Result.Ticks {
		sentBatches := map[string]bool{}
		sentChunks := map[string]bool{}
		for _, agent := range tick.Agents {
			if agent.InflightBytesEnd > agent.BufferMaxBytes {
				t.Fatalf("tick %d agent %s inflight %d exceeds buffer %d", tick.Tick, agent.AgentID, agent.InflightBytesEnd, agent.BufferMaxBytes)
			}
			for _, id := range agent.SentChunks {
				sentChunks[id] = true
				chunk := sim.Result.ChunkByID[id]
				sentBatches[chunk.BatchID] = true
			}
			for _, item := range agent.Pending {
				if item.Position < 1 {
					t.Fatalf("bad pending position %+v", item)
				}
				if item.ViolatedRule == engine.RuleBufferBytes {
					sawBufferPending = true
				}
			}
			if agent.ElapsedNanoseconds < 0 {
				t.Fatal("negative elapsed observation")
			}
		}
		for _, drop := range tick.Drops {
			if sentBatches[drop.BatchID] {
				t.Fatalf("tick %d batch %s both sent and dropped", tick.Tick, drop.BatchID)
			}
			for _, id := range drop.ChunkIDs {
				if sentChunks[id] {
					t.Fatalf("tick %d chunk %s both sent and dropped", tick.Tick, id)
				}
			}
			if drop.RuleID == engine.RuleQueueLimit {
				sawQueueCode = true
			}
			if drop.RuleID == engine.RulePartitionPressure || drop.RuleID == engine.RuleWatermarkLag {
				sawPartitionDrop = true
			}
			if drop.PartitionID == "" || drop.TriggerPartition == "" || drop.Code == "" {
				t.Fatalf("drop record missing locator fields: %+v", drop)
			}
		}
		for _, signal := range tick.Signals {
			if signal.RuleID == engine.RuleBufferExhausted && signal.Action == model.ActionSlow {
				sawSlow = true
			}
		}
	}
	if !sawBufferPending {
		t.Fatal("expected at least one buffer-constrained pending item")
	}
	if !sawSlow {
		t.Fatal("expected buffer exhaustion duration to produce a slowdown signal")
	}
	if !sawQueueCode {
		t.Fatal("expected queue budget exhaustion to produce observable E_AGENT_QUEUE_FULL")
	}
	if !sawPartitionDrop {
		t.Fatal("expected partition watermark pressure to produce a drop")
	}

	tick0 := sim.Result.Ticks[0]
	p1 := findPartition(t, tick0, "p1")
	if p1.WrittenBytes != 160 || p1.AckedBytes != 0 || p1.BacklogBytes != 160 {
		t.Fatalf("unexpected tick0 p1 truth: %+v", p1)
	}
	tick2 := sim.Result.Ticks[2]
	p1at2 := findPartition(t, tick2, "p1")
	if p1at2.AckedBytes != 60 {
		t.Fatalf("expected rate-limited ack 60 at tick2, got %d", p1at2.AckedBytes)
	}
}

func TestConcurrencyAndSchedulingDoNotChangeSemanticResult(t *testing.T) {
	one := runSample(t, 1)
	four := runSample(t, 4)
	eight := runSample(t, 8)
	if !testsupport.EqualSemanticTicks(one.Result.Ticks, four.Result.Ticks) {
		t.Fatal("worker count changed tick results")
	}
	if !testsupport.EqualSemanticTicks(four.Result.Ticks, eight.Result.Ticks) {
		t.Fatal("worker count 8 changed tick results")
	}
	for i := 0; i < 5; i++ {
		again := runSample(t, 4)
		if !testsupport.EqualSemanticTicks(four.Result.Ticks, again.Result.Ticks) {
			t.Fatalf("repeat run %d changed result", i)
		}
	}

	shuffled := sample.GenerateProject(sample.DefaultSeed)
	for i, j := 0, len(shuffled.Agents)-1; i < j; i, j = i+1, j-1 {
		shuffled.Agents[i], shuffled.Agents[j] = shuffled.Agents[j], shuffled.Agents[i]
	}
	shuffledRunner := newRunner(t, engine.DefaultBudget())
	shuffledSim, err := shuffledRunner.Run(context.Background(), shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if !testsupport.EqualSemanticTicks(four.Result.Ticks, shuffledSim.Result.Ticks) {
		t.Fatal("agent declaration/scheduling order changed result")
	}
}

func TestStableFaultCodes(t *testing.T) {
	ctx := context.Background()

	zeroBuffer := sample.GenerateProject(sample.DefaultSeed)
	zeroBuffer.Agents[0].BufferMaxBytes = 0
	if _, err := newRunner(t, engine.DefaultBudget()).Run(ctx, zeroBuffer); !hasCode(err, bpderr.EBufferLimitZero) {
		t.Fatalf("expected E_BUFFER_LIMIT_ZERO, got %v", err)
	}

	cycle := sample.GenerateProject(sample.DefaultSeed)
	for i := range cycle.Rules {
		if cycle.Rules[i].ID == "r-acme-all" {
			cycle.Rules[i].ParentID = "r-acme-click"
		}
	}
	if _, err := newRunner(t, engine.DefaultBudget()).Run(ctx, cycle); !hasCode(err, bpderr.EKeyRuleCycle) {
		t.Fatalf("expected E_KEY_RULE_CYCLE, got %v", err)
	}

	ahead := sample.GenerateProject(sample.DefaultSeed)
	ahead.WatermarkUpdates = append(ahead.WatermarkUpdates, model.WatermarkUpdate{
		PartitionID: "p1", Tick: 0, Watermark: 9_999_999,
	})
	if _, err := newRunner(t, engine.DefaultBudget()).Run(ctx, ahead); !hasCode(err, bpderr.EWatermarkAheadAck) {
		t.Fatalf("expected E_WATERMARK_AHEAD_ACK, got %v", err)
	}

	stale := sample.GenerateProject(sample.DefaultSeed)
	for i := range stale.Partitions {
		if stale.Partitions[i].ID == "p3" {
			stale.Partitions[i].StrictStaleWatermark = true
			stale.Partitions[i].ConsumerWatermark = nil
		}
	}
	stale.WatermarkUpdates = append(stale.WatermarkUpdates, model.WatermarkUpdate{
		PartitionID: "p3", Tick: 6, Watermark: 1,
	})
	if _, err := newRunner(t, engine.DefaultBudget()).Run(ctx, stale); !hasCode(err, bpderr.EWatermarkBeforeAck) {
		t.Fatalf("expected E_WATERMARK_BEFORE_ACK, got %v", err)
	}

	if _, err := engine.NewRunner(engine.Budget{Workers: 0}); !hasCode(err, bpderr.EBudgetOutOfRange) {
		t.Fatalf("expected worker budget E_BUDGET_OUT_OF_RANGE, got %v", err)
	}
	if _, err := engine.NewRunner(engine.Budget{Workers: 2, PerAgentQueue: 0, SchedulerQueue: 8}); !hasCode(err, bpderr.EBudgetOutOfRange) {
		t.Fatalf("expected queue budget E_BUDGET_OUT_OF_RANGE, got %v", err)
	}
	if _, err := engine.NewRunner(engine.Budget{Workers: 2, PerAgentQueue: 8, SchedulerQueue: 0}); !hasCode(err, bpderr.EBudgetOutOfRange) {
		t.Fatalf("expected scheduler budget E_BUDGET_OUT_OF_RANGE, got %v", err)
	}

	queueFull := sample.GenerateProject(sample.DefaultSeed)
	for i := range queueFull.Agents {
		if queueFull.Agents[i].ID == "a1" {
			queueFull.Agents[i].PendingQueueLimit = 1
			queueFull.Agents[i].OverflowPolicy = model.OverflowError
		}
	}
	if _, err := newRunner(t, engine.DefaultBudget()).Run(ctx, queueFull); !hasCode(err, bpderr.EAgentQueueFull) {
		t.Fatalf("expected E_AGENT_QUEUE_FULL, got %v", err)
	}

	scheduler, err := engine.NewScheduler(engine.Budget{Workers: 1, PerAgentQueue: 1, SchedulerQueue: 1})
	if err != nil {
		t.Fatal(err)
	}
	firstDone, err := scheduler.TrySubmit("first")
	if err != nil {
		t.Fatal(err)
	}
	defer firstDone()
	if _, err := scheduler.TrySubmit("second"); !hasCode(err, bpderr.EWorkerBudgetExhausted) {
		t.Fatalf("expected E_WORKER_BUDGET_EXHAUSTED, got %v", err)
	}
}

func TestMissingWatermarkIsDerivedAndSourceProjectUnmodified(t *testing.T) {
	original := sample.GenerateProject(sample.DefaultSeed)
	before, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}

	v1Data, err := schema.RenderJSON(schema.V1, original)
	if err != nil {
		t.Fatal(err)
	}
	v1Project, err := schema.LoadJSON(v1Data)
	if err != nil {
		t.Fatal(err)
	}
	v1Sim, err := newRunner(t, engine.DefaultBudget()).Run(context.Background(), v1Project)
	if err != nil {
		t.Fatal(err)
	}
	if got := findPartition(t, v1Sim.Result.Ticks[0], "p3").WatermarkSource; got != model.WatermarkDerived {
		t.Fatalf("v1 missing consumer watermark source = %s, want %s", got, model.WatermarkDerived)
	}

	v3Data, err := schema.RenderJSON(schema.V3, original)
	if err != nil {
		t.Fatal(err)
	}
	v3Project, err := schema.LoadJSON(v3Data)
	if err != nil {
		t.Fatal(err)
	}
	v3Sim, err := newRunner(t, engine.DefaultBudget()).Run(context.Background(), v3Project)
	if err != nil {
		t.Fatal(err)
	}
	if got := findPartition(t, v3Sim.Result.Ticks[0], "p3").WatermarkSource; got != model.WatermarkDeclared {
		t.Fatalf("v3 declared watermark source = %s, want %s", got, model.WatermarkDeclared)
	}

	after, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("runner modified the source project")
	}
}
