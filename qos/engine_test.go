package qos

import (
	"context"
	"reflect"
	"testing"
)

func TestTokenBucketBudgetQueueAndReplayInvariants(t *testing.T) {
	p := Scenario(42)
	p.SchemaVersion = 1
	eng, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Delays) == 0 || len(res.Drops) == 0 || len(res.Expired) == 0 {
		t.Fatalf("sample must exercise delays, drops and expiry: %#v", res)
	}
	previousQueue := map[string]QueueEntry{}
	replayedSeen := false
	suppressedSeen := false
	for _, st := range res.Steps {
		var emittedBytes int64
		for _, em := range st.Emitted {
			emittedBytes += em.SizeBytes
		}
		if emittedBytes > p.EgressBudgetBytes {
			t.Fatalf("step %d emitted %d > budget %d", st.Index, emittedBytes, p.EgressBudgetBytes)
		}
		for _, tok := range st.Tokens {
			if tok.TokensBytes < 0 || tok.TokensBytes > tok.CapacityBytes {
				t.Fatalf("step %d channel %s token %d outside [0,%d]", st.Index, tok.ChannelID, tok.TokensBytes, tok.CapacityBytes)
			}
		}
		currentQueue := map[string]QueueEntry{}
		var prevExpiry int64 = -1
		for _, q := range st.Queue {
			if _, ok := currentQueue[q.PacketID]; ok {
				t.Fatalf("step %d duplicate queue member %s", st.Index, q.PacketID)
			}
			if q.ExpireAtMS < prevExpiry {
				t.Fatalf("step %d delay queue not sorted", st.Index)
			}
			prevExpiry = q.ExpireAtMS
			if q.RemainingMS < 0 {
				t.Fatalf("step %d negative remaining duration", st.Index)
			}
			currentQueue[q.PacketID] = q
		}
		for id := range previousQueue {
			_, stillQueued := currentQueue[id]
			if !stillQueued && !containsReplay(st.Replayed, id) && !containsDrop(st.Dropped, id) {
				t.Fatalf("step %d queued packet %s vanished", st.Index, id)
			}
		}
		previousQueue = currentQueue
		for _, r := range st.Replayed {
			if r.ReplaySeq <= 0 {
				t.Fatalf("step %d replay sequence not assigned for %s", st.Index, r.PacketID)
			}
			replayedSeen = true
		}
		if len(st.Suppressed) > 0 {
			suppressedSeen = true
		}
	}
	if !replayedSeen || !suppressedSeen {
		t.Fatalf("sample should have replay and suppression evidence")
	}
	reasonCodes := map[int]bool{}
	for _, d := range res.Drops {
		reasonCodes[d.ReasonCode] = true
	}
	if !reasonCodes[DropTokenShortage] || !reasonCodes[DropEgressBudget] || !reasonCodes[DropQueueExpired] {
		t.Fatalf("sample missing expected drop reasons: %v", reasonCodes)
	}
}

func containsReplay(list []ReplayEvent, id string) bool {
	for _, e := range list {
		if e.PacketID == id {
			return true
		}
	}
	return false
}

func containsDrop(list []DropEvent, id string) bool {
	for _, e := range list {
		if e.PacketID == id {
			return true
		}
	}
	return false
}

func TestPriorityAndBudgetSuppression(t *testing.T) {
	p := scheduleProject(2, 100)
	p.Channels[0].Priority = 9
	p.Channels[1].Priority = 1
	eng, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	st, err := eng.Step()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Emitted) != 1 || st.Emitted[0].ChannelID != "a" {
		t.Fatalf("priority channel was not selected first: %#v", st.Emitted)
	}
	foundSuppressed := false
	for _, s := range st.Suppressed {
		if s.ChannelID == "b" {
			foundSuppressed = true
		}
	}
	if !foundSuppressed {
		t.Fatalf("low-priority channel was not marked suppressed: %#v", st.Suppressed)
	}
}

func TestWeightedOrderingAndBasisIDs(t *testing.T) {
	p := scheduleProject(8, 100)
	p.Channels[0].WeightPermyriad = 6000
	p.Channels[1].WeightPermyriad = 3000
	eng, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	limit := len(res.Emissions)
	if limit > 10 {
		limit = 10
	}
	for _, em := range res.Emissions[:limit] {
		if em.WeightBasisID == "" {
			t.Fatalf("emission lacks weight basis id: %#v", em)
		}
		counts[em.ChannelID]++
	}
	if counts["a"] <= counts["b"] {
		t.Fatalf("higher weight channel did not receive more turns: %v", counts)
	}
	for _, st := range res.Steps {
		var total int64
		for _, em := range st.Emitted {
			total += em.SizeBytes
		}
		if total > p.EgressBudgetBytes {
			t.Fatalf("budget violated: %d", total)
		}
	}
}

func scheduleProject(packetsPerChannel, budget int64) *Project {
	p := &Project{
		Name: "sched", Version: "sched-1", TickMS: 10,
		NominalPacketBytes: 100, EgressBudgetBytes: budget, Width: 64,
		WorkerThreads: 1, QueueDepth: 128, HorizonMS: 5000,
		Channels: []Channel{
			{ID: "a", Order: 0, RatePermyriad: 10000, CapacityBytes: 2000, InitialTokens: 2000,
				Policy: PolicyDefer, Priority: 1, WeightPermyriad: 5000, QueueAgeMS: 10, MaxDelayMS: 1000},
			{ID: "b", Order: 1, RatePermyriad: 10000, CapacityBytes: 2000, InitialTokens: 2000,
				Policy: PolicyDefer, Priority: 1, WeightPermyriad: 5000, QueueAgeMS: 10, MaxDelayMS: 1000},
		},
	}
	seq := 0
	for i := int64(0); i < packetsPerChannel; i++ {
		for _, ch := range []string{"a", "b"} {
			p.Packets = append(p.Packets, Packet{
				ID:        ch + string(rune('0'+i)),
				ChannelID: ch, ArrivalMS: 0, SizeBytes: 100, Seq: seq,
			})
			seq++
		}
	}
	return p
}

func TestEmittedSequenceIsDeterministic(t *testing.T) {
	p := Scenario(99)
	var seqs [][]string
	for i := 0; i < 3; i++ {
		eng, err := NewEngine(p)
		if err != nil {
			t.Fatal(err)
		}
		res, err := eng.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(res.Emissions))
		for _, em := range res.Emissions {
			ids = append(ids, em.PacketID)
		}
		seqs = append(seqs, ids)
	}
	for i := 1; i < len(seqs); i++ {
		if !reflect.DeepEqual(seqs[0], seqs[i]) {
			t.Fatalf("non-deterministic emission order")
		}
	}
}
