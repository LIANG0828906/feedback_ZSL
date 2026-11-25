package samplegen_test

import (
	"encoding/json"
	"testing"

	"example.com/multiscreen-cast/protocol"
	"example.com/multiscreen-cast/samplegen"
)

func TestGeneratorIsDeterministic(t *testing.T) {
	options := samplegen.Options{Seed: 101, Screens: 4, Windows: 9, Sessions: 2}
	first, err := samplegen.Generate(options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := samplegen.Generate(options)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := json.Marshal(first)
	right, _ := json.Marshal(second)
	if string(left) != string(right) {
		t.Fatal("same seed produced different sample documents")
	}
}

func TestGeneratedTruthShape(t *testing.T) {
	set, err := samplegen.Generate(samplegen.Options{Seed: 202, Screens: 5, Windows: 7, Sessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	truth := set.Truths[0]
	if len(truth.Windows) != 7 {
		t.Fatalf("truth windows=%d", len(truth.Windows))
	}
	if truth.FinalState != protocol.StateEnded {
		t.Fatalf("final state=%s", truth.FinalState)
	}
	if len(truth.FinalEndpoints) == 0 || len(truth.EventEffects) == 0 {
		t.Fatalf("incomplete truth: %+v", truth)
	}
	replays := 0
	for _, event := range truth.EventEffects {
		if event.Replay != nil {
			replays++
		}
	}
	if replays == 0 {
		t.Fatal("sample did not exercise join replay truth")
	}
}
