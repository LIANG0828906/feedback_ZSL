package registry_test

import (
	"encoding/json"
	"strings"
	"testing"

	"serviceregistry/registry"
	"serviceregistry/samplegen"
)

func TestLoaderThreeVersionsNormalizeDurationsThresholdsAndOrder(t *testing.T) {
	bundle, err := samplegen.BuildBundle(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	var baseline *registry.Project
	for _, schema := range []registry.SchemaVersion{registry.SchemaV1, registry.SchemaV2, registry.SchemaV3} {
		document, err := registry.LoadDocument(bundle.Files[schema])
		if err != nil {
			t.Fatalf("%s: %v", schema, err)
		}
		project := document.Project
		if baseline == nil {
			baseline = project
			continue
		}
		if len(project.Services) != len(baseline.Services) || len(project.Instances) != len(baseline.Instances) {
			t.Fatalf("%s shape differs", schema)
		}
		for index := range project.Instances {
			left := project.Instances[index]
			right := baseline.Instances[index]
			if left.ID != right.ID || left.ServiceID != right.ServiceID || left.Order != right.Order {
				t.Fatalf("%s instance order differs at %d: %s/%s", schema, index, left.ID, right.ID)
			}
			if left.LeaseTTLMS != right.LeaseTTLMS || left.HeartbeatMS != right.HeartbeatMS ||
				left.ProbePeriodMS != right.ProbePeriodMS || left.ProbeTimeoutMS != right.ProbeTimeoutMS {
				t.Fatalf("%s duration normalization differs for %s", schema, left.ID)
			}
			if left.SuccessThreshold != right.SuccessThreshold || left.FailureThreshold != right.FailureThreshold {
				t.Fatalf("%s threshold normalization differs for %s", schema, left.ID)
			}
			if left.LeaseWatermarkMS != nil || right.LeaseWatermarkMS != nil {
				t.Fatalf("%s watermark leaked into runtime model", schema)
			}
		}
	}
	if baseline.Instances[0].LeaseTTLMS != 1500 || baseline.Instances[0].SuccessThreshold != 2 {
		t.Fatalf("unexpected normalized baseline: %+v", baseline.Instances[0])
	}
}

func TestLoaderUnknownFieldHasStableCodeAndPath(t *testing.T) {
	bundle, err := samplegen.BuildBundle(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(bundle.Files[registry.SchemaV2], &document); err != nil {
		t.Fatal(err)
	}
	services := document["services"].([]any)
	service := services[0].(map[string]any)
	instances := service["instances"].([]any)
	instance := instances[0].(map[string]any)
	instance["lease_ttl_ticks"] = 10
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.LoadDocument(data)
	if registry.ErrorCode(err) != registry.CodeSchemaField {
		t.Fatalf("code = %s, err = %v", registry.ErrorCode(err), err)
	}
	biz, _ := registry.AsError(err)
	if !strings.HasSuffix(biz.Context.Path, ".instances[0].lease_ttl_ticks") {
		t.Fatalf("path = %s", biz.Context.Path)
	}
}

func TestLoaderUnknownSchemaAndMalformedJSON(t *testing.T) {
	_, err := registry.LoadDocument([]byte(`{"schemaVersion":"v9"}`))
	if registry.ErrorCode(err) != registry.CodeSchemaVersionMismatch && registry.ErrorCode(err) != registry.CodeSchemaUnknown {
		t.Fatalf("unknown schema code = %s (%v)", registry.ErrorCode(err), err)
	}
	_, err = registry.LoadDocument([]byte(`{"schemaVersion":`))
	if registry.ErrorCode(err) != registry.CodeSchemaLocation {
		t.Fatalf("malformed code = %s (%v)", registry.ErrorCode(err), err)
	}
}

func TestV3WatermarkDisambiguatesLegacyLeaseAndThresholdMode(t *testing.T) {
	bundle, err := samplegen.BuildBundle(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(bundle.Files[registry.SchemaV3], &document); err != nil {
		t.Fatal(err)
	}
	delete(document, "truth")
	service := document["services"].([]any)[0].(map[string]any)
	instance := service["instances"].([]any)[0].(map[string]any)
	delete(instance, "lease-ttl-seconds")
	delete(instance, "lease-ttl-millis")
	instance["lease-ttl"] = int64(5)
	instance["lease-watermark-ms"] = int64(500)
	delete(instance, "success-threshold-remaining")
	delete(instance, "failure-threshold-remaining")
	instance["success-threshold"] = int64(2)
	instance["failure-threshold"] = int64(2)
	instance["threshold-mode"] = "absolute"
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.LoadProject(data)
	if err != nil {
		t.Fatal(err)
	}
	loaded := project.Instances[0]
	if loaded.LeaseTTLMS != 500 {
		t.Fatalf("legacy lease normalized to %d, want 500", loaded.LeaseTTLMS)
	}
	if loaded.SuccessThreshold != 2 || loaded.FailureThreshold != 2 {
		t.Fatalf("generic thresholds normalized to %d/%d", loaded.SuccessThreshold, loaded.FailureThreshold)
	}
}
