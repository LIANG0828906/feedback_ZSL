package slikernel

import (
	"context"
	"runtime"
	"sort"
	"sync"
	"time"
)

type Options struct {
	Workers            int    `json:"workers"`
	PerWorkerBuffer    int    `json:"per_worker_buffer"`
	BatchSize          int    `json:"batch_size,omitempty"`
	SubmissionOrder    string `json:"submission_order,omitempty"`
	FailOnBackpressure bool   `json:"fail_on_backpressure,omitempty"`
}

func DefaultOptions() Options {
	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	if workers > 4 {
		workers = 4
	}
	return Options{Workers: workers, PerWorkerBuffer: DefaultPerWorkerBuffer, SubmissionOrder: "forward"}
}

func normalizeOptions(options Options) (Options, error) {
	if options.Workers == 0 && options.PerWorkerBuffer == 0 {
		options = DefaultOptions()
	}
	if options.SubmissionOrder == "" {
		options.SubmissionOrder = "forward"
	}
	if options.BatchSize < 0 {
		return Options{}, biz(CodeBufferBudget, "validate_options", nil, ctx("batch_size", options.BatchSize))
	}
	if options.Workers < 1 || options.Workers > MaxWorkers {
		return Options{}, biz(CodeThreadBudget, "validate_options", nil, ctx("workers", options.Workers))
	}
	if options.PerWorkerBuffer < 1 || options.PerWorkerBuffer > MaxPerWorkerBuffer {
		return Options{}, biz(CodeBufferBudget, "validate_options", nil, ctx("per_worker_buffer", options.PerWorkerBuffer))
	}
	capacity := options.Workers * options.PerWorkerBuffer
	if options.BatchSize > capacity {
		return Options{}, biz(CodeBufferBudget, "validate_options", nil,
			ctx2("batch_size", options.BatchSize, "capacity", capacity))
	}
	switch options.SubmissionOrder {
	case "forward", "reverse", "shuffle":
	default:
		return Options{}, biz(CodeInvalidInput, "validate_options", nil,
			ctx("submission_order", options.SubmissionOrder))
	}
	return options, nil
}

type BackpressureSignal struct {
	Code     Code   `json:"code"`
	Stage    string `json:"stage"`
	Pending  int    `json:"pending"`
	Capacity int    `json:"capacity"`
	Batch    int    `json:"batch"`
	Action   string `json:"action"`
}

type WindowDuration struct {
	Stage          string `json:"stage"`
	Service        string `json:"service"`
	SLI            string `json:"sli"`
	Index          int64  `json:"index"`
	DurationMicros int64  `json:"duration_micros"`
}

type Diagnostics struct {
	Durations    []WindowDuration     `json:"durations,omitempty"`
	Backpressure []BackpressureSignal `json:"backpressure,omitempty"`
	Recomputed   []RecomputedWindow   `json:"recomputed,omitempty"`
	Reused       []RecomputedWindow   `json:"reused,omitempty"`
}

type RecomputedWindow struct {
	Stage   string `json:"stage"`
	Service string `json:"service"`
	SLI     string `json:"sli"`
	Index   int64  `json:"index"`
}

type ResultKey struct {
	Service   string `json:"service"`
	SLI       string `json:"sli"`
	EvalIndex int64  `json:"eval_index"`
}

type GroupAgg struct {
	StatusCode       int   `json:"status_code"`
	Events           int64 `json:"events"`
	Errors           int64 `json:"errors"`
	LatencySamples   int64 `json:"latency_samples"`
	LatencyFailures  int64 `json:"latency_failures"`
	ThroughputEvents int64 `json:"throughput_events"`
	QuantileUs       int64 `json:"quantile_us,omitempty"`
}

type BaseAgg struct {
	Service              string                 `json:"service"`
	SLI                  string                 `json:"sli"`
	Route                string                 `json:"route"`
	Kind                 SLIKind                `json:"kind"`
	AggregationRevision  string                 `json:"aggregation_revision"`
	WindowIndex          int64                  `json:"window_index"`
	StartMs              int64                  `json:"start_ms"`
	EndMs                int64                  `json:"end_ms"`
	Events               int64                  `json:"events"`
	Errors               int64                  `json:"errors"`
	LatencySamplesUs     []int64                `json:"latency_samples_us,omitempty"`
	LatencyFailures      int64                  `json:"latency_failures"`
	ObservedThroughput   int64                  `json:"observed_throughput"`
	QuantileUs           int64                  `json:"quantile_us,omitempty"`
	Groups               []GroupAgg             `json:"groups,omitempty"`
	StatusLatencySamples []StatusLatencySamples `json:"status_latency_samples,omitempty"`
	HasData              bool                   `json:"has_data"`
	MaxObservationSeq    int64                  `json:"max_observation_seq"`
}

type StatusLatencySamples struct {
	StatusCode int     `json:"status_code"`
	SamplesUs  []int64 `json:"samples_us"`
}

type BudgetResult struct {
	TargetPPM       int64 `json:"target_ppm"`
	Total           int64 `json:"total"`
	Failures        int64 `json:"failures"`
	AllowedErrors   int64 `json:"allowed_errors"`
	ConsumedPPM     int64 `json:"consumed_ppm"`
	RemainingErrors int64 `json:"remaining_errors"`
	DeficitErrors   int64 `json:"deficit_errors"`
	Exhausted       bool  `json:"exhausted"`
}

type ThresholdState struct {
	ID                  string `json:"id"`
	BurnPPM             int64  `json:"burn_ppm"`
	ConsecutiveRequired int    `json:"consecutive_required"`
	ConsecutiveObserved int    `json:"consecutive_observed"`
	Matched             bool   `json:"matched"`
	Fired               bool   `json:"fired"`
}

type AlertResult struct {
	Alert             bool             `json:"alert"`
	GatePass          bool             `json:"gate_pass"`
	MatchedThresholds []string         `json:"matched_thresholds,omitempty"`
	FiredThresholds   []string         `json:"fired_thresholds,omitempty"`
	PrimaryThreshold  string           `json:"primary_threshold,omitempty"`
	Thresholds        []ThresholdState `json:"thresholds,omitempty"`
}

type Provenance struct {
	InvalidationIDs      []string         `json:"invalidation_ids,omitempty"`
	PropagationClosure   []DependencyNode `json:"propagation_closure,omitempty"`
	AggregationFromCache bool             `json:"aggregation_from_cache"`
	BudgetFromCache      bool             `json:"budget_from_cache"`
	AlertFromCache       bool             `json:"alert_from_cache"`
	FromCache            bool             `json:"from_cache"`
}

type WindowResult struct {
	Service             string       `json:"service"`
	SLI                 string       `json:"sli"`
	Route               string       `json:"route"`
	Kind                SLIKind      `json:"kind"`
	EvalIndex           int64        `json:"eval_index"`
	StartMs             int64        `json:"start_ms"`
	EndMs               int64        `json:"end_ms"`
	AggregationRevision string       `json:"aggregation_revision"`
	SLIValuePPM         int64        `json:"sli_value_ppm"`
	QuantileUs          int64        `json:"quantile_us,omitempty"`
	Total               int64        `json:"total"`
	Failures            int64        `json:"failures"`
	NoData              bool         `json:"no_data,omitempty"`
	Groups              []GroupAgg   `json:"groups,omitempty"`
	Budget              BudgetResult `json:"budget"`
	Alert               AlertResult  `json:"alert"`
	Provenance          Provenance   `json:"provenance"`
}

func (result WindowResult) Key() ResultKey {
	return ResultKey{Service: result.Service, SLI: result.SLI, EvalIndex: result.EvalIndex}
}

type Report struct {
	ProjectID               string         `json:"project_id"`
	RuleVersion             string         `json:"rule_version"`
	CommittedObservationSeq int64          `json:"committed_observation_seq"`
	Results                 []WindowResult `json:"results"`
	Diagnostics             Diagnostics    `json:"diagnostics,omitempty"`
}

type Engine struct {
	mu          sync.Mutex
	project     Project
	options     Options
	ruleVersion string
	baseCache   map[baseCacheKey]*BaseAgg
	results     map[resultCacheKey]*WindowResult
}

type baseCacheKey struct {
	Service  string
	SLI      string
	Revision string
	Index    int64
}

type resultCacheKey struct {
	Service  string
	SLI      string
	Revision string
	Index    int64
}

func NewEngine(project Project, options Options) (*Engine, error) {
	normalized, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	clone := CloneProject(project)
	if err := ValidateProject(clone); err != nil {
		return nil, err
	}
	return &Engine{
		project: clone, options: normalized, ruleVersion: ProjectRuleVersion(clone),
		baseCache: make(map[baseCacheKey]*BaseAgg), results: make(map[resultCacheKey]*WindowResult),
	}, nil
}

func (engine *Engine) Project() Project {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return CloneProject(engine.project)
}

func (engine *Engine) RuleVersion() string {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.ruleVersion
}

func (engine *Engine) Options() Options {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.options
}

// CachedReport returns the current committed in-memory result without
// recomputing. It is primarily useful after checkpoint recovery.
func (engine *Engine) CachedReport() (*Report, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	evalTasks, err := allEvalTasks(engine.project)
	if err != nil {
		return nil, err
	}
	if len(engine.results) != len(evalTasks) || len(engine.baseCache) == 0 {
		return nil, biz(CodeCheckpointCorrupt, "cached_report", nil, map[string]any{
			"eval_cached": len(engine.results), "eval_expected": len(evalTasks),
		})
	}
	return engine.buildReport(engine.results), nil
}

func (engine *Engine) ComputeAll(ctx context.Context) (*Report, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.computeAllLocked(ctx)
}

func (engine *Engine) computeAllLocked(ctx context.Context) (*Report, error) {
	baseTasks, err := allBaseTasks(engine.project)
	if err != nil {
		return nil, err
	}
	newBaseCache := make(map[baseCacheKey]*BaseAgg, len(baseTasks))
	baseOutput, err := engine.runBaseTasks(ctx, "aggregate", baseTasks, func(task baseTask) (*BaseAgg, error) {
		return aggregateBase(engine.project, task), nil
	})
	if err != nil {
		return nil, err
	}
	for index, task := range baseTasks {
		newBaseCache[task.key()] = baseOutput.results[index]
	}

	evalTasks, err := allEvalTasks(engine.project)
	if err != nil {
		return nil, err
	}
	evalOutput, err := engine.runEvalTasks(ctx, "evaluate", evalTasks, newBaseCache, func(task evalTask) (*WindowResult, error) {
		return composeEvaluation(engine.project, task, newBaseCache)
	})
	if err != nil {
		return nil, err
	}
	newResults := make(map[resultCacheKey]*WindowResult, len(evalTasks))
	for index, task := range evalTasks {
		newResults[task.cacheKey()] = evalOutput.results[index]
	}
	applyAllAlerts(engine.project, newResults, nil, nil, nil)

	report := engine.buildReport(newResults)
	report.Diagnostics.Backpressure = append(baseOutput.signals, evalOutput.signals...)
	report.Diagnostics.Durations = append(
		baseWindowDurations(baseTasks, baseOutput.durations, "aggregate"),
		evalWindowDurations(evalTasks, evalOutput.durations, "evaluate")...,
	)
	for _, task := range evalTasks {
		report.Diagnostics.Recomputed = append(report.Diagnostics.Recomputed, RecomputedWindow{
			Stage: "evaluate", Service: task.service, SLI: task.sli.ID, Index: task.evalIndex,
		})
	}
	engine.baseCache = newBaseCache
	engine.results = newResults
	return report, nil
}

type baseTask struct {
	position int
	service  string
	sli      SLI
	index    int64
}

func (task baseTask) key() baseCacheKey {
	return baseCacheKey{
		Service: task.service, SLI: task.sli.ID,
		Revision: SLIAggregationRevision(task.service, task.sli), Index: task.index,
	}
}

type evalTask struct {
	position  int
	service   string
	sli       SLI
	evalIndex int64
}

func (task evalTask) key() ResultKey {
	return ResultKey{Service: task.service, SLI: task.sli.ID, EvalIndex: task.evalIndex}
}

func (task evalTask) cacheKey() resultCacheKey {
	return resultCacheKey{
		Service: task.service, SLI: task.sli.ID,
		Revision: SLIAggregationRevision(task.service, task.sli), Index: task.evalIndex,
	}
}

func allBaseTasks(project Project) ([]baseTask, error) {
	var tasks []baseTask
	for _, service := range project.Services {
		for _, sli := range service.SLIs {
			start, end, err := baseWindowRange(project, sli)
			if err != nil {
				return nil, err
			}
			for index := start; index < end; index++ {
				tasks = append(tasks, baseTask{service: service.Name, sli: sli, index: index})
			}
		}
	}
	for index := range tasks {
		tasks[index].position = index
	}
	return tasks, nil
}

func allEvalTasks(project Project) ([]evalTask, error) {
	var tasks []evalTask
	for _, service := range project.Services {
		for _, sli := range service.SLIs {
			indices, err := evaluationIndices(project, sli)
			if err != nil {
				return nil, err
			}
			for _, index := range indices {
				tasks = append(tasks, evalTask{service: service.Name, sli: sli, evalIndex: index})
			}
		}
	}
	for index := range tasks {
		tasks[index].position = index
	}
	return tasks, nil
}

func baseWindowRange(project Project, sli SLI) (int64, int64, error) {
	if (project.EvaluationStartMs-project.TimeOriginMs)%sli.WindowMs != 0 ||
		(project.EvaluationEndMs-project.TimeOriginMs)%sli.WindowMs != 0 {
		return 0, 0, biz(CodeWindowLength, "base_window_range", nil, ctx("sli", sli.ID))
	}
	start := (project.EvaluationStartMs - project.TimeOriginMs) / sli.WindowMs
	end := (project.EvaluationEndMs - project.TimeOriginMs) / sli.WindowMs
	return start, end, nil
}

func evaluationIndices(project Project, sli SLI) ([]int64, error) {
	start, end, err := baseWindowRange(project, sli)
	if err != nil {
		return nil, err
	}
	if int64(sli.RollingWindows) > end-start {
		return nil, biz(CodeWindowLength, "evaluation_indices", nil,
			ctx3("sli", sli.ID, "rolling_windows", sli.RollingWindows, "base_windows", end-start))
	}
	first := start + int64(sli.RollingWindows) - 1
	indices := make([]int64, 0, end-first)
	for index := first; index < end; index++ {
		indices = append(indices, index)
	}
	return indices, nil
}

func aggregateBase(project Project, task baseTask) *BaseAgg {
	sli := task.sli
	start := project.TimeOriginMs + task.index*sli.WindowMs
	end := start + sli.WindowMs
	aggregation := &BaseAgg{
		Service: task.service, SLI: sli.ID, Route: sli.Route, Kind: sli.Kind,
		AggregationRevision: SLIAggregationRevision(task.service, sli),
		WindowIndex:         task.index, StartMs: start, EndMs: end,
	}
	groups := make(map[int]*GroupAgg)
	groupLatencies := make(map[int][]int64)
	for _, observation := range project.Observations {
		if observation.Service != task.service || !routeMatches(sli.Route, observation.Route) {
			continue
		}
		if !statusMatches(sli.StatusCodes, observation.StatusCode) {
			continue
		}
		if observation.TimestampMs < start || observation.TimestampMs >= end {
			continue
		}
		aggregation.HasData = true
		aggregation.MaxObservationSeq = maxInt64(aggregation.MaxObservationSeq, observation.Seq)
		group := groups[observation.StatusCode]
		if group == nil {
			group = &GroupAgg{StatusCode: observation.StatusCode}
			groups[observation.StatusCode] = group
		}
		switch sli.Kind {
		case KindAvailability:
			aggregation.Events += observation.Events
			aggregation.Errors += observation.Errors
			group.Events += observation.Events
			group.Errors += observation.Errors
		case KindLatency:
			for _, latency := range observation.LatencySamplesUs {
				aggregation.LatencySamplesUs = append(aggregation.LatencySamplesUs, latency)
				groupLatencies[observation.StatusCode] = append(groupLatencies[observation.StatusCode], latency)
				group.LatencySamples++
				if latency > sli.LatencyThresholdUs {
					aggregation.LatencyFailures++
					group.LatencyFailures++
				}
			}
		case KindThroughput:
			aggregation.ObservedThroughput += observation.ThroughputEvents
			group.ThroughputEvents += observation.ThroughputEvents
		}
	}
	if sli.Kind == KindLatency {
		sort.Slice(aggregation.LatencySamplesUs, func(i, j int) bool {
			return aggregation.LatencySamplesUs[i] < aggregation.LatencySamplesUs[j]
		})
		aggregation.QuantileUs = nearestRankQuantile(aggregation.LatencySamplesUs, sli.QuantilePPM)
	}
	for _, group := range groups {
		if samples := groupLatencies[group.StatusCode]; len(samples) > 0 {
			sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
			group.QuantileUs = nearestRankQuantile(samples, sli.QuantilePPM)
			aggregation.StatusLatencySamples = append(aggregation.StatusLatencySamples, StatusLatencySamples{
				StatusCode: group.StatusCode, SamplesUs: append([]int64(nil), samples...),
			})
		}
		aggregation.Groups = append(aggregation.Groups, *group)
	}
	sort.Slice(aggregation.Groups, func(i, j int) bool {
		return aggregation.Groups[i].StatusCode < aggregation.Groups[j].StatusCode
	})
	sort.Slice(aggregation.StatusLatencySamples, func(i, j int) bool {
		return aggregation.StatusLatencySamples[i].StatusCode < aggregation.StatusLatencySamples[j].StatusCode
	})
	return aggregation
}

func routeMatches(pattern, route string) bool {
	return pattern == "*" || pattern == route
}

func statusMatches(allowed []int, status int) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if candidate == status {
			return true
		}
	}
	return false
}

func composeEvaluation(project Project, task evalTask, baseCache map[baseCacheKey]*BaseAgg) (*WindowResult, error) {
	sli := task.sli
	first := task.evalIndex - int64(sli.RollingWindows) + 1
	result := &WindowResult{
		Service: task.service, SLI: sli.ID, Route: sli.Route, Kind: sli.Kind, EvalIndex: task.evalIndex,
		AggregationRevision: SLIAggregationRevision(task.service, sli),
		StartMs:             project.TimeOriginMs + first*sli.WindowMs,
		EndMs:               project.TimeOriginMs + (task.evalIndex+1)*sli.WindowMs,
	}
	groups := make(map[int]*GroupAgg)
	groupLatencies := make(map[int][]int64)
	var allLatencies []int64
	var expectedThroughput int64
	result.NoData = true
	for index := first; index <= task.evalIndex; index++ {
		key := baseCacheKey{
			Service: task.service, SLI: sli.ID,
			Revision: result.AggregationRevision, Index: index,
		}
		base := baseCache[key]
		if base == nil {
			return nil, biz(CodeInternal, "compose_evaluation", nil,
				ctx3("service", task.service, "sli", sli.ID, "base_index", index))
		}
		if base.HasData {
			result.NoData = false
		}
		switch sli.Kind {
		case KindAvailability:
			result.Total += base.Events
			result.Failures += base.Errors
		case KindLatency:
			allLatencies = append(allLatencies, base.LatencySamplesUs...)
			result.Total += int64(len(base.LatencySamplesUs))
			result.Failures += base.LatencyFailures
		case KindThroughput:
			expectedThroughput += sli.ExpectedEventsPerWindow
			result.Total += base.ObservedThroughput
		}
		for _, source := range base.Groups {
			target := groups[source.StatusCode]
			if target == nil {
				target = &GroupAgg{StatusCode: source.StatusCode}
				groups[source.StatusCode] = target
			}
			target.Events += source.Events
			target.Errors += source.Errors
			target.LatencySamples += source.LatencySamples
			target.LatencyFailures += source.LatencyFailures
			target.ThroughputEvents += source.ThroughputEvents
		}
		for _, source := range base.StatusLatencySamples {
			groupLatencies[source.StatusCode] = append(groupLatencies[source.StatusCode], source.SamplesUs...)
		}
	}
	if sli.Kind == KindThroughput {
		result.Failures = maxInt64(0, expectedThroughput-result.Total)
		result.Total = expectedThroughput
		result.SLIValuePPM = ratioPPM(expectedThroughput-result.Failures, expectedThroughput)
	} else {
		result.SLIValuePPM = ratioPPM(result.Total-result.Failures, result.Total)
	}
	if sli.Kind == KindLatency {
		sort.Slice(allLatencies, func(i, j int) bool { return allLatencies[i] < allLatencies[j] })
		result.QuantileUs = nearestRankQuantile(allLatencies, sli.QuantilePPM)
	}
	for _, group := range groups {
		if samples := groupLatencies[group.StatusCode]; len(samples) > 0 {
			sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
			group.QuantileUs = nearestRankQuantile(samples, sli.QuantilePPM)
		}
		result.Groups = append(result.Groups, *group)
	}
	sort.Slice(result.Groups, func(i, j int) bool {
		return result.Groups[i].StatusCode < result.Groups[j].StatusCode
	})
	result.Budget = deriveBudget(sli, result.Total, result.Failures)
	return result, nil
}

func deriveBudget(sli SLI, total, failures int64) BudgetResult {
	allowed := mulDivFloor(total, PPM-sli.TargetPPM, PPM)
	budget := BudgetResult{
		TargetPPM: sli.TargetPPM, Total: total, Failures: failures, AllowedErrors: allowed,
		RemainingErrors: maxInt64(0, allowed-failures),
		DeficitErrors:   maxInt64(0, failures-allowed),
	}
	if allowed > 0 {
		budget.ConsumedPPM = mulDivRound(failures, PPM, allowed)
	} else if failures > 0 {
		budget.ConsumedPPM = PPM * 2
	}
	if allowed == 0 {
		budget.Exhausted = failures > 0
	} else {
		budget.Exhausted = failures >= allowed
	}
	return budget
}

func applyAllAlerts(project Project, results map[resultCacheKey]*WindowResult, start map[sliIdentity]int64, provenance map[resultCacheKey][]string, only map[resultCacheKey]bool) {
	for _, service := range project.Services {
		for _, sli := range service.SLIs {
			revision := SLIAggregationRevision(service.Name, sli)
			indices, _ := evaluationIndices(project, sli)
			firstIndex := int64(0)
			hasFirst := false
			for _, index := range indices {
				key := resultCacheKey{Service: service.Name, SLI: sli.ID, Revision: revision, Index: index}
				if only != nil && !only[key] {
					continue
				}
				if !hasFirst {
					firstIndex = index
					hasFirst = true
				}
			}
			if !hasFirst {
				continue
			}
			if start != nil {
				if candidate, ok := start[sliIdentity{service.Name, sli.ID}]; ok {
					firstIndex = candidate
				}
			}
			applyAlertsFrom(project, service.Name, sli, revision, results, firstIndex, provenance)
		}
	}
}

type sliIdentity struct {
	Service string
	SLI     string
}

func applyAlertsFrom(project Project, serviceName string, sli SLI, revision string, results map[resultCacheKey]*WindowResult, from int64, provenance map[resultCacheKey][]string) {
	indices, err := evaluationIndices(project, sli)
	if err != nil {
		return
	}
	states := make([]ThresholdState, len(sli.Thresholds))
	for index, threshold := range sli.Thresholds {
		states[index] = ThresholdState{
			ID: threshold.ID, BurnPPM: threshold.BurnPPM, ConsecutiveRequired: threshold.ConsecutiveWindows,
		}
	}
	if from > indices[0] {
		previousKey := resultCacheKey{Service: serviceName, SLI: sli.ID, Revision: revision, Index: from - 1}
		if previous := results[previousKey]; previous != nil {
			for index := range states {
				if index < len(previous.Alert.Thresholds) {
					states[index].ConsecutiveObserved = previous.Alert.Thresholds[index].ConsecutiveObserved
				}
			}
		}
	}
	for _, evalIndex := range indices {
		if evalIndex < from {
			continue
		}
		key := resultCacheKey{Service: serviceName, SLI: sli.ID, Revision: revision, Index: evalIndex}
		result := results[key]
		if result == nil {
			continue
		}
		alert := AlertResult{GatePass: true}
		for index := range states {
			state := &states[index]
			state.Matched = result.Budget.ConsumedPPM >= state.BurnPPM
			if state.Matched {
				state.ConsecutiveObserved++
			} else {
				state.ConsecutiveObserved = 0
			}
			state.Fired = state.ConsecutiveObserved >= state.ConsecutiveRequired
			if state.Matched {
				alert.MatchedThresholds = append(alert.MatchedThresholds, state.ID)
			}
			if state.Fired {
				alert.FiredThresholds = append(alert.FiredThresholds, state.ID)
				alert.Alert = true
				alert.PrimaryThreshold = state.ID
			}
			alert.Thresholds = append(alert.Thresholds, *state)
		}
		alert.GatePass = !alert.Alert
		result.Alert = alert
		if ids := provenance[key]; len(ids) > 0 {
			result.Provenance.InvalidationIDs = appendUnique(result.Provenance.InvalidationIDs, ids...)
		}
	}
}

func appendUnique(target []string, values ...string) []string {
	seen := make(map[string]struct{}, len(target)+len(values))
	for _, value := range target {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		target = append(target, value)
	}
	return target
}

func (engine *Engine) buildReport(results map[resultCacheKey]*WindowResult) *Report {
	report := &Report{
		ProjectID: engine.project.ProjectID, RuleVersion: engine.ruleVersion,
		CommittedObservationSeq: engine.project.LastObservationSeq(),
	}
	for _, service := range engine.project.Services {
		for _, sli := range service.SLIs {
			indices, _ := evaluationIndices(engine.project, sli)
			revision := SLIAggregationRevision(service.Name, sli)
			for _, index := range indices {
				key := resultCacheKey{Service: service.Name, SLI: sli.ID, Revision: revision, Index: index}
				if result := results[key]; result != nil {
					report.Results = append(report.Results, *result)
				}
			}
		}
	}
	return report
}

type parallelResult[T any] struct {
	results   []T
	durations []time.Duration
	signals   []BackpressureSignal
}

func (engine *Engine) runBaseTasks(ctx context.Context, stage string, tasks []baseTask, fn func(baseTask) (*BaseAgg, error)) (parallelResult[*BaseAgg], error) {
	output := parallelResult[*BaseAgg]{results: make([]*BaseAgg, len(tasks)), durations: make([]time.Duration, len(tasks))}
	durations, signals, err := runParallel(ctx, engine.options, stage, len(tasks), func(index int) (time.Duration, error) {
		started := time.Now()
		result, err := fn(tasks[index])
		output.results[index] = result
		return time.Since(started), err
	})
	output.durations = durations
	output.signals = signals
	return output, err
}

func (engine *Engine) runEvalTasks(ctx context.Context, stage string, tasks []evalTask, baseCache map[baseCacheKey]*BaseAgg, fn func(evalTask) (*WindowResult, error)) (parallelResult[*WindowResult], error) {
	output := parallelResult[*WindowResult]{results: make([]*WindowResult, len(tasks)), durations: make([]time.Duration, len(tasks))}
	durations, signals, err := runParallel(ctx, engine.options, stage, len(tasks), func(index int) (time.Duration, error) {
		started := time.Now()
		result, err := fn(tasks[index])
		output.results[index] = result
		return time.Since(started), err
	})
	output.durations = durations
	output.signals = signals
	return output, err
}

func runParallel(ctx context.Context, options Options, stage string, count int, fn func(int) (time.Duration, error)) ([]time.Duration, []BackpressureSignal, error) {
	if count == 0 {
		return nil, nil, nil
	}
	capacity := options.Workers * options.PerWorkerBuffer
	batchSize := options.BatchSize
	if batchSize <= 0 || batchSize > capacity {
		batchSize = capacity
	}
	var signals []BackpressureSignal
	if count > capacity {
		signal := BackpressureSignal{
			Code: CodeBackpressure, Stage: stage, Pending: count, Capacity: capacity, Batch: batchSize,
			Action: "batched",
		}
		if options.FailOnBackpressure {
			signal.Action = "rejected"
			signals = append(signals, signal)
			return nil, signals, biz(CodeBackpressure, "run_parallel", nil, map[string]any{
				"stage": stage, "pending": count, "capacity": capacity,
			})
		}
		signals = append(signals, signal)
	}
	order := submissionOrder(count, options.SubmissionOrder)
	durations := make([]time.Duration, count)
	taskErrors := make([]error, count)
	for batchStart := 0; batchStart < len(order); batchStart += batchSize {
		batchEnd := batchStart + batchSize
		if batchEnd > len(order) {
			batchEnd = len(order)
		}
		batch := order[batchStart:batchEnd]
		if err := ctx.Err(); err != nil {
			return durations, signals, biz(CodeGracefulInterrupted, "run_parallel", err, map[string]any{"stage": stage})
		}
		queue := make(chan int, len(batch))
		for _, index := range batch {
			queue <- index
		}
		close(queue)
		var wait sync.WaitGroup
		workers := options.Workers
		if workers > len(batch) {
			workers = len(batch)
		}
		for worker := 0; worker < workers; worker++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				for index := range queue {
					duration, err := fn(index)
					durations[index] = duration
					taskErrors[index] = err
				}
			}()
		}
		wait.Wait()
	}
	for _, err := range taskErrors {
		if err != nil {
			return durations, signals, err
		}
	}
	return durations, signals, nil
}

func baseWindowDurations(tasks []baseTask, durations []time.Duration, stage string) []WindowDuration {
	output := make([]WindowDuration, 0, len(tasks))
	for index, task := range tasks {
		if index >= len(durations) {
			break
		}
		output = append(output, WindowDuration{
			Stage: stage, Service: task.service, SLI: task.sli.ID, Index: task.index,
			DurationMicros: durations[index].Microseconds(),
		})
	}
	return output
}

func evalWindowDurations(tasks []evalTask, durations []time.Duration, stage string) []WindowDuration {
	output := make([]WindowDuration, 0, len(tasks))
	for index, task := range tasks {
		if index >= len(durations) {
			break
		}
		output = append(output, WindowDuration{
			Stage: stage, Service: task.service, SLI: task.sli.ID, Index: task.evalIndex,
			DurationMicros: durations[index].Microseconds(),
		})
	}
	return output
}

func submissionOrder(count int, mode string) []int {
	order := make([]int, count)
	for index := range order {
		order[index] = index
	}
	switch mode {
	case "reverse":
		for left, right := 0, len(order)-1; left < right; left, right = left+1, right-1 {
			order[left], order[right] = order[right], order[left]
		}
	case "shuffle":
		state := uint64(0x9e3779b97f4a7c15)
		for index := len(order) - 1; index > 0; index-- {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			swap := int(state % uint64(index+1))
			order[index], order[swap] = order[swap], order[index]
		}
	}
	return order
}
