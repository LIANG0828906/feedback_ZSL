package fogkernel

import "testing"

func TestIncrementalEditsMatchFull(t *testing.T) {
	bundle, err := GenerateDefaultBundle()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("voxel", func(t *testing.T) {
		session := NewIncrementalSession(bundle.Canonical)
		req := EditRequest{Kind: "voxel", MediumID: "fog-alpha", X: 2, Y: 1, Z: 1, Value: 210}
		res, err := session.ApplyEdit(req)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Invalidation.DirectKeys) == 0 {
			t.Fatalf("expected direct invalidation for central voxel")
		}
		if res.FullCheck == nil || !res.FullCheck.OK {
			t.Fatalf("full check failed: %+v", res.FullCheck)
		}
		if len(res.Diff.ChangedRays) == 0 {
			t.Fatalf("expected at least one changed ray")
		}
	})

	t.Run("coefficients", func(t *testing.T) {
		session := NewIncrementalSession(bundle.Canonical)
		a := int64(210)
		s := int64(820)
		res, err := session.ApplyEdit(EditRequest{Kind: "coefficients", MediumID: "fog-beta", SigmaAPermille: &a, SigmaSPermille: &s})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Invalidation.DirectKeys) == 0 {
			t.Fatal("expected coefficient edit to invalidate volume blocks")
		}
		if !res.FullCheck.OK {
			t.Fatalf("full check failed: %+v", res.FullCheck.Mismatches)
		}
	})

	t.Run("camera", func(t *testing.T) {
		session := NewIncrementalSession(bundle.Canonical)
		yaw := int64(12000)
		res, err := session.ApplyEdit(EditRequest{Kind: "camera", CameraIndex: 1, YawMilliDeg: &yaw})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Invalidation.RecomputedKeys) == 0 {
			t.Fatal("expected camera rays to be recomputed")
		}
		if !res.FullCheck.OK {
			t.Fatalf("full check failed: %+v", res.FullCheck.Mismatches)
		}
	})
}
