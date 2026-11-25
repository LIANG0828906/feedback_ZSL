package demo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"alertkernel/checkpoint"
	"alertkernel/engine"
	"alertkernel/errs"
	"alertkernel/fixed"
	"alertkernel/incremental"
	"alertkernel/model"
	"alertkernel/sample"
	"alertkernel/schema"
)

func milestoneChecks(report *engine.RunReport) []Check {
	checks := []Check{}
	add := func(name string, ok bool, detail string) {
		checks = append(checks, Check{Name: name, OK: ok, Detail: detail})
	}

	cpuH1Pending := stateAt(report, 1, "R_CPU", model.Labels{"host": "h1"})
	add("cpu_h1_pending_at_t1", cpuH1Pending != nil && cpuH1Pending.State == engine.StatePending, stateDetail(cpuH1Pending))
	cpuH1Firing := stateAt(report, 3, "R_CPU", model.Labels{"host": "h1"})
	add("cpu_h1_fires_at_t3_after_for_30s", cpuH1Firing != nil && cpuH1Firing.State == engine.StateFiring, stateDetail(cpuH1Firing))
	cpuH1Keep := stateAt(report, 5, "R_CPU", model.Labels{"host": "h1"})
	add("cpu_h1_kept_firing_through_t5", cpuH1Keep != nil && cpuH1Keep.State == engine.StateFiring, stateDetail(cpuH1Keep))
	cpuH1Resolved := stateAt(report, 6, "R_CPU", model.Labels{"host": "h1"})
	add("cpu_h1_inactive_at_t6_after_keep_boundary", cpuH1Resolved != nil && cpuH1Resolved.State == engine.StateInactive, stateDetail(cpuH1Resolved))

	h3NoData := observationAt(report, 2, "R_CPU", model.Labels{"host": "h3"})
	add("cpu_h3_gap_is_no_data_not_zero", h3NoData != nil && !h3NoData.HasData && h3NoData.NoDataReason != "", obsDetail(h3NoData))

	disk := stateAt(report, 2, "R_DISK", model.Labels{"host": "db1"})
	add("zero_for_duration_fires_immediately", disk != nil && disk.State == engine.StateFiring && disk.FireBasisNo > 0, stateDetail(disk))
	diskResolved := stateAt(report, 4, "R_DISK", model.Labels{"host": "db1"})
	add("zero_keep_resolves_on_next_false_eval", diskResolved != nil && diskResolved.State == engine.StateInactive, stateDetail(diskResolved))

	targetT4 := stateAt(report, 4, "R_TARGET", model.Labels{"instance": "db-0"})
	add("target_internal_firing_at_t4", targetT4 != nil && targetT4.State == engine.StateFiring, stateDetail(targetT4))
	add("target_visible_at_t4", visibleAt(report, 4, "R_TARGET", "db-0"), "target should be visible before suppression")

	d5 := suppressionAt(report, 5, "R_TARGET")
	add("target_suppressed_at_t5_by_source", d5 != nil && d5.PrimaryReason == engine.ReasonSuppressed && d5.SourceRuleID == "R_SOURCE", decisionDetail(d5))
	add("target_hidden_at_t5", !visibleAt(report, 5, "R_TARGET", "db-0"), "suppressed target must be hidden")

	d7 := suppressionAt(report, 7, "R_TARGET")
	add("overlap_uses_declared_suppression_priority_at_t7", d7 != nil && d7.PrimaryReason == engine.ReasonSuppressed && d7.Suppressed && d7.Silenced, decisionDetail(d7))
	d8 := suppressionAt(report, 8, "R_TARGET")
	add("silence_only_at_t8", d8 != nil && d8.PrimaryReason == engine.ReasonSilenced && d8.SilenceID == "SIL-DB-EVENING", decisionDetail(d8))
	add("target_hidden_at_t8", !visibleAt(report, 8, "R_TARGET", "db-0"), "silenced target must be hidden")

	cpuBatch := batchAt(report, 3, "CPUHigh")
	add("cpu_two_instances_one_dedup_batch", cpuBatch != nil && len(cpuBatch.Members) == 2, batchDetail(cpuBatch))
	add("cpu_batch_routes_to_ops", cpuBatch != nil && cpuBatch.Receiver == "ops" && cpuBatch.RouteID == "route-ops", batchDetail(cpuBatch))
	add("exactly_four_notification_batches", countBatches(report) == 4, fmt.Sprintf("batch_count=%d", countBatches(report)))
	add("no_repeated_cpu_notification", !batchContainsRuleAt(report, 4, "R_CPU") && !batchContainsRuleAt(report, 5, "R_CPU"), "same firing state must not renotify")
	return checks
}

func checkConcurrency(ctx context.Context, p *model.Project, events model.EventBundle) (incremental.Comparison, int, error) {
	one := clone(p)
	one.Budget.Workers = 1
	one.Budget.PerRuleBuffer = 1
	r1, err := engine.RunAll(ctx, one, events)
	if err != nil {
		return incremental.Comparison{}, 0, err
	}
	eight := clone(p)
	eight.Budget.Workers = 8
	eight.Budget.PerRuleBuffer = 2
	r8, err := engine.RunAll(ctx, eight, events)
	if err != nil {
		return incremental.Comparison{}, 0, err
	}
	backpressure := 0
	for i := range r1.Times {
		backpressure += r1.Times[i].Scheduling.BackpressureEvents
		r1.Times[i].Scheduling = engine.SchedulingReport{}
		r8.Times[i].Scheduling = engine.SchedulingReport{}
	}
	return incremental.CompareReports(r1, r8), backpressure, nil
}

func checkEdits(ctx context.Context, bundle *sample.Bundle) []EditCheck {
	cases := []struct {
		name string
		edit incremental.Edit
	}{
		{"metric_overwrite_reuses_prefix", incremental.Edit{Kind: incremental.MetricOverwrite, Target: "cpu-h2", Sample: &model.Sample{Index: 2, Raw: 7500}}},
		{"silence_delete_reveals_suffix", incremental.Edit{Kind: incremental.SilenceDelete, Target: "SIL-DB-EVENING"}},
		{"predicate_threshold_recompute_all", incremental.Edit{Kind: incremental.RulePredicate, Target: "R_CPU", Threshold: int64Ptr(9999)}},
		{"for_duration_zero_changes_trigger_closure", incremental.Edit{Kind: incremental.RuleForDuration, Target: "R_CPU", ValueMS: int64Ptr(0)}},
		{"label_rewrite_changes_identity", incremental.Edit{Kind: incremental.RuleRewrite, Target: "R_CPU", Rewrite: map[string]string{
			"alertname": "CPUHigh", "env": "${env}", "host": "${host}", "zone": "${region}", "severity": "P9",
		}}},
		{"metric_append_closes_gap", incremental.Edit{Kind: incremental.MetricAppend, Target: "cpu-h3-gap", Sample: &model.Sample{Index: 3, Raw: 9900}}},
		{"silence_add_hides_one_dedup_member", incremental.Edit{Kind: incremental.SilenceAdd, Target: "SIL-CPU-H1", Silence: &model.SilenceWindow{
			ID: "SIL-CPU-H1", Matchers: []model.Matcher{{Label: "host", Op: "=", Value: "h1"}}, StartT: 3, EndT: 3,
		}}},
		{"suppression_event_delete", incremental.Edit{Kind: incremental.SuppressionDelete, Target: "SEV-INH-DB-05"}},
		{"inhibition_relation_add", incremental.Edit{Kind: incremental.InhibitionAdd, Target: "INH-CPU-DISK", Inhibition: &model.InhibitionRule{
			ID:             "INH-CPU-DISK",
			SourceMatchers: []model.Matcher{{Label: "alertname", Op: "=", Value: "CPUHigh"}},
			TargetMatchers: []model.Matcher{{Label: "alertname", Op: "=", Value: "DiskHigh"}},
			EqualLabels:    []string{"env"},
		}}},
	}
	out := []EditCheck{}
	for _, tc := range cases {
		p := clone(bundle.Project)
		events := clone(&bundle.Events)
		manager, err := incremental.NewManager(ctx, p, *events)
		if err != nil {
			out = append(out, EditCheck{Name: tc.name, Comparison: incremental.Comparison{Equal: false, Differences: []string{err.Error()}}})
			continue
		}
		outcome, err := manager.Apply(ctx, tc.edit)
		if err != nil {
			out = append(out, EditCheck{Name: tc.name, Edit: tc.edit, Comparison: incremental.Comparison{Equal: false, Differences: []string{err.Error()}}})
			continue
		}
		full, err := manager.FullRecompute(ctx)
		if err != nil {
			out = append(out, EditCheck{Name: tc.name, Edit: tc.edit, Invalidation: outcome.Invalidation, Comparison: incremental.Comparison{Equal: false, Differences: []string{err.Error()}}})
			continue
		}
		cmp := incremental.CompareReports(full, outcome.Report)
		out = append(out, EditCheck{Name: tc.name, Edit: tc.edit, Invalidation: outcome.Invalidation, Comparison: cmp, Outcome: outcome})
	}
	return out
}

func checkRecovery(ctx context.Context, dir string, bundle *sample.Bundle) Check {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Check{Name: "sqlite_recovery", OK: false, Detail: err.Error()}
	}
	path := filepath.Join(dir, "recover.db")
	removeCheckpointFiles(path)
	store, err := checkpoint.Open(path)
	if err != nil {
		return Check{Name: "sqlite_recovery", OK: false, Detail: err.Error()}
	}
	runner, err := engine.NewRunner(bundle.Project, bundle.Events, nil, 0)
	if err != nil {
		return Check{Name: "sqlite_recovery", OK: false, Detail: err.Error()}
	}
	for t := 0; t <= 4; t++ {
		frame, err := runner.Step(ctx, t)
		if err != nil {
			return Check{Name: "sqlite_recovery", OK: false, Detail: err.Error()}
		}
		if err := store.SaveTime(ctx, frame, bundle.Project.RuleVersion); err != nil {
			return Check{Name: "sqlite_recovery", OK: false, Detail: err.Error()}
		}
	}
	if err := store.Close(); err != nil {
		return Check{Name: "sqlite_recovery", OK: false, Detail: err.Error()}
	}

	reopened, err := checkpoint.Open(path)
	if err != nil {
		return Check{Name: "sqlite_recovery", OK: false, Detail: err.Error()}
	}
	defer reopened.Close()
	recovery, err := reopened.LoadRecovery(ctx, bundle.Project.RuleVersion)
	if err != nil {
		return Check{Name: "sqlite_recovery", OK: false, Detail: err.Error()}
	}
	if recovery.LastT != 4 || len(recovery.States) == 0 {
		return Check{Name: "sqlite_recovery", OK: false, Detail: fmt.Sprintf("last_t=%d states=%d", recovery.LastT, len(recovery.States))}
	}
	runner2, err := engine.NewRunner(bundle.Project, bundle.Events, recovery.States, recovery.BasisSeq)
	if err != nil {
		return Check{Name: "sqlite_recovery", OK: false, Detail: err.Error()}
	}
	gotTimes := append([]engine.TimeResult{}, recovery.Times...)
	for t := 5; t < bundle.Project.Clock.Steps; t++ {
		frame, err := runner2.Step(ctx, t)
		if err != nil {
			return Check{Name: "sqlite_recovery", OK: false, Detail: err.Error()}
		}
		if err := reopened.SaveTime(ctx, frame, bundle.Project.RuleVersion); err != nil {
			return Check{Name: "sqlite_recovery", OK: false, Detail: err.Error()}
		}
		gotTimes = append(gotTimes, *frame)
	}
	got := &engine.RunReport{ProjectID: bundle.Project.ProjectID, RuleVersion: bundle.Project.RuleVersion, Times: gotTimes}
	cmp := incremental.CompareReports(bundle.Truth, got)
	if !cmp.Equal {
		return Check{Name: "sqlite_recovery", OK: false, Detail: fmt.Sprintf("recovered report mismatch: %v", cmp.Differences)}
	}
	return Check{Name: "sqlite_recovery", OK: true, Detail: fmt.Sprintf("committed_t=4 recovered_states=%d resumed_times=5..9", len(recovery.States))}
}

func checkVersionMismatch(ctx context.Context, dir string, bundle *sample.Bundle) Check {
	path := filepath.Join(dir, "version-mismatch.db")
	removeCheckpointFiles(path)
	store, err := checkpoint.Open(path)
	if err != nil {
		return Check{Name: "checkpoint_version_mismatch", OK: false, Detail: err.Error()}
	}
	if err := store.SaveTime(ctx, &engine.TimeResult{T: 0, AtMS: bundle.Project.Clock.BaseEpochMS, BasisSeqAfter: 0}, "rv_old_seed"); err != nil {
		_ = store.Close()
		return Check{Name: "checkpoint_version_mismatch", OK: false, Detail: err.Error()}
	}
	if err := store.Close(); err != nil {
		return Check{Name: "checkpoint_version_mismatch", OK: false, Detail: err.Error()}
	}
	reopened, err := checkpoint.Open(path)
	if err != nil {
		return Check{Name: "checkpoint_version_mismatch", OK: false, Detail: err.Error()}
	}
	defer reopened.Close()
	_, err = reopened.LoadRecovery(ctx, bundle.Project.RuleVersion)
	if !errs.IsCode(err, errs.ECheckpointVersion) {
		return Check{Name: "checkpoint_version_mismatch", OK: false, Detail: fmt.Sprintf("want %s, got %v", errs.ECheckpointVersion, err)}
	}
	if err := reopened.Reset(ctx); err != nil {
		return Check{Name: "checkpoint_version_mismatch", OK: false, Detail: "reset: " + err.Error()}
	}
	recovery, err := reopened.LoadRecovery(ctx, bundle.Project.RuleVersion)
	if err != nil || recovery.LastT != -1 {
		return Check{Name: "checkpoint_version_mismatch", OK: false, Detail: fmt.Sprintf("reset did not force full recompute: recovery=%+v err=%v", recovery, err)}
	}
	return Check{Name: "checkpoint_version_mismatch", OK: true, Detail: "mismatch returns E_CHECKPOINT_VERSION_MISMATCH and reset starts at t=0"}
}

func checkCorruptCheckpoint(ctx context.Context, dir string, bundle *sample.Bundle) Check {
	path := filepath.Join(dir, "truncated.db")
	removeCheckpointFiles(path)
	store, err := checkpoint.Open(path)
	if err != nil {
		return Check{Name: "truncated_checkpoint_code", OK: false, Detail: err.Error()}
	}
	frame := &engine.TimeResult{T: 0, States: []engine.State{{RuleID: "R_CPU", InstanceKey: "x", State: "inactive", Labels: model.Labels{"host": "x"}}}}
	if err := store.SaveTime(ctx, frame, bundle.Project.RuleVersion); err != nil {
		return Check{Name: "truncated_checkpoint_code", OK: false, Detail: err.Error()}
	}
	if err := store.Close(); err != nil {
		return Check{Name: "truncated_checkpoint_code", OK: false, Detail: err.Error()}
	}
	removeCheckpointFiles(path + "-wal")
	removeCheckpointFiles(path + "-shm")
	info, err := os.Stat(path)
	if err != nil {
		return Check{Name: "truncated_checkpoint_code", OK: false, Detail: err.Error()}
	}
	if err := os.Truncate(path, info.Size()/2); err != nil {
		return Check{Name: "truncated_checkpoint_code", OK: false, Detail: err.Error()}
	}
	_, err = checkpoint.Open(path)
	if !errs.IsCode(err, errs.ECheckpointCorrupt) {
		return Check{Name: "truncated_checkpoint_code", OK: false, Detail: fmt.Sprintf("want %s, got code=%q err=%v", errs.ECheckpointCorrupt, errs.CodeOf(err), err)}
	}
	return Check{Name: "truncated_checkpoint_code", OK: true, Detail: "truncated sqlite file returns E_CHECKPOINT_CORRUPT"}
}

func injectionChecks() []Check {
	out := []Check{}
	expectCode := func(name, code string, fn func() error) {
		err := fn()
		out = append(out, Check{Name: name, OK: errs.IsCode(err, code), Detail: fmt.Sprintf("want=%s got=%q err=%v", code, errs.CodeOf(err), err)})
	}
	expectCode("unknown_schema_combo", errs.ESchemaUnknown, func() error {
		_, err := schema.Load([]byte(`{"schemaVersion":"v9"}`))
		return err
	})
	expectCode("malformed_predicate", errs.EPredicateMalformed, func() error {
		p := sampleProjectForInjection()
		p.Rules[0].Predicate.Op = "~~"
		return schema.Canonicalize(p)
	})
	expectCode("negative_duration", errs.EDurationNegative, func() error {
		p := sampleProjectForInjection()
		p.Rules[0].ForMS = -1
		return schema.Canonicalize(p)
	})
	expectCode("workers_out_of_range", errs.EBudgetWorkersOutOfRange, func() error {
		return schema.ValidateBudget(model.Budget{Workers: 999, PerRuleBuffer: 8})
	})
	expectCode("buffer_out_of_range", errs.EBudgetBufferOutOfRange, func() error {
		return schema.ValidateBudget(model.Budget{Workers: 2, PerRuleBuffer: 0})
	})
	expectCode("receiver_not_found", errs.EReceiverNotFound, func() error {
		p := sampleProjectForInjection()
		p.Routes = append(p.Routes, model.Route{ID: "bad", Receiver: "ghost"})
		return schema.Canonicalize(p)
	})
	expectCode("fixed_overflow_detected", errs.EFixedOverflow, func() error {
		_, _, err := fixed.Add(fixed.Value{Raw: 120, Bits: 8}, fixed.Value{Raw: 20, Bits: 8}, fixed.OverflowError, "inject")
		return err
	})

	saturated, overflow, err := fixed.Add(fixed.Value{Raw: 120, Bits: 8}, fixed.Value{Raw: 20, Bits: 8}, fixed.OverflowSaturate, "inject")
	out = append(out, Check{Name: "fixed_overflow_saturates", OK: err == nil && overflow && saturated.Raw == 127, Detail: fmt.Sprintf("raw=%d overflow=%v err=%v", saturated.Raw, overflow, err)})

	var coded *errs.Error
	ok := errors.As(errs.New(errs.EEditInvalid, "x", "y"), &coded)
	out = append(out, Check{Name: "stable_error_type", OK: ok && coded.Code == errs.EEditInvalid, Detail: "errs.Error carries E_xxx code and location"})
	return out
}

func sampleProjectForInjection() *model.Project {
	bundle, err := sample.Generate()
	if err != nil {
		panic(err)
	}
	return clone(bundle.Project)
}

func int64Ptr(v int64) *int64 { return &v }

func removeCheckpointFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}
