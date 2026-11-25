package lipsync_test

import (
	"errors"
	"testing"

	lipsync "lipsync"
)

func TestDiffResultsLocatesDialogueFrameAndField(t *testing.T) {
	project := lipsync.GenerateProject(lipsync.DefaultGenerateOptions())
	expected, err := lipsync.BuildFullResult(project)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := lipsync.BuildFullResult(project)
	if err != nil {
		t.Fatal(err)
	}
	const frame = 4
	dialogueID := actual.Dialogues[0].Dialogue.ID
	if frame >= len(actual.Dialogues[0].Attribution) {
		t.Fatalf("frame %d out of range", frame)
	}
	actual.Dialogues[0].Attribution[frame].PackID = "tampered-pack"

	diff := lipsync.DiffResults(expected, actual)
	var domain *lipsync.Error
	if !errors.As(diff, &domain) || domain.Code != lipsync.ECDiff {
		t.Fatalf("expected E_DIFF, got %v", diff)
	}
	if domain.DialogueID != dialogueID || !domain.HasFrame || domain.Frame != frame {
		t.Fatalf("diff location mismatch: %+v", domain)
	}
	if domain.FieldPath == "" {
		t.Fatal("expected field path")
	}
}
