package bake

import (
	"sort"
	"sync"
)

// Cache is a concurrency-safe in-memory unit cache keyed by chunk/time.
type Cache struct {
	mu    sync.RWMutex
	units map[string]*BakedUnit
}

// NewCache creates an empty cache.
func NewCache() *Cache {
	return &Cache{units: make(map[string]*BakedUnit)}
}

// Get returns a cached unit.
func (c *Cache) Get(chunkID string, time int) (*BakedUnit, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	u, ok := c.units[UnitID(chunkID, time)]
	return u, ok
}

// Put atomically replaces one unit.
func (c *Cache) Put(unit *BakedUnit) {
	if unit == nil {
		return
	}
	c.mu.Lock()
	c.units[UnitID(unit.ChunkID, unit.Time)] = unit
	c.mu.Unlock()
}

// Delete removes one unit.
func (c *Cache) Delete(chunkID string, time int) {
	c.mu.Lock()
	delete(c.units, UnitID(chunkID, time))
	c.mu.Unlock()
}

// DeleteBatch removes a set of units under one cache lock.
func (c *Cache) DeleteBatch(refs []UnitRef) {
	c.mu.Lock()
	for _, ref := range refs {
		delete(c.units, UnitID(ref.ChunkID, ref.Time))
	}
	c.mu.Unlock()
}

// Len returns the cached unit count.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.units)
}

// Snapshot returns a stable-time/chunk ordered copy of cached unit pointers.
func (c *Cache) Snapshot() []*BakedUnit {
	c.mu.RLock()
	out := make([]*BakedUnit, 0, len(c.units))
	for _, u := range c.units {
		out = append(out, u)
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time != out[j].Time {
			return out[i].Time < out[j].Time
		}
		return out[i].ChunkID < out[j].ChunkID
	})
	return out
}

// UnitsByRef returns all cached units in canonical order.
func (c *Cache) UnitsByRef(refs []UnitRef) []*BakedUnit {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*BakedUnit, 0, len(refs))
	for _, ref := range refs {
		if u, ok := c.units[UnitID(ref.ChunkID, ref.Time)]; ok {
			out = append(out, u)
		}
	}
	return out
}
