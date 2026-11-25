package pipeline

import (
	"strconv"
	"sync"

	"qualitybench/internal/checkpoint"
	"qualitybench/internal/core"
)

type BlockValue struct {
	Pixels     []byte
	CacheKey   string
	InputHash  string
	OutputHash string
}

type MemoryCache struct {
	mu      sync.RWMutex
	entries map[string]BlockValue
	hits    int64
	misses  int64
}

func NewMemoryCache() *MemoryCache {
	return &MemoryCache{entries: make(map[string]BlockValue)}
}

func (c *MemoryCache) Get(key string) (BlockValue, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[key]
	if ok {
		c.hits++
	} else {
		c.misses++
	}
	return v, ok
}

func (c *MemoryCache) Put(key string, v BlockValue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]byte, len(v.Pixels))
	copy(cp, v.Pixels)
	v.Pixels = cp
	c.entries[key] = v
}

func (c *MemoryCache) LoadArtifact(a checkpoint.Artifact) {
	if a.CacheKey == "" || len(a.Pixels) == 0 {
		return
	}
	c.Put(a.CacheKey, BlockValue{
		Pixels: a.Pixels, CacheKey: a.CacheKey, InputHash: a.InputHash, OutputHash: a.OutputHash,
	})
}

func (c *MemoryCache) Stats() (hits, misses int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hits, c.misses
}

func CacheKey(algorithmVersion string, stepIndex int, inputHash, params, upstreamKey string) string {
	step := core.HashParts("step-index", strconv.Itoa(stepIndex))
	return core.HashParts("qb-cache-v1", algorithmVersion, step, inputHash, params, upstreamKey)
}

func RootCacheKey(baselineBlockHash string) string {
	return core.HashParts("qb-root-v1", baselineBlockHash)
}
