package lipsync

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestLoadTruthsRoundTripAndVerify(t *testing.T) {
	set := sampleSetForTest(t, 2)
	data, err := json.Marshal(set.Truths)
	if err != nil {
		t.Fatal(err)
	}
	truths, err := LoadTruths(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(truths) != len(set.Projects) {
		t.Fatalf("truths = %d, want %d", len(truths), len(set.Projects))
	}
	for i := range truths {
		comparisons, err := VerifyTruth(set.Projects[i], truths[i])
		if err != nil {
			t.Fatal(err)
		}
		for _, comparison := range comparisons {
			if !comparison.Consistent {
				t.Fatalf("%s/%s: %v", set.Projects[i].DialogueID, comparison.Name, comparison.Mismatch)
			}
		}
	}
}

func TestTruthMissingFieldReturnsSchemaPath(t *testing.T) {
	set := sampleSetForTest(t, 1)
	data, _ := json.Marshal(set.Truths)
	var root []any
	_ = json.Unmarshal(data, &root)
	frames := root[0].(map[string]any)["frames"].([]any)
	delete(frames[0].(map[string]any), "lip")
	data, _ = json.Marshal(root)
	_, err := LoadTruths(bytes.NewReader(data))
	de, _ := asDomainError(err)
	if ErrorCode(err) != CodeSchema || de.Field != "truths[0].frames[0].lip" {
		t.Fatalf("truth schema error = %+v", de)
	}
}

func TestTruthIdentityMismatch(t *testing.T) {
	set := sampleSetForTest(t, 1)
	truth := set.Truths[0]
	truth.ContentHash = "wrong"
	_, err := VerifyTruth(set.Projects[0], truth)
	de, _ := asDomainError(err)
	if ErrorCode(err) != CodeSchema || de.Field != "truth.identity" {
		t.Fatalf("truth identity error = %+v", de)
	}
}
