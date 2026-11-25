package qos

import (
	"context"
	"reflect"
	"testing"
)

func TestFixedConversionAndLongRun(t *testing.T) {
	a, err := ConvertRateToPermyriad(2500, 1000, 64)
	if err != nil || a != 25000 {
		t.Fatalf("per-mille conversion: %d %v", a, err)
	}
	b, err := ConvertRateToPermyriad(25000, 10000, 64)
	if err != nil || b != a {
		t.Fatalf("per-myriad conversion: %d %v", b, err)
	}
	capacity, err := ConvertPacketsToBytes(8, 128, 64)
	if err != nil || capacity != 1024 {
		t.Fatalf("capacity conversion: %d %v", capacity, err)
	}
	if _, err := ConvertRateToPermyriad(-1, 1000, 64); CodeOf(err) != ERateNegative {
		t.Fatalf("negative rate code=%s", CodeOf(err))
	}
	if _, err := ConvertRateToPermyriad(1<<31-1, 1000, 32); CodeOf(err) != ERateOverflow {
		t.Fatalf("width overflow code=%s", CodeOf(err))
	}
	saturated, err := Refill(500, 0, 30000, 10, 512, 64)
	if err != nil || !saturated.Saturated || saturated.Tokens != 512 {
		t.Fatalf("refill saturation failed: %#v %v", saturated, err)
	}
	tokens, rem := int64(0), int64(0)
	for i := 0; i < 100000; i++ {
		r, err := Refill(tokens, rem, 25000, 1, 1<<50, 64)
		if err != nil {
			t.Fatal(err)
		}
		tokens, rem = r.Tokens, r.Remainder
	}
	if tokens != 250000 || rem != 0 {
		t.Fatalf("long-run drift tokens=%d remainder=%d", tokens, rem)
	}
	report, err := CriticalCases()
	if err != nil {
		t.Fatal(err)
	}
	if !report.AllPass {
		t.Fatalf("critical cases failed: %#v", report.Cases)
	}
}

func TestLongRunDecisionsDoNotDependOnSchema(t *testing.T) {
	project := &Project{
		Name: "long-run", Version: "long-1", TickMS: 1,
		NominalPacketBytes: 128, EgressBudgetBytes: 512, Width: 64,
		WorkerThreads: 1, QueueDepth: 16, HorizonMS: 20000,
		Channels: []Channel{{
			ID: "c", Order: 0, RatePermyriad: 25000, CapacityBytes: 30080,
			InitialTokens: 0, Policy: PolicyDefer, Priority: 0,
			WeightPermyriad: 10000, QueueAgeMS: 1, MaxDelayMS: 20000,
		}},
		Packets: []Packet{{
			ID: "late", ChannelID: "c", ArrivalMS: 10000, SizeBytes: 256, Seq: 0,
		}},
	}
	var first *RunResult
	for version := 1; version <= 3; version++ {
		raw, err := EncodeProject(project, version)
		if err != nil {
			t.Fatalf("encode v%d: %v", version, err)
		}
		p, err := LoadProject(raw)
		if err != nil {
			t.Fatalf("load v%d: %v", version, err)
		}
		eng, err := NewEngine(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := eng.StepUntil(10000); err != nil {
			t.Fatal(err)
		}
		res, err := eng.Run(context.Background())
		if err != nil {
			t.Fatalf("v%d run: %v", version, err)
		}
		if first == nil {
			first = res
		} else if !reflect.DeepEqual(first.Emissions, res.Emissions) ||
			!reflect.DeepEqual(first.FinalTokens, res.FinalTokens) {
			t.Fatalf("v%d long-run decision differs", version)
		}
	}
	if len(first.Emissions) != 1 {
		t.Fatalf("expected one emission, got %d", len(first.Emissions))
	}
}
