package bonerig

import (
	"encoding/json"
	"errors"
	"testing"
)

func asBonerigError(t *testing.T, err error) *Error {
	t.Helper()
	var be *Error
	if !errors.As(err, &be) {
		t.Fatalf("expected *bonerig.Error, got %T: %v", err, err)
	}
	return be
}

func TestThreeFormatsLoadRetargetAndTruth(t *testing.T) {
	sample, err := NewDeterministicSample(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{V1Schema, V2Schema, V3Schema} {
		t.Run(schema, func(t *testing.T) {
			loaded, lerr := LoadDocument(sample.Docs[schema])
			if lerr != nil {
				t.Fatal(lerr)
			}
			if loaded.Asset.SourceFormat != schema {
				t.Fatalf("source format = %q", loaded.Asset.SourceFormat)
			}
			if len(loaded.Asset.Traces) == 0 || len(loaded.NormTraces) == 0 {
				t.Fatalf("traces missing: transforms=%d norm=%d", len(loaded.Asset.Traces), len(loaded.NormTraces))
			}
			loaded.Asset.Truth = sample.Truth
			frames, rerr := RetargetAll(loaded.Asset)
			if rerr != nil {
				t.Fatal(rerr)
			}
			for _, frame := range frames {
				for _, rec := range frame.Precision {
					if !rec.WithinBound {
						t.Fatalf("UB exceeded: %+v", rec)
					}
					if rec.MeasuredDeviation > sample.Truth.Tolerance {
						t.Fatalf("truth tolerance: joint=%s frame=%d dev=%d tol=%d", rec.Joint, rec.FrameIndex, rec.MeasuredDeviation, sample.Truth.Tolerance)
					}
				}
			}
		})
	}
}

func TestSchemaUnknownAndMissingFieldPath(t *testing.T) {
	_, err := LoadDocument([]byte(`{"schema":"rig.nope"}`))
	be := asBonerigError(t, err)
	if be.Code != CodeSchema || be.Field != "$.schema" {
		t.Fatalf("unexpected unknown schema error: %+v", be)
	}

	sample, _ := NewDeterministicSample(DefaultSeed)
	var doc map[string]any
	if err := json.Unmarshal(sample.Docs[V1Schema], &doc); err != nil {
		t.Fatal(err)
	}
	bones := doc["bones"].([]any)
	bone := bones[0].(map[string]any)
	delete(bone, "name")
	b, _ := json.Marshal(doc)
	_, lerr := LoadDocument(b)
	be = asBonerigError(t, lerr)
	if be.Code != CodeSchema || be.Field != "$.bones[0].name" {
		t.Fatalf("missing field path = %q code=%s", be.Field, be.Code)
	}
}

func TestLossyListUniqueAndSameVersionRoundTrip(t *testing.T) {
	sample, _ := NewDeterministicSample(DefaultSeed)
	for _, schema := range []string{V1Schema, V2Schema, V3Schema} {
		t.Run(schema, func(t *testing.T) {
			data, lossy, eerr := ExportFormat(sample.Asset, schema)
			if eerr != nil {
				t.Fatal(eerr)
			}
			seen := map[string]bool{}
			for _, item := range lossy {
				key := item.FieldPath + "|" + item.Kind
				if seen[key] {
					t.Fatalf("duplicate lossy item %s", key)
				}
				seen[key] = true
				if item.Code != CodeLossy || item.Disposition == "" {
					t.Fatalf("incomplete lossy item: %+v", item)
				}
			}
			if schema == V1Schema {
				if len(lossy) < len(sample.Asset.Mapping.Items) {
					t.Fatalf("expected every V1 retarget scale loss, got %d", len(lossy))
				}
				foundScale := false
				for _, item := range lossy {
					if item.Kind == "retarget_scale" {
						foundScale = true
					}
				}
				if !foundScale {
					t.Fatal("missing retarget_scale loss entries")
				}
			}

			first, lerr := LoadDocument(data)
			if lerr != nil {
				t.Fatal(lerr)
			}
			data2, _, e2 := ExportFormat(first.Asset, schema)
			if e2 != nil {
				t.Fatal(e2)
			}
			second, l2 := LoadDocument(data2)
			if l2 != nil {
				t.Fatal(l2)
			}
			assertModelRoundTrip(t, first.Asset, second.Asset)
		})
	}
}

func assertModelRoundTrip(t *testing.T, a, b Asset) {
	t.Helper()
	if len(a.Skeleton.Joints) != len(b.Skeleton.Joints) {
		t.Fatalf("joint count %d != %d", len(a.Skeleton.Joints), len(b.Skeleton.Joints))
	}
	for i := range a.Skeleton.Joints {
		aj, bj := a.Skeleton.Joints[i], b.Skeleton.Joints[i]
		if aj.ID != bj.ID || aj.ParentID != bj.ParentID {
			t.Fatalf("hierarchy mismatch at %d: %+v vs %+v", i, aj, bj)
		}
		if (aj.Constraint == nil) != (bj.Constraint == nil) {
			t.Fatalf("constraint presence mismatch %s", aj.ID)
		}
		if aj.Constraint != nil {
			ac, bc := aj.Constraint, bj.Constraint
			if ac.Enabled != bc.Enabled || ac.AxisX != bc.AxisX || ac.AxisY != bc.AxisY || ac.AxisZ != bc.AxisZ || ac.MinAngleMicroRad != bc.MinAngleMicroRad || ac.MaxAngleMicroRad != bc.MaxAngleMicroRad {
				t.Fatalf("constraint mismatch %s: %+v vs %+v", aj.ID, ac, bc)
			}
		}
	}
	if len(a.Clip.Frames) != len(b.Clip.Frames) {
		t.Fatalf("frame count mismatch")
	}
	for fi := range a.Clip.Frames {
		af, bf := a.Clip.Frames[fi], b.Clip.Frames[fi]
		for joint, ap := range af.Poses {
			bp, ok := bf.Poses[joint]
			if !ok {
				t.Fatalf("frame %d missing joint %s", fi, joint)
			}
			if ap.Rotation != bp.Rotation {
				t.Fatalf("frame %d joint %s rotation %+v != %+v", fi, joint, ap.Rotation, bp.Rotation)
			}
		}
	}
}
