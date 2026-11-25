package fog

import (
	"testing"
)

func TestSmokeGeneratedTruth(t *testing.T) {
	s, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	t.Logf("pixel truths=%d density truths=%d expected edit blocks=%v",
		len(s.Truth.Pixels), len(s.Truth.DensityIntegrals), s.Truth.DensityInvalidation.ExpectedBlocks)
	if len(s.Truth.Pixels) == 0 {
		t.Fatal("no pixel truth")
	}
	integ := NewIntegrator(s, ModeFull)
	for _, pt := range s.Truth.Pixels {
		cam, _ := s.Camera(pt.CameraID)
		ray := cam.RayForPixel(pt.PixelX, pt.PixelY, s.Width, s.Height, pt.BlockID)
		tr := TraceRay(ray, s.Media, s.FixedScale)
		got, err := integ.Integrate(tr)
		if err != nil {
			t.Fatalf("integrate truth pixel %d,%d: %v", pt.PixelX, pt.PixelY, err)
		}
		t.Logf("px(%d,%d) want T %.12f L %.12f got T %.12f L %.12f dT %.2e dL %.2e",
			pt.PixelX, pt.PixelY, pt.ExpectedTransmittance, pt.ExpectedRadiance,
			got.Transmittance, got.Radiance,
			absFloat(got.Transmittance-pt.ExpectedTransmittance),
			absFloat(got.Radiance-pt.ExpectedRadiance))
		if absFloat(got.Transmittance-pt.ExpectedTransmittance) > pt.ToleranceT {
			t.Fatalf("T mismatch at %d,%d", pt.PixelX, pt.PixelY)
		}
		if absFloat(got.Radiance-pt.ExpectedRadiance) > pt.ToleranceL {
			t.Fatalf("L mismatch at %d,%d", pt.PixelX, pt.PixelY)
		}
	}
}
