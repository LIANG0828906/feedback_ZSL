package graph_test

import (
	"slices"
	"testing"

	"lipsync/domain"
	"lipsync/graph"
)

func fixture() (*domain.Project, *domain.ScheduleResult) {
	p := &domain.Project{
		Render: domain.RenderConfig{MaxDelayFrames: 0, ChannelSlots: 1, FrameBudget: 2},
		Dialogues: []domain.Dialogue{
			{ID: "d01", ShardIndex: 0, Channel: 0, Phonemes: []domain.Phoneme{
				{ID: "a", StartFrame: 0, EndFrame: 5},
				{ID: "b", StartFrame: 20, EndFrame: 25},
			}},
			{ID: "d02", ShardIndex: 1, Channel: 1, Phonemes: []domain.Phoneme{
				{ID: "c", StartFrame: 0, EndFrame: 5},
				{ID: "d", StartFrame: 50, EndFrame: 55},
			}},
		},
	}
	pkgs := []domain.ScheduledPackage{
		{Package: domain.Package{ID: "p-a", DialogueID: "d01", ShardIndex: 0, Channel: 0, StartFrame: 0, EndFrame: 5, PhonemeIDs: []string{"a"}}, Status: "accepted", DispatchStartFrame: 0, DispatchEndFrame: 5},
		{Package: domain.Package{ID: "p-b", DialogueID: "d01", ShardIndex: 0, Channel: 0, StartFrame: 20, EndFrame: 25, PhonemeIDs: []string{"b"}}, Status: "accepted", DispatchStartFrame: 20, DispatchEndFrame: 25},
		{Package: domain.Package{ID: "p-c", DialogueID: "d02", ShardIndex: 1, Channel: 1, StartFrame: 0, EndFrame: 5, PhonemeIDs: []string{"c"}}, Status: "accepted", DispatchStartFrame: 0, DispatchEndFrame: 5},
		{Package: domain.Package{ID: "p-d", DialogueID: "d02", ShardIndex: 1, Channel: 1, StartFrame: 50, EndFrame: 55, PhonemeIDs: []string{"d"}}, Status: "accepted", DispatchStartFrame: 50, DispatchEndFrame: 55},
	}
	return p, &domain.ScheduleResult{Packages: pkgs}
}

func TestWeightEditInvalidatesOnlyOverlappingDownstream(t *testing.T) {
	p, sched := fixture()
	g := graph.Build(p, sched)
	inv, err := g.Invalidate(domain.Edit{
		Kind: domain.EditWeight, DialogueID: "d01", ShardIndex: 0, PhonemeID: "a",
		NewWeight: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(inv.PackageIDs, "p-a") || !slices.Contains(inv.PackageIDs, "p-c") {
		t.Fatalf("overlapping packages not invalidated: %v", inv.PackageIDs)
	}
	if slices.Contains(inv.PackageIDs, "p-b") || slices.Contains(inv.PackageIDs, "p-d") {
		t.Fatalf("disjoint packages were invalidated: %v", inv.PackageIDs)
	}
	if !slices.Contains(inv.ReusedPackages, "p-b") || !slices.Contains(inv.ReusedPackages, "p-d") {
		t.Fatalf("disjoint packages not offered for reuse: %v", inv.ReusedPackages)
	}
	if !slices.Contains(inv.FrameNodes, "dispatch:p-a:0") {
		t.Fatalf("dispatch nodes missing: %v", inv.FrameNodes)
	}
}

func TestBoundaryEditIncludesDialogueDownstreamButNotFarOtherChannel(t *testing.T) {
	p, sched := fixture()
	g := graph.Build(p, sched)
	inv, err := g.Invalidate(domain.Edit{
		Kind: domain.EditBoundary, DialogueID: "d01", ShardIndex: 0, PhonemeID: "a",
		NewStartFrame: 0, NewEndFrame: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(inv.PackageIDs, "p-b") {
		t.Fatalf("dialogue downstream package not invalidated: %v", inv.PackageIDs)
	}
	if slices.Contains(inv.PackageIDs, "p-d") {
		t.Fatalf("far package was invalidated: %v", inv.PackageIDs)
	}
	if !slices.Contains(inv.ReusedPackages, "p-d") {
		t.Fatalf("far package not reusable")
	}
}

func TestFPSEditInvalidatesEverything(t *testing.T) {
	p, sched := fixture()
	g := graph.Build(p, sched)
	inv, err := g.Invalidate(domain.Edit{Kind: domain.EditFPS, NewFPS: 60})
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.PackageIDs) != len(sched.Packages) || len(inv.ReusedPackages) != 0 {
		t.Fatalf("fps edit invalidation = %+v", inv)
	}
	if len(inv.PhonemeNodes) != 4 {
		t.Fatalf("phoneme nodes = %v", inv.PhonemeNodes)
	}
}

func TestUnknownPhonemeReturnsParameterError(t *testing.T) {
	p, sched := fixture()
	g := graph.Build(p, sched)
	_, err := g.Invalidate(domain.Edit{
		Kind: domain.EditWeight, DialogueID: "d01", PhonemeID: "missing",
	})
	if domain.ErrorCode(err) != domain.CodeParam {
		t.Fatalf("code = %s: %v", domain.ErrorCode(err), err)
	}
}
