package graph_test

import (
	"reflect"
	"testing"

	"dialogueaudio/generator"
	"dialogueaudio/graph"
)

func TestPreciseInvalidation(t *testing.T) {
	set, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	g, err := graph.Build(set.Project, "test-quant")
	if err != nil {
		t.Fatal(err)
	}
	gain, err := g.Invalidate(graph.Edit{Kind: graph.EditClipGain, ClipID: "c02"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gain.AffectedClips, []string{"c02"}) || len(gain.FrameIntervals) != 1 ||
		len(gain.FrameIntervals[0].FrameIndices) != 12 {
		t.Fatalf("gain invalidation too broad: %+v", gain)
	}
	for _, clip := range gain.ReusedEnvelopeClips {
		if clip == "c02" {
			t.Fatal("c02 envelope was not invalidated")
		}
	}
	target, err := g.Invalidate(graph.Edit{Kind: graph.EditTarget})
	if err != nil {
		t.Fatal(err)
	}
	if len(target.FrameIntervals) != 0 || len(target.AffectedClips) != 6 || len(target.ReusedEnvelopeClips) != 6 {
		t.Fatalf("target invalidation should affect calibration only: %+v", target)
	}
	fps, err := g.Invalidate(graph.Edit{Kind: graph.EditFrameRate})
	if err != nil {
		t.Fatal(err)
	}
	if len(fps.FrameIntervals) != 6 || len(fps.ReusedEnvelopeClips) != 0 {
		t.Fatalf("frame-rate invalidation incomplete: %+v", fps)
	}
}

func TestKeyFormulaSensitivity(t *testing.T) {
	base := graph.KeyFor("content", "frame:1", "q1", []string{"a", "b"})
	if base == graph.KeyFor("content2", "frame:1", "q1", []string{"a", "b"}) {
		t.Fatal("content hash did not affect key")
	}
	if base == graph.KeyFor("content", "frame:2", "q1", []string{"a", "b"}) {
		t.Fatal("interval did not affect key")
	}
	if base == graph.KeyFor("content", "frame:1", "q2", []string{"a", "b"}) {
		t.Fatal("quant version did not affect key")
	}
	if base == graph.KeyFor("content", "frame:1", "q1", []string{"a", "c"}) {
		t.Fatal("upstream key did not affect key")
	}
}
