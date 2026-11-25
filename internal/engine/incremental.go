package engine

import (
	"reflect"
	"sort"

	"credrot/internal/bizerr"
	"credrot/internal/model"
	"credrot/internal/project"
)

type EditKind string

const (
	EditCredential         EditKind = "credential"
	EditSubscription       EditKind = "subscription"
	EditAddSubscription    EditKind = "add_subscription"
	EditRemoveSubscription EditKind = "remove_subscription"
	EditReload             EditKind = "reload"
)

type ProjectEdit struct {
	Kind           EditKind
	Credential     *model.Credential
	Subscription   *model.Subscription
	SubscriptionID string
	Reload         *model.ReloadConfig
}

type TickRange struct {
	StartTick int64 `json:"start_tick"`
	EndTick   int64 `json:"end_tick"`
}

type InvalidatedRange struct {
	SubscriptionID string    `json:"subscription_id"`
	Range          TickRange `json:"range"`
	Source         string    `json:"source"`
}

type ReuseEvidence struct {
	SubscriptionID string    `json:"subscription_id"`
	Range          TickRange `json:"range"`
	Source         string    `json:"source"`
}

type PropagationEvidence struct {
	CredentialID string `json:"credential_id"`
	Reused       bool   `json:"reused"`
	Source       string `json:"source"`
}

type Snapshot struct {
	Project     *model.Project         `json:"project"`
	Validity    []SubscriptionValidity `json:"validity"`
	Propagation *PropagationResult     `json:"propagation"`
	Schedule    *ScheduleResult        `json:"schedule"`
}

type IncrementalResult struct {
	Snapshot            *Snapshot             `json:"snapshot"`
	Invalidated         []InvalidatedRange    `json:"invalidated"`
	Reused              []ReuseEvidence       `json:"reused"`
	PropagationEvidence []PropagationEvidence `json:"propagation_evidence"`
	ScheduleRecomputed  bool                  `json:"schedule_recomputed"`
	ScheduleSource      string                `json:"schedule_source"`
	RecomputedValidity  int                   `json:"recomputed_validity_cells"`
	ReusedValidity      int                   `json:"reused_validity_cells"`
}

func BuildSnapshot(p *model.Project) (*Snapshot, error) {
	solver, err := NewSolver(p)
	if err != nil {
		return nil, err
	}
	validity, err := solver.ValidityAll()
	if err != nil {
		return nil, err
	}
	propagation, err := Propagate(p)
	if err != nil {
		return nil, err
	}
	schedule, err := Schedule(p, propagation)
	if err != nil {
		return nil, err
	}
	return &Snapshot{
		Project:     p.Clone(),
		Validity:    validity,
		Propagation: propagation,
		Schedule:    schedule,
	}, nil
}

func ApplyEdit(p *model.Project, edit ProjectEdit) (*model.Project, error) {
	if p == nil {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "project is nil", nil)
	}
	out := p.Clone()
	switch edit.Kind {
	case EditCredential:
		if edit.Credential == nil {
			return nil, bizerr.New(bizerr.CodeSchemaInvalid, "credential edit has no credential payload", nil)
		}
		found := false
		for i := range out.Credentials {
			if out.Credentials[i].ID == edit.Credential.ID {
				out.Credentials[i] = *edit.Credential
				found = true
				break
			}
		}
		if !found {
			out.Credentials = append(out.Credentials, *edit.Credential)
		}
	case EditSubscription:
		if edit.Subscription == nil {
			return nil, bizerr.New(bizerr.CodeSchemaInvalid, "subscription edit has no subscription payload", nil)
		}
		found := false
		for i := range out.Subscriptions {
			if out.Subscriptions[i].ID == edit.Subscription.ID {
				out.Subscriptions[i] = cloneSubscription(*edit.Subscription)
				found = true
				break
			}
		}
		if !found {
			return nil, bizerr.New(bizerr.CodeMissingSubscription, "subscription edit target is unknown", map[string]any{
				"subscription_id": edit.Subscription.ID,
			})
		}
	case EditAddSubscription:
		if edit.Subscription == nil {
			return nil, bizerr.New(bizerr.CodeSchemaInvalid, "subscription add has no subscription payload", nil)
		}
		for _, sub := range out.Subscriptions {
			if sub.ID == edit.Subscription.ID {
				return nil, bizerr.New(bizerr.CodeDuplicateSub, "subscription already exists", map[string]any{
					"subscription_id": edit.Subscription.ID,
				})
			}
		}
		out.Subscriptions = append(out.Subscriptions, cloneSubscription(*edit.Subscription))
	case EditRemoveSubscription:
		if edit.SubscriptionID == "" {
			return nil, bizerr.New(bizerr.CodeSchemaInvalid, "subscription removal has no target id", nil)
		}
		next := out.Subscriptions[:0]
		found := false
		for _, sub := range out.Subscriptions {
			if sub.ID == edit.SubscriptionID {
				found = true
				continue
			}
			deps := sub.DependsOn[:0]
			for _, dep := range sub.DependsOn {
				if dep != edit.SubscriptionID {
					deps = append(deps, dep)
				}
			}
			sub.DependsOn = deps
			next = append(next, sub)
		}
		if !found {
			return nil, bizerr.New(bizerr.CodeMissingSubscription, "subscription removal target is unknown", map[string]any{
				"subscription_id": edit.SubscriptionID,
			})
		}
		out.Subscriptions = next
	case EditReload:
		if edit.Reload == nil {
			return nil, bizerr.New(bizerr.CodeSchemaInvalid, "reload edit has no reload payload", nil)
		}
		out.Reload = *edit.Reload
		out.Reload.FailurePlan = make([]model.FailureInjection, len(edit.Reload.FailurePlan))
		for i, fp := range edit.Reload.FailurePlan {
			out.Reload.FailurePlan[i] = model.FailureInjection{
				SubscriptionID: fp.SubscriptionID,
				Attempts:       append([]int(nil), fp.Attempts...),
			}
		}
	default:
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "unknown edit kind", map[string]any{"kind": edit.Kind})
	}
	out.Sort()
	if err := project.Validate(out); err != nil {
		return nil, err
	}
	return out, nil
}

func Incremental(oldSnapshot *Snapshot, newProject *model.Project) (*IncrementalResult, error) {
	if oldSnapshot == nil || oldSnapshot.Project == nil {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "incremental cache is empty", nil)
	}
	if newProject == nil {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "new project is nil", nil)
	}
	if err := project.Validate(newProject); err != nil {
		return nil, err
	}
	oldP := oldSnapshot.Project
	newP := newProject.Clone()
	newP.Sort()

	oldCreds := oldP.CredentialByID()
	newCreds := newP.CredentialByID()
	changedCredentialIDs := make(map[string]bool)
	for id, oldCred := range oldCreds {
		newCred, ok := newCreds[id]
		if !ok || !reflect.DeepEqual(oldCred, newCred) {
			changedCredentialIDs[id] = true
		}
	}
	for id := range newCreds {
		if _, ok := oldCreds[id]; !ok {
			changedCredentialIDs[id] = true
		}
	}
	oldSubs := oldP.SubscriptionByID()
	newSubs := newP.SubscriptionByID()
	changedSubIDs := make(map[string]bool)
	for id, oldSub := range oldSubs {
		newSub, ok := newSubs[id]
		if !ok || !subscriptionsEqual(oldSub, newSub) {
			changedSubIDs[id] = true
		}
	}
	for id := range newSubs {
		if _, ok := oldSubs[id]; !ok {
			changedSubIDs[id] = true
		}
	}

	impact := make(map[string]string)
	credentialTicks := make(map[string][]TickRange)
	for id := range changedCredentialIDs {
		interval := credentialTickRange(oldP, oldCreds[id], newP, newCreds[id])
		credentialTicks[id] = interval
		for _, sub := range oldP.Subscriptions {
			if subscriptionReferences(sub, id) {
				addDescendants(oldP, sub.ID, impact, "credential:"+id)
			}
		}
		for _, sub := range newP.Subscriptions {
			if subscriptionReferences(sub, id) {
				addDescendants(newP, sub.ID, impact, "credential:"+id)
			}
		}
	}
	for id := range changedSubIDs {
		addDescendants(oldP, id, impact, "subscription:"+id)
		addDescendants(newP, id, impact, "subscription:"+id)
	}

	oldValidity := make(map[string]SubscriptionValidity, len(oldSnapshot.Validity))
	for _, value := range oldSnapshot.Validity {
		oldValidity[validityKey(value.Tick, value.SubscriptionID)] = value
	}
	solver, err := NewSolver(newP)
	if err != nil {
		return nil, err
	}
	newValidity := make([]SubscriptionValidity, 0, len(oldSnapshot.Validity)+len(changedSubIDs)*int(newP.TickCount()))
	recomputed := make(map[string]bool)
	recomputedCount := 0
	reusedCount := 0
	for tick := int64(0); tick <= newP.HorizonTicks; tick++ {
		for _, sub := range newP.Subscriptions {
			key := validityKey(tick, sub.ID)
			reason, impacted := impact[sub.ID]
			useCredentialInterval := impacted && reason != "" && len(reason) > len("credential:") && reason[:len("credential:")] == "credential:"
			if useCredentialInterval {
				if !containsTick(credentialTicks[reason[len("credential:"):]], tick) {
					impacted = false
				}
			}
			if !impacted {
				if cached, ok := oldValidity[key]; ok {
					newValidity = append(newValidity, cached)
					reusedCount++
					continue
				}
			}
			value, err := solver.ValidityForSubscription(tick, sub.ID)
			if err != nil {
				return nil, err
			}
			newValidity = append(newValidity, value)
			recomputed[key] = true
			recomputedCount++
		}
	}

	oldProp := make(map[string]SuspendedCredential)
	if oldSnapshot.Propagation != nil {
		for _, item := range oldSnapshot.Propagation.Suspended {
			oldProp[item.CredentialID] = item
		}
	}
	newPropCtx := newPropagationContext(newP)
	newSuspended := make([]SuspendedCredential, 0, len(newP.Rotations))
	propEvidence := make([]PropagationEvidence, 0, len(newP.Rotations))
	for _, rotation := range newP.Rotations {
		reuse := false
		cached, exists := oldProp[rotation.CredentialID]
		if exists && !changedCredentialIDs[rotation.CredentialID] && !propagationInvalidated(cached, changedSubIDs, oldP, newP) {
			reuse = true
		}
		if reuse {
			newSuspended = append(newSuspended, cached)
			propEvidence = append(propEvidence, PropagationEvidence{
				CredentialID: rotation.CredentialID,
				Reused:       true,
				Source:       "cache:rotation:" + rotation.CredentialID,
			})
			continue
		}
		suspended, err := newPropCtx.one(rotation)
		if err != nil {
			return nil, err
		}
		newSuspended = append(newSuspended, suspended)
		propEvidence = append(propEvidence, PropagationEvidence{
			CredentialID: rotation.CredentialID,
			Reused:       false,
			Source:       "reload:" + rotation.CredentialID,
		})
	}
	sort.Slice(newSuspended, func(i, j int) bool {
		return newSuspended[i].CredentialID < newSuspended[j].CredentialID
	})
	sort.Slice(propEvidence, func(i, j int) bool {
		return propEvidence[i].CredentialID < propEvidence[j].CredentialID
	})
	propagationRecomputed := false
	for _, evidence := range propEvidence {
		if !evidence.Reused {
			propagationRecomputed = true
			break
		}
	}
	reloadChanged := !reflect.DeepEqual(oldP.Reload, newP.Reload)
	var newSchedule *ScheduleResult
	scheduleSource := "cache:schedule"
	scheduleRecomputed := false
	switch {
	case reloadChanged:
		newSchedule, err = Schedule(newP, &PropagationResult{Suspended: newSuspended})
		if err != nil {
			return nil, err
		}
		scheduleRecomputed = true
		scheduleSource = "reload:config"
	case propagationRecomputed || oldSnapshot.Schedule == nil:
		newSchedule, err = Schedule(newP, &PropagationResult{Suspended: newSuspended})
		if err != nil {
			return nil, err
		}
		scheduleRecomputed = true
		scheduleSource = "reload:propagation"
	default:
		reused := *oldSnapshot.Schedule
		newSchedule = &reused
	}

	invalidatedRanges := make([]InvalidatedRange, 0)
	reusedRanges := make([]ReuseEvidence, 0)
	for _, sub := range newP.Subscriptions {
		var recomputedTicks, reusedTicks []int64
		for tick := int64(0); tick <= newP.HorizonTicks; tick++ {
			if recomputed[validityKey(tick, sub.ID)] {
				recomputedTicks = append(recomputedTicks, tick)
			} else {
				reusedTicks = append(reusedTicks, tick)
			}
		}
		source := impact[sub.ID]
		if source == "" {
			source = "subscription:new"
		}
		for _, r := range coalesceTicks(recomputedTicks) {
			invalidatedRanges = append(invalidatedRanges, InvalidatedRange{SubscriptionID: sub.ID, Range: r, Source: source})
		}
		for _, r := range coalesceTicks(reusedTicks) {
			reusedRanges = append(reusedRanges, ReuseEvidence{SubscriptionID: sub.ID, Range: r, Source: "cache:" + sub.ID})
		}
	}
	sort.Slice(invalidatedRanges, func(i, j int) bool {
		if invalidatedRanges[i].SubscriptionID != invalidatedRanges[j].SubscriptionID {
			return invalidatedRanges[i].SubscriptionID < invalidatedRanges[j].SubscriptionID
		}
		return invalidatedRanges[i].Range.StartTick < invalidatedRanges[j].Range.StartTick
	})
	sort.Slice(reusedRanges, func(i, j int) bool {
		if reusedRanges[i].SubscriptionID != reusedRanges[j].SubscriptionID {
			return reusedRanges[i].SubscriptionID < reusedRanges[j].SubscriptionID
		}
		return reusedRanges[i].Range.StartTick < reusedRanges[j].Range.StartTick
	})
	sort.Slice(newValidity, func(i, j int) bool {
		if newValidity[i].Tick != newValidity[j].Tick {
			return newValidity[i].Tick < newValidity[j].Tick
		}
		return newValidity[i].SubscriptionID < newValidity[j].SubscriptionID
	})
	return &IncrementalResult{
		Snapshot: &Snapshot{
			Project:     newP,
			Validity:    newValidity,
			Propagation: &PropagationResult{Suspended: newSuspended},
			Schedule:    newSchedule,
		},
		Invalidated:         invalidatedRanges,
		Reused:              reusedRanges,
		PropagationEvidence: propEvidence,
		ScheduleRecomputed:  scheduleRecomputed,
		ScheduleSource:      scheduleSource,
		RecomputedValidity:  recomputedCount,
		ReusedValidity:      reusedCount,
	}, nil
}

func propagationInvalidated(cached SuspendedCredential, changedSubs map[string]bool, oldP, newP *model.Project) bool {
	arrivals := make(map[string]bool, len(cached.ArrivalSet))
	for _, id := range cached.ArrivalSet {
		arrivals[id] = true
	}
	oldSubs := oldP.SubscriptionByID()
	newSubs := newP.SubscriptionByID()
	for changed := range changedSubs {
		if arrivals[changed] {
			return true
		}
		var stack []string
		if sub, exists := oldSubs[changed]; exists {
			stack = append(stack, sub.DependsOn...)
		}
		if sub, exists := newSubs[changed]; exists {
			stack = append(stack, sub.DependsOn...)
		}
		seen := make(map[string]bool)
		for len(stack) > 0 {
			dep := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[dep] {
				continue
			}
			seen[dep] = true
			if arrivals[dep] {
				return true
			}
			if sub, ok := oldSubs[dep]; ok {
				stack = append(stack, sub.DependsOn...)
			}
			if sub, ok := newSubs[dep]; ok {
				stack = append(stack, sub.DependsOn...)
			}
		}
	}
	return false
}

func addDescendants(p *model.Project, start string, impact map[string]string, source string) {
	subs := p.SubscriptionByID()
	if _, ok := subs[start]; !ok {
		return
	}
	dependents := make(map[string][]string)
	for _, sub := range p.Subscriptions {
		for _, dep := range sub.DependsOn {
			dependents[dep] = append(dependents[dep], sub.ID)
		}
	}
	queue := []string{start}
	seen := make(map[string]bool)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		current := impact[id]
		if current == "" || sourceRank(source) > sourceRank(current) {
			impact[id] = source
		}
		children := append([]string(nil), dependents[id]...)
		sort.Strings(children)
		queue = append(queue, children...)
	}
}

func sourceRank(source string) int {
	if len(source) >= len("subscription:") && source[:len("subscription:")] == "subscription:" {
		return 2
	}
	if len(source) >= len("credential:") && source[:len("credential:")] == "credential:" {
		return 1
	}
	return 0
}

func credentialTickRange(oldP *model.Project, oldCred model.Credential, newP *model.Project, newCred model.Credential) []TickRange {
	if oldCred.ID == "" && newCred.ID == "" {
		return nil
	}
	maxTick := oldP.HorizonTicks
	if newP.HorizonTicks < maxTick {
		maxTick = newP.HorizonTicks
	}
	if maxTick < 0 {
		return nil
	}
	start := int64(0)
	// A TTL-only edit changes expiry-derived state from the first instant at
	// which either version can become valid through the end of the horizon.
	if oldCred.ID != "" && newCred.ID != "" {
		oldCompare, newCompare := oldCred, newCred
		oldCompare.TTLMillis, newCompare.TTLMillis = 0, 0
		if reflect.DeepEqual(oldCompare, newCompare) {
			oldFrom := oldCred.ValidFromMillis(oldP.LastRotationWatermarkMillis)
			newFrom := newCred.ValidFromMillis(newP.LastRotationWatermarkMillis)
			from := oldFrom
			if newFrom < from {
				from = newFrom
			}
			start = oldP.MillisToTickCeil(from)
			if start < 0 {
				start = 0
			}
		}
	}
	if start > maxTick {
		return nil
	}
	return []TickRange{{StartTick: start, EndTick: maxTick}}
}

func containsTick(ranges []TickRange, tick int64) bool {
	for _, r := range ranges {
		if tick >= r.StartTick && tick <= r.EndTick {
			return true
		}
	}
	return false
}

func coalesceTicks(ticks []int64) []TickRange {
	if len(ticks) == 0 {
		return nil
	}
	sort.Slice(ticks, func(i, j int) bool { return ticks[i] < ticks[j] })
	out := []TickRange{{StartTick: ticks[0], EndTick: ticks[0]}}
	for _, tick := range ticks[1:] {
		last := &out[len(out)-1]
		if tick == last.EndTick+1 {
			last.EndTick = tick
		} else {
			out = append(out, TickRange{StartTick: tick, EndTick: tick})
		}
	}
	return out
}

func validityKey(tick int64, sub string) string {
	return formatTick(tick) + "|" + sub
}

func formatTick(tick int64) string {
	if tick == 0 {
		return "0"
	}
	negative := tick < 0
	var buf [24]byte
	i := len(buf)
	if negative {
		tick = -tick
	}
	for tick > 0 {
		i--
		buf[i] = byte('0' + tick%10)
		tick /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func subscriptionReferences(sub model.Subscription, credentialID string) bool {
	for _, ref := range sub.Refs {
		if ref.CredentialID == credentialID {
			return true
		}
	}
	return false
}

func subscriptionsEqual(a, b model.Subscription) bool {
	return reflect.DeepEqual(a, b)
}

func cloneSubscription(sub model.Subscription) model.Subscription {
	out := sub
	out.DependsOn = append([]string(nil), sub.DependsOn...)
	out.Refs = append([]model.CredentialRef(nil), sub.Refs...)
	return out
}
