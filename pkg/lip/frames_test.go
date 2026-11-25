package lip_test

import (
	"testing"

	"lipsync.local/prototype/pkg/domain"
	"lipsync.local/prototype/pkg/lip"
)

func TestTransitionBridgeAndTolerance(t *testing.T) {
	phonemes := []domain.QuantizedPhoneme{
		{PhonemeID: "sil", StartFrame: 0, EndFrame: 4, Category: domain.CategorySilence},
		{PhonemeID: "v", StartFrame: 4, EndFrame: 8, Category: domain.CategoryVowel},
	}
	packages := []domain.LipPackage{
		{ID: "d/pkg/000", DialogueID: "d", StartFrame: 0, EndFrame: 4, PhonemeIDs: []string{"sil"}},
		{ID: "d/pkg/001", DialogueID: "d", StartFrame: 4, EndFrame: 8, PhonemeIDs: []string{"v"}},
	}
	got, err := lip.RenderFrames("d", phonemes, packages, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Frames[4].Lip != domain.LipClosed || !got.Frames[4].Transition {
		t.Fatalf("missing bridge frame: %+v", got.Frames[4])
	}
	if len(got.Transitions) != 1 || got.Transitions[0].Frame != 4 {
		t.Fatalf("transition evidence: %+v", got.Transitions)
	}
	_, err = lip.RenderFrames("d", phonemes, packages, 0)
	if code := domain.CodeOf(err); code != domain.ErrLip {
		t.Fatalf("want E_LIP, got %s: %v", code, err)
	}
	de := err.(*domain.Error)
	if de.PackageID != "d/pkg/001" || de.Frame != 4 || de.LipA != domain.LipRest || de.LipB != domain.LipOpen {
		t.Fatalf("E_LIP lacks bridge evidence: %+v", de)
	}
}

func TestMissingPackageMapsToLipError(t *testing.T) {
	phonemes := []domain.QuantizedPhoneme{
		{PhonemeID: "v", StartFrame: 0, EndFrame: 2, Category: domain.CategoryVowel},
	}
	packages := []domain.LipPackage{{ID: "pkg", StartFrame: 0, EndFrame: 2, PhonemeIDs: []string{"other"}}}
	_, err := lip.RenderFrames("d", phonemes, packages, 1)
	if code := domain.CodeOf(err); code != domain.ErrLip {
		t.Fatalf("want E_LIP, got %s: %v", code, err)
	}
}
