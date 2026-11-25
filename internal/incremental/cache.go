package incremental

import (
	"context"
	"fmt"
	"sort"

	"featurekernel/internal/engine"
	"featurekernel/internal/kernel"
)

type Cache struct {
	project kernel.Project
	windows map[string]kernel.WindowResult
	derived map[string]kernel.DerivedResult
}

type Report struct {
	Change          ChangeSet         `json:"change"`
	RecomputedKeys  []string          `json:"recomputed_keys"`
	ReusedKeys      []string          `json:"reused_keys"`
	InvalidatedKeys []string          `json:"invalidated_keys"`
	RemovedKeys     []string          `json:"removed_keys"`
	SourceClosures  []SourceClosure   `json:"source_closures"`
	ResultSet       *kernel.ResultSet `json:"result_set"`
}

type SourceClosure struct {
	Source      string   `json:"source"`
	WindowKeys  []string `json:"window_keys"`
	DerivedKeys []string `json:"derived_keys,omitempty"`
	Closure     []string `json:"propagation_closure"`
}

func windowKey(featureID string, index int64) string {
	return fmt.Sprintf("window:%s:%d", featureID, index)
}

func derivedKey(derivedID string, index int64) string {
	return fmt.Sprintf("derived:%s:%d", derivedID, index)
}

func NewCache(project *kernel.Project) (*Cache, *kernel.ResultSet, error) {
	if project == nil {
		return nil, nil, kernel.NewError(kernel.CodeInvalidArgument, "project is required", nil)
	}
	cloned, err := CloneProject(*project)
	if err != nil {
		return nil, nil, err
	}
	eng, err := engine.New(&cloned)
	if err != nil {
		return nil, nil, err
	}
	result, err := eng.Run(context.Background())
	if err != nil {
		return nil, nil, err
	}
	cache := &Cache{
		project: cloned,
		windows: map[string]kernel.WindowResult{},
		derived: map[string]kernel.DerivedResult{},
	}
	for _, window := range result.Windows {
		cache.windows[windowKey(window.FeatureID, window.WindowIndex)] = window
	}
	for _, derived := range result.Derived {
		cache.derived[derivedKey(derived.DerivedID, derived.WindowIndex)] = derived
	}
	return cache, result, nil
}

func (c *Cache) Project() *kernel.Project { return &c.project }

func (c *Cache) Apply(edit Edit) (*Report, error) {
	candidate, err := CloneProject(c.project)
	if err != nil {
		return nil, err
	}
	change := ChangeSet{}
	invalidated := map[string]bool{}
	sourcesByKey := map[string][]string{}
	keysBySource := map[string][]string{}

	addInvalidation := func(key, source string) {
		if !invalidated[key] {
			invalidated[key] = true
		}
		sourcesByKey[key] = appendUnique(sourcesByKey[key], source)
		keysBySource[source] = appendUnique(keysBySource[source], key)
	}

	for featureID, width := range edit.ChangeWidth {
		change.WidthFeatureIDs = append(change.WidthFeatureIDs, featureID)
		source := "definition:" + featureID + ":width"
		if err := applyWidth(&candidate, featureID, width); err != nil {
			return nil, err
		}
		for key := range c.windows {
			if keyBelongsFeature(key, featureID) {
				addInvalidation(key, source)
			}
		}
	}

	for featureID, aggs := range edit.ChangeAggregations {
		change.AggregationFeatureIDs = append(change.AggregationFeatureIDs, featureID)
		source := "definition:" + featureID + ":aggregations"
		if err := applyAggregations(&candidate, featureID, aggs); err != nil {
			return nil, err
		}
		for key := range c.windows {
			if keyBelongsFeature(key, featureID) {
				addInvalidation(key, source)
			}
		}
	}

	if err := collectEventInvalidations(c.project, candidate, edit, addInvalidation, &change); err != nil {
		return nil, err
	}
	applyEventEdits(&candidate, edit, &change)

	if err := validateCandidate(candidate); err != nil {
		return nil, err
	}

	report := &Report{Change: change, ResultSet: &kernel.ResultSet{
		ProjectID: candidate.ProjectID, BatchID: candidate.BatchID,
		CanonicalVersion: kernel.CanonicalVersion, Windows: []kernel.WindowResult{},
		Derived: []kernel.DerivedResult{}, Recomputations: []kernel.Recomputation{},
		RecomputedWindows: []string{}, Discarded: []kernel.DiscardedRecord{},
		PathDecisions: []kernel.PathDecision{}, WatermarkStatus: candidate.WatermarkPolicy,
	}}

	currentKeys := map[string]bool{}
	featureByID := map[string]kernel.FeatureDef{}
	for _, feature := range candidate.Features {
		featureByID[feature.ID] = feature
		report.ResultSet.PathDecisions = append(report.ResultSet.PathDecisions, kernel.SelectPath(feature.WidthMs, feature.SlideMs, candidate.PathPolicy))
	}

	indicesByFeature := map[string]map[int64]bool{}
	for _, record := range candidate.Records {
		if record.Kind != kernel.RecordEvent || record.Event == nil {
			continue
		}
		feature, ok := featureByID[record.Event.FeatureID]
		if !ok {
			return nil, kernel.NewError(kernel.CodeEventUnknownFeature, "event references unknown feature after edit", map[string]any{"feature_id": record.Event.FeatureID, "seq": record.Seq})
		}
		coords, err := kernel.WindowsForTime(feature.AnchorMs, feature.WidthMs, feature.SlideMs, record.Event.TimeMs)
		if err != nil {
			return nil, err
		}
		if indicesByFeature[feature.ID] == nil {
			indicesByFeature[feature.ID] = map[int64]bool{}
		}
		for _, coord := range coords {
			indicesByFeature[feature.ID][coord.Index] = true
		}
	}

	for _, feature := range candidate.Features {
		indices := indicesByFeature[feature.ID]
		sortedIndices := make([]int64, 0, len(indices))
		for index := range indices {
			sortedIndices = append(sortedIndices, index)
		}
		sort.Slice(sortedIndices, func(i, j int) bool { return sortedIndices[i] < sortedIndices[j] })
		for _, index := range sortedIndices {
			key := windowKey(feature.ID, index)
			currentKeys[key] = true
			_, cached := c.windows[key]
			reason := ""
			if invalidated[key] {
				reason = "explicit_invalidation"
			} else if !cached {
				reason = "cache_miss_new_window"
				addInvalidation(key, reason)
			}

			var window kernel.WindowResult
			recomputed := invalidated[key] || !cached
			if recomputed {
				window, err = recomputeWindow(candidate, feature, index)
				if err != nil {
					return nil, err
				}
				report.RecomputedKeys = append(report.RecomputedKeys, key)
				if window.EventCount == 0 {
					report.RemovedKeys = append(report.RemovedKeys, key)
					delete(c.windows, key)
					continue
				}
				window.Invalidation = &kernel.ResultProvenance{
					Status: kernel.ProvenanceRecomputed, ReasonCode: reason,
					InvalidationSources: kernel.UniqueSortedStrings(sourcesByKey[key]),
					CacheKey:            key,
				}
				c.windows[key] = window
			} else {
				window = c.windows[key]
				window.Invalidation = &kernel.ResultProvenance{
					Status: kernel.ProvenanceReused, ReasonCode: "unaffected_cache_entry_fresh",
					CacheKey: key, PropagationClosure: []string{key},
				}
				report.ReusedKeys = append(report.ReusedKeys, key)
			}
			report.ResultSet.Windows = append(report.ResultSet.Windows, window)
		}
	}

	for key := range c.windows {
		if !currentKeys[key] {
			report.RemovedKeys = append(report.RemovedKeys, key)
			delete(c.windows, key)
		}
	}

	dependents, err := c.recomputeDerived(candidate, report, invalidated, sourcesByKey)
	if err != nil {
		return nil, err
	}
	for windowKeyName, derivedKeys := range dependents {
		if sources := sourcesByKey[windowKeyName]; len(sources) > 0 {
			for _, source := range sources {
				keysBySource[source] = append(keysBySource[source], derivedKeys...)
			}
		}
	}

	for i := range report.ResultSet.Windows {
		key := report.ResultSet.Windows[i].Invalidation.CacheKey
		closure := []string{key}
		closure = append(closure, dependents[key]...)
		report.ResultSet.Windows[i].Invalidation.PropagationClosure = kernel.UniqueSortedStrings(closure)
	}

	c.project = candidate
	report.InvalidatedKeys = sortedKeys(invalidated)
	report.RecomputedKeys = kernel.UniqueSortedStrings(report.RecomputedKeys)
	report.ReusedKeys = kernel.UniqueSortedStrings(report.ReusedKeys)
	report.RemovedKeys = kernel.UniqueSortedStrings(report.RemovedKeys)
	report.SourceClosures = buildSourceClosures(keysBySource, dependents)
	return report, nil
}

func (c *Cache) recomputeDerived(project kernel.Project, report *Report, invalidated map[string]bool, sourcesByKey map[string][]string) (map[string][]string, error) {
	byGeometry := map[string]kernel.WindowResult{}
	for _, window := range report.ResultSet.Windows {
		byGeometry[geometryKey(window.FeatureID, window.StartMs, window.EndMs)] = window
	}
	dependents := map[string][]string{}

	for _, spec := range project.Derived {
		for _, base := range report.ResultSet.Windows {
			if base.FeatureID != spec.BaseFeatureID {
				continue
			}
			numerator, okNum := byGeometry[geometryKey(spec.NumeratorFeature, base.StartMs, base.EndMs)]
			denominator, okDen := byGeometry[geometryKey(spec.DenominatorFeature, base.StartMs, base.EndMs)]
			numeratorKey := windowKey(spec.NumeratorFeature, base.WindowIndex)
			denominatorKey := windowKey(spec.DenominatorFeature, base.WindowIndex)
			if okNum {
				numeratorKey = windowKey(numerator.FeatureID, numerator.WindowIndex)
			}
			if okDen {
				denominatorKey = windowKey(denominator.FeatureID, denominator.WindowIndex)
			}
			key := derivedKey(spec.ID, base.WindowIndex)
			dependents[numeratorKey] = appendUnique(dependents[numeratorKey], key)
			dependents[denominatorKey] = appendUnique(dependents[denominatorKey], key)

			shouldRecompute := invalidated[numeratorKey] || invalidated[denominatorKey]
			if _, cached := c.derived[key]; !cached {
				shouldRecompute = true
			}
			var result kernel.DerivedResult
			if shouldRecompute {
				num := 0.0
				if okNum {
					num = aggregationValue(numerator.Aggregations, kernel.AggSum)
				}
				den := 0.0
				if okDen {
					den = aggregationValue(denominator.Aggregations, kernel.AggSum)
				}
				value := 0.0
				if den != 0 {
					value = num / den
				}
				result = kernel.DerivedResult{
					DerivedID: spec.ID, WindowID: spec.ID + ":" + base.WindowID, WindowIndex: base.WindowIndex,
					StartMs: base.StartMs, EndMs: base.EndMs, Value: value,
					Dependencies: []string{numeratorKey, denominatorKey},
				}
				sources := append(append([]string{}, sourcesByKey[numeratorKey]...), sourcesByKey[denominatorKey]...)
				result.Invalidation = &kernel.ResultProvenance{
					Status: kernel.ProvenanceRecomputed, ReasonCode: "dependency_closure_invalidated",
					InvalidationSources: kernel.UniqueSortedStrings(sources),
					PropagationClosure:  kernel.UniqueSortedStrings([]string{numeratorKey, denominatorKey, key}),
					CacheKey:            key,
				}
				c.derived[key] = result
			} else {
				result = c.derived[key]
				result.Invalidation = &kernel.ResultProvenance{
					Status: kernel.ProvenanceReused, ReasonCode: "derived_dependencies_fresh",
					CacheKey: key, PropagationClosure: []string{numeratorKey, denominatorKey, key},
				}
			}
			report.ResultSet.Derived = append(report.ResultSet.Derived, result)
		}
	}
	sort.Slice(report.ResultSet.Derived, func(i, j int) bool {
		if report.ResultSet.Derived[i].DerivedID != report.ResultSet.Derived[j].DerivedID {
			return report.ResultSet.Derived[i].DerivedID < report.ResultSet.Derived[j].DerivedID
		}
		return report.ResultSet.Derived[i].WindowIndex < report.ResultSet.Derived[j].WindowIndex
	})
	return dependents, nil
}

func buildSourceClosures(keysBySource map[string][]string, dependents map[string][]string) []SourceClosure {
	sources := make([]string, 0, len(keysBySource))
	for source := range keysBySource {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	out := make([]SourceClosure, 0, len(sources))
	for _, source := range sources {
		windowKeys := kernel.UniqueSortedStrings(keysBySource[source])
		derivedKeys := []string{}
		closure := append([]string{source}, windowKeys...)
		for _, key := range windowKeys {
			if deps, ok := dependents[key]; ok {
				derivedKeys = append(derivedKeys, deps...)
			}
		}
		derivedKeys = kernel.UniqueSortedStrings(derivedKeys)
		closure = append(closure, derivedKeys...)
		out = append(out, SourceClosure{
			Source: source, WindowKeys: windowKeys, DerivedKeys: derivedKeys,
			Closure: kernel.UniqueSortedStrings(closure),
		})
	}
	return out
}

func sortedKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func appendUnique(values []string, additions ...string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range additions {
		if !seen[value] {
			values = append(values, value)
			seen[value] = true
		}
	}
	return values
}

func keyBelongsFeature(key, featureID string) bool {
	prefix := "window:" + featureID + ":"
	return len(key) > len(prefix) && key[:len(prefix)] == prefix
}

func geometryKey(featureID string, start, end int64) string {
	return featureID + "|" + fmt.Sprint(start) + "|" + fmt.Sprint(end)
}

func aggregationValue(values []kernel.AggregationValue, fn kernel.AggFunc) float64 {
	for _, value := range values {
		if value.Func == fn {
			return value.Value
		}
	}
	return 0
}

func validateCandidate(project kernel.Project) error {
	if err := kernel.ValidateBudget(project.Budget); err != nil {
		return err
	}
	for _, feature := range project.Features {
		if err := kernel.ValidateFeature(feature); err != nil {
			return err
		}
	}
	return nil
}
