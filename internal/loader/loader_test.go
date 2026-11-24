package loader

import (
	"testing"

	"spatialir/internal/generator"
	"spatialir/internal/model"
)

func loadDataset(t *testing.T, faults model.FaultSpec) (*Dataset, string) {
	t.Helper()
	dir := t.TempDir()
	spec := generator.BuiltinSpecs()[0]
	spec.Faults = faults
	_, _, _, err := generator.Generate(spec, dir)
	if err != nil {
		t.Fatal(err)
	}
	ds, err := Load(dir, spec.Name+"_manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	return ds, dir
}

func TestLoadAndMetaErrors(t *testing.T) {
	ds, _ := loadDataset(t, model.FaultSpec{})
	if ds == nil || len(ds.Signals) != 2 {
		t.Fatal("dataset not loaded")
	}

	bad := *ds.Manifest
	bad.SampleRate = 0
	if err := validateMeta(&bad); err == nil || model.AsAppError(err).Code != model.CodeMeta ||
		model.AsAppError(err).Field != "sample_rate" {
		t.Fatalf("expect E_META sample_rate, got %v", err)
	}
	bad2 := *ds.Manifest
	bad2.Layout.Names = nil
	if err := validateMeta(&bad2); model.AsAppError(err).Code != model.CodeMeta {
		t.Fatalf("expect E_META layout, got %v", err)
	}
	bad3 := *ds.Manifest
	bad3.Layout.Channels = 0
	if ae := model.AsAppError(validateMeta(&bad3)); ae == nil || ae.Field != "layout.channels" {
		t.Fatalf("expect E_META layout.channels, got %v", bad3)
	}
	bad4 := *ds.Manifest
	bad4.LengthSamples = 0
	if ae := model.AsAppError(validateMeta(&bad4)); ae == nil || ae.Field != "length_samples" {
		t.Fatalf("expect E_META length_samples, got %v", bad4)
	}
}

func TestMissingChannelReturnsEChan(t *testing.T) {
	dir := t.TempDir()
	spec := generator.BuiltinSpecs()[0]
	spec.Faults = model.FaultSpec{MissingChannel: "L"}
	if _, _, _, err := generator.Generate(spec, dir); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir, spec.Name+"_manifest.json")
	ae := model.AsAppError(err)
	if ae == nil || ae.Code != model.CodeChan || ae.Channel != "L" {
		t.Fatalf("expect E_CHAN L, got %v", err)
	}
}

func TestValidateParams(t *testing.T) {
	ds, _ := loadDataset(t, model.FaultSpec{})
	good := model.FrameParams{
		FrameLength: 1024, FrameHop: 512, WindowLen: 768, ConvWindow: 64,
		RenderBlock: 512, InterpOrder: 1, BoundScale: 1, MaxWorkers: 2, BufferCap: 2048,
	}
	if err := ValidateParams(good, ds.Manifest.LengthSamples); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.FrameHop = 9999
	err := ValidateParams(bad, ds.Manifest.LengthSamples)
	if ae := model.AsAppError(err); ae == nil || ae.Code != model.CodePara || ae.Field != "frame_hop" {
		t.Fatalf("expect E_PARA frame_hop, got %v", err)
	}
	bad = good
	bad.ConvWindow = 0
	if ae := model.AsAppError(ValidateParams(bad, ds.Manifest.LengthSamples)); ae == nil ||
		ae.Field != "conv_window" {
		t.Fatalf("expect E_PARA conv_window")
	}
}
