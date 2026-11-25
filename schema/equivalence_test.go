package schema_test

import (
	"reflect"
	"testing"

	"github.com/liang/collector-backpressure/bpderr"
	"github.com/liang/collector-backpressure/model"
	"github.com/liang/collector-backpressure/planner"
	"github.com/liang/collector-backpressure/sample"
	"github.com/liang/collector-backpressure/schema"
)

func loadVersion(t *testing.T, p *model.Project, version string) *model.Project {
	t.Helper()
	data, err := schema.RenderJSON(version, p)
	if err != nil {
		t.Fatalf("render %s: %v", version, err)
	}
	loaded, err := schema.LoadJSON(data)
	if err != nil {
		t.Fatalf("load %s: %v\n%s", version, err, string(data))
	}
	return loaded
}

func comparableCore(p *model.Project) model.Project {
	core := *p
	core.SchemaVersion = ""
	for i := range core.Partitions {
		core.Partitions[i].ConsumerWatermark = nil
	}
	return core
}

func TestThreeSchemaVersionsLoadEquivalentCore(t *testing.T) {
	original := sample.GenerateProject(sample.DefaultSeed)
	v1 := loadVersion(t, original, schema.V1)
	v2 := loadVersion(t, original, schema.V2)
	v3 := loadVersion(t, original, schema.V3)

	want := comparableCore(original)
	if got := comparableCore(v1); !reflect.DeepEqual(got, want) {
		t.Fatalf("v1 core mismatch\n got=%#v\nwant=%#v", got, want)
	}
	if got := comparableCore(v2); !reflect.DeepEqual(got, want) {
		t.Fatalf("v2 core mismatch\n got=%#v\nwant=%#v", got, want)
	}
	if got := comparableCore(v3); !reflect.DeepEqual(got, want) {
		t.Fatalf("v3 core mismatch\n got=%#v\nwant=%#v", got, want)
	}

	chunks1, err := planner.BuildChunks(v1)
	if err != nil {
		t.Fatal(err)
	}
	chunks2, err := planner.BuildChunks(v2)
	if err != nil {
		t.Fatal(err)
	}
	chunks3, err := planner.BuildChunks(v3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(chunks2, chunks1) || !reflect.DeepEqual(chunks3, chunks1) {
		t.Fatalf("chunk planning differs across schema versions")
	}
	for _, truth := range mustBundle(t).Truth {
		var found bool
		for _, chunk := range chunks1 {
			if chunk.BatchID == truth.BatchID {
				found = true
				if chunk.PartitionID != truth.PartitionID {
					t.Fatalf("truth partition mismatch for %s: got %s want %s", truth.BatchID, chunk.PartitionID, truth.PartitionID)
				}
			}
		}
		if !found {
			t.Fatalf("truth batch %s has no chunks", truth.BatchID)
		}
	}
}

func mustBundle(t *testing.T) *sample.Bundle {
	t.Helper()
	bundle, err := sample.GenerateBundle(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestAmbiguousBufferAndTimeReturnStableCodes(t *testing.T) {
	_, err := schema.NormalizeBuffer(32, true, 9, true, 4)
	if code, ok := bpderr.As(err); !ok || code.Code != bpderr.ESchemaAmbiguous {
		t.Fatalf("expected E_SCHEMA_AMBIGUOUS buffer, got %v", err)
	}
	_, err = schema.NormalizeTickTime(3, true, 40, true, 10, "arrival")
	if code, ok := bpderr.As(err); !ok || code.Code != bpderr.ESchemaAmbiguous {
		t.Fatalf("expected E_SCHEMA_AMBIGUOUS time, got %v", err)
	}
	_, err = schema.NormalizeTickTime(0, false, 25, true, 10, "arrival")
	if code, ok := bpderr.As(err); !ok || code.Code != bpderr.ESchemaAmbiguous {
		t.Fatalf("expected non-divisible time ambiguity, got %v", err)
	}
	_, err = schema.NormalizeByteOrEquiv(220, true, 54, true, 4, "backlogLimit")
	if code, ok := bpderr.As(err); !ok || code.Code != bpderr.ESchemaAmbiguous {
		t.Fatalf("expected E_SCHEMA_AMBIGUOUS backlog expression, got %v", err)
	}
}
