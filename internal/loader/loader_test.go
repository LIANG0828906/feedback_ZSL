package loader_test

import (
	"encoding/json"
	"testing"

	"schedlab/internal/domain"
	"schedlab/internal/faults"
	"schedlab/internal/gen"
	"schedlab/internal/loader"
)

func loadAll(t *testing.T, b *gen.Bundle) map[string]*domain.Project {
	t.Helper()
	out := map[string]*domain.Project{}
	for ver, raw := range map[string][]byte{
		loader.V1: b.V1JSON, loader.V2: b.V2JSON, loader.V3: b.V3JSON,
	} {
		p, err := loader.Load(raw, ver, loader.Options{TopologySemantics: b.TopoSemantics()})
		if err != nil {
			t.Fatalf("load %s: %v", ver, err)
		}
		out[ver] = p
	}
	return out
}

func TestThreeVersionEquality(t *testing.T) {
	for _, profile := range []string{"standard", "large"} {
		b, err := gen.BuildBundle(profile)
		if err != nil {
			t.Fatal(err)
		}
		loaded := loadAll(t, b)
		truth := b.Truth.(*domain.Project)
		for ver, p := range loaded {
			assertProjectEqual(t, ver, truth, p)
		}
	}
}

func assertProjectEqual(t *testing.T, ver string, want, got *domain.Project) {
	t.Helper()
	if len(want.Nodes) != len(got.Nodes) || len(want.Pods) != len(got.Pods) {
		t.Fatalf("%s: length mismatch", ver)
	}
	for i := range want.Nodes {
		a, b := want.Nodes[i], got.Nodes[i]
		if a.ID != b.ID || a.Order != b.Order || a.Allocatable != b.Allocatable ||
			!mapsEqual(a.Labels, b.Labels) || !mapsEqual(a.Topology, b.Topology) {
			t.Fatalf("%s node %d mismatch: %+v vs %+v", ver, i, a, b)
		}
	}
	for i := range want.Pods {
		a, b := want.Pods[i], got.Pods[i]
		if a.ID != b.ID || a.Request != b.Request ||
			len(a.MustRules) != len(b.MustRules) ||
			len(a.PreferRules) != len(b.PreferRules) {
			t.Fatalf("%s pod %d mismatch", ver, i)
		}
		for j := range a.PreferRules {
			if a.PreferRules[j].WeightBps != b.PreferRules[j].WeightBps ||
				a.PreferRules[j].MaxCoLocated != b.PreferRules[j].MaxCoLocated ||
				a.PreferRules[j].TopologyKey != b.PreferRules[j].TopologyKey {
				t.Fatalf("%s pod %d rule %d mismatch", ver, i, j)
			}
		}
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestWeightOutOfRangeCode(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	var v3 gen.V3Project
	_ = json.Unmarshal(b.V3JSON, &v3)
	v3.Pods[0].PreferRules[0].WeightBps = 99999
	raw, _ := json.Marshal(&v3)
	_, err := loader.Load(raw, loader.V3, loader.Options{})
	if !faults.Is(err, faults.E_WEIGHT_RANGE) {
		t.Fatalf("got %v want E_WEIGHT_RANGE", err)
	}
}

func TestNegativeResourceCode(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	var v3 gen.V3Project
	_ = json.Unmarshal(b.V3JSON, &v3)
	v3.Pods[0].Request.CPU = "-50m"
	raw, _ := json.Marshal(&v3)
	_, err := loader.Load(raw, loader.V3, loader.Options{})
	if !faults.Is(err, faults.E_RESOURCE_NEGATIVE) {
		t.Fatalf("got %v want E_RESOURCE_NEGATIVE", err)
	}
}

func TestTopoSemanticConflictCode(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	_, err := loader.Load(b.V3JSON, loader.V3, loader.Options{
		TopologySemantics: map[string]string{"zone": "routing/zone"},
	})
	if !faults.Is(err, faults.E_TOPO_KEY_CONFLICT) {
		t.Fatalf("got %v want E_TOPO_KEY_CONFLICT", err)
	}
}

func TestUnknownSchemaVersion(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	if _, err := loader.Load(b.V1JSON, "v9", loader.Options{}); !faults.Is(err, faults.E_SCHEMA_VERSION) {
		t.Fatalf("got %v want E_SCHEMA_VERSION", err)
	}
}
