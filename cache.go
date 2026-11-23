package ortho

import (
	"image"
	"sync"
)

// CacheEntry 为单阶段的已提交缓存项。
type CacheEntry struct {
	Key         string
	Img         image.Image
	ContentHash string
}

// Cache 保存各阶段输出。提交串行化，提交顺序稳定记录。
type Cache struct {
	mu          sync.Mutex
	entries     map[string]*CacheEntry
	invalid     map[string]bool
	commitOrder []string
}

func NewCache() *Cache {
	return &Cache{
		entries: make(map[string]*CacheEntry),
		invalid: make(map[string]bool),
	}
}

// Get 读取阶段缓存项。
func (c *Cache) Get(stageID string) (*CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[stageID]
	return e, ok
}

// MarkInvalid 将阶段标记为失效，允许后续提交替换。
func (c *Cache) MarkInvalid(stageID string) {
	c.mu.Lock()
	c.invalid[stageID] = true
	c.mu.Unlock()
}

// Commit 原子替换阶段缓存项。已提交且未被显式失效的项
// 不得被静默改写，违反时返回 E_STATE。
func (c *Cache) Commit(stageID string, e *CacheEntry) *Error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[stageID]; ok && !c.invalid[stageID] {
		short := old.Key
		if len(short) > 12 {
			short = short[:12] + "..."
		}
		return &Error{Code: EState, Stage: stageID,
			Msg: "refusing to silently overwrite a committed stage result (existing key " + short + ")"}
	}
	delete(c.invalid, stageID)
	c.entries[stageID] = e
	c.commitOrder = append(c.commitOrder, stageID)
	return nil
}

// CommitOrder 返回提交顺序的副本。
func (c *Cache) CommitOrder() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.commitOrder))
	copy(out, c.commitOrder)
	return out
}

// StageIDs 返回当前已提交的阶段标识。
func (c *Cache) StageIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.entries))
	for id := range c.entries {
		out = append(out, id)
	}
	return out
}
