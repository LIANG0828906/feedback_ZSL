package engine

import (
	"sort"
	"sync"

	"graphlab/internal/model"
)

type Runtime struct {
	Project   *model.Project
	Options   BuildOptions
	Index     *IndexStore
	Cache     *CacheStore
	CommitSeq int
	Metrics   []ShardMetric
}

func NewRuntime(project *model.Project, opts BuildOptions) (*Runtime, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	p := model.CloneProject(project)
	queryIDs := make(map[string]bool, len(p.Queries))
	for _, q := range p.Queries {
		if q.ID == "" {
			return nil, model.Err(model.EInvalidQuery, "query id is empty", map[string]any{})
		}
		if queryIDs[q.ID] {
			return nil, model.Err(model.EDuplicateID, "duplicate query id", map[string]any{"query_id": q.ID})
		}
		queryIDs[q.ID] = true
	}
	idx, metrics, err := BuildIndex(p, opts)
	if err != nil {
		return nil, err
	}
	model.SortProject(p)
	return &Runtime{
		Project: p, Options: opts, Index: idx, Cache: NewCacheStore(),
		Metrics: metrics,
	}, nil
}

func (r *Runtime) EvaluateAll(layout string) ([]model.QueryResult, error) {
	if err := r.Options.Validate(); err != nil {
		return nil, err
	}
	if layout != LayoutIndex && layout != LayoutScan {
		layout = LayoutIndex
	}
	queries := append([]model.Query{}, r.Project.Queries...)
	sort.Slice(queries, func(i, j int) bool { return queries[i].ID < queries[j].ID })

	for _, entry := range r.Cache.Entries {
		entry.Reused = false
	}
	type miss struct {
		index int
		query model.Query
	}
	results := make([]model.QueryResult, len(queries))
	var misses []miss
	for i, q := range queries {
		if entry := r.Cache.Entries[q.CacheID()]; entry != nil {
			cp := entry.Result
			cp.ProjectVersion = r.Project.Version
			results[i] = cp
			entry.Reused = true
			entry.Hits++
			continue
		}
		misses = append(misses, miss{index: i, query: q})
	}
	type evalOut struct {
		result model.QueryResult
		err    error
	}
	outs := make([]evalOut, len(misses))
	jobs := make(chan int)
	var wg sync.WaitGroup
	workers := r.Options.Workers
	if workers > len(misses) {
		workers = len(misses)
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				res := evaluateQuery(r.Project, r.Index, misses[i].query, layout)
				workingSet := len(res.VisitedVertices) + len(res.VisitedEdges) +
					len(res.IndexKeys) + len(res.Dependencies)
				if workingSet > r.Options.BufferPerWorker {
					outs[i].err = model.Err(model.EBufferBudget,
						"per-worker traversal buffer exhausted", map[string]any{
							"query_id": misses[i].query.ID, "limit": r.Options.BufferPerWorker,
							"observed_working_set": workingSet,
						})
					continue
				}
				outs[i].result = res
			}
		}()
	}
	for i := range misses {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for i := range outs {
		if outs[i].err != nil {
			return nil, outs[i].err
		}
	}
	for i, m := range misses {
		entry := &CacheEntry{
			CacheID:             m.query.CacheID(),
			QueryKey:            m.query.Canonical(),
			QueryID:             m.query.ID,
			Result:              outs[i].result,
			Dependencies:        append([]string(nil), outs[i].result.Dependencies...),
			BuiltProjectVersion: r.Project.Version,
		}
		r.Cache.Entries[entry.CacheID] = entry
		results[m.index] = outs[i].result
	}
	return results, nil
}

func (r *Runtime) FullRebuild(layout string) (*Runtime, []model.QueryResult, error) {
	fresh, err := NewRuntime(r.Project, r.Options)
	if err != nil {
		return nil, nil, err
	}
	results, err := fresh.EvaluateAll(layout)
	if err != nil {
		return nil, nil, err
	}
	return fresh, results, nil
}

func (r *Runtime) KeyViewsWithCaches() []IndexKeyView {
	views := r.Index.KeyViews()
	cacheEntries := r.Cache.EntriesSorted()
	for i := range views {
		for _, entry := range cacheEntries {
			for _, dep := range entry.Dependencies {
				if dep == views[i].Key {
					views[i].CacheIDs = append(views[i].CacheIDs, entry.CacheID)
					break
				}
			}
		}
	}
	return views
}

func (r *Runtime) ShardViews(ids map[string]bool) []ShardIndex {
	if ids == nil {
		ids = make(map[string]bool)
	}
	if len(ids) == 0 {
		for id := range r.Index.Shards {
			ids[id] = true
		}
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	out := make([]ShardIndex, 0, len(sorted))
	for _, id := range sorted {
		si := r.Index.Shards[id]
		if si == nil {
			continue
		}
		cp := &ShardIndex{
			ID: si.ID, Version: si.Version,
			VertexOrder:   append([]string(nil), si.VertexOrder...),
			EdgeOrder:     append([]string(nil), si.EdgeOrder...),
			Contributions: make(map[string][]string, len(si.Contributions)),
		}
		for key, ents := range si.Contributions {
			cp.Contributions[key] = append([]string(nil), ents...)
		}
		out = append(out, *cp)
	}
	return out
}

func CompareQueryResults(a, b []model.QueryResult) []string {
	var diffs []string
	if len(a) != len(b) {
		return []string{"result count differs"}
	}
	for i := range a {
		if a[i].QueryID != b[i].QueryID {
			diffs = append(diffs, "query id order differs")
			continue
		}
		if a[i].Status != b[i].Status ||
			!sameSlice(a[i].VisitedVertices, b[i].VisitedVertices) ||
			!sameSlice(a[i].VisitedEdges, b[i].VisitedEdges) ||
			!sameSlice(a[i].ResultVertices, b[i].ResultVertices) ||
			!sameSlice(a[i].PathVertices, b[i].PathVertices) ||
			!sameSlice(a[i].PathEdges, b[i].PathEdges) ||
			a[i].CacheID != b[i].CacheID ||
			a[i].ProjectVersion != b[i].ProjectVersion {
			diffs = append(diffs, "query result differs: "+a[i].QueryID)
		}
	}
	return diffs
}

func sameSlice(a, b []string) bool {
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
