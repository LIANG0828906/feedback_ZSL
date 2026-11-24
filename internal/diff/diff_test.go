package diff

import (
	"testing"

	"roomreverb/internal/model"
)

func TestCompareLocatesFirstCurveMismatch(t *testing.T) {
	expected := &model.FullResult{
		Order: []string{"R1"},
		Paths: []model.PathResult{{
			Key: "S1->L1", ListenerRoomID: "R1", RoomChain: []string{"R1"},
			CurveDB: []float64{1, 2, 3}, SPLDB: 70,
		}},
	}
	actual := &model.FullResult{
		Order: []string{"R1"},
		Paths: []model.PathResult{{
			Key: "S1->L1", ListenerRoomID: "R1", RoomChain: []string{"R1"},
			CurveDB: []float64{1, 2.25, 3}, SPLDB: 70,
		}},
	}
	err := CompareFullResults(expected, actual)
	if err == nil || err.Code != model.CodeDiff {
		t.Fatalf("expected E_DIFF, got %#v", err)
	}
	if err.Details["room_id"] != "R1" || err.Details["field"] != "curve_db" || err.Details["sample_index"] != 1 {
		t.Fatalf("unexpected diff location: %#v", err.Details)
	}
}
