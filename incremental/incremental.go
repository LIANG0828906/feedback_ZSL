package incremental

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"alertkernel/engine"
	"alertkernel/errs"
	"alertkernel/model"
	"alertkernel/schema"
)

const (
	RulePredicate     = "rule.predicate"
	RuleForDuration   = "rule.for_duration"
	RuleKeepDuration  = "rule.keep_duration"
	RuleRewrite       = "rule.rewrite"
	MetricAppend      = "metric.append"
	MetricOverwrite   = "metric.overwrite"
	SilenceAdd        = "silence.add"
	SilenceDelete     = "silence.delete"
	SuppressionAdd    = "suppression.add"
	SuppressionDelete = "suppression.delete"
	InhibitionAdd     = "inhibition.add"
)

type Edit struct {
	Kind              string                   `json:"kind"`
	Target            string                   `json:"target"`
	Threshold         *int64                   `json:"threshold,omitempty"`
	Operator          string                   `json:"operator,omitempty"`
	ValueMS           *int64                   `json:"value_ms,omitempty"`
	Rewrite           map[string]string        `json:"rewrite,omitempty"`
	Sample            *model.Sample            `json:"sample,omitempty"`
	Silence           *model.SilenceWindow     `json:"silence,omitempty"`
	SuppressionWindow *model.SuppressionWindow `json:"suppression_window,omitempty"`
	Inhibition        *model.InhibitionRule    `json:"inhibition,omitempty"`
}

type Invalidation struct {
	EditKind           string   `json:"edit_kind"`
	Target             string   `json:"target"`
	DirectFromT        int      `json:"direct_from_t"`
	ClosureFromT       int      `json:"closure_from_t"`
	ClosureToT         int      `json:"closure_to_t"`
	BackwardLookbackMS int64    `json:"backward_lookback_ms"`
	ForwardKeepMS      int64    `json:"forward_keep_ms"`
	RecomputedTimes    []int    `json:"recomputed_times"`
	ReusedTimes        []int    `json:"reused_times"`
	DependencySources  []string `json:"dependency_sources"`
	ReuseBasis         string   `json:"reuse_basis"`
	ClosureRationale   string   `json:"closure_rationale"`
}

type Outcome struct {
	Report       *engine.RunReport `json:"report"`
	Invalidation Invalidation      `json:"invalidation"`
}

type Manager struct {
	Project *model.Project
	Events  model.EventBundle
	Report  *engine.RunReport
}

func NewManager(ctx context.Context, p *model.Project, events model.EventBundle) (*Manager, error) {
	report, err := engine.RunAll(ctx, p, events)
	if err != nil {
		return nil, err
	}
	return &Manager{Project: p, Events: events, Report: report}, nil
}

func (m *Manager) Apply(ctx context.Context, edit Edit) (*Outcome, error) {
	project := cloneJSON(m.Project)
	events := cloneJSON(m.Events)
	inv := Invalidation{EditKind: edit.Kind, Target: edit.Target, ClosureToT: project.Clock.Steps - 1}
	inv.ReuseBasis = "prefix TimeResult.States + BasisSeqAfter is reused; suffix is recomputed"
	inv.ClosureRationale = "for/keep_firing_for state carry, suppression joins, route and notification dedupe chain are suffix-closed"

	var err error
	switch edit.Kind {
	case RulePredicate:
		err = m.editRulePredicate(project, edit, &inv)
	case RuleForDuration, RuleKeepDuration:
		err = m.editDuration(project, edit, &inv)
	case RuleRewrite:
		err = m.editRewrite(project, edit, &inv)
	case MetricAppend, MetricOverwrite:
		err = m.editMetric(project, edit, &inv)
	case SilenceAdd:
		err = m.addSilence(project, edit, &inv)
	case SilenceDelete:
		err = m.deleteSilence(project, edit, &inv)
	case SuppressionAdd:
		err = m.addSuppression(&events, edit, &inv)
	case SuppressionDelete:
		err = m.deleteSuppression(&events, edit, &inv)
	case InhibitionAdd:
		err = m.addInhibition(project, edit, &inv)
	default:
		err = errs.New(errs.EEditInvalid, "edit.kind", "unsupported edit kind %q", edit.Kind)
	}
	if err != nil {
		return nil, err
	}

	maxFor, maxKeep := int64(0), int64(0)
	for _, rule := range project.Rules {
		if rule.ForMS > maxFor {
			maxFor = rule.ForMS
		}
		if rule.KeepFiringMS > maxKeep {
			maxKeep = rule.KeepFiringMS
		}
	}
	inv.BackwardLookbackMS = maxFor
	inv.ForwardKeepMS = maxKeep
	if inv.DirectFromT < 0 {
		inv.DirectFromT = 0
	}
	if inv.DirectFromT >= project.Clock.Steps {
		return nil, errs.New(errs.EEditInvalid, "edit.time", "invalidation start %d is outside 0..%d", inv.DirectFromT, project.Clock.Steps-1)
	}
	inv.ClosureFromT = inv.DirectFromT
	for t := 0; t < inv.ClosureFromT; t++ {
		inv.ReusedTimes = append(inv.ReusedTimes, t)
	}
	for t := inv.ClosureFromT; t <= inv.ClosureToT; t++ {
		inv.RecomputedTimes = append(inv.RecomputedTimes, t)
	}

	project.RuleVersion = ""
	if err := schema.Canonicalize(project); err != nil {
		return nil, err
	}

	var initial []engine.State
	basisSeq := 0
	times := []engine.TimeResult{}
	if inv.ClosureFromT > 0 {
		previous := m.Report.Times[inv.ClosureFromT-1]
		initial = previous.States
		basisSeq = previous.BasisSeqAfter
		times = append(times, m.Report.Times[:inv.ClosureFromT]...)
	}
	runner, err := engine.NewRunner(project, events, initial, basisSeq)
	if err != nil {
		return nil, err
	}
	for _, t := range inv.RecomputedTimes {
		frame, err := runner.Step(ctx, t)
		if err != nil {
			return nil, err
		}
		frame.InvalidationSources = append([]string{}, inv.DependencySources...)
		times = append(times, *frame)
	}
	report := &engine.RunReport{ProjectID: project.ProjectID, RuleVersion: project.RuleVersion, Times: times}
	outcome := &Outcome{Report: report, Invalidation: inv}
	m.Project = project
	m.Events = events
	m.Report = report
	return outcome, nil
}

func (m *Manager) FullRecompute(ctx context.Context) (*engine.RunReport, error) {
	return engine.RunAll(ctx, m.Project, m.Events)
}

type Comparison struct {
	Equal          bool     `json:"equal"`
	DifferentTimes []int    `json:"different_times,omitempty"`
	Differences    []string `json:"differences,omitempty"`
}

func CompareReports(want, got *engine.RunReport) Comparison {
	w := cloneJSON(want)
	g := cloneJSON(got)
	sanitize(w)
	sanitize(g)
	result := Comparison{Equal: reflect.DeepEqual(w, g)}
	if result.Equal {
		return result
	}
	maxLen := len(w.Times)
	if len(g.Times) > maxLen {
		maxLen = len(g.Times)
	}
	for t := 0; t < maxLen; t++ {
		if t >= len(w.Times) || t >= len(g.Times) {
			result.DifferentTimes = append(result.DifferentTimes, t)
			result.Differences = append(result.Differences, fmt.Sprintf("t=%d length mismatch want=%d got=%d", t, len(w.Times), len(g.Times)))
			continue
		}
		if !reflect.DeepEqual(w.Times[t], g.Times[t]) {
			result.DifferentTimes = append(result.DifferentTimes, t)
			result.Differences = append(result.Differences, fmt.Sprintf("t=%d semantic result differs", t))
		}
	}
	if len(result.Differences) == 0 {
		result.Differences = append(result.Differences, "report metadata differs")
	}
	return result
}

func sanitize(r *engine.RunReport) {
	if r == nil {
		return
	}
	for i := range r.Times {
		r.Times[i].InvalidationSources = nil
		for j := range r.Times[i].Scheduling.Rules {
			r.Times[i].Scheduling.Rules[j].DurationNS = 0
		}
	}
}

func cloneJSON[T any](in T) T {
	data, err := json.Marshal(in)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return out
}

func (m *Manager) editRulePredicate(p *model.Project, edit Edit, inv *Invalidation) error {
	rule := findRule(p, edit.Target)
	if rule == nil {
		return errs.New(errs.EEditInvalid, "edit.target", "rule %q not found", edit.Target)
	}
	if edit.Threshold != nil {
		rule.Predicate.Threshold = *edit.Threshold
	}
	if edit.Operator != "" {
		rule.Predicate.Op = edit.Operator
	}
	inv.DirectFromT = 0
	inv.DependencySources = []string{
		"rule:" + rule.ID + ".predicate",
		"evaluator:fixed_point_compare",
		"state_chain:for/keep_firing_for",
		"adjudication:suppression/silence",
		"notifications:dedup_and_route",
	}
	return nil
}

func (m *Manager) editDuration(p *model.Project, edit Edit, inv *Invalidation) error {
	rule := findRule(p, edit.Target)
	if rule == nil {
		return errs.New(errs.EEditInvalid, "edit.target", "rule %q not found", edit.Target)
	}
	if edit.ValueMS == nil {
		return errs.New(errs.EEditInvalid, "edit.value_ms", "duration edit requires value_ms")
	}
	if edit.Kind == RuleForDuration {
		rule.ForMS = *edit.ValueMS
	} else {
		rule.KeepFiringMS = *edit.ValueMS
	}
	inv.DirectFromT = 0
	inv.DependencySources = []string{"rule:" + rule.ID + "." + edit.Kind, "state_chain:duration_boundary", "notifications:firing_transition"}
	return nil
}

func (m *Manager) editRewrite(p *model.Project, edit Edit, inv *Invalidation) error {
	rule := findRule(p, edit.Target)
	if rule == nil {
		return errs.New(errs.EEditInvalid, "edit.target", "rule %q not found", edit.Target)
	}
	if edit.Rewrite == nil {
		return errs.New(errs.EEditInvalid, "edit.rewrite", "rewrite edit requires label map")
	}
	rule.LabelRewrite = edit.Rewrite
	inv.DirectFromT = 0
	inv.DependencySources = []string{"rule:" + rule.ID + ".label_rewrite", "instance_identity:fingerprint", "route/group/dedup labels", "matcher targets"}
	return nil
}

func (m *Manager) editMetric(p *model.Project, edit Edit, inv *Invalidation) error {
	series := findSeries(p, edit.Target)
	if series == nil {
		return errs.New(errs.EEditInvalid, "edit.target", "series %q not found", edit.Target)
	}
	if edit.Sample == nil {
		return errs.New(errs.EEditInvalid, "edit.sample", "metric edit requires sample")
	}
	sample := *edit.Sample
	if sample.AtMS == 0 {
		sample.AtMS = p.Clock.TimeMS(sample.Index)
	}
	inv.DirectFromT = sample.Index
	if edit.Kind == MetricAppend {
		for _, existing := range series.Samples {
			if existing.Index == sample.Index {
				return errs.New(errs.EEditInvalid, "edit.sample.index", "sample index %d already exists; use metric.overwrite", sample.Index)
			}
		}
		series.Samples = append(series.Samples, sample)
	} else {
		replaced := false
		for i := range series.Samples {
			if series.Samples[i].Index == sample.Index {
				series.Samples[i] = sample
				replaced = true
				break
			}
		}
		if !replaced {
			return errs.New(errs.EEditInvalid, "edit.sample.index", "sample index %d not found; use metric.append", sample.Index)
		}
	}
	inv.DependencySources = []string{"metric:" + series.ID + "@t" + fmt.Sprint(sample.Index), "latest_sample/freshness_window"}
	for _, rule := range p.Rules {
		if rule.Predicate.Metric == series.Metric {
			inv.DependencySources = append(inv.DependencySources, "predicate:"+rule.ID, "state_chain:"+rule.ID)
		}
	}
	inv.DependencySources = append(inv.DependencySources, "adjudication:downstream_visibility", "notifications:dedup_chain")
	return nil
}

func (m *Manager) addSilence(p *model.Project, edit Edit, inv *Invalidation) error {
	if edit.Silence == nil || edit.Silence.ID == "" {
		return errs.New(errs.EEditInvalid, "edit.silence", "silence.add requires silence.id/window")
	}
	p.Silences = append(p.Silences, *edit.Silence)
	inv.DirectFromT = edit.Silence.StartT
	inv.DependencySources = []string{"silence:" + edit.Silence.ID, "label_selector", "visible_set", "notification_route"}
	return nil
}

func (m *Manager) deleteSilence(p *model.Project, edit Edit, inv *Invalidation) error {
	for i, silence := range p.Silences {
		if silence.ID == edit.Target {
			inv.DirectFromT = silence.StartT
			inv.DependencySources = []string{"silence.delete:" + silence.ID, "visible_set", "notification_route"}
			p.Silences = append(p.Silences[:i], p.Silences[i+1:]...)
			return nil
		}
	}
	return errs.New(errs.EEditInvalid, "edit.target", "silence %q not found", edit.Target)
}

func (m *Manager) addSuppression(e *model.EventBundle, edit Edit, inv *Invalidation) error {
	if edit.SuppressionWindow == nil || edit.SuppressionWindow.ID == "" {
		return errs.New(errs.EEditInvalid, "edit.suppression_window", "suppression.add requires window")
	}
	e.SuppressionWindows = append(e.SuppressionWindows, *edit.SuppressionWindow)
	inv.DirectFromT = edit.SuppressionWindow.StartT
	inv.DependencySources = []string{"suppression_event:" + edit.SuppressionWindow.ID, "source_target_matchers", "visible_set"}
	return nil
}

func (m *Manager) deleteSuppression(e *model.EventBundle, edit Edit, inv *Invalidation) error {
	for i, window := range e.SuppressionWindows {
		if window.ID == edit.Target {
			inv.DirectFromT = window.StartT
			inv.DependencySources = []string{"suppression.delete:" + window.ID, "visible_set"}
			e.SuppressionWindows = append(e.SuppressionWindows[:i], e.SuppressionWindows[i+1:]...)
			return nil
		}
	}
	return errs.New(errs.EEditInvalid, "edit.target", "suppression event %q not found", edit.Target)
}

func (m *Manager) addInhibition(p *model.Project, edit Edit, inv *Invalidation) error {
	if edit.Inhibition == nil || edit.Inhibition.ID == "" {
		return errs.New(errs.EEditInvalid, "edit.inhibition", "inhibition.add requires relation")
	}
	p.Inhibitions = append(p.Inhibitions, *edit.Inhibition)
	inv.DirectFromT = 0
	inv.DependencySources = []string{"inhibition:" + edit.Inhibition.ID, "source_target_label_match", "visible_set"}
	return nil
}

func findRule(p *model.Project, id string) *model.Rule {
	for _, rule := range p.Rules {
		if rule.ID == id {
			return rule
		}
	}
	return nil
}

func findSeries(p *model.Project, id string) *model.Series {
	for _, series := range p.Series {
		if series.ID == id {
			return series
		}
	}
	return nil
}
