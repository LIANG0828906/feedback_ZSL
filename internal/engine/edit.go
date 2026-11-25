package engine

import (
	"sort"

	"sre-timeline/internal/bizerr"
	"sre-timeline/internal/model"
	"sre-timeline/internal/topology"
)

type InvalidationPlan struct {
	Source               string   `json:"source"`
	Target               string   `json:"target,omitempty"`
	Reason               string   `json:"reason"`
	OldHash              string   `json:"old_hash,omitempty"`
	NewHash              string   `json:"new_hash,omitempty"`
	ServiceClosure       []string `json:"service_closure,omitempty"`
	AffectedServices     []string `json:"affected_services,omitempty"`
	InvalidatedServices  []string `json:"invalidated_services,omitempty"`
	StaleIntervalKeys    []string `json:"stale_interval_keys,omitempty"`
	InvalidatedScoreKeys []string `json:"invalidated_score_keys,omitempty"`
	CacheCleared         bool     `json:"cache_cleared"`
	AlignmentRecomputed  bool     `json:"alignment_recomputed"`
	RankingRecomputed    bool     `json:"ranking_recomputed"`
}

func (e *Engine) newPlan(source, target, reason string) *InvalidationPlan {
	return &InvalidationPlan{Source: source, Target: target, Reason: reason, OldHash: e.hash, RankingRecomputed: true}
}

func (e *Engine) closureFor(serviceID string) []string {
	if e.topo == nil {
		return []string{serviceID}
	}
	return e.topo.ReverseClosure(serviceID)
}

func (e *Engine) scoreKeysForServices(services []string) []string {
	if e.last == nil {
		return nil
	}
	wanted := map[string]bool{}
	for _, id := range services {
		wanted[id] = true
	}
	keys := map[string]bool{}
	for _, target := range e.last.Targets {
		if wanted[target.Data.ServiceID] {
			keys[target.Data.Key] = true
		}
	}
	return sortedKeys(keys)
}

func (e *Engine) intervalKeysForService(serviceID string) []string {
	if e.last == nil {
		return nil
	}
	keys := map[string]bool{}
	for _, target := range e.last.Targets {
		if target.Data.ServiceID == serviceID {
			keys[target.Data.Key] = true
		}
	}
	return sortedKeys(keys)
}

func (e *Engine) invalidateScoreKeys(keys []string) {
	for _, key := range keys {
		delete(e.scoreCache, key)
	}
}

func (e *Engine) EditThreshold(serviceID string, thresholdBP int) (*InvalidationPlan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if thresholdBP < 0 {
		return nil, bizerr.New(bizerr.CodeThresholdNegative, "threshold", "threshold must not be negative").
			With("service", serviceID).With("value", thresholdBP)
	}
	idx := -1
	for i := range e.project.Services {
		if e.project.Services[i].ID == serviceID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, bizerr.New(bizerr.CodeEditTarget, "service_id", "unknown service %q", serviceID).With("service", serviceID)
	}
	plan := e.newPlan("threshold.edit", serviceID, "threshold changes interval predicate and global candidate aggregation")
	closure := e.closureFor(serviceID)
	plan.ServiceClosure = closure
	plan.StaleIntervalKeys = e.intervalKeysForService(serviceID)
	plan.InvalidatedScoreKeys = e.scoreKeysForServices(closure)
	plan.InvalidatedServices = []string{serviceID}

	delete(e.intervalCache, serviceID)
	e.invalidateScoreKeys(plan.InvalidatedScoreKeys)
	if serviceID == "all-metric-thresholds" {
		e.project.Services[idx].ErrorThresholdBP = thresholdBP
		e.project.Services[idx].SaturationThresholdBP = thresholdBP
	} else {
		e.project.Services[idx].ErrorThresholdBP = thresholdBP
	}
	plan.NewHash = ContentHash(e.project)
	e.hash = plan.NewHash
	e.LastPlan = plan
	return plan, nil
}

func (e *Engine) AddEvent(event model.Event) (*InvalidationPlan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if event.ID == "" {
		return nil, bizerr.New(bizerr.CodeSchemaField, "event.id", "event id is empty")
	}
	if _, ok := e.project.Service(event.ServiceID); !ok {
		return nil, bizerr.New(bizerr.CodeEditTarget, "event.service_id", "unknown service %q", event.ServiceID).With("service", event.ServiceID)
	}
	for _, existing := range e.project.Events {
		if existing.ID == event.ID {
			return nil, bizerr.New(bizerr.CodeEditTarget, "event.id", "event already exists").With("event", event.ID)
		}
	}
	if event.Seq <= 0 {
		maxSeq := 0
		for _, existing := range e.project.Events {
			if existing.Seq > maxSeq {
				maxSeq = existing.Seq
			}
		}
		event.Seq = maxSeq + 1
	}
	plan := e.newPlan("event.add", event.ID, "alignment/dedup and dependency-reachable scores may change")
	closure := e.closureFor(event.ServiceID)
	plan.ServiceClosure = closure
	plan.AlignmentRecomputed = true
	plan.InvalidatedScoreKeys = e.scoreKeysForServices(closure)
	if event.Source == model.SourceHealth || event.Source == model.SourceCapacity {
		plan.InvalidatedServices = []string{event.ServiceID}
		plan.StaleIntervalKeys = e.intervalKeysForService(event.ServiceID)
		delete(e.intervalCache, event.ServiceID)
	}
	e.invalidateScoreKeys(plan.InvalidatedScoreKeys)
	e.project.Events = append(e.project.Events, event)
	plan.NewHash = ContentHash(e.project)
	e.hash = plan.NewHash
	e.LastPlan = plan
	return plan, nil
}

func (e *Engine) CorrectEventTime(eventID string, newOccurredMS int64) (*InvalidationPlan, error) {
	return e.correctEvent(eventID, newOccurredMS, "")
}

func (e *Engine) MoveEventService(eventID, newServiceID string) (*InvalidationPlan, error) {
	return e.correctEvent(eventID, 0, newServiceID)
}

func (e *Engine) correctEvent(eventID string, newTimeMS int64, newServiceID string) (*InvalidationPlan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	idx := -1
	var old model.Event
	for i := range e.project.Events {
		if e.project.Events[i].ID == eventID {
			idx = i
			old = e.project.Events[i]
			break
		}
	}
	if idx < 0 {
		return nil, bizerr.New(bizerr.CodeEditTarget, "event.id", "unknown event %q", eventID).With("event", eventID)
	}
	if newServiceID != "" {
		if _, ok := e.project.Service(newServiceID); !ok {
			return nil, bizerr.New(bizerr.CodeEditTarget, "event.service_id", "unknown target service %q", newServiceID).With("service", newServiceID)
		}
	} else {
		newServiceID = old.ServiceID
	}
	closureSet := map[string]bool{}
	for _, id := range e.closureFor(old.ServiceID) {
		closureSet[id] = true
	}
	for _, id := range e.closureFor(newServiceID) {
		closureSet[id] = true
	}
	closure := sortedStringSet(closureSet)
	plan := e.newPlan("event.correct", eventID, "corrected time/service changes alignment, interval evidence and reachability")
	plan.ServiceClosure = closure
	plan.AlignmentRecomputed = true
	plan.InvalidatedScoreKeys = e.scoreKeysForServices(closure)
	metricEvent := old.Source == model.SourceHealth || old.Source == model.SourceCapacity
	if metricEvent {
		services := map[string]bool{old.ServiceID: true, newServiceID: true}
		for id := range services {
			delete(e.intervalCache, id)
			plan.InvalidatedServices = append(plan.InvalidatedServices, id)
			plan.StaleIntervalKeys = append(plan.StaleIntervalKeys, e.intervalKeysForService(id)...)
		}
	}
	plan.InvalidatedServices = sortedStringSet(stringSet(plan.InvalidatedServices))
	plan.StaleIntervalKeys = sortedStringSet(stringSet(plan.StaleIntervalKeys))
	e.invalidateScoreKeys(plan.InvalidatedScoreKeys)
	if newTimeMS != 0 || true {
		// Callers that only move service pass newTimeMS=0; zero is a valid epoch
		// relative value only in tests, so preserve old time via sentinel below.
	}
	if newTimeMS > 0 {
		e.project.Events[idx].OccurredMS = newTimeMS
	}
	if newServiceID != old.ServiceID {
		e.project.Events[idx].ServiceID = newServiceID
	}
	plan.NewHash = ContentHash(e.project)
	e.hash = plan.NewHash
	e.LastPlan = plan
	return plan, nil
}

func (e *Engine) AddEdge(caller, callee string) (*InvalidationPlan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.project.Service(caller); !ok {
		return nil, bizerr.New(bizerr.CodeEditTarget, "edge.caller", "unknown caller %q", caller).With("service", caller)
	}
	if _, ok := e.project.Service(callee); !ok {
		return nil, bizerr.New(bizerr.CodeEditTarget, "edge.callee", "unknown callee %q", callee).With("service", callee)
	}
	for _, edge := range e.project.Edges {
		if edge.Caller == caller && edge.Callee == callee {
			plan := e.newPlan("edge.add", caller+"->"+callee, "edge already exists")
			e.LastPlan = plan
			return plan, nil
		}
	}
	oldReach := e.topo.Reachability()
	e.project.Edges = append(e.project.Edges, model.Edge{Caller: caller, Callee: callee})
	newTopo, err := topology.New(e.project.Services, e.project.Edges)
	if err == nil {
		err = newTopo.ValidateAcyclic()
	}
	if err != nil {
		e.project.Edges = e.project.Edges[:len(e.project.Edges)-1]
		return nil, err
	}
	e.topo = newTopo
	plan := e.newPlan("edge.add", caller+"->"+callee, "dependency reachability changed")
	affected := affectedByReachability(oldReach, newTopo.Reachability())
	plan.AffectedServices = affected
	plan.ServiceClosure = affected
	plan.InvalidatedScoreKeys = e.scoreKeysForServices(affected)
	e.invalidateScoreKeys(plan.InvalidatedScoreKeys)
	plan.NewHash = ContentHash(e.project)
	e.hash = plan.NewHash
	e.LastPlan = plan
	return plan, nil
}

func (e *Engine) DeleteEdge(caller, callee string) (*InvalidationPlan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	oldReach := e.topo.Reachability()
	next := make([]model.Edge, 0, len(e.project.Edges))
	found := false
	for _, edge := range e.project.Edges {
		if edge.Caller == caller && edge.Callee == callee {
			found = true
			continue
		}
		next = append(next, edge)
	}
	if !found {
		return nil, bizerr.New(bizerr.CodeEditTarget, "edge", "edge not found").With("caller", caller).With("callee", callee)
	}
	e.project.Edges = next
	newTopo, err := topology.New(e.project.Services, e.project.Edges)
	if err != nil {
		return nil, err
	}
	e.topo = newTopo
	plan := e.newPlan("edge.delete", caller+"->"+callee, "dependency reachability changed")
	affected := affectedByReachability(oldReach, newTopo.Reachability())
	plan.AffectedServices = affected
	plan.ServiceClosure = affected
	plan.InvalidatedScoreKeys = e.scoreKeysForServices(affected)
	e.invalidateScoreKeys(plan.InvalidatedScoreKeys)
	plan.NewHash = ContentHash(e.project)
	e.hash = plan.NewHash
	e.LastPlan = plan
	return plan, nil
}

func (e *Engine) ReplaceSnapshot(next *model.Project) (*InvalidationPlan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	clone, err := CloneProject(next)
	if err != nil {
		return nil, err
	}
	topo, err := topology.New(clone.Services, clone.Edges)
	if err != nil {
		return nil, err
	}
	if err := topo.ValidateAcyclic(); err != nil {
		return nil, err
	}
	newHash := ContentHash(clone)
	cleared := newHash != e.hash || clone.Schema != e.project.Schema
	plan := e.newPlan("snapshot.replace", clone.ID, "normalized hash or schema variant changed")
	plan.NewHash = newHash
	plan.CacheCleared = cleared
	if cleared {
		e.intervalCache = map[string]*intervalCacheEntry{}
		e.scoreCache = map[string]*scoreCacheEntry{}
		e.last = nil
	}
	e.project = clone
	e.topo = topo
	e.hash = newHash
	e.LastPlan = plan
	return plan, nil
}

func affectedByReachability(oldReach, newReach map[string]map[string]int) []string {
	affected := map[string]bool{}
	for svc := range oldReach {
		if !sameDistanceMap(oldReach[svc], newReach[svc]) {
			affected[svc] = true
		}
	}
	for svc := range newReach {
		if !sameDistanceMap(oldReach[svc], newReach[svc]) {
			affected[svc] = true
		}
	}
	return sortedStringSet(affected)
}

func sameDistanceMap(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func sortedKeys(set map[string]bool) []string { return sortedStringSet(set) }

func stringSet(in []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range in {
		out[v] = true
	}
	return out
}

func sortedStringSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
