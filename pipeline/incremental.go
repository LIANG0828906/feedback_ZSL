package pipeline

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

const (
	InvalidateFeatureWindow = "I_FEATURE_WINDOW_CHANGED"
	InvalidateAggregation   = "I_AGGREGATION_CHANGED"
	InvalidateEventAdded    = "I_EVENT_ADDED"
	InvalidateEventRemoved  = "I_EVENT_REMOVED"
	InvalidateEventValue    = "I_EVENT_VALUE_CHANGED"
	InvalidateEventTime     = "I_EVENT_TIME_CHANGED"
	InvalidateEventFeature  = "I_EVENT_FEATURE_CHANGED"
	InvalidateEventOther    = "I_EVENT_CONTENT_CHANGED"
	InvalidateDependency    = "I_DERIVED_DEPENDENCY"
)

// InvalidationSource describes the edit that made a window stale.
type InvalidationSource struct {
	Kind         string     `json:"kind"`
	FeatureID    string     `json:"feature_id,omitempty"`
	EventSeq     int64      `json:"event_seq,omitempty"`
	ReasonCode   string     `json:"reason_code"`
	ParentWindow *WindowKey `json:"parent_window,omitempty"`
}

// WindowInvalidation ties a recomputed window to its source and dependency
// path. A derived window lists the parent window through which staleness
// propagated.
type WindowInvalidation struct {
	Window      WindowKey            `json:"window"`
	Sources     []InvalidationSource `json:"sources"`
	ClosurePath []WindowKey          `json:"closure_path,omitempty"`
}

// EditClosure is the transitive invalidation closure for one edit source.
type EditClosure struct {
	Source             InvalidationSource `json:"source"`
	InitialWindows     []WindowKey        `json:"initial_windows"`
	PropagationClosure []WindowKey        `json:"propagation_closure"`
}

// IncrementalResult contains the incrementally assembled result and audit
// information. MatchesFull is set by an explicit verification run.
type IncrementalResult struct {
	Result            *RunResult           `json:"result"`
	Edits             []EditClosure        `json:"edits"`
	Invalidations     []WindowInvalidation `json:"invalidations"`
	RecomputedWindows []WindowKey          `json:"recomputed_windows"`
	ReusedWindows     []WindowKey          `json:"reused_windows"`
	RemovedWindows    []WindowKey          `json:"removed_windows,omitempty"`
	MatchesFull       bool                 `json:"matches_full"`
	Mismatches        []string             `json:"mismatches,omitempty"`
}

// ApplyIncrement recomputes only windows reached by the edit dependency
// closure. Reused windows are copied verbatim from the previous result. A
// separate full run is used only to verify the increment and populate
// MatchesFull; it does not contribute values to Result.
func ApplyIncrement(ctx context.Context, previous *Project, previousResult *RunResult, next *Project, opts RunOptions) (*IncrementalResult, error) {
	if previous == nil || next == nil {
		return nil, NewBusinessError(CodeEditTargetInvalid, "both previous and next projects are required")
	}
	if err := validateProjectForRun(next); err != nil {
		return nil, err
	}
	if err := validateGlobalWatermark(next); err != nil {
		return nil, err
	}
	opts = normalizeRunOptions(opts, len(next.Features))
	if err := validateRunOptions(opts); err != nil {
		return nil, err
	}
	newPlans, err := WindowPlans(next)
	if err != nil {
		return nil, err
	}
	oldPlans, err := WindowPlans(previous)
	if err != nil {
		return nil, err
	}
	diffs, err := collectEditDiffs(previous, next)
	if err != nil {
		return nil, err
	}
	if previousResult == nil {
		full, err := Run(ctx, next, opts)
		if err != nil {
			return nil, err
		}
		return &IncrementalResult{
			Result: full, MatchesFull: true,
			RecomputedWindows: allPlanKeys(newPlans),
		}, nil
	}

	affected := map[WindowKey]*affectedWindow{}
	for _, diff := range diffs {
		keys := expandInitialWindows(diff, oldPlans, newPlans)
		for _, key := range keys {
			entry := ensureAffected(affected, key)
			entry.sources = appendUniqueSource(entry.sources, diff.source)
			entry.initial = true
		}
	}
	propagateDependencies(next, newPlans, affected)

	oldWindows := map[WindowKey]WindowResult{}
	for _, w := range previousResult.Windows {
		oldWindows[w.Key] = w
	}
	oldDiscards := make(map[WindowKey][]DiscardRecord)
	for _, d := range previousResult.Discards {
		oldDiscards[d.Window] = append(oldDiscards[d.Window], d)
	}
	newKeys := allPlanKeys(newPlans)
	newKeySet := map[WindowKey]bool{}
	for _, key := range newKeys {
		newKeySet[key] = true
	}
	var removed []WindowKey
	for _, key := range allPlanKeys(oldPlans) {
		if !newKeySet[key] {
			removed = append(removed, key)
		}
	}

	var windows []WindowResult
	var discards []DiscardRecord
	var recomputed, reused []WindowKey
	for _, key := range newKeys {
		feature, ok := next.FeatureByID(key.FeatureID)
		if !ok {
			return nil, NewBusinessError(CodeFeatureUnknown, "window references unknown feature").
				WithContext("feature_id", key.FeatureID)
		}
		if _, changed := affected[key]; !changed {
			if cached, ok := oldWindows[key]; ok {
				windows = append(windows, cached)
				discards = append(discards, oldDiscards[key]...)
				reused = append(reused, key)
				continue
			}
		}
		value, rejected, err := recomputeWindow(next, feature, key, opts.RatioThreshold)
		if err != nil {
			return nil, err
		}
		windows = append(windows, value)
		discards = append(discards, rejected...)
		recomputed = append(recomputed, key)
	}
	sortWindows(windows)
	sortDiscards(discards)
	sortWindowKeys(recomputed)
	sortWindowKeys(reused)
	sortWindowKeys(removed)

	result := buildRunResult(next, windows, discards, opts)
	result.ResumedFromSeq = 0
	result.LastCommittedSeq = int64(len(next.Events))
	increment := &IncrementalResult{
		Result: result, RecomputedWindows: recomputed, ReusedWindows: reused,
		RemovedWindows: removed,
	}
	increment.Invalidations = buildWindowInvalidations(affected)
	increment.Edits = buildEditClosures(affected, newPlans)
	verifyIncrementMatchesFull(ctx, next, opts, increment)
	return increment, nil
}

type editDiff struct {
	source  InvalidationSource
	initial []WindowKey
}

type affectedWindow struct {
	sources []InvalidationSource
	via     []WindowKey
	initial bool
}

func ensureAffected(m map[WindowKey]*affectedWindow, key WindowKey) *affectedWindow {
	entry, ok := m[key]
	if !ok {
		entry = &affectedWindow{}
		m[key] = entry
	}
	return entry
}

func collectEditDiffs(oldProject, newProject *Project) ([]editDiff, error) {
	var diffs []editDiff
	oldFeatures := map[string]FeatureDefinition{}
	for _, f := range oldProject.Features {
		oldFeatures[f.ID] = f
	}
	newFeatures := map[string]FeatureDefinition{}
	for _, f := range newProject.Features {
		newFeatures[f.ID] = f
	}
	allFeatureIDs := map[string]bool{}
	for id := range oldFeatures {
		allFeatureIDs[id] = true
	}
	for id := range newFeatures {
		allFeatureIDs[id] = true
	}
	featureIDs := make([]string, 0, len(allFeatureIDs))
	for id := range allFeatureIDs {
		featureIDs = append(featureIDs, id)
	}
	sort.Strings(featureIDs)
	for _, id := range featureIDs {
		oldF, oldOK := oldFeatures[id]
		newF, newOK := newFeatures[id]
		if oldOK && newOK {
			if oldF.WindowWidthMs != newF.WindowWidthMs || oldF.WindowStepMs != newF.WindowStepMs ||
				oldF.AllowedLatenessMs != newF.AllowedLatenessMs {
				diffs = append(diffs, editDiff{
					source: InvalidationSource{Kind: "feature_definition", FeatureID: id, ReasonCode: InvalidateFeatureWindow},
				})
			}
			if !reflectAggsEqual(oldF.Aggregations, newF.Aggregations) {
				diffs = append(diffs, editDiff{
					source: InvalidationSource{Kind: "aggregation", FeatureID: id, ReasonCode: InvalidateAggregation},
				})
			}
			if !reflectStringsEqual(oldF.DerivedFrom, newF.DerivedFrom) {
				diffs = append(diffs, editDiff{
					source: InvalidationSource{Kind: "feature_definition", FeatureID: id, ReasonCode: InvalidateFeatureWindow},
				})
			}
		} else if newOK {
			diffs = append(diffs, editDiff{
				source: InvalidationSource{Kind: "feature_added", FeatureID: id, ReasonCode: InvalidateFeatureWindow},
			})
		} else {
			diffs = append(diffs, editDiff{
				source: InvalidationSource{Kind: "feature_removed", FeatureID: id, ReasonCode: InvalidateFeatureWindow},
			})
		}
	}
	oldEvents := map[int64]ObservationEvent{}
	for _, e := range oldProject.Events {
		oldEvents[e.Seq] = e
	}
	newEvents := map[int64]ObservationEvent{}
	maxSeq := int64(0)
	for _, e := range newProject.Events {
		newEvents[e.Seq] = e
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	for seq := range oldEvents {
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	for seq := int64(1); seq <= maxSeq; seq++ {
		oldE, oldOK := oldEvents[seq]
		newE, newOK := newEvents[seq]
		switch {
		case !oldOK && newOK:
			diffs = append(diffs, eventDiff(InvalidateEventAdded, "event_added", newE, newE.ObservedAtMs))
		case oldOK && !newOK:
			diffs = append(diffs, eventDiff(InvalidateEventRemoved, "event_removed", oldE, oldE.ObservedAtMs))
		case oldOK && newOK:
			if oldE.Value != newE.Value {
				diffs = append(diffs, eventDiff(InvalidateEventValue, "event_value", newE, newE.ObservedAtMs))
			}
			if oldE.ObservedAtMs != newE.ObservedAtMs {
				d := eventDiff(InvalidateEventTime, "event_time", newE, newE.ObservedAtMs)
				d.source.FeatureID = newE.FeatureID
				d.initial = append(d.initial, WindowKey{FeatureID: oldE.FeatureID, StartMs: oldE.ObservedAtMs})
				if oldE.FeatureID != newE.FeatureID {
					d.source.ReasonCode = InvalidateEventFeature
				}
				diffs = append(diffs, d)
			}
			if oldE.FeatureID != newE.FeatureID {
				d := eventDiff(InvalidateEventFeature, "event_feature", newE, newE.ObservedAtMs)
				d.initial = append(d.initial, WindowKey{FeatureID: oldE.FeatureID, StartMs: oldE.ObservedAtMs})
				diffs = append(diffs, d)
			}
			if oldE.Channel != newE.Channel {
				diffs = append(diffs, eventDiff(InvalidateEventOther, "event_channel", newE, newE.ObservedAtMs))
			}
			if !watermarkEqual(oldE.WatermarkMs, newE.WatermarkMs) {
				diffs = append(diffs, eventDiff(InvalidateEventOther, "event_watermark", newE, newE.ObservedAtMs))
			}
		}
	}
	return diffs, nil
}

func eventDiff(code, kind string, event ObservationEvent, observed int64) editDiff {
	return editDiff{
		source: InvalidationSource{
			Kind: kind, FeatureID: event.FeatureID, EventSeq: event.Seq, ReasonCode: code,
		},
		initial: []WindowKey{{FeatureID: event.FeatureID, StartMs: observed}},
	}
}

func watermarkEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func reflectAggsEqual(a, b []Aggregation) bool {
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

func reflectStringsEqual(a, b []string) bool {
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

// expandInitialWindows converts a diff into concrete old/new windows. Event
// edits at a timestamp affect every overlapping window in either plan.
func expandInitialWindows(diff editDiff, oldPlans, newPlans map[string]FeatureWindowPlan) []WindowKey {
	var out []WindowKey
	if diff.source.Kind == "feature_definition" || diff.source.Kind == "aggregation" ||
		diff.source.Kind == "feature_added" || diff.source.Kind == "feature_removed" {
		for _, plan := range oldPlans {
			if plan.FeatureID == diff.source.FeatureID {
				out = append(out, plan.Windows...)
			}
		}
		for _, plan := range newPlans {
			if plan.FeatureID == diff.source.FeatureID {
				out = append(out, plan.Windows...)
			}
		}
		return dedupKeys(out)
	}
	if diff.source.EventSeq > 0 {
		// The initial placeholder contains the observed time and feature.
		for _, key := range diff.initial {
			for _, plan := range oldPlans {
				if plan.FeatureID == key.FeatureID {
					out = append(out, windowsContaining(plan.Windows, key.StartMs)...)
				}
			}
			for _, plan := range newPlans {
				if plan.FeatureID == key.FeatureID {
					out = append(out, windowsContaining(plan.Windows, key.StartMs)...)
				}
			}
		}
	}
	return dedupKeys(out)
}

func windowsContaining(windows []WindowKey, observed int64) []WindowKey {
	var out []WindowKey
	for _, key := range windows {
		if observed >= key.StartMs && observed < key.EndMs {
			out = append(out, key)
		}
	}
	return out
}

func dedupKeys(keys []WindowKey) []WindowKey {
	seen := map[WindowKey]bool{}
	var out []WindowKey
	for _, key := range keys {
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	sortWindowKeys(out)
	return out
}

func propagateDependencies(project *Project, plans map[string]FeatureWindowPlan, affected map[WindowKey]*affectedWindow) {
	dependents := map[string][]FeatureDefinition{}
	for _, f := range project.Features {
		for _, parent := range f.DerivedFrom {
			dependents[parent] = append(dependents[parent], f)
		}
	}
	changed := true
	for changed {
		changed = false
		keys := make([]WindowKey, 0, len(affected))
		for key := range affected {
			keys = append(keys, key)
		}
		sortWindowKeys(keys)
		for _, key := range keys {
			entry := affected[key]
			for _, child := range dependents[key.FeatureID] {
				plan := plans[child.ID]
				for _, childKey := range plan.Windows {
					if !rangesOverlap(key, childKey) {
						continue
					}
					childEntry, exists := affected[childKey]
					if !exists {
						childEntry = ensureAffected(affected, childKey)
						changed = true
					}
					for _, source := range entry.sources {
						parent := key
						derived := source
						derived.Kind = "dependency_closure"
						derived.ReasonCode = InvalidateDependency
						derived.ParentWindow = &parent
						childEntry.sources = appendUniqueSource(childEntry.sources, derived)
					}
					childEntry.via = appendUniqueKey(childEntry.via, key)
					_ = childEntry
				}
			}
		}
	}
}

func rangesOverlap(a, b WindowKey) bool {
	return a.StartMs < b.EndMs && b.StartMs < a.EndMs
}

func appendUniqueSource(list []InvalidationSource, source InvalidationSource) []InvalidationSource {
	for _, existing := range list {
		parentEqual := existing.ParentWindow == nil && source.ParentWindow == nil
		if existing.ParentWindow != nil && source.ParentWindow != nil {
			parentEqual = *existing.ParentWindow == *source.ParentWindow
		}
		if existing.Kind == source.Kind && existing.FeatureID == source.FeatureID &&
			existing.EventSeq == source.EventSeq && existing.ReasonCode == source.ReasonCode && parentEqual {
			return list
		}
	}
	return append(list, source)
}

func appendUniqueKey(list []WindowKey, key WindowKey) []WindowKey {
	for _, existing := range list {
		if existing == key {
			return list
		}
	}
	return append(list, key)
}

func recomputeWindow(project *Project, feature FeatureDefinition, key WindowKey, threshold int64) (WindowResult, []DiscardRecord, error) {
	var refs []eventRef
	var discards []DiscardRecord
	var recompute int64
	mode := TraversalRescan
	reason := PathReasonRescan
	if feature.WindowWidthMs > threshold*feature.WindowStepMs {
		mode = TraversalCumulative
		reason = PathReasonCumulative
	}
	for _, event := range project.Events {
		if event.FeatureID != feature.ID || event.ObservedAtMs < key.StartMs || event.ObservedAtMs >= key.EndMs {
			continue
		}
		if project.WatermarkEnabled {
			if event.WatermarkMs == nil {
				return WindowResult{}, nil, NewBusinessError(CodeWatermarkMissing, "watermark missing during incremental recompute").
					WithContext("event_seq", fmt.Sprint(event.Seq)).WithContext("window", key.String())
			}
			wm := *event.WatermarkMs
			if wm >= key.EndMs+feature.AllowedLatenessMs {
				discards = append(discards, DiscardRecord{
					EventSeq: event.Seq, FeatureID: feature.ID, Window: key,
					ObservedAtMs: event.ObservedAtMs, WatermarkMs: wm,
					LateByMs:   wm - event.ObservedAtMs,
					ReasonCode: "E_LATE_BEYOND_ALLOWED_LATENESS",
				})
				continue
			}
			if wm > event.ObservedAtMs && len(refs) > 0 {
				recompute++
			}
		}
		refs = append(refs, eventRef{Seq: event.Seq, ObservedAtMs: event.ObservedAtMs, Value: event.Value})
	}
	sortRefs(refs)
	return WindowResult{
		Key: key, Status: WindowCommitted, EventSeqs: refsToSeqs(refs),
		ObservationCount: len(refs), Aggregates: aggregateRefs(refs, feature.Aggregations),
		RecomputeCount: recompute, Traversal: mode, PathReasonCode: reason,
	}, discards, nil
}

func buildRunResult(project *Project, windows []WindowResult, discards []DiscardRecord, opts RunOptions) *RunResult {
	result := &RunResult{
		ProjectID: project.ProjectID, SchemaVersion: project.SchemaVersion,
		WatermarkEnabled: project.WatermarkEnabled, WatermarkApplied: project.WatermarkEnabled,
		BufferLimit: opts.WindowBufferPerFeature, WorkersUsed: minInt(opts.Workers, len(project.Features)),
		WatermarkNote: "enabled", Windows: windows, Discards: discards,
	}
	if !project.WatermarkEnabled {
		result.WatermarkNote = "disabled_by_declaration"
	}
	for _, f := range project.Features {
		mode := TraversalRescan
		reason := PathReasonRescan
		if f.WindowWidthMs > opts.RatioThreshold*f.WindowStepMs {
			mode = TraversalCumulative
			reason = PathReasonCumulative
		}
		result.PathDecisions = append(result.PathDecisions, PathDecision{
			FeatureID: f.ID, WidthMs: f.WindowWidthMs, StepMs: f.WindowStepMs,
			RatioNum: f.WindowWidthMs, RatioDen: f.WindowStepMs, Threshold: opts.RatioThreshold,
			Mode: mode, ReasonCode: reason,
		})
	}
	sort.Slice(result.PathDecisions, func(i, j int) bool {
		return result.PathDecisions[i].FeatureID < result.PathDecisions[j].FeatureID
	})
	for _, w := range windows {
		result.RecomputeTotal += w.RecomputeCount
	}
	return result
}

func buildWindowInvalidations(affected map[WindowKey]*affectedWindow) []WindowInvalidation {
	keys := make([]WindowKey, 0, len(affected))
	for key := range affected {
		keys = append(keys, key)
	}
	sortWindowKeys(keys)
	out := make([]WindowInvalidation, 0, len(keys))
	for _, key := range keys {
		entry := affected[key]
		sources := append([]InvalidationSource(nil), entry.sources...)
		sort.Slice(sources, func(i, j int) bool {
			return invalidationSourceKey(sources[i]) < invalidationSourceKey(sources[j])
		})
		via := append([]WindowKey(nil), entry.via...)
		sortWindowKeys(via)
		out = append(out, WindowInvalidation{Window: key, Sources: sources, ClosurePath: via})
	}
	return out
}

func buildEditClosures(affected map[WindowKey]*affectedWindow, plans map[string]FeatureWindowPlan) []EditClosure {
	type group struct {
		source  InvalidationSource
		initial []WindowKey
	}
	groups := map[string]*group{}
	for key, entry := range affected {
		if !entry.initial {
			continue
		}
		for _, source := range entry.sources {
			if source.Kind == "dependency_closure" {
				continue
			}
			id := invalidationSourceKey(source)
			g, ok := groups[id]
			if !ok {
				g = &group{source: source}
				groups[id] = g
			}
			g.initial = appendUniqueKey(g.initial, key)
		}
	}
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]EditClosure, 0, len(ids))
	for _, id := range ids {
		g := groups[id]
		closure := dependencyClosure(g.initial, affected)
		sortWindowKeys(g.initial)
		out = append(out, EditClosure{
			Source: g.source, InitialWindows: g.initial, PropagationClosure: closure,
		})
	}
	return out
}

func dependencyClosure(initial []WindowKey, affected map[WindowKey]*affectedWindow) []WindowKey {
	seen := map[WindowKey]bool{}
	queue := append([]WindowKey(nil), initial...)
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if seen[key] {
			continue
		}
		seen[key] = true
		entry := affected[key]
		if entry == nil {
			continue
		}
		for _, via := range entry.via {
			if !seen[via] {
				queue = append(queue, via)
			}
		}
		// Reverse dependency edges are represented by propagated parent paths.
		for otherKey, other := range affected {
			for _, parent := range other.via {
				if parent == key && !seen[otherKey] {
					queue = append(queue, otherKey)
				}
			}
		}
	}
	keys := make([]WindowKey, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sortWindowKeys(keys)
	return keys
}

func verifyIncrementMatchesFull(ctx context.Context, project *Project, opts RunOptions, increment *IncrementalResult) {
	verifyOpts := opts
	verifyOpts.Checkpoint = nil
	verifyOpts.Resume = false
	full, err := Run(ctx, project, verifyOpts)
	if err != nil {
		increment.Mismatches = append(increment.Mismatches, "verification run failed: "+err.Error())
		return
	}
	if err := compareResults(full, increment.Result); err != nil {
		increment.Mismatches = append(increment.Mismatches, err.Error())
		increment.MatchesFull = false
		return
	}
	increment.MatchesFull = true
}

func compareResults(want, got *RunResult) error {
	if len(want.Windows) != len(got.Windows) {
		return fmt.Errorf("window count: full=%d incremental=%d", len(want.Windows), len(got.Windows))
	}
	for i := range want.Windows {
		if !windowSemanticallyEqual(want.Windows[i], got.Windows[i]) {
			return fmt.Errorf("window differs at %s", want.Windows[i].Key)
		}
	}
	if len(want.Discards) != len(got.Discards) {
		return fmt.Errorf("discard count: full=%d incremental=%d", len(want.Discards), len(got.Discards))
	}
	for i := range want.Discards {
		if want.Discards[i] != got.Discards[i] {
			return fmt.Errorf("discard differs at event %d window %s",
				want.Discards[i].EventSeq, want.Discards[i].Window)
		}
	}
	if !reflectPathDecisions(want.PathDecisions, got.PathDecisions) {
		return fmt.Errorf("path decisions differ")
	}
	return nil
}

func windowSemanticallyEqual(a, b WindowResult) bool {
	if a.Key != b.Key || a.Status != b.Status || a.ObservationCount != b.ObservationCount ||
		a.RecomputeCount != b.RecomputeCount || a.Traversal != b.Traversal ||
		a.PathReasonCode != b.PathReasonCode {
		return false
	}
	if len(a.EventSeqs) != len(b.EventSeqs) || len(a.Aggregates) != len(b.Aggregates) {
		return false
	}
	for i := range a.EventSeqs {
		if a.EventSeqs[i] != b.EventSeqs[i] {
			return false
		}
	}
	for i := range a.Aggregates {
		if a.Aggregates[i] != b.Aggregates[i] {
			return false
		}
	}
	return true
}

func reflectPathDecisions(a, b []PathDecision) bool {
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

func invalidationSourceKey(source InvalidationSource) string {
	parent := ""
	if source.ParentWindow != nil {
		parent = source.ParentWindow.String()
	}
	return strings.Join([]string{
		source.Kind, source.FeatureID, fmt.Sprint(source.EventSeq), source.ReasonCode, parent,
	}, "|")
}

func allPlanKeys(plans map[string]FeatureWindowPlan) []WindowKey {
	var keys []WindowKey
	for _, plan := range plans {
		keys = append(keys, plan.Windows...)
	}
	sortWindowKeys(keys)
	return keys
}

func sortWindowKeys(keys []WindowKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].FeatureID != keys[j].FeatureID {
			return keys[i].FeatureID < keys[j].FeatureID
		}
		return keys[i].StartMs < keys[j].StartMs
	})
}

func sortWindows(windows []WindowResult) {
	sort.Slice(windows, func(i, j int) bool {
		if windows[i].Key.FeatureID != windows[j].Key.FeatureID {
			return windows[i].Key.FeatureID < windows[j].Key.FeatureID
		}
		return windows[i].Key.StartMs < windows[j].Key.StartMs
	})
}
