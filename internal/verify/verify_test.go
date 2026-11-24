package verify_test

import (
	"context"
	"testing"

	"economycore/internal/core"
	"economycore/internal/samples"
	"economycore/internal/verify"
)

func TestThreePathVerification(t *testing.T) {
	scene, err := samples.Build(samples.Profile{
		ID: "verify_tiny", Intents: 420, Horizon: 320, TruthSamples: 8,
	}, 1234)
	if err != nil {
		t.Fatal(err)
	}
	opts := verify.DefaultOptions(t.TempDir())
	opts.WindowSize = 32
	opts.PreviewStep = 8
	opts.Workers = 3
	opts.Buffer = 2
	opts.ResumeAfter = 2
	report, err := verify.Run(context.Background(), []*core.Scenario{scene}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !report.AllOK {
		t.Fatalf("verification failed: %+v", report)
	}
}
