package pipeline

import (
	"sync"

	"collectionrecon/internal/model"
)

// MemoryCache is an in-process Cache used by the example and tests. It can be
// pre-seeded from SQLite checkpoints and keeps writes deterministic.
type MemoryCache struct {
	mu      sync.Mutex
	entries map[int]model.ZoneResult
}

// NewMemoryCache creates an empty cache.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{entries: make(map[int]model.ZoneResult)}
}

// Seed loads zone results into the cache (e.g. after checkpoint recovery).
func (c *MemoryCache) Seed(results []model.ZoneResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range results {
		c.entries[r.ZoneID] = r
	}
}

// Get returns a cached zone result.
func (c *MemoryCache) Get(zoneID int) (model.ZoneResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.entries[zoneID]
	return r, ok
}

// Put stores a zone result with its stable sequence position.
func (c *MemoryCache) Put(_ int, result model.ZoneResult) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[result.ZoneID] = result
	return nil
}

// Snapshot returns cached results in ascending zone order.
func (c *MemoryCache) Snapshot() []model.ZoneResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := make([]int, 0, len(c.entries))
	for id := range c.entries {
		ids = append(ids, id)
	}
	sortInts(ids)
	out := make([]model.ZoneResult, 0, len(ids))
	for _, id := range ids {
		out = append(out, c.entries[id])
	}
	return out
}

func sortInts(xs []int) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}
