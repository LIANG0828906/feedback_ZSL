package interop_test

import (
	"errors"
	"testing"

	"storyboardstudio/internal/interop"
	"storyboardstudio/internal/model"
)

func TestThreeVersionsUpgradeExportAndStableBytes(t *testing.T) {
	doc := &interop.Document{NodeID: "n01_office", Shots: []model.Shot{{
		ID: "shot-1", DurationMS: 1250, DurationPresent: true, AspectPath: "aspect://16x9",
		Camera: "close", Motion: "dolly", Transition: "fade",
	}}}

	v3First, err := interop.Encode(doc, interop.Version3)
	if err != nil {
		t.Fatal(err)
	}
	v3Second, err := interop.Encode(doc, interop.Version3)
	if err != nil {
		t.Fatal(err)
	}
	if string(v3First.Data) != string(v3Second.Data) {
		t.Fatal("v3 export is not byte-stable")
	}
	decoded, report, err := interop.Decode(v3First.Data)
	if err != nil {
		t.Fatal(err)
	}
	if report == nil || len(report.UpgradeTrace) == 0 {
		t.Fatal("expected v3 conversion trace")
	}
	if decoded.Shots[0].DurationMS != 1250 || decoded.Shots[0].Motion != "dolly" {
		t.Fatalf("v3 roundtrip lost data: %+v", decoded.Shots[0])
	}

	v2, err := interop.Encode(doc, interop.Version2)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Lossy == nil || v2.Lossy.Code != model.CodeLossy || len(v2.Lossy.LossyFields) == 0 {
		t.Fatalf("expected E_LOSSY for motion export to v2: %+v", v2.Lossy)
	}
	v2Decoded, upgrade, err := interop.Decode(v2.Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(upgrade.UpgradeTrace) == 0 || v2Decoded.Shots[0].Motion != "static" {
		t.Fatalf("v2 upgrade defaults/traces missing: %+v", upgrade)
	}

	v1, err := interop.Encode(doc, interop.Version1)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Lossy == nil || len(v1.Lossy.LossyFields) < 2 {
		t.Fatalf("expected duration and presentation fields to be lossy in v1: %+v", v1.Lossy)
	}
	v1Stable, err := interop.Encode(doc, interop.Version1)
	if err != nil {
		t.Fatal(err)
	}
	if string(v1.Data) != string(v1Stable.Data) {
		t.Fatal("v1 export is not byte-stable")
	}
	v1Decoded, v1Upgrade, err := interop.Decode(v1.Data)
	if err != nil {
		t.Fatal(err)
	}
	if v1Decoded.Shots[0].DurationMS != 1200 {
		t.Fatalf("expected declared decisecond truncation to 1200ms, got %d", v1Decoded.Shots[0].DurationMS)
	}
	if len(v1Upgrade.UpgradeTrace) < 3 {
		t.Fatalf("expected upgrade trace, got %+v", v1Upgrade)
	}
}

func TestUnknownVersionHasDistinctErrorCode(t *testing.T) {
	_, _, err := interop.Decode([]byte(`{"schema":"storyboard/0.1"}`))
	var coded *model.Error
	if !errors.As(err, &coded) || coded.Code != model.CodeVersion {
		t.Fatalf("expected E_VERSION, got %v", err)
	}
}
