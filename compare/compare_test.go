package compare_test

import (
	"testing"

	"dialoguefit/compare"
	"dialoguefit/model"
)

func TestRunsEqualLocatesFirstFrameMismatch(t *testing.T) {
	expected := []*model.DialogueResult{{
		DialogueID: "dlg-09", CommitSequence: []string{"dlg-09/p0"},
		PhonemeResults: []model.PhonemeResult{{
			Index: 2, Frames: []model.Frame{{Index: 4, Value: 0.5}},
			Segment: &model.StretchSegment{StartSample: 10, EndSample: 20},
		}},
	}}
	actual := []*model.DialogueResult{{
		DialogueID: "dlg-09", CommitSequence: []string{"dlg-09/p0"},
		PhonemeResults: []model.PhonemeResult{{
			Index: 2, Frames: []model.Frame{{Index: 4, Value: 0.51}},
			Segment: &model.StretchSegment{StartSample: 10, EndSample: 20},
		}},
	}}
	m := compare.RunsEqual(expected, actual)
	if m.OK || m.DialogueID != "dlg-09" || m.PhonemeIndex != 2 || m.Field != "frames[0].value" {
		t.Fatalf("unexpected mismatch: %+v", m)
	}
}

func TestRunsEqualAcceptsIdenticalResults(t *testing.T) {
	r := &model.DialogueResult{DialogueID: "d", PhonemeResults: []model.PhonemeResult{{Index: 0}}}
	m := compare.RunsEqual([]*model.DialogueResult{r}, []*model.DialogueResult{{
		DialogueID: "d", PhonemeResults: []model.PhonemeResult{{Index: 0}},
	}})
	if !m.OK {
		t.Fatalf("expected equal: %+v", m)
	}
}
