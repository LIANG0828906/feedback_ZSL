package merge_test

import (
	"encoding/json"
	"testing"

	"lipsync.local/prototype/pkg/domain"
	"lipsync.local/prototype/pkg/merge"
)

func TestMergeRulesAndOrderIndependence(t *testing.T) {
	input := []domain.QuantizedPhoneme{
		{PhonemeID: "p0", StartFrame: 0, EndFrame: 4, Category: domain.CategoryConsonant, AffinityToNext: 800},
		{PhonemeID: "p1", StartFrame: 4, EndFrame: 8, Category: domain.CategoryVowel, AffinityToNext: 900},
		{PhonemeID: "p2", StartFrame: 8, EndFrame: 12, Category: domain.CategoryConsonant, AffinityToNext: 100},
		{PhonemeID: "p3", StartFrame: 12, EndFrame: 16, Category: domain.CategoryVowel, AffinityToNext: 1000},
		{PhonemeID: "p4", StartFrame: 16, EndFrame: 20, Category: domain.CategorySilence, AffinityToNext: 0},
		{PhonemeID: "p5", StartFrame: 20, EndFrame: 24, Category: domain.CategoryVowel, AffinityToNext: 0},
	}
	cfg := merge.Config{AffinityThreshold: 600, MaxPhonemesPerPackage: 3}
	want, err := merge.Merge("d", "ch", input, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 4 {
		t.Fatalf("want 4 packages, got %+v", want)
	}
	if got := want[0].PhonemeIDs; len(got) != 3 || got[0] != "p0" || got[2] != "p2" {
		t.Fatalf("unexpected max-size package: %v", got)
	}
	if len(want[2].PhonemeIDs) != 1 || want[2].PhonemeIDs[0] != "p4" {
		t.Fatalf("silence package crossed boundary: %+v", want[2])
	}
	shuffled := []domain.QuantizedPhoneme{input[5], input[2], input[0], input[4], input[1], input[3]}
	got, err := merge.Merge("d", "ch", shuffled, cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("enumeration changed partition:\n%s\n%s", a, b)
	}
}

func TestMergeRejectsOutOfRangeAffinity(t *testing.T) {
	input := []domain.QuantizedPhoneme{
		{PhonemeID: "a", StartFrame: 0, EndFrame: 2, Category: domain.CategoryVowel, AffinityToNext: 1001},
		{PhonemeID: "b", StartFrame: 2, EndFrame: 4, Category: domain.CategoryConsonant},
	}
	_, err := merge.Merge("d", "ch", input, merge.Config{AffinityThreshold: 600, MaxPhonemesPerPackage: 2})
	if code := domain.CodeOf(err); code != domain.ErrPara {
		t.Fatalf("want E_PARA, got %s: %v", code, err)
	}
	de := err.(*domain.Error)
	if de.PhonemeA != "a" || de.PhonemeB != "b" {
		t.Fatalf("missing phoneme pair evidence: %+v", de)
	}
}
