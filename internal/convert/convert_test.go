package convert_test

import (
	"bytes"
	"testing"

	"hdrproof/internal/apperr"
	"hdrproof/internal/convert"
	"hdrproof/internal/model"
	"hdrproof/internal/sample"
)

func TestConvertFrameDeterministicAndTruthAligned(t *testing.T) {
	dir := t.TempDir()
	if _, err := sample.Generate(sample.GenerateOptions{Dir: dir, Seed: 42}); err != nil {
		t.Fatal(err)
	}
	ds, err := sample.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	rd := ds.Routes[0]
	p := model.DefaultParams()
	a, err := convert.ConvertFrame(rd.Route, rd.DecodedFrames[0], rd.Truth.Frames[0], p, ds.TruthFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	b, err := convert.ConvertFrame(rd.Route, rd.DecodedFrames[0], rd.Truth.Frames[0], p, ds.TruthFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Pixels, b.Pixels) || !bytes.Equal(a.ProofJSON, b.ProofJSON) {
		t.Fatal("repeated conversion is not byte deterministic")
	}
	if a.Proof.RecordSHA256 == "" || a.Proof.RecordSHA256 != b.Proof.RecordSHA256 {
		t.Fatal("record hash missing or unstable")
	}
	for _, patch := range a.Proof.Patches {
		if patch.DeltaMax > 0.02 {
			t.Fatalf("patch %s exceeds truth tolerance: %f", patch.Name, patch.DeltaMax)
		}
	}
}

func TestParameterErrorField(t *testing.T) {
	p := model.DefaultParams()
	p.TargetPeakNits = 999
	err := convert.ValidateParams(p, "r", 3)
	if err == nil || apperr.CodeOf(err) != apperr.CodePara {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	info, _ := apperr.As(err)
	if info.Field != "target_peak_nits" || info.Segment != 3 {
		t.Fatalf("unexpected error context: %+v", info)
	}
}
