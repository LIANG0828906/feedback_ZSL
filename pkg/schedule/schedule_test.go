package schedule_test

import (
	"encoding/json"
	"testing"

	"lipsync.local/prototype/pkg/domain"
	"lipsync.local/prototype/pkg/schedule"
)

func pkg(id, dialogue, channel string, start, end int) domain.LipPackage {
	return domain.LipPackage{ID: id, DialogueID: dialogue, Channel: channel, StartFrame: start, EndFrame: end}
}

func TestDeferRetryAndBudget(t *testing.T) {
	packages := []domain.LipPackage{
		pkg("d1/pkg/000", "d1", "a", 0, 4),
		pkg("d2/pkg/000", "d2", "a", 0, 4),
	}
	cfg := schedule.Config{
		MaxPackagesPerFrame: 1, MaxPackagesPerChannelFrame: 1,
		MaxDeferFrames: 2, Channels: map[string]int{"a": 1},
	}
	result, err := schedule.Dispatch(packages, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.DispatchFrame["d1/pkg/000"] != 0 || result.DispatchFrame["d2/pkg/000"] != 1 {
		t.Fatalf("unexpected dispatch frames: %+v", result.DispatchFrame)
	}
	if len(result.Deferred) != 1 || result.Deferred[0] != "d2/pkg/000" {
		t.Fatalf("missing deferral: %v", result.Deferred)
	}
	var sawDefer, sawRetry, sawAccept bool
	for _, event := range result.Events {
		if event.PackageID != "d2/pkg/000" {
			continue
		}
		switch event.Action {
		case "defer":
			sawDefer = true
		case "retry":
			sawRetry = true
		case "accept":
			sawAccept = true
		}
	}
	if !sawDefer || !sawRetry || !sawAccept {
		t.Fatalf("missing explainable retry trace: %+v", result.Events)
	}
	for _, frame := range result.Frames {
		if frame.Used > cfg.MaxPackagesPerFrame {
			t.Fatalf("frame budget exceeded: %+v", frame)
		}
		if len(frame.Channels["a"]) > 1 {
			t.Fatalf("channel budget exceeded: %+v", frame)
		}
	}
	shuffled, err := schedule.Dispatch([]domain.LipPackage{packages[1], packages[0]}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(result)
	b, _ := json.Marshal(shuffled)
	if string(a) != string(b) {
		t.Fatalf("input order changed schedule:\n%s\n%s", a, b)
	}
}

func TestRejectedPackageDoesNotConsumeBudget(t *testing.T) {
	packages := []domain.LipPackage{
		pkg("d1/pkg/000", "d1", "a", 0, 4),
		pkg("d2/pkg/000", "d2", "a", 0, 4),
	}
	result, err := schedule.Dispatch(packages, schedule.Config{
		MaxPackagesPerFrame: 1, MaxPackagesPerChannelFrame: 1,
		MaxDeferFrames: 0, Channels: map[string]int{"a": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rejected) != 1 || result.Rejected[0] != "d2/pkg/000" {
		t.Fatalf("expected deterministic rejection: %v", result.Rejected)
	}
	for _, id := range result.Frames[0].Dispatched {
		if id == "d2/pkg/000" {
			t.Fatal("rejected package occupied dispatch budget")
		}
	}
}

func TestScheduleConflict(t *testing.T) {
	_, err := schedule.Dispatch([]domain.LipPackage{pkg("d/pkg/000", "d", "missing", 0, 2)}, schedule.Config{
		MaxPackagesPerFrame: 1, MaxPackagesPerChannelFrame: 1,
		MaxDeferFrames: 0, Channels: map[string]int{"a": 1},
	})
	if code := domain.CodeOf(err); code != domain.ErrConflict {
		t.Fatalf("want E_CONFLICT, got %s: %v", code, err)
	}
}
