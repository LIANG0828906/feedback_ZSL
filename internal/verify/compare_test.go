package verify

import (
	"testing"

	"levelacoustic/internal/model"
)

func TestCompareExactLocatesFirstField(t *testing.T) {
	want := &model.SolveResult{
		CommitSequence: []string{"R1"},
		ListenerResults: []model.ListenerResult{{
			SourceID: "S1", ListenerID: "L1", ListenerRoomID: "R1", Reachable: true,
			RoomChain: []string{"R1"}, CurveDBMicro: []int64{10, 9}, SPLDBMicro: 70,
		}},
	}
	got := &model.SolveResult{
		CommitSequence: []string{"R1"},
		ListenerResults: []model.ListenerResult{{
			SourceID: "S1", ListenerID: "L1", ListenerRoomID: "R1", Reachable: true,
			RoomChain: []string{"R1"}, CurveDBMicro: []int64{10, 8}, SPLDBMicro: 70,
		}},
	}
	err := CompareExact(want, got)
	me, ok := model.AsError(err)
	if !ok || me.Code != model.ErrDiff {
		t.Fatalf("want E_DIFF, got %v", err)
	}
	if me.RoomID != "R1" || me.ListenerID != "L1" || me.Field != "curve_db_micro[1]" {
		t.Fatalf("wrong difference location: %+v", me)
	}
}

func TestToleranceSweepIsMonotonic(t *testing.T) {
	p := &model.Project{Truth: &model.Truth{Entries: []model.TruthEntry{
		{SourceID: "S1", ListenerID: "L1", ListenerRoomID: "R1", Reachable: true, RoomChain: []string{"R1"}, PathLengthMicro: 100, CurveDBMicro: []int64{50, 40}, SPLDBMicro: 70},
		{SourceID: "S1", ListenerID: "L2", ListenerRoomID: "R1", Reachable: true, RoomChain: []string{"R1"}, PathLengthMicro: 100, CurveDBMicro: []int64{50, 20}, SPLDBMicro: 70},
	}}}
	result := &model.SolveResult{ListenerResults: []model.ListenerResult{
		{SourceID: "S1", ListenerID: "L1", ListenerRoomID: "R1", Reachable: true, RoomChain: []string{"R1"}, PathLengthMicro: 100, CurveDBMicro: []int64{52, 40}, SPLDBMicro: 70},
		{SourceID: "S1", ListenerID: "L2", ListenerRoomID: "R1", Reachable: true, RoomChain: []string{"R1"}, PathLengthMicro: 100, CurveDBMicro: []int64{50, 0}, SPLDBMicro: 70},
	}}
	counts := ToleranceSweep(p, result, []int64{0, 1, 5, 25})
	wantFailed := []int{2, 2, 1, 0}
	for i, want := range wantFailed {
		if counts[i].Failed != want {
			t.Fatalf("tol=%d failed=%d want %d (%+v)", counts[i].Tolerance, counts[i].Failed, want, counts)
		}
	}
	for i := 1; i < len(counts); i++ {
		if counts[i].Failed > counts[i-1].Failed {
			t.Fatalf("tolerance sweep not monotonic: %+v", counts)
		}
	}
}
