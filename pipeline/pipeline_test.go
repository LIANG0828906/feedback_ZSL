package pipeline_test

import (
	"context"
	"strings"
	"testing"

	"dialogueaudio/audioerr"
	"dialogueaudio/engine"
	"dialogueaudio/generator"
	"dialogueaudio/pipeline"
)

func options() pipeline.Options {
	return pipeline.Options{
		Batch: engine.BatchOptions{MaxParallel: 2, QueueBudget: 1, SampleBufferSamples: 960},
	}
}

func TestThreePathsAndCancellationRecovery(t *testing.T) {
	set, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	rtOpts := options()
	rtOpts.CheckpointDir = root + "/rt"
	rtRunner, err := pipeline.NewRunner(set.Project, rtOpts)
	if err != nil {
		t.Fatal(err)
	}
	realtime, err := rtRunner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resumeOpts := options()
	resumeOpts.CheckpointDir = root + "/resume"
	resumeOpts.MaxNewCommits = 2
	firstRunner, err := pipeline.NewRunner(set.Project, resumeOpts)
	if err != nil {
		t.Fatal(err)
	}
	first, err := firstRunner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Progress.Committed != 2 {
		t.Fatalf("expected partial commit, got %+v", first.Progress)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelRunner, err := pipeline.NewRunner(set.Project, resumeOpts)
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := cancelRunner.Run(ctx)
	if !audioerr.IsCode(err, audioerr.CodeCancelled) {
		t.Fatalf("want E_CANCELLED, got %v", err)
	}
	if canceled.Progress.Committed != 2 {
		t.Fatalf("cancellation committed more clips: %+v", canceled.Progress)
	}
	resumeOpts.MaxNewCommits = 0
	finalRunner, err := pipeline.NewRunner(set.Project, resumeOpts)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := finalRunner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.CommittedFromCheckpoint != 2 {
		t.Fatalf("resume did not start from committed prefix: %+v", resumed)
	}
	fullRunner, err := pipeline.NewRunner(set.Project, options())
	if err != nil {
		t.Fatal(err)
	}
	full, err := fullRunner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := pipeline.CompareRuns(realtime, resumed); mismatch != nil {
		t.Fatal(mismatch)
	}
	if mismatch := pipeline.CompareRuns(realtime, full); mismatch != nil {
		t.Fatal(mismatch)
	}
}

func TestCacheReuseAfterGainEdit(t *testing.T) {
	set, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	opts := options()
	opts.CacheDir = cacheDir
	warmRunner, err := pipeline.NewRunner(set.Project, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := warmRunner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	edited := set.Project.Clone()
	for i := range edited.Clips {
		if edited.Clips[i].ID == "c02" {
			edited.Clips[i].GainQ16 += 6554
		}
	}
	editedRunner, err := pipeline.NewRunner(edited, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := editedRunner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.RecomputedEnvelopeClips, ",") != "c02" ||
		strings.Join(result.RecomputedCalibrationClips, ",") != "c02" {
		t.Fatalf("unaffected clips were recomputed: %+v", result)
	}
	if len(result.ReusedEnvelopeClips) != 5 || len(result.ReusedCalibrationClips) != 5 {
		t.Fatalf("cache did not reuse unaffected clips: %+v", result)
	}
}

func TestContentMismatchRebuildsCheckpoint(t *testing.T) {
	set, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	opts := options()
	opts.CheckpointDir = t.TempDir()
	runner, err := pipeline.NewRunner(set.Project, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	changed := set.Project.Clone()
	changed.Clips[0].PCM[5] ^= 1
	changed.PopulateContentHashes()
	changedRunner, err := pipeline.NewRunner(changed, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := changedRunner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Rebuilt || result.CommittedFromCheckpoint != 0 || len(result.CommitOrder) != 6 {
		t.Fatalf("content mismatch was not rebuilt: %+v", result)
	}
}
