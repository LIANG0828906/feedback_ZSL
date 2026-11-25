package spectral_test

import (
	"math"
	"testing"

	"spectralforge/internal/domain"
	"spectralforge/internal/quant"
	"spectralforge/internal/sample"
	"spectralforge/internal/spectral"
)

func bundle(t *testing.T) *domain.SampleBundle {
	t.Helper()
	dir := t.TempDir()
	b, err := sample.Generate(dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBothQuadratureMethodsCoverTheSameInterval(t *testing.T) {
	b := bundle(t)
	for _, method := range []string{"trapezoid", "midpoint"} {
		points, err := spectral.IntegrationSamples(b.Spectral.Wavelengths, 31, method)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if len(points) != 31 {
			t.Fatalf("%s: got %d points", method, len(points))
		}
		sum := 0.0
		for i, p := range points {
			sum += p.Weight
			if i > 0 && p.Wavelength <= points[i-1].Wavelength {
				t.Fatalf("%s: wavelengths are not strictly increasing", method)
			}
		}
		want := b.Spectral.Wavelengths[len(b.Spectral.Wavelengths)-1] - b.Spectral.Wavelengths[0]
		if math.Abs(sum-want) > 1e-9 {
			t.Fatalf("%s: weight sum %.12g, want %.12g", method, sum, want)
		}
	}
}

func TestKernelMatchesDirectPerSampleProductIntegration(t *testing.T) {
	b := bundle(t)
	if err := domain.ValidateConfig(b.Config); err != nil {
		t.Fatal(err)
	}
	cache := quant.NewCache()
	materials, err := quant.BuildSet(b.Scene, b.Config.Quantization, cache)
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := spectral.BuildKernel(b.Spectral, *b.Config)
	if err != nil {
		t.Fatal(err)
	}
	obj := b.Scene.Objects[0]
	mat := materials.Objects[obj.MeshID]
	reflectance := make([]float64, len(b.Spectral.Wavelengths))
	for i := range reflectance {
		for basis := range mat.Coefficients {
			reflectance[i] += mat.Coefficients[basis] * b.Spectral.Basis[basis].Values[i]
		}
	}
	for li := range b.Spectral.Lights {
		direct, err := spectral.IntegrateProducts(b.Spectral, li, kernel.Points, reflectance)
		if err != nil {
			t.Fatal(err)
		}
		var fast spectral.XYZ
		for basis := range mat.Coefficients {
			fast.X += kernel.Lights[li].Basis[basis].X * mat.Coefficients[basis]
			fast.Y += kernel.Lights[li].Basis[basis].Y * mat.Coefficients[basis]
			fast.Z += kernel.Lights[li].Basis[basis].Z * mat.Coefficients[basis]
		}
		const eps = 1e-12
		if math.Abs(direct.X-fast.X) > eps || math.Abs(direct.Y-fast.Y) > eps || math.Abs(direct.Z-fast.Z) > eps {
			t.Fatalf("light %d direct=%+v kernel=%+v", li, direct, fast)
		}
	}
}

func TestSameParametersProduceIdenticalKernel(t *testing.T) {
	b := bundle(t)
	k1, err := spectral.BuildKernel(b.Spectral, *b.Config)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := spectral.BuildKernel(b.Spectral, *b.Config)
	if err != nil {
		t.Fatal(err)
	}
	for i := range k1.Lights {
		for j := range k1.Lights[i].Basis {
			if k1.Lights[i].Basis[j] != k2.Lights[i].Basis[j] {
				t.Fatalf("kernel differs at light %d basis %d", i, j)
			}
		}
	}
}

func TestTrapezoidAndMidpointAgreeForSmoothConstantSpectrum(t *testing.T) {
	b := bundle(t)
	constant := make([]float64, len(b.Spectral.Wavelengths))
	for i := range constant {
		constant[i] = 0.5
	}
	trapCfg := *b.Config
	trapCfg.Integration = "trapezoid"
	trapCfg.WavelengthSteps = 101
	midCfg := trapCfg
	midCfg.Integration = "midpoint"
	trapPoints, _ := spectral.IntegrationSamples(b.Spectral.Wavelengths, trapCfg.WavelengthSteps, trapCfg.Integration)
	midPoints, _ := spectral.IntegrationSamples(b.Spectral.Wavelengths, midCfg.WavelengthSteps, midCfg.Integration)
	trap, err := spectral.IntegrateProducts(b.Spectral, 0, trapPoints, constant)
	if err != nil {
		t.Fatal(err)
	}
	mid, err := spectral.IntegrateProducts(b.Spectral, 0, midPoints, constant)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(trap.Y-mid.Y) > 0.05 {
		t.Fatalf("integration methods diverge too much: trap=%g mid=%g", trap.Y, mid.Y)
	}
}
