package engine

import (
	"sort"

	"credrot/internal/bizerr"
	"credrot/internal/model"
	"credrot/internal/project"
)

type DeferredSubscription struct {
	SubscriptionID string `json:"subscription_id"`
	OriginalBatch  int64  `json:"original_batch"`
	ActualBatch    int64  `json:"actual_batch"`
	Reason         string `json:"reason"`
	Constraint     string `json:"constraint"`
}

type RetryAttempt struct {
	Attempt   int      `json:"attempt"`
	Tick      int64    `json:"tick"`
	Millis    int64    `json:"millis"`
	Failed    []string `json:"failed"`
	Succeeded []string `json:"succeeded"`
	Trigger   string   `json:"trigger"`
}

type ReloadFailure struct {
	SubscriptionID string `json:"subscription_id"`
	Attempts       int    `json:"attempts"`
	FailureTick    int64  `json:"failure_tick"`
	FailureMillis  int64  `json:"failure_ms"`
	TriggerSource  string `json:"trigger_source"`
	Constraint     string `json:"constraint,omitempty"`
}

type ReloadBatchResult struct {
	Batch     int64                  `json:"batch"`
	Planned   []string               `json:"planned"`
	Actual    []string               `json:"actual"`
	Failed    []ReloadFailure        `json:"failed"`
	Deferred  []DeferredSubscription `json:"deferred,omitempty"`
	Attempts  []RetryAttempt         `json:"attempts"`
	StartTick int64                  `json:"start_tick"`
}

type ScheduleResult struct {
	Batches        []ReloadBatchResult `json:"batches"`
	FullSet        []string            `json:"full_set"`
	ActualUnion    []string            `json:"actual_union"`
	FailedUnion    []string            `json:"failed_union"`
	CoverageMatch  bool                `json:"coverage_match"`
	OneShotSet     []string            `json:"one_shot_set"`
	Constraint     string              `json:"capacity_constraint"`
	ArrivalSources map[string][]string `json:"arrival_sources"`
}

func Schedule(p *model.Project, propagation *PropagationResult) (*ScheduleResult, error) {
	if p == nil {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "project is nil", nil)
	}
	if err := project.Validate(p); err != nil {
		return nil, err
	}
	if propagation == nil {
		var err error
		propagation, err = Propagate(p)
		if err != nil {
			return nil, err
		}
	}

	targetBatch := make(map[string]int64)
	arrivalSources := make(map[string][]string)
	for _, suspended := range propagation.Suspended {
		for _, arrival := range suspended.Arrivals {
			if existing, ok := targetBatch[arrival.SubscriptionID]; !ok || arrival.Batch > existing {
				targetBatch[arrival.SubscriptionID] = arrival.Batch
			}
			arrivalSources[arrival.SubscriptionID] = append(arrivalSources[arrival.SubscriptionID], suspended.CredentialID)
		}
	}
	for id := range arrivalSources {
		sort.Strings(arrivalSources[id])
	}
	fullSet := make([]string, 0, len(targetBatch))
	for id := range targetBatch {
		fullSet = append(fullSet, id)
	}
	sort.Strings(fullSet)
	if len(fullSet) == 0 {
		return &ScheduleResult{
			Batches:        []ReloadBatchResult{},
			FullSet:        []string{},
			ActualUnion:    []string{},
			FailedUnion:    []string{},
			CoverageMatch:  true,
			OneShotSet:     []string{},
			Constraint:     "REL-CAP-001",
			ArrivalSources: map[string][]string{},
		}, nil
	}

	pending := make(map[int64][]string)
	for sub, batch := range targetBatch {
		pending[batch] = append(pending[batch], sub)
	}
	plannedByBatch := make(map[int64][]string)
	deferredBySub := make(map[string][]DeferredSubscription)
	for {
		batch, ok := smallestBatch(pending)
		if !ok {
			break
		}
		items := pending[batch]
		delete(pending, batch)
		sort.Strings(items)
		if len(items) == 0 {
			continue
		}
		if len(items) > p.Reload.MaxPerBatch {
			keep := append([]string(nil), items[:p.Reload.MaxPerBatch]...)
			move := append([]string(nil), items[p.Reload.MaxPerBatch:]...)
			plannedByBatch[batch] = append(plannedByBatch[batch], keep...)
			nextBatch := batch + 1
			pending[nextBatch] = append(pending[nextBatch], move...)
			sort.Strings(pending[nextBatch])
			for _, sub := range move {
				deferredBySub[sub] = append(deferredBySub[sub], DeferredSubscription{
					SubscriptionID: sub,
					OriginalBatch:  targetBatch[sub],
					ActualBatch:    nextBatch,
					Reason:         "batch capacity exceeded; moved to the next reload slice",
					Constraint:     "REL-CAP-001",
				})
			}
			continue
		}
		plannedByBatch[batch] = append(plannedByBatch[batch], items...)
	}

	subs := p.SubscriptionByID()
	dependents := make(map[string][]string)
	for _, sub := range p.Subscriptions {
		for _, dep := range sub.DependsOn {
			dependents[dep] = append(dependents[dep], sub.ID)
		}
	}
	for id := range dependents {
		sort.Strings(dependents[id])
	}
	failureAttempts := make(map[string]map[int]bool)
	for _, fp := range p.Reload.FailurePlan {
		if failureAttempts[fp.SubscriptionID] == nil {
			failureAttempts[fp.SubscriptionID] = make(map[int]bool)
		}
		for _, attempt := range fp.Attempts {
			failureAttempts[fp.SubscriptionID][attempt] = true
		}
	}

	batchNumbers := make([]int64, 0, len(plannedByBatch))
	for batch := range plannedByBatch {
		batchNumbers = append(batchNumbers, batch)
	}
	sort.Slice(batchNumbers, func(i, j int) bool { return batchNumbers[i] < batchNumbers[j] })

	result := &ScheduleResult{
		Batches:        make([]ReloadBatchResult, 0, len(batchNumbers)),
		FullSet:        fullSet,
		ActualUnion:    []string{},
		FailedUnion:    []string{},
		OneShotSet:     append([]string(nil), fullSet...),
		Constraint:     "REL-CAP-001",
		ArrivalSources: arrivalSources,
	}
	failedSet := make(map[string]ReloadFailure)
	actualSet := make(map[string]bool)
	for _, batch := range batchNumbers {
		planned := append([]string(nil), plannedByBatch[batch]...)
		var deferred []DeferredSubscription
		for _, sub := range planned {
			deferred = append(deferred, deferredBySub[sub]...)
		}
		sort.Strings(planned)
		sort.Slice(deferred, func(i, j int) bool {
			if deferred[i].SubscriptionID != deferred[j].SubscriptionID {
				return deferred[i].SubscriptionID < deferred[j].SubscriptionID
			}
			return deferred[i].ActualBatch < deferred[j].ActualBatch
		})
		topo := topoOrderSubset(p, planned)
		batchStartMillis := scheduleBatchStartMillis(p, batch)
		batchStartTick := p.MillisToTickCeil(batchStartMillis)
		batchResult := ReloadBatchResult{
			Batch:     batch,
			Planned:   planned,
			Actual:    []string{},
			Failed:    []ReloadFailure{},
			Deferred:  deferred,
			Attempts:  []RetryAttempt{},
			StartTick: batchStartTick,
		}

		terminal := make(map[string]bool)
		remaining := make([]string, 0, len(topo))
		for _, sub := range topo {
			upstream := firstFailedDependency(subs[sub].DependsOn, terminal, failedSet)
			if upstream != "" {
				failure := ReloadFailure{
					SubscriptionID: sub,
					Attempts:       0,
					FailureTick:    batchStartTick,
					FailureMillis:  batchStartMillis,
					TriggerSource:  "upstream:" + upstream,
					Constraint:     "REL-TOPO-002",
				}
				terminal[sub] = true
				failedSet[sub] = failure
				batchResult.Failed = append(batchResult.Failed, failure)
				continue
			}
			remaining = append(remaining, sub)
		}

		active := append([]string(nil), remaining...)
		var lastFailureMillis int64
		for attempt := 1; attempt <= p.Reload.RetryLimit && len(active) > 0; attempt++ {
			attemptMillis := batchStartMillis + int64(attempt-1)*p.Reload.RetryStepMillis
			attemptTick := p.MillisToTickCeil(attemptMillis)
			record := RetryAttempt{
				Attempt:   attempt,
				Tick:      attemptTick,
				Millis:    attemptMillis,
				Failed:    []string{},
				Succeeded: []string{},
				Trigger:   "scheduled_retry",
			}
			next := make([]string, 0, len(active))
			for _, sub := range active {
				if failureAttempts[sub][attempt] {
					record.Failed = append(record.Failed, sub)
					next = append(next, sub)
					lastFailureMillis = attemptMillis
				} else {
					record.Succeeded = append(record.Succeeded, sub)
					actualSet[sub] = true
					batchResult.Actual = append(batchResult.Actual, sub)
				}
			}
			sort.Strings(record.Failed)
			sort.Strings(record.Succeeded)
			batchResult.Attempts = append(batchResult.Attempts, record)
			active = next
		}
		for _, sub := range active {
			failure := ReloadFailure{
				SubscriptionID: sub,
				Attempts:       p.Reload.RetryLimit,
				FailureTick:    p.MillisToTickCeil(lastFailureMillis),
				FailureMillis:  lastFailureMillis,
				TriggerSource:  "retry_exhausted;credential_source=" + joinList(arrivalSources[sub]),
				Constraint:     "REL-RETRY-003",
			}
			terminal[sub] = true
			failedSet[sub] = failure
			batchResult.Failed = append(batchResult.Failed, failure)
		}
		sort.Strings(batchResult.Actual)
		sort.Slice(batchResult.Failed, func(i, j int) bool {
			return batchResult.Failed[i].SubscriptionID < batchResult.Failed[j].SubscriptionID
		})
		result.Batches = append(result.Batches, batchResult)
	}

	for sub := range actualSet {
		result.ActualUnion = append(result.ActualUnion, sub)
	}
	for sub := range failedSet {
		result.FailedUnion = append(result.FailedUnion, sub)
	}
	sort.Strings(result.ActualUnion)
	sort.Strings(result.FailedUnion)
	result.CoverageMatch = sameSet(append(append([]string(nil), result.ActualUnion...), result.FailedUnion...), fullSet)
	return result, nil
}

func smallestBatch(pending map[int64][]string) (int64, bool) {
	var (
		found bool
		min   int64
	)
	for batch, items := range pending {
		if len(items) == 0 {
			continue
		}
		if !found || batch < min {
			found = true
			min = batch
		}
	}
	return min, found
}

func topoOrderSubset(p *model.Project, subset []string) []string {
	wanted := make(map[string]bool, len(subset))
	for _, id := range subset {
		wanted[id] = true
	}
	subs := p.SubscriptionByID()
	order := make([]string, 0, len(subset))
	state := make(map[string]uint8, len(subset))
	var visit func(string)
	visit = func(id string) {
		if !wanted[id] || state[id] != 0 {
			return
		}
		state[id] = 1
		deps := append([]string(nil), subs[id].DependsOn...)
		sort.Strings(deps)
		for _, dep := range deps {
			if wanted[dep] {
				visit(dep)
			}
		}
		state[id] = 2
		order = append(order, id)
	}
	ids := append([]string(nil), subset...)
	sort.Strings(ids)
	for _, id := range ids {
		visit(id)
	}
	return order
}

func firstFailedDependency(deps []string, terminal map[string]bool, failed map[string]ReloadFailure) string {
	candidates := append([]string(nil), deps...)
	sort.Strings(candidates)
	for _, dep := range candidates {
		if terminal[dep] || failed[dep].SubscriptionID != "" {
			return dep
		}
	}
	return ""
}

func scheduleBatchStartMillis(p *model.Project, batch int64) int64 {
	attemptsPerBatch := int64(p.Reload.RetryLimit)
	tick := p.Reload.ScheduleStartTick + batch*attemptsPerBatch
	return p.TickToMillis(tick)
}

func joinList(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	out := values[0]
	for _, value := range values[1:] {
		out += "," + value
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
