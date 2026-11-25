package iconmask

import "testing"

func TestRefineDefaultSamples(t *testing.T) {
	project := testProject(t)
	for _, image := range project.Images {
		result, err := RefineSample(image, DefaultRepairConfig())
		if err != nil {
			t.Fatalf("%s: %v", image.ID, err)
		}
		if result.EdgeEmpty || result.Status != "converged" {
			t.Fatalf("%s: expected converged non-empty band: %+v", image.ID, result.Status)
		}
		if result.Stats.Count != len(image.Truth.Components) {
			t.Fatalf("%s: expected %d regions, got %d", image.ID, len(image.Truth.Components), result.Stats.Count)
		}
		if result.OutsideChanged != 0 {
			t.Fatalf("%s: %d pixels changed outside edge band", image.ID, result.OutsideChanged)
		}
		if result.Accuracy.IoU < 0.90 {
			t.Fatalf("%s: IoU too low: %.4f", image.ID, result.Accuracy.IoU)
		}
		if len(result.Rounds) == 0 {
			t.Fatalf("%s: expected per-round logs", image.ID)
		}
		for _, round := range result.Rounds {
			if round.CloseChanged < 0 || round.OpenChanged < 0 || round.Changed < 0 {
				t.Fatalf("%s: invalid round log: %+v", image.ID, round)
			}
		}
		for i := range result.Mask.Data {
			if result.EdgeBand.Data[i] == 0 && result.Mask.Data[i] != image.InitialMask.Data[i] {
				t.Fatalf("%s: outside-band pixel changed at index %d", image.ID, i)
			}
		}
	}
}

func TestRefineIterationBudgetError(t *testing.T) {
	project := testProject(t)
	image := findImage(t, project, "icon-96")
	cfg := DefaultRepairConfig()
	cfg.MaxRounds = 1
	result, err := RefineSample(image, cfg)
	domainErr := assertCode(t, err, CodeIteration)
	if result == nil || result.Status != "iteration_budget_exhausted" {
		t.Fatalf("expected retained partial repair result: %+v", result)
	}
	if domainErr.Round == nil || domainErr.Remaining <= 0 {
		t.Fatalf("E_ITER must report round and remaining pixels: %+v", domainErr)
	}
}

func TestRefineExplicitEmptyBand(t *testing.T) {
	mask := NewMask(8, 8)
	result, err := Refine("blank", mask, mask, []BoundarySegment{{ID: "s"}}, DefaultRepairConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !result.EdgeEmpty || result.Status != "empty_edge_band" || result.Converged || result.Success {
		t.Fatalf("empty edge band must be explicit and not reported converged: %+v", result)
	}
}

func TestBlockwiseRepairMatchesFrameRepair(t *testing.T) {
	project := testProject(t)
	for _, image := range project.Images {
		frame, err := RefineSample(image, DefaultRepairConfig())
		if err != nil {
			t.Fatal(err)
		}
		blockwise, err := RefineBlockwise(image, DefaultRepairConfig())
		if err != nil {
			t.Fatal(err)
		}
		if !frame.Mask.Equal(blockwise.Mask) {
			t.Fatalf("%s: blockwise result differs by %d pixels", image.ID, frame.Mask.DiffPixels(blockwise.Mask))
		}
		if !sameStats(frame.Stats, blockwise.Stats) {
			t.Fatalf("%s: blockwise stats differ", image.ID)
		}
	}
}
