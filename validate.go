package leasekit

import "fmt"

func ValidateProject(p *Project) error {
	if p == nil {
		return validationError(CodeInvalidSchema, "$", "project is nil")
	}
	if p.ProjectID == "" {
		return validationError(CodeInvalidSchema, "projectId", "is required")
	}
	if p.Boundary == "" {
		p.Boundary = BoundaryLowerInclusiveUpperExclusive
	}
	if p.Boundary != BoundaryLowerInclusiveUpperExclusive {
		return validationError(CodeFieldOutOfRange, "boundaryPolicy", "unsupported policy %q", p.Boundary)
	}
	if p.FailurePolicy == "" {
		p.FailurePolicy = FailureRetainDeadline
	}
	if p.FailurePolicy != FailureRetainDeadline {
		return validationError(CodeFieldOutOfRange, "failurePolicy", "unsupported policy %q", p.FailurePolicy)
	}

	resourceIDs := map[string]bool{}
	for i := range p.Resources {
		r := &p.Resources[i]
		path := fmt.Sprintf("resources[%d]", i)
		if r.ID == "" {
			return validationError(CodeInvalidSchema, path+".id", "is required")
		}
		if resourceIDs[r.ID] {
			return validationError(CodeDuplicateResource, path+".id", "duplicate resource %q", r.ID)
		}
		resourceIDs[r.ID] = true
		if r.Mode != ModeExclusive && r.Mode != ModeShared {
			return validationError(CodeFieldOutOfRange, path+".mode", "resource %s has unsupported mode %q", r.ID, r.Mode)
		}
		if r.LeaseTTLMS <= 0 || r.LeaseTTLMS > MaxSafeMilli {
			return validationError(CodeInvalidDuration, path+".leaseTtlMs", "must be in (0,%d], got %d", MaxSafeMilli, r.LeaseTTLMS)
		}
		if r.RenewWindowMS < 0 || r.RenewWindowMS > r.LeaseTTLMS {
			return validationError(CodeInvalidDuration, path+".renewWindowMs", "must be in [0,ttl], got %d ttl=%d", r.RenewWindowMS, r.LeaseTTLMS)
		}
		if r.MaxRenewals < 0 || r.MaxRenewals > 1_000_000 {
			return validationError(CodeFieldOutOfRange, path+".maxRenewals", "must be in [0,1000000], got %d", r.MaxRenewals)
		}
		if r.MaxRenewDurationMS <= 0 || r.MaxRenewDurationMS > r.LeaseTTLMS {
			return validationError(CodeInvalidDuration, path+".maxRenewDurationMs", "must be in (0,leaseTtlMs], got %d ttl=%d", r.MaxRenewDurationMS, r.LeaseTTLMS)
		}
	}

	holderIDs := map[string]bool{}
	for i := range p.Holders {
		h := &p.Holders[i]
		path := fmt.Sprintf("holders[%d]", i)
		if h.ID == "" {
			return validationError(CodeInvalidSchema, path+".id", "is required")
		}
		if holderIDs[h.ID] {
			return validationError(CodeDuplicateHolder, path+".id", "duplicate holder identifier %q", h.ID)
		}
		holderIDs[h.ID] = true
		if !resourceIDs[h.ResourceID] {
			return validationError(CodeUnknownResource, path+".resourceId", "holder %q refers to unknown resource %q", h.ID, h.ResourceID)
		}
	}

	seqs := map[int]bool{}
	for i := range p.Operations {
		op := &p.Operations[i]
		path := fmt.Sprintf("operations[%d]", i)
		if op.Seq <= 0 {
			return validationError(CodeFieldOutOfRange, path+".seq", "must be positive, got %d", op.Seq)
		}
		if seqs[op.Seq] {
			return withOp(validationError(CodeDuplicateOperation, path+".seq", "duplicate operation seq %d", op.Seq), op.Seq, op.HolderID)
		}
		seqs[op.Seq] = true
		if !holderIDs[op.HolderID] {
			return withOp(validationError(CodeUnknownHolder, path+".holderId", "unknown holder %q", op.HolderID), op.Seq, op.HolderID)
		}
		if op.AtMS < 0 || op.AtMS > MaxSafeMilli {
			return withOp(validationError(CodeInvalidDuration, path+".atMs", "operation time out of range: %d", op.AtMS), op.Seq, op.HolderID)
		}
		switch op.Kind {
		case OpAcquire, OpRelease, OpAbandon:
		case OpRenew:
			if op.RenewForMS <= 0 {
				return withOp(validationError(CodeInvalidDuration, path+".renewForMs", "renew duration must be positive, got %d", op.RenewForMS), op.Seq, op.HolderID)
			}
			h := holderByID(p, op.HolderID)
			r := resourceByID(p, h.ResourceID)
			if op.RenewForMS > r.MaxRenewDurationMS {
				return withOp(validationError(CodeInvalidDuration, path+".renewForMs", "renew duration %d exceeds declared bound %d", op.RenewForMS, r.MaxRenewDurationMS), op.Seq, op.HolderID)
			}
		default:
			return withOp(validationError(CodeInvalidSchema, path+".kind", "unsupported operation kind %q", op.Kind), op.Seq, op.HolderID)
		}
	}
	return nil
}

func holderByID(p *Project, id string) *Holder {
	for i := range p.Holders {
		if p.Holders[i].ID == id {
			return &p.Holders[i]
		}
	}
	return nil
}

func resourceByID(p *Project, id string) *Resource {
	for i := range p.Resources {
		if p.Resources[i].ID == id {
			return &p.Resources[i]
		}
	}
	return nil
}
