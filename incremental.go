package macrokit

import "sort"

type CacheEntry struct {
	BranchID    string       `json:"branch_id"`
	ContentHash string       `json:"content_hash"`
	State       *BranchState `json:"state"`
}

type ResultCache struct {
	entries map[string]CacheEntry
}

func NewResultCache() *ResultCache {
	return &ResultCache{entries: map[string]CacheEntry{}}
}

func (c *ResultCache) Get(branchID string) (CacheEntry, bool) {
	e, ok := c.entries[branchID]
	return e, ok
}

func (c *ResultCache) Put(state *BranchState) CacheEntry {
	entry := CacheEntry{BranchID: state.BranchID, ContentHash: HashCanonical(state), State: cloneBranchState(state)}
	c.entries[state.BranchID] = entry
	return entry
}

func (c *ResultCache) Invalidate(branchIDs ...string) {
	for _, id := range branchIDs {
		delete(c.entries, id)
	}
}

func (c *ResultCache) BranchIDs() []string {
	out := make([]string, 0, len(c.entries))
	for id := range c.entries {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (c *ResultCache) Load(entries []CacheEntry) {
	for _, entry := range entries {
		c.entries[entry.BranchID] = entry
	}
}

type BranchCacheBackend interface {
	SaveBranchCache(projectID string, entry CacheEntry) error
	LoadBranchCache(projectID string) ([]CacheEntry, error)
	DeleteBranchCache(projectID, branchID string) error
}

type RecomputeReport struct {
	CommitSequence     []string `json:"commit_sequence"`
	ReusedBranches     []string `json:"reused_branches"`
	RecomputedBranches []string `json:"recomputed_branches"`
}

type IncrementalRunner struct {
	Project *Project
	Events  EventStream
	Config  ExecutorConfig
	Graph   *DependencyGraph
	Cache   *ResultCache
	Backend BranchCacheBackend
}

func NewIncrementalRunner(project *Project, events EventStream, config ExecutorConfig, backend BranchCacheBackend) (*IncrementalRunner, error) {
	runner := &IncrementalRunner{
		Project: project, Events: events, Config: config,
		Graph: BuildDependencyGraph(project), Cache: NewResultCache(), Backend: backend,
	}
	if backend != nil {
		entries, err := backend.LoadBranchCache(project.ID)
		if err != nil {
			return nil, err
		}
		runner.Cache.Load(entries)
	}
	return runner, nil
}

// RunEmpty performs a full run with an empty in-memory cache and persists all
// terminal branch results. It is the reference path for incremental parity.
func (r *IncrementalRunner) RunEmpty() (*RunResult, []string, error) {
	r.Cache = NewResultCache()
	result, err := RunProject(r.Project, r.Events, r.Config)
	if err != nil {
		return result, nil, err
	}
	if err := r.storeResult(result); err != nil {
		return result, nil, err
	}
	return result, CommitIDs(result.Commits), nil
}

func (r *IncrementalRunner) Invalidate(report *InvalidationReport) error {
	if report == nil {
		return nil
	}
	r.Cache.Invalidate(report.BranchIDs...)
	if r.Backend == nil {
		return nil
	}
	for _, id := range report.BranchIDs {
		if err := r.Backend.DeleteBranchCache(r.Project.ID, id); err != nil {
			return err
		}
	}
	return nil
}

// Recompute deterministically reconciles the edited project. Unaffected branch
// results are served from the persistent cache; affected entries are replaced.
func (r *IncrementalRunner) Recompute() (*RunResult, *RecomputeReport, error) {
	result, err := RunProject(r.Project, r.Events, r.Config)
	if err != nil {
		return result, nil, err
	}
	report := &RecomputeReport{CommitSequence: CommitIDs(result.Commits)}
	for _, commit := range result.Commits {
		id := commit.BranchID
		state := result.Branches[id]
		if cached, ok := r.Cache.Get(id); ok {
			actualHash := HashCanonical(state)
			if cached.ContentHash != actualHash {
				return result, report, DiffError(commit.Frame, id, "cached_result_hash", "persistent result differs from deterministic replay")
			}
			report.ReusedBranches = append(report.ReusedBranches, id)
			continue
		}
		entry := r.Cache.Put(state)
		report.RecomputedBranches = append(report.RecomputedBranches, id)
		if r.Backend != nil {
			if err := r.Backend.SaveBranchCache(r.Project.ID, entry); err != nil {
				return result, report, err
			}
		}
	}
	sort.Strings(report.ReusedBranches)
	sort.Strings(report.RecomputedBranches)
	return result, report, nil
}

func (r *IncrementalRunner) storeResult(result *RunResult) error {
	for _, commit := range result.Commits {
		state := result.Branches[commit.BranchID]
		entry := r.Cache.Put(state)
		if r.Backend != nil {
			if err := r.Backend.SaveBranchCache(r.Project.ID, entry); err != nil {
				return err
			}
		}
	}
	return nil
}
