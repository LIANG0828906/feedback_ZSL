package scene_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"ms-workbench/internal/apperr"
	"ms-workbench/internal/samples"
	"ms-workbench/internal/scene"
)

func loadPackage(t *testing.T) *samples.Package {
	t.Helper()
	pkg, err := samples.Generate(samples.Options{Seed: 20261125, Width: 64, Height: 48}, "test-scene", "Test Scene")
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

func TestThreeSchemaVersionsLoadIdenticalSemantics(t *testing.T) {
	pkg := loadPackage(t)
	var loaded []*scene.Scene
	for version := 1; version <= 3; version++ {
		s, err := scene.Decode(pkg.Files[version])
		if err != nil {
			t.Fatalf("decode v%d: %v", version, err)
		}
		if s.SourceVersion != version || s.SchemaVersion != version {
			t.Fatalf("v%d source/current = %d/%d", version, s.SourceVersion, s.SchemaVersion)
		}
		loaded = append(loaded, s)
	}
	want := pkg.Scene
	for i, got := range loaded {
		if got.SceneID != want.SceneID || got.Name != want.Name || got.Width != want.Width || got.Height != want.Height {
			t.Fatalf("v%d identity mismatch", i+1)
		}
		if got.BitDepth != want.BitDepth || got.PixelPitchUM != want.PixelPitchUM {
			t.Fatalf("v%d geometry mismatch", i+1)
		}
		if !reflect.DeepEqual(got.WhitePoint, want.WhitePoint) || !reflect.DeepEqual(got.Response, want.Response) {
			t.Fatalf("v%d camera/white mismatch", i+1)
		}
		if got.Settings != want.Settings {
			t.Fatalf("v%d settings mismatch: %+v != %+v", i+1, got.Settings, want.Settings)
		}
		if len(got.Bands) != len(want.Bands) {
			t.Fatalf("v%d band count mismatch", i+1)
		}
		for j := range got.Bands {
			gb, wb := got.Bands[j], want.Bands[j]
			if gb.ID != wb.ID || gb.CenterNM != wb.CenterNM || gb.ResponseXYZ != wb.ResponseXYZ ||
				gb.OffsetX != wb.OffsetX || gb.OffsetY != wb.OffsetY {
				t.Fatalf("v%d band %d metadata mismatch", i+1, j)
			}
			if !reflect.DeepEqual(gb.Detail, wb.Detail) {
				t.Fatalf("v%d band %d detail mismatch", i+1, j)
			}
			if len(gb.Exposures) != len(wb.Exposures) {
				t.Fatalf("v%d band %d exposure count mismatch", i+1, j)
			}
			for k := range gb.Exposures {
				if gb.Exposures[k].DurationUS != wb.Exposures[k].DurationUS ||
					!reflect.DeepEqual(gb.Exposures[k].Codes, wb.Exposures[k].Codes) {
					t.Fatalf("v%d band %d exposure %d mismatch", i+1, j, k)
				}
			}
		}
		if !reflect.DeepEqual(got.Truth, want.Truth) || !reflect.DeepEqual(got.Patches, want.Patches) {
			t.Fatalf("v%d truth mismatch", i+1)
		}
	}
}

func TestUnknownVersionAndMissingFieldErrors(t *testing.T) {
	pkg := loadPackage(t)
	var obj map[string]any
	if err := json.Unmarshal(pkg.Files[3], &obj); err != nil {
		t.Fatal(err)
	}
	obj["schemaVersion"] = 99
	bad, _ := json.Marshal(obj)
	_, err := scene.Decode(bad)
	if !apperr.IsCode(err, apperr.EVER) {
		t.Fatalf("unknown version err = %v", err)
	}

	obj["schemaVersion"] = 3
	delete(obj, "bitsPerSample")
	bad, _ = json.Marshal(obj)
	_, err = scene.Decode(bad)
	if !apperr.IsCode(err, apperr.ESCHEMA) {
		t.Fatalf("missing field err = %v", err)
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Path != "$.bitsPerSample" || ae.SourceVersion != 3 {
		t.Fatalf("missing field context = %#v", ae)
	}
}

func TestV2PartialRenderUsesDeclaredDefaults(t *testing.T) {
	pkg := loadPackage(t)
	var obj map[string]any
	if err := json.Unmarshal(pkg.Files[2], &obj); err != nil {
		t.Fatal(err)
	}
	render := obj["render"].(map[string]any)
	delete(render, "fusionStrength")
	delete(render, "interpolationOrder")
	data, _ := json.Marshal(obj)
	s, err := scene.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	def := scene.DefaultSettings()
	if s.Settings.FusionStrength != def.FusionStrength || s.Settings.InterpolationOrder != def.InterpolationOrder {
		t.Fatalf("partial render defaults = %+v", s.Settings)
	}
	if s.Present["render.fusionStrength"] || s.Present["render.interpolationOrder"] {
		t.Fatal("missing optional fields should remain absent until upgrade")
	}
	up, err := scene.Upgrade(s, 3)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tr := range up.Trace {
		if tr.Path == "render.fusionStrength" {
			found = true
		}
	}
	if !found {
		t.Fatal("upgrade did not record the default fill trace")
	}
}

func TestUpgradeTraceDowngradeLossAndDeterministicBytes(t *testing.T) {
	pkg := loadPackage(t)
	v1, err := scene.Decode(pkg.Files[1])
	if err != nil {
		t.Fatal(err)
	}
	up, err := scene.Upgrade(v1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(up.Trace) != 7 {
		t.Fatalf("upgrade trace count = %d, want 7", len(up.Trace))
	}
	for _, tr := range up.Trace {
		if tr.Action != "upgrade" || tr.Path == "" || tr.DefaultSource != "schema-v3-defaults" {
			t.Fatalf("bad trace: %+v", tr)
		}
	}
	down, losses, err := scene.Downgrade(up, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(losses) != 7 {
		t.Fatalf("losses = %d, want 7", len(losses))
	}
	for _, loss := range losses {
		if loss.Target != 1 || loss.Disposition == "" {
			t.Fatalf("bad loss: %+v", loss)
		}
	}
	data, _, err := scene.Export(down, 1)
	if err != nil {
		if !apperr.IsCode(err, apperr.ELOSSY) {
			t.Fatal(err)
		}
	}
	again, _, _ := scene.Export(down, 1)
	if !bytes.Equal(data, again) {
		t.Fatal("repeated schema export is not byte-identical")
	}
	reloaded, err := scene.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	reup, err := scene.Upgrade(reloaded, 3)
	if err != nil {
		t.Fatal(err)
	}
	if reup.Settings != up.Settings {
		t.Fatalf("round-trip settings changed: %+v != %+v", reup.Settings, up.Settings)
	}

	lossyData, losses, err := scene.Export(up, 1)
	if !apperr.IsCode(err, apperr.ELOSSY) || len(losses) != 7 || len(lossyData) == 0 {
		t.Fatalf("expected E_LOSSY with bytes, got err=%v losses=%d bytes=%d", err, len(losses), len(lossyData))
	}
}

func TestSecondsConversionIsIntegerAndExact(t *testing.T) {
	cases := map[string]int64{
		"0.0015":   1500,
		"0.024":    24000,
		"1":        1_000_000,
		"0.000001": 1,
	}
	for input, want := range cases {
		got, err := scene.ParseSecondsToMicros(input)
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if got != want {
			t.Fatalf("%s => %d, want %d", input, got, want)
		}
	}
	if _, err := scene.ParseSecondsToMicros("0.0000001"); err == nil {
		t.Fatal("expected sub-microsecond value to be rejected")
	}
	if scene.FormatMicrosAsSeconds(1500) != "0.0015" {
		t.Fatalf("format = %q", scene.FormatMicrosAsSeconds(1500))
	}
}
