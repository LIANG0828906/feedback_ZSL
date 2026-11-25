package checkpoint_test

import (
	"path/filepath"
	"testing"

	"prosodyalign/internal/checkpoint"
)

func TestFragmentCheckpointRecoveryBoundary(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for i := 0; i < 3; i++ {
		err := store.CommitFragment(checkpoint.FragmentCheckpoint{
			SnapshotID: "snap", LogicalSeq: int64(i), Epoch: 0, Ordinal: i,
			FragmentID: "f" + string(rune('a'+i)), DialogID: "d" + string(rune('a'+i)),
			EnvelopeDigest: "env", AccentSample: 42, StretchJSON: "{}",
			ContentHash: "hash", ParameterHash: "params", AlgorithmVersion: "algo", FrameCount: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	b, err := store.RecoveryBoundary("snap", map[string]string{"fa": "hash", "fb": "hash", "fc": "hash"}, "algo")
	if err != nil {
		t.Fatal(err)
	}
	if !b.SnapshotValid || b.LastLogicalSeq != 2 || b.ResumeLogicalSeq != 3 || len(b.CompletedCommitKeys) != 3 {
		t.Fatalf("bad boundary: %+v", b)
	}

	bad, err := store.RecoveryBoundary("snap", map[string]string{"fa": "hash", "fb": "changed", "fc": "hash"}, "algo")
	if err != nil {
		t.Fatal(err)
	}
	if bad.SnapshotValid || bad.LastLogicalSeq != 0 || bad.Mismatch == "" {
		t.Fatalf("expected mismatch boundary after fa prefix: %+v", bad)
	}
	if err := store.DeleteFromSequence("snap", bad.ResumeLogicalSeq); err != nil {
		t.Fatal(err)
	}
	rewound, err := store.RecoveryBoundary("snap", map[string]string{"fa": "hash"}, "algo")
	if err != nil {
		t.Fatal(err)
	}
	if !rewound.SnapshotValid || rewound.LastLogicalSeq != 0 {
		t.Fatalf("expected only complete fa prefix after rewind: %+v", rewound)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entry := checkpoint.CacheEntry{Key: "k", DialogID: "d", PhonemeIndex: 2, Kind: "phoneme_envelope", PayloadJSON: "[]", ContentHash: "h", ParameterHash: "p", AlgorithmVersion: "a"}
	if err := store.PutCache(entry); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.GetCache("k")
	if err != nil || !ok || got.PayloadJSON != "[]" {
		t.Fatalf("cache get ok=%v err=%v got=%+v", ok, err, got)
	}
	latest, ok, err := store.FindLatestCache("d", "phoneme_envelope", 2)
	if err != nil || !ok || latest.Key != "k" {
		t.Fatalf("latest cache ok=%v err=%v", ok, err)
	}
}
