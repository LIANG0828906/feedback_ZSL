// Package cache persists reusable batch results and cache keys.
package cache

import (
	"encoding/json"
	"os"

	"wardrobekernel/internal/atomicfile"
	"wardrobekernel/internal/model"
)

// Entry is one atomically replaceable batch cache value.
type Entry struct {
	BatchNumber int         `json:"batch_number"`
	CacheKey    string      `json:"cache_key"`
	Batch       model.Batch `json:"batch"`
}

// PersistentCache is the JSON cache snapshot. Writes are atomic via rename.
type PersistentCache struct {
	Path              string        `json:"-"`
	AlgorithmVersion  string        `json:"algorithm_version"`
	ConstraintVersion string        `json:"constraint_version"`
	Entries           map[int]Entry `json:"entries"`
}

// Load reads a cache or returns an empty one when the file does not exist.
func Load(path, algorithmVersion, constraintVersion string) (*PersistentCache, error) {
	c := &PersistentCache{
		Path: path, AlgorithmVersion: algorithmVersion, ConstraintVersion: constraintVersion,
		Entries: map[int]Entry{},
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, err
	}
	c.Path = path
	return c, nil
}

// Put atomically stages/replaces one batch entry in the snapshot.
func (c *PersistentCache) Put(batch model.Batch) error {
	if c.Entries == nil {
		c.Entries = map[int]Entry{}
	}
	c.Entries[batch.Number] = Entry{BatchNumber: batch.Number, CacheKey: batch.CacheKey, Batch: batch}
	return c.Save()
}

// Save atomically persists the complete cache snapshot.
func (c *PersistentCache) Save() error {
	return atomicfile.WriteJSON(c.Path, c)
}

// Batch returns a persisted batch.
func (c *PersistentCache) Batch(number int) (model.Batch, bool) {
	entry, ok := c.Entries[number]
	return entry.Batch, ok
}
