package ortho

import (
	"math"
	"testing"
)

func TestAffineRoundTrip(t *testing.T) {
	s := NewAffine(AffineParams{Sx: 1.25, Sy: 0.8, Tx: 16, Ty: -4})
	for _, p := range [][2]float64{{0, 0}, {10.5, 20.25}, {319, 239}} {
		fx, fy := s.Forward(p[0], p[1])
		bx, by := s.Inverse(fx, fy)
		if math.Abs(bx-p[0]) > 1e-9 || math.Abs(by-p[1]) > 1e-9 {
			t.Fatalf("affine round trip failed: got (%v,%v)", bx, by)
		}
	}
	w, h := s.OutSize(320, 240)
	if w != 400 || h != 192 {
		t.Fatalf("affine OutSize = %dx%d, want 400x192", w, h)
	}
}

func TestHomographyRoundTrip(t *testing.T) {
	s := NewHomography(DefaultStageParams().Homography)
	for _, p := range [][2]float64{{5, 7}, {100.5, 60.25}, {319, 239}} {
		fx, fy := s.Forward(p[0], p[1])
		bx, by := s.Inverse(fx, fy)
		if math.Abs(bx-p[0]) > 1e-6 || math.Abs(by-p[1]) > 1e-6 {
			t.Fatalf("homography round trip failed: got (%v,%v)", bx, by)
		}
	}
}

func TestRadialInverseOfForward(t *testing.T) {
	s := NewRadial(DefaultStageParams().Radial)
	for _, p := range [][2]float64{{160, 120}, {40, 40}, {300, 220}, {160.5, 30.25}} {
		fx, fy := s.Forward(p[0], p[1])
		bx, by := s.Inverse(fx, fy)
		if math.Abs(bx-p[0]) > 1e-6 || math.Abs(by-p[1]) > 1e-6 {
			t.Fatalf("radial inverse(forward) failed at %v: got (%v,%v)", p, bx, by)
		}
	}
}

func TestParamValidation(t *testing.T) {
	cases := []struct {
		name  string
		stage Stage
		field string
	}{
		{"homography singular", NewHomography(HomographyParams{H: [9]float64{1, 2, 3, 4, 5, 6, 7, 8, 9}}), "H"},
		{"homography nan", NewHomography(HomographyParams{H: [9]float64{math.NaN(), 0, 0, 0, 1, 0, 0, 0, 1}}), "H[0]"},
		{"radial k1", NewRadial(RadialParams{K1: 2, Cx: 1, Cy: 1, Radius: 10}), "K1"},
		{"radial radius", NewRadial(RadialParams{K1: 0.1, Cx: 1, Cy: 1, Radius: 0}), "Radius"},
		{"affine sx", NewAffine(AffineParams{Sx: 0.001, Sy: 1}), "Sx"},
		{"affine ty", NewAffine(AffineParams{Sx: 1, Sy: 1, Ty: 1e9}), "Ty"},
	}
	for _, tc := range cases {
		err := tc.stage.Validate()
		if err == nil {
			t.Fatalf("%s: expected E_PARA", tc.name)
		}
		if err.Code != EParam {
			t.Fatalf("%s: code = %s, want E_PARA", tc.name, err.Code)
		}
		if err.Stage != tc.stage.ID() {
			t.Fatalf("%s: stage id = %s, want %s", tc.name, err.Stage, tc.stage.ID())
		}
		if err.Field != tc.field {
			t.Fatalf("%s: field = %s, want %s", tc.name, err.Field, tc.field)
		}
	}
}

func TestDecodeError(t *testing.T) {
	_, err := DecodeImage(bytesReader("garbage bytes"))
	if err == nil || err.Code != EDecode {
		t.Fatalf("expected E_DECODE, got %v", err)
	}
}
