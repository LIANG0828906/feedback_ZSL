package fixed

import (
	"math"
	"testing"

	"dialogueaudio/audioerr"
)

func TestTimeRescaleRoundTrip(t *testing.T) {
	const samples = 5760
	ms, trace, err := SamplesToMillis(samples, 48000, RoundNearest, "clip.start")
	if err != nil {
		t.Fatal(err)
	}
	if ms != 120 || trace.Residual != 0 {
		t.Fatalf("ms=%d trace=%+v", ms, trace)
	}
	back, trace, err := MillisToSamples(ms, 48000, RoundNearest, "clip.start")
	if err != nil {
		t.Fatal(err)
	}
	if back != samples || trace.Residual != 0 {
		t.Fatalf("back=%d trace=%+v", back, trace)
	}
}

func TestRescaleIdempotentLossy(t *testing.T) {
	low, tr1, err := Rescale(12345, Q16, 1000, RoundNearest, "gain")
	if err != nil {
		t.Fatal(err)
	}
	high, tr2, err := Rescale(low, 1000, Q16, RoundNearest, "gain")
	if err != nil {
		t.Fatal(err)
	}
	low2, _, err := Rescale(high, Q16, 1000, RoundNearest, "gain")
	if err != nil {
		t.Fatal(err)
	}
	if low2 != low {
		t.Fatalf("lossy conversion is not idempotent: low=%d low2=%d", low, low2)
	}
	high2, tr4, err := Rescale(low2, 1000, Q16, RoundNearest, "gain")
	if err != nil {
		t.Fatal(err)
	}
	if high2 != high || tr4.Residual != tr2.Residual {
		t.Fatalf("second round is not stable: high=%d high2=%d residual=%d/%d traces=%+v %+v",
			high, high2, tr2.Residual, tr4.Residual, tr1, tr2)
	}
}

func TestAlignedGainRoundTripResidualIsStable(t *testing.T) {
	low, first, err := Rescale(12321, Q16, 1000, RoundNearest, "gain")
	if err != nil {
		t.Fatal(err)
	}
	high, _, err := Rescale(low, 1000, Q16, RoundNearest, "gain")
	if err != nil {
		t.Fatal(err)
	}
	low2, second, err := Rescale(high, Q16, 1000, RoundNearest, "gain")
	if err != nil {
		t.Fatal(err)
	}
	if low2 != low || second.Residual != first.Residual {
		t.Fatalf("aligned round trip changed residual: low=%d/%d residual=%d/%d",
			low, low2, first.Residual, second.Residual)
	}
}

func TestGainDBRoundTrip(t *testing.T) {
	for _, gain := range []int64{49152, 55706, 58982, 65536, 68813, 72090, 79238, 81920} {
		db, _, err := GainToDBQ16(gain, "gain")
		if err != nil {
			t.Fatal(err)
		}
		back, _, err := DBToGainQ16(db, "gain")
		if err != nil {
			t.Fatal(err)
		}
		diff := back - gain
		if diff < 0 {
			diff = -diff
		}
		if diff > 16 {
			t.Fatalf("gain round trip too large: gain=%d db=%d back=%d diff=%d", gain, db, back, diff)
		}
	}
}

func TestOverflowCarriesFieldAndOperands(t *testing.T) {
	_, err := MulDiv(math.MaxInt64, 2, 1, "clip.gain")
	if !audioerr.IsCode(err, audioerr.CodeOverflow) {
		t.Fatalf("want E_OVF, got %v", err)
	}
	coded, _ := audioerr.As(err)
	if coded.Field != "clip.gain" || coded.OpA != math.MaxInt64 || coded.OpB != 2 {
		t.Fatalf("bad overflow detail: %+v", coded)
	}
}

func TestRoundingDirections(t *testing.T) {
	tests := []struct {
		rounding Rounding
		want     int64
	}{
		{RoundTrunc, 1},
		{RoundFloor, 1},
		{RoundCeil, 2},
		{RoundNearest, 2},
	}
	for _, test := range tests {
		got, trace, err := Rescale(3, 2, 1, test.rounding, "x")
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Fatalf("%s: got %d want %d trace=%+v", test.rounding, got, test.want, trace)
		}
	}
}
