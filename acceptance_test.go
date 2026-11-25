package maskqc

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func generatedProject(t *testing.T, index int) (*SampleSet, string) {
	t.Helper()
	set, err := GenerateSampleSet(FixedSeed)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	root := t.TempDir()
	if err := set.WriteTo(root); err != nil {
		t.Fatalf("write samples: %v", err)
	}
	return set, root
}

func TestGeneratedSamplesArePixelExact(t *testing.T) {
	set, _ := generatedProject(t, 0)
	for _, p := range set.Projects {
		p := p
		t.Run(p.ID, func(t *testing.T) {
			engine := NewEngine(p)
			result, err := engine.RunFullRecompute()
			if err != nil {
				t.Fatal(err)
			}
			if !result.PixelExact || !result.AlphaExact {
				mismatches := 0
				for i := range result.Mask.Alpha {
					a := result.Mask.Alpha[i] >= ForegroundThreshold
					b := p.Truth.Reference.Alpha[i] >= ForegroundThreshold
					if a != b || result.Mask.Alpha[i] != p.Truth.Reference.Alpha[i] {
						mismatches++
					}
				}
				t.Fatalf("mask mismatch: pixelExact=%v alphaExact=%v mismatches=%d topo=%+v", result.PixelExact, result.AlphaExact, mismatches, result.Topology.Error)
			}
			if !result.Topology.OK {
				t.Fatalf("topology failed: %+v", result.Topology.Error)
			}
			if !result.Directions.OK {
				b, _ := json.MarshalIndent(result.Directions.Segments, "", "  ")
				t.Fatalf("directions failed: %s", b)
			}
		})
	}
}

func TestThreeSchemaVersionsLoadAndVerify(t *testing.T) {
	set, root := generatedProject(t, 0)
	for _, original := range set.Projects {
		for _, version := range []string{"v1", "v2", "v3"} {
			path := filepath.Join(root, original.ID, "project-"+version+".json")
			p, err := LoadProjectFile(path)
			if err != nil {
				t.Fatalf("load %s %s: %v", original.ID, version, err)
			}
			result, err := NewEngine(p).RunFullRecompute()
			if err != nil {
				t.Fatalf("verify %s %s: %v", original.ID, version, err)
			}
			if !result.AlphaExact {
				diff := 0
				first := ""
				for i := range result.Mask.Alpha {
					if result.Mask.Alpha[i] != p.Truth.Reference.Alpha[i] {
						diff++
						if first == "" {
							first = filepath.Join(root, original.ID)
						}
					}
				}
				t.Fatalf("%s %s not alpha exact diff=%d firstDir=%s residual=%s", original.ID, version, diff, first, p.ConvertReport.ResidualDigest())
			}
			if !result.Topology.OK || !result.Directions.OK {
				t.Fatalf("%s %s verification failed topo=%v dir=%v", original.ID, version, result.Topology.OK, result.Directions.OK)
			}
		}
	}
}

func TestIncrementalResumeFullAreIdentical(t *testing.T) {
	set, root := generatedProject(t, 0)
	for _, original := range set.Projects {
		path := filepath.Join(root, original.ID, "project-v3.json")
		p, err := LoadProjectFile(path)
		if err != nil {
			t.Fatal(err)
		}

		incDB := filepath.Join(t.TempDir(), "incremental.db")
		incStore, err := OpenCheckpoint(incDB)
		if err != nil {
			t.Fatal(err)
		}
		inc, err := NewEngine(p).RunSequential(RunOptions{Path: "incremental", Store: incStore, Commit: true})
		if err != nil {
			t.Fatal(err)
		}
		incSeq, _ := incStore.Sequence(p.ID)
		_ = incStore.Close()

		breakDB := filepath.Join(t.TempDir(), "breakpoint.db")
		prefixStore, err := OpenCheckpoint(breakDB)
		if err != nil {
			t.Fatal(err)
		}
		engine := NewEngine(p)
		if _, err := engine.RunPrefix(prefixStore, len(p.Strokes)/2); err != nil {
			t.Fatal(err)
		}
		if err := prefixStore.Close(); err != nil {
			t.Fatal(err)
		}
		resumeStore, err := OpenCheckpoint(breakDB)
		if err != nil {
			t.Fatal(err)
		}
		resume, err := NewEngine(p).Resume(resumeStore)
		if err != nil {
			t.Fatal(err)
		}
		resumeSeq, _ := resumeStore.Sequence(p.ID)
		_ = resumeStore.Close()

		full, err := NewEngine(p).RunFullRecompute()
		if err != nil {
			t.Fatal(err)
		}

		if !bytesEqual(inc.Mask.Alpha, resume.Mask.Alpha) || !bytesEqual(inc.Mask.Alpha, full.Mask.Alpha) {
			t.Fatalf("%s: three path masks differ", original.ID)
		}
		if !reflect.DeepEqual(inc.Sequence, resume.Sequence) || !reflect.DeepEqual(inc.Sequence, full.Sequence) {
			t.Fatalf("%s: in-memory sequences differ inc=%v resume=%v full=%v", original.ID, inc.Sequence, resume.Sequence, full.Sequence)
		}
		if !reflect.DeepEqual(incSeq, resumeSeq) || !reflect.DeepEqual(incSeq, full.Sequence) {
			t.Fatalf("%s: checkpoint sequences differ dbInc=%v dbResume=%v full=%v", original.ID, incSeq, resumeSeq, full.Sequence)
		}
		incTopo := mustJSON(t, inc.Topology)
		resumeTopo := mustJSON(t, resume.Topology)
		fullTopo := mustJSON(t, full.Topology)
		if incTopo != resumeTopo || incTopo != fullTopo {
			t.Fatalf("%s: topology reports differ", original.ID)
		}
		incDir := mustJSON(t, inc.Directions)
		resumeDir := mustJSON(t, resume.Directions)
		fullDir := mustJSON(t, full.Directions)
		if incDir != resumeDir || incDir != fullDir {
			t.Fatalf("%s: direction reports differ", original.ID)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCoordinateAndWeightIntegerRoundTrip(t *testing.T) {
	report := &ConversionReport{}
	p := Point{X: 17, Y: 9}
	off, err := OffsetFromAbsolute(p, 3, "stroke.start", report)
	if err != nil {
		t.Fatal(err)
	}
	back, err := AbsoluteFromOffset(off, 3, "stroke.start", report)
	if err != nil {
		t.Fatal(err)
	}
	if back != p {
		t.Fatalf("coordinate round trip: %+v -> %+v -> %+v", p, off, back)
	}
	w, _, err := ConvertScale(2500, 10000, 1000, RoundFloor, "weight", "$.weight", report)
	if err != nil || w != 250 {
		t.Fatalf("bp to permille: %d %v", w, err)
	}
	backW, _, err := ConvertScale(int64(w), 1000, 10000, RoundFloor, "weight", "$.weight", report)
	if err != nil || backW != 2500 {
		t.Fatalf("permille round trip: %d %v", backW, err)
	}
	if report.ResidualDigest() == "" {
		t.Fatal("missing residual digest")
	}
}

func TestSchemaErrorsContainPaths(t *testing.T) {
	_, err := LoadProjectJSON([]byte(`{"schema":"qc.nope.v9"}`), ".")
	assertCodeField(t, err, CodeSchema, "$.schema")

	_, err = LoadProjectJSON([]byte(`{"schema":"qc.strokes.v1","canvas":{"w":32,"h":32}}`), ".")
	assertCodeField(t, err, CodeSchema, "$.icon_id")
}

func assertCodeField(t *testing.T, err error, code, field string) {
	t.Helper()
	qerr, ok := err.(*QCError)
	if !ok {
		t.Fatalf("expected *QCError, got %T %v", err, err)
	}
	if qerr.Code != code || qerr.Path != field {
		t.Fatalf("expected %s at %s, got %+v", code, field, qerr)
	}
}

func TestEditInvalidatesOnlyIntersectingTiles(t *testing.T) {
	set, _ := generatedProject(t, 0)
	p := set.Projects[0]
	before := len(Tiles(p.Width, p.Height, DefaultTileSize))
	plan, err := PlanEdit(p, EditRequest{Type: "update", StrokeID: "ring-top", Start: &Point{X: 8, Y: 9}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.RecomputeTiles) == 0 || len(plan.RecomputeTiles) >= before {
		t.Fatalf("expected subset of %d tiles, got %v", before, plan.RecomputeTiles)
	}
	if len(plan.ChangedStrokeIDs) != 1 {
		t.Fatalf("changed ids: %v", plan.ChangedStrokeIDs)
	}
}

func TestSchedulerIsolatesFailureAndRecordsWait(t *testing.T) {
	sched := NewScheduler(1, 64, 0)
	block := make(chan struct{})
	started := make(chan struct{}, 2)
	jobs := []Job{
		{Index: 0, ID: "blocker", TileID: "t0", Pixels: 8, Fn: func([]byte) error { started <- struct{}{}; <-block; return nil }},
		{Index: 1, ID: "panicked", TileID: "t1", Pixels: 8, Fn: func([]byte) error { started <- struct{}{}; panic("boom") }},
		{Index: 2, ID: "ok", TileID: "t2", Pixels: 8, Fn: func([]byte) error { return nil }},
	}
	done := make(chan []JobResult, 1)
	go func() { done <- sched.Run(jobs) }()
	<-started
	close(block)
	results := <-done
	if results[1].OK || !results[1].Panic {
		t.Fatalf("expected isolated panic: %+v", results[1])
	}
	if !results[2].OK {
		t.Fatalf("later job should run: %+v", results[2])
	}
	if len(sched.Metrics.Waits) == 0 {
		t.Fatal("expected queue wait record under zero budget / single worker")
	}
}
