package leasekit

import (
	"fmt"
	"sort"
)

type StatePatch struct {
	HolderID    string `json:"holderId"`
	BeforeSeq   int    `json:"beforeSeq"`
	Active      *bool  `json:"active,omitempty"`
	ExpiresAtMS *Milli `json:"expiresAtMs,omitempty"`
	RenewCount  *int   `json:"renewCount,omitempty"`
}

type ResultOverride struct {
	Code        string `json:"code"`
	Granted     bool   `json:"granted"`
	ExpiresAtMS Milli  `json:"expiresAtMs,omitempty"`
	RenewCount  int    `json:"renewCount,omitempty"`
}

type IncrementalOptions struct {
	AttemptBudgetPerResource int
	StatePatches             []StatePatch
	ResultOverrides          map[int]ResultOverride
}

type IncrementalResult struct {
	Report            *Report           `json:"report"`
	ChangedResources  []string          `json:"changedResources"`
	CachedResources   []string          `json:"cachedResources"`
	RecomputedSeqs    []int             `json:"recomputedSeqs"`
	CacheHitSeqs      []int             `json:"cacheHitSeqs"`
	DependencyReasons map[int]string    `json:"dependencyReasons"`
	SourcesBySeq      map[int]SourceRef `json:"sourcesBySeq"`
}

type IncrementalSession struct {
	project *Project
	options IncrementalOptions
	base    *Report
}

func NewIncrementalSession(p *Project) (*IncrementalSession, error) {
	clone := p.Clone()
	report, err := Run(clone)
	if err != nil {
		return nil, err
	}
	return &IncrementalSession{project: clone, base: report}, nil
}

func (s *IncrementalSession) Report() *Report { return s.base }

func (s *IncrementalSession) FullRecompute() (*Report, error) {
	return RunWithOptions(s.project, RunOptions{
		AttemptBudgetPerResource: s.options.AttemptBudgetPerResource,
		StatePatches:             s.options.StatePatches,
		ResultOverrides:          s.options.ResultOverrides,
	})
}

func (s *IncrementalSession) EditRenewWindow(resourceID string, windowMS Milli) (*IncrementalResult, error) {
	r := resourceByID(s.project, resourceID)
	if r == nil {
		return nil, validationError(CodeUnknownResource, "resourceId", "unknown resource %q", resourceID)
	}
	if windowMS < 0 || windowMS > r.LeaseTTLMS {
		return nil, validationError(CodeInvalidDuration, "renewWindowMs", "must be in [0,ttl=%d], got %d", r.LeaseTTLMS, windowMS)
	}
	r.RenewWindowMS = windowMS
	return s.recomputeHolder(resourceID, "", 0, true, "renewal window parameter edited")
}

func (s *IncrementalSession) EditHolderExpiry(holderID string, beforeSeq int, expiryMS Milli, renewCount int) (*IncrementalResult, error) {
	h := holderByID(s.project, holderID)
	if h == nil {
		return nil, validationError(CodeUnknownHolder, "holderId", "unknown holder %q", holderID)
	}
	if expiryMS < 0 || expiryMS > MaxSafeMilli {
		return nil, validationError(CodeInvalidDuration, "expiresAtMs", "expiry out of range: %d", expiryMS)
	}
	if renewCount < 0 || renewCount > 1_000_000 {
		return nil, validationError(CodeFieldOutOfRange, "renewCount", "count out of range: %d", renewCount)
	}
	active := true
	s.options.StatePatches = append(s.options.StatePatches, StatePatch{
		HolderID: holderID, BeforeSeq: beforeSeq, Active: &active,
		ExpiresAtMS: &expiryMS, RenewCount: &renewCount,
	})
	return s.recomputeHolder(h.ResourceID, holderID, beforeSeq, false, "holder expiry edited")
}

func (s *IncrementalSession) EditRenewalResult(seq int, override ResultOverride) (*IncrementalResult, error) {
	op := s.operationBySeq(seq)
	if op == nil {
		return nil, validationError(CodeFieldOutOfRange, "seq", "unknown operation seq=%d", seq)
	}
	if op.Kind != OpRenew {
		return nil, validationError(CodeFieldConflict, "seq", "seq=%d is %s, not renew", seq, op.Kind)
	}
	if override.Code == "" {
		if override.Granted {
			override.Code = CodeOK
		} else {
			override.Code = CodeCompetitionLost
		}
	}
	if s.options.ResultOverrides == nil {
		s.options.ResultOverrides = map[int]ResultOverride{}
	}
	s.options.ResultOverrides[seq] = override
	h := holderByID(s.project, op.HolderID)
	return s.recomputeHolder(h.ResourceID, op.HolderID, seq, false, "renewal result edited")
}

func (s *IncrementalSession) operationBySeq(seq int) *Operation {
	for i := range s.project.Operations {
		if s.project.Operations[i].Seq == seq {
			return &s.project.Operations[i]
		}
	}
	return nil
}

func (s *IncrementalSession) recomputeHolder(resourceID, holderID string, cutoff int, resourceWide bool, editReason string) (*IncrementalResult, error) {
	next, err := runResourceWithOptions(s.project, resourceID, s.options.toRunOptions())
	if err != nil {
		return nil, err
	}
	affected, reasons := s.dependencyClosure(resourceID, holderID, cutoff, resourceWide, editReason)
	merged := s.mergeResourceReport(resourceID, next, affected)

	out := &IncrementalResult{
		Report:            merged,
		ChangedResources:  []string{resourceID},
		DependencyReasons: reasons,
		SourcesBySeq:      map[int]SourceRef{},
	}
	for _, r := range s.project.Resources {
		if r.ID == resourceID {
			out.ChangedResources = []string{resourceID}
		} else {
			out.CachedResources = append(out.CachedResources, r.ID)
		}
	}
	for seq := range affected {
		out.RecomputedSeqs = append(out.RecomputedSeqs, seq)
	}
	for _, op := range s.project.SortedOperations() {
		if !affected[op.Seq] {
			out.CacheHitSeqs = append(out.CacheHitSeqs, op.Seq)
		}
		if rr := merged.ResultBySeq(op.Seq); rr != nil {
			out.SourcesBySeq[op.Seq] = rr.Source
		}
	}
	sort.Ints(out.RecomputedSeqs)
	sort.Ints(out.CacheHitSeqs)
	s.base = merged
	return out, nil
}

func (o IncrementalOptions) toRunOptions() RunOptions {
	return RunOptions{
		AttemptBudgetPerResource: o.AttemptBudgetPerResource,
		StatePatches:             append([]StatePatch(nil), o.StatePatches...),
		ResultOverrides:          o.ResultOverrides,
	}
}

func runResourceWithOptions(p *Project, resourceID string, opts RunOptions) (*Report, error) {
	fp := filterResource(p, resourceID)
	return RunWithOptions(fp, opts)
}

func filterResource(p *Project, resourceID string) *Project {
	out := p.Clone()
	out.Resources = nil
	for _, r := range p.Resources {
		if r.ID == resourceID {
			r := r
			out.Resources = []Resource{r}
			break
		}
	}
	holderIDs := map[string]bool{}
	out.Holders = nil
	for _, h := range p.Holders {
		if h.ResourceID == resourceID {
			out.Holders = append(out.Holders, h)
			holderIDs[h.ID] = true
		}
	}
	out.Operations = nil
	for _, op := range p.Operations {
		if holderIDs[op.HolderID] {
			out.Operations = append(out.Operations, op)
		}
	}
	return out
}

func (s *IncrementalSession) dependencyClosure(resourceID, holderID string, cutoff int, resourceWide bool, editReason string) (map[int]bool, map[int]string) {
	affected := map[int]bool{}
	reasons := map[int]string{}
	ops := s.project.SortedOperations()
	holderOps := map[string][]Operation{}
	for _, op := range ops {
		h := holderByID(s.project, op.HolderID)
		if h != nil && h.ResourceID == resourceID {
			holderOps[op.HolderID] = append(holderOps[op.HolderID], op)
		}
	}

	add := func(seq int, reason string) {
		if !affected[seq] {
			affected[seq] = true
			reasons[seq] = reason
		}
	}

	if resourceWide {
		for _, op := range ops {
			h := holderByID(s.project, op.HolderID)
			if h != nil && h.ResourceID == resourceID {
				add(op.Seq, editReason)
			}
		}
		return affected, reasons
	}

	for _, op := range holderOps[holderID] {
		if op.Seq >= cutoff {
			add(op.Seq, editReason+": holder chain")
		}
	}

	cohortNames := map[string]bool{}
	for _, op := range holderOps[holderID] {
		if op.Seq >= cutoff && op.Cohort != "" && op.Kind == OpRenew {
			cohortNames[op.Cohort] = true
		}
	}
	for _, op := range ops {
		if op.Cohort != "" && cohortNames[op.Cohort] {
			add(op.Seq, "same-window arbitration dependency: "+op.Cohort)
		}
	}

	first := cutoff
	for _, op := range ops {
		if op.Kind == OpAcquire {
			h := holderByID(s.project, op.HolderID)
			if h != nil && h.ResourceID == resourceID && op.Seq > first {
				add(op.Seq, fmt.Sprintf("later acquire observes %s invalidation/release", holderID))
			}
		}
	}

	dirtyFrom := map[string]int{}
	for seq := range affected {
		if op := s.operationBySeq(seq); op != nil {
			if first, ok := dirtyFrom[op.HolderID]; !ok || seq < first {
				dirtyFrom[op.HolderID] = seq
			}
		}
	}
	for _, op := range ops {
		if first, ok := dirtyFrom[op.HolderID]; ok && op.Seq >= first {
			add(op.Seq, "downstream holder state chain observes edited result")
		}
	}
	return affected, reasons
}

func (s *IncrementalSession) mergeResourceReport(resourceID string, next *Report, affected map[int]bool) *Report {
	merged := &Report{
		SchemaVersion: CanonicalSchemaVersion,
		ProjectID:     s.project.ProjectID,
		Seed:          s.project.Seed,
		ToleranceMS:   0,
	}
	newBySeq := map[int]OpResult{}
	for _, r := range next.Results {
		newBySeq[r.Seq] = r
	}
	for _, op := range s.project.SortedOperations() {
		if affected[op.Seq] {
			if nr, ok := newBySeq[op.Seq]; ok {
				nr.Recomputed = true
				merged.Results = append(merged.Results, nr)
				continue
			}
		}
		old := s.base.ResultBySeq(op.Seq)
		if old != nil {
			cp := *old
			cp.CacheHit = true
			merged.Results = append(merged.Results, cp)
		}
	}

	for _, ev := range s.base.Events {
		if ev.ResourceID != resourceID {
			merged.Events = append(merged.Events, ev)
		}
	}
	merged.Events = append(merged.Events, next.Events...)
	sort.SliceStable(merged.Events, func(i, j int) bool {
		a, b := merged.Events[i], merged.Events[j]
		if a.AtMS != b.AtMS {
			return a.AtMS < b.AtMS
		}
		if a.ResourceID != b.ResourceID {
			return a.ResourceID < b.ResourceID
		}
		if a.HolderID != b.HolderID {
			return a.HolderID < b.HolderID
		}
		rank := map[string]int{"lease_invalidated": 0, "resource_released": 1}
		if rank[a.Type] != rank[b.Type] {
			return rank[a.Type] < rank[b.Type]
		}
		return a.Type < b.Type
	})
	for i := range merged.Events {
		merged.Events[i].Order = i + 1
	}

	newStates := map[string]HolderState{}
	for _, st := range next.HolderStates {
		newStates[st.HolderID] = st
	}
	for _, h := range s.project.Holders {
		if h.ResourceID == resourceID {
			merged.HolderStates = append(merged.HolderStates, newStates[h.ID])
		} else {
			for _, st := range s.base.HolderStates {
				if st.HolderID == h.ID {
					merged.HolderStates = append(merged.HolderStates, st)
					break
				}
			}
		}
	}
	return merged
}
