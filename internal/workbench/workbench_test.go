package workbench

import (
	"os"
	"path/filepath"
	"testing"

	"texlab/internal/sample"
)

func TestEditPersistsPlanAndVerifiesNewTruth(t *testing.T) {
	root := t.TempDir()
	spec := sample.Spec{
		Name: "wb", Width: 96, Height: 72, Depth: 8, Layout: "RGBA", Seed: 991,
		Patches: []sample.PatchSpec{
			{Name: "coral", XF: 0.1, YF: 0.1, Size: 6, RGBA: [4]int{255, 32, 16, 255}},
		},
	}
	if err := sample.Generate(root, spec); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "data")
	svc, err := NewService(root, data, 4, 262144)
	if err != nil {
		t.Fatal(err)
	}
	st, err := svc.LoadSample(spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if st.Verification.Status != "MATCH" {
		t.Fatalf("initial verification = %+v", st.Verification)
	}
	edited, err := svc.ApplyEdit(EditRequest{
		EntryID: "regional_swap", TargetDepth: 16, TargetChannels: 4,
		Permutation: []int{0, 1, 2, 3}, Alpha: "straight", Quantize: "round",
	})
	if err != nil {
		t.Fatal(err)
	}
	if edited.Plan.Revision != 2 || !edited.IsTruthRevision || edited.Verification.Status != "MATCH" {
		t.Fatalf("edited state = rev %d truth %v verification %+v", edited.Plan.Revision, edited.IsTruthRevision, edited.Verification)
	}
	if edited.InvalidatedCells == 0 || edited.InvalidatedCells >= edited.TotalCells {
		t.Fatalf("invalidated cells = %d / %d", edited.InvalidatedCells, edited.TotalCells)
	}
	if _, err := os.Stat(filepath.Join(data, "truths", "wb-rev2.json")); err != nil {
		t.Fatalf("generated truth missing: %v", err)
	}
	order := make([]string, 0, len(edited.Entries))
	for _, e := range edited.Entries {
		order = append(order, e.ID)
	}
	order[0], order[1] = order[1], order[0]
	reordered, err := svc.Reorder(ReorderRequest{Order: order})
	if err != nil {
		t.Fatal(err)
	}
	if reordered.Plan.Revision != 3 || reordered.Verification.Status != "MATCH" {
		t.Fatalf("reordered state = rev %d verification %+v", reordered.Plan.Revision, reordered.Verification)
	}

	restarted, err := NewService(root, data, 2, 262144)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := restarted.LoadSample(spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Plan.Revision != 3 || reloaded.Verification.Status != "MATCH" {
		t.Fatalf("persisted plan not restored: rev %d verification %+v", reloaded.Plan.Revision, reloaded.Verification)
	}
	cmp, err := restarted.ComparePaths()
	if err != nil {
		t.Fatal(err)
	}
	if !cmp.Equal {
		t.Fatal("incremental and full paths differ after restart")
	}
	exp, err := restarted.Export()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(exp.Files["final_png"]); err != nil {
		t.Fatalf("export missing: %v", err)
	}
}
