package otlp

import (
	"encoding/json"
	"testing"
	"time"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

func TestParseOTLPJSONAndGroup(t *testing.T) {
	payload := map[string]any{
		"resourceLogs": []any{
			map[string]any{
				"resource": map[string]any{
					"attributes": []any{
						map[string]any{"key": "tenant.id", "value": map[string]any{"stringValue": "tenant-02"}},
					},
				},
				"scopeLogs": []any{
					map[string]any{
						"logRecords": []any{
							map[string]any{
								"timeUnixNano": "1700000000000000000",
								"severityText": "INFO",
								"body":         map[string]any{"stringValue": "hello"},
								"attributes": []any{
									map[string]any{"key": "record.id", "value": map[string]any{"stringValue": "record-1"}},
									map[string]any{"key": "batch.id", "value": map[string]any{"stringValue": "batch-1"}},
								},
							},
						},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	records, err := Parse(raw, "application/json", ParseOptions{Now: time.Unix(1, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if records[0].TenantID != "tenant-02" || records[0].RecordID != "record-1" || records[0].BatchID != "batch-1" {
		t.Fatalf("unexpected record identity: %+v", records[0])
	}
	groups := GroupRecords(records, RequestHash("", records))
	if len(groups) != 1 || groups[0].BatchID != "batch-1" || groups[0].Count != 1 {
		t.Fatalf("unexpected groups: %+v", groups)
	}
}

func TestParseOTLPProtobuf(t *testing.T) {
	request := &collectorlogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{
			{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{
						{Key: "tenant.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "tenant-05"}}},
					},
				},
				ScopeLogs: []*logspb.ScopeLogs{
					{
						LogRecords: []*logspb.LogRecord{
							{
								TimeUnixNano: 1700000000000000000,
								Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "protobuf"}},
								Attributes: []*commonpb.KeyValue{
									{Key: "record.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "p-record"}}},
								},
							},
						},
					},
				},
			},
		},
	}
	raw, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	records, err := Parse(raw, "application/x-protobuf", ParseOptions{HeaderBatchID: "header-batch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].TenantID != "tenant-05" || records[0].BatchID != "header-batch" {
		t.Fatalf("unexpected record: %+v", records)
	}
}

func TestParseVectorNDJSON(t *testing.T) {
	raw := []byte(`{"message":"from vector","timestamp":"2026-01-01T00:00:00Z","resources":{"tenant.id":"tenant-03"},"attributes":{"record.id":"vector-record","batch.id":"vector-batch"}}` + "\n")
	records, err := Parse(raw, "application/x-ndjson", ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if records[0].TenantID != "tenant-03" || records[0].RecordID != "vector-record" || records[0].BatchID != "vector-batch" {
		t.Fatalf("unexpected vector record: %+v", records[0])
	}
}

func TestParseRejectsMissingTenant(t *testing.T) {
	raw := []byte(`{"message":"missing tenant","attributes":{"record.id":"r"}}`)
	if _, err := Parse(raw, "application/x-ndjson", ParseOptions{}); err == nil {
		t.Fatal("expected missing tenant error")
	}
}
