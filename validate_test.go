package syncgame

import (
	"encoding/json"
	"testing"
)

func TestParseSampleValidatesFieldPresence(t *testing.T) {
	sample := testFixture(t)
	data, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "tick_rate")
	broken, _ := json.Marshal(raw)
	_, parseErr := ParseSample(broken)
	gameError := requireCode(t, parseErr, CodeTick)
	if gameError.Field != "tick_rate" {
		t.Fatalf("expected tick_rate field, got %+v", gameError)
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var intents []map[string]json.RawMessage
	if err := json.Unmarshal(raw["intents"], &intents); err != nil {
		t.Fatal(err)
	}
	var inputs []map[string]json.RawMessage
	if err := json.Unmarshal(intents[0]["inputs"], &inputs); err != nil {
		t.Fatal(err)
	}
	delete(inputs[0], "kind")
	intents[0]["inputs"], _ = json.Marshal(inputs)
	raw["intents"], _ = json.Marshal(intents)
	broken, _ = json.Marshal(raw)
	_, parseErr = ParseSample(broken)
	gameError = requireCode(t, parseErr, CodeTick)
	if gameError.Tick != 0 || gameError.PlayerID != "p1" || gameError.Field != "intents.inputs.kind" {
		t.Fatalf("unexpected missing-field error: %+v", gameError)
	}
}

func TestValidateSampleTickDisorder(t *testing.T) {
	sample := cloneSample(t, testFixture(t))
	sample.Intents[7].Tick = 9
	err := ValidateSample(sample)
	gameError := requireCode(t, err, CodeTick)
	if gameError.Tick != 9 || gameError.PlayerID == "" || gameError.Field != "intents.tick" {
		t.Fatalf("unexpected tick error: %+v", gameError)
	}
}

func TestValidateSamplePlayerTierAndFiniteBounds(t *testing.T) {
	sample := cloneSample(t, testFixture(t))
	sample.Units[1].PlayerID = sample.Units[0].PlayerID
	requireCode(t, ValidateSample(sample), CodeTick)

	sample = cloneSample(t, testFixture(t))
	sample.LatencyTiers[0].ObservationDelayFrames = -1
	requireCode(t, ValidateSample(sample), CodeTick)

	sample = cloneSample(t, testFixture(t))
	sample.LatencyTiers[0].ID = "unknown"
	requireCode(t, ValidateSample(sample), CodeTick)

	sample = cloneSample(t, testFixture(t))
	sample.Units[0].MoveSpeed = 1 << 30
	requireCode(t, ValidateSample(sample), CodeTick)
}

func TestGeneratedTruthRoundTrip(t *testing.T) {
	sample := testFixture(t)
	if err := ValidateTruth(sample); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSample(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateTruth(parsed); err != nil {
		t.Fatal(err)
	}
}
