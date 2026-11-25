package dialoguecal

import (
	"math"
	"testing"
)

func TestFixedPointDBConversionWithinDeclaredBounds(t *testing.T) {
	maxDBDelta := int64(0)
	maxLinearDelta := int64(0)
	for permille := 1; permille <= 1000; permille++ {
		q := PermilleToQ15(permille)
		db := LinearToDBQ8(q)
		expectedDB := Q8(math.Round(20 * math.Log10(float64(permille)/1000.0) * 256))
		dbDelta := absInt64(int64(db) - int64(expectedDB))
		if dbDelta > maxDBDelta {
			maxDBDelta = dbDelta
		}
		if dbDelta > 16 {
			t.Fatalf("permille %d db=%d want=%d delta=%d", permille, db, expectedDB, dbDelta)
		}
		back, sat := DBQ8ToLinearQ15(db)
		if sat != nil {
			t.Fatalf("unexpected saturation for permille %d: %+v", permille, sat)
		}
		linearDelta := absInt64(int64(back) - int64(q))
		if linearDelta > maxLinearDelta {
			maxLinearDelta = linearDelta
		}
		relativeBound := int64(q)/100 + 2
		if linearDelta > relativeBound {
			t.Fatalf("permille %d q=%d back=%d delta=%d bound=%d", permille, q, back, linearDelta, relativeBound)
		}
	}
	t.Logf("max_db_delta_q8=%d max_linear_roundtrip_delta_q15=%d", maxDBDelta, maxLinearDelta)
	if _, sat := DBQ8ToLinearQ15(Q8(math.MaxInt32)); sat == nil || !sat.Max {
		t.Fatalf("expected max saturation for extreme Q8 dB, got %+v", sat)
	}
}

func TestFrameRateSwitchAndContinuousLimitingConclusionsStable(t *testing.T) {
	loud := make([]int16, 3200)
	for i := range loud {
		loud[i] = int16(22000 * math.Sin(2*math.Pi*140*float64(i)/16000))
	}
	project50 := syntheticProject("fps-50-limit", loud, 1000, 100, 200)
	engine50, err := NewEngine(project50)
	if err != nil {
		t.Fatal(err)
	}
	report50, err := engine50.ComputeAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range report50.Results {
		if !result.Calibration.Limited {
			t.Fatalf("50fps frame %d was not limited", result.Frame.GlobalIndex)
		}
	}
	if last := report50.Results[len(report50.Results)-1].Calibration.State; last != StateCalibrated {
		t.Fatalf("50fps stationary limited tone final state=%s", last)
	}

	project100, err := ApplyEdits(project50, Edit{Kind: EditFrameRate, FramesPerSecond: 100})
	if err != nil {
		t.Fatal(err)
	}
	engine100, err := NewEngine(project100)
	if err != nil {
		t.Fatal(err)
	}
	report100, err := engine100.ComputeAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range report100.Results {
		if !result.Calibration.Limited {
			t.Fatalf("100fps frame %d was not limited", result.Frame.GlobalIndex)
		}
	}
	if last := report100.Results[len(report100.Results)-1].Calibration.State; last != StateCalibrated {
		t.Fatalf("100fps stationary limited tone final state=%s", last)
	}

	truth100 := GenerateTruth(project100, engine100.Frames())
	for _, cmp := range CompareResults(report100.Results, truth100) {
		if !cmp.AllWithin {
			t.Fatalf("100fps truth mismatch: %+v", cmp)
		}
	}
}

func TestRepeatedRecalibrationMatchesTruth(t *testing.T) {
	suite := GenerateCriticalSuite()
	for _, testCase := range suite.Cases {
		if testCase.ID != "repeated_recalibration" {
			continue
		}
		recalibrate := 0
		for _, result := range testCase.Results {
			if result.Calibration.State == StateRecalibrate {
				recalibrate++
			}
		}
		if recalibrate < 4 {
			t.Fatalf("expected repeated recalibration states, got %d", recalibrate)
		}
		if !testCase.AllWithin {
			t.Fatal("repeated recalibration critical case exceeds tolerance")
		}
		return
	}
	t.Fatal("repeated_recalibration case missing")
}

func TestDBAndLinearGainExpressionsProduceSameCorrection(t *testing.T) {
	linear := PermilleToQ15(1125)
	microDB := gainDBMicroFromPermille(1125)
	fromDB := Q15(DBMicroToPermille(microDB)) // permille-scale integer, not Q15 gain
	dbBackToQ15, sat := DBQ8ToLinearQ15(LinearToDBQ8(linear))
	if sat != nil {
		t.Fatal(sat)
	}
	if fromDB != 1125 {
		t.Fatalf("dB schema mapped to permille %d", fromDB)
	}
	if delta := absInt64(int64(dbBackToQ15) - int64(linear)); delta > 40 {
		t.Fatalf("dB/linear correction conversion diverged: linear=%d dbBack=%d delta=%d", linear, dbBackToQ15, delta)
	}
}
