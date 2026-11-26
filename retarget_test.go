package bonerig

import "testing"

func TestMapMissingAndBrokenParentChain(t *testing.T) {
	sample, _ := NewDeterministicSample(DefaultSeed)

	t.Run("missing_mapping", func(t *testing.T) {
		a := sample.Asset
		filtered := a.Mapping.Items[:0]
		for _, m := range a.Mapping.Items {
			if m.TargetJoint != "HandL" {
				filtered = append(filtered, m)
			}
		}
		a.Mapping.Items = filtered
		_, err := RetargetAll(a)
		be := asBonerigError(t, err)
		if be.Code != CodeMap || be.Joint != "HandL" {
			t.Fatalf("unexpected error %+v", be)
		}
		if be.Breakpoint == "" {
			t.Fatal("breakpoint required")
		}
	})

	t.Run("broken_parent", func(t *testing.T) {
		sample2, _ := NewDeterministicSample(DefaultSeed)
		a := sample2.Asset
		idx := a.Skeleton.Index()
		a.Skeleton.Joints[idx["spine"]].ParentID = "ghost"
		_, err := RetargetAll(a)
		be := asBonerigError(t, err)
		if be.Code != CodeMap || be.Joint != "spine" || be.Breakpoint != "missing_parent:ghost" {
			t.Fatalf("unexpected error %+v", be)
		}
	})
}

func TestUpperBoundViolationReturnsDetails(t *testing.T) {
	sample, _ := NewDeterministicSample(DefaultSeed)
	a := sample.Asset
	p := a.Precision
	a.Truth.Frames[0].TargetRotations["Root"] = Quat{W: 0, X: p.WorkScale, Y: 0, Z: 0, Scale: p.WorkScale}
	_, err := RetargetAll(a)
	be := asBonerigError(t, err)
	if be.Code != CodeUB || be.Joint != "Root" || be.Frame == nil || *be.Frame != 0 {
		t.Fatalf("unexpected UB error: %+v", be)
	}
	parts, ok := be.Expected.(map[string]int64)
	if !ok {
		t.Fatalf("four components expected, got %T", be.Expected)
	}
	for _, key := range []string{"norm_upper", "chain_upper", "scale_upper", "map_upper"} {
		if _, ok := parts[key]; !ok {
			t.Fatalf("missing %s in %+v", key, parts)
		}
	}
	if be.OpA == nil || be.OpB == nil {
		t.Fatal("measured deviation and total bound operands required")
	}
}
