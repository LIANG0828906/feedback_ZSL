package volumefog

import (
	"fmt"
	"sort"
	"strings"
)

type DependencyRef struct {
	Kind        string `json:"kind"`
	MediaID     string `json:"media_id,omitempty"`
	Voxel       int    `json:"voxel,omitempty"`
	CameraIndex int    `json:"camera_index,omitempty"`
}

func (d DependencyRef) SourceID() string {
	switch d.Kind {
	case "density":
		return fmt.Sprintf("density:%s:%d", d.MediaID, d.Voxel)
	case "absorption":
		return "absorption:" + d.MediaID
	case "scattering":
		return "scattering:" + d.MediaID
	case "anisotropy":
		return "anisotropy:" + d.MediaID
	case "camera":
		return fmt.Sprintf("camera:%d", d.CameraIndex)
	case "recipe":
		return "recipe"
	default:
		return d.Kind
	}
}

func dependencyKey(d DependencyRef) string {
	return fmt.Sprintf("%s|%s|%d|%d", d.Kind, d.MediaID, d.Voxel, d.CameraIndex)
}

func dedupeDependencies(refs []DependencyRef) []DependencyRef {
	seen := make(map[string]DependencyRef, len(refs))
	for _, r := range refs {
		seen[dependencyKey(r)] = r
	}
	out := make([]DependencyRef, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return dependencyKey(out[i]) < dependencyKey(out[j])
	})
	return out
}

func mergeDependencies(groups ...[]DependencyRef) []DependencyRef {
	var all []DependencyRef
	for _, g := range groups {
		all = append(all, g...)
	}
	return dedupeDependencies(all)
}

// dependencyValue reads the current source value for cache-key generation.
func dependencyValue(scene *Scene, d DependencyRef) string {
	switch d.Kind {
	case "density":
		m, _ := scene.MediumByID(d.MediaID)
		if m == nil || d.Voxel < 0 || d.Voxel >= len(m.Density) {
			return "missing"
		}
		return fmt.Sprintf("%d", m.Density[d.Voxel])
	case "absorption":
		m, _ := scene.MediumByID(d.MediaID)
		if m == nil {
			return "missing"
		}
		return fmt.Sprintf("%d", m.AbsorptionQ)
	case "scattering":
		m, _ := scene.MediumByID(d.MediaID)
		if m == nil {
			return "missing"
		}
		return fmt.Sprintf("%d", m.ScatteringQ)
	case "anisotropy":
		m, _ := scene.MediumByID(d.MediaID)
		if m == nil {
			return "missing"
		}
		return fmt.Sprintf("%d", m.AnisotropyQ)
	case "camera":
		if d.CameraIndex < 0 || d.CameraIndex >= len(scene.Cameras) {
			return "missing"
		}
		c := scene.Cameras[d.CameraIndex]
		return fmt.Sprintf("%d|%v|%v|%d|%d|%.12g|%d|%d|%d|%d",
			c.TimeUS, c.PositionQ, c.OrientationMdeg, c.ViewWidthQ, c.ViewHeightQ,
			c.FovDeg, c.NearQ, c.FarQ, c.Width, c.Height)
	case "recipe":
		return scene.RecipeVersion()
	default:
		return d.Kind
	}
}

func dependencyFingerprint(scene *Scene, refs []DependencyRef) string {
	parts := make([]string, 0, len(refs)+1)
	for _, d := range dedupeDependencies(refs) {
		parts = append(parts, d.SourceID()+"="+dependencyValue(scene, d))
	}
	return hashStrings(parts...)
}

// RayCacheKey builds the per-ray upstream chain used by chunk keys.
func RayCacheKey(scene *Scene, rayID string, refs []DependencyRef, previous string) (fingerprint, key string) {
	fingerprint = dependencyFingerprint(scene, refs)
	key = hashStrings("ray", rayID, fingerprint, previous,
		scene.GeometryVersion(), scene.CameraVersion(), scene.RecipeVersion(), AlgorithmVersion)
	return fingerprint, key
}

func ChunkContentHash(scene *Scene, rayDeps []RayDependency) string {
	parts := make([]string, 0, len(rayDeps)+1)
	for _, rd := range rayDeps {
		parts = append(parts, rd.RayID+":"+dependencyFingerprint(scene, rd.Dependencies))
	}
	sort.Strings(parts)
	return hashStrings(parts...)
}

func ChunkCacheKey(scene *Scene, rayDeps []RayDependency, contentHash string) string {
	parts := []string{AlgorithmVersion, scene.GeometryVersion(), scene.CameraVersion(),
		scene.RecipeVersion(), contentHash}
	for _, rd := range rayDeps {
		parts = append(parts, rd.RayID+":"+rd.CacheKey)
	}
	sort.Strings(parts)
	return hashStrings(parts...)
}

type RayDependency struct {
	RayID        string              `json:"ray_id"`
	CacheKey     string              `json:"cache_key"`
	ContentHash  string              `json:"content_hash"`
	SegmentCount int                 `json:"segment_count"`
	SegmentKeys  []string            `json:"segment_keys,omitempty"`
	Segments     []SegmentDependency `json:"segments,omitempty"`
	Dependencies []DependencyRef     `json:"dependencies"`
}

type SegmentDependency struct {
	Index        int             `json:"index"`
	ContentHash  string          `json:"content_hash"`
	CacheKey     string          `json:"cache_key"`
	UpstreamKey  string          `json:"upstream_key,omitempty"`
	Dependencies []DependencyRef `json:"dependencies"`
}

func buildSegmentDependencies(scene *Scene, rayID string, segments []RaySegmentResult) []SegmentDependency {
	out := make([]SegmentDependency, 0, len(segments))
	prev := ""
	for _, seg := range segments {
		fp, key := RayCacheKey(scene, fmt.Sprintf("%s:seg:%d", rayID, seg.Index), seg.Dependencies, prev)
		out = append(out, SegmentDependency{
			Index: seg.Index, ContentHash: fp, CacheKey: key, UpstreamKey: prev,
			Dependencies: seg.Dependencies,
		})
		prev = key
	}
	return out
}

type Invalidation struct {
	MediaID string     `json:"media_id"`
	Chunk   ChunkCoord `json:"chunk"`
	Reason  string     `json:"reason"`
	Sources []string   `json:"sources"`
}

type depNode struct {
	ID         string
	Kind       string
	Upstream   map[string]struct{}
	Downstream map[string]struct{}
	CacheKey   string
}

// DependencyGraph records source -> ray -> chunk edges and invalidates exact
// downstream chunks.
type DependencyGraph struct {
	nodes           map[string]*depNode
	sourceChunks    map[string]map[string]struct{}
	raysByChunk     map[string][]string
	segmentsByChunk map[string][]string
	chunks          map[string]ChunkCoord
}

func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{
		nodes:           make(map[string]*depNode),
		sourceChunks:    make(map[string]map[string]struct{}),
		raysByChunk:     make(map[string][]string),
		segmentsByChunk: make(map[string][]string),
		chunks:          make(map[string]ChunkCoord),
	}
}

func chunkNodeID(c ChunkCoord) string { return fmt.Sprintf("chunk:%d,%d", c.X, c.Y) }
func rayNodeID(c ChunkCoord, rayID string) string {
	return fmt.Sprintf("ray:%d,%d:%s", c.X, c.Y, rayID)
}
func segNodeID(c ChunkCoord, rayID string, index int) string {
	return fmt.Sprintf("rayseg:%d,%d:%s:%d", c.X, c.Y, rayID, index)
}

func (g *DependencyGraph) node(id, kind string) *depNode {
	n := g.nodes[id]
	if n == nil {
		n = &depNode{ID: id, Kind: kind, Upstream: map[string]struct{}{}, Downstream: map[string]struct{}{}}
		g.nodes[id] = n
	}
	return n
}

func (g *DependencyGraph) edge(from, to string) {
	a := g.node(from, "source")
	b := g.node(to, "downstream")
	a.Downstream[to] = struct{}{}
	b.Upstream[from] = struct{}{}
}

func (g *DependencyGraph) RemoveChunk(c ChunkCoord) {
	cn := chunkNodeID(c)
	for _, sn := range g.segmentsByChunk[cn] {
		if n := g.nodes[sn]; n != nil {
			for up := range n.Upstream {
				if p := g.nodes[up]; p != nil {
					delete(p.Downstream, sn)
				}
			}
			for down := range n.Downstream {
				if d := g.nodes[down]; d != nil {
					delete(d.Upstream, sn)
				}
			}
			delete(g.nodes, sn)
		}
	}
	for _, rn := range g.raysByChunk[cn] {
		if n := g.nodes[rn]; n != nil {
			for up := range n.Upstream {
				if sn := g.nodes[up]; sn != nil {
					delete(sn.Downstream, rn)
				}
			}
			for down := range n.Downstream {
				if dn := g.nodes[down]; dn != nil {
					delete(dn.Upstream, rn)
				}
			}
			delete(g.nodes, rn)
		}
	}
	if n := g.nodes[cn]; n != nil {
		for up := range n.Upstream {
			if sn := g.nodes[up]; sn != nil {
				delete(sn.Downstream, cn)
			}
		}
		delete(g.nodes, cn)
	}
	for src, set := range g.sourceChunks {
		delete(set, cn)
		if len(set) == 0 {
			delete(g.sourceChunks, src)
		}
	}
	delete(g.raysByChunk, cn)
	delete(g.segmentsByChunk, cn)
	delete(g.chunks, cn)
}

func (g *DependencyGraph) RecordChunk(c ChunkCoord, rayDeps []RayDependency, chunkKey string) {
	g.RemoveChunk(c)
	cn := chunkNodeID(c)
	g.chunks[cn] = c
	if n := g.node(cn, "chunk"); n != nil {
		n.CacheKey = chunkKey
	}
	for _, rd := range rayDeps {
		rn := rayNodeID(c, rd.RayID)
		g.node(rn, "ray").CacheKey = rd.CacheKey
		prevSeg := ""
		for _, sd := range rd.Segments {
			sn := segNodeID(c, rd.RayID, sd.Index)
			g.node(sn, "segment").CacheKey = sd.CacheKey
			g.edge(sn, rn)
			if prevSeg != "" {
				g.edge(prevSeg, sn)
			}
			g.segmentsByChunk[cn] = append(g.segmentsByChunk[cn], sn)
			for _, dep := range sd.Dependencies {
				src := dep.SourceID()
				g.edge(src, sn)
				if g.sourceChunks[src] == nil {
					g.sourceChunks[src] = map[string]struct{}{}
				}
				g.sourceChunks[src][cn] = struct{}{}
			}
			prevSeg = sn
		}
		g.edge(rn, cn)
		g.raysByChunk[cn] = append(g.raysByChunk[cn], rn)
		for _, dep := range rd.Dependencies {
			src := dep.SourceID()
			g.edge(src, rn)
			if g.sourceChunks[src] == nil {
				g.sourceChunks[src] = map[string]struct{}{}
			}
			g.sourceChunks[src][cn] = struct{}{}
		}
	}
}

func (g *DependencyGraph) InvalidateSource(source, mediaID, reason string) []Invalidation {
	seen := map[string]Invalidation{}
	for cn := range g.sourceChunks[source] {
		c, ok := g.chunks[cn]
		if !ok {
			continue
		}
		inv := seen[cn]
		inv.MediaID = mediaID
		inv.Chunk = c
		inv.Reason = reason
		inv.Sources = append(inv.Sources, source)
		seen[cn] = inv
	}
	out := make([]Invalidation, 0, len(seen))
	for _, inv := range seen {
		sort.Strings(inv.Sources)
		out = append(out, inv)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Chunk.Y != out[j].Chunk.Y {
			return out[i].Chunk.Y < out[j].Chunk.Y
		}
		return out[i].Chunk.X < out[j].Chunk.X
	})
	return out
}

func (g *DependencyGraph) InvalidatePrefix(prefix, mediaID, reason string) []Invalidation {
	var out []Invalidation
	for src := range g.sourceChunks {
		if strings.HasPrefix(src, prefix) {
			out = append(out, g.InvalidateSource(src, mediaID, reason)...)
		}
	}
	seen := map[string]Invalidation{}
	for _, inv := range out {
		key := chunkNodeID(inv.Chunk)
		if old, ok := seen[key]; ok {
			old.Sources = append(old.Sources, inv.Sources...)
			seen[key] = old
		} else {
			seen[key] = inv
		}
	}
	out = out[:0]
	for _, inv := range seen {
		inv.Sources = dedupeStrings(inv.Sources)
		out = append(out, inv)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Chunk.Y != out[j].Chunk.Y {
			return out[i].Chunk.Y < out[j].Chunk.Y
		}
		return out[i].Chunk.X < out[j].Chunk.X
	})
	return out
}

func dedupeStrings(in []string) []string {
	m := map[string]struct{}{}
	out := in[:0]
	for _, s := range in {
		if _, ok := m[s]; ok {
			continue
		}
		m[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

type GraphStats struct {
	Nodes  int `json:"nodes"`
	Edges  int `json:"edges"`
	Chunks int `json:"chunks"`
	Rays   int `json:"rays"`
}

func (g *DependencyGraph) Stats() GraphStats {
	var st GraphStats
	st.Nodes = len(g.nodes)
	st.Chunks = len(g.chunks)
	for _, n := range g.nodes {
		st.Edges += len(n.Downstream)
		if n.Kind == "ray" {
			st.Rays++
		}
	}
	return st
}
