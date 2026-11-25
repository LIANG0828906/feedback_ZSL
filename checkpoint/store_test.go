package checkpoint_test

import (
	"path/filepath"
	"testing"

	"lipsync/checkpoint"
	"lipsync/domain"
)

func TestWindowCommitIsIdempotentAndAtomic(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	commit := checkpoint.WindowCommit{
		Index: 0, StartFrame: 0, EndFrame: 0, State: string(domain.WindowCommitted),
		ContentHash: "hash", ParamsVersion: "params", AlgorithmVersion: "algo",
		Frames: []domain.FrameResult{{
			Frame: 0,
			Channels: []domain.FrameChannelResult{{
				ChannelID: "spk_main", Accepted: []string{"pkt"}, Delayed: []string{},
				Preempted: []string{}, Resumed: []string{}, OccupiedSlot: 1, UsedBudget: 100,
			}},
		}},
		Preemptions: []domain.PreemptEvent{{
			Frame: 0, ChannelID: "spk_main", PacketID: "old", ByPacketID: "pkt",
			DeliveredFrames: 2, RemainingFrames: 1, RecoveryFrame: 2,
		}},
		Resumes:      []domain.ResumeEvent{{Frame: 2, ChannelID: "spk_main", PacketID: "old", ProtectedUntil: 3}},
		Abandonments: []domain.AbandonEvent{{Frame: 4, ChannelID: "spk_main", PacketID: "gone", Reason: "window_exhausted"}},
		PacketStates: []domain.PacketState{{ID: "pkt", ChannelID: "spk_main", Status: "done", Delivered: 1}},
		Consumed:     map[string]int64{"dialogue": 3},
	}
	first, err := store.SaveWindow(commit)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SaveWindow(commit)
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 || second != first {
		t.Fatalf("commit sequence is not monotonic/idempotent: %d %d", first, second)
	}
	saved, err := store.Load("hash", "algo")
	if err != nil {
		t.Fatal(err)
	}
	if saved.LastWindow != 0 || saved.NextFrame != 1 || len(saved.CommitSequence) != 1 {
		t.Fatalf("unexpected checkpoint summary: %+v", saved)
	}
	if len(saved.Preemptions) != 1 || len(saved.Resumes) != 1 || len(saved.Abandonments) != 1 {
		t.Fatalf("event replay lost rows: %+v", saved)
	}
	if saved.PacketStates[0].Remaining != 0 || saved.Consumed["dialogue"] != 3 {
		t.Fatalf("packet/consumed state not persisted: %+v", saved)
	}
}

func TestUncommittedWindowIsRejected(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.SaveWindow(checkpoint.WindowCommit{
		Index: 0, StartFrame: 0, EndFrame: 0, State: string(domain.WindowArbitrating),
	})
	if domain.CodeOf(err) != domain.CodeState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
}
