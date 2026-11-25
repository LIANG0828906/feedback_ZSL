package lipsync

import (
	"encoding/json"
	"sort"
	"sync"
)

type CacheEntry struct {
	Key           string   `json:"key"`
	ProjectID     string   `json:"projectId"`
	DialogueID    string   `json:"dialogueId"`
	PhonemeID     string   `json:"phonemeId,omitempty"`
	Kind          string   `json:"kind"`
	ContentHash   string   `json:"contentHash"`
	RecipeVersion string   `json:"recipeVersion"`
	UpstreamKeys  []string `json:"upstreamKeys"`
	PayloadJSON   string   `json:"payloadJson"`
	Revision      int      `json:"revision"`
}

type CacheStats struct {
	Entries    int `json:"entries"`
	Hits       int `json:"hits"`
	Misses     int `json:"misses"`
	Reused     int `json:"reused"`
	Persistent int `json:"persistent"`
}

type Cache struct {
	mu      sync.RWMutex
	entries map[string]CacheEntry
	hits    int
	misses  int
	reused  int
	store   *Store
}

func NewCache(store *Store) *Cache {
	return &Cache{entries: map[string]CacheEntry{}, store: store}
}

func (c *Cache) LoadPersistent(projectID string) error {
	if c.store == nil {
		return nil
	}
	entries, err := c.store.LoadCacheEntries(projectID)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range entries {
		c.entries[entry.Key] = entry
	}
	return nil
}

func (c *Cache) Get(key string) (CacheEntry, bool) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if ok {
		c.mu.Lock()
		c.hits++
		c.mu.Unlock()
		return entry, true
	}
	c.mu.Lock()
	c.misses++
	c.mu.Unlock()
	return CacheEntry{}, false
}

func (c *Cache) Put(entry CacheEntry) error {
	c.mu.Lock()
	c.entries[entry.Key] = entry
	c.mu.Unlock()
	if c.store != nil {
		return c.store.UpsertCacheEntry(entry)
	}
	return nil
}

func (c *Cache) PutMemory(entry CacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[entry.Key] = entry
}

func (c *Cache) EntriesForDialogue(dialogueID string) []CacheEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := []CacheEntry{}
	for _, entry := range c.entries {
		if entry.DialogueID == dialogueID {
			out = append(out, entry)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (c *Cache) Reuse(oldKey, newKey string, revision int) (CacheEntry, bool, error) {
	c.mu.RLock()
	entry, ok := c.entries[oldKey]
	c.mu.RUnlock()
	if !ok {
		return CacheEntry{}, false, nil
	}
	entry.Key = newKey
	entry.Revision = revision
	c.mu.Lock()
	c.entries[newKey] = entry
	delete(c.entries, oldKey)
	c.reused++
	c.mu.Unlock()
	if c.store != nil {
		if err := c.store.UpsertCacheEntry(entry); err != nil {
			return CacheEntry{}, false, err
		}
	}
	return entry, true, nil
}

func (c *Cache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	persistent := 0
	if c.store != nil {
		persistent = len(c.entries)
	}
	return CacheStats{
		Entries:    len(c.entries),
		Hits:       c.hits,
		Misses:     c.misses,
		Reused:     c.reused,
		Persistent: persistent,
	}
}

func MakeCacheEntry(projectID, key, kind string, revision int, dialogue *Dialogue, recipeVersion string, upstream []string, payload any) (CacheEntry, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return CacheEntry{}, err
	}
	entry := CacheEntry{
		Key:           key,
		ProjectID:     projectID,
		Kind:          kind,
		RecipeVersion: recipeVersion,
		UpstreamKeys:  append([]string(nil), upstream...),
		PayloadJSON:   string(payloadBytes),
		Revision:      revision,
	}
	if dialogue != nil {
		entry.DialogueID = dialogue.ID
		entry.ContentHash = dialogue.ContentHash
	}
	return entry, nil
}

func PhonemeKey(dialogueContentHash, phonemeID, recipeVersion, upstreamKey string) string {
	return CacheKey("phoneme-v1", dialogueContentHash, phonemeID, recipeVersion, upstreamKey)
}

func PackKey(dialogueContentHash, packID, recipeVersion string, phonemeIDs, upstreamKeys []string) string {
	parts := []string{"pack-v1", dialogueContentHash, packID, recipeVersion}
	parts = append(parts, phonemeIDs...)
	parts = append(parts, upstreamKeys...)
	return CacheKey(parts...)
}

func DecodeCachePayload[T any](entry CacheEntry) (T, error) {
	var value T
	err := json.Unmarshal([]byte(entry.PayloadJSON), &value)
	return value, err
}
