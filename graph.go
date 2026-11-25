package texture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type Chunk struct {
	X    int  `json:"x"`
	Y    int  `json:"y"`
	Rect Rect `json:"rect"`
}

// BuildChunks returns row-major tiles. Every tile is clipped to the image.
func BuildChunks(width, height, chunkWidth, chunkHeight int) ([]Chunk, error) {
	if width <= 0 || height <= 0 {
		return nil, errf(CodePara, "$.image", "width/height", "must be positive")
	}
	if chunkWidth <= 0 {
		return nil, errf(CodePara, "$.chunk_width", "chunk_width", "must be > 0")
	}
	if chunkHeight <= 0 {
		return nil, errf(CodePara, "$.chunk_height", "chunk_height", "must be > 0")
	}
	tilesX := (width + chunkWidth - 1) / chunkWidth
	tilesY := (height + chunkHeight - 1) / chunkHeight
	chunks := make([]Chunk, 0, tilesX*tilesY)
	for y := 0; y < tilesY; y++ {
		for x := 0; x < tilesX; x++ {
			x0, y0 := x*chunkWidth, y*chunkHeight
			x1, y1 := min(x0+chunkWidth, width), min(y0+chunkHeight, height)
			chunks = append(chunks, Chunk{
				X: x, Y: y,
				Rect: Rect{X: x0, Y: y0, Width: x1 - x0, Height: y1 - y0},
			})
		}
	}
	return chunks, nil
}

type EntryDependency struct {
	From int `json:"from"`
	To   int `json:"to"`
}

type RegionDependency struct {
	EntryIndex int     `json:"entry_index"`
	Rect       Rect    `json:"rect"`
	Chunks     []Chunk `json:"chunks"`
}

type StageGraph struct {
	Entries     []int              `json:"entries"`
	EntryDeps   []EntryDependency  `json:"entry_dependencies"`
	RegionDeps  []RegionDependency `json:"region_dependencies"`
	ChunkWidth  int                `json:"chunk_width"`
	ChunkHeight int                `json:"chunk_height"`
}

func BuildStageGraph(p *Plan, chunkWidth, chunkHeight int) (*StageGraph, error) {
	if p == nil {
		return nil, errf(CodeSchema, "$", "plan", "plan is nil")
	}
	chunks, err := BuildChunks(p.Source.Width, p.Source.Height, chunkWidth, chunkHeight)
	if err != nil {
		return nil, err
	}
	graph := &StageGraph{ChunkWidth: chunkWidth, ChunkHeight: chunkHeight}
	for i, entry := range p.Entries {
		graph.Entries = append(graph.Entries, entry.Index)
		rd := RegionDependency{EntryIndex: entry.Index, Rect: entry.Coverage}
		for _, ch := range chunks {
			if ch.Rect.Intersects(entry.Coverage) {
				rd.Chunks = append(rd.Chunks, ch)
			}
		}
		graph.RegionDeps = append(graph.RegionDeps, rd)
		for j := 0; j < i; j++ {
			if p.Entries[j].Coverage.Intersects(entry.Coverage) {
				graph.EntryDeps = append(graph.EntryDeps, EntryDependency{From: p.Entries[j].Index, To: entry.Index})
			}
		}
	}
	return graph, nil
}

// EntryParameterHash is the canonical parameter value used in cache keys.
// Provenance traces are intentionally excluded.
func EntryParameterHash(e Entry) string {
	sum := sha256.Sum256([]byte(EntryParameterSnapshot(e)))
	return hex.EncodeToString(sum[:])
}

// EntryParameterSnapshot is the frozen, human-auditable parameter value stored
// with every committed block.
func EntryParameterSnapshot(e Entry) string {
	type params struct {
		Index          int              `json:"index"`
		Coverage       Rect             `json:"coverage"`
		TargetBitDepth int              `json:"target_bit_depth"`
		TargetChannels []Channel        `json:"target_channels"`
		Mappings       []ChannelMapping `json:"mappings"`
		Rounding       Rounding         `json:"rounding"`
		Alpha          AlphaMode        `json:"alpha"`
	}
	b, _ := json.Marshal(params{
		Index: e.Index, Coverage: e.Coverage, TargetBitDepth: e.TargetBitDepth,
		TargetChannels: e.TargetChannels, Mappings: e.Mappings,
		Rounding: e.Rounding, Alpha: e.Alpha,
	})
	return string(b)
}

// EntryParameterHashForTile returns the effective cache parameter identity for
// one tile. A partial entry has no effect outside its actual coverage, so those
// tiles use a stable no-op identity; this is what makes an edit invalidate only
// the covered tiles rather than every tile in the image.
func EntryParameterHashForTile(e Entry, tile Rect) string {
	if tile.Intersects(e.Coverage) {
		return EntryParameterHash(e)
	}
	payload := struct {
		Index int  `json:"index"`
		NoOp  bool `json:"noop"`
	}{Index: e.Index, NoOp: true}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// BuildCacheKey binds a stage block to its actual input region, ordinal,
// canonical parameters, and the complete upstream cache key chain.
func BuildCacheKey(inputRegionHash string, entryIndex int, parameterHash, upstreamKey string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d:%s:%d:%s:%s", len(inputRegionHash), inputRegionHash, entryIndex, parameterHash, upstreamKey)
	return hex.EncodeToString(h.Sum(nil))
}

type PlanChange struct {
	EntryIndex int      `json:"entry_index"`
	Fields     []string `json:"fields"`
}

type Invalidation struct {
	EntryIndex int    `json:"entry_index"`
	ChunkX     int    `json:"chunk_x"`
	ChunkY     int    `json:"chunk_y"`
	Reason     string `json:"reason"`
}

// Invalidate compares two plan versions and marks only downstream entry/tile
// pairs whose dependency cone intersects the edited entry's actual coverage.
func Invalidate(oldPlan, newPlan *Plan, chunkWidth, chunkHeight int) ([]Invalidation, error) {
	if oldPlan == nil || newPlan == nil {
		return nil, errf(CodeSchema, "$", "plan", "old and new plans are required")
	}
	if !sourceEqual(oldPlan.Source, newPlan.Source) {
		return nil, errf(CodePara, "$.source", "source", "source dimensions may not change during an edit")
	}
	if len(oldPlan.Entries) != len(newPlan.Entries) {
		return nil, errf(CodeSchema, "$.entries", "entries", "entry count changed; create a new plan version")
	}
	chunks, err := BuildChunks(newPlan.Source.Width, newPlan.Source.Height, chunkWidth, chunkHeight)
	if err != nil {
		return nil, err
	}
	reasonsByPair := make(map[string]map[string]bool)
	for i := range oldPlan.Entries {
		oldEntry, newEntry := oldPlan.Entries[i], newPlan.Entries[i]
		fields := changedFields(oldEntry, newEntry)
		if len(fields) == 0 {
			continue
		}
		rootRegions := []Rect{newEntry.Coverage}
		if oldEntry.Coverage != newEntry.Coverage {
			rootRegions = append(rootRegions, oldEntry.Coverage)
		}
		for j := i; j < len(newPlan.Entries); j++ {
			// A downstream stage only depends on the edit when its actual region
			// overlaps the edited region. Subsequent full-frame stages naturally
			// overlap and remain in the dependency cone.
			overlapsRoot := false
			for _, root := range rootRegions {
				if newPlan.Entries[j].Coverage.Intersects(root) {
					overlapsRoot = true
					break
				}
			}
			if j > i && !overlapsRoot {
				continue
			}
			covered := newPlan.Entries[j].Coverage
			for _, chunk := range chunks {
				if !chunk.Rect.Intersects(covered) {
					continue
				}
				inRoot := false
				for _, root := range rootRegions {
					if chunk.Rect.Intersects(root) {
						inRoot = true
						break
					}
				}
				if !inRoot {
					continue
				}
				key := fmt.Sprintf("%d/%d/%d", j, chunk.X, chunk.Y)
				if reasonsByPair[key] == nil {
					reasonsByPair[key] = make(map[string]bool)
				}
				for _, field := range fields {
					reasonsByPair[key][fmt.Sprintf("entry %d changed %s", i, field)] = true
				}
			}
		}
	}
	out := make([]Invalidation, 0, len(reasonsByPair))
	for key, reasons := range reasonsByPair {
		var entry, cx, cy int
		if _, err := fmt.Sscanf(key, "%d/%d/%d", &entry, &cx, &cy); err != nil {
			return nil, errf(CodeStore, "$", "invalidation", "internal key parse failure: %v", err)
		}
		list := make([]string, 0, len(reasons))
		for reason := range reasons {
			list = append(list, reason)
		}
		sort.Strings(list)
		out = append(out, Invalidation{EntryIndex: entry, ChunkX: cx, ChunkY: cy, Reason: joinReasons(list)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EntryIndex != out[j].EntryIndex {
			return out[i].EntryIndex < out[j].EntryIndex
		}
		if out[i].ChunkY != out[j].ChunkY {
			return out[i].ChunkY < out[j].ChunkY
		}
		return out[i].ChunkX < out[j].ChunkX
	})
	return out, nil
}

func changedFields(a, b Entry) []string {
	fields := make([]string, 0, 6)
	if !channelsEqual(a.TargetChannels, b.TargetChannels) || !mappingsEqual(a.Mappings, b.Mappings) {
		fields = append(fields, "channel_permutation")
	}
	if a.TargetBitDepth != b.TargetBitDepth {
		fields = append(fields, "target_bit_depth")
	}
	if fillsDiffer(a.Mappings, b.Mappings) {
		fields = append(fields, "fill")
	}
	if a.Alpha != b.Alpha {
		fields = append(fields, "alpha")
	}
	if a.Rounding != b.Rounding {
		fields = append(fields, "rounding")
	}
	if a.Coverage != b.Coverage {
		fields = append(fields, "coverage")
	}
	return fields
}

func mappingsEqual(a, b []ChannelMapping) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Target != b[i].Target || a[i].Source != b[i].Source || (a[i].Fill == nil) != (b[i].Fill == nil) {
			return false
		}
		if a[i].Fill != nil && canonicalFill(*a[i].Fill) != canonicalFill(*b[i].Fill) {
			return false
		}
	}
	return true
}

func fillsDiffer(a, b []ChannelMapping) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if (a[i].Fill == nil) != (b[i].Fill == nil) {
			return true
		}
		if a[i].Fill != nil && canonicalFill(*a[i].Fill) != canonicalFill(*b[i].Fill) {
			return true
		}
	}
	return false
}

func joinReasons(reasons []string) string {
	out := ""
	for i, reason := range reasons {
		if i > 0 {
			out += "; "
		}
		out += reason
	}
	return out
}
