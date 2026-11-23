package engine_test

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"storyworkbench/engine"
	"storyworkbench/story"
)

const samplePath = "../samples/glass-meridian-20261124.json"

func loadSample(t *testing.T) *story.Project {
	t.Helper()
	project, err := story.LoadProject(samplePath)
	if err != nil {
		t.Fatalf("load sample: %v", err)
	}
	return project
}

func TestPlayMatchesGeneratedTruthAndIsDeterministic(t *testing.T) {
	project := loadSample(t)
	first, err := engine.Play(project, project.Choices)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	second, err := engine.Play(project, project.Choices)
	if err != nil {
		t.Fatalf("play second: %v", err)
	}
	if err := engine.CompareRuns(first, second); err != nil {
		t.Fatalf("deterministic replay diverged: %v", err)
	}
	if project.Truth == nil {
		t.Fatal("sample has no truth")
	}
	if first.ShotCount != project.Truth.ShotCount {
		t.Fatalf("shot count %d, truth %d", first.ShotCount, project.Truth.ShotCount)
	}
	if first.TotalDurationMS != project.Truth.TotalDurationMS {
		t.Fatalf("duration %d, truth %d", first.TotalDurationMS, project.Truth.TotalDurationMS)
	}
	if !jsonEqual(first.EventOrder, project.Truth.EventOrder) {
		t.Fatalf("event order %v, truth %v", first.EventOrder, project.Truth.EventOrder)
	}
	if !jsonEqual(first.ReachableNodes, project.Truth.ReachableNodes) {
		t.Fatalf("reachable %v, truth %v", first.ReachableNodes, project.Truth.ReachableNodes)
	}
	for key, expected := range project.Truth.FinalFlags {
		actual, ok := first.FinalFlags[key]
		if !ok || !story.ScalarEqual(expected, actual) {
			t.Fatalf("final flag %s: got %v, truth %v", key, actual, expected)
		}
	}
	if len(first.BlockedChoices) != 3 {
		t.Fatalf("expected 3 blocked choices, got %d: %+v", len(first.BlockedChoices), first.BlockedChoices)
	}
}

func TestParallelPlayProducesIdenticalResults(t *testing.T) {
	project := loadSample(t)
	expected, err := engine.Play(project, project.Choices)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	const runs = 12
	results := make([]*engine.RunResult, runs)
	errs := make([]error, runs)
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], errs[index] = engine.Play(project, project.Choices)
		}(i)
	}
	wg.Wait()
	for i := 0; i < runs; i++ {
		if errs[i] != nil {
			t.Fatalf("run %d: %v", i, errs[i])
		}
		if err := engine.CompareRuns(expected, results[i]); err != nil {
			t.Fatalf("run %d diverged: %v", i, err)
		}
	}
}

func TestSaveResumeMatchesContinuousAtEveryBoundary(t *testing.T) {
	project := loadSample(t)
	continuous, err := engine.Play(project, project.Choices)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	for boundary := 0; boundary <= continuous.ShotCount; boundary++ {
		session, err := engine.PlayToBoundary(project, project.Choices, boundary)
		if err != nil {
			t.Fatalf("boundary %d: %v", boundary, err)
		}
		save := session.Snapshot()
		if save.State.ShotProgress != boundary {
			t.Fatalf("boundary %d: save progress %d", boundary, save.State.ShotProgress)
		}
		data, err := engine.MarshalSave(save)
		if err != nil {
			t.Fatalf("boundary %d marshal: %v", boundary, err)
		}
		parsed, err := engine.ParseSave(data)
		if err != nil {
			t.Fatalf("boundary %d parse: %v", boundary, err)
		}
		if err := engine.ValidateSave(project, parsed, project.Choices); err != nil {
			t.Fatalf("boundary %d validate: %v", boundary, err)
		}
		resumed, err := engine.Resume(project, parsed, project.Choices)
		if err != nil {
			t.Fatalf("boundary %d resume: %v", boundary, err)
		}
		if err := engine.CompareRuns(continuous, resumed); err != nil {
			t.Fatalf("boundary %d diverged: %v", boundary, err)
		}
	}
}

func TestSaveConflictFieldsAreReportedWithoutPartialLoad(t *testing.T) {
	project := loadSample(t)
	session, err := engine.PlayToBoundary(project, project.Choices, 5)
	if err != nil {
		t.Fatalf("play to boundary: %v", err)
	}
	valid := session.Snapshot()
	data, err := engine.MarshalSave(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ParseSave(data); err != nil {
		t.Fatalf("valid save should parse: %v", err)
	}

	bad := cloneSave(t, valid)
	bad.SchemaVersion = 99
	assertSaveError(t, engine.ValidateSave(project, &bad, project.Choices), "schema_version")

	bad = cloneSave(t, valid)
	bad.AlgorithmVersion = "other"
	assertSaveError(t, engine.ValidateSave(project, &bad, project.Choices), "algorithm_version")

	bad = cloneSave(t, valid)
	bad.ContentHash = strings.Repeat("0", 64)
	assertSaveError(t, engine.ValidateSave(project, &bad, project.Choices), "content_hash")

	bad = cloneSave(t, valid)
	bad.State.Flags["arrived"] = false
	_, err = engine.ParseSave(mustMarshal(t, bad))
	assertSaveError(t, err, "fingerprint")

	bad = cloneSave(t, valid)
	bad.State.Shots[0].Description += " changed"
	_, err = engine.ParseSave(mustMarshal(t, bad))
	assertSaveError(t, err, "fingerprint")

	resumed, err := engine.Resume(project, &valid, project.Choices)
	if err != nil {
		t.Fatalf("valid save was disturbed: %v", err)
	}
	continuous, err := engine.Play(project, project.Choices)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.CompareRuns(continuous, resumed); err != nil {
		t.Fatalf("valid save diverged: %v", err)
	}
}

func TestEditedReplayReusesUnaffectedShotsAndMatchesFullReplay(t *testing.T) {
	project := loadSample(t)
	baseline, err := engine.Play(project, project.Choices)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	oldHash := story.ContentHash(project)
	nodes := map[string]*story.Node{}
	for i := range project.Nodes {
		nodes[project.Nodes[i].ID] = &project.Nodes[i]
	}
	events := append([]story.Event(nil), nodes["archive"].Events...)
	events = append(events, story.Event{
		ID: "ev-archive-editorial", Kind: "set_flag", Key: "editorial_note", Value: "revised",
	})
	outcome, err := engine.ReplayEdited(project, project.Choices, baseline, engine.NodeEdit{
		NodeID: "archive",
		Events: &events,
	})
	if err != nil {
		t.Fatalf("replay edited: %v", err)
	}
	if err := engine.CompareRuns(outcome.VerificationRun, outcome.Run); err != nil {
		t.Fatalf("edited replay diverged from full replay: %v", err)
	}
	if story.ContentHash(project) != oldHash {
		t.Fatal("baseline project was mutated by replay")
	}
	if baseline.Shots[0].Reused || baseline.Shots[0].Invalidated {
		t.Fatal("baseline run was mutated")
	}
	invalid := map[string]bool{}
	for _, node := range outcome.Invalidation.InvalidatedNodes {
		invalid[node.NodeID] = true
	}
	if invalid["archive"] == false || invalid["gate"] == false || invalid["dawn"] == false {
		t.Fatalf("invalidated nodes are incomplete: %+v", outcome.Invalidation.InvalidatedNodes)
	}
	if invalid["arrival"] || invalid["whisper"] {
		t.Fatalf("unaffected upstream nodes were invalidated: %+v", outcome.Invalidation.InvalidatedNodes)
	}
	if len(outcome.Invalidation.ReusedShotSequences) == 0 {
		t.Fatal("no shots were reused")
	}
	if len(outcome.Invalidation.InvalidatedShots) == 0 {
		t.Fatal("no shots were invalidated")
	}
	reusedSeen := false
	recomputedSeen := false
	for _, shot := range outcome.Run.Shots {
		if shot.Reused {
			reusedSeen = true
		}
		if shot.Invalidated && shot.Recomputed {
			recomputedSeen = true
		}
	}
	if !reusedSeen || !recomputedSeen {
		t.Fatalf("shot provenance labels missing: reused=%v recomputed=%v", reusedSeen, recomputedSeen)
	}
}

func TestEditedReplaySequenceMatchesFullReplay(t *testing.T) {
	project := loadSample(t)
	baseline, err := engine.Play(project, project.Choices)
	if err != nil {
		t.Fatal(err)
	}
	archiveEvents := append([]story.Event(nil), project.Nodes[3].Events...)
	archiveEvents = append(archiveEvents, story.Event{
		ID: "ev-sequence-one", Kind: "set_flag", Key: "sequence_one", Value: true,
	})
	gateEvents := append([]story.Event(nil), project.Nodes[5].Events...)
	gateEvents = append(gateEvents, story.Event{
		ID: "ev-sequence-two", Kind: "inc_character", Character: "mara", Key: "focus", Value: int64(1),
	})
	outcome, err := engine.ReplayEditedSequence(project, project.Choices, baseline, []engine.NodeEdit{
		{NodeID: "archive", Events: &archiveEvents},
		{NodeID: "gate", Events: &gateEvents},
	})
	if err != nil {
		t.Fatalf("sequence replay: %v", err)
	}
	if err := engine.CompareRuns(outcome.VerificationRun, outcome.Run); err != nil {
		t.Fatalf("sequence replay diverged: %v", err)
	}
	if outcome.Run.FinalFlags["sequence_one"] != true {
		t.Fatal("first edit did not survive the sequence")
	}
	if len(outcome.Invalidation.InvalidatedNodes) == 0 {
		t.Fatal("aggregate invalidation report is empty")
	}
}

func TestCompareRunsLocatesFirstDivergence(t *testing.T) {
	project := loadSample(t)
	expected, err := engine.Play(project, project.Choices)
	if err != nil {
		t.Fatal(err)
	}
	actual := *expected
	actual.Shots = append([]engine.ShotPresentation(nil), expected.Shots...)
	actual.Shots[4].DurationMS++
	err = engine.CompareRuns(expected, &actual)
	appErr, ok := story.AsError(err)
	if !ok || appErr.Code != story.CodeDiverge {
		t.Fatalf("expected E_DIVERGE, got %v", err)
	}
	if appErr.ShotSequence == nil || *appErr.ShotSequence != 4 {
		t.Fatalf("expected shot 4, got %+v", appErr)
	}
	if appErr.NodeID != expected.Shots[4].NodeID || appErr.Field != "duration_ms" {
		t.Fatalf("unexpected divergence locator: %+v", appErr)
	}
}

func assertSaveError(t *testing.T, err error, field string) {
	t.Helper()
	appErr, ok := story.AsError(err)
	if !ok {
		t.Fatalf("expected *story.Error, got %T: %v", err, err)
	}
	if appErr.Code != story.CodeSave {
		t.Fatalf("expected E_SAVE, got %s (%v)", appErr.Code, err)
	}
	if appErr.Field != field {
		t.Fatalf("expected field %s, got %s (%v)", field, appErr.Field, err)
	}
}

func mustMarshal(t *testing.T, save engine.Save) []byte {
	t.Helper()
	data, err := engine.MarshalSave(save)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func cloneSave(t *testing.T, save engine.Save) engine.Save {
	t.Helper()
	parsed, err := engine.ParseSave(mustMarshal(t, save))
	if err != nil {
		t.Fatal(err)
	}
	return *parsed
}

func jsonEqual(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func TestLoadFixtureExists(t *testing.T) {
	if _, err := os.Stat(samplePath); err != nil {
		t.Fatal(err)
	}
}
