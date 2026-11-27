package schema_test

import (
	"encoding/json"
	"testing"

	"skillsched/internal/errx"
	"skillsched/internal/expr"
	"skillsched/internal/gen"
	"skillsched/internal/model"
	"skillsched/internal/schema"
)

func loadWire(t *testing.T, data []byte) *model.RuleSet {
	t.Helper()
	res, err := schema.Load(data)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return res.Rules
}

func TestThreeVersionsLoadSameContent(t *testing.T) {
	sc := gen.Generate("cross", gen.DefaultConfig())
	wires, err := sc.ExportWires()
	if err != nil {
		t.Fatal(err)
	}
	r1 := loadWire(t, wires.V1)
	r2 := loadWire(t, wires.V2)
	r3 := loadWire(t, wires.V3)
	if r1.SourceVersion != model.V1 || r2.SourceVersion != model.V2 || r3.SourceVersion != model.V3 {
		t.Fatal("source version not retained")
	}
	if len(r1.Skills) != len(r3.Skills) || len(r1.Pools) != len(r3.Pools) {
		t.Fatal("skill/pool count mismatch")
	}
	if r3.ContentHash() != sc.Rules.ContentHash() {
		t.Fatal("v3 roundtrip content hash mismatch")
	}
	src := sc.Rules.SkillByID()
	for _, s := range r2.Skills {
		want := src[s.ID]
		if s.Cooldown != want.Cooldown || s.CDR != want.CDR {
			t.Fatalf("skill %s v2 mismatch", s.ID)
		}
	}
}

func TestUnknownVersionAndMissingField(t *testing.T) {
	if _, err := schema.Load([]byte(`{"schemaVersion":9,"pools":[]}`)); errx.CodeOf(err) != errx.ESchema {
		t.Fatalf("want E_SCHEMA got %v", err)
	}
	v3bad := map[string]any{
		"SchemaVer": 3,
		"Params":    map[string]any{"ParamRevision": 1, "TickSpanMs": 100, "TickHorizon": 8, "WindowTicks": 8},
		"ResourcePools": []any{map[string]any{"PoolID": "p", "DisplayName": "p", "UpperBound": 10,
			"RegenExpr": map[string]any{"id": "r", "op": "num", "value": 1}}},
		"Abilities": []any{map[string]any{"DisplayName": "noname"}},
	}
	b, _ := json.Marshal(v3bad)
	_, err := schema.Load(b)
	if errx.CodeOf(err) != errx.ESchema {
		t.Fatalf("want E_SCHEMA got %v", err)
	}
	var de *errx.Error
	if !errxAs(err, &de) || de.Fields["field_path"] == "" || de.Fields["source_version"] != "v3" {
		t.Fatalf("missing path/version context: %v", err)
	}
}

func errxAs(err error, target **errx.Error) bool {
	for err != nil {
		if e, ok := err.(*errx.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestUpgradeDefaultFills(t *testing.T) {
	sc := gen.Generate("up", gen.DefaultConfig())
	wires, _ := sc.ExportWires()
	res, err := schema.Load(wires.V1)
	if err != nil {
		t.Fatal(err)
	}
	sawCap, sawGroup := false, false
	for _, f := range res.Report.DefaultFills {
		if f.DefaultValue == "5000" && f.OldValue == "<absent>" {
			sawCap = true
		}
		if f.Path == "shared_groups" {
			sawGroup = true
		}
	}
	if !sawCap || !sawGroup {
		t.Fatalf("default fills incomplete cap=%v group=%v", sawCap, sawGroup)
	}
}

func TestDowngradeLossyAndReload(t *testing.T) {
	sc := gen.Generate("down", gen.DefaultConfig())
	sc.Rules.Pools[0].RegenTree = &expr.Node{ID: "regen.sqrt", Op: expr.OpSqrt,
		Left: &expr.Node{ID: "regen.x", Op: expr.OpAdd,
			Left:  &expr.Node{ID: "regen.c", Op: expr.OpNum, Value: 16},
			Right: &expr.Node{ID: "regen.v", Op: expr.OpVar, Name: "base_regen"}}}

	v2bytes, rep, err := schema.Export(sc.Rules, model.V2)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range rep.Losses {
		if l.Kind == "expression_tree" && l.Disposition == "collapse_to_constant" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected expression_tree E_LOSSY item")
	}
	reload := loadWire(t, v2bytes)
	src := sc.Rules.SkillByID()
	for _, s := range reload.Skills {
		want := src[s.ID]
		if s.Cost != want.Cost || s.Cooldown != want.Cooldown || s.CDR != want.CDR {
			t.Fatalf("skill %s not restored except downgraded fields", s.ID)
		}
	}
}

func TestV1DropsSharedGroups(t *testing.T) {
	sc := gen.Generate("v1loss", gen.DefaultConfig())
	_, rep, err := schema.Export(sc.Rules, model.V1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range rep.Losses {
		if l.Kind == "shared_group" && l.Disposition == "dropped" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected shared_group loss on v1 export")
	}
}
