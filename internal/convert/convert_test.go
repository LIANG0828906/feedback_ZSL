package convert

import (
	"math"
	"testing"

	"lip-pipeline-prototype/internal/domain"
)

func TestIntegerTimeAndWeightRoundTrip(t *testing.T) {
	ledger := NewLedger()
	for _, samples := range []int64{0, 48, 4800, 129600} {
		ms, err := SamplesToMS(samples, 48000, "x", ledger)
		if err != nil {
			t.Fatal(err)
		}
		back, err := MSToSamples(ms, 48000, "x", ledger)
		if err != nil {
			t.Fatal(err)
		}
		if back != samples {
			t.Fatalf("aligned sample roundtrip %d -> %dms -> %d", samples, ms, back)
		}
	}

	ms, err := SamplesToMS(49, 48000, "residual", ledger)
	if err != nil {
		t.Fatal(err)
	}
	if ms != 1 {
		t.Fatalf("floor conversion = %d, want 1", ms)
	}
	last := ledger.Records[len(ledger.Records)-1]
	if last.Remainder != 1 {
		t.Fatalf("remainder = %d, want 1", last.Remainder)
	}

	if got := PermilleToPermyriad(123, "w", ledger); got != 1230 {
		t.Fatalf("scale up = %d", got)
	}
	down, residual := PermyriadToPermille(1234, "w", ledger)
	if down != 123 || residual != 4 {
		t.Fatalf("scale down = %d rem %d", down, residual)
	}
}

func TestOverflowIncludesOperands(t *testing.T) {
	_, err := QuantizeFrame(math.MaxInt64, 30, 48000, "overflow.path")
	de, ok := domain.AsError(err)
	if !ok || de.Code != domain.CodeOvf {
		t.Fatalf("expected E_OVF, got %v", err)
	}
	if de.OperandA == "" || de.OperandB == "" || de.Path != "overflow.path" {
		t.Fatalf("overflow error missing path/operands: %+v", de)
	}
}
