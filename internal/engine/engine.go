package engine

import (
	"context"
	"sort"
	"sync"

	"sre-timeline/internal/bizerr"
	"sre-timeline/internal/concurrency"
	"sre-timeline/internal/intervals"
	"sre-timeline/internal/model"
	"sre-timeline/internal/schemaio"
	"sre-timeline/internal/scoring"
	"sre-timeline/internal/timeline"
	"sre-timeline/internal/topology"
)

type Options struct {
	Concurrency concurrency.Config
}

type Diagnostics struct {
	WorkerBudget        int              `json:"worker_budget"`
	BufferLimit         int              `json:"buffer_limit"`
	ServiceDurationsNS  map[string]int64 `json:"service_durations_ns"`
	TargetDurationsNS   map[string]int64 `json:"target_durations_ns"`
	RecomputedServices  []string         `json:"recomputed_services"`
	ReusedServices      []string         `json:"reused_services"`
	RecomputedScoreKeys []string         `json:"recomputed_score_keys"`
	ReusedScoreKeys     []string         `json:"reused_score_keys"`
}

type Analysis struct {
	Hash         string                              `json:"hash"`
	Schema       string                              `json:"schema"`
	Alignment    *timeline.Report                    `json:"alignment"`
	Intervals    map[string]*intervals.ServiceResult `json:"intervals"`
	Targets      []scoring.Target                    `json:"targets"`
	Scoring      *scoring.Report                     `json:"scoring"`
	Propagations []topology.PropagationResult        `json:"propagations,omitempty"`
	Diagnostics  Diagnostics                         `json:"diagnostics"`
	Invalidation *InvalidationPlan                   `json:"invalidation,omitempty"`
	KeptEvents   []model.Event                       `json:"-"`
}

type intervalCacheEntry struct {
	fingerprint string
	result      *intervals.ServiceResult
}

type scoreCacheEntry struct {
	contributions []scoring.Contribution
}

type Engine struct {
	mu            sync.Mutex
	project       *model.Project
	options       Options
	hash          string
	topo          *topology.Topology
	intervalCache map[string]*intervalCacheEntry
	scoreCache    map[string]*scoreCacheEntry
	last          *Analysis
	LastPlan      *InvalidationPlan
}

func New(project *model.Project, options Options) (*Engine, error) {
	clone, err := CloneProject(project)
	if err != nil {
		return nil, err
	}
	cfg := options.Concurrency.Normalize()
	topo, err := topology.New(clone.Services, clone.Edges)
	if err != nil {
		return nil, err
	}
	if err := topo.ValidateAcyclic(); err != nil {
		return nil, err
	}
	return &Engine{
		project:       clone,
		options:       Options{Concurrency: cfg},
		hash:          ContentHash(clone),
		topo:          topo,
		intervalCache: map[string]*intervalCacheEntry{},
		scoreCache:    map[string]*scoreCacheEntry{},
	}, nil
}

func (e *Engine) Project() *model.Project      { return e.project }
func (e *Engine) Hash() string                 { return e.hash }
func (e *Engine) Topology() *topology.Topology { return e.topo }
func (e *Engine) LastAnalysis() *Analysis      { return e.last }

type serviceOutput struct {
	result *intervals.ServiceResult
	reused bool
}

type scoreOutput struct {
	contributions []scoring.Contribution
	reused        bool
}

func (e *Engine) Analyze(ctx context.Context) (*Analysis, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	cfg := e.options.Concurrency.Normalize()
	if err := schemaio.ValidateCanonical(e.project); err != nil {
		return nil, err
	}
	topo, err := topology.New(e.project.Services, e.project.Edges)
	if err != nil {
		return nil, err
	}
	if err := topo.ValidateAcyclic(); err != nil {
		return nil, err
	}
	e.topo = topo
	e.hash = ContentHash(e.project)

	aligned, err := timeline.Align(e.project)
	if err != nil {
		return nil, err
	}
	serviceIDs := e.project.ServiceIDs()
	serviceOutputs, serviceDurations, err := concurrency.RunOrdered(ctx, cfg, serviceIDs, func(serviceID string) (serviceOutput, error) {
		buffer, bufferErr := concurrency.NewServiceBuffer[model.Event](serviceID, cfg.BufferLimit)
		if bufferErr != nil {
			return serviceOutput{}, bufferErr
		}
		serviceEventCount := 0
		for _, ev := range e.project.Events {
			if ev.ServiceID != serviceID {
				continue
			}
			serviceEventCount++
			if pushErr := buffer.TryPush(ev); pushErr != nil {
				return serviceOutput{}, pushErr.(*bizerr.Error).
					With("event_count", serviceEventCount)
			}
		}
		_ = buffer.Items()

		fingerprint := serviceFingerprint(e.project, serviceID)
		if cached, ok := e.intervalCache[serviceID]; ok && cached.fingerprint == fingerprint {
			return serviceOutput{result: cached.result, reused: true}, nil
		}
		result, deriveErr := intervals.DeriveService(e.project, aligned, serviceID)
		if deriveErr != nil {
			return serviceOutput{}, deriveErr
		}
		return serviceOutput{result: result}, nil
	})
	if err != nil {
		return nil, err
	}

	intervalResults := map[string]*intervals.ServiceResult{}
	diagnostics := Diagnostics{
		WorkerBudget: cfg.Workers, BufferLimit: cfg.BufferLimit,
		ServiceDurationsNS: serviceDurations, TargetDurationsNS: map[string]int64{},
	}
	for _, id := range serviceIDs {
		output := serviceOutputs[id]
		intervalResults[id] = output.result
		e.intervalCache[id] = &intervalCacheEntry{fingerprint: serviceFingerprint(e.project, id), result: output.result}
		if output.reused {
			diagnostics.ReusedServices = append(diagnostics.ReusedServices, id)
		} else {
			diagnostics.RecomputedServices = append(diagnostics.RecomputedServices, id)
		}
	}

	targets := scoring.NumberTargets(intervalResults)
	targetByKey := map[string]scoring.Target{}
	scoreKeys := make([]string, 0, len(targets))
	for _, target := range targets {
		targetByKey[target.Data.Key] = target
		scoreKeys = append(scoreKeys, target.Data.Key)
	}
	scoreOutputs, targetDurations, err := concurrency.RunOrdered(ctx, cfg, scoreKeys, func(key string) (scoreOutput, error) {
		target := targetByKey[key]
		if cached, ok := e.scoreCache[key]; ok {
			return scoreOutput{contributions: cached.contributions, reused: true}, nil
		}
		kept := keptEvents(e.project, aligned)
		contribs, _, scoreErr := scoring.ScoreTarget(e.project, e.topo, target, kept)
		if scoreErr != nil {
			return scoreOutput{}, scoreErr
		}
		return scoreOutput{contributions: contribs}, nil
	})
	if err != nil {
		return nil, err
	}

	allContributions := make([]scoring.Contribution, 0)
	for _, target := range targets {
		key := target.Data.Key
		output := scoreOutputs[key]
		contribs := output.contributions
		for i := range contribs {
			contribs[i].IntervalOrdinal = target.Ordinal
			contribs[i].IntervalID = target.ID
		}
		if output.reused {
			diagnostics.ReusedScoreKeys = append(diagnostics.ReusedScoreKeys, key)
		} else {
			diagnostics.RecomputedScoreKeys = append(diagnostics.RecomputedScoreKeys, key)
			e.scoreCache[key] = &scoreCacheEntry{contributions: contribs}
		}
		allContributions = append(allContributions, contribs...)
	}
	diagnostics.TargetDurationsNS = targetDurations
	sort.Strings(diagnostics.RecomputedServices)
	sort.Strings(diagnostics.ReusedServices)
	sort.Strings(diagnostics.RecomputedScoreKeys)
	sort.Strings(diagnostics.ReusedScoreKeys)

	kept := keptEvents(e.project, aligned)
	scoreReport := scoring.Aggregate(targets, allContributions, kept)
	propagations := e.rootPropagations(scoreReport)
	analysis := &Analysis{
		Hash: e.hash, Schema: e.project.Schema, Alignment: aligned, Intervals: intervalResults,
		Targets: targets, Scoring: scoreReport, Diagnostics: diagnostics,
		Propagations: propagations, Invalidation: e.LastPlan, KeptEvents: kept,
	}
	e.last = analysis
	return analysis, nil
}

func (e *Engine) rootPropagations(report *scoring.Report) []topology.PropagationResult {
	sources := map[string]bool{}
	if len(report.Candidates) > 0 {
		sources[report.Candidates[0].ServiceID] = true
	}
	for _, candidate := range report.Candidates {
		if candidate.Source == model.SourceChange && candidate.ContributionCount > 0 {
			sources[candidate.ServiceID] = true
		}
	}
	ordered := make([]string, 0, len(sources))
	for source := range sources {
		ordered = append(ordered, source)
	}
	sort.Strings(ordered)
	results := make([]topology.PropagationResult, 0, len(ordered))
	for _, source := range ordered {
		if result, err := e.topo.Propagate(source, e.project.Policy.PropagationGen, topology.DefaultDecayBP); err == nil {
			results = append(results, result)
		}
	}
	return results
}

func keptEvents(p *model.Project, report *timeline.Report) []model.Event {
	byID := map[string]model.Event{}
	for _, ev := range p.Events {
		byID[ev.ID] = ev
	}
	out := make([]model.Event, 0, len(report.Kept))
	for _, item := range report.Kept {
		if ev, ok := byID[item.EventID]; ok {
			out = append(out, ev)
		}
	}
	return out
}
