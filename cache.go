package vectorcore

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

type Dependency struct {
	Key         string `json:"key"`
	Stage       Stage  `json:"stage"`
	PathID      string `json:"pathId"`
	TimeIndex   int    `json:"timeIndex"`
	Layer       int    `json:"layer"`
	UpstreamKey string `json:"upstreamKey,omitempty"`
	Keyframes   []int  `json:"keyframes,omitempty"`
}

type CacheStats struct {
	Hits   uint64 `json:"hits"`
	Misses uint64 `json:"misses"`
}

type cacheEntry struct {
	key       string
	stage     Stage
	pathID    string
	timeIndex int
	layer     int
	inputHash string
	paramHash string
	upstream  string
	deps      []Dependency
	geometry  *pathGeometry
	coverage  *Coverage
	composite *Bitmap
}

type stageCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry
	hits    atomic.Uint64
	misses  atomic.Uint64
}

func newStageCache() *stageCache {
	return &stageCache{entries: make(map[string]*cacheEntry)}
}

func (c *stageCache) get(key string) (*cacheEntry, bool) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if ok {
		c.hits.Add(1)
	} else {
		c.misses.Add(1)
	}
	return entry, ok
}

func (c *stageCache) put(entry *cacheEntry) {
	c.mu.Lock()
	c.entries[entry.key] = entry
	c.mu.Unlock()
}

func (c *stageCache) removeMatching(match func(*cacheEntry) bool) []*cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	var removed []*cacheEntry
	for key, entry := range c.entries {
		if match(entry) {
			removed = append(removed, entry)
			delete(c.entries, key)
		}
	}
	return removed
}

func (c *stageCache) stats() CacheStats {
	return CacheStats{Hits: c.hits.Load(), Misses: c.misses.Load()}
}

func (c *stageCache) dependencies() []Dependency {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []Dependency
	for _, entry := range c.entries {
		out = append(out, entry.deps...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TimeIndex != out[j].TimeIndex {
			return out[i].TimeIndex < out[j].TimeIndex
		}
		if out[i].PathID != out[j].PathID {
			return out[i].PathID < out[j].PathID
		}
		return out[i].Stage < out[j].Stage
	})
	return out
}

func stageCacheKey(inputHash, pathID string, timeIndex int, stage Stage, paramHash, upstream string) string {
	return HashValue(struct {
		Algorithm string
		Input     string
		PathID    string
		TimeIndex int
		Stage     Stage
		Params    string
		Upstream  string
	}{AlgorithmVersion, inputHash, pathID, timeIndex, stage, paramHash, upstream})
}

func newCacheEntry(key string, stage Stage, pathID string, timeIndex, layer int, inputHash, paramHash, upstream string) *cacheEntry {
	return &cacheEntry{key: key, stage: stage, pathID: pathID, timeIndex: timeIndex, layer: layer,
		inputHash: inputHash, paramHash: paramHash, upstream: upstream}
}

func (e *cacheEntry) addDependency(stage Stage, upstream string, keyframes []int) {
	e.deps = append(e.deps, Dependency{Key: e.key, Stage: stage, PathID: e.pathID, TimeIndex: e.timeIndex,
		Layer: e.layer, UpstreamKey: upstream, Keyframes: append([]int(nil), keyframes...)})
}

// Invalidation reports one affected cache boundary. The list is intentionally
// more precise than a global dirty flag.
type Invalidation struct {
	PathID    string `json:"pathId"`
	TimeIndex int    `json:"timeIndex"`
	Stage     Stage  `json:"stage"`
	Reason    string `json:"reason"`
}

func invalidationKey(v Invalidation) string {
	return fmt.Sprintf("%s|%d|%s", v.PathID, v.TimeIndex, v.Stage)
}
