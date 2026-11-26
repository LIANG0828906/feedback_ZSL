package diagnostics_test

import (
	"testing"

	"dialogueaudio/diagnostics"
	"dialogueaudio/engine"
	"dialogueaudio/generator"
	"dialogueaudio/pipeline"
)

func TestFullAcceptanceReport(t *testing.T) {
	report, err := diagnostics.Run(generator.DefaultSeed, t.TempDir(), pipeline.Options{
		Batch: engine.BatchOptions{MaxParallel: 2, QueueBudget: 1, SampleBufferSamples: 960},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := diagnostics.ValidateReport(report); err != nil {
		t.Fatal(err)
	}
}
