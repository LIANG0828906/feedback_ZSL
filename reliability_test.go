package downsample

import (
	"path/filepath"
	"testing"
)

func TestSequentialConfigEditsMatchFull(t *testing.T) {
	bundle := testBundle(t)
	incProject, _ := LoadProject(bundle.Documents[SchemaV1])
	fullProject, _ := LoadProject(bundle.Documents[SchemaV1])
	inc := NewEngine(incProject, DefaultBudget())
	if _, err := inc.RecomputeAll(InvalidationSource{Kind: "initial"}); err != nil {
		t.Fatal(err)
	}
	edits := []ConfigEdit{
		{MetricID: "mem.usage", Field: "window_width", WidthMS: 30_000},
		{MetricID: "cpu.usage", Field: "alignment", Alignment: SourceAlignFirstPoint, AnchorMS: bundle.BaseTimeMS},
		{MetricID: "cpu.usage", Field: "functions", Functions: []Func{FuncAvg, FuncSum, FuncMin, FuncMax, FuncWeightedAvg, FuncFirst}},
		{MetricID: "cpu.usage", Field: "weight_scale", WeightScale: ScalePermille, WeightBits: 32},
	}
	var last *RecomputeReport
	for _, edit := range edits {
		report, err := inc.ApplyConfigEdit(edit)
		if err != nil {
			t.Fatal(err)
		}
		last = report
	}

	full := NewEngine(fullProject, DefaultBudget())
	for _, edit := range edits {
		if _, err := full.ApplyConfigEdit(edit); err != nil {
			t.Fatal(err)
		}
	}
	fullReport, err := full.RecomputeAll(InvalidationSource{Kind: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if diffs := CompareWindowMaps(fullReport.Results, last.Results, 1e-9); len(diffs) != 0 {
		t.Fatalf("sequential edit mismatch: %v", diffs)
	}
}

func TestFailedConfigEditKeepsCommittedWindow(t *testing.T) {
	bundle := testBundle(t)
	project, _ := LoadProject(bundle.Documents[SchemaV1])
	engine := NewEngine(project, DefaultBudget())
	if _, err := engine.RecomputeAll(InvalidationSource{Kind: "initial"}); err != nil {
		t.Fatal(err)
	}
	before, err := engine.QueryWindow("cpu.usage", bundle.BaseTimeMS)
	if err != nil {
		t.Fatal(err)
	}
	_, coded := engine.ApplyConfigEdit(ConfigEdit{MetricID: "cpu.usage", Field: "window_width", WidthMS: 0})
	if !IsCode(coded, CodeWindowWidthZero) {
		t.Fatalf("expected width code, got %v", coded)
	}
	after, qerr := engine.QueryWindow("cpu.usage", bundle.BaseTimeMS)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if after.PointCount != before.PointCount || after.Values[FuncAvg] != before.Values[FuncAvg] {
		t.Fatalf("committed window changed after rejected edit")
	}
}

func TestConfigEditCheckpointRecoverySeesReusedWindows(t *testing.T) {
	bundle := testBundle(t)
	project, _ := LoadProject(bundle.Documents[SchemaV1])
	path := filepath.Join(t.TempDir(), "edit-checkpoint.sqlite")
	session, coded := NewSession(project, DefaultBudget(), path)
	if coded != nil {
		t.Fatal(coded)
	}
	report, coded := session.Engine.ApplyConfigEdit(ConfigEdit{
		MetricID: "cpu.usage", Field: "functions",
		Functions: []Func{FuncAvg, FuncSum, FuncMin, FuncMax, FuncWeightedAvg, FuncFirst},
	})
	if coded != nil {
		t.Fatal(coded)
	}
	if coded = session.PersistConfigReport(report); coded != nil {
		t.Fatal(coded)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	restartProject, _ := LoadProject(bundle.Documents[SchemaV1])
	restartProject.ConfigVersion = report.ConfigVersion
	recovered, _, coded := RecoverOrInitialize(restartProject, DefaultBudget(), path)
	if coded != nil {
		t.Fatal(coded)
	}
	defer recovered.Close()
	cpu, qerr := recovered.Engine.QueryWindow("cpu.usage", bundle.BaseTimeMS)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if _, ok := cpu.Values[FuncFirst]; !ok {
		t.Fatalf("new function result missing after recovery")
	}
	mem, qerr := recovered.Engine.QueryWindow("mem.usage", bundle.BaseTimeMS)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if mem.PointCount == 0 {
		t.Fatalf("reused mem window was not persisted under new config version")
	}
}

func TestVersionMismatchTriggersFullRecompute(t *testing.T) {
	bundle := testBundle(t)
	project, _ := LoadProject(bundle.Documents[SchemaV1])
	path := filepath.Join(t.TempDir(), "version.sqlite")
	session, coded := NewSession(project, DefaultBudget(), path)
	if coded != nil {
		t.Fatal(coded)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	nextProject, _ := LoadProject(bundle.Documents[SchemaV1])
	nextProject.ConfigVersion = "cfg-v-unexpected"
	recomputed, recovery, coded := RecoverOrInitialize(nextProject, DefaultBudget(), path)
	if !IsCode(coded, CodeCheckpointVersionMismatch) {
		t.Fatalf("expected mismatch notice, got recovery=%v err=%v", recovery, coded)
	}
	if recomputed == nil {
		t.Fatalf("expected recomputed session after mismatch")
	}
	defer recomputed.Close()
	result, qerr := recomputed.Engine.QueryWindow("cpu.usage", bundle.BaseTimeMS)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if result.PointCount != 3 {
		t.Fatalf("full recompute after mismatch count=%d", result.PointCount)
	}
}

func TestAnchorChangeDecision(t *testing.T) {
	if got := AnchorChangeDecision(100, 200, 150); got == "NO_CROSSING" {
		t.Fatalf("expected crossing decision, got %s", got)
	}
	if got := AnchorChangeDecision(100, 200, 100); got != "CHANGE_ON_LEFT_BOUNDARY_USE_NEW_GRID" {
		t.Fatalf("left boundary decision = %s", got)
	}
	if got := AnchorChangeDecision(100, 200, 200); got != "CHANGE_ON_RIGHT_BOUNDARY_USE_OLD_GRID" {
		t.Fatalf("right boundary decision = %s", got)
	}
}
