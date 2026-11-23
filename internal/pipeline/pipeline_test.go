package pipeline_test

import (
	"testing"

	"captionx/internal/checkpoint"
	"captionx/internal/errs"
	"captionx/internal/loader"
	"captionx/internal/model"
	"captionx/internal/pipeline"
	"captionx/internal/samplegen"
	"captionx/internal/segment"
)

func loadProject(t *testing.T) *loader.Project {
	t.Helper()
	dirs, err := samplegen.Generate(samplegen.Options{OutDir: t.TempDir(), Count: 1, Seed: 20261124})
	if err != nil {
		t.Fatal(err)
	}
	p, err := loader.Load(dirs[0])
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestThreePathsConsistent is the acceptance core: realtime processing
// (interrupted + resumed through checkpoints), pure checkpoint replay
// and one-shot full recomputation must agree exactly.
func TestThreePathsConsistent(t *testing.T) {
	p := loadProject(t)
	store, err := checkpoint.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Realtime path, interrupted after two commits.
	part, err := pipeline.RunChunked(p, pipeline.Options{Store: store, StopAfterCommits: 2})
	if err != nil {
		t.Fatal(err)
	}
	if part.Complete {
		t.Fatal("expected interrupted run")
	}
	if c, _ := part.Machine.Progress(); c != 2 {
		t.Fatalf("committed = %d, want 2", c)
	}
	rt, err := pipeline.RunChunked(p, pipeline.Options{Store: store, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rt.Complete {
		t.Fatal("resumed run incomplete")
	}

	// Pure replay from the persisted checkpoint.
	res, err := pipeline.RunChunked(p, pipeline.Options{Store: store, Resume: true})
	if err != nil {
		t.Fatal(err)
	}

	// One-shot full recompute.
	full, err := pipeline.RunFull(p, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := pipeline.Compare(rt.Result, res.Result); err != nil {
		t.Fatalf("realtime vs replay: %v", err)
	}
	if err := pipeline.Compare(rt.Result, full); err != nil {
		t.Fatalf("realtime vs full: %v", err)
	}
}

// TestCommittedChunksFrozen verifies that a reading-parameter override
// only affects uncommitted chunks.
func TestCommittedChunksFrozen(t *testing.T) {
	p := loadProject(t)
	store, _ := checkpoint.NewStore(t.TempDir())

	if _, err := pipeline.RunChunked(p, pipeline.Options{Store: store, StopAfterCommits: 1}); err != nil {
		t.Fatal(err)
	}

	// Override speech rate for the remaining chunks.
	override := p.Manifest.Reading
	override.SpeechRateNum = override.SpeechRateNum * 2
	out, err := pipeline.RunChunked(p, pipeline.Options{
		Store: store, Resume: true, Reading: &override,
	})
	if err != nil {
		t.Fatal(err)
	}
	full, err := pipeline.RunFull(p, nil)
	if err != nil {
		t.Fatal(err)
	}

	chunkSamples := p.Manifest.SegmentChunkSamples
	changed, frozen := 0, 0
	for i, cr := range out.Result.Captions {
		inCommitted := segment.ChunkOf(full.Captions[i].Window.Start, chunkSamples) == 0
		if inCommitted {
			frozen++
			if cr.Window != full.Captions[i].Window {
				t.Fatalf("committed caption %d window changed: %+v vs %+v",
					cr.CaptionID, cr.Window, full.Captions[i].Window)
			}
		} else if cr.Window != full.Captions[i].Window {
			changed++
		}
	}
	if frozen == 0 {
		t.Fatal("no committed captions to check")
	}
	if changed == 0 {
		t.Fatal("override had no effect on uncommitted chunks")
	}
}

// TestBoundViolation surfaces E_BOUND when the measured window deviation
// exceeds the observed-parameter bound (truth corrupted on purpose).
func TestBoundViolation(t *testing.T) {
	p := loadProject(t)
	p.Manifest.Truth.Captions[0].Start += 100 * p.Manifest.Frame.FrameHop
	_, err := pipeline.RunFull(p, nil)
	if errs.Code(err) != errs.EBound {
		t.Fatalf("code = %s, want E_BOUND (%v)", errs.Code(err), err)
	}
	e := err.(*errs.Error)
	if e.Term != "windowDeviation" || !e.HasCap {
		t.Fatalf("E_BOUND must name term and caption: %+v", e)
	}
}

// TestDiffDetection checks that Compare locates divergences with E_DIFF
// and decision-sequence divergence with E_DECIDE.
func TestDiffDetection(t *testing.T) {
	p := loadProject(t)
	a, err := pipeline.RunFull(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	override := p.Manifest.Reading
	override.SpeechRateDen *= 2 // slower speech: longer windows, no trade-off decisions
	b, err := pipeline.RunFull(p, &override)
	if err != nil {
		t.Fatal(err)
	}
	err = pipeline.Compare(a, b)
	if errs.Code(err) != errs.EDiff {
		t.Fatalf("code = %s, want E_DIFF (%v)", errs.Code(err), err)
	}
	e := err.(*errs.Error)
	if !e.HasCap || e.Field == "" {
		t.Fatalf("E_DIFF must locate caption and field: %+v", e)
	}

	// Decision-sequence mismatch -> E_DECIDE.
	c := *a
	c.Decisions = append(c.Decisions, model.Decision{CaptionID: 1, Kind: model.DecisionExtend, Reason: "x"})
	if err := pipeline.Compare(a, &c); errs.Code(err) != errs.EDecide {
		t.Fatalf("code = %s, want E_DECIDE", errs.Code(err))
	}
}

// TestWindowsWithinTolerance validates derived windows against the
// analytic ground truth within the declared tolerance.
func TestWindowsWithinTolerance(t *testing.T) {
	p := loadProject(t)
	res, err := pipeline.RunFull(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	tol := p.Manifest.ToleranceSamples
	truth := map[int]model.CaptionTruth{}
	for _, tr := range p.Manifest.Truth.Captions {
		truth[tr.CaptionID] = tr
	}
	for _, c := range res.Captions {
		tr := truth[c.CaptionID]
		if d := c.Window.Start - tr.Start; d < -tol || d > tol {
			t.Fatalf("caption %d start deviation %d > tolerance %d", c.CaptionID, d, tol)
		}
		if d := c.Window.End - tr.End; d < -tol || d > tol {
			t.Fatalf("caption %d end deviation %d > tolerance %d", c.CaptionID, d, tol)
		}
		// Character schedule: contiguous, inside the window.
		if c.CharStarts[0] != c.Window.Start || c.CharEnds[len(c.CharEnds)-1] != c.Window.End {
			t.Fatalf("caption %d schedule does not span its window", c.CaptionID)
		}
		for i := 0; i+1 < len(c.CharStarts); i++ {
			if c.CharEnds[i] != c.CharStarts[i+1] {
				t.Fatalf("caption %d char %d: gap or overlap", c.CaptionID, i)
			}
		}
	}
}
