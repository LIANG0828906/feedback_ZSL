package engine

import (
	"sync"

	"dialoguefit/model"
)

type Cache struct {
	mu    sync.RWMutex
	items map[string]model.PhonemeResult
}

func NewCache() *Cache {
	return &Cache{items: map[string]model.PhonemeResult{}}
}

func (c *Cache) Get(key string) (model.PhonemeResult, bool) {
	if c == nil {
		return model.PhonemeResult{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.items[key]
	return v, ok
}

// Put atomically replaces one phoneme node result. Callers never observe a
// partially constructed value because PhonemeResult is fully built first.
func (c *Cache) Put(result model.PhonemeResult) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[result.CacheKey] = result
}

func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
