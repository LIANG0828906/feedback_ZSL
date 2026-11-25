package qos

import (
	"context"
	"reflect"
	"testing"
)

func TestIncrementalRateEditReusesPrefixAndReportsClosure(t *testing.T) {
	p := Scenario(42)
	p.SchemaVersion = 1
	inc, err := NewIncremental(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	prefix := append([]StepResult(nil), inc.Trace.Steps[:4]...)
	rate := int64(90000)
	trace, report, err := inc.Apply(context.Background(), Edit{
		SourceID: "rate-edit", AtMS: 40, ChannelID: "gold", RatePermyriad: &rate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.StartStep != 4 || report.StartMS != 40 || report.ReusedSteps != 4 {
		t.Fatalf("unexpected invalidation range: %#v", report)
	}
	if report.CachedUntilStep != 3 || report.RecomputedSteps == 0 {
		t.Fatalf("cache range not reported: %#v", report)
	}
	if !reflect.DeepEqual(report.RootChannels, []string{"gold"}) {
		t.Fatalf("root channels=%v", report.RootChannels)
	}
	if !reflect.DeepEqual(report.ClosureChannels, []string{"gold", "silver", "bronze"}) {
		t.Fatalf("shared scheduler closure must include all channels: %v", report.ClosureChannels)
	}
	if len(report.ClosureEdges) == 0 {
		t.Fatal("closure edges are not explicit")
	}
	for i := range prefix {
		if trace.Steps[i].CacheToken != prefix[i].CacheToken || !trace.Steps[i].Reused {
			t.Fatalf("prefix step %d was not reused", i)
		}
		if TruthStepDigest(trace.Steps[i]) != TruthStepDigest(prefix[i]) {
			t.Fatalf("reused prefix step %d changed", i)
		}
	}
	for i := 4; i < len(trace.Steps); i++ {
		if trace.Steps[i].SourceID != "rate-edit" || trace.Steps[i].Reused {
			t.Fatalf("recomputed step %d provenance=%q reused=%v", i, trace.Steps[i].SourceID, trace.Steps[i].Reused)
		}
	}
	if inc.Trace.Project.Channels[0].RatePermyriad != 90000 {
		t.Fatalf("edited project not installed")
	}
	if p.Channels[0].RatePermyriad != 60000 {
		t.Fatalf("source project was mutated")
	}
}

func TestIncrementalAddRemoveAndWeightCapacityEdits(t *testing.T) {
	p := Scenario(42)
	p.SchemaVersion = 1
	inc, err := NewIncremental(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	added := Packet{
		ID: "injected", ChannelID: "gold", ArrivalMS: 80, SizeBytes: 256,
		Priority: 2, Seq: -1,
	}
	trace, report, err := inc.Apply(context.Background(), Edit{
		SourceID: "add-packet", AddPackets: []Packet{added},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.StartStep != 8 || report.ReusedSteps != 8 {
		t.Fatalf("add packet invalidation range: %#v", report)
	}
	if ch, _ := trace.Project.Channel("gold"); ch == nil {
		t.Fatal("channel missing")
	}
	found := false
	for _, pkt := range trace.Project.Packets {
		if pkt.ID == "injected" {
			found = true
			if pkt.Seq < 0 {
				t.Fatal("sequence was not assigned")
			}
		}
	}
	if !found {
		t.Fatal("added packet absent")
	}

	trace, report, err = inc.Apply(context.Background(), Edit{
		SourceID: "remove-packet", RemovePacketIDs: []string{"injected"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.StartStep != 8 || report.ReusedSteps != 8 {
		t.Fatalf("remove packet invalidation range: %#v", report)
	}
	for _, pkt := range trace.Project.Packets {
		if pkt.ID == "injected" {
			t.Fatal("removed packet still present")
		}
	}

	weight := int64(9000)
	_, report, err = inc.Apply(context.Background(), Edit{
		SourceID: "weight-edit", AtMS: 20, ChannelID: "gold", WeightPermyriad: &weight,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.StartStep != 2 || report.Cause == "" {
		t.Fatalf("weight edit report: %#v", report)
	}
	capacity := int64(1152)
	_, report, err = inc.Apply(context.Background(), Edit{
		SourceID: "capacity-edit", AtMS: 40, ChannelID: "gold", CapacityBytes: &capacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.StartStep != 4 || inc.Trace.Project.Channels[0].CapacityBytes != 1152 {
		t.Fatalf("capacity edit report: %#v", report)
	}
}

func TestIncrementalErrorLeavesCacheUnchanged(t *testing.T) {
	p := Scenario(42)
	p.SchemaVersion = 1
	inc, err := NewIncremental(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	before := inc.Trace
	bad := int64(1)
	_, _, err = inc.Apply(context.Background(), Edit{
		SourceID: "bad-capacity", AtMS: 20, ChannelID: "gold", CapacityBytes: &bad,
	})
	if CodeOf(err) != EWatermarkRange && CodeOf(err) != EBucketTooSmall && CodeOf(err) != ECapacityNegative {
		t.Fatalf("unexpected edit error: %v", err)
	}
	if inc.Trace != before {
		t.Fatal("failed edit replaced the cached trace")
	}
	if p.Channels[0].CapacityBytes != 1024 {
		t.Fatal("failed edit mutated source project")
	}
}
