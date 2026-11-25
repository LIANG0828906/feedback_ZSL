package lightshow

import (
	"context"
	"fmt"
	"sort"
)

type InvalidationReport struct {
	EditedID       string         `json:"edited_id"`
	EditedKind     string         `json:"edited_kind"`
	Invalidated    []int          `json:"invalidated"`
	Reused         []int          `json:"reused"`
	Exports        []string       `json:"exports"`
	Reasons        map[int]string `json:"reasons"`
	DependencyNote string         `json:"dependency_note"`
}

type IncrementalResult struct {
	Report   InvalidationReport
	Schedule *ScheduleResult
}

type IncrementalEngine struct {
	Store *Store
	Show  *Show
}

func NewIncrementalEngine(store *Store, show *Show) (*IncrementalEngine, error) {
	if store == nil || show == nil {
		return nil, engineError(CodeVersion, "project", "show and store are required")
	}
	copy := show.Clone()
	if err := copy.Normalize(); err != nil {
		return nil, err
	}
	return &IncrementalEngine{Store: store, Show: copy}, nil
}

func (e *IncrementalEngine) EditChange(id string, mutate func(*Change) error) (*IncrementalResult, error) {
	next := e.Show.Clone()
	change := next.ChangeByID(id)
	if change == nil {
		return nil, engineError(CodeVersion, "changes", "unknown change %q", id)
	}
	if mutate != nil {
		if err := mutate(change); err != nil {
			return nil, err
		}
	}
	return e.recompute(next, id, "change")
}

func (e *IncrementalEngine) EditGroup(id string, mutate func(*Group) error) (*IncrementalResult, error) {
	next := e.Show.Clone()
	group := next.GroupByID(id)
	if group == nil {
		return nil, engineError(CodeVersion, "groups", "unknown group %q", id)
	}
	if mutate != nil {
		if err := mutate(group); err != nil {
			return nil, err
		}
	}
	return e.recompute(next, id, "group")
}

func (e *IncrementalEngine) Recompute(show *Show) (*IncrementalResult, error) {
	if show == nil {
		return nil, engineError(CodeVersion, "project", "show is nil")
	}
	return e.recompute(show.Clone(), "", "project")
}

func (e *IncrementalEngine) recompute(next *Show, editedID, editedKind string) (*IncrementalResult, error) {
	if err := next.Normalize(); err != nil {
		return nil, err
	}
	oldShow := e.Show
	oldHash := ProjectHash(oldShow)
	algorithm := AlgorithmVersion
	source := oldShow.SourceVersion.String()
	format := oldShow.SourceVersion.String()
	oldCache, err := e.Store.ReadCommitted(oldHash, algorithm, source, format)
	if err != nil {
		return nil, err
	}
	cacheByIndex := make(map[int]WindowCache, len(oldCache))
	for _, entry := range oldCache {
		cacheByIndex[entry.Result.Index] = entry
	}

	plans, err := WindowDependencyPlans(next)
	if err != nil {
		return nil, err
	}
	newHash := ProjectHash(next)
	report := InvalidationReport{
		EditedID: editedID, EditedKind: editedKind,
		Reasons:        map[int]string{},
		DependencyNote: "a window is invalidated only when its outputs, decisions, consumed changes, group snapshot, or dependency set changes",
	}
	currentIndices := make(map[int]bool, len(plans))
	var candidates []int
	for _, plan := range plans {
		currentIndices[plan.Index] = true
		cached, ok := cacheByIndex[plan.Index]
		if !ok || editedKind == "project" ||
			plan.StartUS != cached.Result.StartUS || plan.EndUS != cached.Result.EndUS ||
			plan.FrameStart != cached.Result.FrameStart || plan.FrameEnd != cached.Result.FrameEnd ||
			!dependenciesEqual(cached.Result.Dependencies, plan.Dependencies) ||
			(editedID != "" && (dependenciesContain(cached.Result.Dependencies, editedID) || dependenciesContain(plan.Dependencies, editedID))) {
			candidates = append(candidates, plan.Index)
		}
	}
	compiled, err := ScheduleWindows(next, candidates)
	if err != nil {
		return nil, err
	}
	compiledByIndex := make(map[int]WindowResult, len(compiled))
	for _, window := range compiled {
		compiledByIndex[window.Index] = window
	}

	var toCommit []WindowResult
	commitSet := map[int]bool{}
	allWindows := make([]WindowResult, 0, len(plans))
	for _, plan := range plans {
		cached, ok := cacheByIndex[plan.Index]
		window, wasCandidate := compiledByIndex[plan.Index]
		if !wasCandidate {
			if !ok {
				return nil, engineError(CodeDecode, "incremental.window", "window %d was not scheduled or cached", plan.Index)
			}
			report.Reused = append(report.Reused, plan.Index)
			allWindows = append(allWindows, cached.Result)
			continue
		}
		if ok && cached.Result.ContentHash == window.ContentHash {
			report.Reused = append(report.Reused, plan.Index)
			allWindows = append(allWindows, cached.Result)
			continue
		}
		report.Invalidated = append(report.Invalidated, plan.Index)
		report.Exports = append(report.Exports, fmt.Sprintf("export:window-%04d", plan.Index))
		report.Reasons[plan.Index] = invalidationReason(window, editedID)
		toCommit = append(toCommit, window)
		commitSet[plan.Index] = true
		allWindows = append(allWindows, window)
	}
	for index := range cacheByIndex {
		if !currentIndices[index] {
			if err := e.Store.DeleteWindow(fmt.Sprintf("window-%04d", index)); err != nil {
				return nil, err
			}
		}
	}
	for _, window := range allWindows {
		if commitSet[window.Index] {
			continue
		}
		if err := e.Store.AdoptWindow(window.WindowID, newHash, algorithm, source, format); err != nil {
			return nil, err
		}
	}

	for _, window := range toCommit {
		if err := e.Store.DeleteWindow(window.WindowID); err != nil {
			return nil, err
		}
		if err := e.Store.EnsureWindow(window); err != nil {
			return nil, err
		}
		if err := e.Store.Transition(window.WindowID, StateScheduling); err != nil {
			return nil, err
		}
		window.ContentHash = window.ComputeContentHash()
		if err := e.Store.CommitWindow(context.Background(), window, newHash, algorithm, source, format); err != nil {
			_ = e.Store.Fail(window.WindowID)
			return nil, err
		}
	}
	sort.Ints(report.Invalidated)
	sort.Ints(report.Reused)
	e.Show = next
	return &IncrementalResult{Report: report, Schedule: combineWindows(allWindows)}, nil
}

func invalidationReason(window WindowResult, editedID string) string {
	if editedID != "" {
		for _, dep := range window.Dependencies {
			if (dep.Kind == "change" && dep.Key == editedID) || (dep.Kind == "group" && dep.Key == editedID) {
				return "dependency"
			}
		}
	}
	return "content_changed"
}

func dependenciesContain(deps []Dependency, id string) bool {
	for _, dep := range deps {
		if (dep.Kind == "change" || dep.Kind == "group") && dep.Key == id {
			return true
		}
	}
	return false
}

func dependenciesEqual(a, b []Dependency) bool {
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
