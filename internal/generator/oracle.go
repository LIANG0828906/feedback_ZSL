package generator

import (
	"fmt"
	"sort"

	"credrot/internal/engine"
	"credrot/internal/model"
)

type Oracle struct {
	Seed            int64                         `json:"seed"`
	Manifest        string                        `json:"manifest_version"`
	Validity        []engine.SubscriptionValidity `json:"validity"`
	Propagation     *engine.PropagationResult     `json:"propagation"`
	Schedule        *engine.ScheduleResult        `json:"schedule"`
	Gates           *engine.GateResult            `json:"gates"`
	GateRuntime     engine.GateRuntime            `json:"gate_runtime"`
	CombinedAnomaly engine.GateDecision           `json:"combined_anomaly_gate"`
}

func BuildOracle(p *model.Project, seed int64) (*Oracle, error) {
	validity, err := referenceValidity(p)
	if err != nil {
		return nil, err
	}
	propagation, err := referencePropagation(p)
	if err != nil {
		return nil, err
	}
	schedule, err := referenceSchedule(p, propagation)
	if err != nil {
		return nil, err
	}
	runtime := referenceGateRuntime(p)
	gates, err := referenceGates(p, runtime)
	if err != nil {
		return nil, err
	}
	combined := engine.GateDecision{
		SubscriptionID:      "combined-anomaly",
		Tick:                6,
		Decision:            "reject",
		Code:                "E_VERSION_REGRESSION",
		Source:              "committed:ordered-version",
		Reason:              "credential version regressed from 2 to 1",
		RequestedCredential: "requested-v1",
		CommittedCredential: "committed-v2",
	}
	return &Oracle{
		Seed:            seed,
		Manifest:        p.ManifestVersion,
		Validity:        validity,
		Propagation:     propagation,
		Schedule:        schedule,
		Gates:           gates,
		GateRuntime:     runtime,
		CombinedAnomaly: combined,
	}, nil
}

func referenceValidity(p *model.Project) ([]engine.SubscriptionValidity, error) {
	subs := append([]model.Subscription(nil), p.Subscriptions...)
	sort.Slice(subs, func(i, j int) bool { return subs[i].ID < subs[j].ID })
	creds := p.CredentialByID()
	out := make([]engine.SubscriptionValidity, 0, (p.HorizonTicks+1)*int64(len(subs)))
	for tick := int64(0); tick <= p.HorizonTicks; tick++ {
		now := tick * p.TickMillis
		for _, sub := range subs {
			value := engine.SubscriptionValidity{
				Tick:           tick,
				NowMillis:      now,
				SubscriptionID: sub.ID,
				Active:         sub.Active,
				Available:      []engine.AvailableCredential{},
				States:         []engine.CredentialState{},
			}
			for _, ref := range sub.Refs {
				c := creds[ref.CredentialID]
				from := c.IssuedAtMillis
				if p.LastRotationWatermarkMillis > from {
					from = p.LastRotationWatermarkMillis
				}
				expiry := c.IssuedAtMillis + c.TTLMillis
				state := engine.CredentialState{CredentialID: c.ID, Version: c.Version}
				if now >= expiry {
					state.Status = "expired"
					state.Source = fmt.Sprintf("expiry:%d", expiry)
				} else if now < from {
					state.Status = "not_yet_valid"
					state.Source = fmt.Sprintf("watermark/issued:max=%d", from)
				} else {
					state.Status = "valid"
					state.Source = fmt.Sprintf("issued:%d;ttl:%d;watermark:%d", c.IssuedAtMillis, c.TTLMillis, p.LastRotationWatermarkMillis)
					remaining := expiry - now
					value.Available = append(value.Available, engine.AvailableCredential{
						CredentialID:    c.ID,
						Version:         c.Version,
						ValidFromMillis: from,
						ExpiresAtMillis: expiry,
						RemainingMillis: remaining,
						RemainingTicks:  (remaining + p.TickMillis - 1) / p.TickMillis,
						Source:          "credential:" + c.ID,
					})
				}
				value.States = append(value.States, state)
			}
			sort.Slice(value.Available, func(i, j int) bool {
				if value.Available[i].Version != value.Available[j].Version {
					return value.Available[i].Version > value.Available[j].Version
				}
				return value.Available[i].CredentialID < value.Available[j].CredentialID
			})
			sort.Slice(value.States, func(i, j int) bool {
				return value.States[i].CredentialID < value.States[j].CredentialID
			})
			out = append(out, value)
		}
	}
	return out, nil
}

func referencePropagation(p *model.Project) (*engine.PropagationResult, error) {
	creds := p.CredentialByID()
	subs := p.SubscriptionByID()
	children := make(map[string][]string)
	for _, sub := range p.Subscriptions {
		for _, parent := range sub.DependsOn {
			children[parent] = append(children[parent], sub.ID)
		}
	}
	for id := range children {
		sort.Strings(children[id])
	}
	result := &engine.PropagationResult{Suspended: []engine.SuspendedCredential{}}
	for _, rotation := range p.Rotations {
		target := creds[rotation.CredentialID]
		var direct []string
		for _, sub := range p.Subscriptions {
			if !sub.Active {
				continue
			}
			for _, ref := range sub.Refs {
				if ref.CredentialID == rotation.CredentialID && (ref.ExpectedVersion == 0 || ref.ExpectedVersion == target.Version) {
					direct = append(direct, sub.ID)
				}
			}
		}
		sort.Strings(direct)
		bestDepth := make(map[string]int)
		bestPath := make(map[string][]string)
		var walk func(path []string)
		walk = func(path []string) {
			current := path[len(path)-1]
			depth := len(path) - 1
			if old, ok := bestDepth[current]; !ok || depth > old || (depth == old && pathText(path) < pathText(bestPath[current])) {
				bestDepth[current] = depth
				bestPath[current] = append([]string(nil), path...)
			}
			for _, child := range children[current] {
				if !subs[child].Active || contains(path, child) {
					continue
				}
				walk(append(path, child))
			}
		}
		for _, id := range direct {
			walk([]string{id})
		}
		arrivals := make([]engine.ReloadArrival, 0, len(bestDepth))
		for id, depth := range bestDepth {
			arrivals = append(arrivals, engine.ReloadArrival{
				SubscriptionID: id,
				Depth:          depth,
				Batch:          target.ReloadBatch + int64(depth),
				Path:           append([]string(nil), bestPath[id]...),
			})
		}
		sort.Slice(arrivals, func(i, j int) bool {
			if arrivals[i].Batch != arrivals[j].Batch {
				return arrivals[i].Batch < arrivals[j].Batch
			}
			return arrivals[i].SubscriptionID < arrivals[j].SubscriptionID
		})
		arrivalSet := make([]string, 0, len(arrivals))
		for _, arrival := range arrivals {
			arrivalSet = append(arrivalSet, arrival.SubscriptionID)
		}
		result.Suspended = append(result.Suspended, engine.SuspendedCredential{
			CredentialID:    rotation.CredentialID,
			Status:          "suspended",
			SourceVersion:   rotation.FromVersion,
			TargetVersion:   rotation.ToVersion,
			BaseReloadBatch: target.ReloadBatch,
			Arrivals:        arrivals,
			ArrivalSet:      arrivalSet,
			SourceReason:    "rotation:" + rotation.CredentialID,
		})
	}
	sort.Slice(result.Suspended, func(i, j int) bool {
		return result.Suspended[i].CredentialID < result.Suspended[j].CredentialID
	})
	return result, nil
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func pathText(path []string) string {
	out := ""
	for i, value := range path {
		if i > 0 {
			out += "\x00"
		}
		out += value
	}
	return out
}

func referenceSchedule(p *model.Project, propagation *engine.PropagationResult) (*engine.ScheduleResult, error) {
	target := make(map[string]int64)
	sources := make(map[string][]string)
	for _, suspended := range propagation.Suspended {
		for _, arrival := range suspended.Arrivals {
			if old, ok := target[arrival.SubscriptionID]; !ok || arrival.Batch > old {
				target[arrival.SubscriptionID] = arrival.Batch
			}
			sources[arrival.SubscriptionID] = append(sources[arrival.SubscriptionID], suspended.CredentialID)
		}
	}
	for id := range sources {
		sort.Strings(sources[id])
	}
	full := make([]string, 0, len(target))
	for id := range target {
		full = append(full, id)
	}
	sort.Strings(full)
	pending := make(map[int64][]string)
	for id, batch := range target {
		pending[batch] = append(pending[batch], id)
	}
	planned := make(map[int64][]string)
	deferred := make(map[string][]engine.DeferredSubscription)
	for len(pending) > 0 {
		var batch int64
		found := false
		for b, items := range pending {
			if len(items) == 0 {
				delete(pending, b)
				continue
			}
			if !found || b < batch {
				batch, found = b, true
			}
		}
		if !found {
			break
		}
		items := append([]string(nil), pending[batch]...)
		delete(pending, batch)
		sort.Strings(items)
		if len(items) > p.Reload.MaxPerBatch {
			planned[batch] = append(planned[batch], items[:p.Reload.MaxPerBatch]...)
			move := items[p.Reload.MaxPerBatch:]
			pending[batch+1] = append(pending[batch+1], move...)
			sort.Strings(pending[batch+1])
			for _, id := range move {
				deferred[id] = append(deferred[id], engine.DeferredSubscription{
					SubscriptionID: id,
					OriginalBatch:  target[id],
					ActualBatch:    batch + 1,
					Reason:         "batch capacity exceeded; moved to the next reload slice",
					Constraint:     "REL-CAP-001",
				})
			}
		} else {
			planned[batch] = append(planned[batch], items...)
		}
	}
	batchNumbers := make([]int64, 0, len(planned))
	for batch := range planned {
		batchNumbers = append(batchNumbers, batch)
	}
	sort.Slice(batchNumbers, func(i, j int) bool { return batchNumbers[i] < batchNumbers[j] })
	failAttempts := make(map[string]map[int]bool)
	for _, fp := range p.Reload.FailurePlan {
		if failAttempts[fp.SubscriptionID] == nil {
			failAttempts[fp.SubscriptionID] = make(map[int]bool)
		}
		for _, attempt := range fp.Attempts {
			failAttempts[fp.SubscriptionID][attempt] = true
		}
	}
	result := &engine.ScheduleResult{
		Batches:        []engine.ReloadBatchResult{},
		FullSet:        full,
		ActualUnion:    []string{},
		FailedUnion:    []string{},
		OneShotSet:     append([]string(nil), full...),
		Constraint:     "REL-CAP-001",
		ArrivalSources: sources,
	}
	actual := make(map[string]bool)
	failed := make(map[string]engine.ReloadFailure)
	subs := p.SubscriptionByID()
	for _, batch := range batchNumbers {
		items := append([]string(nil), planned[batch]...)
		sort.Strings(items)
		batchStartMillis := p.TickMillis * (p.Reload.ScheduleStartTick + batch*int64(p.Reload.RetryLimit))
		batchResult := engine.ReloadBatchResult{
			Batch:     batch,
			Planned:   items,
			Actual:    []string{},
			Failed:    []engine.ReloadFailure{},
			Attempts:  []engine.RetryAttempt{},
			StartTick: p.MillisToTickCeil(batchStartMillis),
		}
		for _, id := range items {
			batchResult.Deferred = append(batchResult.Deferred, deferred[id]...)
		}
		sort.Slice(batchResult.Deferred, func(i, j int) bool {
			if batchResult.Deferred[i].SubscriptionID != batchResult.Deferred[j].SubscriptionID {
				return batchResult.Deferred[i].SubscriptionID < batchResult.Deferred[j].SubscriptionID
			}
			return batchResult.Deferred[i].ActualBatch < batchResult.Deferred[j].ActualBatch
		})
		order := referenceTopoSubset(p, items)
		terminal := make(map[string]bool)
		active := make([]string, 0, len(order))
		for _, id := range order {
			if upstream := referenceUpstreamFailure(subs[id].DependsOn, terminal, failed); upstream != "" {
				failure := engine.ReloadFailure{
					SubscriptionID: id,
					Attempts:       0,
					FailureTick:    p.MillisToTickCeil(batchStartMillis),
					FailureMillis:  batchStartMillis,
					TriggerSource:  "upstream:" + upstream,
					Constraint:     "REL-TOPO-002",
				}
				terminal[id] = true
				failed[id] = failure
				batchResult.Failed = append(batchResult.Failed, failure)
			} else {
				active = append(active, id)
			}
		}
		lastFailure := int64(0)
		for attempt := 1; attempt <= p.Reload.RetryLimit && len(active) > 0; attempt++ {
			attemptMillis := batchStartMillis + int64(attempt-1)*p.Reload.RetryStepMillis
			record := engine.RetryAttempt{
				Attempt:   attempt,
				Tick:      p.MillisToTickCeil(attemptMillis),
				Millis:    attemptMillis,
				Failed:    []string{},
				Succeeded: []string{},
				Trigger:   "scheduled_retry",
			}
			var next []string
			for _, id := range active {
				if failAttempts[id][attempt] {
					record.Failed = append(record.Failed, id)
					next = append(next, id)
					lastFailure = attemptMillis
				} else {
					record.Succeeded = append(record.Succeeded, id)
					actual[id] = true
					batchResult.Actual = append(batchResult.Actual, id)
				}
			}
			sort.Strings(record.Failed)
			sort.Strings(record.Succeeded)
			batchResult.Attempts = append(batchResult.Attempts, record)
			active = next
		}
		for _, id := range active {
			failure := engine.ReloadFailure{
				SubscriptionID: id,
				Attempts:       p.Reload.RetryLimit,
				FailureTick:    p.MillisToTickCeil(lastFailure),
				FailureMillis:  lastFailure,
				TriggerSource:  "retry_exhausted;credential_source=" + joinOracle(sources[id]),
				Constraint:     "REL-RETRY-003",
			}
			terminal[id] = true
			failed[id] = failure
			batchResult.Failed = append(batchResult.Failed, failure)
		}
		sort.Strings(batchResult.Actual)
		sort.Slice(batchResult.Failed, func(i, j int) bool {
			return batchResult.Failed[i].SubscriptionID < batchResult.Failed[j].SubscriptionID
		})
		result.Batches = append(result.Batches, batchResult)
	}
	for id := range actual {
		result.ActualUnion = append(result.ActualUnion, id)
	}
	for id := range failed {
		result.FailedUnion = append(result.FailedUnion, id)
	}
	sort.Strings(result.ActualUnion)
	sort.Strings(result.FailedUnion)
	combined := append(append([]string(nil), result.ActualUnion...), result.FailedUnion...)
	sort.Strings(combined)
	result.CoverageMatch = equalStrings(combined, full)
	return result, nil
}

func referenceTopoSubset(p *model.Project, subset []string) []string {
	wanted := make(map[string]bool)
	for _, id := range subset {
		wanted[id] = true
	}
	subs := p.SubscriptionByID()
	indegree := make(map[string]int)
	children := make(map[string][]string)
	for _, id := range subset {
		for _, dep := range subs[id].DependsOn {
			if wanted[dep] {
				indegree[id]++
				children[dep] = append(children[dep], id)
			}
		}
	}
	var ready []string
	for _, id := range subset {
		if indegree[id] == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	var out []string
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, id)
		sort.Strings(children[id])
		for _, child := range children[id] {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
				sort.Strings(ready)
			}
		}
	}
	return out
}

func referenceUpstreamFailure(deps []string, terminal map[string]bool, failed map[string]engine.ReloadFailure) string {
	values := append([]string(nil), deps...)
	sort.Strings(values)
	for _, dep := range values {
		if terminal[dep] || failed[dep].SubscriptionID != "" {
			return dep
		}
	}
	return ""
}

func joinOracle(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	out := values[0]
	for _, value := range values[1:] {
		out += "," + value
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func referenceGateRuntime(p *model.Project) engine.GateRuntime {
	runtime := engine.GateRuntime{
		Confirmed:          map[string]bool{},
		Arrived:            map[string]bool{},
		Committed:          map[string]string{},
		RequestedBatch:     map[string]int64{},
		LastCommittedBatch: map[string]int64{},
		WatermarkKnown:     true,
		RequireWatermark:   true,
	}
	creds := p.CredentialByID()
	for i, rotation := range p.Rotations {
		target := creds[rotation.CredentialID]
		owner := credentialOwner(target.ID)
		runtime.Committed[owner] = target.Supersedes
		runtime.Arrived[owner] = true
		runtime.Confirmed[owner] = i%2 == 0
		runtime.RequestedBatch[owner] = target.ReloadBatch
		runtime.LastCommittedBatch[owner] = target.ReloadBatch - 1
	}
	return runtime
}

func referenceGates(p *model.Project, runtime engine.GateRuntime) (*engine.GateResult, error) {
	creds := p.CredentialByID()
	targets := make(map[string]model.Credential)
	for _, rotation := range p.Rotations {
		targets[rotation.CredentialID] = creds[rotation.CredentialID]
	}
	atTick := int64(6)
	if atTick > p.HorizonTicks {
		atTick = p.HorizonTicks
	}
	now := atTick * p.TickMillis
	valid := func(c model.Credential) (bool, int64) {
		from := c.IssuedAtMillis
		if p.LastRotationWatermarkMillis > from {
			from = p.LastRotationWatermarkMillis
		}
		expiry := c.IssuedAtMillis + c.TTLMillis
		if now < from || now >= expiry {
			return false, 0
		}
		return true, expiry - now
	}
	result := &engine.GateResult{AtTick: atTick, Decisions: []engine.GateDecision{}}
	subs := append([]model.Subscription(nil), p.Subscriptions...)
	sort.Slice(subs, func(i, j int) bool { return subs[i].ID < subs[j].ID })
	for _, sub := range subs {
		var target *model.Credential
		for _, ref := range sub.Refs {
			c, ok := targets[ref.CredentialID]
			if !ok {
				continue
			}
			copy := c
			if target == nil || copy.Version > target.Version {
				target = &copy
			}
		}
		if target == nil {
			result.Decisions = append(result.Decisions, engine.GateDecision{
				SubscriptionID: sub.ID,
				Tick:           atTick,
				Decision:       "not_applicable",
				Source:         "no_suspended_rotation_reference",
				Reason:         "subscription has no rotation target reference",
			})
			continue
		}
		var committed *model.Credential
		if id := runtime.Committed[sub.ID]; id != "" {
			c := creds[id]
			committed = &c
		}
		requestedValid, requestedRemaining := valid(*target)
		committedValid, committedRemaining := false, int64(0)
		if committed != nil {
			committedValid, committedRemaining = valid(*committed)
		}
		if runtime.Confirmed[sub.ID] && runtime.Arrived[sub.ID] && requestedValid {
			result.Decisions = append(result.Decisions, engine.GateDecision{
				SubscriptionID:      sub.ID,
				Tick:                atTick,
				Decision:            "allow_new",
				CredentialID:        target.ID,
				CredentialVersion:   target.Version,
				RemainingMillis:     requestedRemaining,
				Source:              "suspended:" + target.ID + ";state=arrived_confirmed",
				Reason:              "new credential reload is confirmed and credential is valid",
				RequestedCredential: target.ID,
				CommittedCredential: committed.ID,
			})
			continue
		}
		reason := "new credential reload has not been confirmed"
		if committedValid {
			result.Decisions = append(result.Decisions, engine.GateDecision{
				SubscriptionID:      sub.ID,
				Tick:                atTick,
				Decision:            "fallback_old",
				CredentialID:        committed.ID,
				CredentialVersion:   committed.Version,
				RemainingMillis:     committedRemaining,
				Source:              "committed:" + committed.ID + ";state=last_valid",
				Reason:              reason + "; committed credential remains valid",
				RequestedCredential: target.ID,
				CommittedCredential: committed.ID,
			})
			continue
		}
		result.Decisions = append(result.Decisions, engine.GateDecision{
			SubscriptionID:      sub.ID,
			Tick:                atTick,
			Decision:            "reject",
			Code:                "E_GATE_NO_FALLBACK",
			Source:              "committed:none_valid",
			Reason:              reason + "; no committed credential remains valid",
			RequestedCredential: target.ID,
			CommittedCredential: committed.ID,
		})
	}
	return result, nil
}
