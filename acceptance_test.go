package chaosdiag_test

import (
	"reflect"
	"testing"

	"chaosdiag/engine"
	"chaosdiag/gen"
	"chaosdiag/loader"
	"chaosdiag/model"
	"chaosdiag/reconcile"
)

func loadedProjects(t *testing.T) map[string]*model.Project {
	t.Helper()
	bundle, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*model.Project{}
	for version, doc := range bundle.Docs {
		p, err := loader.Load(doc)
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		out[version] = p
	}
	return out
}

func TestThreeSchemaVersionsLoadSameCoreGraphAndFlow(t *testing.T) {
	projects := loadedProjects(t)
	v1 := projects[model.SchemaV1]
	v2 := projects[model.SchemaV2]
	v3 := projects[model.SchemaV3]

	assertNames := func(label string, got, want []string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s mismatch\n got=%v\nwant=%v", label, got, want)
		}
	}

	serviceNames := func(p *model.Project) []string {
		out := []string{}
		for _, s := range p.Services {
			out = append(out, s.Name)
		}
		return out
	}
	edgeIDs := func(p *model.Project) []string {
		out := []string{}
		for _, e := range p.Edges {
			out = append(out, e.ID)
		}
		return out
	}
	instructionIDs := func(p *model.Project) []string {
		out := []string{}
		for _, in := range p.Instructions {
			out = append(out, in.ID)
		}
		return out
	}
	requestIDs := func(p *model.Project) []string {
		out := []string{}
		for _, r := range p.Requests {
			out = append(out, r.ID)
		}
		return out
	}
	paths := func(p *model.Project) [][]string {
		out := [][]string{}
		for _, r := range p.Requests {
			out = append(out, r.Path)
		}
		return out
	}

	assertNames("v2 services", serviceNames(v2), serviceNames(v1))
	assertNames("v3 services", serviceNames(v3), serviceNames(v1))
	assertNames("v2 edges", edgeIDs(v2), edgeIDs(v1))
	assertNames("v3 edges", edgeIDs(v3), edgeIDs(v1))
	assertNames("v2 instructions", instructionIDs(v2), instructionIDs(v1))
	assertNames("v3 instructions", instructionIDs(v3), instructionIDs(v1))
	assertNames("v2 requests", requestIDs(v2), requestIDs(v1))
	assertNames("v3 requests", requestIDs(v3), requestIDs(v1))
	if !reflect.DeepEqual(paths(v2), paths(v1)) || !reflect.DeepEqual(paths(v3), paths(v1)) {
		t.Fatalf("paths differ:\nv1=%v\nv2=%v\nv3=%v", paths(v1), paths(v2), paths(v3))
	}

	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		p := projects[version]
		for _, in := range p.Instructions {
			if in.Kind == model.KindDelay && in.ID == "I-DELAY-INV" && in.DelayMS != 60 {
				t.Fatalf("%s delay normalization = %d, want 60", version, in.DelayMS)
			}
			if in.Kind == model.KindTimeout && in.ID == "I-HOP-TIMEOUT-AUTH" && in.TimeoutMS != 10 {
				t.Fatalf("%s timeout normalization = %d, want 10", version, in.TimeoutMS)
			}
		}
	}
}

func TestEverySchemaMatchesGeneratorTruthAndVerdicts(t *testing.T) {
	bundle, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	for version, doc := range bundle.Docs {
		p, err := loader.Load(doc)
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		report := engine.Run(p)
		if !reflect.DeepEqual(report.Results, bundle.Truth.Results) {
			for i := range bundle.Truth.Results {
				if i >= len(report.Results) {
					break
				}
				if !reflect.DeepEqual(report.Results[i], bundle.Truth.Results[i]) {
					t.Fatalf("%s result %s mismatch\n got=%#v\nwant=%#v", version, report.Results[i].RequestID, report.Results[i], bundle.Truth.Results[i])
				}
			}
			t.Fatalf("%s results do not match generator truth", version)
		}
		if !reflect.DeepEqual(report.Schedules, bundle.Truth.Schedules) {
			t.Fatalf("%s schedules do not match truth", version)
		}
		verdicts := reconcile.ReconcileAll(p, report.Results)
		for _, v := range verdicts {
			want := bundle.Truth.ExpectedVerdicts[v.RequestID]
			if v.Classification != want {
				t.Fatalf("%s %s verdict=%s want=%s reason=%s", version, v.RequestID, v.Classification, want, v.Reason)
			}
		}
	}
}
