package kernel

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type DiagnosticCase struct {
	Name         string         `json:"name"`
	ExpectedCode string         `json:"expected_code"`
	Code         string         `json:"code,omitempty"`
	OK           bool           `json:"ok"`
	Location     string         `json:"location,omitempty"`
	Message      string         `json:"message,omitempty"`
	Context      map[string]any `json:"context,omitempty"`
}

type DiagnosticReport struct {
	Seed            int64            `json:"seed"`
	AllOK           bool             `json:"all_ok"`
	Cases           []DiagnosticCase `json:"cases"`
	PathEquivalence *PathEquivalence `json:"path_equivalence,omitempty"`
}

func RunDiagnostics(seed int64, checkpointDir string) (*DiagnosticReport, error) {
	if checkpointDir == "" {
		checkpointDir = filepath.Join("artifacts", "diagnostics")
	}
	if err := os.MkdirAll(checkpointDir, 0o755); err != nil {
		return nil, err
	}
	report := &DiagnosticReport{Seed: seed, AllOK: true}
	bundle, err := GenerateBundle(seed)
	if err != nil {
		return nil, err
	}

	report.add(verifySchemaEquivalence(bundle))
	eq, err := VerifyPathEquivalence(bundle.Project, DefaultBudgets())
	if err != nil {
		return nil, err
	}
	report.PathEquivalence = eq
	report.add(DiagnosticCase{Name: "fast_exact_parallel_equivalence", ExpectedCode: CodeOK, Code: CodeOK, OK: eq.AllEqual, Context: map[string]any{"all_equal": eq.AllEqual, "allocations": eq.AllocationCount}})

	report.add(expectLoadCode("unknown_field", bundle.V1, func(m map[string]any) {
		nodes := m["nodes"].([]any)
		nodes[0].(map[string]any)["unexpected_tuning_field"] = true
	}, CodeUnknownField))

	report.add(expectLoadCode("negative_quota", bundle.V1, func(m map[string]any) {
		tenants := m["tenants"].([]any)
		tenants[0].(map[string]any)["quota_bytes"] = -1
	}, CodeNegativeQuota))

	report.add(expectLoadCode("guarantee_exceeds_quota", bundle.V1, func(m map[string]any) {
		tenants := m["tenants"].([]any)
		t := tenants[0].(map[string]any)
		t["guarantee_bytes"] = int64(9_999_999_999)
	}, CodeGuaranteeOverQuota))

	report.add(expectLoadCode("burst_percent_out_of_range", bundle.V3, func(m map[string]any) {
		tenants := m["tenants"].([]any)
		tenants[0].(map[string]any)["burst-cap-bp"] = 12000
	}, CodeBurstPercentRange))

	report.add(expectLoadCode("event_tick_out_of_order", bundle.V1, func(m map[string]any) {
		events := m["events"].([]any)
		events[1].(map[string]any)["tick"] = 0
	}, CodeEventOrder))

	report.add(expectLoadCode("unsupported_project_schema", bundle.V1, func(m map[string]any) {
		m["schema"] = "quotakernel.project/v99"
	}, CodeSchemaVersion))

	report.add(burstConflictCase(bundle))
	report.add(budgetCases()...)
	report.add(queueBackpressureCase())
	report.add(workerBackpressureCase())
	report.add(causalCycleCase())
	report.add(fixedPointCases())
	report.add(checkpointExceptionCases(bundle, checkpointDir)...)

	for _, c := range report.Cases {
		if !c.OK {
			report.AllOK = false
		}
	}
	return report, nil
}

func (r *DiagnosticReport) add(cases ...DiagnosticCase) {
	r.Cases = append(r.Cases, cases...)
}

func deepMapCopy(in map[string]any) map[string]any {
	data, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func loadMutated(name string, source map[string]any, mutate func(map[string]any)) (*Project, error) {
	m := deepMapCopy(source)
	mutate(m)
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return LoadJSON(data)
}

func expectLoadCode(name string, source map[string]any, mutate func(map[string]any), expected string) DiagnosticCase {
	_, err := loadMutated(name, source, mutate)
	code := BizCode(err)
	c := DiagnosticCase{Name: name, ExpectedCode: expected, Code: code, OK: code == expected}
	if be, ok := AsBizError(err); ok {
		c.Location = be.Path
		c.Message = be.Message
		c.Context = be.Details
		if be.TenantID != "" {
			c.Context = ensureContext(c.Context)
			c.Context["tenant_id"] = be.TenantID
		}
		if be.NodeID != "" {
			c.Context = ensureContext(c.Context)
			c.Context["node_id"] = be.NodeID
		}
	}
	return c
}

func ensureContext(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func verifySchemaEquivalence(b *Bundle) DiagnosticCase {
	v1, err := loadMap(b.V1)
	if err != nil {
		return DiagnosticCase{Name: "three_schema_equivalence", ExpectedCode: CodeOK, Code: BizCode(err), OK: false, Message: err.Error()}
	}
	v2, err := loadMap(b.V2)
	if err != nil {
		return DiagnosticCase{Name: "three_schema_equivalence", ExpectedCode: CodeOK, Code: BizCode(err), OK: false, Message: err.Error()}
	}
	v3, err := loadMap(b.V3)
	if err != nil {
		return DiagnosticCase{Name: "three_schema_equivalence", ExpectedCode: CodeOK, Code: BizCode(err), OK: false, Message: err.Error()}
	}
	ok := v1.Fingerprint() == v2.Fingerprint() && v1.Fingerprint() == v3.Fingerprint() &&
		len(v1.Tenants) == len(b.Project.Tenants) && len(v1.Nodes) == len(b.Project.Nodes)
	return DiagnosticCase{Name: "three_schema_equivalence", ExpectedCode: CodeOK, Code: CodeOK, OK: ok, Context: map[string]any{
		"fingerprint_v1": v1.Fingerprint(), "fingerprint_v2": v2.Fingerprint(), "fingerprint_v3": v3.Fingerprint(),
	}}
}

func loadMap(m map[string]any) (*Project, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return LoadJSON(data)
}

func burstConflictCase(b *Bundle) DiagnosticCase {
	alpha := b.Project.Tenants[0].ID
	p3, err := loadMutated("burst-conflict", b.V3, func(m map[string]any) {
		tenants := m["tenants"].([]any)
		for _, raw := range tenants {
			t := raw.(map[string]any)
			if t["tenant-id"] == string(alpha) {
				t["burst-cap-bp"] = 1234
			}
		}
	})
	if err != nil {
		return DiagnosticCase{Name: "burst_expression_version_conflict", ExpectedCode: CodeBurstConflict, Code: BizCode(err), OK: false, Message: err.Error()}
	}
	conflict := CheckBurstConflict(b.Project, p3)
	code := BizCode(conflict)
	return DiagnosticCase{Name: "burst_expression_version_conflict", ExpectedCode: CodeBurstConflict, Code: code, OK: code == CodeBurstConflict,
		Location: "$.tenants.burst-cap-bp", Context: map[string]any{"tenant_id": alpha}}
}

func CheckBurstConflict(a, b *Project) error {
	for _, ta := range a.Tenants {
		tb, ok := b.Tenant(ta.ID)
		if !ok {
			return NewBizError(CodeReferenceNotFound, "tenant %q missing in comparison", ta.ID).WithTenant(ta.ID)
		}
		if ta.BurstLimitBytes != tb.BurstLimitBytes {
			return NewBizError(CodeBurstConflict, "burst expressions disagree for tenant %q: %d vs %d bytes", ta.ID, ta.BurstLimitBytes, tb.BurstLimitBytes).
				WithTenant(ta.ID).WithDetail("v_a_burst_bytes", ta.BurstLimitBytes).WithDetail("v_b_burst_bytes", tb.BurstLimitBytes)
		}
	}
	return nil
}

func budgetCases() []DiagnosticCase {
	cases := []DiagnosticCase{}
	err := Budgets{Workers: 0, PerTenantQueue: 4}.Validate()
	cases = append(cases, DiagnosticCase{Name: "worker_budget_out_of_range", ExpectedCode: CodeWorkerBudget, Code: BizCode(err), OK: BizCode(err) == CodeWorkerBudget, Message: errorMessage(err)})
	err = Budgets{Workers: 4, PerTenantQueue: 0}.Validate()
	cases = append(cases, DiagnosticCase{Name: "queue_budget_out_of_range", ExpectedCode: CodeQueueBudget, Code: BizCode(err), OK: BizCode(err) == CodeQueueBudget, Message: errorMessage(err)})
	return cases
}

func queueBackpressureCase() DiagnosticCase {
	p := &Project{
		ProjectID: "queue-backpressure", TickMS: 100, Policy: DefaultFixedPolicy(),
		Nodes:   []Node{{ID: "n1", CapacityBytes: 1024, AllowedTenants: []ID{"t1"}}},
		Tenants: []Tenant{{ID: "t1", Priority: 1, QuotaBytes: 1024 * 1024, GuaranteeBytes: 0, BurstLimitBytes: 0}},
		Events: []Event{
			{ID: "r1", Tick: 1, Type: EventTypeApply, TenantID: "t1", NodeID: "n1", AmountBytes: 4096, BudgetMS: 10},
			{ID: "r2", Tick: 1, Type: EventTypeApply, TenantID: "t1", NodeID: "n1", AmountBytes: 4096, BudgetMS: 10},
		},
	}
	r, err := RunReference(p, Budgets{Workers: 1, PerTenantQueue: 1})
	if err != nil {
		return DiagnosticCase{Name: "queue_backpressure_runtime", ExpectedCode: CodeQueueFull, Code: BizCode(err), OK: false, Message: err.Error()}
	}
	a, ok := r.Allocation("r2")
	code := a.ReasonCode
	if !ok {
		code = "missing_allocation"
	}
	return DiagnosticCase{Name: "queue_backpressure_runtime", ExpectedCode: CodeQueueFull, Code: code, OK: code == CodeQueueFull,
		Context: map[string]any{"decision": a.Decision, "rejected_bytes": a.RejectedBytes, "constraint": a.ConstraintID}}
}

func workerBackpressureCase() DiagnosticCase {
	pool := NewWorkerPool(1)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	if err := pool.TrySubmit(func() {
		entered <- struct{}{}
		<-release
	}); err != nil {
		return DiagnosticCase{Name: "worker_backpressure_runtime", ExpectedCode: CodeWorkerBusy, Code: BizCode(err), OK: false, Message: err.Error()}
	}
	<-entered
	err := pool.TrySubmit(func() {})
	close(release)
	pool.Wait()
	code := BizCode(err)
	return DiagnosticCase{Name: "worker_backpressure_runtime", ExpectedCode: CodeWorkerBusy, Code: code, OK: code == CodeWorkerBusy, Message: errorMessage(err)}
}

func causalCycleCase() DiagnosticCase {
	cycle := CausalCycle([]Event{
		{ID: "a", Refs: []string{"b"}},
		{ID: "b", Refs: []string{"c"}},
		{ID: "c", Refs: []string{"a"}},
	})
	ok := cycle != nil
	code := ""
	if ok {
		code = CodeCausalCycle
	}
	return DiagnosticCase{Name: "causal_reference_cycle", ExpectedCode: CodeCausalCycle, Code: code, OK: ok,
		Location: "$.events.causal_refs", Context: map[string]any{"cycle": cycle}}
}

func fixedPointCases() DiagnosticCase {
	policy := DefaultFixedPolicy()
	ok := !WithinTolerance(12, 10, policy)
	_, err := PermilleToPermyriad(1 << 62)
	overflowCode := BizCode(err)
	return DiagnosticCase{Name: "fixed_point_tolerance_and_overflow", ExpectedCode: CodeTolerance, Code: CodeTolerance, OK: ok && overflowCode == CodeOverflow,
		Location: "tenant=t-critical/node=n-critical", Context: map[string]any{
			"diff": 2, "tolerance_bytes": policy.ToleranceBytes, "overflow_code": overflowCode,
			"tenant_id": "t-critical", "node_id": "n-critical",
		}}
}

func checkpointExceptionCases(b *Bundle, dir string) []DiagnosticCase {
	validPath := filepath.Join(dir, "valid.sqlite")
	cp := BuildCheckpoint(b.Project, DefaultBudgets(), b.Truth.Ticks[len(b.Truth.Ticks)-1])
	if err := SaveCheckpoint(validPath, cp); err != nil {
		return []DiagnosticCase{{Name: "checkpoint_fixture", OK: false, Message: err.Error()}}
	}
	valid, err := os.ReadFile(validPath)
	if err != nil {
		return []DiagnosticCase{{Name: "checkpoint_fixture", OK: false, Message: err.Error()}}
	}
	truncPath := filepath.Join(dir, "truncated.sqlite")
	truncLen := 64
	if len(valid) < truncLen {
		truncLen = len(valid) / 2
	}
	if err := os.WriteFile(truncPath, valid[:truncLen], 0o644); err != nil {
		return []DiagnosticCase{{Name: "checkpoint_truncated", OK: false, Message: err.Error()}}
	}
	_, truncErr := LoadCheckpoint(truncPath)
	truncCode := BizCode(truncErr)

	bad := BuildCheckpoint(b.Project, DefaultBudgets(), b.Truth.Ticks[len(b.Truth.Ticks)-1])
	bad.FormatVersion = "quotakernel-checkpoint-v0"
	bundle, _ := json.Marshal(bad)
	data, err := BuildNativeSQLite(checkpointTables(bad, string(bundle)))
	if err != nil {
		return []DiagnosticCase{{Name: "checkpoint_version_mismatch", OK: false, Message: err.Error()}}
	}
	badPath := filepath.Join(dir, "bad-version.sqlite")
	if err := atomicWrite(badPath, data); err != nil {
		return []DiagnosticCase{{Name: "checkpoint_version_mismatch", OK: false, Message: err.Error()}}
	}
	_, versionErr := LoadCheckpoint(badPath)
	versionCode := BizCode(versionErr)

	return []DiagnosticCase{
		{Name: "truncated_checkpoint", ExpectedCode: CodeCheckpointTruncated, Code: truncCode, OK: truncCode == CodeCheckpointTruncated, Location: truncPath, Message: errorMessage(truncErr)},
		{Name: "checkpoint_version_mismatch", ExpectedCode: CodeCheckpointVersion, Code: versionCode, OK: versionCode == CodeCheckpointVersion, Location: badPath, Message: errorMessage(versionErr)},
	}
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
