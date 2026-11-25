package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"lipsync/arbitration"
	"lipsync/checkpoint"
	"lipsync/domain"
	"lipsync/engine"
	"lipsync/scenario"
)

func sample(t *testing.T, seed int64) *scenario.Artifacts {
	t.Helper()
	artifacts, err := scenario.Generate(seed)
	if err != nil {
		t.Fatal(err)
	}
	return artifacts
}

func openStore(t *testing.T, name string) *checkpoint.Store {
	t.Helper()
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func compareResultToTruth(t *testing.T, report *engine.RunReport, truth domain.ArbitrationResult) {
	t.Helper()
	if !reflect.DeepEqual(report.Frames, truth.Frames) {
		t.Fatalf("frames differ")
	}
	if !reflect.DeepEqual(report.Preemptions, truth.Preemptions) {
		t.Fatalf("preemptions differ")
	}
	if !reflect.DeepEqual(report.Resumes, truth.Resumes) {
		t.Fatalf("resumes differ")
	}
	if !reflect.DeepEqual(report.Abandonments, truth.Abandonments) {
		t.Fatalf("abandonments differ")
	}
	if !reflect.DeepEqual(report.PacketStates, truth.PacketStates) {
		t.Fatalf("packet states differ")
	}
}

func TestEngineLiveParallelMatchesGeneratorTruth(t *testing.T) {
	artifacts := sample(t, 42)
	store := openStore(t, "live.db")
	eng, err := engine.New(artifacts.Canonical, store, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	report, err := eng.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	eng.Close()
	if !report.Complete {
		t.Fatal("run did not complete")
	}
	compareResultToTruth(t, report, artifacts.Truth)
	if len(report.CommitSequence) < 2 {
		t.Fatalf("expected multiple window commits, got %v", report.CommitSequence)
	}
	for i, sequence := range report.CommitSequence {
		if sequence != i+1 {
			t.Fatalf("commit sequence is not stable: %v", report.CommitSequence)
		}
	}
	if report.ParallelStats.MaxRunning > int64(engine.DefaultOptions().MaxWorkers) {
		t.Fatalf("parallelism exceeded cap: %d", report.ParallelStats.MaxRunning)
	}
	assertCapacityInvariants(t, artifacts, report)
}

func assertCapacityInvariants(t *testing.T, artifacts *scenario.Artifacts, report *engine.RunReport) {
	t.Helper()
	channels := map[string]domain.Channel{}
	for _, channel := range artifacts.Canonical.Channels {
		channels[channel.ID] = channel
	}
	acceptedByFrame := map[string]map[int64]bool{}
	for _, frame := range report.Frames {
		for _, channelResult := range frame.Channels {
			channel := channels[channelResult.ChannelID]
			if channelResult.OccupiedSlot > channel.SlotLimit {
				t.Fatalf("frame %d channel %s exceeded slots", frame.Frame, channel.ID)
			}
			if channelResult.UsedBudget > channel.FrameBudgetBasisPoints {
				t.Fatalf("frame %d channel %s exceeded budget", frame.Frame, channel.ID)
			}
			for _, packetID := range channelResult.Accepted {
				if acceptedByFrame[packetID] == nil {
					acceptedByFrame[packetID] = map[int64]bool{}
				}
				acceptedByFrame[packetID][frame.Frame] = true
			}
		}
	}
	for _, resume := range report.Resumes {
		run := 0
		for acceptedByFrame[resume.PacketID][resume.Frame+int64(run)] {
			run++
		}
		if run < artifacts.Canonical.Recovery.MinContinuousFrames {
			t.Fatalf("packet %s resumed with only %d continuous frames", resume.PacketID, run)
		}
	}
	packets := map[string]domain.Packet{}
	for _, packet := range artifacts.Canonical.Packets() {
		packets[packet.ID] = packet
	}
	for _, event := range report.Preemptions {
		if event.DeliveredFrames+event.RemainingFrames != packets[event.PacketID].DurationFrames {
			t.Fatalf("preemption counters inconsistent: %+v", event)
		}
	}
}

func TestSequentialAndParallelAreIdentical(t *testing.T) {
	artifacts := sample(t, 77)
	parallelStore := openStore(t, "parallel.db")
	parallelEngine, err := engine.New(artifacts.Canonical, parallelStore, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	parallelReport, err := parallelEngine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parallelEngine.Close()

	options := engine.DefaultOptions()
	options.DisableParallel = true
	sequentialStore := openStore(t, "sequential.db")
	sequentialEngine, err := engine.New(artifacts.Canonical, sequentialStore, options)
	if err != nil {
		t.Fatal(err)
	}
	sequentialReport, err := sequentialEngine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sequentialEngine.Close()
	compareReports(t, parallelReport, sequentialReport)
}

func TestCheckpointResumeMatchesFullRecompute(t *testing.T) {
	artifacts := sample(t, 42)
	fullStore := openStore(t, "full.db")
	fullEngine, err := engine.New(artifacts.Canonical, fullStore, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	fullReport, err := fullEngine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fullEngine.Close()

	resumeStore := openStore(t, "resume.db")
	options := engine.DefaultOptions()
	options.MaxWindows = 1
	interrupted, err := engine.New(artifacts.Canonical, resumeStore, options)
	if err != nil {
		t.Fatal(err)
	}
	first, err := interrupted.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	interrupted.Close()
	if first.Complete {
		t.Fatal("partial run unexpectedly complete")
	}

	resumedEngine, err := engine.New(artifacts.Canonical, resumeStore, engine.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	resumedReport, err := resumedEngine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resumedEngine.Close()
	if !resumedReport.Complete {
		t.Fatal("resumed run did not complete")
	}
	if resumedReport.Mode != "resumed" {
		t.Fatalf("expected resumed mode, got %s", resumedReport.Mode)
	}
	compareReports(t, fullReport, resumedReport)
}

func compareReports(t *testing.T, a, b *engine.RunReport) {
	t.Helper()
	if !reflect.DeepEqual(a.Frames, b.Frames) {
		t.Fatal("frame sequences differ")
	}
	if !reflect.DeepEqual(a.Preemptions, b.Preemptions) ||
		!reflect.DeepEqual(a.Resumes, b.Resumes) ||
		!reflect.DeepEqual(a.Abandonments, b.Abandonments) {
		t.Fatal("event sequences differ")
	}
	if !reflect.DeepEqual(a.PacketStates, b.PacketStates) {
		t.Fatal("packet states differ")
	}
	if !reflect.DeepEqual(a.CommitSequence, b.CommitSequence) {
		t.Fatalf("commit sequences differ: %v != %v", a.CommitSequence, b.CommitSequence)
	}
	if !reflect.DeepEqual(a.Windows, b.Windows) {
		t.Fatal("window commit sequence differs")
	}
}

func TestChannelFailureIsRolledBackAndRetryable(t *testing.T) {
	artifacts := sample(t, 42)
	store := openStore(t, "fault.db")
	injected := false
	options := engine.DefaultOptions()
	options.Fault = func(channelID string, frame int64) error {
		if channelID == "spk_main" && frame == 2 && !injected {
			injected = true
			return &domain.Error{
				Code: domain.CodeState, Path: "injected.channel", ChannelID: channelID,
				Message: "injected channel failure",
			}
		}
		return nil
	}
	eng, err := engine.New(artifacts.Canonical, store, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background()); err == nil {
		t.Fatal("expected injected failure")
	}
	progress, ok := eng.Progress()
	if !ok || progress.State != domain.WindowFailed {
		t.Fatalf("expected failed window state, got %+v", progress)
	}
	report, err := eng.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	eng.Close()
	if !report.Complete {
		t.Fatal("retry did not complete")
	}
	compareResultToTruth(t, report, artifacts.Truth)
	if len(report.ChannelFailures) != 1 || !report.ChannelFailures[0].Recovered {
		t.Fatalf("failure was not recorded as retried: %+v", report.ChannelFailures)
	}
	if report.CommitSequence[0] != 1 {
		t.Fatalf("failed window polluted checkpoint sequence: %v", report.CommitSequence)
	}
}

func TestResumeDisabledAbandonReturnsEResume(t *testing.T) {
	project := scenario.Canonical(42)
	project.Recovery.AllowAbandon = false
	project.ContentHash = domain.ComputeContentHash(project)
	_, err := arbitration.Run(project)
	if err == nil || domain.CodeOf(err) != domain.CodeResume {
		t.Fatalf("expected E_RESUME, got %v", err)
	}
	var domainErr *domain.Error
	if !errors.As(err, &domainErr) {
		t.Fatalf("expected structured error, got %T", err)
	}
	if domainErr.PacketID == "" || domainErr.Available <= 0 || domainErr.Required <= 0 {
		t.Fatalf("E_RESUME lacks packet/window evidence: %+v", domainErr)
	}
}

func TestWindowStateMachineRejectsIllegalTransition(t *testing.T) {
	progress := domain.NewWindowProgress(3, 12, 15)
	before := progress
	err := progress.Transition(domain.WindowCommitted)
	if domain.CodeOf(err) != domain.CodeState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if progress != before {
		t.Fatalf("illegal transition mutated progress: %+v != %+v", progress, before)
	}
	if err := progress.Transition(domain.WindowArbitrating); err != nil {
		t.Fatal(err)
	}
	if err := progress.Transition(domain.WindowPreempting); err != nil {
		t.Fatal(err)
	}
	if err := progress.Transition(domain.WindowCommitted); err != nil {
		t.Fatal(err)
	}
	if progress.Committed != 1 {
		t.Fatalf("commit progress not recorded: %+v", progress)
	}
}

func TestFaultRoutes(t *testing.T) {
	artifacts := sample(t, 42)
	if err := arbitration.ValidateProject(artifacts.Canonical, true); err != nil {
		t.Fatal(err)
	}
	badCapacity := artifacts.Canonical
	badCapacity.Channels = append([]domain.Channel(nil), artifacts.Canonical.Channels...)
	badCapacity.Channels[0].FrameBudgetBasisPoints = 100
	badCapacity.ContentHash = domain.ComputeContentHash(badCapacity)
	if err := arbitration.ValidateProject(badCapacity, true); domain.CodeOf(err) != domain.CodeCapacity {
		t.Fatalf("expected E_CAP, got %v", err)
	}
	if _, err := arbitration.RunFrames(artifacts.Canonical, []int64{0, 2, 1}); domain.CodeOf(err) != domain.CodeOrder {
		t.Fatalf("expected E_ORDER, got %v", err)
	}
	badContent := artifacts.Canonical
	badContent.Name = "tampered"
	if err := arbitration.ValidateProject(badContent, true); domain.CodeOf(err) != domain.CodeDecode {
		t.Fatalf("expected E_DECODE, got %v", err)
	}
	raw, _ := json.Marshal(artifacts.Canonical)
	_ = raw
}
