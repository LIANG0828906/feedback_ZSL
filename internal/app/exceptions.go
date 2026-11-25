package app

import (
	"os"
	"path/filepath"

	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/checkpoint"
	"multiplexgw/internal/engine"
	"multiplexgw/internal/incremental"
	"multiplexgw/internal/model"
	"multiplexgw/internal/parallel"
	"multiplexgw/internal/sample"
	"multiplexgw/internal/schema"
)

func runExceptions(base *sample.Bundle, artifactDir string) []ExceptionResult {
	var out []ExceptionResult
	add := func(r ExceptionResult) { out = append(out, r) }

	// Negative initial window.
	p, _ := incremental.CloneProject(base.Canonical)
	p.Streams[0].InitialWindowBytes = -4096
	add(errCase("negative_window", bizerr.WindowNegative, schema.Validate(p)))

	// WINDOW_UPDATE over declared connection cap.
	p, _ = incremental.CloneProject(base.Canonical)
	p.Events = []model.Event{{Index: 0, FrameSeq: 1, TimeMS: 10, Type: model.EventWindowUpdate, StreamID: 0, IncrementBytes: 1 << 40}}
	_, err := engine.Run(p, engineOptions())
	add(errCase("window_update_over_cap", bizerr.WindowOverCap, err))

	// Weight out of range and zero weight.
	p, _ = incremental.CloneProject(base.Canonical)
	weight := int64(10001)
	p.Streams[1].WeightPermyriad = weight
	add(errCase("weight_out_of_range", bizerr.WeightRange, schema.Validate(p)))
	p, _ = incremental.CloneProject(base.Canonical)
	p.Streams[1].WeightPermyriad = 0
	add(errCase("zero_weight_report", bizerr.WeightZero, schema.Validate(p)))

	// Dependency cycle: 1 -> 7 -> 3 -> 1.
	p, _ = incremental.CloneProject(base.Canonical)
	for i := range p.Streams {
		if p.Streams[i].ID == 1 {
			p.Streams[i].ParentID = 7
		}
	}
	add(errCase("dependency_cycle", bizerr.DependencyCycle, schema.Validate(p)))

	// Frame larger than max frame size, strict API.
	large := minimalFaultProject(4097)
	_, err = engine.Run(large, engine.Options{MaxPendingFrames: 2})
	add(errCase("frame_too_large", bizerr.FrameTooLarge, err))

	// Fault-recording mode exposes the same code in the event attribution.
	rec, recErr := engine.Run(minimalFaultProject(4097), engine.Options{MaxPendingFrames: 2, RecordFaults: true})
	if recErr != nil || rec == nil || len(rec.Traces) == 0 || rec.Traces[len(rec.Traces)-1].Error == nil {
		add(ExceptionResult{Case: "frame_too_large_attribution", Expected: bizerr.FrameTooLarge, ActualCode: bizerr.CodeOf(recErr), Match: false, Detail: "missing recorded fault trace"})
	} else {
		code := rec.Traces[len(rec.Traces)-1].Error.Code
		add(ExceptionResult{Case: "frame_too_large_attribution", Expected: bizerr.FrameTooLarge, ActualCode: code, Match: code == bizerr.FrameTooLarge, Context: rec.Traces[len(rec.Traces)-1].Error.Context})
	}

	// Out-of-range worker/buffer budget.
	badBudget := parallel.Config{Workers: 999, MaxPendingFrames: 0}
	add(errCase("budget_out_of_range", bizerr.BudgetRange, badBudget.Validate()))

	// Deterministic buffer backpressure.
	_, err = engine.Run(bufferFullProject(), engine.Options{MaxPendingFrames: 1})
	add(errCase("buffer_full_backpressure", bizerr.BufferFull, err))

	// Deterministic thread budget exhaustion probe.
	code, ctx, err := parallel.DeterministicBackpressureProbe(1)
	r := errCase("thread_budget_exhausted", bizerr.ThreadBudgetExhausted, err)
	if err == nil {
		r.ActualCode = code
		r.Match = code == bizerr.ThreadBudgetExhausted
		r.Context = ctx
	}
	add(r)

	// Truncated SQLite checkpoint.
	add(truncatedCheckpointCase(artifactDir))

	// Version-mismatched checkpoint snapshot.
	add(versionMismatchCase(artifactDir))

	return out
}

// RunNamedInjection executes one stable exception case for the demo CLI.
func RunNamedInjection(name, artifactDir string) (ExceptionResult, error) {
	bundle, err := sample.Build(sample.DefaultSeed)
	if err != nil {
		return ExceptionResult{}, err
	}
	for _, r := range runExceptions(bundle, artifactDir) {
		if r.Case == name {
			return r, nil
		}
	}
	return ExceptionResult{Case: name, Expected: ""}, bizerr.New(bizerr.NotFound, "inject.case", "unknown injection case %q", name)
}

// InjectionCaseNames lists stable demo injection names.
func InjectionCaseNames() []string {
	return []string{
		"negative_window",
		"window_update_over_cap",
		"weight_out_of_range",
		"zero_weight_report",
		"dependency_cycle",
		"frame_too_large",
		"frame_too_large_attribution",
		"budget_out_of_range",
		"buffer_full_backpressure",
		"thread_budget_exhausted",
		"checkpoint_truncated",
		"checkpoint_version_mismatch",
	}
}

func errCase(name, expected string, err error) ExceptionResult {
	r := ExceptionResult{Case: name, Expected: expected}
	if err == nil {
		r.ActualCode = bizerr.OK
		r.Match = false
		r.Detail = "expected error but call succeeded"
		return r
	}
	r.ActualCode = bizerr.CodeOf(err)
	r.Match = r.ActualCode == expected
	if be, ok := bizerr.AsError(err); ok {
		r.Context = be.Context
		r.Detail = be.Message
	} else {
		r.Detail = err.Error()
	}
	return r
}

func minimalFaultProject(length int64) *model.Project {
	p := &model.Project{
		SchemaVersion: model.SchemaV1, ProjectID: "exc-frame", FixedPointBits: 16,
		Connection: model.ConnectionSpec{ID: "C-exc", InitialWindowBytes: 65536, MaxWindowBytes: 65536},
		Streams: []model.StreamSpec{{
			ID: 1, ParentID: 0, WeightPermyriad: 10000, InitialWindowBytes: 65536,
			MaxWindowBytes: 65536, MaxFrameBytes: 4096, BodySegments: []int64{length},
		}},
		Events: []model.Event{
			{Index: 0, FrameSeq: 1, TimeMS: 10, Type: model.EventHeaders, StreamID: 1},
			{Index: 1, FrameSeq: 2, TimeMS: 20, Type: model.EventData, StreamID: 1, LengthBytes: length, SegmentIndex: 0},
		},
	}
	return p
}

func bufferFullProject() *model.Project {
	return &model.Project{
		SchemaVersion: model.SchemaV1, ProjectID: "exc-buffer", FixedPointBits: 16,
		Connection: model.ConnectionSpec{ID: "C-buffer", InitialWindowBytes: 0, MaxWindowBytes: 65536},
		Streams: []model.StreamSpec{{
			ID: 1, ParentID: 0, WeightPermyriad: 10000, InitialWindowBytes: 1024,
			MaxWindowBytes: 16384, MaxFrameBytes: 1024, BodySegments: []int64{1024, 1024},
		}},
		Events: []model.Event{
			{Index: 0, FrameSeq: 1, TimeMS: 10, Type: model.EventHeaders, StreamID: 1},
			{Index: 1, FrameSeq: 2, TimeMS: 20, Type: model.EventData, StreamID: 1, LengthBytes: 1024, SegmentIndex: 0},
			{Index: 2, FrameSeq: 3, TimeMS: 30, Type: model.EventData, StreamID: 1, LengthBytes: 1024, SegmentIndex: 1},
		},
	}
}

func truncatedCheckpointCase(artifactDir string) ExceptionResult {
	validPath := filepath.Join(artifactDir, "exception-valid.db")
	truncPath := filepath.Join(artifactDir, "exception-truncated.db")
	store, err := checkpoint.Open(validPath)
	if err != nil {
		return errCase("checkpoint_truncated", bizerr.CheckpointCorrupt, err)
	}
	if err := store.WriteMeta(checkpoint.Meta{CheckpointVersion: checkpoint.CheckpointVersion, SchemaVersion: model.SchemaV1, ProjectID: "exc"}); err != nil {
		_ = store.Close()
		return errCase("checkpoint_truncated", bizerr.CheckpointCorrupt, err)
	}
	_ = store.Close()
	b, err := os.ReadFile(validPath)
	if err != nil {
		return errCase("checkpoint_truncated", bizerr.CheckpointCorrupt, err)
	}
	n := 128
	if len(b) < n {
		n = len(b) / 2
	}
	if err := os.WriteFile(truncPath, b[:n], 0o644); err != nil {
		return errCase("checkpoint_truncated", bizerr.CheckpointCorrupt, err)
	}
	_, err = checkpoint.Open(truncPath)
	return errCase("checkpoint_truncated", bizerr.CheckpointCorrupt, err)
}

func versionMismatchCase(artifactDir string) ExceptionResult {
	path := filepath.Join(artifactDir, "exception-version-mismatch.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		return errCase("checkpoint_version_mismatch", bizerr.CheckpointVersion, err)
	}
	defer store.Close()
	if err := store.WriteMeta(checkpoint.Meta{CheckpointVersion: 999, SchemaVersion: "frameplan/wrong", ProjectID: "exc"}); err != nil {
		return errCase("checkpoint_version_mismatch", bizerr.CheckpointVersion, err)
	}
	err = store.CheckVersion(checkpoint.ExpectedVersion{CheckpointVersion: checkpoint.CheckpointVersion, SchemaVersion: model.SchemaV1})
	return errCase("checkpoint_version_mismatch", bizerr.CheckpointVersion, err)
}
