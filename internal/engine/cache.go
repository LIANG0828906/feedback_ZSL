package engine

import (
	"sort"

	"graphlab/internal/model"
)

type CacheEntry struct {
	CacheID             string            `json:"cache_id"`
	QueryKey            string            `json:"query_key"`
	QueryID             string            `json:"query_id"`
	Result              model.QueryResult `json:"result"`
	Dependencies        []string          `json:"dependencies"`
	BuiltProjectVersion int               `json:"built_project_version"`
	Reused              bool              `json:"reused"`
	Hits                int               `json:"hits"`
}

type CacheStore struct {
	Entries map[string]*CacheEntry
}

func NewCacheStore() *CacheStore {
	return &CacheStore{Entries: make(map[string]*CacheEntry)}
}

func (c *CacheStore) Clone() *CacheStore {
	out := NewCacheStore()
	for id, entry := range c.Entries {
		cp := *entry
		cp.Dependencies = append([]string(nil), entry.Dependencies...)
		out.Entries[id] = &cp
	}
	return out
}

func (c *CacheStore) EntriesSorted() []CacheEntry {
	ids := make([]string, 0, len(c.Entries))
	for id := range c.Entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]CacheEntry, 0, len(ids))
	for _, id := range ids {
		entry := *c.Entries[id]
		entry.Dependencies = append([]string(nil), entry.Dependencies...)
		out = append(out, entry)
	}
	return out
}

func depsIntersect(deps []string, affected map[string]bool) (bool, []string) {
	var reasons []string
	for _, dep := range deps {
		if affected[dep] {
			reasons = append(reasons, dep)
		}
	}
	if len(reasons) == 0 {
		return false, nil
	}
	sort.Strings(reasons)
	return true, reasons
}
