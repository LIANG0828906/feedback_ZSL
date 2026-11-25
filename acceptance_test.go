package combatjudge_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"combatjudge/internal/engine"
	"combatjudge/internal/model"
	"combatjudge/internal/sample"
	"combatjudge/internal/schema"
	"combatjudge/internal/store"
)

func loadGeneratedVersions(t *testing.T) (map[string]*model.Project, *model.TruthFile, string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := sample.GenerateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	versions := []string{model.SchemaV1, model.SchemaV2, model.SchemaV3}
	projects := map[string]*model.Project{}
	for _, v := range versions {
		name := map[string]string{model.SchemaV1: "project.v1.json", model.SchemaV2: "project.v2.json", model.SchemaV3: "project.v3.json"}[v]
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		p, err := schema.Load(data)
		if err != nil {
			t.Fatalf("load %s: %v", v, err)
		}
		if p.SchemaVersion != v {
			t.Fatalf("source version = %q want %q", p.SchemaVersion, v)
		}
		if len(p.Traces) == 0 && v != model.SchemaV3 {
			t.Fatalf("%s expected conversion/default traces", v)
		}
		projects[v] = p
	}
	truthData, err := os.ReadFile(filepath.Join(dir, "truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	truth, err := schema.LoadTruth(truthData)
	if err != nil {
		t.Fatal(err)
	}
	return projects, truth, dir
}

func TestThreeVersionLoadingAndTruth(t *testing.T) {
	projects, truth, _ := loadGeneratedVersions(t)
	ctx := context.Background()
	var base *engine.Report
	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		p := projects[version]
		r, err := engine.Run(ctx, p, engine.Options{PathLabel: version, DBPath: filepath.Join(t.TempDir(), "judge.db"), MaxParallel: 2})
		if err != nil {
			t.Fatalf("run %s: %v", version, err)
		}
		if base == nil {
			base = r
		} else if mm := engine.CompareReports(base, r); mm != nil {
			t.Fatalf("version %s mismatch: %+v", version, mm)
		}
		assertTruth(t, p, r, truth)
	}
}

func assertTruth(t *testing.T, p *model.Project, r *engine.Report, truth *model.TruthFile) {
	t.Helper()
	frameIndex := map[string]engine.FrameResult{}
	for _, f := range r.Frames {
		frameIndex[fmt.Sprintf("%d/%s", f.Frame, f.SweepID)] = f
	}
	for _, want := range truth.Samples {
		got, ok := frameIndex[fmt.Sprintf("%d/%s", want.Frame, want.SweepID)]
		if !ok {
			t.Fatalf("%s missing frame %d", p.SchemaVersion, want.Frame)
		}
		if want.Hit {
			if got.FirstColliderNo != want.FirstColliderNo || got.FirstPart != want.FirstPart || got.FirstSurface != want.FirstSurface {
				t.Fatalf("%s frame %d first = %d/%s/%s want %d/%s/%s", p.SchemaVersion, want.Frame, got.FirstColliderNo, got.FirstPart, got.FirstSurface, want.FirstColliderNo, want.FirstPart, want.FirstSurface)
			}
		}
		if got.RemainingDamageQ != want.RemainingDamageQ {
			t.Fatalf("%s frame %d damage = %d want %d", p.SchemaVersion, want.Frame, got.RemainingDamageQ, want.RemainingDamageQ)
		}
		diff := got.NearestDistanceQ - want.NearestDistanceQ
		if diff < 0 {
			diff = -diff
		}
		if diff > truth.ToleranceQ {
			t.Fatalf("%s frame %d distance = %d want %d ±%d", p.SchemaVersion, want.Frame, got.NearestDistanceQ, want.NearestDistanceQ, truth.ToleranceQ)
		}
	}

	seqIndex := map[string][]engine.SegmentResult{}
	for _, f := range r.Frames {
		for _, s := range f.Segments {
			if s.Status == "hit" || s.Status == "depleted" {
				seqIndex[f.SweepID] = append(seqIndex[f.SweepID], s)
			}
		}
	}
	for _, wantSeq := range truth.Sequences {
		gotSegs := seqIndex[wantSeq.SweepID]
		if len(gotSegs) != len(wantSeq.Segments) {
			t.Fatalf("%s sweep %s segment count = %d want %d", p.SchemaVersion, wantSeq.SweepID, len(gotSegs), len(wantSeq.Segments))
		}
		for i, want := range wantSeq.Segments {
			got := gotSegs[i]
			if got.Segment != want.Segment || got.Status != want.Status || got.ColliderNo != want.ColliderNo || got.Part != want.Part {
				t.Fatalf("%s seq %s[%d] = %d/%s/%d/%s want %d/%s/%d/%s", p.SchemaVersion, wantSeq.SweepID, i, got.Segment, got.Status, got.ColliderNo, got.Part, want.Segment, want.Status, want.ColliderNo, want.Part)
			}
			if want.Surface != "" && got.Surface != want.Surface {
				t.Fatalf("%s seq %s[%d] surface = %q want %q", p.SchemaVersion, wantSeq.SweepID, i, got.Surface, want.Surface)
			}
			if got.RemainingDamageQ != want.RemainingDamageQ {
				t.Fatalf("%s seq %s[%d] remaining = %d want %d", p.SchemaVersion, wantSeq.SweepID, i, got.RemainingDamageQ, want.RemainingDamageQ)
			}
		}
	}
}

func TestLiveResumeFullPathsMatch(t *testing.T) {
	p := sample.BuildProject()
	dir := t.TempDir()
	ctx := context.Background()

	live, err := engine.Run(ctx, p, engine.Options{PathLabel: "live", SnapshotID: "snap", DBPath: filepath.Join(dir, "live.db"), MaxParallel: 1, MaxQueuedIntervals: 1})
	if err != nil {
		t.Fatal(err)
	}
	full, err := engine.Run(ctx, p, engine.Options{PathLabel: "full", SnapshotID: "snap", DBPath: filepath.Join(dir, "full.db"), MaxParallel: 4, MaxQueuedIntervals: 1})
	if err != nil {
		t.Fatal(err)
	}

	resumeDB := filepath.Join(dir, "resume.db")
	seed, err := engine.Run(ctx, p, engine.Options{PathLabel: "resume-seed", SnapshotID: "snap", DBPath: resumeDB, MaxParallel: 2, StopAfterInterval: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(seed.CommitSequence) != 2 {
		t.Fatalf("seed commit sequence = %v want [1 2]", seed.CommitSequence)
	}
	resumed, err := engine.Run(ctx, p, engine.Options{PathLabel: "breakpoint-resume", SnapshotID: "snap", DBPath: resumeDB, MaxParallel: 2})
	if err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]*engine.Report{"live": live, "resume": resumed} {
		if mm := engine.CompareReports(full, r); mm != nil {
			t.Fatalf("%s mismatch vs full: %+v", name, mm)
		}
	}
	if fmt.Sprint(resumed.CommitSequence) != "[1 2 3 4]" || fmt.Sprint(resumed.WriteOrder) != "[1 2 3 4]" {
		t.Fatalf("resume sequences commit=%v write=%v", resumed.CommitSequence, resumed.WriteOrder)
	}
}

func TestIncrementalInvalidationReusesIntervals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "incremental.db")
	old := sample.BuildProject()
	if _, err := engine.Run(ctx, old, engine.Options{PathLabel: "old", SnapshotID: "snap", DBPath: db}); err != nil {
		t.Fatal(err)
	}

	edited := sample.BuildProject()
	edited.Colliders[2].Min.X = 8_500_000
	plan := engine.PlanEdit(old, edited, engine.DefaultIntervalFrames, engine.EditSpec{Kind: "collider_size", ColliderNo: 3, Field: "min.x"})
	var has3, has4 bool
	for _, in := range plan {
		if in.IntervalNo == 3 && in.Kind == "intersection" {
			has3 = true
		}
		if in.IntervalNo == 4 && in.Kind == "downstream_settlement" {
			has4 = true
		}
	}
	if !has3 || !has4 {
		t.Fatalf("invalidation plan missing interval 3/4: %+v", plan)
	}

	inc, err := engine.Run(ctx, edited, engine.Options{PathLabel: "incremental", SnapshotID: "snap", DBPath: db})
	if err != nil {
		t.Fatal(err)
	}
	if !inc.Voided {
		t.Fatal("edited snapshot should be voided and new generation started")
	}
	if fmt.Sprint(inc.ReusedIntervals) != "[1 2]" {
		t.Fatalf("reused = %v want [1 2]", inc.ReusedIntervals)
	}
	if fmt.Sprint(inc.Recomputed) != "[3 4]" {
		t.Fatalf("recomputed = %v want [3 4]", inc.Recomputed)
	}
	full, err := engine.Run(ctx, edited, engine.Options{PathLabel: "full-edited", SnapshotID: "full", DBPath: filepath.Join(dir, "full-edited.db")})
	if err != nil {
		t.Fatal(err)
	}
	if mm := engine.CompareReports(full, inc); mm != nil {
		t.Fatalf("incremental/full mismatch: %+v", mm)
	}
}

func TestDecodeFailureIsolatesAndRetries(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "fault.db")
	p := sample.BuildProject()
	failed, err := engine.Run(ctx, p, engine.Options{SnapshotID: "fault", DBPath: db, Injection: engine.Injection{CorruptColliderNo: 3}})
	var coded *model.Error
	if !errors.As(err, &coded) || coded.Code != model.CodeDecode {
		t.Fatalf("err=%v want E_DECODE", err)
	}
	if len(failed.Intervals) != 2 || len(failed.Failures) != 1 || failed.Failures[0].IntervalNo != 3 {
		t.Fatalf("failure isolation intervals=%d failures=%+v", len(failed.Intervals), failed.Failures)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := st.CommittedIntervalNumbers("fault", 1)
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(committed) != "[1 2]" {
		t.Fatalf("committed after failure = %v want [1 2]", committed)
	}

	retry, err := engine.Run(ctx, p, engine.Options{SnapshotID: "fault", DBPath: db})
	if err != nil {
		t.Fatal(err)
	}
	full, err := engine.Run(ctx, p, engine.Options{SnapshotID: "full", DBPath: filepath.Join(t.TempDir(), "full.db")})
	if err != nil {
		t.Fatal(err)
	}
	if mm := engine.CompareReports(full, retry); mm != nil {
		t.Fatalf("retry mismatch: %+v", mm)
	}
}

func TestExceptionCodesSchemaStepUnitOverflow(t *testing.T) {
	if _, err := schema.Load([]byte(`{"schema":"nope"}`)); err == nil {
		t.Fatal("unknown version accepted")
	} else {
		var me *model.Error
		if !errors.As(err, &me) || me.Code != model.CodeSchema || me.FieldPath != "$.schema" {
			t.Fatalf("unknown schema err=%v", err)
		}
	}
	if _, err := schema.Load([]byte(`{"schemaVersion":"battleformat-v3"}`)); err == nil {
		t.Fatal("missing battleId accepted")
	} else {
		var me *model.Error
		if !errors.As(err, &me) || me.Code != model.CodeSchema || me.FieldPath != "$.battleId" || me.SourceVersion != model.SchemaV3 {
			t.Fatalf("missing field err=%v", err)
		}
	}
	v1Unit := []byte(`{"Schema":"battleformat-v1","BattleId":"b","UnitScale":10000}`)
	if _, err := schema.Load(v1Unit); err == nil {
		t.Fatal("scale contradiction accepted")
	} else {
		var me *model.Error
		if !errors.As(err, &me) || me.Code != model.CodeUnit {
			t.Fatalf("unit err=%v", err)
		}
	}
	p := sample.BuildProject()
	if _, err := engine.Run(context.Background(), p, engine.Options{Injection: engine.Injection{UseBadStep: true, BadStepMS: 0}}); err == nil {
		t.Fatal("zero step accepted")
	} else {
		var me *model.Error
		if !errors.As(err, &me) || me.Code != model.CodeStep {
			t.Fatalf("step err=%v", err)
		}
	}
	if _, err := engine.Run(context.Background(), p, engine.Options{Injection: engine.Injection{UnitContradiction: true}}); err == nil {
		t.Fatal("unit injection accepted")
	} else {
		var me *model.Error
		if !errors.As(err, &me) || me.Code != model.CodeUnit {
			t.Fatalf("injected unit err=%v", err)
		}
	}
}
