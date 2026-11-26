package leasekit

import "encoding/json"

func ExportV1(p *Project) ([]byte, error) {
	if err := ensureTickAligned(p); err != nil {
		return nil, err
	}
	locks := make([]any, 0, len(p.Resources))
	for _, r := range p.Resources {
		locks = append(locks, map[string]any{
			"lock_id":                  r.ID,
			"lock_mode":                string(r.Mode),
			"lease_ticks":              msToTicks(r.LeaseTTLMS),
			"renew_window_ticks":       msToTicks(r.RenewWindowMS),
			"max_renewals":             r.MaxRenewals,
			"max_renew_duration_ticks": msToTicks(r.MaxRenewDurationMS),
			"renew_attempt_budget":     r.RenewAttemptBudget,
		})
	}
	holders := make([]any, 0, len(p.Holders))
	for _, h := range p.Holders {
		holders = append(holders, map[string]any{
			"holder_uid": h.ID,
			"lock_id":    h.ResourceID,
			"priority":   h.Priority,
		})
	}
	ops := make([]any, 0, len(p.Operations))
	for _, op := range p.SortedOperations() {
		item := map[string]any{
			"op_no":       op.Seq,
			"holder_uid":  op.HolderID,
			"action":      string(op.Kind),
			"at_tick":     msToTicks(op.AtMS),
			"contest_ref": op.Cohort,
		}
		if op.Cohort == "" {
			item["contest_ref"] = ""
		}
		if op.Kind == OpRenew {
			item["renew_duration_ticks"] = msToTicks(op.RenewForMS)
		} else {
			item["renew_duration_ticks"] = 0
		}
		ops = append(ops, item)
	}
	doc := map[string]any{
		"schema":          SchemaV1,
		"project_id":      p.ProjectID,
		"seed":            p.Seed,
		"ms_per_tick":     GeneratedTickMS,
		"boundary_policy": string(p.Boundary),
		"failure_policy":  string(p.FailurePolicy),
		"locks":           locks,
		"holders":         holders,
		"ops":             ops,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func ExportV2(p *Project) ([]byte, error) {
	tree := make([]any, 0, len(p.Resources))
	for _, r := range p.Resources {
		holders := make([]any, 0)
		for _, h := range p.Holders {
			if h.ResourceID == r.ID {
				holders = append(holders, map[string]any{"holderId": h.ID, "priority": h.Priority})
			}
		}
		tree = append(tree, map[string]any{
			"lockId":             r.ID,
			"lockMode":           string(r.Mode),
			"leaseDurationMs":    r.LeaseTTLMS,
			"renewWindow":        map[string]any{"kind": "remaining_ttl", "remainingTtlMs": r.RenewWindowMS},
			"maxRenewals":        r.MaxRenewals,
			"maxRenewDurationMs": r.MaxRenewDurationMS,
			"renewAttemptBudget": r.RenewAttemptBudget,
			"holders":            holders,
		})
	}
	ops := make([]any, 0, len(p.Operations))
	for _, op := range p.SortedOperations() {
		item := map[string]any{
			"seq":              op.Seq,
			"holderId":         op.HolderID,
			"action":           string(op.Kind),
			"atMs":             op.AtMS,
			"arbitrationGroup": op.Cohort,
		}
		if op.Kind == OpRenew {
			item["renewDurationMs"] = op.RenewForMS
		} else {
			item["renewDurationMs"] = 0
		}
		ops = append(ops, item)
	}
	doc := map[string]any{
		"schemaVersion":  SchemaV2,
		"projectId":      p.ProjectID,
		"seed":           p.Seed,
		"boundaryPolicy": string(p.Boundary),
		"failurePolicy":  string(p.FailurePolicy),
		"lockTree":       tree,
		"operations":     ops,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func ExportV3(p *Project) ([]byte, error) {
	tree := make([]any, 0, len(p.Resources))
	for _, r := range p.Resources {
		holders := make([]any, 0)
		for _, h := range p.Holders {
			if h.ResourceID == r.ID {
				holders = append(holders, map[string]any{"HolderId": h.ID, "Priority": h.Priority})
			}
		}
		tree = append(tree, map[string]any{
			"LockId":          r.ID,
			"LockMode":        string(r.Mode),
			"LeaseTtlSeconds": msToDecimalSeconds(r.LeaseTTLMS),
			"RenewWindow": map[string]any{
				"Kind":             "absolute_initial_window",
				"OpensAtSecond":    msToDecimalSeconds(r.LeaseTTLMS - r.RenewWindowMS),
				"DeadlineAtSecond": msToDecimalSeconds(r.LeaseTTLMS),
			},
			"MaxRenewals":        r.MaxRenewals,
			"MaxRenewTtlSeconds": msToDecimalSeconds(r.MaxRenewDurationMS),
			"RenewAttemptBudget": r.RenewAttemptBudget,
			"Holders":            holders,
		})
	}
	ops := make([]any, 0, len(p.Operations))
	for _, op := range p.SortedOperations() {
		item := map[string]any{
			"Seq":              op.Seq,
			"HolderId":         op.HolderID,
			"Action":           string(op.Kind),
			"AtSecond":         msToDecimalSeconds(op.AtMS),
			"ArbitrationGroup": op.Cohort,
		}
		if op.Kind == OpRenew {
			item["RenewForSeconds"] = msToDecimalSeconds(op.RenewForMS)
		} else {
			item["RenewForSeconds"] = Decimal("0")
		}
		if op.ExpectedReason != "" {
			item["ExpectedFailureReason"] = op.ExpectedReason
		}
		ops = append(ops, item)
	}
	doc := map[string]any{
		"SchemaVersion":  SchemaV3,
		"ProjectId":      p.ProjectID,
		"Seed":           p.Seed,
		"BoundaryPolicy": string(p.Boundary),
		"FailurePolicy":  string(p.FailurePolicy),
		"ClockOrigin":    "project_zero_initial_grant",
		"LockTree":       tree,
		"Operations":     ops,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func ensureTickAligned(p *Project) error {
	if GeneratedTickMS <= 0 {
		return validationError(CodeInvalidDuration, "ms_per_tick", "must be positive")
	}
	check := func(v Milli, path string) error {
		if int64(v)%GeneratedTickMS != 0 {
			return validationError(CodeTimeConversion, path, "%dms is not aligned to %dms ticks", v, GeneratedTickMS)
		}
		return nil
	}
	for _, r := range p.Resources {
		if err := check(r.LeaseTTLMS, r.ID+".ttl"); err != nil {
			return err
		}
		if err := check(r.RenewWindowMS, r.ID+".window"); err != nil {
			return err
		}
		if err := check(r.MaxRenewDurationMS, r.ID+".maxDuration"); err != nil {
			return err
		}
	}
	for _, op := range p.Operations {
		if err := check(op.AtMS, "op.at"); err != nil {
			return err
		}
		if op.Kind == OpRenew {
			if err := check(op.RenewForMS, "op.renewFor"); err != nil {
				return err
			}
		}
	}
	return nil
}
