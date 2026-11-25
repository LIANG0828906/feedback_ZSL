package timeconv_test

import (
	"math"
	"testing"

	"replayaudit/internal/errs"
	"replayaudit/internal/model"
	"replayaudit/internal/timeconv"
)

func TestTurnMSStartAnchor(t *testing.T) {
	p := model.DefaultParams()
	cases := map[int64]int64{1: 0, 2: 3000, 3: 6000, 10: 27000}
	for turn, want := range cases {
		got, err := timeconv.TurnToMS(turn, p.MsPerTurn, p.Anchor, "e")
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		if got != want {
			t.Fatalf("turn %d = %d, want %d", turn, got, want)
		}
	}
}

func TestMSToTurnFloor(t *testing.T) {
	p := model.DefaultParams()
	cases := map[int64]int64{0: 1, 2999: 1, 3000: 2, 5999: 2, 6000: 3}
	for ms, want := range cases {
		got, err := timeconv.MSToTurn(ms, p.MsPerTurn, p.Round)
		if err != nil {
			t.Fatalf("ms %d: %v", ms, err)
		}
		if got != want {
			t.Fatalf("ms %d turn = %d, want %d", ms, got, want)
		}
	}
}

func TestRoundTripAlignedIdentity(t *testing.T) {
	p := model.DefaultParams()
	for turn := int64(1); turn <= 50; turn++ {
		ms, err := timeconv.TurnToMS(turn, p.MsPerTurn, p.Anchor, "e")
		if err != nil {
			t.Fatal(err)
		}
		gotTurn, back, err := timeconv.RoundTripMS(ms, p, "e")
		if err != nil {
			t.Fatal(err)
		}
		if gotTurn != turn || back != ms {
			t.Fatalf("round trip broken: turn=%d ms=%d -> turn=%d back=%d", turn, ms, gotTurn, back)
		}
	}
}

func TestFloorQuantizationIsStable(t *testing.T) {
	p := model.DefaultParams()
	// Unaligned input is quantized down and then stays stable.
	turn, back, err := timeconv.RoundTripMS(3999, p, "e")
	if err != nil {
		t.Fatal(err)
	}
	if turn != 2 || back != 3000 {
		t.Fatalf("want turn=2 back=3000, got %d %d", turn, back)
	}
	_, back2, err := timeconv.RoundTripMS(back, p, "e")
	if err != nil || back2 != back {
		t.Fatalf("quantization not idempotent: %d %v", back2, err)
	}
}

func TestOverflowTaggedWithEventID(t *testing.T) {
	p := model.DefaultParams()
	_, err := timeconv.TurnToMS(math.MaxInt64, p.MsPerTurn, "start", "e_ovf_42")
	if errs.CodeOf(err) != errs.EOvf {
		t.Fatalf("want E_OVF, got %v", err)
	}
	ae, ok := errs.AsAudit(err)
	if !ok || ae.EventID != "e_ovf_42" {
		t.Fatalf("overflow error must carry event id, got %+v", err)
	}
}

func TestCeilRounding(t *testing.T) {
	got, err := timeconv.MSToTurn(3001, 3000, "ceil")
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("ceil(3001) want turn 3, got %d", got)
	}
}
