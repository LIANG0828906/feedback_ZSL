// Package resultcache persists immutable envelope and calibration results by
// graph cache key. It supports reuse of unaffected clips after an edit.
package resultcache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"dialogueaudio/engine"
)

// Cache stores one JSON file per full cache key.
type Cache struct {
	dir string
	mu  sync.Mutex
}

// Open prepares a cache directory.
func Open(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Cache{dir: dir}, nil
}

// Dir returns the cache directory.
func (c *Cache) Dir() string { return c.dir }

// GetEnvelope reads a persisted envelope.
func (c *Cache) GetEnvelope(key string) (engine.ClipEnvelope, bool) {
	var env engine.ClipEnvelope
	if !c.read("env_"+key+".json", &env) {
		return engine.ClipEnvelope{}, false
	}
	return env, true
}

// PutEnvelope atomically stores an envelope.
func (c *Cache) PutEnvelope(key string, env engine.ClipEnvelope) error {
	return c.write("env_"+key+".json", env)
}

// GetCalibration reads a persisted calibration result.
func (c *Cache) GetCalibration(key string) (engine.ClipCalibration, bool) {
	var calibration engine.ClipCalibration
	if !c.read("cal_"+key+".json", &calibration) {
		return engine.ClipCalibration{}, false
	}
	return calibration, true
}

// PutCalibration atomically stores a calibration result.
func (c *Cache) PutCalibration(key string, calibration engine.ClipCalibration) error {
	return c.write("cal_"+key+".json", calibration)
}

func (c *Cache) read(name string, out any) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(c.dir, name))
	if err != nil {
		return false
	}
	return json.Unmarshal(data, out) == nil
}

func (c *Cache) write(name string, value any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	target := filepath.Join(c.dir, name)
	temp := target + ".tmp"
	if err := os.WriteFile(temp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(temp, target); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

// Remove deletes a cache entry. A missing entry is not an error.
func (c *Cache) Remove(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, prefix := range []string{"env_", "cal_"} {
		err := os.Remove(filepath.Join(c.dir, prefix+key+".json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
