package downsample

import (
	"fmt"
	"sort"
)

type ConfigEdit struct {
	MetricID    string
	Field       string
	WidthMS     int64
	Alignment   SourceAlignment
	AnchorMS    int64
	Functions   []Func
	WeightScale WeightScale
	WeightBits  int
}

func (e *Engine) AppendPoints(points ...Point) (*RecomputeReport, *Error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	direct := map[WindowKey]InvalidationSource{}
	maxSeq := e.globalSeq
	for i := range points {
		point := &points[i]
		metric, ok := e.project.Metric(point.MetricID)
		if !ok {
			return nil, NewError(CodeMetricNotFound, "cannot append point to unknown metric").
				WithContext("metric_id", point.MetricID).
				WithContext("seq", point.Seq)
		}
		if point.Seq == 0 {
			e.globalSeq++
			point.Seq = e.globalSeq
		} else if point.Seq > e.globalSeq {
			e.globalSeq = point.Seq
		} else if point.Seq <= maxSeq {
			// Duplicate/out-of-order logical sequence is rejected before mutation.
			return nil, NewError(CodeConfigEditInvalid, "append sequence is not after committed sequence").
				WithContext("metric_id", point.MetricID).
				WithContext("seq", point.Seq).
				WithContext("committed_seq", e.globalSeq)
		}
		maxSeq = e.globalSeq
		e.project.Series[point.MetricID] = append(e.project.Series[point.MetricID], points[i])
		start := WindowStart(point.TimestampMS, metric.WindowMS, metric.AnchorMS)
		first := WindowStart(e.project.RangeStartMS, metric.WindowMS, metric.AnchorMS)
		key := WindowKey{
			MetricID: metric.ID, StartMS: start,
			WindowIndex: (start - first) / metric.WindowMS,
		}
		direct[key] = InvalidationSource{
			Kind: "append", Reason: "new_sample_in_window",
			ConfigVersion: e.project.ConfigVersion, Sequence: point.Seq,
			ChangedMetricID: metric.ID,
		}
	}

	closure, info := e.expandWindowClosureLocked(direct)
	reused := e.reusedKeysLocked(closure)
	for key := range closure {
		if windows := e.cache[key.MetricID]; windows != nil {
			delete(windows, key.StartMS)
		}
	}
	recomputed := e.recomputeKeysLocked(closure)
	info.WindowCount = len(recomputed)
	return &RecomputeReport{
		ConfigVersion: e.project.ConfigVersion,
		Trigger:       InvalidationSource{Kind: "append", ConfigVersion: e.project.ConfigVersion, Sequence: e.globalSeq},
		Results:       e.snapshotLocked(),
		Recomputed:    recomputed,
		Reused:        reused,
		Closure:       info,
	}, nil
}

func (e *Engine) ApplyConfigEdit(edit ConfigEdit) (*RecomputeReport, *Error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	metric, ok := e.project.Metric(edit.MetricID)
	if !ok {
		return nil, NewError(CodeMetricNotFound, "cannot edit unknown metric").
			WithContext("metric_id", edit.MetricID)
	}

	source := InvalidationSource{
		Kind: "config", ConfigVersion: e.project.ConfigVersion,
		ChangedMetricID: edit.MetricID, ChangedField: edit.Field,
		Reason: "config_edit_" + edit.Field,
	}

	closureMetrics := map[string]bool{edit.MetricID: true}
	edges := []string{}
	queue := []string{edit.MetricID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, candidate := range e.project.Metrics {
			if candidate.Derivation == nil {
				continue
			}
			for _, input := range candidate.Derivation.Inputs {
				if input.MetricID != id || closureMetrics[candidate.ID] {
					continue
				}
				closureMetrics[candidate.ID] = true
				queue = append(queue, candidate.ID)
				edges = append(edges, fmt.Sprintf("%s -> %s via %s", id, candidate.ID, input.Function))
			}
		}
	}

	switch edit.Field {
	case "window_width":
		if edit.WidthMS <= 0 {
			return nil, NewError(CodeWindowWidthZero, "window width must be positive").
				WithFieldPath("edit.width_ms").
				WithContext("metric_id", edit.MetricID).
				WithContext("width", edit.WidthMS)
		}
		metric.WindowMS = edit.WidthMS
	case "alignment":
		if edit.Alignment != SourceAlignEpoch && edit.Alignment != SourceAlignFirstPoint {
			return nil, NewError(CodeConfigEditInvalid, "unsupported alignment").
				WithFieldPath("edit.alignment").
				WithContext("value", edit.Alignment)
		}
		metric.SourceAlignment = edit.Alignment
		if edit.AnchorMS != 0 {
			metric.AnchorMS = edit.AnchorMS
		}
	case "functions":
		if len(edit.Functions) == 0 {
			return nil, NewError(CodeConfigEditInvalid, "function set must not be empty").
				WithFieldPath("edit.functions").
				WithContext("metric_id", edit.MetricID)
		}
		metric.Functions = append([]Func(nil), edit.Functions...)
	case "weight_scale":
		if !edit.WeightScale.Valid() {
			return nil, NewError(CodeWeightScaleOutOfRange, "weight scale must be 1000 or 10000").
				WithFieldPath("edit.weight_scale").
				WithContext("metric_id", edit.MetricID).
				WithContext("value", int64(edit.WeightScale))
		}
		metric.WeightScale = edit.WeightScale
		if edit.WeightBits != 0 {
			if berr := validateWeightBits(edit.WeightBits, "edit.weight_bits"); berr != nil {
				return nil, berr.WithContext("metric_id", edit.MetricID)
			}
			metric.WeightBits = edit.WeightBits
		}
	default:
		return nil, NewError(CodeConfigEditInvalid, "unsupported config edit field").
			WithFieldPath("edit.field").
			WithContext("field", edit.Field)
	}

	e.editCount++
	e.project.ConfigVersion = fmt.Sprintf("cfg-v%d", e.editCount+1)
	source.ConfigVersion = e.project.ConfigVersion

	closure := map[WindowKey]InvalidationSource{}
	metricIDs := make([]string, 0, len(closureMetrics))
	for id := range closureMetrics {
		metricIDs = append(metricIDs, id)
		m, _ := e.project.Metric(id)
		for _, start := range e.WindowStarts(*m) {
			first := WindowStart(e.project.RangeStartMS, m.WindowMS, m.AnchorMS)
			key := WindowKey{MetricID: id, StartMS: start, WindowIndex: (start - first) / m.WindowMS}
			keySource := source
			if id != edit.MetricID {
				keySource = InvalidationSource{
					Kind: "upstream", ConfigVersion: e.project.ConfigVersion,
					Reason:           "derived_dependency_config_change",
					UpstreamMetricID: edit.MetricID, ChangedField: edit.Field,
					ClosurePath: []string{edit.MetricID, id},
				}
			}
			closure[key] = keySource
		}
	}
	sort.Strings(metricIDs)

	info := ClosureInfo{
		RootMetricID: edit.MetricID, Metrics: metricIDs,
		CrossMetricEdges: uniqueSorted(edges),
	}
	for _, id := range metricIDs {
		if id == edit.MetricID {
			continue
		}
		if len(e.dependentMetricIDsLocked(id)) == 0 {
			info.CrossMetricBoundary = append(info.CrossMetricBoundary, "terminal_derived_metric:"+id)
		}
	}
	if len(info.CrossMetricBoundary) == 0 {
		info.CrossMetricBoundary = []string{"root_metric_boundary:" + edit.MetricID}
	}

	reusedAll := e.reusedKeysLocked(closure)
	reused := make([]WindowKey, 0, len(reusedAll))
	for _, key := range reusedAll {
		if !closureMetrics[key.MetricID] {
			reused = append(reused, key)
		}
	}
	for id := range closureMetrics {
		e.cache[id] = map[int64]WindowResult{}
	}
	recomputed := e.recomputeKeysLocked(closure)
	info.WindowCount = len(recomputed)
	return &RecomputeReport{
		ConfigVersion: e.project.ConfigVersion, Trigger: source,
		Results: e.snapshotLocked(), Recomputed: recomputed, Reused: reused, Closure: info,
	}, nil
}

func (e *Engine) expandWindowClosureLocked(direct map[WindowKey]InvalidationSource) (map[WindowKey]InvalidationSource, ClosureInfo) {
	closure := map[WindowKey]InvalidationSource{}
	queue := make([]WindowKey, 0, len(direct))
	edges := []string{}
	for key, source := range direct {
		closure[key] = source
		queue = append(queue, key)
	}

	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]

		fillKeys := e.fillForwardKeysLocked(key)
		for _, fkey := range fillKeys {
			if _, exists := closure[fkey]; !exists {
				source := InvalidationSource{
					Kind: "fill_dependency", Reason: "recent_valid_source_changed",
					ConfigVersion: e.project.ConfigVersion, ChangedMetricID: key.MetricID,
					UpstreamMetricID: key.MetricID, UpstreamWindowMS: key.StartMS,
				}
				closure[fkey] = source
				queue = append(queue, fkey)
			}
		}

		metric, ok := e.project.Metric(key.MetricID)
		if !ok {
			continue
		}
		for _, dep := range e.project.Metrics {
			if dep.Derivation == nil {
				continue
			}
			for _, input := range dep.Derivation.Inputs {
				if input.MetricID != key.MetricID {
					continue
				}
				derivedStart := key.StartMS + int64(input.Lag)*metric.WindowMS
				first := WindowStart(e.project.RangeStartMS, dep.WindowMS, dep.AnchorMS)
				dkey := WindowKey{
					MetricID: dep.ID, StartMS: derivedStart,
					WindowIndex: (derivedStart - first) / dep.WindowMS,
				}
				edge := fmt.Sprintf("%s@%d -> %s@%d via %s", key.MetricID, key.StartMS, dep.ID, derivedStart, input.Function)
				edges = append(edges, edge)
				if _, exists := closure[dkey]; !exists {
					source := InvalidationSource{
						Kind: "upstream", Reason: "derived_window_references_upstream_aggregate",
						ConfigVersion:    e.project.ConfigVersion,
						UpstreamMetricID: key.MetricID, UpstreamWindowMS: key.StartMS,
						ClosurePath: []string{key.MetricID, dep.ID},
					}
					closure[dkey] = source
					queue = append(queue, dkey)
				}
			}
		}
	}

	metricSet := map[string]bool{}
	for key := range closure {
		metricSet[key.MetricID] = true
	}
	metrics := make([]string, 0, len(metricSet))
	for id := range metricSet {
		metrics = append(metrics, id)
	}
	sort.Strings(metrics)
	info := ClosureInfo{
		RootMetricID: firstRootMetric(direct), Metrics: metrics,
		CrossMetricEdges:    uniqueSorted(edges),
		CrossMetricBoundary: e.boundaryForMetricsLocked(metricSet),
	}
	return closure, info
}

func firstRootMetric(direct map[WindowKey]InvalidationSource) string {
	for key := range direct {
		return key.MetricID
	}
	return ""
}

func (e *Engine) fillForwardKeysLocked(key WindowKey) []WindowKey {
	metric, ok := e.project.Metric(key.MetricID)
	if !ok || !metric.Fill.Enabled || metric.Fill.MaxLookbackWindows <= 0 {
		return nil
	}
	out := []WindowKey{}
	for distance := 1; distance <= metric.Fill.MaxLookbackWindows; distance++ {
		start := key.StartMS + int64(distance)*metric.WindowMS
		fkey := WindowKey{MetricID: metric.ID, StartMS: start}
		cached, ok := e.cache[metric.ID][start]
		if !ok {
			break
		}
		if cached.Status == StatusOK && !cached.Filled && cached.PointCount > 0 {
			break
		}
		first := WindowStart(e.project.RangeStartMS, metric.WindowMS, metric.AnchorMS)
		fkey.WindowIndex = (start - first) / metric.WindowMS
		out = append(out, fkey)
	}
	return out
}

func (e *Engine) dependentMetricIDsLocked(metricID string) []string {
	out := []string{}
	for _, candidate := range e.project.Metrics {
		if candidate.Derivation == nil {
			continue
		}
		for _, input := range candidate.Derivation.Inputs {
			if input.MetricID == metricID {
				out = append(out, candidate.ID)
				break
			}
		}
	}
	return uniqueSorted(out)
}

func (e *Engine) boundaryForMetricsLocked(metricSet map[string]bool) []string {
	boundary := []string{}
	for id := range metricSet {
		dependents := e.dependentMetricIDsLocked(id)
		inside := false
		for _, dep := range dependents {
			if metricSet[dep] {
				inside = true
				break
			}
		}
		if !inside {
			boundary = append(boundary, "closure_boundary:"+id)
		}
	}
	return uniqueSorted(boundary)
}

func (e *Engine) reusedKeysLocked(closure map[WindowKey]InvalidationSource) []WindowKey {
	reused := []WindowKey{}
	for metricID, windows := range e.cache {
		for start, result := range windows {
			key := result.Key
			if key.MetricID == "" {
				key = WindowKey{MetricID: metricID, StartMS: start}
			}
			if _, invalid := closure[key]; !invalid {
				reused = append(reused, key)
			}
		}
	}
	sortWindowKeys(e.project, reused)
	return reused
}

func (e *Engine) recomputeKeysLocked(closure map[WindowKey]InvalidationSource) []WindowKey {
	keys := make([]WindowKey, 0, len(closure))
	for key := range closure {
		keys = append(keys, key)
	}
	sortWindowKeys(e.project, keys)
	for _, key := range keys {
		metric, ok := e.project.Metric(key.MetricID)
		if !ok {
			continue
		}
		source := closure[key]
		result := e.ensureWindowLocked(metric, key.StartMS, source, map[WindowKey]bool{})
		result.Key.WindowIndex = key.WindowIndex
		result.WindowIndex = key.WindowIndex
		e.cache[metric.ID][key.StartMS] = result
	}
	return keys
}

func (e *Engine) snapshotLocked() map[WindowKey]WindowResult {
	out := map[WindowKey]WindowResult{}
	for _, windows := range e.cache {
		for _, result := range windows {
			out[result.Key] = result.Clone()
		}
	}
	return out
}

func sortWindowKeys(p *Project, keys []WindowKey) {
	sort.SliceStable(keys, func(i, j int) bool {
		oi := metricOrdinal(p, keys[i].MetricID)
		oj := metricOrdinal(p, keys[j].MetricID)
		if oi != oj {
			return oi < oj
		}
		return keys[i].StartMS < keys[j].StartMS
	})
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
