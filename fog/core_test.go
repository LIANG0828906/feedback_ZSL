package fog

import (
	"bytes"
	"context"
	"math"
	"testing"
)

func smallScene(t *testing.T) *Scene {
	t.Helper()
	opts := DefaultGenerateOptions(4242)
	opts.Width, opts.Height = 8, 6
	opts.TileWidth, opts.TileHeight = 4, 3
	opts.Steps, opts.PreviewSteps = 24, 8
	opts.ShadowSamples, opts.PreviewShadow = 2, 1
	opts.ShadowBudget = 32
	opts.MaxParallel = 2
	opts.RayBuffer = 2
	s := Generate(opts)
	if f := s.Validate(); f != nil {
		t.Fatalf("generated scene invalid: %v", f)
	}
	return s
}

func TestGenerateDeterministicAndRoundTrip(t *testing.T) {
	a := Generate(DefaultGenerateOptions(42))
	b := Generate(DefaultGenerateOptions(42))
	if a.Hash() != b.Hash() {
		t.Fatalf("same seed produced different scenes: %s != %s", a.Hash(), b.Hash())
	}
	var buf bytes.Buffer
	if err := EncodeScene(&buf, a); err != nil {
		t.Fatal(err)
	}
	decoded, f := DecodeScene(&buf)
	if f != nil {
		t.Fatalf("round trip failed: %v", f)
	}
	if decoded.Hash() != a.Hash() {
		t.Fatalf("round trip changed content hash: %s != %s", decoded.Hash(), a.Hash())
	}
	if len(a.Truth) != 4 {
		t.Fatalf("expected 4 analytic truth rays, got %d", len(a.Truth))
	}
}

func TestGenerateFillsOmittedOptions(t *testing.T) {
	s := Generate(GenerateOptions{Seed: 7, Width: 8, Height: 8})
	if f := s.Validate(); f != nil {
		t.Fatalf("partial options did not produce a valid scene: %v", f)
	}
	if s.Recipe.TileWidth <= 0 || s.Recipe.Steps <= 0 || s.Recipe.ShadowBudget <= 0 {
		t.Fatalf("omitted recipe fields were not defaulted: %+v", s.Recipe)
	}
}

func TestValidationCodesAndPaths(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Scene)
		code ErrorCode
		path string
	}{
		{
			name: "bounds",
			mut: func(s *Scene) {
				s.Media[0].Bounds.Min.X = 10
				s.Media[0].Bounds.Max.X = -10
			},
			code: EPara, path: "media[0].bounds",
		},
		{
			name: "resolution",
			mut: func(s *Scene) {
				s.Media[0].DensityRes[0] = 65
			},
			code: EDens, path: "media[0].density_res[0]",
		},
		{
			name: "negative coefficient",
			mut: func(s *Scene) {
				s.Media[0].AbsorptionFP = -1
			},
			code: EPara, path: "media[0].absorption_fp",
		},
		{
			name: "camera order",
			mut: func(s *Scene) {
				s.Cameras[2].LogicalTime = s.Cameras[0].LogicalTime
			},
			code: EOrder, path: "cameras[2].logical_time",
		},
		{
			name: "corrupt density",
			mut: func(s *Scene) {
				s.Media[0].Density[0] = DensityMax + 1
			},
			code: EDecode, path: "media[0].density",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := smallScene(t).Clone()
			tc.mut(s)
			f := s.Validate()
			if f == nil {
				t.Fatal("expected validation fault")
			}
			if f.Code != tc.code {
				t.Fatalf("code=%s want=%s: %v", f.Code, tc.code, f)
			}
			if f.Path != tc.path {
				t.Fatalf("path=%q want=%q: %v", f.Path, tc.path, f)
			}
			if f.Actual == nil {
				t.Fatalf("fault should include measured value: %v", f)
			}
		})
	}
}

func TestIntersectionStableAndOverlapSegmented(t *testing.T) {
	s := smallScene(t).Clone()
	s.Media = []Medium{
		uniformMedium("a", AABB{Min: V(-1, -1, 0), Max: V(1, 1, 1)}, [3]int{2, 2, 2}, 500, 100_000, 50_000, 0, 0),
		uniformMedium("b", AABB{Min: V(-1, -1, 0.5), Max: V(1, 1, 1.5)}, [3]int{2, 2, 2}, 500, 100_000, 50_000, 0, 1),
	}
	ray := Ray{ID: "overlap-ray", Origin: V(0, 0, -1), Direction: V(0, 0, 1)}
	in1 := IntersectRay(s, ray)
	in2 := IntersectRay(s, ray)
	if in1.Digest != in2.Digest {
		t.Fatalf("repeat intersection digest changed")
	}
	if len(in1.Segments) != 3 {
		t.Fatalf("expected 3 elementary segments, got %d: %+v", len(in1.Segments), in1.Segments)
	}
	wantActive := [][]string{{"a"}, {"a", "b"}, {"b"}}
	for i, seg := range in1.Segments {
		if !stringSlicesEqual(seg.ActiveMedia, wantActive[i]) {
			t.Fatalf("segment %d active=%v want=%v", i, seg.ActiveMedia, wantActive[i])
		}
		wantT0 := int64(1_000_000 + i*500_000)
		if seg.T0 != wantT0 {
			t.Fatalf("segment %d t0=%d want=%d", i, seg.T0, wantT0)
		}
		if math.Abs(seg.ResidualT0) > 1e-9 || math.Abs(seg.ResidualT1) > 1e-9 {
			t.Fatalf("unexpected fixed-point residual: %+v", seg)
		}
	}
	if !in1.Segments[1].ContainsOverlap || in1.Segments[0].ContainsOverlap || in1.Segments[2].ContainsOverlap {
		t.Fatalf("overlap flag not localized: %+v", in1.Segments)
	}
}

func TestTruthMatchesAnalytic(t *testing.T) {
	s := Generate(DefaultGenerateOptions(99))
	_, cal := s.MediaByID("calibration")
	if cal == nil {
		t.Fatal("calibration medium missing")
	}
	for _, truth := range s.Truth {
		ray := Ray{ID: truth.ID, Origin: truth.Origin, Direction: truth.Direction}
		tr, sc := AnalyticHomogeneous(s, *cal, ray, truth.T0, truth.T1)
		if math.Abs(tr-truth.Transmittance) > 1e-12 || math.Abs(sc-truth.Scattered) > 1e-12 {
			t.Fatalf("%s truth mismatch: got (%g,%g) want (%g,%g)", truth.ID, tr, sc, truth.Transmittance, truth.Scattered)
		}
	}
}

func TestPhaseFunctions(t *testing.T) {
	iso := 1 / (4 * math.Pi)
	if got := EvalPhase(PhaseIsotropic, 0.3, 0.7); math.Abs(got-iso) > 1e-15 {
		t.Fatalf("isotropic phase=%g want=%g", got, iso)
	}
	if got := EvalPhase(PhaseHG, 0.25, 0); math.Abs(got-iso) > 1e-15 {
		t.Fatalf("HG g=0 should be isotropic: %g", got)
	}
	if got := EvalPhase(PhaseHG, -0.5, 0.6); !isFinite(got) || got <= 0 {
		t.Fatalf("HG returned invalid value: %g", got)
	}
}

func TestIntegrationConvergesAndIsStable(t *testing.T) {
	s := smallScene(t).Clone()
	_, cal := s.MediaByID("calibration")
	if cal == nil {
		t.Fatal("calibration missing")
	}
	s.Media = []Medium{*cal}
	ray := Ray{ID: "convergence", Origin: V(0, 0, 1), Direction: V(0, 0, 1)}
	in := IntersectRay(s, ray)
	if len(in.Segments) != 1 {
		t.Fatalf("expected one segment, got %d", len(in.Segments))
	}
	wantT, wantS := AnalyticHomogeneous(s, *cal, ray, 1, 3)
	prevErr := math.Inf(1)
	for _, steps := range []int{4, 8, 16, 32, 64, 128} {
		rr, f := IntegrateRay(context.Background(), s, ray, in, IntegrateOptions{Steps: steps, ShadowSamples: 0})
		if f != nil {
			t.Fatal(f)
		}
		if math.Abs(rr.Transmittance-wantT) > 1e-14 {
			t.Fatalf("steps=%d transmittance=%g want=%g", steps, rr.Transmittance, wantT)
		}
		err := math.Abs(rr.Radiance - wantS)
		if err > prevErr+1e-15 {
			t.Fatalf("steps=%d error increased: %g -> %g", steps, prevErr, err)
		}
		prevErr = err
		if steps == 128 && err > 5e-4 {
			t.Fatalf("128-step error too large: %g", err)
		}
		rr2, _ := IntegrateRay(context.Background(), s, ray, in, IntegrateOptions{Steps: steps, ShadowSamples: 0})
		if rr.Digest != rr2.Digest || len(rr.Segments) != len(rr2.Segments) {
			t.Fatalf("same integration parameters produced different results")
		}
	}
}

func TestTruthRaysIntegrateWithinTolerance(t *testing.T) {
	s := smallScene(t)
	for _, truth := range s.Truth {
		ray := Ray{ID: truth.ID, Origin: truth.Origin, Direction: truth.Direction}
		in := IntersectRay(s, ray)
		if len(in.Segments) != 1 {
			t.Fatalf("%s unexpectedly intersects %d media segments", truth.ID, len(in.Segments))
		}
		rr, f := IntegrateRay(context.Background(), s, ray, in, IntegrateOptions{Steps: 128, ShadowSamples: 0})
		if f != nil {
			t.Fatal(f)
		}
		if math.Abs(rr.Transmittance-truth.Transmittance) > 1e-12 {
			t.Fatalf("%s transmittance=%g truth=%g", truth.ID, rr.Transmittance, truth.Transmittance)
		}
		if math.Abs(rr.Radiance-truth.Scattered) > 2e-4 {
			t.Fatalf("%s scattered=%g truth=%g diff=%g", truth.ID, rr.Radiance, truth.Scattered, math.Abs(rr.Radiance-truth.Scattered))
		}
		t0 := FixedToFloat(in.Segments[0].T0, s.Recipe.FixedScale)
		t1 := FixedToFloat(in.Segments[0].T1, s.Recipe.FixedScale)
		if math.Abs(t0-truth.T0) > 1e-6 || math.Abs(t1-truth.T1) > 1e-6 {
			t.Fatalf("%s interval=(%g,%g) truth=(%g,%g)", truth.ID, t0, t1, truth.T0, truth.T1)
		}
	}
}
