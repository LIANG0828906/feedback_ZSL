package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"featurekernel/internal/checkpoint"
	"featurekernel/internal/engine"
	"featurekernel/internal/gen"
	"featurekernel/internal/incremental"
	"featurekernel/internal/kernel"
	"featurekernel/internal/schema"
)

type diagnosticCase struct {
	Name         string         `json:"name"`
	ExpectedCode string         `json:"expected_code"`
	ObservedCode string         `json:"observed_code"`
	Stable       bool           `json:"stable"`
	Context      map[string]any `json:"context,omitempty"`
}

func runDiagnose(args []string) error {
	flags := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	_ = flags.Bool("json", true, "emit JSON report")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cases := []diagnosticCase{}
	add := func(name, expected string, err error) {
		observed := kernel.CodeOf(err)
		item := diagnosticCase{Name: name, ExpectedCode: expected, ObservedCode: observed, Stable: observed == expected}
		if ke, ok := kernel.AsError(err); ok {
			item.Context = ke.Context
		}
		cases = append(cases, item)
	}

	project, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		return err
	}

	cache, _, err := incremental.NewCache(project)
	if err != nil {
		return err
	}
	_, err = cache.Apply(incremental.Edit{ChangeWidth: map[string]int64{"latency_ms": -1}})
	add("negative_width", kernel.CodeDefinitionNegativeWidth, err)

	zeroStepProject, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		return err
	}
	zeroStepProject.Features[0].SlideMs = 0
	_, err = engine.New(zeroStepProject)
	add("zero_slide", kernel.CodeDefinitionZeroStep, err)

	weightProject, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		return err
	}
	weightProject.Features[0].ChannelWeightsPPM["web"] = 1_000_001
	_, err = engine.New(weightProject)
	add("weight_out_of_range", kernel.CodeAggWeightOutOfRange, err)

	wmProject, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		return err
	}
	runtime, err := engine.NewRuntime(wmProject, wmProject.Features[0], "")
	if err != nil {
		return err
	}
	previousWM := wmProject.TimeBaseMs + 13*60_000
	runtime.SetWatermark(&previousWM)
	runtime.HydrateState(engine.WindowState{
		Coord:     kernel.WindowAt(wmProject.Features[0].AnchorMs, wmProject.Features[0].WidthMs, wmProject.Features[0].SlideMs, 0),
		Submitted: true,
	})
	err = runtime.ApplyRecord(kernel.Record{
		Seq: 999, Kind: kernel.RecordWatermark,
		Watermark: &kernel.Watermark{ArrivalSeq: 999, TimeMs: wmProject.TimeBaseMs + 60_000},
	})
	add("watermark_before_committed_window", kernel.CodeWatermarkBeforeCommitted, err)

	add("worker_budget_out_of_range", kernel.CodeBudgetWorkersOutOfRange,
		kernel.ValidateBudget(kernel.Budget{Workers: 0, MaxWindowsPerFeature: 32}))
	add("buffer_budget_out_of_range", kernel.CodeBudgetBufferOutOfRange,
		kernel.ValidateBudget(kernel.Budget{Workers: 2, MaxWindowsPerFeature: 0}))

	bufferProject, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		return err
	}
	bufferProject.Budget.MaxWindowsPerFeature = 1
	_, err = engine.ProcessFeature(bufferProject, bufferProject.Features[3], "")
	add("buffer_backpressure", kernel.CodeBackpressureBuffer, err)

	pool, err := engine.NewWorkerPool(1)
	if err != nil {
		return err
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	if err := pool.TrySubmit(func() { close(entered); <-release }); err != nil {
		return err
	}
	<-entered
	add("thread_backpressure", kernel.CodeBackpressureThread, pool.TrySubmit(func() {}))
	close(release)

	v1, err := schema.Encode(project, "v1")
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(v1, &raw); err != nil {
		return err
	}
	raw["future_unknown_combination"] = true
	unknown, _ := json.Marshal(raw)
	_, err = schema.Load(unknown)
	add("unknown_schema_field", kernel.CodeSchemaUnknownField, err)

	dir, err := os.MkdirTemp("", "featurectl-diagnose-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	snapshot := &checkpoint.Snapshot{
		CheckpointVersion: checkpoint.CheckpointVersionV1,
		CanonicalVersion:  kernel.CanonicalVersion,
		Cursor:            7,
	}
	truncatedPath := filepath.Join(dir, "truncated.json")
	if err := checkpoint.WriteSnapshotFile(truncatedPath, snapshot); err != nil {
		return err
	}
	info, err := os.Stat(truncatedPath)
	if err != nil {
		return err
	}
	if err := os.Truncate(truncatedPath, info.Size()/2); err != nil {
		return err
	}
	_, err = checkpoint.ReadSnapshotFile(truncatedPath)
	add("truncated_checkpoint", kernel.CodeCheckpointTruncated, err)

	bad := *snapshot
	bad.CanonicalVersion = "canonical.v9"
	mismatchPath := filepath.Join(dir, "mismatch.json")
	if err := checkpoint.WriteSnapshotFile(mismatchPath, &bad); err != nil {
		return err
	}
	_, err = checkpoint.ReadSnapshotFile(mismatchPath)
	add("version_mismatch_snapshot", kernel.CodeCheckpointVersionMismatch, err)

	allStable := true
	for _, item := range cases {
		if !item.Stable {
			allStable = false
		}
	}
	report := map[string]any{
		"all_stable": allStable,
		"cases":      cases,
		"writes":     "in-memory projects and temporary checkpoint files only; source samples are not modified",
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	if !allStable {
		return fmt.Errorf("diagnostic code mismatch")
	}
	return nil
}
