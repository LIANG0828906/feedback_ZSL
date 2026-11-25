package textureconv

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

type StageNode struct {
	Entry          int      `json:"entry"`
	DependsOn      []int    `json:"dependsOn"`
	Coverage       Rect     `json:"coverage"`
	Tiles          []int    `json:"tiles"`
	InputDepth     BitDepth `json:"inputDepth"`
	InputChannels  int      `json:"inputChannels"`
	TargetDepth    BitDepth `json:"targetDepth"`
	TargetChannels int      `json:"targetChannels"`
	AlphaIndex     int      `json:"alphaIndex"`
}

// DependencyGraph makes stage order and per-stage pixel coverage explicit.
type DependencyGraph struct {
	Width     int         `json:"width"`
	Height    int         `json:"height"`
	TileSize  int         `json:"tileSize"`
	TileCount int         `json:"tileCount"`
	Tiles     []Tile      `json:"tiles"`
	Nodes     []StageNode `json:"nodes"`
}

func BuildDependencyGraph(p *Plan, im *Image, tileSize int) (*DependencyGraph, error) {
	if err := p.ValidateForImage(im); err != nil {
		return nil, err
	}
	tiles, err := MakeTiles(im.Width, im.Height, tileSize)
	if err != nil {
		return nil, err
	}
	g := &DependencyGraph{
		Width: im.Width, Height: im.Height, TileSize: tileSize,
		TileCount: len(tiles), Tiles: tiles,
	}
	depth := im.BitDepth
	channels := 4
	alpha := im.AlphaIndex()
	for i, e := range p.Entries {
		coverage := Rect{W: im.Width, H: im.Height}
		if e.Region != nil {
			coverage = *e.Region
		}
		node := StageNode{
			Entry: e.Seq, Coverage: coverage, Tiles: TilesForRect(tiles, coverage),
			InputDepth: depth, InputChannels: channels,
			TargetDepth: e.TargetDepth, TargetChannels: e.TargetChannels, AlphaIndex: -1,
		}
		if i > 0 {
			node.DependsOn = []int{p.Entries[i-1].Seq}
		}
		for dst, src := range e.Permutation {
			if src == alpha && alpha >= 0 {
				node.AlphaIndex = dst
			}
		}
		g.Nodes = append(g.Nodes, node)
		depth = e.TargetDepth
		channels = e.TargetChannels
		alpha = node.AlphaIndex
	}
	return g, nil
}

// CacheKey is the deterministic stage/tile cache identity. InputHash is the
// content hash of the source or upstream tile; UpstreamKey binds the stage to
// the exact upstream cache entry.
func CacheKey(entry, tile int, paramsHash, inputHash, upstreamKey string) string {
	payload := fmt.Sprintf("%s|entry=%d|tile=%d|params=%s|input=%s|upstream=%s",
		AlgorithmVersion, entry, tile, paramsHash, inputHash, upstreamKey)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

type Invalidation struct {
	Entry  int      `json:"entry"`
	Tiles  []int    `json:"tiles"`
	Region Rect     `json:"region"`
	Reason string   `json:"reason"`
	Fields []string `json:"fields,omitempty"`
}

type InvalidationManifest struct {
	FirstChangedPosition int            `json:"firstChangedPosition"`
	Affected             []Invalidation `json:"affected"`
}

// ComputeInvalidation compares two plans and reports only stages at or after
// the first changed stage and only tiles intersecting their actual coverage.
func ComputeInvalidation(oldPlan, newPlan *Plan, im *Image, tileSize int) (*InvalidationManifest, error) {
	if err := oldPlan.ValidateForImage(im); err != nil {
		return nil, err
	}
	if err := newPlan.ValidateForImage(im); err != nil {
		return nil, err
	}
	oldGraph, err := BuildDependencyGraph(oldPlan, im, tileSize)
	if err != nil {
		return nil, err
	}
	newGraph, err := BuildDependencyGraph(newPlan, im, tileSize)
	if err != nil {
		return nil, err
	}
	first := firstDifference(oldPlan, newPlan)
	if first < 0 {
		return &InvalidationManifest{FirstChangedPosition: -1}, nil
	}
	oldCoverage := Rect{}
	newCoverage := Rect{}
	for i := first; i < len(oldPlan.Entries); i++ {
		oldCoverage = oldCoverage.Union(entryCoverage(oldPlan.Entries[i], im))
	}
	for i := first; i < len(newPlan.Entries); i++ {
		newCoverage = newCoverage.Union(entryCoverage(newPlan.Entries[i], im))
	}
	// A pure parameter edit is localized to the changed entry's own pixels.
	// A sequence edit changes stage identity from that position onward.
	changedCoverage := oldCoverage.Union(newCoverage)
	orderChanged := entriesMoved(oldPlan, newPlan, first)
	if !orderChanged {
		changedCoverage = Rect{}
		if first < len(oldPlan.Entries) {
			changedCoverage = changedCoverage.Union(entryCoverage(oldPlan.Entries[first], im))
		}
		if first < len(newPlan.Entries) {
			changedCoverage = changedCoverage.Union(entryCoverage(newPlan.Entries[first], im))
		}
	}
	changedTiles := TilesForRect(newGraph.Tiles, changedCoverage)
	if len(changedTiles) == 0 && first < len(oldPlan.Entries) {
		changedTiles = TilesForRect(oldGraph.Tiles, changedCoverage)
	}
	manifest := &InvalidationManifest{FirstChangedPosition: first}
	seen := map[int]bool{}
	for i := first; i < len(newPlan.Entries); i++ {
		e := newPlan.Entries[i]
		coverage := entryCoverage(e, im)
		tiles := intersectTileSets(changedTiles, TilesForRect(newGraph.Tiles, coverage))
		if len(tiles) == 0 {
			continue
		}
		reason := fmt.Sprintf("downstream_of_entry_%d", newPlan.Entries[first].Seq)
		fields := []string{}
		if i == first {
			if orderChanged {
				reason = "stage_order_changed"
				fields = []string{"entries"}
			} else {
				reason = "entry_changed"
				fields = changedFields(oldPlan, newPlan, first)
			}
		} else if orderChanged {
			reason = "stage_order_changed"
		}
		key := e.Seq
		if seen[key] {
			continue
		}
		seen[key] = true
		manifest.Affected = append(manifest.Affected, Invalidation{
			Entry: e.Seq, Tiles: tiles, Region: tileUnion(newGraph.Tiles, tiles),
			Reason: reason, Fields: fields,
		})
	}
	// Entries removed by the new plan still need their old cache entries
	// released. They have no downstream stage in the new plan.
	for i := first; i < len(oldPlan.Entries); i++ {
		e := oldPlan.Entries[i]
		if _, ok := newPlan.Entry(e.Seq); ok {
			continue
		}
		tiles := intersectTileSets(changedTiles, TilesForRect(oldGraph.Tiles, entryCoverage(e, im)))
		if len(tiles) > 0 {
			manifest.Affected = append(manifest.Affected, Invalidation{
				Entry: e.Seq, Tiles: tiles, Region: tileUnion(oldGraph.Tiles, tiles),
				Reason: "entry_removed",
			})
		}
	}
	sort.Slice(manifest.Affected, func(i, j int) bool { return manifest.Affected[i].Entry < manifest.Affected[j].Entry })
	return manifest, nil
}

func tileUnion(tiles []Tile, selected []int) Rect {
	var out Rect
	for _, idx := range selected {
		if idx >= 0 && idx < len(tiles) {
			out = out.Union(tiles[idx].Rect())
		}
	}
	return out
}

// entriesMoved detects a suffix reorder even though valid plans renumber seq
// contiguously. Params are compared with seq removed so a moved entry can be
// recognized at its new position.
func entriesMoved(oldPlan, newPlan *Plan, first int) bool {
	if len(oldPlan.Entries) != len(newPlan.Entries) {
		return false
	}
	for i := first; i < len(newPlan.Entries); i++ {
		if entriesEqualIgnoringSeq(oldPlan.Entries[i], newPlan.Entries[i]) {
			continue
		}
		for j := first; j < len(oldPlan.Entries); j++ {
			if j != i && entriesEqualIgnoringSeq(oldPlan.Entries[j], newPlan.Entries[i]) {
				return true
			}
		}
	}
	return false
}

func entriesEqualIgnoringSeq(a, b Entry) bool {
	a.Seq = 0
	b.Seq = 0
	return EntryFingerprint(a) == EntryFingerprint(b)
}

func firstDifference(a, b *Plan) int {
	n := min(len(a.Entries), len(b.Entries))
	for i := 0; i < n; i++ {
		if a.Entries[i].Seq != b.Entries[i].Seq || EntryFingerprint(a.Entries[i]) != EntryFingerprint(b.Entries[i]) {
			return i
		}
	}
	if len(a.Entries) != len(b.Entries) {
		return n
	}
	return -1
}

func changedFields(oldPlan, newPlan *Plan, pos int) []string {
	var fields []string
	if pos >= len(oldPlan.Entries) || pos >= len(newPlan.Entries) {
		return []string{"entries"}
	}
	a, b := oldPlan.Entries[pos], newPlan.Entries[pos]
	if a.TargetDepth != b.TargetDepth {
		fields = append(fields, "targetDepth")
	}
	if a.TargetChannels != b.TargetChannels {
		fields = append(fields, "targetChannels")
	}
	if !equalInts(a.Permutation, b.Permutation) {
		fields = append(fields, "permutation")
	}
	if EntryFingerprint(Entry{Seq: a.Seq, TargetDepth: a.TargetDepth, TargetChannels: a.TargetChannels,
		Permutation: a.Permutation, Fill: a.Fill, Drop: a.Drop, Quantization: a.Quantization,
		Alpha: a.Alpha, Region: a.Region}) != EntryFingerprint(b) {
		// The broad equality above is intentionally followed by targeted fields
		// to keep the manifest useful rather than merely "entry_changed".
		if !sameFill(a.Fill, b.Fill) {
			fields = append(fields, "fill")
		}
		if !equalInts(a.Drop, b.Drop) {
			fields = append(fields, "drop")
		}
		if a.Quantization != b.Quantization {
			fields = append(fields, "quantization")
		}
		if a.Alpha != b.Alpha {
			fields = append(fields, "alpha")
		}
		if rectString(a.Region) != rectString(b.Region) {
			fields = append(fields, "region")
		}
	}
	if len(fields) == 0 {
		fields = []string{"entries"}
	}
	return fields
}

func sameFill(a, b []FillSpec) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]FillSpec(nil), a...)
	bb := append([]FillSpec(nil), b...)
	sort.Slice(aa, func(i, j int) bool { return aa[i].Channel < aa[j].Channel })
	sort.Slice(bb, func(i, j int) bool { return bb[i].Channel < bb[j].Channel })
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func entryCoverage(e Entry, im *Image) Rect {
	if e.Region != nil {
		return *e.Region
	}
	return Rect{W: im.Width, H: im.Height}
}

func intersectTileSets(a, b []int) []int {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	set := make(map[int]bool, len(a))
	for _, v := range a {
		set[v] = true
	}
	var out []int
	for _, v := range b {
		if set[v] {
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

func InvalidationSummary(m *InvalidationManifest) string {
	var parts []string
	for _, a := range m.Affected {
		parts = append(parts, fmt.Sprintf("entry=%d tiles=%v region=(%d,%d,%d,%d) reason=%s fields=%s",
			a.Entry, a.Tiles, a.Region.X, a.Region.Y, a.Region.W, a.Region.H,
			a.Reason, strings.Join(a.Fields, ",")))
	}
	return strings.Join(parts, "; ")
}
