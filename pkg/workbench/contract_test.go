package workbench

import "testing"

func TestSceneJSONRoundTripAndValidation(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/scene.json"
	if err := SaveSceneFile(path, scene); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSceneFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != scene.ID || len(loaded.Bands) != len(scene.Bands) || len(loaded.Exposures) != len(scene.Exposures) {
		t.Fatalf("loaded scene shape differs: %+v", loaded)
	}
	if _, err := LoadScene([]byte(`{"metadata":{"width":0}}`)); AsWorkbenchError(err).Code != CodeMeta {
		t.Fatalf("expected E_META, got %v", err)
	}
}

func TestValidationErrorCodes(t *testing.T) {
	base, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Run("E_META", func(t *testing.T) {
		scene := cloneScene(base)
		scene.Meta.PixelPitchMicrons = 0
		we := AsWorkbenchError(ValidateScene(scene))
		if we.Code != CodeMeta || we.Field != "pixelPitchMicrons" {
			t.Fatalf("got %+v", we)
		}
	})
	t.Run("E_BAND", func(t *testing.T) {
		scene := cloneScene(base)
		scene.Bands = scene.Bands[:len(scene.Bands)-1]
		we := AsWorkbenchError(ValidateScene(scene))
		if we.Code != CodeBand || we.Field != "bands" {
			t.Fatalf("got %+v", we)
		}
	})
	t.Run("E_ORDER", func(t *testing.T) {
		scene := cloneScene(base)
		scene.Meta.ExposureTimes[1], scene.Meta.ExposureTimes[0] = scene.Meta.ExposureTimes[0], scene.Meta.ExposureTimes[1]
		we := AsWorkbenchError(ValidateScene(scene))
		if we.Code != CodeOrder || we.Field != "exposureTimes[1]" {
			t.Fatalf("got %+v", we)
		}
	})
}

func TestRegistrationAndParameterErrors(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultProcessConfig()
	cfg.MaxRegistrationOffset = 0.5
	_, err = RenderScene(scene, cfg, PathFull)
	we := AsWorkbenchError(err)
	if we.Code != CodeReg || we.BandPair == "" || we.Offset == nil {
		t.Fatalf("got %+v", we)
	}
	cfg = DefaultProcessConfig()
	cfg.InterpolationOrder = 9
	_, err = RenderScene(scene, cfg, PathFull)
	we = AsWorkbenchError(err)
	if we.Code != CodePara || we.Field != "interpolationOrder" {
		t.Fatalf("got %+v", we)
	}
}

func TestGamutStrictErrorCarriesPixelAndChannel(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultProcessConfig()
	cfg.GamutIntent = IntentStrict
	_, err = RenderScene(scene, cfg, PathFull)
	we := AsWorkbenchError(err)
	if we.Code != CodeGamut || we.Pixel == nil || we.Channel < 0 {
		t.Fatalf("got %+v", we)
	}
}

func TestScaleMismatchReturnsBlockAndThreeMetrics(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultProcessConfig()
	a, err := RenderScene(scene, cfg, PathFull)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenderScene(scene, cfg, PathTiled)
	if err != nil {
		t.Fatal(err)
	}
	b.FusedRadiance[7] += 0.01
	b.LinearRGB[30] += 0.01
	b.GamutMask[0] ^= 1
	b.OutOfGamutHash = "deliberate-mismatch"
	report, err := CompareRenderResults(a, b, b.Scale.TileWidth, b.Scale.TileHeight, scene.Truth.Tolerance)
	we := AsWorkbenchError(err)
	if we.Code != CodeScale {
		t.Fatalf("got %+v", we)
	}
	if we.Pixel == nil || we.Segment < 0 {
		t.Fatalf("missing pixel/segment: %+v", we)
	}
	if _, ok := we.Metrics["fusedMaxAbs"]; !ok {
		t.Fatalf("missing fused metric: %+v", we.Metrics)
	}
	if _, ok := we.Metrics["rgbMaxAbs"]; !ok {
		t.Fatalf("missing rgb metric: %+v", we.Metrics)
	}
	if _, ok := we.Metrics["gamutSetDiff"]; !ok {
		t.Fatalf("missing gamut metric: %+v", we.Metrics)
	}
	if report == nil || report.Consistent {
		t.Fatalf("report should be inconsistent: %+v", report)
	}
}

func TestAutoScaleSelectsTiledForLargeEffectivePixels(t *testing.T) {
	small := DefaultGenerateConfig()
	smallScale := DecideScale(Metadata{Width: small.Width, Height: small.Height, BandCount: small.Bands, ExposureCount: small.Exposures}, DefaultProcessConfig())
	if smallScale.SelectedPath != PathFull {
		t.Fatalf("small scene selected %s", smallScale.SelectedPath)
	}
	large := Metadata{Width: 2048, Height: 1536, BandCount: 8, ExposureCount: 4}
	cfg := DefaultProcessConfig()
	cfg.MemoryBudgetMB = 8
	largeScale := DecideScale(large, cfg)
	if largeScale.SelectedPath != PathTiled {
		t.Fatalf("large scene selected %s", largeScale.SelectedPath)
	}
	if largeScale.DownsampleFactor < 2 || largeScale.Rationale == "" {
		t.Fatalf("missing tiled strategy details: %+v", largeScale)
	}
}

func TestDetailStrengthIncreasesResidualEnergy(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	low := DefaultProcessConfig()
	low.DetailStrength = 0
	high := low
	high.DetailStrength = 1.2
	a, err := RenderScene(scene, low, PathFull)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenderScene(scene, high, PathFull)
	if err != nil {
		t.Fatal(err)
	}
	var residualEnergy float64
	for i := range a.FusedRadiance {
		delta := b.FusedRadiance[i] - a.FusedRadiance[i]
		residualEnergy += delta * delta
	}
	if residualEnergy < 1e-8 {
		t.Fatal("detail strength produced no residual energy")
	}
}

func cloneScene(src *Scene) *Scene {
	dst := *src
	dst.Meta = src.Meta
	dst.Meta.ExposureTimes = append([]float64(nil), src.Meta.ExposureTimes...)
	dst.Meta.Bands = append([]BandMeta(nil), src.Meta.Bands...)
	dst.Bands = append([]BandImage(nil), src.Bands...)
	dst.Exposures = append([]ExposureFrame(nil), src.Exposures...)
	dst.Detail = append([]float64(nil), src.Detail...)
	dst.Truth = src.Truth
	dst.Truth.Registration = append([]RegistrationOffset(nil), src.Truth.Registration...)
	dst.Truth.Patches = append([]PatchTruth(nil), src.Truth.Patches...)
	return &dst
}
