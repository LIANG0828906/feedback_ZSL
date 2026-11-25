package skillcore_test

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"skillbudget"
	"skillbudget/samplegen"
)

func TestVersionErrorsAndFieldPaths(t *testing.T) {
	_, err := skillcore.Load([]byte(`{"schema_version":99}`))
	assertCodePath(t, err, skillcore.CodeVersion, "schema_version")

	missingCooldown := `{
		"schema_version": 1,
		"tick_millis": 4,
		"resource_scale": 1000,
		"rounding": "floor",
		"resource_pools": [{"pool_id":"mana","capacity":100,"initial":50,"regen_per_tick":1}],
		"skills": [{
			"skill_id":"bad","pool_id":"mana","cost":10,"cast_ticks":1,
			"cooldown_reduction_permille":0,"priority":1,"shared_group":""
		}]
	}`
	_, err = skillcore.Load([]byte(missingCooldown))
	assertCodePath(t, err, skillcore.CodeVersion, "skills[0].cooldown_ticks")

	_, err = skillcore.Load([]byte(`{"format":"other","version":3}`))
	assertCodePath(t, err, skillcore.CodeVersion, "format")
}

func TestIntegerUnitAndScaleConversions(t *testing.T) {
	cases := []struct {
		rounding skillcore.Rounding
		want     int64
	}{
		{skillcore.RoundFloor, 2},
		{skillcore.RoundCeil, 3},
		{skillcore.RoundNearest, 3},
		{skillcore.RoundTrunc, 2},
	}
	for _, test := range cases {
		got, err := skillcore.MillisToTicks(10, 4, test.rounding, "cooldown")
		if err != nil {
			t.Fatalf("%s: %v", test.rounding, err)
		}
		if got != test.want {
			t.Fatalf("%s: got %d, want %d", test.rounding, got, test.want)
		}
	}
	bp, err := skillcore.PermilleToBP(123, skillcore.RoundFloor, "cdr")
	if err != nil || bp != 1230 {
		t.Fatalf("permille conversion: got %d, err=%v", bp, err)
	}
	permille, exact, err := skillcore.BPToPermilleExact(1230, "cdr")
	if err != nil || !exact || permille != 123 {
		t.Fatalf("exact permille conversion: got %d exact=%v err=%v", permille, exact, err)
	}
	_, err = skillcore.TicksToMillis(math.MaxInt64, 4, skillcore.RoundFloor, "skills.arc_nova.cooldown")
	assertCodePath(t, err, skillcore.CodeOverflow, "skills.arc_nova.cooldown")
	domain := new(skillcore.Error)
	if !errors.As(err, &domain) || domain.Unit != string(skillcore.UnitMillisecond) {
		t.Fatalf("overflow unit not reported: %#v", err)
	}
}

func TestThreeVersionRoundTripAndLossyExport(t *testing.T) {
	bundle, err := samplegen.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	source, err := skillcore.LoadVersion(bundle.Versions[3], 3)
	if err != nil {
		t.Fatal(err)
	}
	for version, data := range bundle.Versions {
		project, err := skillcore.LoadVersion(data, version)
		if err != nil {
			t.Fatalf("version %d: %v", version, err)
		}
		if project.ContentHash() != source.ContentHash() {
			t.Fatalf("version %d semantic hash mismatch", version)
		}
	}

	toV2, err := source.Export(2)
	if err != nil {
		t.Fatal(err)
	}
	if toV2.Project.ContentHash() != source.ContentHash() {
		t.Fatal("v2 export changed semantic hash")
	}
	if len(toV2.Losses) == 0 {
		t.Fatal("expected lossy expression/tag entries for v2 export")
	}
	backV3, err := toV2.Project.Export(3)
	if err != nil {
		t.Fatal(err)
	}
	if backV3.Project.ContentHash() != source.ContentHash() {
		t.Fatal("v3 -> v2 -> v3 roundtrip changed semantic hash")
	}
	assertRuleValuesEqual(t, source, backV3.Project)

	toV1, err := source.Export(1)
	if err != nil {
		t.Fatal(err)
	}
	if toV1.Project.ContentHash() != source.ContentHash() {
		t.Fatal("v1 export changed semantic hash")
	}
	roundTripV3, err := toV1.Project.Export(3)
	if err != nil {
		t.Fatal(err)
	}
	if roundTripV3.Project.ContentHash() != source.ContentHash() {
		t.Fatal("v3 -> v1 -> v3 roundtrip changed semantic hash")
	}
	if len(roundTripV3.FillTraces) == 0 {
		t.Fatal("upgrade default fills were not traced")
	}
	foundTags, foundEnabled := false, false
	for _, trace := range roundTripV3.FillTraces {
		if strings.HasSuffix(trace.Path, ".tags") {
			foundTags = true
		}
		if strings.HasSuffix(trace.Path, ".enabled") {
			foundEnabled = true
		}
		if trace.OldValue != "<absent>" || trace.Source == "" {
			t.Fatalf("incomplete fill trace: %#v", trace)
		}
	}
	if !foundTags || !foundEnabled {
		t.Fatalf("missing expected fill traces: %#v", roundTripV3.FillTraces)
	}
	assertRuleValuesEqual(t, source, roundTripV3.Project)
}

func TestUnrepresentableExpressionRejectsWithE_LOSSY(t *testing.T) {
	project := minimalProject()
	project.Skills[0].Cooldown = skillcore.Expr{
		Op: "add",
		Args: []skillcore.Expr{
			skillcore.Const(4),
			{Op: "tick"},
		},
	}
	if err := project.Normalize(); err != nil {
		t.Fatal(err)
	}
	_, err := project.Export(2)
	if !skillcore.IsCode(err, skillcore.CodeLossy) {
		t.Fatalf("expected E_LOSSY, got %v", err)
	}
	domain := new(skillcore.Error)
	if !errors.As(err, &domain) || len(domain.Losses) == 0 {
		t.Fatalf("loss list missing: %#v", err)
	}
}

func TestOverflowDuringVersionLoad(t *testing.T) {
	data := `{
		"schema_version": 1,
		"tick_millis": 4,
		"resource_scale": 1000,
		"rounding": "floor",
		"resource_pools": [{"pool_id":"mana","capacity":9223372036854775807,"initial":0,"regen_per_tick":0}],
		"skills": []
	}`
	_, err := skillcore.Load([]byte(data))
	assertCodePath(t, err, skillcore.CodeOverflow, "resource_pools[0].capacity")
}

func assertCodePath(t *testing.T, err error, code skillcore.Code, path string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s", code)
	}
	domain := new(skillcore.Error)
	if !errors.As(err, &domain) {
		t.Fatalf("expected domain error, got %T: %v", err, err)
	}
	if domain.Code != code || domain.Path != path {
		t.Fatalf("got code=%s path=%q, want code=%s path=%q (%v)",
			domain.Code, domain.Path, code, path, err)
	}
}

func assertRuleValuesEqual(t *testing.T, expected, actual *skillcore.Project) {
	t.Helper()
	if len(expected.Pools) != len(actual.Pools) || len(expected.Skills) != len(actual.Skills) {
		t.Fatalf("rule shape changed: pools %d/%d skills %d/%d",
			len(expected.Pools), len(actual.Pools), len(expected.Skills), len(actual.Skills))
	}
	for i := range expected.Pools {
		left, right := expected.Pools[i], actual.Pools[i]
		if left.ID != right.ID || left.Max != right.Max || left.Initial != right.Initial {
			t.Fatalf("pool %d changed: %#v != %#v", i, expected.Pools[i], actual.Pools[i])
		}
		if !reflect.DeepEqual(left.Regen, right.Regen) {
			leftValue, _, leftErr := left.Regen.Constant(expected.Rounding, "left")
			rightValue, _, rightErr := right.Regen.Constant(expected.Rounding, "right")
			if leftErr != nil || rightErr != nil || leftValue != rightValue {
				t.Fatalf("pool %s regen changed: %#v != %#v", left.ID, left.Regen, right.Regen)
			}
		}
	}
	for i := range expected.Skills {
		left, right := expected.Skills[i], actual.Skills[i]
		if left.ID != right.ID || left.PoolID != right.PoolID || left.Cost != right.Cost ||
			left.CastTicks != right.CastTicks || left.CDRBp != right.CDRBp ||
			left.Priority != right.Priority || left.SharedGroup != right.SharedGroup ||
			left.Enabled != right.Enabled {
			t.Fatalf("skill %s changed: %#v != %#v", left.ID, left, right)
		}
		if !reflect.DeepEqual(left.Cooldown, right.Cooldown) {
			leftValue, _, leftErr := left.Cooldown.Constant(expected.Rounding, "left")
			rightValue, _, rightErr := right.Cooldown.Constant(expected.Rounding, "right")
			if leftErr != nil || rightErr != nil || leftValue != rightValue {
				t.Fatalf("skill %s cooldown changed: %#v != %#v", left.ID, left.Cooldown, right.Cooldown)
			}
		}
	}
}

func minimalProject() *skillcore.Project {
	return &skillcore.Project{
		SourceVersion:  3,
		TickMillis:     4,
		Rounding:       skillcore.RoundFloor,
		ConflictPolicy: skillcore.ConflictPriorityThenID,
		Pools: []skillcore.ResourcePool{{
			ID: "mana", Max: 1000, Initial: 500, Regen: skillcore.Const(10),
		}},
		Skills: []skillcore.Skill{{
			ID: "bolt", PoolID: "mana", Cost: 100, Cooldown: skillcore.Const(4),
			CastTicks: 1, CDRBp: 100, Priority: 1, Enabled: true,
		}},
	}
}
