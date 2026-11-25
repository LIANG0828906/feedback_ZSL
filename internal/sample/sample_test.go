package sample_test

import (
	"testing"

	"semanticshading/internal/apierr"
	"semanticshading/internal/sample"
)

func TestGenerateDeterministic(t *testing.T) {
	a, err := sample.Generate(sample.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	b, err := sample.Generate(sample.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if a.InputHash != b.InputHash || a.ManifestHash != b.ManifestHash || a.Truth.TruthFingerprint != b.Truth.TruthFingerprint {
		t.Fatal("fixed-seed sample generation is not deterministic")
	}
	if len(a.Ref) != a.Manifest.Width*a.Manifest.Height*a.Manifest.Channels {
		t.Fatal("bad reference length")
	}
}

func TestSupportsThreeChannels(t *testing.T) {
	cfg := sample.DefaultConfig()
	cfg.Channels = 3
	s, err := sample.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sample.Load(s.Manifest, s.Ref, s.Recon, s.Labels); err != nil {
		t.Fatal(err)
	}
}

func TestValidationErrorCodesAndLocations(t *testing.T) {
	valid, err := sample.Generate(sample.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("schema", func(t *testing.T) {
		cfg := sample.DefaultConfig()
		cfg.Width = 63
		_, err := sample.Generate(cfg)
		ae, ok := apierr.As(err)
		if !ok || ae.Code != apierr.ESchema || ae.Field != "tile_size" {
			t.Fatalf("got %#v", err)
		}
	})

	t.Run("parameters", func(t *testing.T) {
		cfg := sample.DefaultConfig()
		cfg.QuantGear = 99
		_, err := sample.Generate(cfg)
		ae, ok := apierr.As(err)
		if !ok || ae.Code != apierr.EPara || ae.Field != "quant_gear" {
			t.Fatalf("got %#v", err)
		}
	})

	t.Run("decode", func(t *testing.T) {
		badRef := valid.Ref[:len(valid.Ref)-7]
		_, err := sample.Load(valid.Manifest, badRef, valid.Recon, valid.Labels)
		ae, ok := apierr.As(err)
		if !ok || ae.Code != apierr.EDecode || ae.Field != "ref_pixels" {
			t.Fatalf("got %#v", err)
		}
		if ae.Location.PixelX == nil || ae.Location.PixelY == nil {
			t.Fatalf("decode error lacks pixel location: %#v", ae)
		}
	})

	t.Run("label", func(t *testing.T) {
		labels := append([]byte(nil), valid.Labels...)
		x, y := 17, 17
		labels[y*valid.Manifest.Width+x] = 99
		_, err := sample.Load(valid.Manifest, valid.Ref, valid.Recon, labels)
		ae, ok := apierr.As(err)
		if !ok || ae.Code != apierr.ELabel || ae.Field != "labels" {
			t.Fatalf("got %#v", err)
		}
		if ae.Location.PixelX == nil || *ae.Location.PixelX != x || ae.Location.PixelY == nil || *ae.Location.PixelY != y {
			t.Fatalf("label location = %#v, want %d,%d", ae.Location, x, y)
		}
	})
}
