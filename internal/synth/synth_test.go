package synth_test

import (
	"fmt"
	"testing"

	"audioforge/internal/engine"
	"audioforge/internal/generator"
	"audioforge/internal/model"
	"audioforge/internal/verify"
)

func TestSynthesisMatchesGeneratorTruth(t *testing.T) {
	bundle, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := bundle.SelectStream("arena-tick")
	if err != nil {
		t.Fatal(err)
	}
	params := model.BuildParameters{
		ParamVersion:     "p1",
		AlgorithmVersion: "a1",
		CommitWindow:     4,
		Workers:          3,
		BufferPerWorker:  2,
	}
	built, err := (&engine.Engine{}).Build(stream.Events, bundle.Capability, params)
	if err != nil {
		t.Fatal(err)
	}
	if err := verify.Truth(built, bundle.Truth, bundle.Capability); err != nil {
		t.Fatal(err)
	}
	if err := verify.Conditions(stream.Events, bundle.Truth); err != nil {
		t.Fatal(err)
	}
	foundGate := false
	for _, decision := range built.Decisions {
		if decision.EventID == "evt-008" {
			foundGate = true
		}
	}
	if !foundGate {
		t.Fatal("expected the inactive conditional event to be recorded")
	}
}

func TestSynthesisIsDeterministicAcrossWorkerCounts(t *testing.T) {
	bundle, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	stream, _ := bundle.SelectStream("arena-tick")
	run := func(workers, buffer int) *engine.Result {
		params := model.BuildParameters{
			ParamVersion:     "p1",
			AlgorithmVersion: "a1",
			CommitWindow:     3,
			Workers:          workers,
			BufferPerWorker:  buffer,
		}
		result, err := (&engine.Engine{}).Build(stream.Events, bundle.Capability, params)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	a := run(1, 1)
	b := run(3, 8)
	if err := engine.Compare("serial", a, "parallel", b); err != nil {
		t.Fatal(err)
	}
	if a.Workers != 1 || b.Workers < 2 {
		t.Fatalf("unexpected worker stats: %d %d", a.Workers, b.Workers)
	}
}

func TestFrameDigestIsStable(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	stream, _ := bundle.SelectStream("mobile-ms")
	params := model.BuildParameters{ParamVersion: "p", AlgorithmVersion: "a", CommitWindow: 2, Workers: 2, BufferPerWorker: 2}
	first, err := (&engine.Engine{}).Build(stream.Events, bundle.Capability, params)
	if err != nil {
		t.Fatal(err)
	}
	second, err := (&engine.Engine{}).Build(stream.Events, bundle.Capability, params)
	if err != nil {
		t.Fatal(err)
	}
	for i := range first.Frames {
		if fmt.Sprint(first.Frames[i].Samples) != fmt.Sprint(second.Frames[i].Samples) {
			t.Fatalf("channel %d frame differs", i)
		}
	}
}

func TestInputSliceTraversalOrderDoesNotAffectOutput(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	stream, _ := bundle.SelectStream("arena-tick")
	params := model.BuildParameters{ParamVersion: "p", AlgorithmVersion: "a", CommitWindow: 3, Workers: 3, BufferPerWorker: 2}
	normal, err := (&engine.Engine{}).Build(stream.Events, bundle.Capability, params)
	if err != nil {
		t.Fatal(err)
	}
	reversed := make([]model.Event, len(stream.Events))
	for i := range stream.Events {
		reversed[len(stream.Events)-1-i] = stream.Events[i]
	}
	shuffled, err := (&engine.Engine{}).Build(reversed, bundle.Capability, params)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Compare("normal", normal, "reversed-input", shuffled); err != nil {
		t.Fatal(err)
	}
}
