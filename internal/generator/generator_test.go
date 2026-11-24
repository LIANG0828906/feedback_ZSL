package generator

import (
	"bytes"
	"testing"
)

func TestGenerateDeterministicAndTruth(t *testing.T) {
	first := Generate(Options{Seed: 20261124, Samples: 3, Prefix: "test"})
	second := Generate(Options{Seed: 20261124, Samples: 3, Prefix: "test"})
	firstJSON, err := Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatal("same seed produced different manifests")
	}
	if len(first.Samples) != 3 {
		t.Fatalf("got %d samples, want 3", len(first.Samples))
	}
	for _, sample := range first.Samples {
		if sample.SampleRate < 8000 || sample.SampleRate > 192000 {
			t.Fatalf("sample rate %d outside supported range", sample.SampleRate)
		}
		if len(sample.Layout.Channels) != len(sample.Channels) {
			t.Fatalf("layout/channel count mismatch for %s", sample.ID)
		}
		for _, channel := range sample.Channels {
			if len(channel.Samples) != sample.IRSamples {
				t.Fatalf("channel %s point count mismatch", channel.ID)
			}
			previous := channel.Truth.DirectArrival
			if previous == 0 && channel.Samples[previous] == 0 {
				t.Fatalf("direct impulse missing for %s", channel.ID)
			}
			for _, reflection := range channel.Truth.Reflections {
				if reflection.Arrival <= previous {
					t.Fatalf("reflection order inverted for %s", channel.ID)
				}
				previous = reflection.Arrival
			}
			if channel.Truth.DirectEnergy <= 0 {
				t.Fatalf("direct energy must be positive")
			}
		}
	}
}
