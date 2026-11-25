package qos

import (
	"context"
	"reflect"
	"testing"
)

func TestSmokeRoundTrip(t *testing.T) {
	p := Scenario(42)
	p.SchemaVersion = 1
	if err := ValidateProject(p); err != nil {
		t.Fatal(err)
	}
	var canonical *Project
	for v := 1; v <= 3; v++ {
		raw, err := EncodeProject(p, v)
		if err != nil {
			t.Fatalf("encode v%d: %v", v, err)
		}
		loaded, err := LoadProject(raw)
		if err != nil {
			t.Fatalf("load v%d: %v\n%s", v, err, raw)
		}
		if canonical == nil {
			canonical = loaded
		} else {
			d1, _ := canonical.CanonicalDigest()
			d2, _ := loaded.CanonicalDigest()
			if d1 != d2 {
				t.Fatalf("digest mismatch v%d: %s != %s", v, d1, d2)
			}
			if len(canonical.Channels) != len(loaded.Channels) {
				t.Fatalf("channel count mismatch v%d", v)
			}
			for i := range canonical.Channels {
				a, b := canonical.Channels[i], loaded.Channels[i]
				if a.ID != b.ID || a.Order != b.Order || a.RatePermyriad != b.RatePermyriad ||
					a.CapacityBytes != b.CapacityBytes || a.InitialTokens != b.InitialTokens ||
					a.WeightPermyriad != b.WeightPermyriad || a.QueueAgeMS != b.QueueAgeMS {
					t.Fatalf("normalized channel mismatch v%d at %d: %#v != %#v", v, i, a, b)
				}
			}
		}
	}
	eng, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("steps=%d emitted=%d delayed=%d dropped=%d expired=%d final=%v",
		res.TotalSteps, len(res.Emissions), len(res.Delays), len(res.Drops), len(res.Expired), res.FinalTokens)
	if len(res.Delays) == 0 {
		t.Fatal("sample should contain delay events")
	}
	if len(res.Drops) == 0 {
		t.Fatal("sample should contain drops")
	}
	rawTruth, err := EncodeTruth(p, res)
	if err != nil {
		t.Fatal(err)
	}
	truth, err := DecodeTruth(rawTruth, p)
	if err != nil {
		t.Fatalf("decode truth: %v\n%s", err, rawTruth)
	}
	if len(truth.Steps) != len(res.Steps) {
		t.Fatalf("truth steps=%d result steps=%d", len(truth.Steps), len(res.Steps))
	}
	for i := range res.Steps {
		want := res.Steps[i]
		want.SourceID, want.Reused, want.CacheToken = "", false, ""
		if !reflect.DeepEqual(want, truth.Steps[i]) {
			t.Fatalf("truth mismatch at step %d\nwant=%#v\ngot=%#v", i, want, truth.Steps[i])
		}
	}
}
