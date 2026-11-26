package engine_test

import (
	"context"
	"reflect"
	"testing"

	"dialogueaudio/audioerr"
	"dialogueaudio/engine"
	"dialogueaudio/fixed"
	"dialogueaudio/generator"
)

func params(set *generator.SampleSet) engine.FrameParams {
	return engine.FrameParams{
		SampleRate: set.Project.SampleRate, FrameLen: set.Project.FrameLenSamples,
		Hop: set.Project.HopSamples, SessionGainQ16: set.Project.SessionGainQ16,
	}
}

func TestTruthExactAndChunkInvariant(t *testing.T) {
	set, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	clip := set.Project.Clips[0]
	baseline, err := engine.MeasureClip(clip, params(set), 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []int{1, 480, 960, 1920, 100000} {
		got, err := engine.MeasureClip(clip, params(set), chunk)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, baseline) {
			t.Fatalf("chunk size %d changed frame values", chunk)
		}
	}
	all := make(map[string]engine.ClipEnvelope)
	for _, c := range set.Project.Clips {
		env, err := engine.MeasureClip(c, params(set), 960)
		if err != nil {
			t.Fatal(err)
		}
		all[c.ID] = env
	}
	cmp := generator.CompareEnvelopes(set.Truth, all, 0)
	if !cmp.Pass || cmp.MaxDeviationQ16 != 0 {
		t.Fatalf("truth mismatch: %+v", cmp)
	}
}

func TestParallelSchedulingInvariant(t *testing.T) {
	set, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	one, err := engine.MeasureClips(context.Background(), set.Project.Clips, params(set),
		engine.BatchOptions{MaxParallel: 1, QueueBudget: 1, SampleBufferSamples: 480})
	if err != nil {
		t.Fatal(err)
	}
	four, err := engine.MeasureClips(context.Background(), set.Project.Clips, params(set),
		engine.BatchOptions{MaxParallel: 4, QueueBudget: 1, SampleBufferSamples: 1920})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(one.Envelopes, four.Envelopes) {
		t.Fatal("parallel scheduling changed envelope samples")
	}
	if len(four.QueueEvents) == 0 {
		t.Fatal("expected queue budget exhaustion evidence")
	}
	for _, event := range four.QueueEvents {
		if event.Reason != "queue_budget_exhausted" {
			t.Fatalf("bad queue event: %+v", event)
		}
	}
}

func TestCalibrationReachableAndUnreachable(t *testing.T) {
	set, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	envelopes := make(map[string]engine.ClipEnvelope)
	for _, clip := range set.Project.Clips {
		env, err := engine.MeasureClip(clip, params(set), 960)
		if err != nil {
			t.Fatal(err)
		}
		envelopes[clip.ID] = env
	}
	cal, err := engine.CalibrateProject(set.Project, envelopes, 256)
	if err != nil {
		t.Fatal(err)
	}
	for _, clip := range cal.Clips {
		if !clip.WithinTolerance {
			t.Fatalf("calibration outside tolerance for %s: %+v", clip.ClipID, clip)
		}
		if clip.AdjustmentGainQ16 <= 0 || clip.ClampedDBQ16 != 0 {
			t.Fatalf("bad calibration value for %s: %+v", clip.ClipID, clip)
		}
	}
	unreachable := set.Project.Clone()
	unreachable.TargetLoudnessQ16 += 20 * fixed.Q16
	_, err = engine.CalibrateProject(unreachable, envelopes, 256)
	coded, ok := audioerr.As(err)
	if !ok || coded.Code != audioerr.CodeLoudness {
		t.Fatalf("want E_LOUD, got %v", err)
	}
	if coded.Values["first_limited_clip"] == "" {
		t.Fatalf("E_LOUD missing first limited clip: %+v", coded)
	}
}

func TestCalibrationDisabledLeavesResultsUnchanged(t *testing.T) {
	set, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	project := set.Project.Clone()
	project.CalibrationEnabled = false
	envelopes := make(map[string]engine.ClipEnvelope)
	for _, clip := range project.Clips {
		env, err := engine.MeasureClip(clip, params(set), 960)
		if err != nil {
			t.Fatal(err)
		}
		envelopes[clip.ID] = env
	}
	cal, err := engine.CalibrateProject(project, envelopes, 256)
	if err != nil {
		t.Fatal(err)
	}
	for _, clip := range cal.Clips {
		if clip.AdjustmentGainQ16 != fixed.Q16 ||
			clip.AfterLoudnessQ16 != clip.BeforeLoudnessQ16 {
			t.Fatalf("disabled calibration changed %s: %+v", clip.ClipID, clip)
		}
	}
}
