package incremental

import (
	"fmt"
	"sort"

	"chaosdiag/engine"
	"chaosdiag/model"
	"chaosdiag/reconcile"
)

type Mutation struct {
	Kind          string          `json:"kind"`
	InstructionID string          `json:"instruction_id,omitempty"`
	EdgeID        string          `json:"edge_id,omitempty"`
	DelayMS       *int64          `json:"delay_ms,omitempty"`
	TimeoutMS     *int64          `json:"timeout_ms,omitempty"`
	AmplitudeMS   *int64          `json:"amplitude_ms,omitempty"`
	StartTick     *int            `json:"start_tick,omitempty"`
	EndTick       *int            `json:"end_tick,omitempty"`
	Selector      *model.Selector `json:"selector,omitempty"`
	EdgeLatencyMS *int64          `json:"edge_latency_ms,omitempty"`
}

type InvalidationSource struct {
	ObjectID   string         `json:"object_id"`
	ObjectKind string         `json:"object_kind"`
	Change     string         `json:"change"`
	Field      string         `json:"field,omitempty"`
	Reason     string         `json:"reason"`
	Interval   model.Interval `json:"interval,omitempty"`
}

type CacheDecision struct {
	RequestID string               `json:"request_id"`
	CacheKey  string               `json:"cache_key"`
	Reused    bool                 `json:"reused"`
	Reason    string               `json:"reason"`
	Sources   []InvalidationSource `json:"sources,omitempty"`
}

type ClosureNode struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

type ClosureEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type Closure struct {
	ChangedObject    string           `json:"changed_object"`
	Nodes            []ClosureNode    `json:"nodes"`
	Edges            []ClosureEdge    `json:"edges"`
	RequestIDs       []string         `json:"request_ids"`
	CacheKeys        []string         `json:"cache_keys"`
	Intervals        []model.Interval `json:"intervals"`
	FirstAffectedHop map[string]int   `json:"first_affected_hop"`
}

type RequestDiff struct {
	RequestID      string   `json:"request_id"`
	Changed        bool     `json:"changed"`
	BeforeStatus   string   `json:"before_status"`
	AfterStatus    string   `json:"after_status"`
	BeforeTotalMS  int64    `json:"before_total_ms"`
	AfterTotalMS   int64    `json:"after_total_ms"`
	TotalDeltaMS   int64    `json:"total_delta_ms"`
	BeforeHopCount int      `json:"before_hop_count"`
	AfterHopCount  int      `json:"after_hop_count"`
	HitAdded       []string `json:"hit_added,omitempty"`
	HitRemoved     []string `json:"hit_removed,omitempty"`
}

type Diff struct {
	MutationKind string        `json:"mutation_kind"`
	ChangedCount int           `json:"changed_count"`
	Requests     []RequestDiff `json:"requests"`
}

type Result struct {
	Report         *model.Report   `json:"report"`
	Diff           *Diff           `json:"diff"`
	Closure        *Closure        `json:"closure"`
	CacheDecisions []CacheDecision `json:"cache_decisions"`
	Recomputed     []string        `json:"recomputed"`
	Reused         []string        `json:"reused"`
}

type cacheEntry struct {
	key     string
	result  model.RequestResult
	verdict model.Verdict
}

type Engine struct {
	project *model.Project
	report  *model.Report
	cache   map[string]cacheEntry
}

func NewEngine(p *model.Project) (*Engine, error) {
	clone, err := model.CloneProject(p)
	if err != nil {
		return nil, err
	}
	if err := model.ValidateProject(clone); err != nil {
		return nil, err
	}
	e := &Engine{project: clone, cache: map[string]cacheEntry{}}
	e.report = engine.Run(clone)
	e.report.Verdicts = reconcile.ReconcileAll(clone, e.report.Results)
	for i, rr := range e.report.Results {
		v := e.report.Verdicts[i]
		e.cache[rr.RequestID] = cacheEntry{key: cacheKey(clone.ProjectVersion, rr), result: rr, verdict: v}
	}
	return e, nil
}

func (e *Engine) Project() *model.Project { return e.project }
func (e *Engine) Report() *model.Report   { return e.report }

func FullReport(p *model.Project) *model.Report {
	r := engine.Run(p)
	r.Verdicts = reconcile.ReconcileAll(p, r.Results)
	return r
}

func (e *Engine) Apply(mut Mutation) (*Result, error) {
	oldProject, err := model.CloneProject(e.project)
	if err != nil {
		return nil, err
	}
	newProject, err := model.CloneProject(e.project)
	if err != nil {
		return nil, err
	}
	oldSchedules := engine.Schedule(oldProject)

	changedObject, field, change, err := applyMutation(newProject, mut)
	if err != nil {
		return nil, err
	}
	if err := model.ValidateProject(newProject); err != nil {
		return nil, err
	}
	newSchedules := engine.Schedule(newProject)
	closure, sources := buildClosure(oldProject, newProject, mut, changedObject, field, change, oldSchedules, newSchedules)

	oldResultByID := map[string]model.RequestResult{}
	for _, rr := range e.report.Results {
		oldResultByID[rr.RequestID] = rr
	}
	newActive := engine.ActiveMap(newSchedules)
	affected := map[string]bool{}
	for id := range sources {
		affected[id] = true
	}

	decisions := make([]CacheDecision, 0, len(newProject.Requests))
	recomputed := []string{}
	reused := []string{}
	newResults := make([]model.RequestResult, 0, len(newProject.Requests))
	newVerdicts := make([]model.Verdict, 0, len(newProject.Requests))

	requests := append([]model.Request{}, newProject.Requests...)
	sort.SliceStable(requests, func(i, j int) bool { return requests[i].Sequence < requests[j].Sequence })
	for _, req := range requests {
		key := cacheKey(newProject.ProjectVersion, model.RequestResult{RequestID: req.ID, Tick: req.Tick})
		if !affected[req.ID] {
			entry := e.cache[req.ID]
			newResults = append(newResults, entry.result)
			newVerdicts = append(newVerdicts, entry.verdict)
			reused = append(reused, req.ID)
			decisions = append(decisions, CacheDecision{
				RequestID: req.ID, CacheKey: entry.key, Reused: true,
				Reason: "request path/tick and scheduling active set do not intersect invalidation closure",
			})
			continue
		}
		rr := engine.SimulateRequest(newProject, req, newActive[req.Tick])
		v := reconcile.ReconcileOne(newProject, req, rr)
		entry := cacheEntry{key: key, result: rr, verdict: v}
		e.cache[req.ID] = entry
		newResults = append(newResults, rr)
		newVerdicts = append(newVerdicts, v)
		recomputed = append(recomputed, req.ID)
		decisions = append(decisions, CacheDecision{
			RequestID: req.ID, CacheKey: key, Reused: false,
			Reason: "invalidated by explicit propagation closure", Sources: sources[req.ID],
		})
	}

	newReport := &model.Report{
		ProjectID: newProject.ProjectID, ProjectVersion: newProject.ProjectVersion, Seed: newProject.Seed,
		Ticks: ticksFromSchedules(newSchedules), Schedules: newSchedules, Results: newResults, Verdicts: newVerdicts,
	}
	diff := buildDiff(mut.Kind, e.report, newReport)
	result := &Result{
		Report: newReport, Diff: diff, Closure: closure, CacheDecisions: decisions,
		Recomputed: recomputed, Reused: reused,
	}

	e.project = newProject
	e.report = newReport
	return result, nil
}

func cacheKey(version string, rr model.RequestResult) string {
	return fmt.Sprintf("%s:%s@tick-%d", version, rr.RequestID, rr.Tick)
}

func ticksFromSchedules(in []model.TickSchedule) []int {
	out := make([]int, 0, len(in))
	for _, s := range in {
		out = append(out, s.Tick)
	}
	return out
}

func applyMutation(p *model.Project, mut Mutation) (objectID, field, change string, err error) {
	switch mut.Kind {
	case "instruction_amplitude":
		ins := engine.FindInstruction(p, mut.InstructionID)
		if ins == nil {
			return "", "", "", model.NewError(model.CodeMutationTargetMissing, "instruction_id", "instruction %q not found", mut.InstructionID)
		}
		objectID = ins.ID
		field = "amplitude"
		change = "amplitude_changed"
		if mut.AmplitudeMS != nil {
			switch ins.Kind {
			case model.KindTimeout:
				ins.TimeoutMS = *mut.AmplitudeMS
				field = "timeout_ms"
			default:
				ins.DelayMS = *mut.AmplitudeMS
				field = "delay_ms"
			}
			ins.BudgetCostMS = *mut.AmplitudeMS
		}
		if mut.DelayMS != nil {
			ins.DelayMS = *mut.DelayMS
			ins.BudgetCostMS = *mut.DelayMS
			field = "delay_ms"
		}
		if mut.TimeoutMS != nil {
			ins.TimeoutMS = *mut.TimeoutMS
			ins.BudgetCostMS = *mut.TimeoutMS
			field = "timeout_ms"
		}
		return objectID, field, change, nil
	case "instruction_window":
		ins := engine.FindInstruction(p, mut.InstructionID)
		if ins == nil {
			return "", "", "", model.NewError(model.CodeMutationTargetMissing, "instruction_id", "instruction %q not found", mut.InstructionID)
		}
		if mut.StartTick != nil {
			ins.StartTick = *mut.StartTick
		}
		if mut.EndTick != nil {
			ins.EndTick = *mut.EndTick
		}
		return ins.ID, "effective_window", "window_changed", nil
	case "instruction_selector":
		ins := engine.FindInstruction(p, mut.InstructionID)
		if ins == nil {
			return "", "", "", model.NewError(model.CodeMutationTargetMissing, "instruction_id", "instruction %q not found", mut.InstructionID)
		}
		if mut.Selector == nil {
			return "", "", "", model.NewError(model.CodeFieldRequired, "selector", "selector is required for selector mutation")
		}
		sel := *mut.Selector
		if sel.ID == "" {
			sel.ID = ins.SelectorID
		}
		ins.Selector = sel
		return ins.ID, "selector", "selector_changed", nil
	case "edge_latency", "dependency_edge":
		edge, ok := p.EdgeByID(mut.EdgeID)
		if !ok {
			return "", "", "", model.NewError(model.CodeMutationTargetMissing, "edge_id", "edge %q not found", mut.EdgeID)
		}
		if mut.EdgeLatencyMS != nil {
			edge.LatencyMS = *mut.EdgeLatencyMS
			for i := range p.Edges {
				if p.Edges[i].ID == edge.ID {
					p.Edges[i].LatencyMS = edge.LatencyMS
				}
			}
		}
		return edge.ID, "edge_latency_ms", "edge_changed", nil
	default:
		return "", "", "", model.NewError(model.CodeFieldInvalid, "kind", "unsupported mutation kind %q", mut.Kind)
	}
}
