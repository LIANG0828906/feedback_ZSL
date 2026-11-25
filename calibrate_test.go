package dialogue

import (
	"errors"
	"testing"
)

func TestCalibrationMatchesTruthAndBudget(t *testing.T) {
	p, truth, _ := sampleProject(t)
	rows, pErr := FullBuild(p)
	if pErr != nil {
		t.Fatal(pErr)
	}
	truthCal := make(map[string]CalibratedFrame)
	var adjustmentSum int64
	for _, clip := range truth.Clips {
		adjustmentSum += abs64(clip.GainAdjustBP)
		for _, cal := range clip.Calibration {
			truthCal[cal.RangeID] = cal
		}
	}
	if adjustmentSum > p.TotalBudgetBP {
		t.Fatalf("adjustment sum %d exceeds budget %d", adjustmentSum, p.TotalBudgetBP)
	}
	rowAdjustmentSum := int64(0)
	seenClips := map[string]bool{}
	for _, row := range rows {
		want := truthCal[row.RangeID]
		if abs64(row.Calibration.LoudAfterMDB-want.LoudAfterMDB) > 2 {
			t.Fatalf("%s loud after=%d truth=%d", row.RangeID, row.Calibration.LoudAfterMDB, want.LoudAfterMDB)
		}
		if row.Calibration.GainAdjustBP != want.GainAdjustBP {
			t.Fatalf("%s adjust=%d truth=%d", row.RangeID, row.Calibration.GainAdjustBP, want.GainAdjustBP)
		}
		if !seenClips[row.ClipID] {
			rowAdjustmentSum += abs64(row.Calibration.GainAdjustBP)
			seenClips[row.ClipID] = true
		}
	}
	if rowAdjustmentSum != adjustmentSum || rowAdjustmentSum > p.TotalBudgetBP {
		t.Fatalf("row adjustment sum=%d truth=%d budget=%d", rowAdjustmentSum, adjustmentSum, p.TotalBudgetBP)
	}
}

func TestLoudnessUnreachableReturnsE_LOUDWithGaps(t *testing.T) {
	p, _, _ := sampleProject(t)
	features := make(map[string][]FrameFeature)
	for _, clip := range p.Clips {
		out, pErr := ProcessClip(p, clip, nil)
		if pErr != nil && pErr.Code != CodeLoud {
			t.Fatal(pErr)
		}
		if out != nil {
			features[clip.ID] = out.Features
		}
	}
	unreachable := CloneProject(p)
	unreachable.TargetAmpMicro = LinearScale
	unreachable.TargetLoudMDB = 0
	unreachable.MaxAdjustBP = 0
	_, err := CalibrateProject(unreachable, features)
	var be *Error
	if !errors.As(err, &be) || be.Code != CodeLoud {
		t.Fatalf("expected E_LOUD, got %#v", err)
	}
	details, ok := be.Details.(LoudnessErrorDetails)
	if !ok || len(details.Gaps) != len(p.Clips) {
		t.Fatalf("expected gaps for every clip, got %#v", be.Details)
	}
	for _, gap := range details.Gaps {
		if gap.Reachable || gap.ReachableMaxLoudMDB == 0 || gap.GapMDB <= 0 {
			t.Fatalf("incomplete unreachable gap: %+v", gap)
		}
	}
}
