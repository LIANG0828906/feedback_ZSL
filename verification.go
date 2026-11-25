package hckernel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type TruthFile struct {
	Seed             int64                    `json:"seed"`
	Fingerprint      string                   `json:"fingerprint"`
	InstanceIDs      []string                 `json:"instance_ids"`
	RackIDs          []string                 `json:"rack_ids"`
	WeightAllocation map[string]int           `json:"weight_allocation_5_slots"`
	Result           *Result                  `json:"result"`
	Incremental      []IncrementalExpectation `json:"incremental"`
	ExpectedCodes    map[string]ErrorCode     `json:"expected_codes"`
}

type IncrementalExpectation struct {
	Edit        Event   `json:"edit"`
	MatchesFull bool    `json:"matches_full"`
	CacheHits   int     `json:"cache_hits"`
	Closure     Closure `json:"closure"`
}

type Check struct {
	Name   string    `json:"name"`
	OK     bool      `json:"ok"`
	Code   ErrorCode `json:"code,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

type VerificationReport struct {
	OK                 bool              `json:"ok"`
	Seed               int64             `json:"seed"`
	Fingerprint        string            `json:"fingerprint"`
	Checks             []Check           `json:"checks"`
	SchemaFingerprints map[string]string `json:"schema_fingerprints"`
	RoundCount         int               `json:"round_count"`
	ObservationCount   int               `json:"observation_count"`
	DispatchCount      int               `json:"dispatch_count"`
	QueuePeak          int               `json:"queue_peak"`
	DropCount          int               `json:"drop_count"`
	ParallelRounds     int               `json:"parallel_rounds"`
	ParallelDurationNS int64             `json:"parallel_duration_ns"`
}

func (r *VerificationReport) add(name string, err error) {
	if err == nil {
		r.Checks = append(r.Checks, Check{Name: name, OK: true})
		return
	}
	r.OK = false
	r.Checks = append(r.Checks, Check{Name: name, OK: false, Code: CodeOf(err), Detail: err.Error()})
}

// WriteSampleFiles generates deterministic documents, events, truth and
// exception fixtures under dir.
func WriteSampleFiles(dir string, seed int64) (*TruthFile, error) {
	g := NewGenerator(seed)
	p := g.BuildProject()
	events := g.BuildEvents()
	edits := g.BuildEdits()
	if err := os.MkdirAll(filepath.Join(dir, "invalid"), 0o755); err != nil {
		return nil, err
	}

	v1, err := MarshalV1(p)
	if err != nil {
		return nil, err
	}
	v2, _ := MarshalV2(p)
	v3, _ := MarshalV3(p)
	canonical, _ := json.MarshalIndent(p, "", "  ")
	eventBytes, _ := json.MarshalIndent(events, "", "  ")
	editBytes, _ := json.MarshalIndent(edits, "", "  ")
	files := map[string][]byte{
		"project.v1.json":        v1,
		"project.v2.json":        v2,
		"project.v3.json":        v3,
		"project.canonical.json": canonical,
		"events.json":            eventBytes,
		"edits.json":             editBytes,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return nil, err
		}
	}

	engine, err := NewEngine(p, events)
	if err != nil {
		return nil, err
	}
	result, err := engine.Run()
	if err != nil {
		return nil, err
	}
	fp, _ := p.Fingerprint()
	weights := map[string]int64{}
	for _, in := range p.Instances {
		weights[in.ID] = in.WeightPPM
	}
	allocation, err := AllocateWeightedSlots(weights, 5)
	if err != nil {
		return nil, err
	}

	cache, err := BuildCache(p, events)
	if err != nil {
		return nil, err
	}
	expectations := []IncrementalExpectation{}
	for _, edit := range edits {
		inc, err := cache.ApplyEdit(edit)
		if err != nil {
			return nil, err
		}
		expectations = append(expectations, IncrementalExpectation{
			Edit: edit, MatchesFull: inc.MatchesFull, CacheHits: inc.CacheHits, Closure: inc.Closure,
		})
	}

	truth := &TruthFile{
		Seed: seed, Fingerprint: fp,
		InstanceIDs: SortedInstanceIDs(p.Instances),
		RackIDs:     rackIDs(p.Racks), WeightAllocation: allocation,
		Result: result, Incremental: expectations,
		ExpectedCodes: map[string]ErrorCode{
			"unknown-schema.json":         ErrUnknownSchemaVersion,
			"negative-timeout.json":       ErrNegativeTimeout,
			"threshold-out-of-range.json": ErrThresholdOutOfRange,
			"missing-rack.json":           ErrRackReferenceMissing,
			"zero-interval.json":          ErrIntervalZero,
			"truncated-checkpoint.bin":    ErrCheckpointTruncated,
			"version-mismatch-checkpoint": ErrCheckpointVersionMatch,
		},
	}
	truthBytes, _ := json.MarshalIndent(truth, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), truthBytes, 0o644); err != nil {
		return nil, err
	}

	invalid := InvalidDocuments(p)
	for name, data := range invalid {
		if err := os.WriteFile(filepath.Join(dir, "invalid", name), data, 0o644); err != nil {
			return nil, err
		}
	}
	if err := WriteTruncatedCheckpoint(filepath.Join(dir, "invalid", "truncated-checkpoint.bin")); err != nil {
		return nil, err
	}
	return truth, nil
}

func rackIDs(racks []Rack) []string {
	out := make([]string, 0, len(racks))
	for _, r := range racks {
		out = append(out, r.ID)
	}
	return out
}

// VerifyDirectory validates generated files and executes all acceptance checks.
func VerifyDirectory(dir string) (*VerificationReport, error) {
	report := &VerificationReport{OK: true, SchemaFingerprints: map[string]string{}}
	readJSON := func(name string, v any) error {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		return json.Unmarshal(data, v)
	}

	var truth TruthFile
	if err := readJSON("truth.json", &truth); err != nil {
		return nil, err
	}
	report.Seed = truth.Seed
	report.Fingerprint = truth.Fingerprint

	loadedProjects := map[string]*Project{}
	for _, file := range []string{"project.v1.json", "project.v2.json", "project.v3.json"} {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			return nil, err
		}
		loaded, err := LoadDocument(data)
		report.add("load:"+file, err)
		if err == nil {
			fp, _ := loaded.Project.Fingerprint()
			report.SchemaFingerprints[loaded.SourceSchema] = fp
			loadedProjects[file] = loaded.Project
		}
	}

	var events []Event
	if err := readJSON("events.json", &events); err != nil {
		return nil, err
	}
	var edits []Event
	if err := readJSON("edits.json", &edits); err != nil {
		return nil, err
	}

	for file, project := range loadedProjects {
		engine, err := NewEngine(project, events)
		if err != nil {
			report.add("engine:"+file, err)
			continue
		}
		result, err := engine.Run()
		report.add("run:"+file, err)
		if err != nil {
			continue
		}
		equal := ResultsEqualSemantic(result, truth.Result)
		if !equal {
			report.add("truth:"+file, fmt.Errorf("round/state truth mismatch"))
		} else {
			report.add("truth:"+file, nil)
		}
		if file == "project.v1.json" {
			report.RoundCount = len(result.Rounds)
			report.ObservationCount = len(result.HealthTrace)
			for _, r := range result.Rounds {
				report.DispatchCount += len(r.Dispatched)
				report.DropCount += len(r.Dropped)
				if len(r.Queue) > report.QueuePeak {
					report.QueuePeak = len(r.Queue)
				}
			}
		}
	}

	project := loadedProjects["project.v1.json"]
	if project != nil {
		cache, err := BuildCache(project, events)
		report.add("incremental-cache", err)
		if err == nil {
			for _, edit := range edits {
				inc, err := cache.ApplyEdit(edit)
				if err != nil {
					report.add("incremental:"+edit.ID, err)
					continue
				}
				if !inc.MatchesFull {
					report.add("incremental:"+edit.ID, fmt.Errorf("incremental != full"))
				} else {
					report.add("incremental:"+edit.ID, nil)
				}
			}
		}

		parallelRun, perr := RunParallel(context.Background(), project, events, nil)
		report.add("parallel-run-all-rounds", perr)
		if perr == nil {
			report.ParallelRounds = len(parallelRun.Batches)
			for _, batch := range parallelRun.Batches {
				report.ParallelDurationNS += int64(batch.Duration)
			}
		}
		executor, err := NewParallelExecutor(project.Workers, project.BufferCapacity)
		report.add("parallel-executor", err)
		if err == nil {
			_, berr := executor.ExecuteBatch(context.Background(), 1, []Probe{{ID: "budget"}}, 99, nil)
			checkExpectedCode(report, "worker-backpressure-code", berr, ErrBackpressureWorker)
			small, _ := NewParallelExecutor(1, 0)
			_, berr = small.ExecuteBatch(context.Background(), 1, []Probe{{ID: "x"}}, 1, nil)
			checkExpectedCode(report, "buffer-backpressure-code", berr, ErrBackpressureBufferFull)
		}

		cpFile, err := os.CreateTemp("", "hckernel-verify-checkpoint-*.log")
		if err != nil {
			report.add("checkpoint-tempfile", err)
		}
		cpPath := ""
		if cpFile != nil {
			cpPath = cpFile.Name()
			_ = cpFile.Close()
			defer os.Remove(cpPath)
		}
		if cpPath != "" {
			engine, _ := NewEngine(project, events)
			for round := 1; round <= 3 && round <= project.Rounds; round++ {
				_ = engine.Advance(int64(round-1)*project.StepMs, round)
				cp := engine.Snapshot(round)
				err := CommitCheckpoint(cpPath, cp, CommitOptions{CrashBeforeCursor: round == 3})
				if round == 3 {
					checkExpectedCode(report, "checkpoint-injected-crash", err, ErrInjectedCrash)
				} else {
					report.add(fmt.Sprintf("checkpoint-commit-round-%d", round), err)
				}
			}
			cp, lerr := LoadCheckpoint(cpPath, LoadCheckpointOptions{ExpectedFingerprint: truth.Fingerprint})
			if lerr != nil {
				report.add("checkpoint-crash-cursor", lerr)
			} else if cp == nil || cp.CommittedRound != 2 {
				report.add("checkpoint-crash-cursor", fmt.Errorf("committed checkpoint=%+v", cp))
			} else {
				report.add("checkpoint-crash-cursor", nil)
			}
			if cp != nil {
				resume, rerr := RestoreEngine(project, events, cp)
				report.add("checkpoint-restore", rerr)
				if rerr == nil {
					resumeErr := error(nil)
					for round := cp.CommittedRound + 1; round <= project.Rounds; round++ {
						if err := resume.Advance(int64(round-1)*project.StepMs, round); err != nil {
							report.add("checkpoint-resume-round", err)
							resumeErr = err
							break
						}
					}
					if resumeErr == nil {
						report.add("checkpoint-resume-round", nil)
					}
				}
			}
		}
	}

	for name, expected := range truth.ExpectedCodes {
		path := filepath.Join(dir, "invalid", name)
		if name == "version-mismatch-checkpoint" {
			// Created below in a temp location because this case needs a valid log.
			continue
		}
		if name == "truncated-checkpoint.bin" {
			_, err := LoadCheckpoint(path, LoadCheckpointOptions{})
			checkExpectedCode(report, "exception:"+name, err, expected)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			report.add("exception:"+name, err)
			continue
		}
		_, lerr := LoadDocument(data)
		checkExpectedCode(report, "exception:"+name, lerr, expected)
	}

	tmpDir, err := os.MkdirTemp("", "hckernel-verify-*")
	if err != nil {
		return nil, err
	}
	mismatchPath := filepath.Join(tmpDir, "mismatch.log")
	cp := Checkpoint{Format: CheckpointFormat, ProjectVersion: truth.SeedString(), Fingerprint: "correct", CommittedRound: 1}
	_ = CommitCheckpoint(mismatchPath, cp, CommitOptions{})
	_, merr := LoadCheckpoint(mismatchPath, LoadCheckpointOptions{ExpectedFingerprint: truth.Fingerprint})
	checkExpectedCode(report, "exception:version-mismatch-checkpoint", merr, ErrCheckpointVersionMatch)

	return report, nil
}

func (t TruthFile) SeedString() string { return fmt.Sprintf("%d", t.Seed) }

func checkExpectedCode(report *VerificationReport, name string, err error, expected ErrorCode) {
	if IsCode(err, expected) {
		report.Checks = append(report.Checks, Check{Name: name, OK: true, Code: expected})
		return
	}
	report.OK = false
	report.Checks = append(report.Checks, Check{Name: name, OK: false, Code: CodeOf(err), Detail: fmt.Sprintf("want %s: %v", expected, err)})
}

// InvalidDocuments creates mutation-based stable-code fixtures.
func InvalidDocuments(p *Project) map[string][]byte {
	base, _ := MarshalV1(p)
	mutate := func(fn func(map[string]any)) []byte {
		var doc map[string]any
		_ = json.Unmarshal(base, &doc)
		fn(doc)
		out, _ := json.MarshalIndent(doc, "", "  ")
		return out
	}
	firstTarget := func(doc map[string]any) map[string]any {
		targets := doc["targets"].([]any)
		return targets[0].(map[string]any)
	}
	return map[string][]byte{
		"unknown-schema.json": []byte(`{"schema":"hcs/v999","project_name":"bad"}`),
		"negative-timeout.json": mutate(func(doc map[string]any) {
			firstTarget(doc)["timeout_ticks"] = -1
		}),
		"threshold-out-of-range.json": mutate(func(doc map[string]any) {
			firstTarget(doc)["success_threshold"] = 101
		}),
		"missing-rack.json": mutate(func(doc map[string]any) {
			firstTarget(doc)["rack"] = "rack-does-not-exist"
		}),
		"zero-interval.json": mutate(func(doc map[string]any) {
			firstTarget(doc)["interval_ticks"] = 0
		}),
	}
}
