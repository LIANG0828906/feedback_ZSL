package audiofp_test

import (
	"reflect"
	"testing"

	"audiofp"
	"audiofp/samplegen"
)

func TestFingerprintsDeterministicAcrossConcurrency(t *testing.T) {
	m := samplegen.GenerateDefault()
	params := audiofp.DefaultParams()

	var expected []audiofp.Fingerprint
	for _, workers := range []int{1, 2, 4, 8} {
		for _, queue := range []int{0, 1, 3, 64} {
			limits := audiofp.DefaultResourceLimits()
			limits.MaxWorkers = workers
			limits.MaxQueuedJobs = queue
			frames, err := audiofp.ExtractFrames(m.PCM.Samples, m.PCM.SampleRate, params, limits)
			if err != nil {
				t.Fatalf("workers=%d queue=%d: %v", workers, queue, err)
			}
			if expected == nil {
				expected = frames
				continue
			}
			if !reflect.DeepEqual(expected, frames) {
				t.Fatalf("fingerprints differ for workers=%d queue=%d", workers, queue)
			}
		}
	}

	for i, fp := range expected {
		if fp.FrameIndex != i {
			t.Fatalf("frame %d has index %d", i, fp.FrameIndex)
		}
		if fp.StartSample != i*params.FrameShift {
			t.Fatalf("frame %d bad start %d", i, fp.StartSample)
		}
		if got := fp.Digest(); got == "" || len(got) != 64 {
			t.Fatalf("frame %d bad digest %q", i, got)
		}
	}
}

func TestFingerprintVersionsStable(t *testing.T) {
	p1 := audiofp.DefaultParams()
	p2 := audiofp.DefaultParams()
	if p1.Version() != p2.Version() || p1.FeatureVersion() != p2.FeatureVersion() {
		t.Fatalf("identical params must have stable versions")
	}
	p3 := p1
	p3.BandCount++
	if p3.FeatureVersion() == p1.FeatureVersion() {
		t.Fatalf("band count change must alter feature version")
	}
	p4 := p1
	p4.GapToleranceSamples += 128
	if p4.Version() == p1.Version() {
		t.Fatalf("gap change must alter full parameter version")
	}
	if p4.FeatureVersion() != p1.FeatureVersion() {
		t.Fatalf("gap must not alter feature version")
	}
}
