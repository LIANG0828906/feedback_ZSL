package lipsync

import (
	"math"
	"testing"
)

func sampleSetForTest(t *testing.T, count int) SampleSet {
	t.Helper()
	set, err := GenerateSampleSet(42, count)
	if err != nil {
		t.Fatalf("GenerateSampleSet: %v", err)
	}
	if len(set.Projects) != count || len(set.Truths) != count {
		t.Fatalf("sample counts = %d/%d, want %d", len(set.Projects), len(set.Truths), count)
	}
	return set
}

func TestQuantizeDirections(t *testing.T) {
	cases := []struct {
		sample     int64
		sampleRate int
		fps        int
		direction  string
		want       int64
	}{
		{8000, 48000, 24, "ceil", 4},
		{8000, 48000, 24, "floor", 4},
		{8000, 48000, 24, "round", 4},
		{8001, 48000, 24, "ceil", 5},
		{8001, 48000, 24, "floor", 4},
		{8001, 48000, 24, "round", 4},
		{23999, 48000, 1, "round", 0},
		{24000, 48000, 1, "round", 1},
		{24001, 48000, 1, "round", 1},
	}
	for _, tc := range cases {
		got, err := QuantizeFrame(tc.sample, tc.sampleRate, tc.fps, tc.direction)
		if err != nil {
			t.Fatalf("QuantizeFrame(%v): %v", tc, err)
		}
		if got != tc.want {
			t.Fatalf("QuantizeFrame(%v) = %d, want %d", tc, got, tc.want)
		}
	}
}

func TestQuantizeMonotonicInFPS(t *testing.T) {
	samples := []int64{0, 1, 7999, 12345, 48000, 142000, 999999}
	directions := []string{"ceil", "floor", "round"}
	for _, direction := range directions {
		for _, sample := range samples {
			previous, err := QuantizeFrame(sample, 48000, 1, direction)
			if err != nil {
				t.Fatal(err)
			}
			for fps := 2; fps <= SupportedMaxFPS; fps++ {
				current, err := QuantizeFrame(sample, 48000, fps, direction)
				if err != nil {
					t.Fatal(err)
				}
				if current < previous {
					t.Fatalf("direction=%s sample=%d fps=%d maps to %d, earlier than %d", direction, sample, fps, current, previous)
				}
				previous = current
			}
		}
	}
}

func TestQuantizeOverflowAndNegative(t *testing.T) {
	_, err := QuantizeFrame(math.MaxInt64, 48000, 240, "ceil")
	if ErrorCode(err) != CodeRange {
		t.Fatalf("overflow code = %s, want E_RANGE (%v)", ErrorCode(err), err)
	}
	de, ok := asDomainError(err)
	if !ok || de.Sample == nil || de.FPS == nil {
		t.Fatalf("overflow error lacks frame-rate/sample context: %+v", de)
	}
	_, err = QuantizeFrame(-1, 48000, 24, "ceil")
	if ErrorCode(err) != CodeRange {
		t.Fatalf("negative sample code = %s, want E_RANGE", ErrorCode(err))
	}
}

func TestQuantizeProjectBoundaries(t *testing.T) {
	set := sampleSetForTest(t, 1)
	project := set.Projects[0]
	timing, err := QuantizeProject(project)
	if err != nil {
		t.Fatal(err)
	}
	wantFrames, err := QuantizeFrame(project.TotalSamples(), project.SampleRate, project.FPS, project.Quantization.Direction)
	if err != nil {
		t.Fatal(err)
	}
	if int64(timing.FrameCount) != wantFrames {
		t.Fatalf("frame count = %d, want %d", timing.FrameCount, wantFrames)
	}
	for _, phoneme := range timing.Phonemes {
		start, err := QuantizeFrame(phoneme.StartSample, project.SampleRate, project.FPS, project.Quantization.Direction)
		if err != nil {
			t.Fatal(err)
		}
		end, err := QuantizeFrame(phoneme.EndSample, project.SampleRate, project.FPS, project.Quantization.Direction)
		if err != nil {
			t.Fatal(err)
		}
		if phoneme.StartFrame != start || phoneme.EndFrame != end {
			t.Fatalf("phoneme %s frames = %d..%d, want %d..%d", phoneme.ID, phoneme.StartFrame, phoneme.EndFrame, start, end)
		}
	}
}
