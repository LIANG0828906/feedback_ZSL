package hckernel

import (
	"fmt"
	"sort"
)

type Closure struct {
	EditID           string   `json:"edit_id"`
	Field            string   `json:"field"`
	AffectedInstance string   `json:"affected_instance"`
	FromRound        int      `json:"from_round"`
	ToRound          int      `json:"to_round"`
	FromAtMs         int64    `json:"from_at_ms"`
	EventCursorFrom  int      `json:"event_cursor_from"`
	Scopes           []string `json:"scopes"`
	DependencyChain  []string `json:"dependency_chain"`
}

type IncrementalResult struct {
	Closure          Closure `json:"closure"`
	Result           *Result `json:"result"`
	CacheHits        int     `json:"cache_hits"`
	RecomputedRounds []int   `json:"recomputed_rounds"`
	FullResult       *Result `json:"-"`
	MatchesFull      bool    `json:"matches_full"`
}

type ResultCache struct {
	Project   Project
	Events    []Event
	Result    *Result
	Snapshots []Checkpoint
}

// BuildCache runs the baseline once and retains per-round durable-shaped
// snapshots that can resume dependency closures.
func BuildCache(project *Project, events []Event) (*ResultCache, error) {
	engine, err := NewEngine(project, events)
	if err != nil {
		return nil, err
	}
	cache := &ResultCache{
		Project:   CloneProject(engine.project),
		Events:    append([]Event(nil), engine.events...),
		Snapshots: make([]Checkpoint, project.Rounds+1),
	}
	cache.Snapshots[0] = engine.Snapshot(0)
	for round := 1; round <= project.Rounds; round++ {
		now := int64(round-1) * project.StepMs
		if err := engine.Advance(now, round); err != nil {
			return nil, err
		}
		cache.Snapshots[round] = engine.Snapshot(round)
	}
	cache.Result = engine.Result()
	return cache, nil
}

// ApplyEdit applies one logical-time edit event, reuses the unaffected prefix,
// recomputes the explicit closure suffix, and compares it with a full replay.
func (c *ResultCache) ApplyEdit(edit Event) (*IncrementalResult, error) {
	if edit.Type != EventConfigEdit || edit.Edit == nil {
		return nil, newError(ErrEditFieldUnsupported, "ApplyEdit requires config_edited event", withEvent(edit.ID))
	}
	if c.Project.StepMs <= 0 {
		return nil, newError(ErrIntervalInvalid, "cache project step is invalid")
	}
	fromRound := int(edit.AtMs/c.Project.StepMs) + 1
	if fromRound < 1 {
		fromRound = 1
	}
	if fromRound > c.Project.Rounds+1 {
		return nil, newError(ErrEditFieldUnsupported, "edit time is after declared rounds", withEvent(edit.ID))
	}

	editedEvents := append(append([]Event(nil), c.Events...), edit)
	snapshot := c.Snapshots[fromRound-1]
	engine, err := RestoreEngine(&c.Project, editedEvents, &snapshot)
	if err != nil {
		return nil, err
	}
	sortEventsForCursor(engine)
	engine.eventCursor = countEventsBefore(engine.eventOrder, edit.AtMs)

	recomputedRounds := []int{}
	for round := fromRound; round <= c.Project.Rounds; round++ {
		now := int64(round-1) * c.Project.StepMs
		if err := engine.Advance(now, round); err != nil {
			return nil, err
		}
		recomputedRounds = append(recomputedRounds, round)
	}

	closure := Closure{
		EditID:           edit.ID,
		Field:            edit.Edit.Field,
		AffectedInstance: edit.InstanceID,
		FromRound:        fromRound,
		ToRound:          c.Project.Rounds,
		FromAtMs:         edit.AtMs,
		EventCursorFrom:  engine.eventCursor,
		Scopes:           closureScopes(edit.Edit.Field),
		DependencyChain: []string{
			"instance:" + edit.InstanceID + ":config",
			"round:" + fmt.Sprint(fromRound) + ":eligibility-or-threshold",
			"round:" + fmt.Sprint(fromRound) + ":budget-queue",
			"health-trace:" + edit.InstanceID,
		},
	}

	result := &Result{
		ProjectID:   c.Result.ProjectID,
		Version:     c.Result.Version,
		Fingerprint: c.Result.Fingerprint,
		FinalHealth: map[string]HealthState{},
		FinalCounts: map[string][2]int{},
	}

	cacheHits := 0
	prefixCount := fromRound - 1
	for i := 0; i < prefixCount && i < len(c.Result.Rounds); i++ {
		report := c.Result.Rounds[i]
		report.Provenance = &Provenance{
			Kind:               "baseline_cache",
			Source:             "per-round-snapshot",
			InvalidationSource: "none_before_" + edit.ID,
			CacheHits:          1,
		}
		result.Rounds = append(result.Rounds, report)
		cacheHits++
	}
	for _, report := range engine.reports {
		report.Provenance = &Provenance{
			Kind:               "recomputed",
			Source:             edit.Edit.Field,
			InvalidationSource: edit.ID,
			Dependencies:       closure.DependencyChain,
			RecomputedRounds:   []int{report.Round},
		}
		result.Rounds = append(result.Rounds, report)
	}

	for _, obs := range c.Result.HealthTrace {
		if obs.AtMs >= edit.AtMs {
			continue
		}
		obs.InvalidationSource = "baseline_cache"
		result.HealthTrace = append(result.HealthTrace, obs)
		cacheHits++
	}
	for _, obs := range engine.trace {
		if obs.InstanceID == edit.InstanceID {
			obs.InvalidationSource = edit.ID + ":" + edit.Edit.Field
		} else {
			obs.InvalidationSource = "dependency_closure:" + edit.ID
		}
		result.HealthTrace = append(result.HealthTrace, obs)
	}

	final := engine.Result()
	result.FinalHealth = final.FinalHealth
	result.FinalCounts = final.FinalCounts

	full, err := NewEngine(&c.Project, editedEvents)
	if err != nil {
		return nil, err
	}
	fullResult, err := full.Run()
	if err != nil {
		return nil, err
	}
	matches := ResultsEqualSemantic(result, fullResult)
	return &IncrementalResult{
		Closure:          closure,
		Result:           result,
		CacheHits:        cacheHits,
		RecomputedRounds: recomputedRounds,
		FullResult:       fullResult,
		MatchesFull:      matches,
	}, nil
}

func closureScopes(field string) []string {
	switch field {
	case "success_threshold", "failure_threshold":
		return []string{"health-threshold", "state-migration", "half-open-chain", "possible-budget-chain"}
	case "weight_ppm":
		return []string{"weighted-order", "budget-allocation", "deferred-queue", "other-instance-slot-chain"}
	case "rack_id":
		return []string{"rack-capacity", "global-capacity", "deferred-queue", "old-and-new-rack-chain"}
	default:
		return []string{"eligibility", "timeout-backoff", "budget-allocation", "deferred-queue"}
	}
}

func sortEventsForCursor(e *Engine) {
	sort.SliceStable(e.eventOrder, func(i, j int) bool {
		if e.eventOrder[i].AtMs != e.eventOrder[j].AtMs {
			return e.eventOrder[i].AtMs < e.eventOrder[j].AtMs
		}
		return e.eventOrder[i].ID < e.eventOrder[j].ID
	})
}

func countEventsBefore(events []Event, atMs int64) int {
	count := 0
	for _, ev := range events {
		if ev.AtMs < atMs {
			count++
		}
	}
	return count
}

// ResultsEqualSemantic compares all deterministic scheduling and health
// fields while ignoring incremental provenance annotations.
func ResultsEqualSemantic(a, b *Result) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.Rounds) != len(b.Rounds) || len(a.HealthTrace) != len(b.HealthTrace) {
		return false
	}
	for i := range a.Rounds {
		ar := a.Rounds[i]
		br := b.Rounds[i]
		ar.Provenance = nil
		br.Provenance = nil
		if !roundEqual(ar, br) {
			return false
		}
	}
	for i := range a.HealthTrace {
		ao := a.HealthTrace[i]
		bo := b.HealthTrace[i]
		ao.InvalidationSource = ""
		bo.InvalidationSource = ""
		if ao != bo {
			return false
		}
	}
	return true
}

func roundEqual(a, b RoundReport) bool {
	if a.Round != b.Round || a.NowMs != b.NowMs {
		return false
	}
	if !stringSlicesEqual(a.Eligible, b.Eligible) {
		return false
	}
	if len(a.Dispatched) != len(b.Dispatched) || len(a.Suppressed) != len(b.Suppressed) ||
		len(a.Queue) != len(b.Queue) || len(a.Dropped) != len(b.Dropped) ||
		len(a.Rescheduled) != len(b.Rescheduled) {
		return false
	}
	for i := range a.Dispatched {
		if a.Dispatched[i] != b.Dispatched[i] {
			return false
		}
	}
	for i := range a.Suppressed {
		if a.Suppressed[i] != b.Suppressed[i] {
			return false
		}
	}
	for i := range a.Queue {
		if a.Queue[i] != b.Queue[i] {
			return false
		}
	}
	for i := range a.Dropped {
		if a.Dropped[i] != b.Dropped[i] {
			return false
		}
	}
	for i := range a.Rescheduled {
		if a.Rescheduled[i] != b.Rescheduled[i] {
			return false
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
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
