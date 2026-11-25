package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"maintenanceorchestrator/internal/domain"
	"maintenanceorchestrator/internal/generator"
	"maintenanceorchestrator/internal/lease"
	"maintenanceorchestrator/internal/parallel"
	"maintenanceorchestrator/internal/schema"
	"maintenanceorchestrator/internal/store"
)

func DiagnoseFault(name, workDir string) error {
	if workDir != "" {
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return err
		}
	}
	switch name {
	case "negative_watermark":
		p := generator.Generate(generator.DefaultSeed)
		p = domain.CloneProject(p)
		p.Nodes[4].WatermarkBasisPoints = -1
		return domain.ValidateProject(p)

	case "time_conflict":
		return diagnoseTimeConflict()

	case "unknown_field":
		return diagnoseUnknownField()

	case "migration_window":
		p := generator.Generate(generator.DefaultSeed)
		p = domain.CloneProject(p)
		p.Instances[0].MigrationTicks = 999
		return domain.ValidateProject(p)

	case "loan_order":
		p := generator.Generate(generator.DefaultSeed)
		p = domain.CloneProject(p)
		p.Loans[0].ExpiryTick = p.Loans[0].StartTick - 1
		return domain.ValidateProject(p)

	case "loan_maintenance":
		p := generator.Generate(generator.DefaultSeed)
		p = domain.CloneProject(p)
		p.Loans[0].BorrowerNodeID = "m-fast"
		return domain.ValidateProject(p)

	case "thread_budget":
		return domain.ValidateConcurrency(0, 8)

	case "queue_budget":
		return domain.ValidateConcurrency(4, 0)

	case "backpressure_queue":
		return diagnoseQueueBackpressure()

	case "backpressure_thread":
		return diagnoseThreadBackpressure()

	case "double_booking":
		p := generator.Generate(generator.DefaultSeed)
		_, err := lease.Settle(lease.Options{Project: p, FreedCapacity: map[string]int64{"m-fast": 0}, Ticks: p.ProbeTicks})
		return err

	case "truncated_checkpoint":
		return diagnoseTruncatedEnvelope(workDir)

	case "version_mismatch":
		return diagnoseVersionMismatch(workDir)

	default:
		return domain.NewError(domain.EInvalidFaultInjection, "$.fault", "unknown fault case %q", name)
	}
}

func FaultCases() []string {
	return []string{
		"negative_watermark", "time_conflict", "unknown_field", "migration_window",
		"loan_order", "loan_maintenance", "thread_budget", "queue_budget",
		"backpressure_queue", "backpressure_thread", "double_booking",
		"truncated_checkpoint", "version_mismatch",
	}
}

func diagnoseTimeConflict() error {
	p := generator.Generate(generator.DefaultSeed)
	data, err := schema.EncodeV1(p)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	events := doc["maintenance_events"].([]any)
	first := events[0].(map[string]any)
	first["window_offset"] = float64(999)
	raw, _ := json.Marshal(doc)
	_, err = schema.Load(raw)
	return err
}

func diagnoseUnknownField() error {
	p := generator.Generate(generator.DefaultSeed)
	data, err := schema.EncodeV2(p)
	if err != nil {
		return err
	}
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)
	doc["unrecognized_combination"] = map[string]any{"force": true, "flat_tree": "both"}
	raw, _ := json.Marshal(doc)
	_, err = schema.Load(raw)
	return err
}

func diagnoseQueueBackpressure() error {
	s, err := parallel.NewScheduler(1, 1)
	if err != nil {
		return err
	}
	defer s.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	if err := s.Submit("node-x", func(context.Context) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		return err
	}
	<-started
	if err := s.Submit("node-x", func(context.Context) error { return nil }); err != nil {
		return err
	}
	err = s.Submit("node-x", func(context.Context) error { return nil })
	close(release)
	return err
}

func diagnoseThreadBackpressure() error {
	s, err := parallel.NewScheduler(1, 1)
	if err != nil {
		return err
	}
	defer s.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_ = s.TryExecute(func(context.Context) error {
			close(started)
			<-release
			return nil
		})
		close(done)
	}()
	<-started
	err = s.TryExecute(func(context.Context) error { return nil })
	close(release)
	<-done
	return err
}

func diagnoseTruncatedEnvelope(workDir string) error {
	if workDir == "" {
		workDir = ".fault-work"
	}
	path := filepath.Join(workDir, "fault-truncated.db")
	s, err := store.Open(path)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.SaveEnvelope("cp-1", []byte(`{"wave":1,"nodes":["m-fast"]}`)); err != nil {
		return err
	}
	if err := s.TruncateEnvelope("cp-1"); err != nil {
		return err
	}
	_, err = s.LoadEnvelope("cp-1")
	return err
}

func diagnoseVersionMismatch(workDir string) error {
	if workDir == "" {
		workDir = ".fault-work"
	}
	path := filepath.Join(workDir, "fault-version.db")
	s, err := store.Open(path)
	if err != nil {
		return err
	}
	if err := s.SaveCommit(store.Checkpoint{StateVersion: 999, ProjectID: "fault", CommittedWave: 1}); err != nil {
		s.Close()
		return err
	}
	if err := s.Close(); err != nil {
		return err
	}
	reopened, err := store.Open(path)
	if err != nil {
		return err
	}
	defer reopened.Close()
	_, _, err = reopened.LoadRecovery()
	return err
}
