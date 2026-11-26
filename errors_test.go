package slikernel

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func assertBusinessCode(t *testing.T, err error, code Code) *BizError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	business, ok := err.(*BizError)
	if !ok {
		t.Fatalf("expected *BizError %s, got %T: %v", code, err, err)
	}
	if business.Code != code {
		t.Fatalf("expected %s, got %s: %v", code, business.Code, err)
	}
	if len(business.Context) == 0 {
		t.Fatalf("expected location context for %s: %v", code, err)
	}
	return business
}

func TestStableLoaderBusinessCodes(t *testing.T) {
	data := fixture(t)
	v1, _ := EncodeProject(data.Project, 1)
	v2, _ := EncodeProject(data.Project, 2)
	v3, _ := EncodeProject(data.Project, 3)

	t.Run("json", func(t *testing.T) {
		_, err := LoadProject([]byte(`{"schema_version":`))
		assertBusinessCode(t, err, CodeJSON)
	})
	t.Run("schema", func(t *testing.T) {
		_, err := LoadProject([]byte(`{"schema_version":"9"}`))
		assertBusinessCode(t, err, CodeSchema)
	})
	t.Run("schema conflict", func(t *testing.T) {
		doc := map[string]any{"schema_version": "1", "schema": "2"}
		payload, _ := json.Marshal(doc)
		_, err := LoadProject(payload)
		assertBusinessCode(t, err, CodeAmbiguousField)
	})
	t.Run("duplicate service", func(t *testing.T) {
		payload := mutateJSON(t, v1, func(document map[string]any) {
			services := serviceMap(t, document)
			document["services"] = append(services, services[0])
		})
		_, err := LoadProject(payload)
		assertBusinessCode(t, err, CodeDuplicateService)
	})
	t.Run("duplicate sli", func(t *testing.T) {
		payload := mutateJSON(t, v1, func(document map[string]any) {
			service := serviceMap(t, document)[0].(map[string]any)
			slis := service["slis"].([]any)
			service["slis"] = append(slis, slis[0])
		})
		_, err := LoadProject(payload)
		assertBusinessCode(t, err, CodeDuplicateSLI)
	})
	t.Run("ambiguous target", func(t *testing.T) {
		payload := mutateJSON(t, v2, func(document map[string]any) {
			service := serviceMap(t, document)[0].(map[string]any)
			sli := sliMaps(t, service, "sli_defs")[0].(map[string]any)
			sli["target_percent"] = 99.5
		})
		_, err := LoadProject(payload)
		assertBusinessCode(t, err, CodeAmbiguousField)
	})
	t.Run("ratio range", func(t *testing.T) {
		payload := mutateJSON(t, v2, func(document map[string]any) {
			service := serviceMap(t, document)[0].(map[string]any)
			sli := sliMaps(t, service, "sli_defs")[0].(map[string]any)
			sli["target_bps"] = 10001
		})
		_, err := LoadProject(payload)
		assertBusinessCode(t, err, CodeRatioRange)
	})
	t.Run("quantile range", func(t *testing.T) {
		payload := mutateJSON(t, v2, func(document map[string]any) {
			service := serviceMap(t, document)[0].(map[string]any)
			sli := sliMaps(t, service, "sli_defs")[1].(map[string]any)
			sli["quantile_permille"] = 1001
		})
		_, err := LoadProject(payload)
		assertBusinessCode(t, err, CodeQuantileRange)
	})
	t.Run("window length", func(t *testing.T) {
		payload := mutateJSON(t, v2, func(document map[string]any) {
			service := serviceMap(t, document)[0].(map[string]any)
			sli := sliMaps(t, service, "sli_defs")[0].(map[string]any)
			sli["window_length"] = -1
		})
		_, err := LoadProject(payload)
		assertBusinessCode(t, err, CodeWindowLength)
	})
	t.Run("unknown duration unit", func(t *testing.T) {
		payload := mutateJSON(t, v2, func(document map[string]any) {
			service := serviceMap(t, document)[0].(map[string]any)
			sli := sliMaps(t, service, "sli_defs")[0].(map[string]any)
			sli["window_unit"] = "fortnight"
		})
		_, err := LoadProject(payload)
		assertBusinessCode(t, err, CodeUnit)
	})
	t.Run("ambiguous time basis", func(t *testing.T) {
		payload := mutateJSON(t, v2, func(document map[string]any) {
			observation := document["observations"].([]any)[0].(map[string]any)
			observation["timestamp_ms"] = 1700000000000
		})
		_, err := LoadProject(payload)
		assertBusinessCode(t, err, CodeAmbiguousField)
	})
	t.Run("observation sequence", func(t *testing.T) {
		payload := mutateJSON(t, v1, func(document map[string]any) {
			observations := document["observations"].([]any)
			observations[1].(map[string]any)["seq"] = observations[0].(map[string]any)["seq"]
		})
		_, err := LoadProject(payload)
		assertBusinessCode(t, err, CodeObservationSeq)
	})
	t.Run("ambiguous latency unit fields", func(t *testing.T) {
		payload := mutateJSON(t, v3, func(document map[string]any) {
			observation := document["observations"].([]any)[0].(map[string]any)
			metrics := observation["metrics"].(map[string]any)
			metrics["latency_samples_ms"] = []int{1, 2}
			if _, ok := metrics["latency_samples_us"]; !ok {
				metrics["latency_samples_us"] = []int{1000}
			}
		})
		_, err := LoadProject(payload)
		assertBusinessCode(t, err, CodeAmbiguousField)
	})
}

func TestSourceBytesRemainUnchangedOnLoaderFailure(t *testing.T) {
	data := fixture(t)
	payload, _ := EncodeProject(data.Project, 2)
	bad := mutateJSON(t, payload, func(document map[string]any) {
		service := serviceMap(t, document)[0].(map[string]any)
		sli := sliMaps(t, service, "sli_defs")[0].(map[string]any)
		sli["target_bps"] = 10001
	})
	before := append([]byte(nil), bad...)
	_, err := LoadProject(bad)
	if code := ErrorCode(err); code != CodeRatioRange {
		t.Fatalf("expected %s, got %v", CodeRatioRange, err)
	}
	if !bytes.Equal(before, bad) {
		t.Fatal("loader mutated source bytes on failure")
	}
}

func TestEditErrorsAreAtomicAndCoded(t *testing.T) {
	data := fixture(t)
	engine := newTestEngine(t, data.Project, testOptions())
	target := int64(990_000)
	_, err := engine.ApplyEdits(context.Background(), []Edit{
		{ID: "same", Kind: EditSetSLOTarget, Service: "checkout", SLI: "checkout-availability", TargetPPM: &target},
		{ID: "same", Kind: EditSetSLOTarget, Service: "search", SLI: "search-availability", TargetPPM: &target},
	})
	assertBusinessCode(t, err, CodeEdit)
	report, err := engine.CachedReport()
	if err != nil {
		t.Fatalf("engine became unusable after edit error: %v", err)
	}
	truth := compute(t, data.Project, testOptions())
	assertReportCoreEqual(t, truth, report)
}
