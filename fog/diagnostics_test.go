package fog

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
)

func TestDiagnosticsReportJSONAndResults(t *testing.T) {
	s := smallScene(t)
	rep, err := RunDiagnostics(s, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Preview == nil || rep.Exact == nil || rep.Comparison == nil {
		t.Fatalf("diagnostics missing core frames: %+v", rep)
	}
	if !rep.Comparison.SameCoverage || rep.Comparison.Diverged {
		t.Fatalf("preview/exact comparison failed: %+v fault=%v", rep.Comparison, rep.ComparisonFault)
	}
	if rep.CacheNoEditHits != len(rep.Exact.Tiles) {
		t.Fatalf("no-edit cache hits=%d want=%d", rep.CacheNoEditHits, len(rep.Exact.Tiles))
	}
	if rep.EditReport == nil || len(rep.EditReport.Tiles) == 0 {
		t.Fatalf("invalidation not exercised: %+v", rep.EditReport)
	}
	if rep.PostEditHits != len(rep.Exact.Tiles)-len(rep.EditReport.Tiles) {
		t.Fatalf("post-edit cache hits=%d invalidated=%d total=%d", rep.PostEditHits, len(rep.EditReport.Tiles), len(rep.Exact.Tiles))
	}
	if rep.ThreePath == nil || !rep.ThreePath.PixelsEqual || !rep.ThreePath.SummariesEqual || !rep.ThreePath.CommitEqual {
		t.Fatalf("three-path diagnostic failed: %+v fault=%v", rep.ThreePath, rep.ThreePathFault)
	}
	if _, err := json.Marshal(rep); err != nil {
		t.Fatalf("diagnostics JSON encoding failed: %v; first non-finite: %s", err, findNonFinite(reflect.ValueOf(rep), "$"))
	}
	for _, inj := range rep.Injections {
		if inj.Fault == nil || inj.Code == "" {
			t.Fatalf("injection report incomplete: %+v", inj)
		}
	}
}

func findNonFinite(v reflect.Value, path string) string {
	if !v.IsValid() {
		return ""
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return ""
		}
		return findNonFinite(v.Elem(), path)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if got := findNonFinite(v.Field(i), path+"."+v.Type().Field(i).Name); got != "" {
				return got
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if got := findNonFinite(v.Index(i), fmt.Sprintf("%s[%d]", path, i)); got != "" {
				return got
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if got := findNonFinite(iter.Value(), fmt.Sprintf("%s[%v]", path, iter.Key())); got != "" {
				return got
			}
		}
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return path + "=" + fmt.Sprint(f)
		}
	}
	return ""
}
