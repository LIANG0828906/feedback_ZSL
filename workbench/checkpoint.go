package workbench

import (
	"encoding/json"
	"os"
	"path/filepath"

	"schedlab/internal/domain"
	"schedlab/internal/gen"
	"schedlab/internal/runtime"
)

// CheckpointReport runs a scored run, simulates a forced termination by
// reopening the store, and confirms the committed frames survive and are
// reused by the next run.
func CheckpointReport(b *gen.Bundle, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	store, err := runtime.OpenStore(dir)
	if err != nil {
		return err
	}
	_ = store.Reset()
	p := b.Truth.(*domain.Project)

	runner, err := runtime.NewRunner(p, store, runtime.Config{
		Concurrency: 4, BatchBuffer: 4096, RuleVersion: "r1",
	})
	if err != nil {
		return err
	}
	first, err := runner.Run()
	if err != nil {
		return err
	}

	// Simulate forced termination: discard the runner and reopen the store from
	// disk in a brand-new process-equivalent object.
	store2, err := runtime.OpenStore(dir)
	if err != nil {
		return err
	}
	ptr, err := store2.LoadCommit()
	if err != nil {
		return err
	}
	snap, err := store2.LoadSnapshot(ptr.SnapshotSeq)
	if err != nil {
		return err
	}
	frames, err := store2.LoadCache(ptr.SnapshotSeq)
	if err != nil {
		return err
	}

	runner2, err := runtime.NewRunner(p, store2, runtime.Config{
		Concurrency: 4, BatchBuffer: 4096, RuleVersion: "r1",
	})
	if err != nil {
		return err
	}
	second, err := runner2.Run()
	if err != nil {
		return err
	}

	report := map[string]any{
		"first_run": map[string]any{
			"snapshot_seq": first.SnapshotSeq,
			"outcomes":     len(first.Outcomes),
			"backpressure": first.BackpressureEvents,
		},
		"forced_termination_recovery": map[string]any{
			"commit_rule_version": ptr.RuleVersion,
			"snapshot_hash":       snap.NodeHash,
			"recovered_frames":    len(frames),
			"reused_on_rerun":     second.ReusedFromCheckpoint,
			"new_snapshot_seq":    second.SnapshotSeq,
		},
		"recovery_sound": len(frames) > 0 && second.ReusedFromCheckpoint == len(frames),
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(filepath.Dir(dir), "checkpoint.json"), data, 0o644)
}
