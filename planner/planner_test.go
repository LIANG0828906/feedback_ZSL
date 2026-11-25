package planner_test

import (
	"testing"

	"github.com/liang/collector-backpressure/planner"
	"github.com/liang/collector-backpressure/sample"
)

func TestChunkSplitsMatchGeneratorTruth(t *testing.T) {
	bundle, err := sample.GenerateBundle(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := planner.BuildChunks(bundle.Project)
	if err != nil {
		t.Fatal(err)
	}

	agentLimits := map[string]struct {
		bytes int64
		count int
	}{}
	for _, agent := range bundle.Project.Agents {
		agentLimits[agent.ID] = struct {
			bytes int64
			count int
		}{agent.MaxBatchBytes, agent.MaxBatchMessages}
	}

	type aggregate struct {
		partition string
		chunks    int
		messages  int
		bytes     int64
	}
	agg := map[string]*aggregate{}
	for _, chunk := range chunks {
		a := agg[chunk.BatchID]
		if a == nil {
			a = &aggregate{partition: chunk.PartitionID}
			agg[chunk.BatchID] = a
		}
		a.chunks++
		a.messages += chunk.Count
		a.bytes += chunk.Bytes
		if chunk.Count > agentLimits[chunk.AgentID].count {
			t.Fatalf("chunk %s exceeds message limit", chunk.ChunkID)
		}
		if chunk.Bytes > agentLimits[chunk.AgentID].bytes {
			t.Fatalf("chunk %s exceeds byte limit", chunk.ChunkID)
		}
		if chunk.SplitIndex < 1 || chunk.SplitIndex > chunk.SplitCount {
			t.Fatalf("bad split ordinal for %s", chunk.ChunkID)
		}
	}

	for _, truth := range bundle.Truth {
		got := agg[truth.BatchID]
		if got == nil {
			t.Fatalf("batch %s produced no chunks", truth.BatchID)
		}
		if got.partition != truth.PartitionID || got.chunks != truth.ChunkCount ||
			got.messages != truth.MessageCount || got.bytes != truth.Bytes {
			t.Fatalf("truth mismatch %s: got %+v want %+v", truth.BatchID, got, truth)
		}
	}

	var first, second int
	for _, chunk := range chunks {
		if chunk.BatchID == "B-001" {
			switch chunk.SplitIndex {
			case 1:
				first = chunk.Count
				if chunk.Bytes != 120 {
					t.Fatalf("first split bytes = %d, want 120", chunk.Bytes)
				}
			case 2:
				second = chunk.Count
				if chunk.Bytes != 40 {
					t.Fatalf("second split bytes = %d, want 40", chunk.Bytes)
				}
			}
		}
	}
	if first != 3 || second != 1 {
		t.Fatalf("B-001 split counts = %d,%d want 3,1", first, second)
	}
}
