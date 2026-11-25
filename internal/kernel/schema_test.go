package kernel

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestThreeSchemaVersionsLoadAndPredictIdentically(t *testing.T) {
	sample, err := GenerateSampleSet(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{SchemaV1, SchemaV2, SchemaV3} {
		loaded, err := LoadDocument(sample.Documents[version])
		if err != nil {
			t.Fatalf("v%d load: %v", version, err)
		}
		if len(loaded.Scene.Segments) != len(sample.Scene.Segments) {
			t.Fatalf("v%d segments=%d want %d", version, len(loaded.Scene.Segments), len(sample.Scene.Segments))
		}
		report, err := Predict(loaded.Scene)
		if err != nil {
			t.Fatalf("v%d predict: %v", version, err)
		}
		if len(report.Trajectories) != len(sample.Report.Trajectories) {
			t.Fatalf("v%d trajectories=%d", version, len(report.Trajectories))
		}
		for i := range sample.Report.Trajectories {
			want := sample.Report.Trajectories[i]
			got := report.Trajectories[i]
			if got.Attribution.FirstHitSegmentID != want.Attribution.FirstHitSegmentID ||
				got.Attribution.BounceCount != want.Attribution.BounceCount ||
				got.Attribution.Escaped != want.Attribution.Escaped {
				t.Fatalf("v%d trajectory %s attribution mismatch: got=%+v want=%+v",
					version, want.RequestID, got.Attribution, want.Attribution)
			}
			for j := range want.Samples {
				if j >= len(got.Samples) || got.Samples[j].Position != want.Samples[j].Position {
					t.Fatalf("v%d trajectory %s sample %d mismatch", version, want.RequestID, j)
				}
			}
		}
	}

	v1, err := LoadDocument(sample.Documents[SchemaV1])
	if err != nil {
		t.Fatal(err)
	}
	if len(v1.Transcript.Fills) < 2*len(sample.Scene.Segments) {
		t.Fatalf("expected normal and penetration fill traces, got %+v", v1.Transcript.Fills)
	}
}

func TestExportV1RoundTripPreservesEndpointsAndRestitution(t *testing.T) {
	sample, err := GenerateSampleSet(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := Export(sample.Scene, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if len(exported.Transcript.Losses) == 0 {
		t.Fatal("expected E_LOSSY records for fields absent from v1")
	}
	loaded, err := LoadDocument(exported.Data)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Segment{}
	for _, seg := range loaded.Scene.Segments {
		byID[seg.ID] = seg
	}
	for _, want := range sample.Scene.Segments {
		got := byID[want.ID]
		if got.A != want.A || got.B != want.B {
			t.Fatalf("segment %s endpoints changed: got %+v want %+v", want.ID, got, want)
		}
		if got.Restitution != want.Restitution {
			t.Fatalf("segment %s restitution got %d want %d", want.ID, got.Restitution, want.Restitution)
		}
	}
}

func TestHighLowHighFieldRoundTripAndIntegerConversions(t *testing.T) {
	sample, err := GenerateSampleSet(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	high, err := LoadDocument(sample.Documents[SchemaV3])
	if err != nil {
		t.Fatal(err)
	}
	lowExport, err := Export(high.Scene, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	low, err := LoadDocument(lowExport.Data)
	if err != nil {
		t.Fatal(err)
	}
	highAgainExport, err := Export(low.Scene, SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	highAgain, err := LoadDocument(highAgainExport.Data)
	if err != nil {
		t.Fatal(err)
	}
	for i := range high.Scene.Segments {
		want := high.Scene.Segments[i]
		got := highAgain.Scene.Segments[i]
		if got.A != want.A || got.B != want.B || got.Restitution != want.Restitution {
			t.Fatalf("high-low-high mismatch segment %s: got A=%+v B=%+v r=%d; want A=%+v B=%+v r=%d",
				want.ID, got.A, got.B, got.Restitution, want.A, want.B, want.Restitution)
		}
	}

	if got, ok := PermilleToMyriad(820); !ok || got != 8200 {
		t.Fatalf("permille to myriad=%d ok=%v", got, ok)
	}
	if got, rem, err := DivScalar(8205, 10, RoundingNearest); err != nil || got != 821 || rem != 5 {
		t.Fatalf("rounded coefficient got=%d rem=%d err=%v", got, rem, err)
	}
	if got, ok := TicksToMS(3, 50); !ok || got != 150 {
		t.Fatalf("ticks to ms=%d ok=%v", got, ok)
	}
	if got, rem, err := DivScalar(150, 50, RoundingFloor); err != nil || got != 3 || rem != 0 {
		t.Fatalf("ms to ticks got=%d rem=%d err=%v", got, rem, err)
	}
}

func TestIncrementalEndpointDowngradeCreatesLossTrace(t *testing.T) {
	sample, err := GenerateSampleSet(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	loadedV2, err := LoadDocument(sample.Documents[SchemaV2])
	if err != nil {
		t.Fatal(err)
	}
	exportedV1, err := Export(loadedV2.Scene, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, issue := range exportedV1.Transcript.Losses {
		if issue.Code == CodeLossy && issue.Field == "$.walls[0].start_delta" {
			found = true
			if issue.Disposition != "flatten_incremental_chain_to_absolute_endpoints" {
				t.Fatalf("unexpected disposition %q", issue.Disposition)
			}
		}
	}
	if !found {
		t.Fatalf("missing incremental endpoint E_LOSSY trace: %+v", exportedV1.Transcript.Losses)
	}
	if _, err := LoadDocument(exportedV1.Data); err != nil {
		t.Fatalf("downgraded artifact not loadable: %v", err)
	}
}

func TestSchemaErrors(t *testing.T) {
	if _, err := LoadDocument([]byte(`{not json`)); err == nil || errorCode(err) != CodeDecode {
		t.Fatalf("corrupt json error=%v want E_DECODE", err)
	}
	if _, err := LoadDocument([]byte(`{"schema_version":99}`)); err == nil || errorCode(err) != CodeSchema {
		t.Fatalf("unknown schema error=%v want E_SCHEMA", err)
	}

	sample, err := GenerateSampleSet(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(sample.Documents[SchemaV3], &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "level_id")
	data, _ := json.Marshal(doc)
	_, err = LoadDocument(data)
	if code := errorCode(err); code != CodeSchema {
		t.Fatalf("missing level id code=%v err=%v want E_SCHEMA", code, err)
	}
	var kernelErr *Error
	if errors.As(err, &kernelErr) && kernelErr.Field != "$.level_id" {
		t.Fatalf("field path=%q want $.level_id", kernelErr.Field)
	}

	_, err = CheckedMulField(math.MaxInt64, PosScale, "$.overflow", "id", 3)
	if code := errorCode(err); code != CodeOvf {
		t.Fatalf("overflow code=%v err=%v want E_OVF", code, err)
	}
}

func errorCode(err error) ErrorCode {
	var kernelErr *Error
	if errors.As(err, &kernelErr) {
		return kernelErr.Code
	}
	return ""
}
