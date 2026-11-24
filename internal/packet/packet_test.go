package packet_test

import (
	"bytes"
	"testing"

	"audioforge/internal/errs"
	"audioforge/internal/generator"
	"audioforge/internal/model"
	"audioforge/internal/packet"
	"audioforge/internal/synth"
)

func TestPacketSlicingIsByteStableAndRespectsLimits(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	stream, _ := bundle.SelectStream("arena-tick")
	synthesis, err := synth.Synthesize(stream.Events, bundle.Capability, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	first, err := packet.Build(synthesis.Frames, bundle.Capability)
	if err != nil {
		t.Fatal(err)
	}
	second, err := packet.Build(synthesis.Frames, bundle.Capability)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("no packets")
	}
	for i := range first {
		if first[i].ContentHash != second[i].ContentHash {
			t.Fatalf("hash differs at packet %d", i)
		}
		if int64(len(first[i].Samples)) > bundle.Capability.MaxPacketSamples {
			t.Fatalf("packet exceeds limit")
		}
		before, _ := packet.Encode(first[i])
		after, _ := packet.Encode(second[i])
		if !bytes.Equal(before, after) {
			t.Fatalf("wire bytes differ at packet %d", i)
		}
	}
}

func TestTimingWindowViolationReturnsPackError(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	stream, _ := bundle.SelectStream("arena-tick")
	synthesis, err := synth.Synthesize(stream.Events, bundle.Capability, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	capability := bundle.Capability
	capability.TimingWindow = model.TimingWindow{Min: 0, Max: 10}
	_, err = packet.Build(synthesis.Frames, capability)
	if !errs.IsCode(err, errs.EPack) {
		t.Fatalf("expected E_PACK, got %v", err)
	}
	coded := err.(*errs.Error)
	if coded.Field != "timing_mark" {
		t.Fatalf("unexpected field: %+v", coded)
	}
}

func TestOversizedPacketValidationReturnsPackError(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	packetValue := model.Packet{
		Seq:         7,
		Channel:     0,
		StartSample: 0,
		Samples:     make([]int64, int(bundle.Capability.MaxPacketSamples)+1),
		TimingMark:  0,
	}
	packetValue.RefreshHash()
	err := packet.Validate(packetValue, bundle.Capability)
	if !errs.IsCode(err, errs.EPack) {
		t.Fatalf("expected E_PACK, got %v", err)
	}
}
