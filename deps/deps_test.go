package deps_test

import (
	"testing"

	"lipsync/arbitration"
	"lipsync/deps"
	"lipsync/scenario"
)

func TestDependencyKeysAndPreciseBudgetInvalidation(t *testing.T) {
	project := scenario.Canonical(42)
	result, err := arbitration.Run(project)
	if err != nil {
		t.Fatal(err)
	}
	graph := deps.Build(project, *result)
	if graph.ContentHash != project.ContentHash || graph.ParamsVersion == "" {
		t.Fatal("graph root metadata missing")
	}
	packetNodeID := "packet:pkt-d1-main-low"
	packetNode, ok := graph.Nodes[packetNodeID]
	if !ok {
		t.Fatal("packet node missing")
	}
	if len(packetNode.CacheKey) != 64 {
		t.Fatalf("unexpected packet cache key %q", packetNode.CacheKey)
	}
	frameNode := graph.Nodes["frame:spk_main:2"]
	if frameNode.CacheKey == "" || len(frameNode.Upstream) == 0 {
		t.Fatal("frame must expose cache key and upstream edges")
	}
	hasPacketUpstream := false
	for _, upstream := range frameNode.Upstream {
		if upstream == packetNodeID {
			hasPacketUpstream = true
		}
	}
	if !hasPacketUpstream {
		t.Fatalf("frame node lacks packet dependency: %v", frameNode.Upstream)
	}

	store := deps.NewMemoryStore()
	updatedProject, updatedResult, updatedGraph, report, err := deps.ApplyEdit(project, deps.Change{
		Kind: deps.ChangeBudget, ChannelID: "spk_main", Budget: 7000,
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	channel, _ := updatedProject.ChannelByID("spk_main")
	if channel.FrameBudgetBasisPoints != 7000 {
		t.Fatal("budget edit not applied")
	}
	if report.ReusedFrames == 0 || report.RecomputedFrames == 0 {
		t.Fatalf("expected partial invalidation, got reused=%d recomputed=%d", report.ReusedFrames, report.RecomputedFrames)
	}
	if report.StoreHits == 0 {
		t.Fatal("unchanged frames were not revalidated from the store")
	}
	if len(report.InvalidatedPackets) == 0 {
		t.Fatal("invalidated packet list is empty")
	}
	if len(report.Intervals) == 0 {
		t.Fatal("invalidated frame intervals are empty")
	}
	for _, interval := range report.Intervals {
		if interval.ChannelID != "spk_main" {
			t.Fatalf("budget edit invalidated an unaffected channel frame: %+v", interval)
		}
		if interval.Start > interval.End {
			t.Fatalf("invalid interval: %+v", interval)
		}
	}
	if updatedGraph.ParamsVersion == graph.ParamsVersion {
		t.Fatal("parameter version did not change with budget edit")
	}
	if len(updatedResult.Frames) == 0 {
		t.Fatal("updated arbitration result is empty")
	}
}

func TestNoopEditReusesEveryFrame(t *testing.T) {
	project := scenario.Canonical(9)
	store := deps.NewMemoryStore()
	_, _, _, report, err := deps.ApplyEdit(project, deps.Change{
		Kind: deps.ChangeBudget, ChannelID: "spk_main", Budget: 6000,
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	if report.RecomputedFrames != 0 || len(report.Invalidated) != 0 {
		t.Fatalf("noop edit invalidated frames: %+v", report)
	}
	if report.ReusedFrames == 0 || report.StoreHits != report.ReusedFrames {
		t.Fatalf("noop edit did not reuse persisted frames: %+v", report)
	}
}

func TestOrderAndRecoveryInvalidation(t *testing.T) {
	project := scenario.Canonical(42)
	_, _, _, orderReport, err := deps.ApplyEdit(project, deps.Change{
		Kind: deps.ChangeOrder, Order: []string{"spk_alt", "spk_main", "spk_wide"},
	}, deps.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	if len(orderReport.Invalidated) == 0 {
		t.Fatal("order edit should invalidate aggregate frame order")
	}
	channels := map[string]bool{}
	for _, interval := range orderReport.Intervals {
		channels[interval.ChannelID] = true
	}
	if !channels["spk_main"] || !channels["spk_alt"] || !channels["spk_wide"] {
		t.Fatalf("order invalidation did not cover all channels: %+v", orderReport.Intervals)
	}

	recovery := project.Recovery
	recovery.Strategy = "next_window"
	_, _, _, recoveryReport, err := deps.ApplyEdit(project, deps.Change{
		Kind: deps.ChangeRecovery, Recovery: &recovery,
	}, deps.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveryReport.InvalidatedPackets) == 0 {
		t.Fatal("recovery edit did not identify downstream packets")
	}
	if len(recoveryReport.Invalidated) == 0 {
		t.Fatal("recovery edit did not identify affected frames")
	}
}
