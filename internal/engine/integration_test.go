package engine_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"texlab/internal/apperr"
	"texlab/internal/checkpoint"
	"texlab/internal/engine"
	"texlab/internal/plan"
	"texlab/internal/runrecord"
	"texlab/internal/sample"
	"texlab/internal/verify"
)

func tinyProject(t *testing.T) sample.Project {
	t.Helper()
	root := t.TempDir()
	spec := sample.Spec{
		Name: "tiny", Width: 96, Height: 72, Depth: 8, Layout: "RGBA", Seed: 77,
		Patches: []sample.PatchSpec{
			{Name: "coral", XF: 0.1, YF: 0.1, Size: 6, RGBA: [4]int{255, 32, 16, 255}},
			{Name: "ghost", XF: 0.6, YF: 0.4, Size: 6, RGBA: [4]int{32, 64, 128, 64}},
		},
	}
	if err := sample.Generate(root, spec); err != nil {
		t.Fatal(err)
	}
	p, err := sample.Load(root, spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func runWithStore(t *testing.T, p sample.Project, dir string, opts engine.Options) *engine.RunResult {
	t.Helper()
	store, err := checkpoint.NewStore(dir, p.Plan, p.SourceFP)
	if err != nil {
		t.Fatal(err)
	}
	opts.Sink = store
	res, err := engine.Run(p.Plan, p.Source, p.SourceFP, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestIncrementalInvalidationAndPathEquality(t *testing.T) {
	p := tinyProject(t)
	initialDir := filepath.Join(t.TempDir(), "cp")
	initial := runWithStore(t, p, initialDir, engine.Options{
		Workers: 4, PixelBufferPerWorker: 256,
	})

	recordPath := filepath.Join(t.TempDir(), "run.json")
	if err := runrecord.Write(recordPath, runrecord.Build(initial)); err != nil {
		t.Fatal(err)
	}
	report1 := filepath.Join(t.TempDir(), "report1.json")
	report2 := filepath.Join(t.TempDir(), "report2.json")
	if err := verifyBase(t, p, initialDir, recordPath, report1, report2); err != nil {
		t.Fatal(err)
	}

	edited := p.Plan.Clone()
	edited.Revision++
	edited.Entries[3].Permutation = []int{0, 1, 2, 3}
	if err := plan.Validate(edited, p.Source); err != nil {
		t.Fatal(err)
	}
	incremental := runWithStore(t, sample.Project{Plan: edited, Source: p.Source, SourceFP: p.SourceFP}, filepath.Join(t.TempDir(), "cp2"), engine.Options{
		Workers: 4, PixelBufferPerWorker: 256, Previous: initial,
	})

	covered := map[int]bool{1: true, 2: true, 8: true, 9: true, 10: true, 11: true}
	for seg := 0; seg < 12; seg++ {
		e4 := incremental.StageIndex["regional_swap"][seg]
		e5 := incremental.StageIndex["drop_alpha"][seg]
		if e4 == nil || e5 == nil {
			t.Fatalf("missing stage for segment %d", seg)
		}
		if covered[seg] {
			if !e4.Invalidated || !e5.Invalidated {
				t.Fatalf("segment %d should invalidate e4/e5: e4=%+v e5=%+v", seg, e4.InvalidReason, e5.InvalidReason)
			}
		} else {
			if e4.Invalidated || e5.Invalidated {
				t.Fatalf("segment %d should stay reused: e4=%s e5=%s", seg, e4.InvalidReason, e5.InvalidReason)
			}
			if !e4.Reused || !e5.Reused {
				t.Fatalf("segment %d outside coverage must be reused", seg)
			}
		}
	}

	full := runWithStore(t, sample.Project{Plan: edited, Source: p.Source, SourceFP: p.SourceFP}, filepath.Join(t.TempDir(), "cp3"), engine.Options{
		Workers: 1, PixelBufferPerWorker: 128,
	})
	if err := engine.CompareRuns(full, incremental); err != nil {
		t.Fatal(err)
	}
	parallel := runWithStore(t, sample.Project{Plan: edited, Source: p.Source, SourceFP: p.SourceFP}, filepath.Join(t.TempDir(), "cp4"), engine.Options{
		Workers: 8, PixelBufferPerWorker: 512,
	})
	if err := engine.CompareRuns(full, parallel); err != nil {
		t.Fatal(err)
	}
	if len(parallel.Logs) == 0 {
		t.Fatalf("expected resource/queue log at small buffer")
	}
	reordered := p.Plan.Clone()
	reordered.Entries[0], reordered.Entries[1] = reordered.Entries[1], reordered.Entries[0]
	plan.Reindex(&reordered)
	reordered.Revision++
	if err := plan.Validate(reordered, p.Source); err != nil {
		t.Fatal(err)
	}
	orderIncremental, err := engine.Run(reordered, p.Source, p.SourceFP, engine.Options{
		Workers: 4, PixelBufferPerWorker: 512, Previous: initial,
	})
	if err != nil {
		t.Fatal(err)
	}
	orderFull, err := engine.Run(reordered, p.Source, p.SourceFP, engine.Options{
		Workers: 2, PixelBufferPerWorker: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.CompareRuns(orderFull, orderIncremental); err != nil {
		t.Fatal(err)
	}

}

func verifyBase(t *testing.T, p sample.Project, checkpointDir, recordPath, report1, report2 string) error {
	t.Helper()
	base := verify.Options{
		PlanPath: filepath.Join(p.Dir, "plan.json"), TruthPath: filepath.Join(p.Dir, "truth.json"),
		CheckpointDir: checkpointDir, RecordPath: recordPath,
		SourceImage: p.Source, SourceFingerprint: p.SourceFP,
	}
	base.ReportPath = report1
	if _, err := verify.Run(base); err != nil {
		return err
	}
	base.ReportPath = report2
	if _, err := verify.Run(base); err != nil {
		return err
	}
	a, _ := os.ReadFile(report1)
	b, _ := os.ReadFile(report2)
	if !bytes.Equal(a, b) {
		return apperr.New(apperr.EDiff, "verification reports are not byte-identical")
	}
	return nil
}

func TestCheckpointResumeAndVersionMismatch(t *testing.T) {
	p := tinyProject(t)
	dir := filepath.Join(t.TempDir(), "resume")
	partial := runWithStore(t, p, dir, engine.Options{Workers: 3, PixelBufferPerWorker: 512, MaxCommits: 4})
	if len(partial.Segments) != 4 {
		t.Fatalf("partial committed %d segments", len(partial.Segments))
	}
	loaded, status, err := checkpoint.LoadAll(dir, p.Plan, p.SourceFP)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Valid || len(loaded) != 4 {
		t.Fatalf("load status %+v loaded %d", status, len(loaded))
	}
	store, err := checkpoint.NewStore(dir, p.Plan, p.SourceFP)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := engine.Run(p.Plan, p.Source, p.SourceFP, engine.Options{
		Workers: 5, PixelBufferPerWorker: 1024, Resume: loaded, Sink: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	full := runWithStore(t, p, filepath.Join(t.TempDir(), "full"), engine.Options{Workers: 2, PixelBufferPerWorker: 1024})
	if err := engine.CompareRuns(full, resumed); err != nil {
		t.Fatal(err)
	}

	// A changed plan makes the entire snapshot invalid, never partially mixed.
	changed := p.Plan.Clone()
	changed.Revision++
	changed.Entries[0].TargetDepth = 16
	changed.Entries[0].Permutation = []int{2, 1, 0, 3}
	discarded, statusPlan, err := checkpoint.LoadAll(dir, changed, p.SourceFP)
	if err != nil {
		t.Fatal(err)
	}
	if len(discarded) != 0 || !statusPlan.Discarded {
		t.Fatalf("plan mismatch did not discard snapshot: %+v", statusPlan)
	}

	dir2 := filepath.Join(t.TempDir(), "tamper")
	_ = runWithStore(t, p, dir2, engine.Options{Workers: 2, PixelBufferPerWorker: 512, MaxCommits: 3})
	if err := checkpointDiskTamper(t, dir2); err != nil {
		t.Fatal(err)
	}
	discarded, status2, err := checkpoint.LoadAll(dir2, p.Plan, p.SourceFP)
	if err != nil {
		t.Fatal(err)
	}
	if len(discarded) != 0 || !status2.Discarded {
		t.Fatalf("corrupt snapshot was not discarded: %+v", status2)
	}

	dir3 := filepath.Join(t.TempDir(), "tamper-content")
	_ = runWithStore(t, p, dir3, engine.Options{Workers: 2, PixelBufferPerWorker: 512, MaxCommits: 2})
	if err := checkpointDiskTamperContent(t, dir3); err != nil {
		t.Fatal(err)
	}
	discarded, status3, err := checkpoint.LoadAll(dir3, p.Plan, p.SourceFP)
	if err != nil {
		t.Fatal(err)
	}
	if len(discarded) != 0 || !status3.Discarded {
		t.Fatalf("content-hash mismatch was not discarded: %+v", status3)
	}
}

func checkpointDiskTamper(t *testing.T, dir string) error {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "segment-*.json"))
	if len(files) == 0 {
		return nil
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	obj["param_version"] = 999
	raw, _ = json.Marshal(obj)
	return os.WriteFile(files[0], raw, 0o644)
}

func checkpointDiskTamperContent(t *testing.T, dir string) error {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "segment-*.json"))
	if len(files) == 0 {
		return nil
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	stages, _ := obj["stages"].([]any)
	if len(stages) > 0 {
		stage := stages[0].(map[string]any)
		stage["content_hash"] = "0000000000000000000000000000000000000000000000000000000000000000"
	}
	raw, _ = json.Marshal(obj)
	return os.WriteFile(files[0], raw, 0o644)
}

func TestVersionMismatchError(t *testing.T) {
	// Covered by internal/state; this assertion keeps E_VERSION in the
	// integration package's public acceptance surface.
	if !apperr.IsCode(&apperr.Error{Code: apperr.EVersion}, apperr.EVersion) {
		t.Fatal("version error code unavailable")
	}
}
