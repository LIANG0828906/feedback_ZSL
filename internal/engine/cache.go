package engine

import (
	"fmt"

	"mirrorcast/internal/session"
)

type MappingCache struct {
	entries map[string]session.Mapping
	Hits    int64
	Misses  int64
}

func NewMappingCache() *MappingCache {
	return &MappingCache{entries: map[string]session.Mapping{}}
}

func MappingKey(master session.Rect, width, height, scale int64, chroma session.ChromaFormat) string {
	return fmt.Sprintf("%d,%d,%d,%d|%dx%d|s%d|%s", master.X, master.Y, master.Width, master.Height, width, height, scale, chroma)
}

func (c *MappingCache) Peek(key string) (session.Mapping, bool) {
	if c == nil {
		return session.Mapping{}, false
	}
	mapping, ok := c.entries[key]
	return mapping, ok
}

func (e *Engine) mapRegion(master session.Rect, width, height, scale int64, chroma string) (session.Mapping, error) {
	format := session.ChromaFormat(chroma)
	key := MappingKey(master, width, height, scale, format)
	if e.cache != nil {
		if mapping, ok := e.cache.entries[key]; ok {
			e.cache.Hits++
			return mapping, nil
		}
	}
	mapping, err := session.MapRegion(master, width, height, scale, chroma)
	if err != nil {
		return session.Mapping{}, err
	}
	if e.cache != nil {
		e.cache.Misses++
		e.cache.entries[key] = mapping
	}
	return mapping, nil
}

func (c *MappingCache) Keys() map[string]struct{} {
	out := make(map[string]struct{}, len(c.entries))
	for key := range c.entries {
		out[key] = struct{}{}
	}
	return out
}
