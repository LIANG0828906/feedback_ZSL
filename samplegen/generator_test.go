package samplegen

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"serviceregistry/registry"
)

func TestBundleVersionsNormalizeToSameProjectAndTruth(t *testing.T) {
	bundle, err := BuildBundle(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	baselineHash := ""
	var baselineTruth registry.TruthReport
	for _, schema := range []registry.SchemaVersion{registry.SchemaV1, registry.SchemaV2, registry.SchemaV3} {
		document, err := registry.LoadDocument(bundle.Files[schema])
		if err != nil {
			t.Fatalf("%s load: %v", schema, err)
		}
		if document.Project.SchemaVersion != schema {
			t.Fatalf("%s schema marker = %s", schema, document.Project.SchemaVersion)
		}
		hash, err := document.Project.ModelHash()
		if err != nil {
			t.Fatal(err)
		}
		if baselineHash == "" {
			baselineHash = hash
		} else if hash != baselineHash {
			t.Fatalf("%s model hash %s != %s", schema, hash, baselineHash)
		}
		if document.Truth == nil {
			t.Fatalf("%s has no embedded truth", schema)
		}
		run, err := registry.RunProject(document.Project, registry.RunOptions{})
		if err != nil {
			t.Fatalf("%s run: %v", schema, err)
		}
		actual := run.Truth()
		if actual.ModelHash != baselineHash {
			t.Fatalf("%s truth hash = %s, want %s", schema, actual.ModelHash, baselineHash)
		}
		actualJSON, _ := json.MarshalIndent(actual, "", "  ")
		truthJSON, _ := json.MarshalIndent(*document.Truth, "", "  ")
		if !bytes.Equal(actualJSON, truthJSON) {
			index := firstDifference(actualJSON, truthJSON)
			begin := index - 240
			if begin < 0 {
				begin = 0
			}
			endActual := index + 240
			if endActual > len(actualJSON) {
				endActual = len(actualJSON)
			}
			endTruth := index + 240
			if endTruth > len(truthJSON) {
				endTruth = len(truthJSON)
			}
			t.Fatalf("%s truth mismatch at byte %d\nactual:\n%s\nexpected:\n%s", schema, index, actualJSON[begin:endActual], truthJSON[begin:endTruth])
		}
		if schema == registry.SchemaV1 {
			baselineTruth = actual
		} else {
			baselineJSON, _ := json.Marshal(baselineTruth)
			currentJSON, _ := json.Marshal(actual)
			if !bytes.Equal(baselineJSON, currentJSON) {
				t.Fatalf("%s truth differs from v1", schema)
			}
		}
	}
	if len(baselineTruth.Steps) != 21 {
		t.Fatalf("truth has %d steps, want 21", len(baselineTruth.Steps))
	}
	assertSampleCoverage(t, baselineTruth)
}

func TestCheckedInGoldenSamples(t *testing.T) {
	bundle, err := BuildBundle(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range []registry.SchemaVersion{registry.SchemaV1, registry.SchemaV2, registry.SchemaV3} {
		path := filepath.Join("..", "samples", string(schema)+".json")
		checkedIn, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read golden sample %s: %v", path, err)
		}
		expectedFile := append(append([]byte(nil), bundle.Files[schema]...), '\n')
		if !bytes.Equal(checkedIn, expectedFile) {
			t.Fatalf("%s is not reproducible from the fixed seed", path)
		}
		document, err := registry.LoadDocument(checkedIn)
		if err != nil {
			t.Fatalf("load golden %s: %v", schema, err)
		}
		run, err := registry.RunProject(document.Project, registry.RunOptions{})
		if err != nil {
			t.Fatalf("run golden %s: %v", schema, err)
		}
		actual, _ := json.Marshal(run.Truth())
		expected, _ := json.Marshal(document.Truth)
		if !bytes.Equal(actual, expected) {
			t.Fatalf("golden truth mismatch for %s", schema)
		}
	}
}

func firstDifference(left, right []byte) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for index := 0; index < limit; index++ {
		if left[index] != right[index] {
			return index
		}
	}
	if len(left) != len(right) {
		return limit
	}
	return -1
}

func assertSampleCoverage(t *testing.T, truth registry.TruthReport) {
	t.Helper()
	var transitions, deferrals, queries, expirations int
	for _, step := range truth.Steps {
		transitions += len(step.Transitions)
		deferrals += len(step.DeferredProbes)
		queries += len(step.Queries)
		for _, instance := range step.Instances {
			if instance.Phase == registry.PhaseRemoval && instance.LeaseRemainingMS == 0 {
				expirations++
			}
		}
	}
	if transitions == 0 {
		t.Fatal("sample has no health transitions")
	}
	if deferrals == 0 {
		t.Fatal("sample has no deferred probes")
	}
	if queries < 8 {
		t.Fatalf("sample has %d query results, want at least 8", queries)
	}
	if expirations == 0 {
		t.Fatal("sample has no lease-expiry steps")
	}
}
