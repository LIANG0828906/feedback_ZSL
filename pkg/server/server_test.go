package server

import (
	"testing"

	"roomacoustics/pkg/acoustics"
)

func TestServiceInitialStateAndEdit(t *testing.T) {
	svc, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	st := svc.State()
	if !st.OK || st.Report.Progress.Committed != st.Report.Progress.Total {
		t.Fatalf("bad initial state: %+v", st.Report.Progress)
	}
	if st.Report.Verification == nil || !st.Report.Verification.OK {
		t.Fatalf("initial truth verification failed: %+v", st.Report.Verification)
	}

	alpha := 0.33
	edited, err := svc.ApplyEdit(acoustics.Edit{Type: "set_absorption", ObjectID: "RF-R2-B", Absorption: &alpha})
	if err != nil {
		t.Fatal(err)
	}
	if edited.Report.Progress.Committed != 4 || edited.Invalidation == nil {
		t.Fatalf("edit did not complete: progress=%+v invalidation=%+v", edited.Report.Progress, edited.Invalidation)
	}
	found := false
	for _, id := range edited.Invalidation.ReusedRooms {
		if id == "R1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("R1 should be reused after R2 absorption edit: %+v", edited.Invalidation)
	}
}
