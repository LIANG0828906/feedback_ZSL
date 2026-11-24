package dsp

import (
	"testing"

	"roomreverb/internal/config"
	"roomreverb/internal/model"
)

func TestCheckTruthReportsPrecisionSampleIndex(t *testing.T) {
	profile := config.Default().Profiles["tight"]
	result := model.PathResult{
		Key:       "S1->L1",
		RoomChain: []string{"R1"},
		CurveDB:   []float64{1, 2, 3},
		SPLDB:     70,
		Length:    10,
	}
	truth := model.PathTruth{
		RoomChain: []string{"R1"},
		CurveDB:   []float64{1, 2, 3.5},
		SPLDB:     70,
		Length:    10,
	}
	err := CheckTruth(result, truth, profile)
	if err == nil || err.Code != model.CodePrec {
		t.Fatalf("expected E_PREC, got %#v", err)
	}
	if err.Details["sample_index"] != 2 {
		t.Fatalf("expected sample_index=2, got %#v", err.Details)
	}
	if deviation, ok := err.Details["observed_deviation"].(float64); !ok || deviation < 0.49 {
		t.Fatalf("expected observed deviation near .5, got %#v", err.Details)
	}
}
