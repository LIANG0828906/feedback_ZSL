package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"alertkernel/engine"
	"alertkernel/incremental"
	"alertkernel/model"
	"alertkernel/sample"
	"alertkernel/schema"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type EditCheck struct {
	Name         string                   `json:"name"`
	Edit         incremental.Edit         `json:"edit"`
	Invalidation incremental.Invalidation `json:"invalidation"`
	Comparison   incremental.Comparison   `json:"comparison"`
	Outcome      *incremental.Outcome     `json:"-"`
}

type VerificationReport struct {
	OK                   bool        `json:"ok"`
	Seed                 int64       `json:"seed"`
	RuleVersion          string      `json:"rule_version"`
	SchemaCoreEqual      bool        `json:"schema_core_equal"`
	SchemaReportEqual    bool        `json:"schema_report_equal"`
	ConcurrencyEqual     bool        `json:"concurrency_equal"`
	BackpressureEvents   int         `json:"backpressure_events"`
	MilestoneChecks      []Check     `json:"milestone_checks"`
	EditChecks           []EditCheck `json:"edit_checks"`
	RecoveryCheck        Check       `json:"recovery_check"`
	VersionMismatchCheck Check       `json:"version_mismatch_check"`
	CorruptCheck         Check       `json:"corrupt_check"`
	InjectionChecks      []Check     `json:"injection_checks"`
	Errors               []string    `json:"errors,omitempty"`
}

func VerifyAll(ctx context.Context, outDir string) (*VerificationReport, error) {
	report := &VerificationReport{OK: true}
	bundle, err := sample.Generate()
	if err != nil {
		return nil, err
	}
	report.Seed = bundle.Seed
	report.RuleVersion = bundle.Project.RuleVersion
	if outDir != "" {
		if err := WriteGenerated(outDir, bundle); err != nil {
			return nil, err
		}
	}

	loaded := map[string]*model.Project{}
	for version, data := range bundle.Docs {
		p, err := schema.Load(data)
		if err != nil {
			report.fail(fmt.Sprintf("load %s: %v", version, err))
			continue
		}
		loaded[version] = p
	}
	if len(loaded) == 3 {
		report.SchemaCoreEqual = reflect.DeepEqual(coreProject(bundle.Project), coreProject(loaded["v1"])) &&
			reflect.DeepEqual(coreProject(bundle.Project), coreProject(loaded["v2"])) &&
			reflect.DeepEqual(coreProject(bundle.Project), coreProject(loaded["v3"]))
		if !report.SchemaCoreEqual {
			report.fail("canonical project cores differ across schema versions")
		}
		allSchemaReportsEqual := true
		for version := range loaded {
			events := eventsForLoaded(loaded[version], bundle.Events, bundle.Project.Silences)
			got, err := engine.RunAll(ctx, loaded[version], events)
			if err != nil {
				report.fail(fmt.Sprintf("run schema %s: %v", version, err))
				continue
			}
			cmp := incremental.CompareReports(bundle.Truth, got)
			if !cmp.Equal {
				allSchemaReportsEqual = false
				report.fail(fmt.Sprintf("schema %s report differs: %v", version, cmp.Differences))
			}
		}
		report.SchemaReportEqual = allSchemaReportsEqual
	}

	report.MilestoneChecks = milestoneChecks(bundle.Truth)
	for _, check := range report.MilestoneChecks {
		if !check.OK {
			report.fail(check.Name + ": " + check.Detail)
		}
	}

	concurrencyCmp, concurrencyBackpressure, err := checkConcurrency(ctx, bundle.Project, bundle.Events)
	if err != nil {
		report.fail("concurrency: " + err.Error())
	} else {
		report.ConcurrencyEqual = concurrencyCmp.Equal
		report.BackpressureEvents = concurrencyBackpressure
		if !concurrencyCmp.Equal {
			report.fail("concurrency results differ: " + fmt.Sprint(concurrencyCmp.Differences))
		}
		if concurrencyBackpressure != 20 {
			report.fail(fmt.Sprintf("expected 20 deterministic buffer backpressure events, got %d", concurrencyBackpressure))
		}
	}

	report.EditChecks = checkEdits(ctx, bundle)
	for _, check := range report.EditChecks {
		if !check.Comparison.Equal {
			report.fail("incremental " + check.Name + " differs: " + fmt.Sprint(check.Comparison.Differences))
		}
	}
	if outDir != "" {
		for i, check := range report.EditChecks {
			if check.Outcome == nil || check.Outcome.Report == nil {
				continue
			}
			data, err := json.MarshalIndent(check.Outcome.Report, "", "  ")
			if err != nil {
				return nil, err
			}
			name := fmt.Sprintf("incremental-%d-%s.json", i+1, check.Name)
			if err := os.WriteFile(filepath.Join(outDir, name), data, 0o644); err != nil {
				return nil, err
			}
		}
	}

	if outDir != "" {
		recoveryDir := filepath.Join(outDir, "checkpoints")
		report.RecoveryCheck = checkRecovery(ctx, recoveryDir, bundle)
		if !report.RecoveryCheck.OK {
			report.fail(report.RecoveryCheck.Detail)
		}
		report.VersionMismatchCheck = checkVersionMismatch(ctx, recoveryDir, bundle)
		if !report.VersionMismatchCheck.OK {
			report.fail(report.VersionMismatchCheck.Detail)
		}
		report.CorruptCheck = checkCorruptCheckpoint(ctx, recoveryDir, bundle)
		if !report.CorruptCheck.OK {
			report.fail(report.CorruptCheck.Detail)
		}
	}

	report.InjectionChecks = injectionChecks()
	for _, check := range report.InjectionChecks {
		if !check.OK {
			report.fail("injection " + check.Name + ": " + check.Detail)
		}
	}
	report.OK = len(report.Errors) == 0
	return report, nil
}

func WriteGenerated(outDir string, bundle *sample.Bundle) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	names := map[string]string{"v1": "project-v1.json", "v2": "project-v2.json", "v3": "project-v3.json"}
	for version, name := range names {
		if err := os.WriteFile(filepath.Join(outDir, name), bundle.Docs[version], 0o644); err != nil {
			return err
		}
	}
	truthJSON, err := json.MarshalIndent(bundle.Truth, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "truth.json"), truthJSON, 0o644); err != nil {
		return err
	}
	eventsJSON, err := json.MarshalIndent(bundle.Events, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "events.json"), eventsJSON, 0o644)
}

func (r *VerificationReport) fail(message string) {
	r.OK = false
	r.Errors = append(r.Errors, message)
}

func coreProject(p *model.Project) *model.Project {
	core := clone(p)
	core.SourceSchema = ""
	core.Silences = nil
	return core
}

func eventsForLoaded(p *model.Project, common model.EventBundle, canonicalSilences []model.SilenceWindow) model.EventBundle {
	events := clone(&common)
	if len(p.Silences) == 0 && len(canonicalSilences) > 0 {
		events.Silences = append(events.Silences, canonicalSilences...)
	}
	return *events
}

func clone[T any](in *T) *T {
	data, _ := json.Marshal(in)
	var out T
	_ = json.Unmarshal(data, &out)
	return &out
}
