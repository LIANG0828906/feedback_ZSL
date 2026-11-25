package replaybench_test

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"replaybench/internal/diff"
	"replaybench/internal/eerr"
	"replaybench/internal/engine"
	"replaybench/internal/generator"
	"replaybench/internal/loader"
	"replaybench/internal/model"
	"replaybench/internal/timeconv"
)

func loadSample(t *testing.T, format model.FormatID) *loader.Loaded {
	t.Helper()
	set, err := generator.DeterministicSet(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loader.LoadJSON(set.JSON[format])
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestSchemaErrorsPointAtFieldPath(t *testing.T) {
	set, _ := generator.DeterministicSet(generator.DefaultSeed)
	var doc map[string]any
	if err := json.Unmarshal(set.JSON[model.FormatWeightCast], &doc); err != nil {
		t.Fatal(err)
	}
	doc["format"] = float64(404)
	raw, _ := json.Marshal(doc)
	_, err := loader.LoadJSON(raw)
	if !eerr.IsCode(err, eerr.ESchema) {
		t.Fatalf("unknown format err = %v", err)
	}
	if e, _ := eerr.AsError(err); e.Field != "$.format" {
		t.Fatalf("field = %q", e.Field)
	}

	delete(doc, "format")
	raw, _ = json.Marshal(doc)
	_, err = loader.LoadJSON(raw)
	if !eerr.IsCode(err, eerr.ESchema) {
		t.Fatalf("missing format err = %v", err)
	}

	var unitDoc map[string]any
	_ = json.Unmarshal(set.JSON[1], &doc)
	units := doc["units"].([]any)
	unitDoc = units[0].(map[string]any)
	delete(unitDoc, "primary_int")
	raw, _ = json.Marshal(doc)
	_, err = loader.LoadJSON(raw)
	if !eerr.IsCode(err, eerr.ESchema) {
		t.Fatalf("missing primary_int err = %v", err)
	}
	if e, _ := eerr.AsError(err); e.Field != "$.units[0].primary_int" {
		t.Fatalf("field = %q want units[0].primary_int", e.Field)
	}
}

func TestTimeConversionOverflowAndRoundTrip(t *testing.T) {
	basis := model.TimeBasis{MsPerRound: math.MaxInt64, Rounding: timeconv.Floor}
	_, err := timeconv.RoundToTimestamp(math.MaxInt, basis, "$.frames[1].timestamp_ms")
	if !eerr.IsCode(err, eerr.EOvf) {
		t.Fatalf("err = %v", err)
	}
	ee, ok := eerr.AsError(err)
	if !ok || ee.Field != "$.frames[1].timestamp_ms" || len(ee.Operands) != 2 {
		t.Fatalf("overflow detail wrong: %+v", ee)
	}

	normal := model.TimeBasis{MsPerRound: 1000, EpochMs: 1700000000000, Rounding: timeconv.Floor}
	for round := 1; round <= 12; round++ {
		ms, back, err := timeconv.RoundTrip(round, normal, "$.frames[i].timestamp_ms")
		if err != nil || back != round {
			t.Fatalf("round=%d ms=%d back=%d err=%v", round, ms, back, err)
		}
	}
}

func TestHighLowHighFormatRoundTripPreservesSemantics(t *testing.T) {
	set, _ := generator.DeterministicSet(generator.DefaultSeed)
	high := set.JSON[model.FormatTimestamp]
	low, err := loader.ConvertDocument(high, model.FormatWeightCast)
	if err != nil {
		t.Fatal(err)
	}
	highAgain, err := loader.ConvertDocument(low, model.FormatTimestamp)
	if err != nil {
		t.Fatal(err)
	}
	a, err := loader.LoadJSON(high)
	if err != nil {
		t.Fatal(err)
	}
	b, err := loader.LoadJSON(highAgain)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Replay.Directives, b.Replay.Directives) {
		t.Fatalf("directives changed:\n%#v\n%#v", a.Replay.Directives, b.Replay.Directives)
	}
	if !reflect.DeepEqual(a.Replay.Units, b.Replay.Units) {
		t.Fatal("unit weight/cooldown slots changed")
	}
	ra, err := engine.Run(context.Background(), a.Replay)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := engine.Run(context.Background(), b.Replay)
	if err != nil {
		t.Fatal(err)
	}
	if err := diff.Snapshots(ra.Snapshots, rb.Snapshots); err != nil {
		t.Fatal(err)
	}
}

func TestPrecedenceCycleReturnsEConflict(t *testing.T) {
	loaded := loadSample(t, model.FormatWeightCast)
	loaded.Replay.Rules.Precedence = [][2]string{{"a_nova", "a_strike"}, {"a_strike", "a_nova"}}
	_, err := engine.Run(context.Background(), loaded.Replay)
	if !eerr.IsCode(err, eerr.EConflict) {
		t.Fatalf("err = %v", err)
	}
	ee, _ := eerr.AsError(err)
	if !reflect.DeepEqual(ee.Set, []string{"a_nova", "a_strike"}) {
		t.Fatalf("conflict set = %v", ee.Set)
	}
}

func TestConditionalBitFlagConversion(t *testing.T) {
	set, _ := generator.DeterministicSet(generator.DefaultSeed)
	var doc map[string]any
	if err := json.Unmarshal(set.JSON[model.FormatWeightCast], &doc); err != nil {
		t.Fatal(err)
	}
	doc["flags"].(map[string]any)["execute_flag"] = float64(0)
	raw, _ := json.Marshal(doc)
	loaded, err := loader.LoadJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Run(context.Background(), loaded.Replay)
	if err != nil {
		t.Fatal(err)
	}
	for _, adj := range res.Snapshots[1].Adjudications {
		if adj.SkillID == "a_execute" {
			if adj.Result != "skipped" || adj.ContentionType != "condition_flag" {
				t.Fatalf("conditional adjudication = %+v", adj)
			}
			return
		}
	}
	t.Fatal("a_execute adjudication not found")
}
