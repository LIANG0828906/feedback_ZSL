package quant_test

import (
	"encoding/json"
	"math"
	"sync"
	"testing"

	"lipsync.local/prototype/pkg/domain"
	"lipsync.local/prototype/pkg/quant"
	"lipsync.local/prototype/pkg/sample"
)

func TestSampleToFrameRoundingAndTruncation(t *testing.T) {
	cases := []struct {
		sample int64
		mode   string
		want   int
		adjust bool
	}{
		{479, domain.QuantizeFloor, 0, false},
		{479, domain.QuantizeCeil, 1, true},
		{479, domain.QuantizeNearest, 0, false},
		{959, domain.QuantizeNearest, 1, true},
		{960, domain.QuantizeNearest, 1, false},
		{1439, domain.QuantizeNearest, 1, false},
		{1440, domain.QuantizeNearest, 2, true},
	}
	for _, tc := range cases {
		got, tr, err := quant.SampleToFrame("d", "p", "start", tc.sample, 48000, 50, tc.mode)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want || tr.Adjusted != tc.adjust {
			t.Fatalf("sample=%d mode=%s got frame=%d adjusted=%v want=%d/%v", tc.sample, tc.mode, got, tr.Adjusted, tc.want, tc.adjust)
		}
		if tr.Sample != tc.sample || tr.Denominator != 48000 || tr.Direction != tc.mode {
			t.Fatalf("truncation evidence incomplete: %+v", tr)
		}
	}
}

func TestOverflowDiagnostics(t *testing.T) {
	_, _, err := quant.SampleToFrame("d", "p", "end", math.MaxInt64, 48000, 50, domain.QuantizeNearest)
	if code := domain.CodeOf(err); code != domain.ErrOverflow {
		t.Fatalf("want E_OVF, got %s: %v", code, err)
	}
	de := err.(*domain.Error)
	if de.Field != "sample*frame_rate" || de.OperandA != math.MaxInt64 || de.OperandB != 50 {
		t.Fatalf("overflow evidence incomplete: %+v", de)
	}
	_, err = quant.PermilleToBasisPoints(math.MaxInt64, "d", "p")
	if code := domain.CodeOf(err); code != domain.ErrOverflow {
		t.Fatalf("want weight E_OVF, got %s: %v", code, err)
	}
}

func TestQuantizationIsOrderAndConcurrencyIndependent(t *testing.T) {
	project, _, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	d := project.Dialogues[0]
	want, err := quant.QuantizeDialogue(d, project.Recipe.FrameRate, project.Recipe.Quantize)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(want)
	reversed := append([]domain.Phoneme(nil), d.Phonemes...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	d2 := d
	d2.Phonemes = reversed
	shuffled, err := quant.QuantizeDialogue(d2, project.Recipe.FrameRate, project.Recipe.Quantize)
	if err != nil {
		t.Fatal(err)
	}
	shuffledJSON, _ := json.Marshal(shuffled)
	if string(wantJSON) != string(shuffledJSON) {
		t.Fatalf("order changed quantized output:\n%s\n%s", wantJSON, shuffledJSON)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := quant.QuantizeDialogue(d2, project.Recipe.FrameRate, project.Recipe.Quantize)
			if err != nil {
				errs <- err
				return
			}
			b, _ := json.Marshal(got)
			if string(b) != string(wantJSON) {
				errs <- &domain.Error{Code: domain.ErrDiff, Field: "concurrent_quantization"}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestWeightScaleRoundTrip(t *testing.T) {
	basis, err := quant.PermilleToBasisPoints(173, "d", "p")
	if err != nil {
		t.Fatal(err)
	}
	if basis != 1730 {
		t.Fatalf("basis=%d", basis)
	}
	permille, err := quant.BasisPointsToPermille(1739, "d", "p")
	if err != nil {
		t.Fatal(err)
	}
	if permille != 173 {
		t.Fatalf("permille=%d", permille)
	}
}
