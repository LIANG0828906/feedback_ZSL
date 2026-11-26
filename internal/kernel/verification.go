package kernel

import (
	"path/filepath"
	"reflect"
)

type IncrementalCheck struct {
	Name               string              `json:"name"`
	Edit               Edit                `json:"edit"`
	EqualFullRecompute bool                `json:"equal_full_recompute"`
	Invalidation       *InvalidationReport `json:"invalidation"`
}

type CheckpointCheck struct {
	Path               string `json:"path"`
	SavedTick          int64  `json:"saved_tick"`
	LoadedTick         int64  `json:"loaded_tick"`
	RoundTripOK        bool   `json:"round_trip_ok"`
	ResumeSuffixOK     bool   `json:"resume_suffix_ok"`
	RecoveryAction     string `json:"recovery_action,omitempty"`
	RecoveryReasonCode string `json:"recovery_reason_code,omitempty"`
}

type VerificationReport struct {
	Seed             int64              `json:"seed"`
	SchemaEquivalent bool               `json:"schema_equivalent"`
	PathEquivalence  *PathEquivalence   `json:"path_equivalence"`
	Incremental      []IncrementalCheck `json:"incremental_checks"`
	Checkpoint       *CheckpointCheck   `json:"checkpoint"`
	Diagnostics      *DiagnosticReport  `json:"diagnostics"`
	AllOK            bool               `json:"all_ok"`
}

func RunFullVerification(seed int64, artifactDir string) (*VerificationReport, error) {
	if artifactDir == "" {
		artifactDir = "artifacts"
	}
	bundle, err := GenerateBundle(seed)
	if err != nil {
		return nil, err
	}
	report := &VerificationReport{Seed: seed, AllOK: true}

	schemaCase := verifySchemaEquivalence(bundle)
	report.SchemaEquivalent = schemaCase.OK

	eq, err := VerifyPathEquivalence(bundle.Project, DefaultBudgets())
	if err != nil {
		return nil, err
	}
	report.PathEquivalence = eq

	incremental, err := verifyIncremental(bundle.Project)
	if err != nil {
		return nil, err
	}
	report.Incremental = incremental

	cpCheck, err := verifyCheckpoint(bundle.Project, filepath.Join(artifactDir, "checkpoint.sqlite"))
	if err != nil {
		return nil, err
	}
	report.Checkpoint = cpCheck

	diagDir := filepath.Join(artifactDir, "diagnostics")
	diag, err := RunDiagnostics(seed, diagDir)
	if err != nil {
		return nil, err
	}
	report.Diagnostics = diag

	report.AllOK = report.SchemaEquivalent && eq.AllEqual && cpCheck.RoundTripOK && cpCheck.ResumeSuffixOK && diag.AllOK
	for _, c := range incremental {
		if !c.EqualFullRecompute {
			report.AllOK = false
		}
	}
	return report, nil
}

func verifyIncremental(p *Project) ([]IncrementalCheck, error) {
	alpha, _ := p.Tenant("t-alpha")
	edits := []Edit{
		{Kind: EditWeightEvent, EventID: "e-delta-weight-up", NewValue: 9500},
		{Kind: EditTenantQuota, TenantID: alpha.ID, NewValue: alpha.QuotaBytes + mustKiB(1024)},
		{Kind: EditTenantGuarantee, TenantID: "t-beta", NewValue: mustKiB(1100)},
		{Kind: EditNodeCapacity, NodeID: "node-a", NewValue: mustKiB(4608)},
	}
	out := make([]IncrementalCheck, 0, len(edits))
	for _, edit := range edits {
		engine, err := NewIncrementalEngine(p, DefaultBudgets())
		if err != nil {
			return nil, err
		}
		inv, incReport, err := engine.ApplyEdit(edit)
		if err != nil {
			return nil, err
		}
		full, err := RunReference(engine.Project, DefaultBudgets())
		if err != nil {
			return nil, err
		}
		equal := ReportsEqualState(incReport, full)
		out = append(out, IncrementalCheck{
			Name: string(edit.Kind), Edit: edit, EqualFullRecompute: equal, Invalidation: inv,
		})
	}
	return out, nil
}

func verifyCheckpoint(p *Project, path string) (*CheckpointCheck, error) {
	budgets := DefaultBudgets()
	full, err := RunReference(p, budgets)
	if err != nil {
		return nil, err
	}
	tickIndex := int64(3)
	tick := full.Tick(tickIndex)
	if tick == nil {
		last := full.Ticks[len(full.Ticks)-1]
		tick = &last
	}
	cp := BuildCheckpoint(p, budgets, *tick)
	if err := SaveCheckpoint(path, cp); err != nil {
		return nil, err
	}
	loaded, err := LoadCheckpoint(path)
	if err != nil {
		return &CheckpointCheck{Path: path, SavedTick: cp.CommittedTick}, err
	}
	check := &CheckpointCheck{
		Path: path, SavedTick: cp.CommittedTick, LoadedTick: loaded.CommittedTick,
		RoundTripOK: loaded.CommittedTick == cp.CommittedTick && loaded.ProjectFingerprint == p.Fingerprint(),
	}
	resumed, err := ResumeFromCheckpoint(p, budgets, loaded)
	if err != nil {
		return check, err
	}
	check.ResumeSuffixOK = reportMatchesFromTick(resumed, full, loaded.CommittedTick+1)

	recovered, _, recovery, err := RecoverOrRun(p, budgets, path)
	if err != nil {
		return check, err
	}
	if recovery != nil {
		check.RecoveryAction = recovery.Action
		check.RecoveryReasonCode = recovery.ReasonCode
	}
	_ = recovered
	return check, nil
}

func reportMatchesFromTick(got, want *RunReport, fromTick int64) bool {
	if fromTick > lastTickFromReports(want) {
		return true
	}
	for tick := fromTick; tick <= lastTickFromReports(want); tick++ {
		a := got.Tick(tick)
		b := want.Tick(tick)
		if a == nil || b == nil {
			return false
		}
		ca := CanonicalReport(&RunReport{Ticks: []TickReport{*a}})
		cb := CanonicalReport(&RunReport{Ticks: []TickReport{*b}})
		if !reflect.DeepEqual(ca, cb) {
			return false
		}
	}
	return true
}

func lastTickFromReports(r *RunReport) int64 {
	if len(r.Ticks) == 0 {
		return 0
	}
	return r.Ticks[len(r.Ticks)-1].Tick
}
