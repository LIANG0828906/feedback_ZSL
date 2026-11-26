package loudness

import (
	"bytes"
	"testing"
)

func TestGenerateAllDeterministic(t *testing.T) {
	seed := int64(20261127)
	first, err := GenerateAll(seed)
	if err != nil {
		t.Fatalf("GenerateAll: %v", err)
	}
	second, err := GenerateAll(seed)
	if err != nil {
		t.Fatalf("GenerateAll second: %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("got %d cases, want 3", len(first))
	}
	for i := range first {
		if first[i].Version != SupportedVersions()[i] {
			t.Fatalf("case %d version = %s", i, first[i].Version)
		}
		if !bytes.Equal(first[i].Wire, second[i].Wire) {
			t.Fatalf("seed is not deterministic for %s", first[i].Version)
		}
		if first[i].Truth == nil || len(first[i].Truth.Clips) == 0 {
			t.Fatalf("missing truth for %s", first[i].Version)
		}
		if first[i].Project.Provenance.OriginalVersion != first[i].Version {
			t.Fatalf("source version not preserved for %s", first[i].Version)
		}
	}
}
