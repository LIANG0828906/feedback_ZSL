// Package pipeline wires the domain, engine and SQLite store into runnable
// local acceptance scenarios.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"roomacoustics/pkg/acoustics"
	"roomacoustics/pkg/checkpoint"
	"roomacoustics/pkg/engine"
	"roomacoustics/pkg/generator"
)

type PathSummary struct {
	Name             string                        `json:"name"`
	OK               bool                          `json:"ok"`
	GlobalHash       string                        `json:"global_hash"`
	CommitSequence   []string                      `json:"commit_sequence"`
	StoreCommitOrder []string                      `json:"store_commit_order,omitempty"`
	Progress         acoustics.Progress            `json:"progress"`
	ReusedRooms      []string                      `json:"reused_rooms,omitempty"`
	RecomputedRooms  []string                      `json:"recomputed_rooms,omitempty"`
	QueueEventCount  int                           `json:"queue_event_count"`
	Invalidation     *acoustics.InvalidationReport `json:"invalidation,omitempty"`
}

type AcceptanceReport struct {
	OK                     bool                          `json:"ok"`
	Seed                   int64                         `json:"seed"`
	SampleDir              string                        `json:"sample_dir"`
	LevelPath              string                        `json:"level_path"`
	TruthPath              string                        `json:"truth_path"`
	TruthVerification      *acoustics.VerificationReport `json:"truth_verification"`
	AbsorptionInvalidation *acoustics.InvalidationReport `json:"absorption_invalidation"`
	FinalInvalidation      *acoustics.InvalidationReport `json:"final_invalidation"`
	Preview                PathSummary                   `json:"preview"`
	Recovery               PathSummary                   `json:"recovery"`
	Full                   PathSummary                   `json:"full"`
	Diff                   *acoustics.DiffMismatch       `json:"diff,omitempty"`
	Message                string                        `json:"message,omitempty"`
}

func SampleDir(root string) string { return filepath.Join(root, "samples") }

func GenerateSample(root string) (*generator.Spec, string, string, error) {
	dir := SampleDir(root)
	spec, err := generator.GenerateFiles(dir, generator.DefaultSeed)
	if err != nil {
		return nil, "", "", err
	}
	return spec, filepath.Join(dir, "level.json"), filepath.Join(dir, "truth.json"), nil
}

func RunAcceptance(ctx context.Context, root string) (*AcceptanceReport, error) {
	dir := SampleDir(root)
	_, err := generator.GenerateFiles(dir, generator.DefaultSeed)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"acceptance-preview.db", "acceptance-recovery.db", "acceptance-full.db"} {
		removeDBFiles(filepath.Join(dir, name))
	}
	levelPath := filepath.Join(dir, "level.json")
	truthPath := filepath.Join(dir, "truth.json")
	level, truth, err := acoustics.LoadProjectFiles(levelPath, truthPath)
	if err != nil {
		return nil, err
	}
	out := &AcceptanceReport{OK: false, Seed: level.Seed, SampleDir: dir, LevelPath: levelPath, TruthPath: truthPath}

	baseStore, err := checkpoint.Open(filepath.Join(dir, "acceptance-preview.db"))
	if err != nil {
		return nil, err
	}
	baseEng, err := engine.New(level, engine.Config{Store: baseStore, MaxWorkers: 2, BufferPerWorker: 4})
	if err != nil {
		_ = baseStore.Close()
		return nil, err
	}
	baseReport, err := baseEng.Run(ctx)
	if err != nil {
		baseEng.Close()
		_ = baseStore.Close()
		return nil, err
	}
	truthVR, err := acoustics.VerifyTruth(level, baseReport, truth)
	out.TruthVerification = truthVR
	if err != nil {
		out.Message = err.Error()
		baseEng.Close()
		_ = baseStore.Close()
		return out, nil
	}

	alpha := 0.42
	edited1, err := acoustics.ApplyEdit(level, acoustics.Edit{Type: "set_absorption", ObjectID: "RF-R1-B", Absorption: &alpha})
	if err != nil {
		return nil, err
	}
	absPlan := acoustics.PlanInvalidation(level, edited1, baseReport)
	out.AbsorptionInvalidation = absPlan

	edited2, err := acoustics.ApplyEdit(edited1, acoustics.Edit{Type: "drag_room_boundary", RoomID: "R3", Side: "right", Coordinate: 34})
	if err != nil {
		return nil, err
	}

	// Persistent preview path: reload the prior report from SQLite before each
	// edit window, then apply dependency-aware reuse.
	persistedBase, err := LoadReportFromStore(ctx, baseStore)
	if err != nil {
		return nil, err
	}
	plan1 := acoustics.PlanInvalidation(level, edited1, persistedBase)
	eng1, err := engine.New(edited1, engine.Config{Store: baseStore, ReuseReport: persistedBase, Invalidation: plan1, MaxWorkers: 2, BufferPerWorker: 4})
	if err != nil {
		return nil, err
	}
	if _, err := eng1.Run(ctx); err != nil {
		return nil, err
	}
	persisted1, err := LoadReportFromStore(ctx, baseStore)
	if err != nil {
		return nil, err
	}
	finalPlan := acoustics.PlanInvalidation(edited1, edited2, persisted1)
	out.FinalInvalidation = acoustics.PlanInvalidation(level, edited2, persistedBase)
	eng2, err := engine.New(edited2, engine.Config{Store: baseStore, ReuseReport: persisted1, Invalidation: finalPlan, MaxWorkers: 2, BufferPerWorker: 4})
	if err != nil {
		return nil, err
	}
	previewReport, err := eng2.Run(ctx)
	if err != nil {
		return nil, err
	}
	previewOrder, err := baseStore.CommitOrder(ctx, previewReport.GlobalHash)
	if err != nil {
		return nil, err
	}
	out.Preview = summary("incremental-preview", previewReport, previewOrder, finalPlan)
	eng2.Close()
	eng1.Close()
	baseEng.Close()
	_ = baseStore.Close()

	// Recovery path: same final edited level, deterministic interruption after
	// two durable room commits, then reopen and resume from SQLite.
	recoveryDB := filepath.Join(dir, "acceptance-recovery.db")
	recoveryStore1, err := checkpoint.Open(recoveryDB)
	if err != nil {
		return nil, err
	}
	recoveryEng1, err := engine.New(edited2, engine.Config{Store: recoveryStore1, MaxWorkers: 2, BufferPerWorker: 4, StopAfterCommits: 2})
	if err != nil {
		return nil, err
	}
	_, runErr := recoveryEng1.Run(ctx)
	if !errors.Is(runErr, engine.ErrInterrupted) {
		return nil, errors.New("expected deterministic interruption after two commits")
	}
	recoveryEng1.Close()
	_ = recoveryStore1.Close()

	recoveryStore2, err := checkpoint.Open(recoveryDB)
	if err != nil {
		return nil, err
	}
	recoveryEng2, err := engine.New(edited2, engine.Config{Store: recoveryStore2, MaxWorkers: 2, BufferPerWorker: 4})
	if err != nil {
		return nil, err
	}
	recoveryReport, err := recoveryEng2.Run(ctx)
	if err != nil {
		return nil, err
	}
	recoveryOrder, err := recoveryStore2.CommitOrder(ctx, recoveryReport.GlobalHash)
	if err != nil {
		return nil, err
	}
	out.Recovery = summary("checkpoint-recovery", recoveryReport, recoveryOrder, nil)
	recoveryEng2.Close()
	_ = recoveryStore2.Close()

	// One-shot full recomputation.
	fullStore, err := checkpoint.Open(filepath.Join(dir, "acceptance-full.db"))
	if err != nil {
		return nil, err
	}
	fullEng, err := engine.New(edited2, engine.Config{Store: fullStore, MaxWorkers: 2, BufferPerWorker: 4})
	if err != nil {
		return nil, err
	}
	fullReport, err := fullEng.Run(ctx)
	if err != nil {
		return nil, err
	}
	fullOrder, err := fullStore.CommitOrder(ctx, fullReport.GlobalHash)
	if err != nil {
		return nil, err
	}
	out.Full = summary("one-shot-full", fullReport, fullOrder, nil)
	fullEng.Close()
	_ = fullStore.Close()

	if err := acoustics.CompareReports(fullReport, previewReport); err != nil {
		return finishDiff(out, err)
	}
	if err := acoustics.CompareReports(fullReport, recoveryReport); err != nil {
		return finishDiff(out, err)
	}
	if stringsJoin(out.Preview.StoreCommitOrder) != stringsJoin(out.Full.StoreCommitOrder) ||
		stringsJoin(out.Recovery.StoreCommitOrder) != stringsJoin(out.Full.StoreCommitOrder) {
		d := acoustics.DiffMismatch{Field: "store_commit_order", Expected: out.Full.StoreCommitOrder, Actual: []string{stringsJoin(out.Preview.StoreCommitOrder), stringsJoin(out.Recovery.StoreCommitOrder)}}
		out.Diff = &d
		out.Message = acoustics.CodeDiff + ": stable SQLite commit order mismatch"
		return out, nil
	}

	out.OK = true
	out.Message = "truth, incremental preview, recovery and full recomputation agree"
	return out, nil
}

func removeDBFiles(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			// Stale WAL/SHM removal is best-effort; opening the database still
			// succeeds and SQLite recovers its own side files.
			_ = err
		}
	}
}

func LoadReportFromStore(ctx context.Context, store *checkpoint.Store) (*acoustics.Report, error) {
	_, order, cps, err := store.LoadWindow(ctx)
	if err != nil {
		return nil, err
	}
	report := &acoustics.Report{Rooms: []acoustics.RoomResult{}}
	for _, id := range order {
		cp, ok := cps[id]
		if !ok {
			continue
		}
		var rr acoustics.RoomResult
		if err := json.Unmarshal(cp.ResultJSON, &rr); err != nil {
			return nil, err
		}
		report.Rooms = append(report.Rooms, rr)
		report.Pairs = append(report.Pairs, rr.PairResults...)
		if report.Params.ParamVersion == "" {
			report.Params = acoustics.DefaultParams()
		}
	}
	return report, nil
}

func summary(name string, r *acoustics.Report, order []string, inv *acoustics.InvalidationReport) PathSummary {
	reused := []string{}
	recomputed := []string{}
	for _, room := range r.Rooms {
		if room.Reused {
			reused = append(reused, room.RoomID)
		} else if room.Committed {
			recomputed = append(recomputed, room.RoomID)
		}
	}
	return PathSummary{
		Name: name, OK: r.Progress.Failed == 0 && r.Progress.Committed == r.Progress.Total,
		GlobalHash: r.GlobalHash, CommitSequence: r.CommitSequence, StoreCommitOrder: order,
		Progress: r.Progress, ReusedRooms: reused, RecomputedRooms: recomputed,
		QueueEventCount: len(r.QueueEvents), Invalidation: inv,
	}
}

func finishDiff(out *AcceptanceReport, err error) (*AcceptanceReport, error) {
	if de, ok := acoustics.AsDomainError(err); ok {
		if d, ok := de.Details.(acoustics.DiffMismatch); ok {
			out.Diff = &d
		}
		out.Message = de.Error()
		return out, nil
	}
	out.Message = err.Error()
	return out, nil
}

func stringsJoin(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += "|"
		}
		out += x
	}
	return out
}
