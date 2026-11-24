package spectralrender

import (
	"math"
	"testing"
)

func TestSampleGridAndIntegrationMethods(t *testing.T) {
	s := loadSamples(t)
	n := 35
	w, err := SampleGrid(s.scene.Spectrum.StartNM, s.scene.Spectrum.EndNM, n)
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != n || w[0] != s.scene.Spectrum.StartNM || w[n-1] != s.scene.Spectrum.EndNM {
		t.Fatalf("bad sample grid: len=%d endpoints=%g/%g", len(w), w[0], w[len(w)-1])
	}
	rangeNM := s.scene.Spectrum.EndNM - s.scene.Spectrum.StartNM
	for _, method := range []IntegrationMethod{IntegrationRectangle, IntegrationTrapezoid, IntegrationSimpson} {
		weights, ierr := IntegrationWeights(w, method)
		if ierr != nil {
			t.Fatal(ierr)
		}
		sum := 0.0
		for _, x := range weights {
			sum += x
		}
		if method == IntegrationRectangle {
			if math.Abs(sum-rangeNM) > 1e-9 {
				t.Fatalf("%s weight sum=%g, want %g", method, sum, rangeNM)
			}
		} else if math.Abs(sum-rangeNM) > 1e-9 {
			t.Fatalf("%s weight sum=%g, want %g", method, sum, rangeNM)
		}
	}
	_, perr := SampleGrid(0, 1, 3)
	if perr == nil || perr.Code != ErrPara || perr.Field != "sample_count" {
		t.Fatalf("expected E_PARA sample_count, got %v", perr)
	}
}

func TestParameterValidationErrors(t *testing.T) {
	p := DefaultRenderParams()
	p.SampleCount = 3
	if err := p.Validate(); err == nil || err.Code != ErrPara || err.Field != "sample_count" {
		t.Fatalf("expected E_PARA sample_count, got %v", err)
	}
	p = DefaultRenderParams()
	p.Integration = "bogus"
	if err := p.Validate(); err == nil || err.Code != ErrPara || err.Field != "integration" {
		t.Fatalf("expected E_PARA integration, got %v", err)
	}
	p = DefaultRenderParams()
	p.Gamma = 0
	if err := p.Validate(); err == nil || err.Code != ErrPara || err.Field != "gamma" {
		t.Fatalf("expected E_PARA gamma, got %v", err)
	}
}

func TestSpectralIntegrationTruthAndRepeatability(t *testing.T) {
	s := loadSamples(t)
	p := DefaultRenderParams()
	p.Version = 1
	f1, _, err := Render(s.scene, s.hash, p, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f2, _, err := Render(s.scene, s.hash, p, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !sameFloatBits(f1.LinearRGB, f2.LinearRGB) || !sameBytes(f1.EncodedRGB, f2.EncodedRGB) {
		t.Fatal("repeated render is not bitwise deterministic")
	}
	check, cerr := CheckTruthFrame(f1, s.truth)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if !check.Pass {
		t.Fatalf("truth check failed: max rgb=%g max chroma=%g", check.MaxLinearRGB, check.MaxChromaticity)
	}
	if check.MaxLinearRGB > s.truth.Bounds.LinearRGBAbs+1e-12 {
		t.Fatalf("linear error %g above declared bound %g", check.MaxLinearRGB, s.truth.Bounds.LinearRGBAbs)
	}
}

func TestSupportedIntegrationMethodsAgainstTruth(t *testing.T) {
	s := loadSamples(t)
	for _, method := range []IntegrationMethod{IntegrationRectangle, IntegrationTrapezoid, IntegrationSimpson} {
		t.Run(string(method), func(t *testing.T) {
			p := DefaultRenderParams()
			p.Version = 2
			p.Integration = method
			frame, _, err := Render(s.scene, s.hash, p, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			check, cerr := CheckTruthFrame(frame, s.truth)
			if cerr != nil {
				t.Fatal(cerr)
			}
			if !check.Pass {
				t.Fatalf("%s max linear error %.9g exceeds declared tolerance %.9g", method,
					check.MaxLinearRGB, s.truth.Bounds.LinearRGBAbs)
			}
		})
	}
}

func TestWhitePointNormalization(t *testing.T) {
	s := loadSamples(t)
	p := DefaultRenderParams()
	eng, err := NewSpectralEngine(s.scene, p)
	if err != nil {
		t.Fatal(err)
	}
	white := RGB{s.scene.Spectrum.WhitePoint[0], s.scene.Spectrum.WhitePoint[1], s.scene.Spectrum.WhitePoint[2]}
	got := eng.WhiteNormalizedRGB(white)
	for i := 0; i < 3; i++ {
		if math.Abs(got[i]-1) > 1e-12 {
			t.Fatalf("white maps to %v, want unit RGB", got)
		}
	}
}

func TestGamutAndVersionErrors(t *testing.T) {
	p := DefaultRenderParams()
	p.Version = 3
	lf := &LinearFrame{
		Width: 1, Height: 1,
		LinearRGB:        []float64{-0.1, 0.2, 0.3},
		XYZ:              []float64{0.1, 0.2, 0.3},
		PeakRadiance:     []float64{0.2},
		ParamVersion:     3,
		SamplingVersion:  "s",
		MaterialCacheKey: "m",
		SceneHash:        "h",
	}
	_, err := EncodeDisplay(lf, p)
	if err == nil || err.Code != ErrGamut || err.Pixel == nil || err.Pixel.X != 0 || err.Channel != "R" {
		t.Fatalf("expected E_GAMUT at pixel 0 channel R, got %v", err)
	}
	pBad := p
	pBad.Version = 4
	_, err = EncodeDisplay(lf, pBad)
	if err == nil || err.Code != ErrVer || len(err.Versions) != 2 || err.Versions[0] != 3 || err.Versions[1] != 4 {
		t.Fatalf("expected E_VER versions [3,4], got %v", err)
	}
}

func TestQuantizationIsMonotonicAndKeyed(t *testing.T) {
	s := loadSamples(t)
	lastMax, lastMean, lastRMSE := math.Inf(1), math.Inf(1), math.Inf(1)
	keys := make(map[string]bool)
	for bits := 1; bits <= 24; bits++ {
		p := DefaultRenderParams()
		p.VertexBitsOverride = bits
		p.CoefficientBitsOverride = bits
		q, err := QuantizeScene(s.scene, p)
		if err != nil {
			t.Fatal(err)
		}
		stats := q.Report.OverallStats
		if stats.MaxAbs > lastMax+1e-12 || stats.MeanAbs > lastMean+1e-12 || stats.RMSE > lastRMSE+1e-12 {
			t.Fatalf("quantization error increased at %d bits: max %g->%g mean %g->%g rmse %g->%g",
				bits, lastMax, stats.MaxAbs, lastMean, stats.MeanAbs, lastRMSE, stats.RMSE)
		}
		if len(q.Report.Samples) == 0 {
			t.Fatal("expected per-sample quantization records")
		}
		if q.Report.MaterialCacheKey == "" {
			t.Fatal("empty material cache key")
		}
		keys[q.Report.MaterialCacheKey] = true
		lastMax, lastMean, lastRMSE = stats.MaxAbs, stats.MeanAbs, stats.RMSE
	}
	if len(keys) != 24 {
		t.Fatalf("quantization parameters did not change the material cache key: %d unique keys", len(keys))
	}
}

func TestVersionedPreviewAndExactSnapshot(t *testing.T) {
	s := loadSamples(t)
	store, err := NewParamStore(DefaultRenderParams())
	if err != nil {
		t.Fatal(err)
	}
	exact := BeginExact(store)
	exactFrame, _, xerr := exact.Render(s.scene, s.hash, RenderOptions{})
	if xerr != nil {
		t.Fatal(xerr)
	}
	// Same visible parameters, but a new version. Preview and exact must still
	// produce identical pixels because no values were mixed across the boundary.
	updated, uerr := store.Update(DefaultRenderParams())
	if uerr != nil {
		t.Fatal(uerr)
	}
	previewFrame, _, perr := RenderPreview(s.scene, s.hash, store, RenderOptions{})
	if perr != nil {
		t.Fatal(perr)
	}
	if updated.Version == exactFrame.ParamVersion {
		t.Fatal("expected a new preview version")
	}
	if !sameFloatBits(exactFrame.LinearRGB, previewFrame.LinearRGB) || !sameBytes(exactFrame.EncodedRGB, previewFrame.EncodedRGB) {
		t.Fatal("preview and exact differ despite identical visible parameters")
	}
	if _, verr := EncodeDisplay(&LinearFrame{
		Width: 1, Height: 1, LinearRGB: []float64{0, 0, 0}, XYZ: []float64{0, 0, 0},
		PeakRadiance: []float64{0}, ParamVersion: exactFrame.ParamVersion,
		SamplingVersion: "s", MaterialCacheKey: "m",
	}, updated); verr == nil || verr.Code != ErrVer {
		t.Fatalf("expected E_VER when mixing versions, got %v", verr)
	}
}

func sameFloatBits(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Float64bits(a[i]) != math.Float64bits(b[i]) {
			return false
		}
	}
	return true
}

func sameBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
