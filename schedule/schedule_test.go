package schedule_test

import (
	"reflect"
	"testing"

	"lipsync/domain"
	"lipsync/generate"
	"lipsync/schedule"
	"lipsync/schema"
)

func generatedProject(t *testing.T) *domain.Project {
	t.Helper()
	rep, err := generate.Generate(generate.Config{OutDir: t.TempDir(), Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := schema.LoadBundle(rep.Projects["v3"])
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScheduleDeterministicAndConstrained(t *testing.T) {
	p := generatedProject(t)
	pkgs, err := schedule.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := schedule.Schedule(p, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := schedule.ValidateSchedule(p.Render, got); err != nil {
		t.Fatal(err)
	}
	reversed := append([]domain.Package(nil), pkgs...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	got2, err := schedule.Schedule(p, reversed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Packages, got2.Packages) || !reflect.DeepEqual(got.Frames, got2.Frames) {
		t.Fatalf("schedule depends on input enumeration order")
	}
	delayed, retried := 0, 0
	for _, ev := range got.Events {
		if ev.Status == "delayed" {
			delayed++
			if ev.RetryFrame != ev.Frame+1 {
				t.Fatalf("delay event has no next-frame retry: %+v", ev)
			}
		}
		if ev.Status == "retry" {
			retried++
		}
	}
	if delayed == 0 || retried == 0 {
		t.Fatalf("sample did not exercise delay/retry: delayed=%d retried=%d", delayed, retried)
	}
}

func TestManifestTruthFramesMatchScheduler(t *testing.T) {
	rep, err := generate.Generate(generate.Config{OutDir: t.TempDir(), Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	p, m, err := schema.LoadBundle(rep.Projects["v3"])
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := schedule.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := schedule.Schedule(p, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Frames) != len(m.Frames) {
		t.Fatalf("frame count %d != truth %d", len(got.Frames), len(m.Frames))
	}
	for i := range m.Frames {
		if got.Frames[i].Frame != m.Frames[i].Frame ||
			!reflect.DeepEqual(got.Frames[i].PackageIDs, m.Frames[i].PackageIDs) {
			t.Fatalf("frame %d mismatch: %+v != %+v", i, got.Frames[i], m.Frames[i])
		}
	}
}

func TestValidateBudgetIdentifiesFrameAndChannel(t *testing.T) {
	cfg := domain.RenderConfig{FrameBudget: 3, ChannelSlots: 1}
	result := &domain.ScheduleResult{Packages: []domain.ScheduledPackage{
		{Package: domain.Package{ID: "a", Channel: 0}},
		{Package: domain.Package{ID: "b", Channel: 0}},
		{Package: domain.Package{ID: "c", Channel: 0}},
		{Package: domain.Package{ID: "d", Channel: 0}},
	}}
	result.Frames = []domain.FrameRecord{{Frame: 7, PackageIDs: []string{"a", "b", "c", "d"}}}
	err := schedule.ValidateSchedule(cfg, result)
	if domain.ErrorCode(err) != domain.CodeBudget {
		t.Fatalf("code = %s: %v", domain.ErrorCode(err), err)
	}
	de, _ := domain.AsError(err)
	if de.Frame != 7 || de.Channel != 0 {
		t.Fatalf("budget error location = %+v", de)
	}

	cfg.FrameBudget = 10
	result.Frames[0].PackageIDs = []string{"a", "b"}
	err = schedule.ValidateSchedule(cfg, result)
	if domain.ErrorCode(err) != domain.CodeBudget {
		t.Fatalf("channel budget code = %s: %v", domain.ErrorCode(err), err)
	}
	de, _ = domain.AsError(err)
	if de.Frame != 7 || de.Channel != 0 {
		t.Fatalf("channel budget location = %+v", de)
	}
}

func TestMinimumVisibleFramesExtendsDispatchInterval(t *testing.T) {
	p := &domain.Project{Render: domain.RenderConfig{
		TargetFPS: 50, Rounding: domain.RoundFloor, FrameBudget: 2,
		ChannelSlots: 1, MinVisibleFrames: 3, MaxPhonemesPerPackage: 2,
		AffinityThreshold: 0.5, MaxDelayFrames: 0,
	}}
	pkg := domain.Package{
		ID: "short", DialogueID: "d", ShardIndex: 0, Channel: 0,
		StartFrame: 10, EndFrame: 11, PhonemeIDs: []string{"p"},
	}
	got, err := schedule.Schedule(p, []domain.Package{pkg})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Packages) != 1 || got.Packages[0].DispatchStartFrame != 10 ||
		got.Packages[0].DispatchEndFrame != 13 {
		t.Fatalf("minimum visible dispatch = %+v", got.Packages)
	}
	if len(got.Frames) != 3 {
		t.Fatalf("visible frame count = %d", len(got.Frames))
	}
}
