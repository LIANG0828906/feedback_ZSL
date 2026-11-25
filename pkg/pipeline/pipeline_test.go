package pipeline_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"lipsync.local/prototype/pkg/checkpoint"
	"lipsync.local/prototype/pkg/domain"
	"lipsync.local/prototype/pkg/pipeline"
	"lipsync.local/prototype/pkg/sample"
	"lipsync.local/prototype/pkg/verify"
	"lipsync.local/prototype/pkg/workpool"
)

func TestThreePathsMatchTruthAndEachOther(t *testing.T) {
	project, truth, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	reports := make(map[pipeline.Path]*pipeline.Report)
	for _, path := range []pipeline.Path{pipeline.PathRealtime, pipeline.PathFull, pipeline.PathResume} {
		store, err := checkpoint.Open(filepath.Join(t.TempDir(), string(path)+".sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		report, err := pipeline.Run(context.Background(), project, pipeline.Options{
			Path: path, Store: store,
			Pool: workpoolLimits(),
		})
		if err != nil {
			store.Close()
			t.Fatalf("%s: %v", path, err)
		}
		if err := verify.Truth(project, report, truth); err != nil {
			store.Close()
			t.Fatalf("%s truth mismatch: %v", path, err)
		}
		reports[path] = report
		store.Close()
	}
	if err := pipeline.CompareReports(reports[pipeline.PathFull], reports[pipeline.PathRealtime]); err != nil {
		t.Fatalf("realtime/full mismatch: %v", err)
	}
	if err := pipeline.CompareReports(reports[pipeline.PathFull], reports[pipeline.PathResume]); err != nil {
		t.Fatalf("resume/full mismatch: %v", err)
	}
	if len(reports[pipeline.PathFull].GlobalDispatch.Deferred) == 0 {
		t.Fatal("sample should exercise at least one deferral")
	}
}

func TestResumeReusesOnlyCommittedShards(t *testing.T) {
	project, _, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "resume.sqlite")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := pipeline.Run(context.Background(), project, pipeline.Options{Path: pipeline.PathFull, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	store, err = checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resumed, err := pipeline.Run(context.Background(), project, pipeline.Options{Path: pipeline.PathResume, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Recovered) != len(project.Dialogues) {
		t.Fatalf("expected all shards recovered, got %v", resumed.Recovered)
	}
	if err := pipeline.CompareReports(first, resumed); err != nil {
		t.Fatal(err)
	}
	wrongVersion, err := store.Recover(context.Background(), expectedHashes(project), "wrong-version", project.Recipe.AlgorithmVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(wrongVersion) != 0 {
		t.Fatalf("version mismatch should not recover: %d", len(wrongVersion))
	}
}

func TestSingleShardFailureIsIsolated(t *testing.T) {
	project, _, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "fault.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	report, err := pipeline.Run(context.Background(), project, pipeline.Options{
		Path: pipeline.PathRealtime, Store: store,
		Faults: map[string]pipeline.FaultKind{project.Dialogues[1].ID: pipeline.FaultStage},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := domain.CodeOf(report.FirstError()); code != domain.ErrStage {
		t.Fatalf("want E_STAGE, got %s (%v)", code, report.FirstError())
	}
	if len(report.CommitSequence) != len(project.Dialogues)-1 {
		t.Fatalf("failed shard should not commit: %v", report.CommitSequence)
	}
	for _, shard := range report.Shards {
		if shard.DialogueID == project.Dialogues[1].ID {
			if shard.Err == nil {
				t.Fatal("injected shard unexpectedly succeeded")
			}
			continue
		}
		if shard.Err != nil {
			t.Fatalf("healthy shard was polluted: %v", shard.Err)
		}
	}
}

func TestCompareReportsLocatesFirstDifference(t *testing.T) {
	project, _, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "compare.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	report, err := pipeline.Run(context.Background(), project, pipeline.Options{Path: pipeline.PathFull, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	altered := *report
	altered.Shards = append([]pipeline.ShardResult(nil), report.Shards...)
	altered.Shards[2].Lip = report.Shards[2].Lip
	altered.Shards[2].Lip.Frames = append([]domain.FrameLip(nil), report.Shards[2].Lip.Frames...)
	frame := altered.Shards[2].Lip.Frames[17].Frame
	altered.Shards[2].Lip.Frames[17].Lip = "deliberate-diff"
	err = pipeline.CompareReports(report, &altered)
	if code := domain.CodeOf(err); code != domain.ErrDiff {
		t.Fatalf("want E_DIFF, got %s: %v", code, err)
	}
	de := err.(*domain.Error)
	if de.DialogueID != report.Shards[2].DialogueID || de.Frame != frame || de.Field != "frame.lip" {
		t.Fatalf("E_DIFF lacks precise location: %+v", de)
	}
}

func TestThreePathsRemainEqualAfterEditSequence(t *testing.T) {
	project, _, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	var edited domain.Project
	if err := json.Unmarshal(data, &edited); err != nil {
		t.Fatal(err)
	}
	edited.Dialogues[0].Phonemes[1].WeightPermille += 31
	edited.Dialogues[1].Phonemes[2].EndSample -= 90
	edited.Dialogues[2].Phonemes[1].AffinityToNext = 990
	if err := domain.ValidateProject(&edited); err != nil {
		t.Fatal(err)
	}
	reports := make(map[pipeline.Path]*pipeline.Report)
	for _, path := range []pipeline.Path{pipeline.PathRealtime, pipeline.PathFull, pipeline.PathResume} {
		store, err := checkpoint.Open(filepath.Join(t.TempDir(), string(path)+".sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		report, err := pipeline.Run(context.Background(), &edited, pipeline.Options{
			Path: path, Store: store, Pool: workpoolLimits(),
		})
		store.Close()
		if err != nil {
			t.Fatal(err)
		}
		reports[path] = report
	}
	if err := pipeline.CompareReports(reports[pipeline.PathFull], reports[pipeline.PathRealtime]); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.CompareReports(reports[pipeline.PathFull], reports[pipeline.PathResume]); err != nil {
		t.Fatal(err)
	}
}

func expectedHashes(project *domain.Project) map[string]string {
	out := make(map[string]string, len(project.Dialogues))
	for i, d := range domain.SortedDialogues(project.Dialogues) {
		out[shardID(i)] = d.FullContentHash(project.Recipe)
	}
	return out
}

func shardID(i int) string {
	return fmt.Sprintf("shard-%03d", i)
}

func workpoolLimits() workpool.Pool {
	return workpool.Pool{Workers: 2, PerWorkerBuffer: 1, QueueBudget: 1}
}
