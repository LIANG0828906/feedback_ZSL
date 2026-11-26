package budget

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testFixture(t *testing.T) *Fixture {
	t.Helper()
	fixture, err := GenerateFixture(DefaultSeed)
	if err != nil {
		t.Fatalf("GenerateFixture returned error: %v", err)
	}
	return fixture
}

func TestThreeSchemaVersionsLoadAndMatchTruth(t *testing.T) {
	fixture := testFixture(t)
	versions := []SourceVersion{SourceV1, SourceV2, SourceV3}
	for _, version := range versions {
		version := version
		t.Run(string(version), func(t *testing.T) {
			doc, err := RenderDocument(fixture.Project, version)
			if err != nil {
				t.Fatal(err)
			}
			project, err := LoadDocument(doc)
			if err != nil {
				t.Fatalf("LoadDocument(%s): %v", version, err)
			}
			if project.SourceVersion != version {
				t.Fatalf("source version = %q, want %q", project.SourceVersion, version)
			}
			if len(project.Traces) == 0 {
				t.Fatal("field conversion traces are empty")
			}
			report, err := ReferenceSchedule(project, DefaultParams())
			if err != nil {
				t.Fatal(err)
			}
			if err := CompareReports(fixture.Truth, report); err != nil {
				t.Fatalf("version %s differs from truth: %v", version, err)
			}
		})
	}
}

func TestSchemaUnknownVersionAndMissingField(t *testing.T) {
	_, err := LoadDocument([]byte(`{"schema":"combat-budget/v9"}`))
	be, ok := AsError(err)
	if !ok || be.Code != ECodeSchema {
		t.Fatalf("unknown version error = %v, want E_SCHEMA", err)
	}

	fixture := testFixture(t)
	doc, err := RenderDocument(fixture.Project, SourceV1)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(doc, &root); err != nil {
		t.Fatal(err)
	}
	delete(root, "project_id")
	broken, _ := json.Marshal(root)
	_, err = LoadDocument(broken)
	be, ok = AsError(err)
	if !ok || be.Code != ECodeSchema {
		t.Fatalf("missing field error = %v, want E_SCHEMA", err)
	}
	if be.SourceVersion != SourceV1 || !strings.Contains(be.Field, "project_id") {
		t.Fatalf("error locator = version %q field %q", be.SourceVersion, be.Field)
	}
}

func TestFixedPointAndTimeRoundTrip(t *testing.T) {
	low, err := ConvertScaled(4234, ScalePermyriad, ScalePermille, RoundFloor, "cost")
	if err != nil {
		t.Fatal(err)
	}
	if low.Value != 423 || low.Residual != 4 {
		t.Fatalf("downscale = %+v", low)
	}
	restored, err := low.RestoreHighResolution(ScalePermyriad, "cost")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Value != 4234 {
		t.Fatalf("restored = %d", restored.Value)
	}

	ceilLow, err := ConvertScaled(4231, ScalePermyriad, ScalePermille, RoundCeil, "cost_ceil")
	if err != nil {
		t.Fatal(err)
	}
	if ceilLow.Value != 424 || ceilLow.Residual != -9 {
		t.Fatalf("ceil conversion = %+v", ceilLow)
	}
	ceilRestore, err := ceilLow.RestoreHighResolution(ScalePermyriad, "cost_ceil")
	if err != nil || ceilRestore.Value != 4231 {
		t.Fatalf("ceil restore = %+v, %v", ceilRestore, err)
	}

	ticks, residualMS, err := MillisToTicks(450, 100, RoundCeil, "cooldown")
	if err != nil {
		t.Fatal(err)
	}
	if ticks != 5 || residualMS != -50 || ticks*100+residualMS != 450 {
		t.Fatalf("time conversion ticks=%d residual=%d", ticks, residualMS)
	}

	_, err = ConvertScaled(math.MaxInt64, ScalePermille, ScalePermyriad, RoundExact, "overflow.field")
	be, ok := AsError(err)
	if !ok || be.Code != ECodeOVF || be.Field != "overflow.field" || len(be.Operands) != 2 {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestExpressionEquivalenceAndEvalErrors(t *testing.T) {
	fixture := testFixture(t)
	mismatch, err := CompareFlatAndTree(fixture.Flat, fixture.Trees, map[string]int64{"attunement": fixture.Project.Skills[0].CDRFlat - 2000})
	if err != nil || mismatch != nil {
		t.Fatalf("equivalence mismatch=%v err=%v", mismatch, err)
	}

	brokenFlat := FlatRuleTable{}
	for key, value := range fixture.Flat {
		brokenFlat[key] = value
	}
	brokenFlat["cdr:fireball"]++
	mismatch, err = CompareFlatAndTree(brokenFlat, fixture.Trees, map[string]int64{"attunement": fixture.Project.Skills[0].CDRFlat - 2000})
	if err == nil {
		t.Fatal("expected divergence error")
	}
	if mismatch == nil || mismatch.FirstNode != "cdr:fireball" {
		t.Fatalf("first mismatch = %+v", mismatch)
	}

	_, err = Eval(&Expr{
		Kind: "div", Node: "div-zero",
		Args: []*Expr{{Kind: "lit", Value: 1}, {Kind: "lit", Value: 0}},
	}, nil, "div")
	be, ok := AsError(err)
	if !ok || be.Code != ECodeEval || be.Node != "div-zero" || be.Arguments["arg1"] != 0 {
		t.Fatalf("div-zero error = %v", err)
	}

	_, _, err = EditRegen(fixture.Project, -1)
	be, ok = AsError(err)
	if !ok || be.Code != ECodeEval || be.Node == "" {
		t.Fatalf("negative regen error = %v", err)
	}

	_, err = Eval(&Expr{
		Kind: "sub", Node: "sub-overflow",
		Args: []*Expr{{Kind: "lit", Value: 0}, {Kind: "lit", Value: math.MinInt64}},
	}, nil, "sub")
	be, ok = AsError(err)
	if !ok || be.Code != ECodeOVF || be.Node != "sub-overflow" || len(be.Operands) != 2 {
		t.Fatalf("sub overflow error = %v", err)
	}
}

func TestDeterministicArbitrationRecords(t *testing.T) {
	fixture := testFixture(t)
	report := fixture.Truth
	var tick1 []ArbitrationRecord
	for _, group := range report.Arbitration {
		if len(group) > 0 && group[0].Tick == 1 {
			tick1 = group
		}
	}
	bySkill := map[string]ArbitrationRecord{}
	for _, record := range tick1 {
		bySkill[record.SkillID] = record
	}
	fire := bySkill["fireball"]
	if fire.Decision != "accepted" || fire.Contention != "shared_cooldown_group" {
		t.Fatalf("fire record = %+v", fire)
	}
	frost := bySkill["frost_nova"]
	if frost.Decision != "rejected" || frost.Reason != "group_cooldown" {
		t.Fatalf("frost record = %+v", frost)
	}
	slash := bySkill["magic_slash"]
	if slash.Decision != "rejected" || slash.Reason != "insufficient_resource" || slash.Contention != "shared_resource_pool" {
		t.Fatalf("slash record = %+v", slash)
	}

	again, err := ReferenceSchedule(fixture.Project, DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report, again) {
		t.Fatal("repeated deterministic schedule differs")
	}

	ambiguous, err := CloneProject(fixture.Project)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ambiguous.Skills {
		if ambiguous.Skills[i].ID == "frost_nova" {
			ambiguous.Skills[i].Priority = 10
		}
	}
	params := DefaultParams()
	params.StrictPriorityTies = true
	_, err = ReferenceSchedule(ambiguous, params)
	be, ok := AsError(err)
	if !ok || be.Code != ECodeARB {
		t.Fatalf("strict tie error = %v", err)
	}
	if !contains(be.Contested, "fireball") || !contains(be.Contested, "frost_nova") {
		t.Fatalf("contested set = %v", be.Contested)
	}
}

func TestInvalidationAndPersistentCache(t *testing.T) {
	fixture := testFixture(t)
	params := DefaultParams()
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	if _, _, err := BuildBaselineCache(fixture.Project, fixture.Truth, params, cachePath); err != nil {
		t.Fatal(err)
	}

	edited, changed, err := EditSkillCooldown(fixture.Project, "fireball", 6)
	if err != nil {
		t.Fatal(err)
	}
	inc, err := RerunWithPersistentCache(fixture.Project, fixture.Truth, edited, params, cachePath, changed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inc.ReusedTicks, []int{0}) {
		t.Fatalf("reused = %v", inc.ReusedTicks)
	}
	if len(inc.RecomputedTicks) != 11 || inc.RecomputedTicks[0] != 1 {
		t.Fatalf("recomputed = %v", inc.RecomputedTicks)
	}
	if len(inc.Plan.Entries) != 1 || !reflect.DeepEqual(inc.Plan.Entries[0].Ranges, []TickRange{{First: 1, Last: 11}}) {
		t.Fatalf("plan entries = %+v", inc.Plan.Entries)
	}
	expected, err := ReferenceSchedule(edited, params)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareReports(expected, inc.Report); err != nil {
		t.Fatalf("incremental report mismatch: %v", err)
	}

	unusedEdited, unusedChanged, err := EditSkillCooldown(fixture.Project, "unused_ward", 9)
	if err != nil {
		t.Fatal(err)
	}
	unusedCache := filepath.Join(t.TempDir(), "unused-cache.json")
	if _, _, err := BuildBaselineCache(fixture.Project, fixture.Truth, params, unusedCache); err != nil {
		t.Fatal(err)
	}
	unusedInc, err := RerunWithPersistentCache(fixture.Project, fixture.Truth, unusedEdited, params, unusedCache, unusedChanged)
	if err != nil {
		t.Fatal(err)
	}
	if len(unusedInc.ReusedTicks) != fixture.Project.TickCount || len(unusedInc.RecomputedTicks) != 0 {
		t.Fatalf("unused edit reused=%v recomputed=%v", unusedInc.ReusedTicks, unusedInc.RecomputedTicks)
	}

	regenEdited, regenChanged, err := EditRegen(fixture.Project, fixture.Project.Resource.RegenFlat+200)
	if err != nil {
		t.Fatal(err)
	}
	regenCache := filepath.Join(t.TempDir(), "regen-cache.json")
	if _, _, err := BuildBaselineCache(fixture.Project, fixture.Truth, params, regenCache); err != nil {
		t.Fatal(err)
	}
	regenInc, err := RerunWithPersistentCache(fixture.Project, fixture.Truth, regenEdited, params, regenCache, regenChanged)
	if err != nil {
		t.Fatal(err)
	}
	if len(regenInc.ReusedTicks) != 0 || len(regenInc.RecomputedTicks) != fixture.Project.TickCount {
		t.Fatalf("regen edit reused=%v recomputed=%v", regenInc.ReusedTicks, regenInc.RecomputedTicks)
	}
	regenExpected, err := ReferenceSchedule(regenEdited, params)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareTickReports(regenExpected, regenInc.Report); err != nil {
		t.Fatalf("regen incremental mismatch: %v", err)
	}
}

func TestCheckpointThreePathsAndRecoveryBoundaries(t *testing.T) {
	fixture := testFixture(t)
	params := DefaultParams()
	params.WindowTicks = 3
	base := t.TempDir()
	result, err := RunThreePathCheck(fixture.Project, params, base, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, report := range []*RunReport{result.Realtime, result.Resumed, result.Full} {
		if err := CompareTickReports(fixture.Truth, report); err != nil {
			t.Fatalf("persistence path differs from truth: %v", err)
		}
	}
	if len(result.Realtime.Commits) != 4 {
		t.Fatalf("commits = %d, want 4", len(result.Realtime.Commits))
	}

	store, err := NewCheckpointStore(filepath.Join(base, "realtime"))
	if err != nil {
		t.Fatal(err)
	}
	cp, _, err := store.LoadLatest()
	if err != nil {
		t.Fatal(err)
	}
	if cp.AlgorithmVersion != AlgorithmVersion || cp.ContentHash != fixture.Project.ContentHash {
		t.Fatalf("checkpoint envelope = %+v", cp)
	}
	if cp.IntentOffset != len(fixture.Project.Intents) {
		t.Fatalf("final offset = %d, want %d", cp.IntentOffset, len(fixture.Project.Intents))
	}
	if entries, _ := os.ReadDir(store.Dir); containsPrefix(entries, ".window-") {
		t.Fatal("temporary checkpoint file was left behind")
	}

	corruptPath := filepath.Join(t.TempDir(), "window-000000.json")
	if err := os.WriteFile(corruptPath, []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadCheckpointFile(corruptPath)
	if be, ok := AsError(err); !ok || be.Code != ECodeDecode {
		t.Fatalf("corrupt checkpoint error = %v", err)
	}

	mismatchDir := t.TempDir()
	if _, err := RunFirstWindows(fixture.Project, params, mismatchDir, 1); err != nil {
		t.Fatal(err)
	}
	mismatchStore, _ := NewCheckpointStore(mismatchDir)
	mismatchCP, mismatchPath, err := mismatchStore.LoadLatest()
	if err != nil {
		t.Fatal(err)
	}
	mismatchCP.AlgorithmVersion = "future-algo"
	if err := WriteAtomicJSON(mismatchPath, mismatchCP); err != nil {
		t.Fatal(err)
	}
	_, err = ResumeFromStore(fixture.Project, params, mismatchDir)
	if be, ok := AsError(err); !ok || be.Code != ECodeSchema {
		t.Fatalf("version mismatch resume error = %v", err)
	}
}

func TestFaultProbesAndDiagnosticsDemo(t *testing.T) {
	out := filepath.Join(t.TempDir(), "demo")
	result, err := RunDemo(DefaultSeed, out)
	if err != nil {
		t.Fatal(err)
	}
	if !result.AllFaultsPass {
		t.Fatalf("fault probes failed: %+v", result.Faults)
	}
	htmlBytes, err := os.ReadFile(filepath.Join(out, "diagnostics.html"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(htmlBytes)
	for _, want := range []string{"逐 Tick 状态", "裁决明细", "精确失效清单", "预算占用"} {
		if !strings.Contains(page, want) {
			t.Fatalf("diagnostics page missing %q", want)
		}
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsPrefix(entries []os.DirEntry, prefix string) bool {
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			return true
		}
	}
	return false
}
