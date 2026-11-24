package sample

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"economycore/engine"
	"economycore/format"
	"economycore/model"
)

func TestGeneratedVersionsShareSemanticsAndTruth(t *testing.T) {
	dir := t.TempDir()
	manifest, err := Generate(context.Background(), dir, 42)
	if err != nil {
		t.Fatal(err)
	}
	truth, err := LoadTruth(filepath.Join(dir, "truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(truth.Cycles) != 12 {
		t.Fatalf("truth cycles=%d, want 12", len(truth.Cycles))
	}
	if truth.OverflowTotals["farm/grain"] == 0 || truth.OverflowTotals["sawmill/plank"] == 0 {
		t.Fatalf("truth should contain overflow sources: %+v", truth.OverflowTotals)
	}
	canonical, err := format.LoadFile(filepath.Join(dir, "v3", "project.json"))
	if err != nil {
		t.Fatal(err)
	}
	canonicalHash, err := canonical.ContentHash()
	if err != nil {
		t.Fatal(err)
	}
	if canonicalHash != manifest.ContentHash {
		t.Fatalf("canonical content hash differs: %s vs %s", canonicalHash, manifest.ContentHash)
	}
	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		loaded, err := format.LoadFile(filepath.Join(dir, version, "project.json"))
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		if loaded.SourceVersion != version {
			t.Fatalf("source version=%s, want %s", loaded.SourceVersion, version)
		}
		hash, err := loaded.ContentHash()
		if err != nil {
			t.Fatal(err)
		}
		if hash != manifest.ContentHash {
			t.Fatalf("%s content hash=%s, want %s", version, hash, manifest.ContentHash)
		}
		if loaded.CycleMs != canonical.CycleMs {
			t.Fatalf("%s cycle span=%d, want %d", version, loaded.CycleMs, canonical.CycleMs)
		}
		if !reflect.DeepEqual(loaded.InitialStocks, canonical.InitialStocks) {
			t.Fatalf("%s initial stocks differ", version)
		}
		for _, node := range canonical.Nodes {
			other := loaded.NodeMap()[node.ID]
			if other == nil {
				t.Fatalf("%s missing node %s", version, node.ID)
			}
			leftRate, _, _ := node.ConstantRate()
			rightRate, _, _ := other.ConstantRate()
			if leftRate != rightRate {
				t.Fatalf("%s node %s rate=%d, want %d", version, node.ID, rightRate, leftRate)
			}
			if node.Recipe != other.Recipe {
				t.Fatalf("%s node %s recipe differs", version, node.ID)
			}
		}
		eng, err := engine.New(loaded, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		run, err := eng.RunFull(context.Background())
		if err != nil {
			t.Fatalf("%s run: %v", version, err)
		}
		if !reflect.DeepEqual(run.Cycles, truth.Cycles) {
			t.Fatalf("%s settlement differs from truth", version)
		}
		if !reflect.DeepEqual(run.OverflowTotals, truth.OverflowTotals) {
			t.Fatalf("%s overflow differs from truth: %+v", version, run.OverflowTotals)
		}
	}
	if len(manifest.Upgrades[model.SchemaV1]) == 0 {
		t.Fatal("v1 -> v3 upgrade traces should be recorded")
	}
}

func TestLosslessV3V1V3RoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(context.Background(), dir, 7); err != nil {
		t.Fatal(err)
	}
	v3, err := format.LoadFile(filepath.Join(dir, "v3", "project.json"))
	if err != nil {
		t.Fatal(err)
	}
	exported, err := format.Export(v3, model.SchemaV1, format.ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := format.Load(exported.Data)
	if err != nil {
		t.Fatal(err)
	}
	upgraded, roundTripTraces, err := format.Upgrade(reloaded, model.SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := format.Load(exported.Data)
	if err != nil {
		t.Fatal(err)
	}
	_, firstTraces, err := format.Upgrade(v1, model.SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstTraces, roundTripTraces) {
		t.Fatalf("trace sequence changed after round trip:\nfirst=%+v\nround=%+v", firstTraces, roundTripTraces)
	}
	if upgraded.CycleMs != v3.CycleMs || !reflect.DeepEqual(upgraded.InitialStocks, v3.InitialStocks) {
		t.Fatal("period/resource stocks changed in round trip")
	}
	for _, node := range v3.Nodes {
		other := upgraded.NodeMap()[node.ID]
		if other == nil {
			t.Fatalf("round trip lost node %s", node.ID)
		}
		a, _, _ := node.ConstantRate()
		b, _, _ := other.ConstantRate()
		if a != b || node.Recipe != other.Recipe {
			t.Fatalf("round trip changed node %s", node.ID)
		}
	}
	eng, err := engine.New(upgraded, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	run, err := eng.RunFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	truth, err := LoadTruth(filepath.Join(dir, "truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(run.Cycles, truth.Cycles) {
		t.Fatal("round-tripped project produced different settlement")
	}
}

func TestLossyDowngradeListing(t *testing.T) {
	m := Scenario(3)
	m.UpgradeDiscountBasisPoints = 1500
	m.OverflowPolicy = "reject"
	result, err := format.Export(m, model.SchemaV1, format.ExportOptions{})
	if !model.IsCode(err, model.ELossy) {
		t.Fatalf("expected E_LOSSY, got %v", err)
	}
	if result == nil || len(result.Losses) < 2 {
		t.Fatalf("expected loss list, got %+v", result)
	}
	allowed, err := format.Export(m, model.SchemaV1, format.ExportOptions{AllowLossy: true})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := format.Load(allowed.Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range m.Nodes {
		other := loaded.NodeMap()[node.ID]
		if other == nil || node.Recipe != other.Recipe {
			t.Fatalf("lossy export failed to preserve node/recipe %s", node.ID)
		}
		originalRate, _, _ := node.ConstantRate()
		loadedRate, _, _ := other.ConstantRate()
		if originalRate != loadedRate {
			t.Fatalf("lossy export changed node rate %s: %d != %d", node.ID, originalRate, loadedRate)
		}
	}
	if got := loaded.NodeMap()["forest"].Upgrade.Cost["wood"]; got != 9 {
		t.Fatalf("discount should be flattened into upgrade cost 9, got %d", got)
	}
	if loaded.OverflowPolicy != model.DefaultOverflowPolicy {
		t.Fatalf("lossy v1 export should use cap_drop, got %s", loaded.OverflowPolicy)
	}
}

func TestGenerationIsByteDeterministic(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	if _, err := Generate(context.Background(), first, 99); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(context.Background(), second, 99); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", "truth.json", "v1/project.json", "v2/project.json", "v3/project.json"} {
		a, err := os.ReadFile(filepath.Join(first, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(second, name))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s is not deterministic", name)
		}
		var raw any
		if err := json.Unmarshal(a, &raw); err != nil {
			t.Fatalf("invalid JSON %s: %v", name, err)
		}
	}
}
