// Package incremental implements dependency-aware cache invalidation.
package incremental

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"

	"quota-cluster/internal/bizerror"
	"quota-cluster/internal/engine"
	"quota-cluster/internal/model"
)

// Provenance explains whether one tick was reused or recomputed and why.
type Provenance struct {
	Tick         int64    `json:"tick"`
	Status       string   `json:"status"`
	Dependencies []string `json:"dependencies"`
	Source       string   `json:"source"`
	CacheDigest  string   `json:"cache_digest,omitempty"`
}

// Result is the output of one incremental edit.
type Result struct {
	Edit            model.Edit    `json:"edit"`
	StartTick       int64         `json:"start_tick"`
	Dependencies    []string      `json:"dependencies"`
	Provenance      []Provenance  `json:"provenance"`
	Report          *model.Report `json:"report"`
	RecomputedTicks int           `json:"recomputed_ticks"`
	ReusedTicks     int           `json:"reused_ticks"`
	FullEqual       bool          `json:"full_equal"`
}

// Service keeps the base run and restorable per-tick state cache.
type Service struct {
	project *model.Project
	opts    engine.Options
	base    *engine.RunResult
}

func NewService(project *model.Project, opts engine.Options) (*Service, error) {
	run, err := engine.RunWithStates(project, opts)
	if err != nil {
		return nil, err
	}
	return &Service{project: project, opts: opts, base: run}, nil
}

// Apply invalidates the dependency suffix for edit and compares it with a full
// recomputation.
func (s *Service) Apply(edit model.Edit) (*Result, error) {
	edited, err := CloneProject(s.project)
	if err != nil {
		return nil, err
	}
	dependencies := []string{}
	sourceTarget := edit.TargetID

	switch edit.Kind {
	case model.EditNodeCapacity:
		if !nodeExists(edited, edit.TargetID) {
			return nil, bizerror.New(bizerror.CodeReferenceMissing, "/edit/target_id", "node %q does not exist", edit.TargetID)
		}
		if edit.Value <= 0 {
			return nil, bizerror.New(bizerror.CodeValueOutOfRange, "/edit/value", "node capacity must be positive")
		}
		dependencies = append(dependencies, "node:"+edit.TargetID)
	case model.EditTenantQuota, model.EditTenantGuarantee:
		if !tenantExists(edited, edit.TargetID) {
			return nil, bizerror.New(bizerror.CodeReferenceMissing, "/edit/target_id", "tenant %q does not exist", edit.TargetID)
		}
		if edit.Value < 0 {
			return nil, bizerror.New(bizerror.CodeValueOutOfRange, "/edit/value", "quota value cannot be negative")
		}
		dependencies = append(dependencies, "tenant:"+edit.TargetID)
	case model.EditWeightEvent:
		found := false
		for i := range edited.Events {
			ev := &edited.Events[i]
			if ev.Kind == model.EventWeight && (edit.EventSequence == 0 || ev.Sequence == edit.EventSequence) && (edit.TargetID == "" || ev.TenantID == edit.TargetID) {
				ev.Weight = edit.Value
				edit.EventSequence = ev.Sequence
				edit.AtTick = ev.Tick
				sourceTarget = ev.TenantID
				found = true
				break
			}
		}
		if !found {
			return nil, bizerror.New(bizerror.CodeReferenceMissing, "/edit/event_sequence", "weight event not found")
		}
		if edit.Value <= 0 {
			return nil, bizerror.New(bizerror.CodeValueOutOfRange, "/edit/value", "weight must be positive")
		}
		dependencies = append(dependencies, "tenant:"+sourceTarget, fmt.Sprintf("event:%d", edit.EventSequence))
	default:
		return nil, bizerror.New(bizerror.CodeFieldInvalidType, "/edit/kind", "unsupported edit kind %q", edit.Kind)
	}
	if edit.AtTick < 0 || edit.AtTick > edited.LogicalTicks {
		return nil, bizerror.New(bizerror.CodeValueOutOfRange, "/edit/at_tick", "edit tick outside logical range")
	}

	full, err := engine.Run(edited, engine.Options{Workers: s.opts.Workers, BatchSize: s.opts.BatchSize, Edits: []model.Edit{edit}})
	if err != nil {
		return nil, err
	}

	var baseState *engine.State
	if edit.AtTick > 0 {
		baseState = s.base.States[edit.AtTick-1]
	}
	suffix, err := engine.RecomputeSuffix(edited, engine.Options{Workers: s.opts.Workers, BatchSize: s.opts.BatchSize, Edits: []model.Edit{edit}}, edit.AtTick, baseState)
	if err != nil {
		return nil, err
	}

	hybrid := &model.Report{
		ProjectID: edited.ProjectID, Seed: edited.Seed, FinalTick: edited.LogicalTicks,
		WorkerBudget: suffix.Report.WorkerBudget, BatchSize: suffix.Report.BatchSize,
	}
	for _, tick := range s.base.Report.Ticks {
		if tick.Tick < edit.AtTick {
			hybrid.Ticks = append(hybrid.Ticks, tick)
		}
	}
	hybrid.Ticks = append(hybrid.Ticks, suffix.Report.Ticks...)
	for _, d := range s.base.Report.Decisions {
		if d.Tick < edit.AtTick {
			hybrid.Decisions = append(hybrid.Decisions, d)
		}
	}
	hybrid.Decisions = append(hybrid.Decisions, suffix.Report.Decisions...)

	provenance := []Provenance{}
	reused, recomputed := 0, 0
	source := fmt.Sprintf("invalidation:%s:%s@tick-%d", edit.Kind, sourceTarget, edit.AtTick)
	for _, tick := range hybrid.Ticks {
		p := Provenance{Tick: tick.Tick, Dependencies: append([]string(nil), dependencies...)}
		if tick.Tick < edit.AtTick {
			p.Status = "reused"
			p.Source = "cache:base-run"
			p.CacheDigest = Digest(tick)
			reused++
		} else {
			p.Status = "recomputed"
			p.Source = source
			recomputed++
		}
		provenance = append(provenance, p)
	}

	equal := reflect.DeepEqual(hybrid, full)
	result := &Result{
		Edit: edit, StartTick: edit.AtTick, Dependencies: dependencies,
		Provenance: provenance, Report: hybrid, RecomputedTicks: recomputed,
		ReusedTicks: reused, FullEqual: equal,
	}
	if !equal {
		return result, bizerror.New(bizerror.CodeToleranceExceeded, "/incremental/equivalence", "incremental report differs from full recomputation")
	}
	return result, nil
}

// CloneProject deep-copies a project through its deterministic JSON form.
func CloneProject(p *model.Project) (*model.Project, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var out model.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Digest returns a stable short digest of a cached tick report.
func Digest(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

func nodeExists(p *model.Project, id string) bool {
	for _, n := range p.Nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

func tenantExists(p *model.Project, id string) bool {
	for _, t := range p.Tenants {
		if t.ID == id {
			return true
		}
	}
	return false
}
