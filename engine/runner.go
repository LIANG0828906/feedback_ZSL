package engine

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"

	"alertkernel/errs"
	"alertkernel/model"
	"alertkernel/schema"
)

type Runner struct {
	project     *model.Project
	events      model.EventBundle
	rulesByID   map[string]*model.Rule
	states      map[string]*State
	basisSeq    int
	silences    []model.SilenceWindow
	inhibitions []model.InhibitionRule
}

func NewRunner(project *model.Project, events model.EventBundle, initial []State, basisSeq int) (*Runner, error) {
	if project == nil {
		return nil, errs.New(errs.EValidation, "project", "nil project")
	}
	if err := schema.ValidateBudget(project.Budget); err != nil {
		return nil, err
	}
	r := &Runner{
		project: project, events: events,
		rulesByID: map[string]*model.Rule{},
		states:    map[string]*State{},
		basisSeq:  basisSeq,
		silences:  append(append([]model.SilenceWindow{}, project.Silences...), events.Silences...),
	}
	for _, rule := range project.Rules {
		r.rulesByID[rule.ID] = rule
		if rule.Receiver != "" && !project.ReceiverNames()[rule.Receiver] {
			return nil, errs.New(errs.EReceiverNotFound, "rules."+rule.ID+".receiver", "receiver %q is not declared", rule.Receiver)
		}
	}
	for _, st := range initial {
		copyState := st
		r.states[stateKey(st.RuleID, st.InstanceKey)] = &copyState
		if st.BasisNo > r.basisSeq {
			r.basisSeq = st.BasisNo
		}
	}
	r.inhibitions = append([]model.InhibitionRule{}, project.Inhibitions...)
	sort.SliceStable(r.inhibitions, func(i, j int) bool {
		if r.inhibitions[i].Priority != r.inhibitions[j].Priority {
			return r.inhibitions[i].Priority > r.inhibitions[j].Priority
		}
		return r.inhibitions[i].ID < r.inhibitions[j].ID
	})
	sort.SliceStable(r.silences, func(i, j int) bool {
		if r.silences[i].StartT != r.silences[j].StartT {
			return r.silences[i].StartT < r.silences[j].StartT
		}
		return r.silences[i].ID < r.silences[j].ID
	})
	return r, nil
}

func (r *Runner) BasisSeq() int { return r.basisSeq }

func (r *Runner) States() []State {
	out := make([]State, 0, len(r.states))
	for _, st := range r.states {
		out = append(out, *st)
	}
	sortStates(r.project, out)
	return out
}

func RunAll(ctx context.Context, project *model.Project, events model.EventBundle) (*RunReport, error) {
	runner, err := NewRunner(project, events, nil, 0)
	if err != nil {
		return nil, err
	}
	report := &RunReport{ProjectID: project.ProjectID, RuleVersion: project.RuleVersion}
	for t := 0; t < project.Clock.Steps; t++ {
		if err := ctx.Err(); err != nil {
			return report, errs.Wrap(errs.EInterrupted, "clock", err, "interrupted before logical step %d; last committed step is %d", t, t-1)
		}
		frame, err := runner.Step(ctx, t)
		if err != nil {
			return report, err
		}
		report.Times = append(report.Times, *frame)
	}
	return report, nil
}

func (r *Runner) Step(ctx context.Context, t int) (*TimeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, errs.Wrap(errs.EInterrupted, "clock", err, "interrupted before logical step %d", t)
	}
	observationsByRule, scheduling, err := r.evaluateAllRules(ctx, t)
	if err != nil {
		return nil, err
	}
	frame := &TimeResult{
		T: t, AtMS: r.project.Clock.TimeMS(t), Scheduling: scheduling,
	}

	for ruleIndex, rule := range r.project.Rules {
		observations := observationsByRule[ruleIndex]
		obsByInstance := map[string]Observation{}
		instanceKeys := map[string]bool{}
		for _, obs := range observations {
			obsByInstance[obs.InstanceKey] = obs
			instanceKeys[obs.InstanceKey] = true
			frame.Observations = append(frame.Observations, obs)
		}
		prefix := rule.ID + "\x00"
		for key, st := range r.states {
			if len(key) > len(prefix) && key[:len(prefix)] == prefix {
				instanceKeys[st.InstanceKey] = true
			}
		}
		keys := make([]string, 0, len(instanceKeys))
		for key := range instanceKeys {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		for _, instanceKey := range keys {
			mapKey := stateKey(rule.ID, instanceKey)
			st, exists := r.states[mapKey]
			if !exists {
				labels := model.Labels{}
				if obs, ok := obsByInstance[instanceKey]; ok {
					labels = obs.Labels.Clone()
				}
				st = &State{
					RuleID: rule.ID, InstanceKey: instanceKey, Labels: labels,
					State: StateInactive, EnteredT: t, EnteredAtMS: r.project.Clock.TimeMS(t),
				}
				r.states[mapKey] = st
			}
			obs, ok := obsByInstance[instanceKey]
			if !ok {
				obs = Observation{
					RuleID: rule.ID, Order: rule.Order, InstanceKey: instanceKey,
					Labels: st.Labels.Clone(), HasData: false, Matched: false, NoDataReason: "missing_metric",
				}
				frame.Observations = append(frame.Observations, obs)
			}
			r.transition(rule, st, obs, t)
			frame.States = append(frame.States, *st)
		}
	}

	frame.SuppressionDetails = r.adjudicate(t, frame.States)
	hidden := map[string]bool{}
	for _, decision := range frame.SuppressionDetails {
		if decision.PrimaryReason != "" {
			hidden[stateKey(decision.RuleID, decision.InstanceKey)] = true
		}
	}
	for _, st := range frame.States {
		if st.State == StateInactive || hidden[stateKey(st.RuleID, st.InstanceKey)] {
			continue
		}
		frame.Visible = append(frame.Visible, VisibleAlert{
			RuleID: st.RuleID, InstanceKey: st.InstanceKey, Labels: st.Labels.Clone(),
			State: st.State, Priority: r.rulesByID[st.RuleID].Priority,
		})
	}
	frame.Notifications, err = r.notifications(t, frame.States, hidden)
	if err != nil {
		return nil, err
	}
	frame.BasisSeqAfter = r.basisSeq
	sort.SliceStable(frame.Observations, func(i, j int) bool {
		if frame.Observations[i].Order != frame.Observations[j].Order {
			return frame.Observations[i].Order < frame.Observations[j].Order
		}
		return frame.Observations[i].InstanceKey < frame.Observations[j].InstanceKey
	})
	sortVisible(frame.Visible)
	return frame, nil
}

func (r *Runner) evaluateAllRules(ctx context.Context, t int) ([][]Observation, SchedulingReport, error) {
	_ = ctx
	rules := r.project.Rules
	out := make([][]Observation, len(rules))
	timings := make([]RuleScheduling, len(rules))
	errsByRule := make([]error, len(rules))
	workers := r.project.Budget.Workers
	if workers > len(rules) && len(rules) > 0 {
		workers = len(rules)
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				obs, timing, err := evaluateRule(r.project, rules[i], t)
				out[i] = obs
				timings[i] = timing
				errsByRule[i] = err
			}
		}()
	}
	for i := range rules {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	report := SchedulingReport{Workers: r.project.Budget.Workers, PerRuleBuffer: r.project.Budget.PerRuleBuffer, RuleCount: len(rules)}
	for i := range rules {
		if errsByRule[i] != nil {
			return nil, report, errsByRule[i]
		}
		report.Rules = append(report.Rules, timings[i])
		report.BackpressureEvents += timings[i].Backpressure
	}
	return out, report, nil
}

func (r *Runner) transition(rule *model.Rule, st *State, obs Observation, t int) {
	nowMS := r.project.Clock.TimeMS(t)
	st.Changed = false
	st.LastTransition = ""
	matched := obs.HasData && obs.Matched

	fire := func() {
		st.State = StateFiring
		st.EnteredT = t
		st.EnteredAtMS = nowMS
		no := r.nextBasis()
		st.BasisNo = no
		st.BasisID = fmt.Sprintf("B%04d:fire:%s:t%d", no, rule.ID, t)
		st.FireBasisNo = no
		st.FireBasisID = st.BasisID
		st.LastTransition = StateFiring
		st.Changed = true
	}
	enterPending := func() {
		st.State = StatePending
		st.EnteredT = t
		st.EnteredAtMS = nowMS
		no := r.nextBasis()
		st.BasisNo = no
		st.BasisID = fmt.Sprintf("B%04d:pending:%s:t%d", no, rule.ID, t)
		st.LastTransition = StatePending
		st.Changed = true
	}
	resolve := func() {
		st.State = StateInactive
		st.EnteredT = t
		st.EnteredAtMS = nowMS
		no := r.nextBasis()
		st.BasisNo = no
		st.BasisID = fmt.Sprintf("B%04d:resolve:%s:t%d", no, rule.ID, t)
		st.ResolveBasisNo = no
		st.ResolveBasisID = st.BasisID
		st.LastTransition = StateInactive
		st.Changed = true
		st.LastMatchT = nil
		st.PendingSinceT = nil
	}

	if matched {
		matchT := t
		st.LastMatchT = &matchT
		switch st.State {
		case StateInactive:
			pendingT := t
			st.PendingSinceT = &pendingT
			if rule.ForMS == 0 {
				fire()
			} else {
				enterPending()
			}
		case StatePending:
			if st.PendingSinceT == nil {
				pendingT := t
				st.PendingSinceT = &pendingT
			}
			elapsed := nowMS - r.project.Clock.TimeMS(*st.PendingSinceT)
			if elapsed >= rule.ForMS {
				fire()
			}
		case StateFiring:
		}
	} else {
		switch st.State {
		case StatePending:
			resolve()
		case StateFiring:
			if st.LastMatchT == nil || nowMS-r.project.Clock.TimeMS(*st.LastMatchT) > rule.KeepFiringMS {
				resolve()
			}
		}
	}
	st.DurationMS = nowMS - st.EnteredAtMS
}

func (r *Runner) nextBasis() int {
	r.basisSeq++
	return r.basisSeq
}

func (r *Runner) adjudicate(t int, states []State) []SuppressionDecision {
	decisions := []SuppressionDecision{}
	stateByKey := map[string]State{}
	for _, st := range states {
		stateByKey[stateKey(st.RuleID, st.InstanceKey)] = st
	}
	for _, target := range states {
		if target.State == StateInactive {
			continue
		}
		decision := SuppressionDecision{
			RuleID: target.RuleID, InstanceKey: target.InstanceKey, Labels: target.Labels.Clone(),
			StartT: t, EndT: t, StartAtMS: r.project.Clock.TimeMS(t), EndAtMS: r.project.Clock.TimeMS(t),
		}

		for _, silence := range r.silences {
			if t < silence.StartT || t > silence.EndT || !matchAll(silence.Matchers, target.Labels) {
				continue
			}
			decision.Silenced = true
			decision.SilenceID = silence.ID
			decision.StartT = silence.StartT
			decision.EndT = silence.EndT
			break
		}

		for _, inhibition := range r.inhibitions {
			if !matchAll(inhibition.TargetMatchers, target.Labels) {
				continue
			}
			window, windowOK := r.activeSuppressionWindow(inhibition.ID, t)
			if r.hasWindowsForInhibition(inhibition.ID) && !windowOK {
				continue
			}
			source, ok := r.findSource(inhibition, target, states)
			if !ok {
				continue
			}
			decision.Suppressed = true
			decision.InhibitionID = inhibition.ID
			decision.SourceRuleID = source.RuleID
			decision.SourceInstanceKey = source.InstanceKey
			if windowOK {
				decision.SuppressionEventID = window.ID
				decision.StartT = window.StartT
				decision.EndT = window.EndT
				decision.Comment = window.Comment
			} else {
				decision.StartT = 0
				decision.EndT = r.project.Clock.Steps - 1
			}
			break
		}

		if !decision.Suppressed && !decision.Silenced {
			continue
		}
		for _, priority := range r.project.Policy.ReasonPriority {
			if priority == ReasonSuppressed && decision.Suppressed {
				decision.PrimaryReason = ReasonSuppressed
				break
			}
			if priority == ReasonSilenced && decision.Silenced {
				decision.PrimaryReason = ReasonSilenced
				break
			}
		}
		if decision.PrimaryReason == "" {
			if decision.Suppressed {
				decision.PrimaryReason = ReasonSuppressed
			} else {
				decision.PrimaryReason = ReasonSilenced
			}
		}
		decision.StartAtMS = r.project.Clock.TimeMS(decision.StartT)
		decision.EndAtMS = r.project.Clock.TimeMS(decision.EndT)
		decisions = append(decisions, decision)
	}
	sort.SliceStable(decisions, func(i, j int) bool {
		if r.project.RuleVersion != "" && decisions[i].RuleID != decisions[j].RuleID {
			return ruleOrder(r.project, decisions[i].RuleID) < ruleOrder(r.project, decisions[j].RuleID)
		}
		return decisions[i].InstanceKey < decisions[j].InstanceKey
	})
	return decisions
}

func (r *Runner) hasWindowsForInhibition(id string) bool {
	for _, w := range r.events.SuppressionWindows {
		if w.InhibitionID == id {
			return true
		}
	}
	return false
}

func (r *Runner) activeSuppressionWindow(inhibitionID string, t int) (model.SuppressionWindow, bool) {
	var candidates []model.SuppressionWindow
	for _, w := range r.events.SuppressionWindows {
		if w.InhibitionID == inhibitionID && t >= w.StartT && t <= w.EndT {
			candidates = append(candidates, w)
		}
	}
	if len(candidates) == 0 {
		return model.SuppressionWindow{}, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].StartT != candidates[j].StartT {
			return candidates[i].StartT < candidates[j].StartT
		}
		return candidates[i].ID < candidates[j].ID
	})
	return candidates[0], true
}

func (r *Runner) findSource(inhibition model.InhibitionRule, target State, states []State) (State, bool) {
	var candidates []State
	for _, source := range states {
		if source.State != StateFiring {
			continue
		}
		if stateKey(source.RuleID, source.InstanceKey) == stateKey(target.RuleID, target.InstanceKey) {
			continue
		}
		if !matchAll(inhibition.SourceMatchers, source.Labels) {
			continue
		}
		equal := true
		for _, label := range inhibition.EqualLabels {
			if source.Labels[label] != target.Labels[label] {
				equal = false
				break
			}
		}
		if equal {
			candidates = append(candidates, source)
		}
	}
	if len(candidates) == 0 {
		return State{}, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].RuleID != candidates[j].RuleID {
			return candidates[i].RuleID < candidates[j].RuleID
		}
		return candidates[i].InstanceKey < candidates[j].InstanceKey
	})
	return candidates[0], true
}

func (r *Runner) notifications(t int, states []State, hidden map[string]bool) ([]NotificationBatch, error) {
	batchByDedup := map[string]*NotificationBatch{}
	for _, st := range states {
		if st.State != StateFiring || !st.Changed || st.LastTransition != StateFiring {
			continue
		}
		if hidden[stateKey(st.RuleID, st.InstanceKey)] {
			continue
		}
		rule := r.rulesByID[st.RuleID]
		receiver, routeID, basis, err := r.route(rule, st.Labels)
		if err != nil {
			return nil, err
		}
		dedup := receiver + "|" + st.Labels.GroupKey(rule.GroupBy)
		batch, ok := batchByDedup[dedup]
		if !ok {
			h := fnv.New64a()
			_, _ = h.Write([]byte(dedup))
			batch = &NotificationBatch{
				BatchKey: fmt.Sprintf("batch-t%d-%x", t, h.Sum64()),
				DedupKey: dedup, Receiver: receiver, RouteID: routeID, RoutingBasis: basis,
				T: t, AtMS: r.project.Clock.TimeMS(t),
			}
			batchByDedup[dedup] = batch
		}
		batch.Members = append(batch.Members, NotificationMember{
			RuleID: st.RuleID, InstanceKey: st.InstanceKey, Labels: st.Labels.Clone(),
			BasisNo: st.FireBasisNo, BasisID: st.FireBasisID,
		})
	}
	out := make([]NotificationBatch, 0, len(batchByDedup))
	for _, batch := range batchByDedup {
		sort.SliceStable(batch.Members, func(i, j int) bool {
			if batch.Members[i].RuleID != batch.Members[j].RuleID {
				return batch.Members[i].RuleID < batch.Members[j].RuleID
			}
			return batch.Members[i].InstanceKey < batch.Members[j].InstanceKey
		})
		out = append(out, *batch)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DedupKey < out[j].DedupKey })
	return out, nil
}

func (r *Runner) route(rule *model.Rule, labels model.Labels) (receiver, routeID, basis string, err error) {
	for _, route := range r.project.Routes {
		if matchAll(route.Matchers, labels) {
			if !r.project.ReceiverNames()[route.Receiver] {
				return "", "", "", errs.New(errs.EReceiverNotFound, "routes."+route.ID+".receiver", "receiver %q is not declared", route.Receiver)
			}
			return route.Receiver, route.ID, "route:" + route.ID, nil
		}
	}
	if rule.Receiver != "" {
		if !r.project.ReceiverNames()[rule.Receiver] {
			return "", "", "", errs.New(errs.EReceiverNotFound, "rules."+rule.ID+".receiver", "receiver %q is not declared", rule.Receiver)
		}
		return rule.Receiver, "direct", "rule:" + rule.ID + ".receiver", nil
	}
	return "", "", "", errs.New(errs.EReceiverNotFound, "rules."+rule.ID, "no route or receiver matches alert labels %s", labels.CanonicalKey())
}

func stateKey(ruleID, instanceKey string) string { return ruleID + "\x00" + instanceKey }

func ruleOrder(p *model.Project, ruleID string) int {
	for _, rule := range p.Rules {
		if rule.ID == ruleID {
			return rule.Order
		}
	}
	return 1 << 30
}

func sortStates(p *model.Project, states []State) {
	sort.SliceStable(states, func(i, j int) bool {
		oi, oj := ruleOrder(p, states[i].RuleID), ruleOrder(p, states[j].RuleID)
		if oi != oj {
			return oi < oj
		}
		return states[i].InstanceKey < states[j].InstanceKey
	})
}

func sortVisible(alerts []VisibleAlert) {
	sort.SliceStable(alerts, func(i, j int) bool {
		if alerts[i].RuleID != alerts[j].RuleID {
			return alerts[i].RuleID < alerts[j].RuleID
		}
		return alerts[i].InstanceKey < alerts[j].InstanceKey
	})
}
