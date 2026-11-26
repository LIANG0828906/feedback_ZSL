package dialogue

import (
	"math"
	"testing"
)

func TestFixedScaleRoundTripResidual(t *testing.T) {
	for _, value := range []int64{12005, -12005, 9995, 10005} {
		low, traces, err := ConvertScale(value, InternalScale, 1000, "gain", "test.to_permille")
		if err != nil {
			t.Fatal(err)
		}
		if len(traces) == 0 || traces[0].Remainder == 0 {
			t.Fatalf("expected truncation trace for %d", value)
		}
		high, _, err := ConvertScale(low, 1000, InternalScale, "gain", "test.to_myriad")
		if err != nil {
			t.Fatal(err)
		}
		if abs64(high-value) > 5 {
			t.Fatalf("roundtrip %d -> %d -> %d exceeds 5", value, low, high)
		}
	}
}

func TestTimeConversionsAreIntegerAndTraced(t *testing.T) {
	samples, traces, err := MillisecondsToSamples(20, 16000, "frame", "test.frame")
	if err != nil {
		t.Fatal(err)
	}
	if samples != 320 || len(traces) != 0 {
		t.Fatalf("20ms = %d samples traces=%d", samples, len(traces))
	}
	ms, traces, err := SamplesToMilliseconds(1, 16000, "sample", "test.sample")
	if err != nil {
		t.Fatal(err)
	}
	if ms != 0 || len(traces) != 1 || traces[0].Remainder != 1000 {
		t.Fatalf("1 sample -> %d ms traces=%+v", ms, traces)
	}
}

func TestScaleOverflowReturnsEOVFWithOperands(t *testing.T) {
	_, _, err := ConvertScale(math.MaxInt64, 1, InternalScale, "gain_limit", "test.overflow")
	if CodeOf(err) != CodeOVF {
		t.Fatalf("expected E_OVF, got %v", err)
	}
	kernelError := err.(*Error)
	if kernelError.Field != "gain_limit" || len(kernelError.Operands) != 2 {
		t.Fatalf("incomplete E_OVF: %+v", kernelError)
	}
}

func TestParallelMeasurementIsDeterministic(t *testing.T) {
	project, err := GenerateProject(42)
	if err != nil {
		t.Fatal(err)
	}
	sequential, err := MeasureProjectEnvelopes(project)
	if err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 5; run++ {
		parallel, err := MeasureProjectEnvelopesParallel(project)
		if err != nil {
			t.Fatal(err)
		}
		if len(parallel) != len(sequential) {
			t.Fatal("different segment count")
		}
		for i := range sequential {
			if sequential[i].Digest() != parallel[i].Digest() || len(sequential[i].Frames) != len(parallel[i].Frames) {
				t.Fatalf("parallel mismatch run %d segment %s", run, sequential[i].SegmentID)
			}
			for j := range sequential[i].Frames {
				if sequential[i].Frames[j].Envelope != parallel[i].Frames[j].Envelope ||
					sequential[i].Frames[j].Energy != parallel[i].Frames[j].Energy ||
					sequential[i].Frames[j].PeakPosition != parallel[i].Frames[j].PeakPosition {
					t.Fatalf("parallel frame mismatch run %d segment %s frame %d", run, sequential[i].SegmentID, j)
				}
			}
		}
	}
}

func TestGeneratorIsDeterministic(t *testing.T) {
	first, err := GenerateProject(42)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateProject(42)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.DeclaredTotalSamples != second.DeclaredTotalSamples ||
		len(first.PCM) != len(second.PCM) || len(first.Truth.Frames) != len(second.Truth.Frames) {
		t.Fatal("generator metadata differs")
	}
	for i := range first.PCM {
		if first.PCM[i] != second.PCM[i] {
			t.Fatalf("PCM differs at %d", i)
		}
	}
	for i := range first.Truth.Frames {
		if first.Truth.Frames[i] != second.Truth.Frames[i] {
			t.Fatalf("truth differs at frame %d", i)
		}
	}
}
