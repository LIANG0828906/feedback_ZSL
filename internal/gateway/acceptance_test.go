package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
)

func testBundle(t *testing.T) *SampleBundle {
	t.Helper()
	bundle, err := GenerateSample(DefaultSeed)
	if err != nil {
		t.Fatalf("GenerateSample returned error: %v", err)
	}
	return bundle
}

func mustRun(t *testing.T, p *Project) *RunReport {
	t.Helper()
	engine, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	report, err := engine.Run()
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestDeterministicSampleIsStable(t *testing.T) {
	first := testBundle(t)
	second, err := GenerateSample(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	if CanonicalJSON(first.Canonical) != CanonicalJSON(second.Canonical) {
		t.Fatal("same seed produced different canonical projects")
	}
	for _, version := range []string{"v1", "v2", "v3"} {
		if string(first.Schemas[version]) != string(second.Schemas[version]) {
			t.Fatalf("schema %s is not deterministic", version)
		}
	}
}

func TestThreeSchemaVersionsLoadToSameIdentity(t *testing.T) {
	bundle := testBundle(t)
	loaded := map[SchemaName]*Project{}
	for _, version := range []SchemaName{SchemaV1, SchemaV2, SchemaV3} {
		p, err := LoadSchema(bundle.Schemas[string(version)])
		if err != nil {
			t.Fatalf("LoadSchema(%s) error: %v", version, err)
		}
		loaded[version] = p
	}

	base := loaded[SchemaV1]
	for _, version := range []SchemaName{SchemaV2, SchemaV3} {
		if len(loaded[version].Routes) != len(base.Routes) {
			t.Fatalf("%s route count = %d, want %d", version, len(loaded[version].Routes), len(base.Routes))
		}
		for i := range base.Routes {
			if loaded[version].Routes[i].ID != base.Routes[i].ID || loaded[version].Routes[i].ServiceID != base.Routes[i].ServiceID {
				t.Fatalf("%s route order mismatch at %d: %+v vs %+v", version, i, loaded[version].Routes[i], base.Routes[i])
			}
		}
		if len(loaded[version].Records) != len(base.Records) {
			t.Fatalf("%s record count = %d, want %d", version, len(loaded[version].Records), len(base.Records))
		}
		for i := range base.Records {
			a := base.Records[i]
			b := loaded[version].Records[i]
			if a.ID != b.ID || a.ArrivalSeq != b.ArrivalSeq || a.EventTimeMS != b.EventTimeMS {
				t.Fatalf("%s record identity/time mismatch at %d: %+v vs %+v", version, i, b, a)
			}
			if a.DurationUS != b.DurationUS || a.DurationFixed != b.DurationFixed {
				t.Fatalf("%s duration conversion mismatch record %s: %dus/%d vs %dus/%d",
					version, a.ID, b.DurationUS, b.DurationFixed, a.DurationUS, a.DurationFixed)
			}
			keysA, err := groupKeysForRecord(base, a)
			if err != nil {
				t.Fatal(err)
			}
			keysB, err := groupKeysForRecord(loaded[version], b)
			if err != nil {
				t.Fatal(err)
			}
			if CanonicalJSON(keysA) != CanonicalJSON(keysB) {
				t.Fatalf("%s group keys mismatch record %s: %s vs %s", version, a.ID, CanonicalJSON(keysB), CanonicalJSON(keysA))
			}
		}
	}

	if loaded[SchemaV2].WatermarkMS != nil {
		t.Fatalf("v2 should omit explicit watermark, got %d", *loaded[SchemaV2].WatermarkMS)
	}
	if loaded[SchemaV2].CommittedBoundaryMS == nil {
		t.Fatal("v2 should retain declared committed boundary fallback")
	}
}

func TestSchemaAmbiguousDurationReturnsStableCode(t *testing.T) {
	bundle := testBundle(t)
	var doc map[string]any
	if err := json.Unmarshal(bundle.Schemas["v1"], &doc); err != nil {
		t.Fatal(err)
	}
	records := doc["access_records"].([]any)
	first := records[0].(map[string]any)
	first["latency_ms"] = 999999
	data, _ := json.Marshal(doc)
	_, err := LoadSchema(data)
	assertErrorCode(t, err, CodeFieldAmbiguous)
}

func TestDualRouteRepresentationReturnsStableCode(t *testing.T) {
	bundle := testBundle(t)
	var doc map[string]any
	if err := json.Unmarshal(bundle.Schemas["v2"], &doc); err != nil {
		t.Fatal(err)
	}
	doc["route_list"] = []any{map[string]any{"route_id": "duplicate"}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadSchema(data)
	assertErrorCode(t, err, CodeFieldAmbiguous)
}

func TestWindowAggregatesMatchGeneratorTruth(t *testing.T) {
	bundle := testBundle(t)
	for _, version := range []SchemaName{SchemaV1, SchemaV3} {
		p, err := LoadSchema(bundle.Schemas[string(version)])
		if err != nil {
			t.Fatal(err)
		}
		engine, err := NewEngine(p)
		if err != nil {
			t.Fatal(err)
		}
		report, err := engine.Run()
		if err != nil {
			t.Fatalf("%s Run error: %v", version, err)
		}
		assertAggregatesMatch(t, bundle.Truth.Windows, report.Windows, p.CountTolerance)
		assertDropsMatch(t, bundle.Truth.Dropped, report.Dropped)
		if !CompareBatches(bundle.Truth.Batches, report.Batches) {
			t.Fatalf("%s batches differ from truth", version)
		}
	}
}

func assertErrorCode(t *testing.T, err error, code string) *Error {
	t.Helper()
	ge, ok := AsError(err)
	if !ok {
		t.Fatalf("expected *Error with code %s, got %T: %v", code, err, err)
	}
	if ge.Code != code {
		t.Fatalf("error code = %s, want %s (%s)", ge.Code, code, ge.Error())
	}
	return ge
}

func aggregateMap(windows []*Aggregate) map[string]*Aggregate {
	out := map[string]*Aggregate{}
	for _, agg := range windows {
		out[agg.Key.String()] = agg
	}
	return out
}

func assertAggregatesMatch(t *testing.T, expectedList, actualList []*Aggregate, tolerance int64) {
	t.Helper()
	expected := aggregateMap(expectedList)
	actual := aggregateMap(actualList)
	if len(expected) != len(actual) {
		t.Fatalf("window count = %d, want %d; missing=%v extra=%v", len(actual), len(expected), missingKeys(expected, actual), extraKeys(expected, actual))
	}
	for key, want := range expected {
		got := actual[key]
		if got == nil {
			t.Fatalf("missing window %s", key)
		}
		if got.RecordCount != want.RecordCount {
			t.Fatalf("%s count = %d, want %d", key, got.RecordCount, want.RecordCount)
		}
		if got.RequestBytes != want.RequestBytes || got.ResponseBytes != want.ResponseBytes {
			t.Fatalf("%s bytes = %d/%d, want %d/%d", key, got.RequestBytes, got.ResponseBytes, want.RequestBytes, want.ResponseBytes)
		}
		if len(got.BucketCounts) != len(want.BucketCounts) {
			t.Fatalf("%s bucket length = %d, want %d", key, len(got.BucketCounts), len(want.BucketCounts))
		}
		for bucket := range want.BucketCounts {
			delta := got.BucketCounts[bucket] - want.BucketCounts[bucket]
			if delta < 0 {
				delta = -delta
			}
			if delta > tolerance {
				t.Fatalf("%s bucket %d count = %d, want %d tolerance %d", key, bucket, got.BucketCounts[bucket], want.BucketCounts[bucket], tolerance)
			}
		}
		if !reflect.DeepEqual(got.ErrorClassCounts, want.ErrorClassCounts) {
			t.Fatalf("%s error classes = %v, want %v", key, got.ErrorClassCounts, want.ErrorClassCounts)
		}
	}
}

func missingKeys(expected, actual map[string]*Aggregate) []string {
	var out []string
	for key := range expected {
		if actual[key] == nil {
			out = append(out, key)
		}
	}
	return out
}

func extraKeys(expected, actual map[string]*Aggregate) []string {
	var out []string
	for key := range actual {
		if expected[key] == nil {
			out = append(out, key)
		}
	}
	return out
}

func assertDropsMatch(t *testing.T, expected, actual []DroppedRecord) {
	t.Helper()
	if CanonicalJSON(expected) != CanonicalJSON(actual) {
		t.Fatalf("drop sets differ\nexpected: %s\nactual:   %s", CanonicalJSON(expected), CanonicalJSON(actual))
	}
}
