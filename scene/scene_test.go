package scene_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"diffrender/errs"
	"diffrender/sample"
	"diffrender/scene"
)

func TestThreeVersionsUnifyIntoSameModel(t *testing.T) {
	task, err := sample.GenerateTask(101)
	if err != nil {
		t.Fatal(err)
	}
	wantTrace := map[string]int{scene.Version1: 4, scene.Version2: 3, scene.Version3: 0}
	for _, version := range scene.SupportedVersions {
		data := task.VersionFiles[version]
		got, err := scene.Load(data, "scene-"+version+".json")
		if err != nil {
			t.Fatalf("version %s: %v", version, err)
		}
		if got.SourceVersion != version {
			t.Fatalf("version %s: source=%q", version, got.SourceVersion)
		}
		if got.WorkingVersion != scene.WorkingVersion {
			t.Fatalf("version %s: working=%q", version, got.WorkingVersion)
		}
		if len(got.Trace) != wantTrace[version] {
			t.Fatalf("version %s: trace count=%d, want %d", version, len(got.Trace), wantTrace[version])
		}
		for i, tr := range got.Trace {
			if tr.Field == "" || tr.OldValue != "<missing>" || tr.DefaultSource == "" {
				t.Fatalf("version %s trace %d is incomplete: %+v", version, i, tr)
			}
		}
		if err := scene.CompareSemantics(task.Truth, got, scene.Version3); err != nil {
			t.Fatalf("version %s semantics: %v", version, err)
		}
	}
}

func TestLoadVersionAndRequiredFieldErrors(t *testing.T) {
	unknown := []byte(`{"schema_version":"99.0"}`)
	_, err := scene.Load(unknown, "unknown.json")
	code, ok := errs.CodeOf(err)
	if !ok || code != errs.CodeVersion {
		t.Fatalf("unknown version error=%v", err)
	}
	e, _ := errs.As(err)
	if e.File != "unknown.json" || e.Field != "schema_version" {
		t.Fatalf("unknown version location=%+v", e)
	}

	task, err := sample.GenerateTask(102)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(task.VersionFiles[scene.Version1], &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "objects")
	broken, _ := json.Marshal(doc)
	_, err = scene.Load(broken, "missing.json")
	code, ok = errs.CodeOf(err)
	if !ok || code != errs.CodeVersion {
		t.Fatalf("missing field error=%v", err)
	}
	e, _ = errs.As(err)
	if e.File != "missing.json" || e.Field != "objects" {
		t.Fatalf("missing field location=%+v", e)
	}

	_, err = scene.Load([]byte(`{"objects":[]}`), "noschema.json")
	code, _ = errs.CodeOf(err)
	if code != errs.CodeVersion {
		t.Fatalf("missing schema error=%v", err)
	}
}

func TestExportIsByteStableAndRoundTrips(t *testing.T) {
	task, err := sample.GenerateTask(103)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range scene.SupportedVersions {
		opts := scene.ExportOptions{Version: version}
		first, err := scene.VerifyRoundTrip(task.Truth, opts)
		if err != nil {
			t.Fatalf("version %s round-trip: %v", version, err)
		}
		second, err := scene.Export(task.Truth, opts)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first.Data, second.Data) {
			t.Fatalf("version %s export is not byte-stable", version)
		}
		reloaded, err := scene.Load(first.Data, "reloaded-"+version+".json")
		if err != nil {
			t.Fatalf("version %s reload: %v", version, err)
		}
		if reloaded.SourceVersion != version {
			t.Fatalf("version %s reloaded source=%q", version, reloaded.SourceVersion)
		}
	}
}

func TestLossyDowngradeAndRoundMismatch(t *testing.T) {
	task, err := sample.GenerateTask(104)
	if err != nil {
		t.Fatal(err)
	}
	edited := task.Truth.Clone()
	edited.Spheres[0].Coat = 0.5
	edited.Planes[0].Coat = 0.25
	edited.Settings.ToneMapping = "srgb"
	edited.Settings.ShadowSoftness = 0.2
	originalCenter := edited.Spheres[0].Center
	originalPower := edited.Lights[0].RadiantPowerW
	_, err = scene.Export(edited, scene.ExportOptions{Version: scene.Version1})
	code, ok := errs.CodeOf(err)
	if !ok || code != errs.CodeLossy {
		t.Fatalf("expected E_LOSSY, got %v", err)
	}
	e, _ := errs.As(err)
	losses, ok := e.Details.([]scene.LossItem)
	if !ok || len(losses) < 3 {
		t.Fatalf("loss details=%#v", e.Details)
	}
	for _, item := range losses {
		if item.Field == "" || item.OriginalValue == "" || item.Disposition == "" {
			t.Fatalf("incomplete loss item: %+v", item)
		}
	}
	result, err := scene.Export(edited, scene.ExportOptions{Version: scene.Version1, AllowLossy: true})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := scene.Load(result.Data, "lossy.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := scene.CompareSemantics(edited, reloaded, scene.Version1); err != nil {
		t.Fatalf("lossy export changed geometry/light: %v", err)
	}
	if reloaded.Spheres[0].Center != originalCenter || reloaded.Lights[0].RadiantPowerW != originalPower {
		t.Fatal("lossy downgrade changed center or light power")
	}

	altered := task.Truth.Clone()
	altered.Spheres[0].Center.X += 1e-4
	err = scene.CompareSemantics(task.Truth, altered, scene.Version3)
	code, _ = errs.CodeOf(err)
	if code != errs.CodeRound {
		t.Fatalf("expected E_ROUND, got %v", err)
	}

	_, err = scene.Export(task.Truth, scene.ExportOptions{Version: "8.0"})
	code, _ = errs.CodeOf(err)
	if code != errs.CodeVersion {
		t.Fatalf("expected E_VER, got %v", err)
	}
}

func TestCompareSemanticsDetectsFirstField(t *testing.T) {
	task, err := sample.GenerateTask(105)
	if err != nil {
		t.Fatal(err)
	}
	altered := task.Truth.Clone()
	altered.Lights[0].RadiantPowerW += 0.5
	err = scene.CompareSemantics(task.Truth, altered, scene.Version3)
	if err == nil {
		t.Fatal("expected mismatch")
	}
	var pe *errs.Error
	if !errors.As(err, &pe) || pe.Code != errs.CodeRound {
		t.Fatalf("expected E_ROUND, got %v", err)
	}
	if pe.Field != "lights[0].radiantPowerW" {
		t.Fatalf("mismatch field=%q", pe.Field)
	}
}
