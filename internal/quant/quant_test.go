package quant_test

import (
	"testing"

	"spectralforge/internal/domain"
	"spectralforge/internal/errs"
	"spectralforge/internal/quant"
	"spectralforge/internal/sample"
)

func bundle(t *testing.T) *domain.SampleBundle {
	t.Helper()
	dir := t.TempDir()
	b, err := sample.Generate(dir, 91)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestQuantizationErrorStatsAreMonotoneWithBitDepth(t *testing.T) {
	b := bundle(t)
	var prev quant.ErrorStats
	for bits := 4; bits <= 12; bits++ {
		q := domain.Quantization{AlbedoBits: bits, RoughnessBits: bits, MetallicBits: bits, BasisBits: bits}
		set, err := quant.BuildSet(b.Scene, q, quant.NewCache())
		if err != nil {
			t.Fatalf("bits %d: %v", bits, err)
		}
		if bits > 4 {
			if set.Stats.MeanAbs > prev.MeanAbs+1e-15 ||
				set.Stats.MaxAbs > prev.MaxAbs+1e-15 ||
				set.Stats.RMSE > prev.RMSE+1e-15 {
				t.Fatalf("error increased from bits %d to %d: prev=%+v now=%+v", bits-1, bits, prev, set.Stats)
			}
		}
		prev = set.Stats
	}
}

func TestMaterialCacheRejectsQuantizationMix(t *testing.T) {
	b := bundle(t)
	cache := quant.NewCache()
	obj := b.Scene.Objects[0]
	q8 := domain.DefaultQuantization()
	if _, err := cache.GetOrBuild(b.Scene, obj, q8); err != nil {
		t.Fatal(err)
	}
	q10 := q8
	q10.BasisBits = 10
	_, err := cache.GetOrBuild(b.Scene, obj, q10)
	if err == nil {
		t.Fatal("expected E_QMIX")
	}
	e, ok := errs.As(err)
	if !ok || e.Code != errs.CodeQMix {
		t.Fatalf("expected E_QMIX, got %v", err)
	}
	cache.Reset()
	if _, err := cache.GetOrBuild(b.Scene, obj, q10); err != nil {
		t.Fatalf("reset cache should accept new quantization: %v", err)
	}
}

func TestMaterialKeyIncludesContentAndQuantization(t *testing.T) {
	b := bundle(t)
	obj := b.Scene.Objects[0]
	q1 := domain.DefaultQuantization()
	q2 := q1
	q2.AlbedoBits++
	k1 := domain.ObjectMaterialKey(b.Scene, obj, q1)
	k2 := domain.ObjectMaterialKey(b.Scene, obj, q2)
	if k1 == k2 {
		t.Fatal("material key did not change with quantization")
	}
	if domain.ObjectContentHash(b.Scene, obj) == "" {
		t.Fatal("content hash missing")
	}
	if domain.ObjectMaterialKey(b.Scene, obj, q1) != k1 {
		t.Fatal("material key is not deterministic")
	}
}
