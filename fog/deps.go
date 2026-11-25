package fog

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type DepNode struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Label      string   `json:"label"`
	Upstream   []string `json:"upstream,omitempty"`
	Downstream []string `json:"downstream,omitempty"`
}

type DependencyGraph struct {
	mu    sync.RWMutex
	nodes map[string]*DepNode
}

func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{nodes: map[string]*DepNode{}}
}

func (g *DependencyGraph) AddNode(id, kind, label string) {
	if id == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.nodes[id]; ok {
		return
	}
	g.nodes[id] = &DepNode{ID: id, Kind: kind, Label: label}
}

func (g *DependencyGraph) AddEdge(from, to string) {
	if from == "" || to == "" || from == to {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.nodes[from]
	if !ok {
		f = &DepNode{ID: from, Kind: "unknown"}
		g.nodes[from] = f
	}
	t, ok := g.nodes[to]
	if !ok {
		t = &DepNode{ID: to, Kind: "unknown"}
		g.nodes[to] = t
	}
	if !containsString(f.Downstream, to) {
		f.Downstream = append(f.Downstream, to)
		sort.Strings(f.Downstream)
	}
	if !containsString(t.Upstream, from) {
		t.Upstream = append(t.Upstream, from)
		sort.Strings(t.Upstream)
	}
}

func (g *DependencyGraph) Has(id string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	_, ok := g.nodes[id]
	return ok
}

func (g *DependencyGraph) Nodes() []DepNode {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]DepNode, 0, len(g.nodes))
	for _, n := range g.nodes {
		cp := *n
		cp.Upstream = append([]string(nil), n.Upstream...)
		cp.Downstream = append([]string(nil), n.Downstream...)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type InvalidationItem struct {
	TileID   string   `json:"tile_id"`
	Pixel    PixelRef `json:"pixel"`
	RayIDs   []string `json:"ray_ids"`
	Segments []string `json:"segments"`
	Sources  []string `json:"sources"`
}

type InvalidationReport struct {
	Sources      []string           `json:"sources"`
	Tiles        []string           `json:"tiles"`
	Pixels       []PixelRef         `json:"pixels"`
	Rays         []string           `json:"rays"`
	Segments     []string           `json:"segments"`
	Items        []InvalidationItem `json:"items"`
	Unchanged    []string           `json:"unchanged_tiles"`
	CandidateAll []string           `json:"candidate_tiles"`
}

func (g *DependencyGraph) Invalidate(sources ...string) *InvalidationReport {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rep := &InvalidationReport{Sources: append([]string(nil), sources...)}
	sort.Strings(rep.Sources)
	tileSet := map[string]bool{}
	pixelSet := map[string]PixelRef{}
	raySet := map[string]bool{}
	segSet := map[string]bool{}
	nodeSources := map[string]map[string]bool{}
	allTiles := map[string]bool{}
	for _, n := range g.nodes {
		if n.Kind == "tile" {
			allTiles[tileIDFromNode(n.ID)] = true
		}
	}
	for _, src := range sources {
		if _, ok := g.nodes[src]; !ok {
			continue
		}
		queue := []string{src}
		seen := map[string]bool{src: true}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			n := g.nodes[id]
			if n == nil {
				continue
			}
			if nodeSources[id] == nil {
				nodeSources[id] = map[string]bool{}
			}
			nodeSources[id][src] = true
			switch n.Kind {
			case "tile":
				tileSet[tileIDFromNode(id)] = true
			case "pixel":
				// Pixel IDs are pixel:<rayid>; the parser below extracts x/y
				// from the stable ray id.
				if px, ok := parsePixelNode(n.ID); ok {
					pixelSet[n.ID] = px
				}
			case "segment":
				segSet[id] = true
				if rayID, ok := parseSegmentRay(n.ID); ok {
					raySet[rayID] = true
				}
			}
			for _, next := range n.Downstream {
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
		}
	}
	for id := range tileSet {
		rep.Tiles = append(rep.Tiles, id)
	}
	sort.Strings(rep.Tiles)
	for _, px := range pixelSet {
		rep.Pixels = append(rep.Pixels, px)
	}
	sort.Slice(rep.Pixels, func(i, j int) bool {
		if rep.Pixels[i].Y != rep.Pixels[j].Y {
			return rep.Pixels[i].Y < rep.Pixels[j].Y
		}
		return rep.Pixels[i].X < rep.Pixels[j].X
	})
	for id := range raySet {
		rep.Rays = append(rep.Rays, id)
	}
	sort.Strings(rep.Rays)
	for id := range segSet {
		rep.Segments = append(rep.Segments, id)
	}
	sort.Strings(rep.Segments)
	for id := range allTiles {
		if tileSet[id] {
			continue
		}
		rep.Unchanged = append(rep.Unchanged, id)
	}
	sort.Strings(rep.Unchanged)
	rep.CandidateAll = make([]string, 0, len(allTiles))
	for id := range allTiles {
		rep.CandidateAll = append(rep.CandidateAll, id)
	}
	sort.Strings(rep.CandidateAll)
	for pxID, px := range pixelSet {
		item := InvalidationItem{Pixel: px}
		if n := g.nodes[pxID]; n != nil {
			for _, down := range n.Downstream {
				if dn := g.nodes[down]; dn != nil && dn.Kind == "tile" {
					item.TileID = tileIDFromNode(down)
					break
				}
			}
		}
		item.RayIDs = []string{pixelRayID(pxID)}
		srcSet := map[string]bool{}
		for s := range nodeSources[pxID] {
			srcSet[s] = true
		}
		if n := g.nodes[pxID]; n != nil {
			for _, up := range n.Upstream {
				if len(nodeSources[up]) > 0 {
					if un := g.nodes[up]; un != nil && un.Kind == "segment" {
						item.Segments = append(item.Segments, up)
					}
					for s := range nodeSources[up] {
						srcSet[s] = true
					}
				}
			}
		}
		sort.Strings(item.Segments)
		for s := range srcSet {
			item.Sources = append(item.Sources, s)
		}
		sort.Strings(item.Sources)
		rep.Items = append(rep.Items, item)
	}
	sort.Slice(rep.Items, func(i, j int) bool {
		if rep.Items[i].Pixel.Y != rep.Items[j].Pixel.Y {
			return rep.Items[i].Pixel.Y < rep.Items[j].Pixel.Y
		}
		return rep.Items[i].Pixel.X < rep.Items[j].Pixel.X
	})
	return rep
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func DensitySource(mediaID string, voxel int) string {
	return fmt.Sprintf("density:%s:%d", mediaID, voxel)
}

func CoefficientSource(mediaID string) string {
	return "coeff:" + mediaID
}

func SourceMediaID(source string) string {
	switch {
	case strings.HasPrefix(source, "coeff:"):
		return strings.TrimPrefix(source, "coeff:")
	case strings.HasPrefix(source, "density:"):
		rest := strings.TrimPrefix(source, "density:")
		if i := strings.LastIndexByte(rest, ':'); i >= 0 {
			return rest[:i]
		}
	}
	return ""
}

func CoefficientContentHash(m *Medium, scale int64) string {
	if m == nil {
		return ""
	}
	f := func(v float64) string { return strconv.FormatFloat(v, 'g', 17, 64) }
	return hashStrings("coeff", m.ID, itoa(m.AbsorptionFP), itoa(m.ScatteringFP),
		f(m.Anisotropy), itoa(scale), strconv.Itoa(m.Priority),
		strconv.Itoa(m.DensityRes[0]), strconv.Itoa(m.DensityRes[1]), strconv.Itoa(m.DensityRes[2]),
		f(m.Bounds.Min.X), f(m.Bounds.Min.Y), f(m.Bounds.Min.Z),
		f(m.Bounds.Max.X), f(m.Bounds.Max.Y), f(m.Bounds.Max.Z))
}

func DensityContentHash(m *Medium, voxel int) string {
	if m == nil || voxel < 0 || voxel >= len(m.Density) {
		return ""
	}
	return hashStrings("density", m.ID, strconv.Itoa(voxel), strconv.Itoa(int(m.Density[voxel])))
}

func SourceContentHash(scene *Scene, source string) (string, bool) {
	mediaID := SourceMediaID(source)
	if mediaID == "" {
		return "", false
	}
	_, m := scene.MediaByID(mediaID)
	if m == nil {
		return "", false
	}
	if strings.HasPrefix(source, "coeff:") {
		return CoefficientContentHash(m, scene.Recipe.CoefficientScale), true
	}
	rest := strings.TrimPrefix(source, "density:")
	i := strings.LastIndexByte(rest, ':')
	if i < 0 {
		return "", false
	}
	idx, err := strconv.Atoi(rest[i+1:])
	if err != nil {
		return "", false
	}
	h := DensityContentHash(m, idx)
	return h, h != ""
}

// MediaDependencyHash hashes only the coefficient/voxel sources that a tile
// actually used. It is the per-tile "media content hash" input to cache keys.
func MediaDependencyHash(mediaID string, dependencyHashes map[string]string) string {
	srcs := make([]string, 0)
	for source := range dependencyHashes {
		if SourceMediaID(source) == mediaID {
			srcs = append(srcs, source)
		}
	}
	if len(srcs) == 0 {
		return ""
	}
	sort.Strings(srcs)
	parts := make([]string, 0, len(srcs)*2)
	for _, source := range srcs {
		parts = append(parts, source, dependencyHashes[source])
	}
	return hashStrings(parts...)
}

func CameraSource(hash string) string { return "camera:" + hash }
func RecipeSource(hash string) string { return "recipe:" + hash }

func SegmentNodeID(rayID string, index int) string {
	return fmt.Sprintf("segment:%s:%d", rayID, index)
}

func PixelNodeID(rayID string) string { return "pixel:" + rayID }
func TileNodeID(tileID string) string { return "tile:" + tileID }

func tileIDFromNode(node string) string {
	return strings.TrimPrefix(node, "tile:")
}

func parsePixelNode(id string) (PixelRef, bool) {
	ray := pixelRayID(id)
	var t int64
	var x, y int
	_, err := fmt.Sscanf(ray, "ray:c%d:x%d:y%d", &t, &x, &y)
	return PixelRef{X: x, Y: y}, err == nil
}

func pixelRayID(pixelNode string) string {
	const prefix = "pixel:"
	if len(pixelNode) > len(prefix) {
		return pixelNode[len(prefix):]
	}
	return pixelNode
}

func parseSegmentRay(id string) (string, bool) {
	const prefix = "segment:"
	if len(id) <= len(prefix) {
		return "", false
	}
	rest := id[len(prefix):]
	for i := len(rest) - 1; i >= 0; i-- {
		if rest[i] == ':' {
			return rest[:i], true
		}
	}
	return "", false
}

func (g *DependencyGraph) Summary() map[string]int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := map[string]int{}
	for _, n := range g.nodes {
		out[n.Kind]++
	}
	return out
}
