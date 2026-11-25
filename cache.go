package maskqc

import (
	"encoding/hex"
	"hash/fnv"
	"sort"
	"strings"
)

type CacheKeyInput struct {
	TileID           string
	TileInputHash    string
	StrokeID         string
	OrderVersion     string
	ParamVersion     string
	UpstreamKey      string
	AlgorithmVersion string
}

func ComponentCacheKey(in CacheKeyInput) string {
	parts := []string{
		CacheVersion,
		AlgorithmVersion,
		in.TileID,
		in.TileInputHash,
		in.StrokeID,
		OrderVersion,
		ParamVersion,
		in.UpstreamKey,
	}
	return sha256Sum([]byte(strings.Join(parts, "|")))
}

func HashBytes(b []byte) string {
	return sha256Sum(b)
}

func HashMaskRegion(m *Mask, r BBox) string {
	h := fnv.New64a()
	if m == nil {
		return "nil"
	}
	for y := r.MinY; y <= r.MaxY; y++ {
		for x := r.MinX; x <= r.MaxX; x++ {
			_, _ = h.Write([]byte{m.Get(x, y)})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func SortedStrokeIDs(strokes []Stroke) []string {
	out := make([]string, 0, len(strokes))
	for _, s := range strokes {
		out = append(out, s.ID)
	}
	sort.Strings(out)
	return out
}
