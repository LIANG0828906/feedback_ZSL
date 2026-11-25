package verify

import (
	"path/filepath"
	"testing"

	"edgerefine/internal/model"
	"edgerefine/internal/state"
	"edgerefine/internal/store"
)

func TestMissingCommittedRowReportsFirstViolation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "verify.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	truth := model.Manifest{
		SampleID: "missing", Width: 32, Height: 32,
		Params: model.DefaultParams(),
	}
	rep, err := Verify(st, truth, "fp")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK || rep.FirstViolation == nil {
		t.Fatalf("expected first violation, got %+v", rep)
	}
	v := rep.FirstViolation
	if v.Tile != [2]int{0, 0} || v.Stage != string(state.StageForeground) || v.Field != "committed_row" {
		t.Fatalf("unexpected violation: %+v", v)
	}
}
