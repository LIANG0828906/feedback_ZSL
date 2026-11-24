package deps_test

import (
	"testing"

	"audioforge/internal/deps"
	"audioforge/internal/engine"
	"audioforge/internal/generator"
	"audioforge/internal/model"
)

func TestPriorityEditInvalidatesOnlyChannelSuffix(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	params := model.BuildParameters{ParamVersion: "p", AlgorithmVersion: "a", CommitWindow: 2, Workers: 2, BufferPerWorker: 2}
	baseline, err := (&engine.Engine{}).Build(bundle.CanonicalEvents, bundle.Capability, params)
	if err != nil {
		t.Fatal(err)
	}
	edited := cloneEvents(bundle.CanonicalEvents)
	for i := range edited {
		if edited[i].ID == "evt-001" {
			edited[i].Priority = 10
		}
	}
	updated, err := (&engine.Engine{}).Build(edited, bundle.Capability, params)
	if err != nil {
		t.Fatal(err)
	}
	graph := deps.Build(baseline.Packets, bundle.Capability)
	invalidation := graph.InvalidatedBy(deps.Build(updated.Packets, bundle.Capability), deps.Change{
		Kind:    deps.ChangeEventPriority,
		EventID: "evt-001",
	})
	if invalidation.ReusedCount == 0 {
		t.Fatal("expected an unaffected channel prefix/cache to be reused")
	}
	if len(invalidation.Invalidated) == 0 {
		t.Fatal("expected invalidations")
	}
	firstChanged := map[int]int{}
	for _, item := range invalidation.Invalidated {
		if _, ok := firstChanged[item.Channel]; !ok {
			firstChanged[item.Channel] = item.PacketSeq
		}
	}
	if firstChanged[0] != 1 {
		t.Fatalf("channel 0 first invalidated packet = %d, want 1", firstChanged[0])
	}
	for _, item := range invalidation.Invalidated {
		if item.Channel != 0 && item.PacketSeq >= 0 {
			t.Fatalf("unexpected invalidation on unaffected channel: %+v", item)
		}
	}
	if _, ok := firstChanged[1]; ok {
		t.Fatal("channel 1 should not be invalidated")
	}
	if _, ok := firstChanged[2]; ok {
		t.Fatal("channel 2 should not be invalidated")
	}
}

func TestQuantizationChangeInvalidatesAllPackets(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	params := model.BuildParameters{ParamVersion: "p", AlgorithmVersion: "a", CommitWindow: 2, Workers: 1, BufferPerWorker: 1}
	baseline, _ := (&engine.Engine{}).Build(bundle.CanonicalEvents, bundle.Capability, params)
	capability := bundle.Capability
	capability.QuantUnit *= 2
	updated, err := (&engine.Engine{}).Build(bundle.CanonicalEvents, capability, params)
	if err != nil {
		t.Fatal(err)
	}
	invalidation := deps.Build(baseline.Packets, bundle.Capability).InvalidatedBy(
		deps.Build(updated.Packets, capability),
		deps.Change{Kind: deps.ChangeQuantUnit, Old: bundle.Capability.QuantUnit, New: capability.QuantUnit},
	)
	if invalidation.ReusedCount != 0 {
		t.Fatalf("quantization change reused %d packets", invalidation.ReusedCount)
	}
	if len(invalidation.Invalidated) != len(updated.Packets) {
		t.Fatalf("invalidated %d of %d packets", len(invalidation.Invalidated), len(updated.Packets))
	}
}

func TestRemovedEventInvalidatesOnlyItsChannelSuffix(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	params := model.BuildParameters{ParamVersion: "p", AlgorithmVersion: "a", CommitWindow: 2, Workers: 1, BufferPerWorker: 1}
	baseline, _ := (&engine.Engine{}).Build(bundle.CanonicalEvents, bundle.Capability, params)
	edited := make([]model.Event, 0, len(bundle.CanonicalEvents)-1)
	for _, event := range bundle.CanonicalEvents {
		if event.ID != "evt-014" {
			edited = append(edited, event)
		}
	}
	updated, err := (&engine.Engine{}).Build(edited, bundle.Capability, params)
	if err != nil {
		t.Fatal(err)
	}
	invalidation := deps.Build(baseline.Packets, bundle.Capability).InvalidatedBy(
		deps.Build(updated.Packets, bundle.Capability),
		deps.Change{Kind: deps.ChangeEventRemoved, EventID: "evt-014"},
	)
	if invalidation.ReusedCount == 0 {
		t.Fatal("expected earlier channel packets to be reused")
	}
	for _, item := range invalidation.Invalidated {
		if item.Channel != 1 {
			t.Fatalf("removed channel 1 event invalidated channel %d packet %d", item.Channel, item.PacketSeq)
		}
	}
}

func TestPacketSizeChangeInvalidatesAllBoundaries(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	params := model.BuildParameters{ParamVersion: "p", AlgorithmVersion: "a", CommitWindow: 2, Workers: 1, BufferPerWorker: 1}
	baseline, _ := (&engine.Engine{}).Build(bundle.CanonicalEvents, bundle.Capability, params)
	capability := bundle.Capability
	capability.MaxPacketSamples = 64
	updated, err := (&engine.Engine{}).Build(bundle.CanonicalEvents, capability, params)
	if err != nil {
		t.Fatal(err)
	}
	invalidation := deps.Build(baseline.Packets, bundle.Capability).InvalidatedBy(
		deps.Build(updated.Packets, capability),
		deps.Change{Kind: deps.ChangeMaxPacketSamples, Old: bundle.Capability.MaxPacketSamples, New: capability.MaxPacketSamples},
	)
	if invalidation.ReusedCount != 0 {
		t.Fatalf("packet-size change reused %d packets", invalidation.ReusedCount)
	}
	if len(invalidation.Invalidated) != len(updated.Packets) {
		t.Fatalf("invalidated %d of %d packets", len(invalidation.Invalidated), len(updated.Packets))
	}
}

func TestChannelTemplateChangeInvalidatesThatChannel(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	params := model.BuildParameters{ParamVersion: "p", AlgorithmVersion: "a", CommitWindow: 2, Workers: 1, BufferPerWorker: 1}
	baseline, _ := (&engine.Engine{}).Build(bundle.CanonicalEvents, bundle.Capability, params)
	capability := bundle.Capability
	capability.Channels[0].Template[2] += 100
	updated, err := (&engine.Engine{}).Build(bundle.CanonicalEvents, capability, params)
	if err != nil {
		t.Fatal(err)
	}
	invalidation := deps.Build(baseline.Packets, bundle.Capability).InvalidatedBy(
		deps.Build(updated.Packets, capability),
		deps.Change{Kind: deps.ChangeChannelTemplate, Old: "v1", New: "v2"},
	)
	if invalidation.ReusedCount == 0 {
		t.Fatal("expected unaffected channels to reuse packets")
	}
	for _, item := range invalidation.Invalidated {
		if item.Channel != 0 {
			t.Fatalf("channel 0 template edit invalidated channel %d", item.Channel)
		}
	}
}

func cloneEvents(events []model.Event) []model.Event {
	out := make([]model.Event, len(events))
	for i := range events {
		out[i] = events[i].Clone()
	}
	return out
}
