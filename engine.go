package downsample

import (
	"context"
	"math"
	"sort"
	"sync"
)

type Engine struct {
	mu        sync.RWMutex
	project   *Project
	budget    Budget
	gate      *Gate
	cache     map[string]map[int64]WindowResult
	globalSeq int64
	editCount int
}

func NewEngine(project *Project, budget Budget) *Engine {
	if err := budget.Validate(); err != nil {
		// Constructors in tests/demo use DefaultBudget; invalid budgets are
		// reported explicitly through ValidateBudget/NewGate for injection.
		budget = DefaultBudget()
	}
	gate, _ := NewGate(budget)
	e := &Engine{
		project:   project,
		budget:    budget,
		gate:      gate,
		cache:     map[string]map[int64]WindowResult{},
		globalSeq: 0,
	}
	for _, metric := range project.Metrics {
		e.cache[metric.ID] = map[int64]WindowResult{}
	}
	for _, pts := range project.Series {
		for _, p := range pts {
			if p.Seq > e.globalSeq {
				e.globalSeq = p.Seq
			}
		}
	}
	return e
}

func ValidateBudget(budget Budget) *Error { return budget.Validate() }

func (e *Engine) Project() *Project {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.project
}

func (e *Engine) GlobalSequence() int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.globalSeq
}

func (e *Engine) ConfigVersion() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.project.ConfigVersion
}

func (e *Engine) Budget() Budget { return e.budget }

func (e *Engine) SnapshotResults() map[WindowKey]WindowResult {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := map[WindowKey]WindowResult{}
	for _, windows := range e.cache {
		for _, result := range windows {
			out[result.Key] = result.Clone()
		}
	}
	return out
}

func (e *Engine) QueryWindow(metricID string, startMS int64) (WindowResult, *Error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	metric, ok := e.project.Metric(metricID)
	if !ok {
		return WindowResult{}, NewError(CodeMetricNotFound, "metric not found").
			WithContext("metric_id", metricID)
	}
	if windows, ok := e.cache[metricID]; ok {
		if result, hit := windows[startMS]; hit {
			return result.Clone(), nil
		}
	}
	source := InvalidationSource{Kind: "query_on_demand", ConfigVersion: e.project.ConfigVersion, ChangedMetricID: metricID}
	result := e.ensureWindowLocked(metric, startMS, source, map[WindowKey]bool{})
	return result.Clone(), nil
}

func (e *Engine) ensureWindowLocked(metric *Metric, startMS int64, source InvalidationSource, active map[WindowKey]bool) WindowResult {
	if windows, ok := e.cache[metric.ID]; ok {
		if result, hit := windows[startMS]; hit {
			return result
		}
	}
	key := WindowKey{MetricID: metric.ID, StartMS: startMS}
	if active[key] {
		return WindowResult{Key: key, MetricID: metric.ID, StartMS: startMS, Status: StatusEmpty, ConfigVersion: e.project.ConfigVersion}
	}
	active[key] = true

	var result WindowResult
	if metric.Kind == "derived" && metric.Derivation != nil {
		result = e.computeDerivedLocked(metric, startMS, source, active)
	} else {
		result = e.aggregateRaw(metric, startMS, source)
	}
	e.applyFillLocked(metric, &result, source, active)
	if e.cache[metric.ID] == nil {
		e.cache[metric.ID] = map[int64]WindowResult{}
	}
	e.cache[metric.ID][startMS] = result
	return result
}

func (e *Engine) WindowStarts(metric Metric) []int64 {
	count := e.project.WindowCount(metric)
	first := WindowStart(e.project.RangeStartMS, metric.WindowMS, metric.AnchorMS)
	out := make([]int64, 0, count)
	for i := int64(0); i < count; i++ {
		out = append(out, first+i*metric.WindowMS)
	}
	return out
}

func (e *Engine) aggregateRaw(metric *Metric, start int64, source InvalidationSource) WindowResult {
	end := start + metric.WindowMS
	points := e.project.SortedPoints(metric.ID)
	inWindow := make([]Point, 0)
	for _, p := range points {
		if p.TimestampMS >= start && p.TimestampMS < end {
			inWindow = append(inWindow, p)
		}
	}

	result := WindowResult{
		Key: WindowKey{
			MetricID: metric.ID,
			StartMS:  start,
		},
		MetricID:         metric.ID,
		StartMS:          start,
		EndMS:            end,
		Values:           map[Func]float64{},
		Status:           StatusEmpty,
		Invalidation:     withSourceWindow(source, metric.ID, start),
		ConfigVersion:    e.project.ConfigVersion,
		FillFieldPresent: metric.Fill.FieldPresent,
		FillDegraded:     metric.Fill.Degraded,
	}
	firstGrid := WindowStart(e.project.RangeStartMS, metric.WindowMS, metric.AnchorMS)
	result.WindowIndex = (start - firstGrid) / metric.WindowMS
	result.Key.WindowIndex = result.WindowIndex

	if len(inWindow) == 0 {
		return result
	}

	result.PointCount = len(inWindow)
	result.Status = StatusOK
	var sum, weightedSum float64
	var weightTotal int64
	minV := inWindow[0].Value
	maxV := inWindow[0].Value
	for i, p := range inWindow {
		sum += p.Value
		weightedSum += p.Value * float64(p.Weight)
		weightTotal += p.Weight
		if p.Value < minV {
			minV = p.Value
		}
		if p.Value > maxV {
			maxV = p.Value
		}
		if i == 0 {
			result.Values[FuncFirst] = p.Value
		}
		if i == len(inWindow)-1 {
			result.Values[FuncLast] = p.Value
		}
	}
	avg := sum / float64(len(inWindow))
	result.Values[FuncCount] = float64(len(inWindow))
	result.Values[FuncSum] = sum
	result.Values[FuncAvg] = avg
	result.Values[FuncMin] = minV
	result.Values[FuncMax] = maxV
	if weightTotal > 0 {
		result.Weight = WeightAggregate{
			WeightedSumRaw: weightedSum,
			WeightTotalRaw: weightTotal,
			WeightedMean:   weightedSum / float64(weightTotal),
			Scale:          int64(ScalePermyriad),
		}
		result.Values[FuncWeightedAvg] = result.Weight.WeightedMean
		result.Values[FuncWeightedSum] = weightedSum
		result.Values[FuncWeightTotal] = float64(weightTotal)
	}

	// Restrict the public function map to declared functions plus count.
	declared := map[Func]bool{FuncCount: true}
	for _, fn := range metric.Functions {
		declared[fn] = true
	}
	for fn := range result.Values {
		if !declared[fn] {
			delete(result.Values, fn)
		}
	}
	return result
}

func withSourceWindow(source InvalidationSource, metricID string, start int64) InvalidationSource {
	out := source
	if out.Kind == "upstream" && out.UpstreamMetricID == "" {
		out.UpstreamMetricID = metricID
		out.UpstreamWindowMS = start
	}
	return out
}

func (e *Engine) computeDerivedLocked(metric *Metric, start int64, source InvalidationSource, active map[WindowKey]bool) WindowResult {
	end := start + metric.WindowMS
	result := WindowResult{
		Key: WindowKey{MetricID: metric.ID, StartMS: start}, MetricID: metric.ID,
		StartMS: start, EndMS: end, Values: map[Func]float64{}, Status: StatusEmpty,
		Invalidation: withSourceWindow(source, metric.ID, start), ConfigVersion: e.project.ConfigVersion,
		FillFieldPresent: metric.Fill.FieldPresent, FillDegraded: metric.Fill.Degraded,
	}
	firstGrid := WindowStart(e.project.RangeStartMS, metric.WindowMS, metric.AnchorMS)
	result.WindowIndex = (start - firstGrid) / metric.WindowMS
	result.Key.WindowIndex = result.WindowIndex

	d := metric.Derivation
	if len(d.Inputs) == 0 {
		return result
	}
	values := make([]float64, 0, len(d.Inputs))
	var upstreamCount int
	var upstreamWeight WeightAggregate
	var filled bool
	var fillInfo *FillInfo
	for _, input := range d.Inputs {
		upstream, ok := e.project.Metric(input.MetricID)
		if !ok {
			return result
		}
		inputStart := start - int64(input.Lag)*upstream.WindowMS
		upResult := e.ensureWindowLocked(upstream, inputStart, InvalidationSource{
			Kind: "upstream", ConfigVersion: e.project.ConfigVersion,
			UpstreamMetricID: upstream.ID, UpstreamWindowMS: inputStart,
			ClosurePath: append(append([]string{}, source.ClosurePath...), upstream.ID),
		}, active)
		v, ok := upResult.Values[input.Function]
		if !ok && input.Function == FuncAvg {
			v, ok = upResult.Values[FuncDerived]
		}
		if !ok || upResult.Status == StatusEmpty || upResult.Status == StatusMissing {
			return result
		}
		values = append(values, v)
		upstreamCount = upResult.PointCount
		upstreamWeight = upResult.Weight
		if upResult.Filled {
			filled = true
			fillInfo = upResult.Fill
		}
	}

	var derived float64
	switch d.Op {
	case "scale":
		derived = values[0] * d.Factor
	case "add":
		for _, v := range values {
			derived += v
		}
	case "weighted_add":
		for i, v := range values {
			if i == 0 {
				derived += v * d.Factor
			} else {
				derived += v
			}
		}
	default:
		derived = values[0]
	}
	result.Values[FuncDerived] = derived
	result.PointCount = upstreamCount
	result.Weight = upstreamWeight
	result.Status = StatusOK
	if filled && fillInfo != nil {
		result.Filled = true
		result.Status = StatusFilled
		result.Fill = fillInfo
	}
	return result
}

func (e *Engine) applyFillLocked(metric *Metric, result *WindowResult, source InvalidationSource, active map[WindowKey]bool) {
	if result.Filled {
		if result.FillAnnotation == "" {
			result.FillAnnotation = "DERIVED_FROM_FILLED_UPSTREAM"
		}
		return
	}
	if result.PointCount > 0 || result.Status == StatusOK {
		result.FillAnnotation = "OBSERVED_SAMPLES"
		return
	}
	result.Status = StatusEmpty
	if !metric.Fill.Enabled || metric.Fill.MaxLookbackWindows <= 0 {
		if !metric.Fill.FieldPresent {
			result.FillAnnotation = "UNFILLED_OPTIONAL_FIELD_MISSING_DEGRADATION_DISABLED"
		} else {
			result.FillAnnotation = "UNFILLED_POLICY_DISABLED"
		}
		return
	}
	for distance := 1; distance <= metric.Fill.MaxLookbackWindows; distance++ {
		sourceStart := result.StartMS - int64(distance)*metric.WindowMS
		var sourceResult WindowResult
		if cached, ok := e.cache[metric.ID][sourceStart]; ok {
			sourceResult = cached
		} else if metric.Kind == "derived" && metric.Derivation != nil {
			sourceResult = e.computeDerivedLocked(metric, sourceStart, InvalidationSource{
				Kind: "fill_source", ConfigVersion: e.project.ConfigVersion,
				ChangedMetricID: metric.ID,
			}, active)
		} else {
			sourceResult = e.aggregateRaw(metric, sourceStart, InvalidationSource{
				Kind: "fill_source", ConfigVersion: e.project.ConfigVersion,
				ChangedMetricID: metric.ID,
			})
		}
		if sourceResult.Status != StatusOK || sourceResult.PointCount == 0 {
			continue
		}
		values := map[Func]float64{}
		for k, v := range sourceResult.Values {
			values[k] = v
		}
		result.Values = values
		result.Status = StatusFilled
		result.Filled = true
		if metric.Fill.FieldPresent {
			result.FillAnnotation = "FILLED_LAST_VALID_WINDOW"
		} else {
			result.FillAnnotation = "FILLED_USING_DECLARED_DEFAULT_DEGRADATION__FILL_FIELD_NOT_PROVIDED"
		}
		result.Fill = &FillInfo{
			SourceWindowStartMS: sourceStart,
			SourceWindowIndex:   sourceResult.WindowIndex,
			DistanceWindows:     distance,
			Values:              values,
		}
		// PointCount intentionally remains zero: carried values are not samples.
		if source.Kind != "" {
			result.Invalidation = source
		}
		return
	}
	result.FillAnnotation = "UNFILLED_NO_VALID_SOURCE_WITHIN_DECLARED_LOOKBACK"
}

func (e *Engine) RecomputeAll(trigger InvalidationSource) (*RecomputeReport, *Error) {
	if err := e.budget.Validate(); err != nil {
		return nil, err
	}
	if trigger.Kind == "" {
		trigger.Kind = "initial"
	}
	trigger.ConfigVersion = e.ConfigVersion()
	e.mu.Lock()
	project := e.project
	metrics := project.SortedMetrics()
	for _, metric := range metrics {
		if e.cache[metric.ID] == nil {
			e.cache[metric.ID] = map[int64]WindowResult{}
		}
	}
	e.mu.Unlock()

	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr *Error
	for _, metricValue := range metrics {
		metric := metricValue
		wg.Add(1)
		go func() {
			defer wg.Done()
			if metric.Kind == "derived" {
				return
			}
			starts := e.WindowStarts(metric)
			for _, start := range starts {
				release, err := e.gate.Acquire(context.Background(), metric.ID)
				if err != nil {
					coded := AsError(err)
					errMu.Lock()
					if firstErr == nil {
						firstErr = coded
					}
					errMu.Unlock()
					return
				}
				source := trigger
				source.Kind = "initial"
				raw := e.aggregateRaw(&metric, start, source)
				e.mu.Lock()
				e.cache[metric.ID][start] = raw
				e.mu.Unlock()
				release()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	allKeys := []WindowKey{}
	for _, metric := range metrics {
		starts := e.WindowStarts(metric)
		for _, start := range starts {
			source := trigger
			source.Kind = "initial"
			var result WindowResult
			if metric.Kind == "derived" && metric.Derivation != nil {
				result = e.computeDerivedLocked(&metric, start, source, map[WindowKey]bool{})
			} else {
				result = e.cache[metric.ID][start]
			}
			e.applyFillLocked(&metric, &result, result.Invalidation, map[WindowKey]bool{})
			e.cache[metric.ID][start] = result
			allKeys = append(allKeys, result.Key)
		}
	}
	sort.Slice(allKeys, func(i, j int) bool {
		if allKeys[i].MetricID == allKeys[j].MetricID {
			return allKeys[i].StartMS < allKeys[j].StartMS
		}
		return metricOrdinal(project, allKeys[i].MetricID) < metricOrdinal(project, allKeys[j].MetricID)
	})
	results := map[WindowKey]WindowResult{}
	for _, windows := range e.cache {
		for _, result := range windows {
			results[result.Key] = result.Clone()
		}
	}
	return &RecomputeReport{
		ConfigVersion: project.ConfigVersion,
		Trigger:       trigger,
		Results:       results,
		Recomputed:    allKeys,
		Closure:       ClosureInfo{Metrics: metricIDs(metrics), RootMetricID: "*"},
	}, nil
}

func metricOrdinal(p *Project, id string) int {
	if m, ok := p.Metric(id); ok {
		return m.Ordinal
	}
	return math.MaxInt
}

func metricIDs(metrics []Metric) []string {
	out := make([]string, 0, len(metrics))
	for _, m := range metrics {
		out = append(out, m.ID)
	}
	return out
}

type RecomputeReport struct {
	ConfigVersion string                     `json:"config_version"`
	Trigger       InvalidationSource         `json:"trigger"`
	Results       map[WindowKey]WindowResult `json:"-"`
	Recomputed    []WindowKey                `json:"recomputed"`
	Reused        []WindowKey                `json:"reused"`
	Closure       ClosureInfo                `json:"closure"`
}

type ClosureInfo struct {
	RootMetricID        string   `json:"root_metric_id"`
	Metrics             []string `json:"metrics"`
	WindowCount         int      `json:"window_count"`
	CrossMetricEdges    []string `json:"cross_metric_edges"`
	CrossMetricBoundary []string `json:"cross_metric_boundary"`
}

func CompareWindowMaps(expected, actual map[WindowKey]WindowResult, tolerance float64) []string {
	diffs := []string{}
	for key, want := range expected {
		got, ok := actual[key]
		if !ok {
			diffs = append(diffs, "missing window "+key.String())
			continue
		}
		if got.PointCount != want.PointCount {
			diffs = append(diffs, key.String()+": count mismatch")
		}
		if got.Status != want.Status && !(want.Status == StatusFilled && got.Filled) {
			diffs = append(diffs, key.String()+": status mismatch "+string(want.Status)+" != "+string(got.Status))
		}
		if got.Filled != want.Filled {
			diffs = append(diffs, key.String()+": filled mismatch")
		}
		if got.Weight.WeightTotalRaw != want.Weight.WeightTotalRaw {
			diffs = append(diffs, key.String()+": weight total mismatch")
		}
		if math.Abs(got.Weight.WeightedMean-want.Weight.WeightedMean) > tolerance {
			diffs = append(diffs, key.String()+": weighted mean mismatch")
		}
		for fn, wv := range want.Values {
			gv, ok := got.Values[fn]
			if !ok {
				diffs = append(diffs, key.String()+": missing fn "+string(fn))
				continue
			}
			if math.Abs(gv-wv) > tolerance {
				diffs = append(diffs, key.String()+": value mismatch "+string(fn))
			}
		}
	}
	for key := range actual {
		if _, ok := expected[key]; !ok {
			diffs = append(diffs, "unexpected window "+key.String())
		}
	}
	return diffs
}
