package scene_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"volumekernel/internal/generator"
	"volumekernel/internal/model"
	"volumekernel/internal/scene"
)

func validScene(t *testing.T) model.Scene {
	t.Helper()
	s := generator.Generate(generator.DefaultSeed)
	if err := scene.Validate(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func requireCode(t *testing.T, err error, code model.Code) *model.Error {
	t.Helper()
	var ke *model.Error
	if !errors.As(err, &ke) {
		t.Fatalf("expected %s error, got %v", code, err)
	}
	if ke.Code != code {
		t.Fatalf("expected %s, got %s: %v", code, ke.Code, err)
	}
	return ke
}

func TestGeneratorIsDeterministic(t *testing.T) {
	a := generator.Generate(generator.DefaultSeed)
	b := generator.Generate(generator.DefaultSeed)
	ha, err := model.HashScene(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := model.HashScene(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatalf("deterministic seed produced different scenes: %s vs %s", ha, hb)
	}
	c := generator.Generate(generator.DefaultSeed + 1)
	hc, _ := model.HashScene(c)
	if ha == hc {
		t.Fatal("different seeds produced the same scene")
	}
}

func TestJSONRoundTripAndTruncation(t *testing.T) {
	s := validScene(t)
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := scene.LoadJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != s.ID {
		t.Fatal("round-trip changed scene")
	}
	_, err = scene.LoadJSON(data[:len(data)/2])
	ke := requireCode(t, err, model.EDecode)
	if ke.Message == "" {
		t.Fatal("decode error lacks message")
	}
	if ke.MediumID == "" {
		t.Fatal("truncated medium payload should preserve a best-effort medium id")
	}
	_, err = scene.LoadJSON(append(data, []byte("{}")...))
	requireCode(t, err, model.EDecode)
}

func TestBoundsAndCoefficientValidation(t *testing.T) {
	s := validScene(t)
	s.Media[0].Bounds.Min.X = s.Media[0].Bounds.Max.X + 1
	ke := requireCode(t, scene.Validate(s), model.EPara)
	if ke.MediumID != s.Media[0].ID || ke.Field != "bounds" {
		t.Fatalf("missing location: %+v", ke)
	}

	s = validScene(t)
	s.Media[1].Scattering = -0.01
	ke = requireCode(t, scene.Validate(s), model.EPara)
	if ke.MediumID != s.Media[1].ID || ke.Field != "scattering" {
		t.Fatalf("missing coefficient location: %+v", ke)
	}

	s = validScene(t)
	s.Media[2].Absorption = 0.123456789
	ke = requireCode(t, scene.Validate(s), model.EPara)
	if ke.MediumID != s.Media[2].ID || ke.Field != "absorption" {
		t.Fatalf("missing fixed-scale location: %+v", ke)
	}
}

func TestDensityAndOrderValidation(t *testing.T) {
	s := validScene(t)
	s.Media[0].Density.Values = s.Media[0].Density.Values[:len(s.Media[0].Density.Values)-1]
	ke := requireCode(t, scene.Validate(s), model.EDensity)
	if !strings.Contains(ke.Message, "density values") || ke.MediumID == "" {
		t.Fatalf("density error lacks location: %v", ke)
	}

	s = validScene(t)
	s.Media[1].Density.NX = scene.MaxDensityResolution + 1
	ke = requireCode(t, scene.Validate(s), model.EDensity)
	if ke.MediumID != s.Media[1].ID || ke.Field == "" {
		t.Fatalf("resolution error lacks location: %v", ke)
	}

	s = validScene(t)
	s.CameraTrack.Samples[0].Time, s.CameraTrack.Samples[1].Time =
		s.CameraTrack.Samples[1].Time, s.CameraTrack.Samples[0].Time
	ke = requireCode(t, scene.Validate(s), model.EOrder)
	if !strings.Contains(ke.Message, "time") {
		t.Fatalf("order error lacks context: %v", ke)
	}
	if ke.CameraID == "" {
		t.Fatal("order error must identify the offending camera")
	}
}

func TestInjectionCodes(t *testing.T) {
	s := validScene(t)
	cases := []struct {
		kind scene.InjectionKind
		code model.Code
	}{
		{scene.InjectDecode, model.EDecode},
		{scene.InjectOrder, model.EOrder},
		{scene.InjectDens, model.EDensity},
		{scene.InjectBlock, model.EBlock},
	}
	for _, tc := range cases {
		_, err := scene.Inject(s, tc.kind)
		requireCode(t, err, tc.code)
	}
}
