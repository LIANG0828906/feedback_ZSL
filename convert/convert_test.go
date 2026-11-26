package convert_test

import (
	"math"
	"testing"

	"iconflow/convert"
	"iconflow/errs"
	"iconflow/model"
)

func TestPermillePermyriadRoundTripResidual(t *testing.T) {
	for _, value := range []int{850, 853, 999, 1} {
		permyriad, trace, err := convert.PermilleToPermyriad(value, "$.threshold")
		if err != nil {
			t.Fatal(err)
		}
		back, downTrace, err := convert.PermyriadToPermille(permyriad, model.RoundFloor, "$.threshold")
		if err != nil {
			t.Fatal(err)
		}
		if back != value {
			t.Fatalf("exact conversion changed %d -> %d", value, back)
		}
		if trace.Operation == "" || downTrace.Operation == "" {
			t.Fatal("conversion trace missing")
		}
	}

	_, trace, err := convert.PermyriadToPermille(8535, model.RoundFloor, "$.threshold")
	if err != nil {
		t.Fatal(err)
	}
	if trace.Remainder != 5 || !convert.RemainderWithin(5, 9) {
		t.Fatalf("expected remainder 5, got %+v", trace)
	}
	ceil, _, err := convert.PermyriadToPermille(8535, model.RoundCeil, "$.threshold")
	if err != nil || ceil != 854 {
		t.Fatalf("ceil = %d err=%v", ceil, err)
	}
}

func TestBlockOriginAndChannelOffsets(t *testing.T) {
	x, y, tx, ty, traces, err := convert.BlockOrigin(7, 4, 16, 16, "$.block")
	if err != nil {
		t.Fatal(err)
	}
	if tx != 3 || ty != 1 || x != 48 || y != 16 {
		t.Fatalf("unexpected origin %d,%d tile %d,%d", x, y, tx, ty)
	}
	if len(traces) != 1 || traces[0].Remainder != 3 {
		t.Fatalf("missing divmod trace: %+v", traces)
	}
	absX, oerr := convert.AbsoluteCoordinate(x, 5, "$.x")
	if oerr != nil || absX != 53 {
		t.Fatalf("absolute x=%d err=%v", absX, oerr)
	}
	flat, _, err := convert.ChannelOffset(model.LayoutFlat, 2, 3, 16, 16, 1, 4, "$.flat")
	if err != nil || flat != (3*16+2)*4+1 {
		t.Fatalf("flat offset=%d err=%v", flat, err)
	}
	grouped, _, err := convert.ChannelOffset(model.LayoutGrouped, 2, 3, 16, 16, 1, 4, "$.group")
	if err != nil || grouped != 1*256+3*16+2 {
		t.Fatalf("grouped offset=%d err=%v", grouped, err)
	}
}

func TestQuantizedCoordinateRoundTripResidual(t *testing.T) {
	index, residual, trace, err := convert.QuantizeCoordinate(33, 6, model.RoundFloor, "$.quant.x")
	if err != nil {
		t.Fatal(err)
	}
	if index != 5 || residual != 3 || trace.Remainder != 3 {
		t.Fatalf("unexpected quantization index=%d residual=%d trace=%+v", index, residual, trace)
	}
	origin, err := convert.DequantizeCoordinate(index, 6, "$.quant.x")
	if err != nil || origin != 30 {
		t.Fatalf("origin=%d err=%v", origin, err)
	}
	if !convert.RemainderWithin(3, int64(6-1)) {
		t.Fatal("residual should be within declared step-1 bound")
	}
}

func TestOverflowReturnsEOvfWithOperands(t *testing.T) {
	_, err := convert.AbsoluteCoordinate(math.MaxInt64, 1, "$.overflow")
	if err == nil || err.Code != errs.EOvf {
		t.Fatalf("expected E_OVF, got %+v", err)
	}
	if err.Op1 == nil || err.Op2 == nil || err.Field != "$.overflow" {
		t.Fatalf("overflow error missing operands/field: %+v", err)
	}
}
