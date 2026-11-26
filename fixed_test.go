package bonerig

import "testing"

func TestQuaternionHighLowHighResidual(t *testing.T) {
	sample, _ := NewDeterministicSample(DefaultSeed)
	p := sample.Asset.Precision
	original := sample.Asset.Clip.Frames[3].Poses["spine"].Rotation
	wire := quatToScale(original, V1QuatScale)
	low := Quat{W: wire[0], X: wire[1], Y: wire[2], Z: wire[3], Scale: V1QuatScale}
	_, work, trace, err := NormalizeQuat(low, p, "spine", 3)
	if err != nil {
		t.Fatal(err)
	}
	origFQ := FQ{W: original.W, X: original.X, Y: original.Y, Z: original.Z}
	dev := quatL1(origFQ, work)
	ratio := p.WorkScale / V1QuatScale
	declaredL1 := 2 * p.Bound.Components * ratio
	if dev > declaredL1 {
		t.Fatalf("round-trip L1 residual %d > declared %d", dev, declaredL1)
	}
	norm, ok := quatNorm(work)
	if !ok {
		t.Fatal("norm overflow")
	}
	if normDiff := abs64(norm - p.WorkScale); normDiff > ratio {
		t.Fatalf("norm residual %d > one format unit %d", normDiff, ratio)
	}
	if len(trace.Components) != 4 {
		t.Fatalf("component truncation traces = %d", len(trace.Components))
	}
}

func TestQuaternionErrorCodesAndOperands(t *testing.T) {
	p := DefaultPrecision()
	cases := []struct {
		name string
		q    Quat
	}{
		{"illegal_scale", Quat{W: 1, Scale: 0}},
		{"bad_modulus", Quat{W: 1, X: 0, Y: 0, Z: 0, Scale: V1QuatScale}},
		{"component_overflow", Quat{W: p.MaxComponent + 1, Scale: p.WorkScale}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := NormalizeQuat(tc.q, p, "hand_l", 7)
			be := asBonerigError(t, err)
			if be.Code != CodeQuat {
				t.Fatalf("code = %s", be.Code)
			}
			if be.Joint != "hand_l" || be.Frame == nil || *be.Frame != 7 {
				t.Fatalf("location missing: %+v", be)
			}
			if be.OpA == nil || be.OpB == nil {
				t.Fatalf("two operands required: %+v", be)
			}
		})
	}
}
