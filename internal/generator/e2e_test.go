package generator_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"audioforge/internal/capability"
	"audioforge/internal/device"
	"audioforge/internal/engine"
	"audioforge/internal/generator"
	"audioforge/internal/model"
	"audioforge/internal/semantics"
	"audioforge/internal/store"
	"audioforge/internal/verify"
)

func TestEndToEndGeneratedSamplesToLoopbackEcho(t *testing.T) {
	dir := t.TempDir()
	bundle, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Write(dir); err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := capability.Load(filepath.Join(dir, "capability.json"))
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(bundle.Streams))
	for _, stream := range bundle.Streams {
		paths = append(paths, filepath.Join(dir, stream.File))
	}
	streams, err := semantics.LoadFiles(paths, capabilityValue.SampleRate, bundle.Registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := semantics.CompareStreams(streams); err != nil {
		t.Fatal(err)
	}
	selected := streams[0]
	for _, stream := range streams {
		if stream.Spec.Name == "mobile-ms" {
			selected = stream
		}
	}
	params := model.BuildParameters{ParamVersion: "e2e-p", AlgorithmVersion: "e2e-a", CommitWindow: 4, Workers: 3, BufferPerWorker: 2}
	db, err := store.Open(filepath.Join(dir, "checkpoints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runner := &engine.Engine{Store: db}
	result, err := runner.Run(context.Background(), "e2e", selected.Events, capabilityValue, params, engine.RunOptions{Persist: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := verify.Truth(result, bundle.Truth, capabilityValue); err != nil {
		t.Fatal(err)
	}
	replay, err := runner.Replay("e2e", params)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Compare("realtime", result, "replay", replay); err != nil {
		t.Fatal(err)
	}

	service, err := device.NewService(capabilityValue)
	if err != nil {
		t.Fatal(err)
	}
	baseURL, err := service.Start(0)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	client := device.NewClient(baseURL)
	exchange, err := client.Send(context.Background(), result.Packets)
	if err != nil {
		t.Fatal(err)
	}
	alignment := device.Align(exchange, 2*time.Second, exchange.CompletedAt)
	if len(alignment.Issues) != 0 {
		t.Fatalf("unexpected echo anomalies: %+v", alignment.Issues)
	}
	if len(alignment.Matches) != len(result.Packets) {
		t.Fatalf("aligned %d of %d packets", len(alignment.Matches), len(result.Packets))
	}
}
