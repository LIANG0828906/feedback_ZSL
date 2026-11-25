package formats_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"replayaudit/internal/engine"
	"replayaudit/internal/errs"
	"replayaudit/internal/formats"
	"replayaudit/internal/gen"
	"replayaudit/internal/model"
)

func TestGenerateLoadAndMergeAllPackages(t *testing.T) {
	root := t.TempDir()
	if _, err := gen.GenerateTree(root); err != nil {
		t.Fatal(err)
	}
	for _, b := range gen.BuildAll() {
		dir := filepath.Join(root, b.Replay.ReplayID)
		pkg, err := formats.LoadPackage(dir)
		if err != nil {
			t.Fatalf("load %s: %v", b.Replay.ReplayID, err)
		}
		if len(pkg.Replay.Events) != len(b.Replay.Events) {
			t.Fatalf("%s: merged event count %d want %d", b.Replay.ReplayID,
				len(pkg.Replay.Events), len(b.Replay.Events))
		}
		for _, e := range pkg.Replay.Events {
			if !reflect.DeepEqual(e.SourceFormats, []int{1, 2, 3}) {
				t.Fatalf("%s/%s source formats = %v", b.Replay.ReplayID, e.ID, e.SourceFormats)
			}
			if e.FieldSource["at"] != 1 {
				t.Fatalf("%s/%s missing field provenance: %+v", b.Replay.ReplayID, e.ID, e.FieldSource)
			}
		}
		rep, err := engine.SettleFull(pkg.Replay, pkg.Params)
		if err != nil {
			t.Fatalf("settle %s: %v", b.Replay.ReplayID, err)
		}
		if mm := engine.CompareTruth(pkg.Truth, rep, pkg.Params.Tolerance); mm != nil {
			t.Fatalf("%s truth: %v", b.Replay.ReplayID, mm)
		}
	}
}

func TestUnknownFormatCode(t *testing.T) {
	_, _, err := formats.Decode([]byte(`{"format":7}`), model.DefaultParams())
	if errs.CodeOf(err) != errs.EFormat {
		t.Fatalf("want E_FORMAT, got %v", err)
	}
}

func TestMissingRequiredFieldPath(t *testing.T) {
	doc := `{"format":1,"replay_id":"x","entities":[{"id":"a","side":"s","max_hp":10}],
		"events":[{"id":"e1","kind":"damage","amount":1,"chain":["a"],"targets":["a"]}]}`
	_, _, err := formats.Decode([]byte(doc), model.DefaultParams())
	if errs.CodeOf(err) != errs.ESchema {
		t.Fatalf("want E_SCHEMA, got %v", err)
	}
	ae, _ := errs.AsAudit(err)
	if ae.Path != "events[0].at" {
		t.Fatalf("want path events[0].at, got %q", ae.Path)
	}
}

func TestSequenceSemanticSwapOnDecode(t *testing.T) {
	p := model.DefaultParams()
	f1 := `{"format":1,"replay_id":"x","entities":[{"id":"c","side":"s","max_hp":10},{"id":"t","side":"s","max_hp":10}],
		"events":[{"id":"e1","at":1,"kind":"damage","amount":1,"chain":["c"],"targets":["t"]}]}`
	r1, _, err := formats.Decode([]byte(f1), p)
	if err != nil {
		t.Fatal(err)
	}
	if got := r1.Events[0].Casters; !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("f1 chain should be casters, got %v", got)
	}
	f2 := `{"format":2,"replay_id":"x","entities":[{"id":"c","side":"s","max_hp":10},{"id":"t","side":"s","max_hp":10}],
		"events":[{"id":"e1","at":0,"kind":"damage","amount":1,"chain":["t"],"casters":["c"]}]}`
	r2, _, err := formats.Decode([]byte(f2), p)
	if err != nil {
		t.Fatal(err)
	}
	if got := r2.Events[0].Targets; !reflect.DeepEqual(got, []string{"t"}) {
		t.Fatalf("f2 chain should be targets, got %v", got)
	}
}

func TestHighLowHighOrderPreserved(t *testing.T) {
	root := t.TempDir()
	if _, err := gen.GenerateTree(root); err != nil {
		t.Fatal(err)
	}
	for _, b := range gen.BuildAll() {
		dir := filepath.Join(root, b.Replay.ReplayID)
		p1, _, err := formats.LoadFile(filepath.Join(dir, formats.File1), model.DefaultParams())
		if err != nil {
			t.Fatal(err)
		}
		p2, _, err := formats.LoadFile(filepath.Join(dir, formats.File2), model.DefaultParams())
		if err != nil {
			t.Fatal(err)
		}
		o1 := ids(engine.CanonicalOrder(p1.Events))
		o2 := ids(engine.CanonicalOrder(p2.Events))
		if !reflect.DeepEqual(o1, o2) {
			t.Fatalf("%s: f1 order %v != f2 order %v", b.Replay.ReplayID, o1, o2)
		}
		idx := p1.EventIndex()
		for _, e2 := range p2.Events {
			e1 := idx[e2.ID]
			if e1.At != e2.At {
				t.Fatalf("%s/%s normalized time %d vs %d", b.Replay.ReplayID, e2.ID, e1.At, e2.At)
			}
		}
	}
}

func ids(es []*model.Event) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}
