package fog

import (
	"fmt"
	"sort"
)

type NodeKind string

const (
	NodeDensity     NodeKind = "medium_density"
	NodeCoefficient NodeKind = "medium_coefficients"
	NodeCamera      NodeKind = "camera"
	NodeRecipe      NodeKind = "recipe"
	NodeSegment     NodeKind = "ray_segment"
	NodeTile        NodeKind = "tile"
	NodePixel       NodeKind = "pixel"
)

type GraphNode struct {
	ID       string   `json:"id"`
	Kind     NodeKind `json:"kind"`
	Label    string   `json:"label"`
	MediumID string   `json:"medium_id,omitempty"`
	BlockID  *int     `json:"block_id,omitempty"`
}

type GraphEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type RayRef struct {
	RayID   string `json:"ray_id"`
	BlockID int    `json:"block_id"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
}

type InvalidationItem struct {
	BlockID   int     `json:"block_id"`
	MediumID  string  `json:"medium_id,omitempty"`
	Reason    string  `json:"reason"`
	FieldPath string  `json:"field_path,omitempty"`
	VoxelKey  string  `json:"voxel_key,omitempty"`
	Pixels    []Point `json:"pixels,omitempty"`
}

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type InvalidationManifest struct {
	Reason string             `json:"reason"`
	Items  []InvalidationItem `json:"items"`
	Blocks []int              `json:"blocks"`
}

func (m InvalidationManifest) Len() int { return len(m.Items) }

// DependencyGraph contains the explicit downstream relations for one camera
// version and viewport. It is rebuilt deterministically from scene content.
type DependencyGraph struct {
	Nodes       map[string]GraphNode   `json:"nodes"`
	Edges       []GraphEdge            `json:"edges"`
	VoxelRefs   map[string][]RayRef    `json:"voxel_refs"`
	MediumRefs  map[string][]RayRef    `json:"medium_refs"`
	BlockPixels map[int][]Point        `json:"block_pixels"`
	Traces      map[string]TraceResult `json:"-"`
	Viewport    Viewport               `json:"viewport"`
	CameraID    string                 `json:"camera_id"`
}

func BuildDependencyGraph(s *Scenario, cameraID string) (*DependencyGraph, error) {
	cam, ok := s.Camera(cameraID)
	if !ok {
		cam = s.ActiveCamera()
	}
	vp := NewViewport(s.Width, s.Height, s.Recipe.TileSize)
	g := &DependencyGraph{
		Nodes:       map[string]GraphNode{},
		VoxelRefs:   map[string][]RayRef{},
		MediumRefs:  map[string][]RayRef{},
		BlockPixels: map[int][]Point{},
		Traces:      map[string]TraceResult{},
		Viewport:    vp,
		CameraID:    cam.ID,
	}

	cameraNode := "camera:" + cam.ID + ":" + cam.Version
	recipeNode := "recipe:" + s.Recipe.Version + ":" + AlgorithmVersion
	g.addNode(GraphNode{ID: cameraNode, Kind: NodeCamera, Label: "Camera " + cam.ID})
	g.addNode(GraphNode{ID: recipeNode, Kind: NodeRecipe, Label: "Recipe " + s.Recipe.Version})

	for _, m := range s.Media {
		densityNode := "density:" + m.ID + ":" + DensityHash(m)
		coeffNode := "coeff:" + m.ID + ":" + CoefficientHash(m)
		g.addNode(GraphNode{ID: densityNode, Kind: NodeDensity, Label: m.ID + " density", MediumID: m.ID})
		g.addNode(GraphNode{ID: coeffNode, Kind: NodeCoefficient, Label: m.ID + " coefficients", MediumID: m.ID})
	}

	for _, tile := range vp.Tiles {
		blockNode := fmt.Sprintf("tile:%d", tile.ID)
		bid := tile.ID
		g.addNode(GraphNode{ID: blockNode, Kind: NodeTile, Label: tileLabel(tile.ID), BlockID: &bid})
		for y := tile.Y0; y < tile.Y0+tile.Height; y++ {
			for x := tile.X0; x < tile.X0+tile.Width; x++ {
				ray := cam.RayForPixel(x, y, s.Width, s.Height, tile.ID)
				tr := TraceRay(ray, s.Media, s.FixedScale)
				g.Traces[ray.ID] = tr
				ref := RayRef{RayID: ray.ID, BlockID: tile.ID, X: x, Y: y}
				pixelNode := fmt.Sprintf("pixel:%d:%d", x, y)
				g.addNode(GraphNode{ID: pixelNode, Kind: NodePixel, Label: fmt.Sprintf("px %d,%d", x, y), BlockID: &bid})
				g.addEdge(blockNode, pixelNode, "tile-contains-pixel")
				g.BlockPixels[tile.ID] = append(g.BlockPixels[tile.ID], Point{X: x, Y: y})

				for _, span := range tr.Spans {
					segNode := fmt.Sprintf("segment:%s:%d", ray.ID, span.Index)
					g.addNode(GraphNode{ID: segNode, Kind: NodeSegment, Label: fmt.Sprintf("%s span %d", ray.ID, span.Index), BlockID: &bid})
					g.addEdge(segNode, blockNode, "segment-in-block")
					g.addEdge(cameraNode, segNode, "camera-ray")
					g.addEdge(recipeNode, segNode, "recipe-step-phase-shadow")
					for _, mediumID := range span.MediumIDs {
						m, exists := s.Medium(mediumID)
						if !exists {
							continue
						}
						g.addEdge("density:"+mediumID+":"+DensityHash(m), segNode, "density-voxel-sample")
						g.addEdge("coeff:"+mediumID+":"+CoefficientHash(m), segNode, "absorption-scattering-phase")
						g.MediumRefs[mediumID] = append(g.MediumRefs[mediumID], ref)
					}
					for _, key := range span.VoxelKeys {
						g.VoxelRefs[key] = append(g.VoxelRefs[key], ref)
					}
				}
			}
		}
	}
	g.sortRefsAndEdges()
	return g, nil
}

func (g *DependencyGraph) addNode(n GraphNode) { g.Nodes[n.ID] = n }

func (g *DependencyGraph) addEdge(from, to, reason string) {
	for _, e := range g.Edges {
		if e.From == from && e.To == to && e.Reason == reason {
			return
		}
	}
	g.Edges = append(g.Edges, GraphEdge{From: from, To: to, Reason: reason})
}

func (g *DependencyGraph) sortRefsAndEdges() {
	for k := range g.VoxelRefs {
		sortRayRefs(g.VoxelRefs[k])
	}
	for k := range g.MediumRefs {
		sortRayRefs(g.MediumRefs[k])
	}
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		if g.Edges[i].To != g.Edges[j].To {
			return g.Edges[i].To < g.Edges[j].To
		}
		return g.Edges[i].Reason < g.Edges[j].Reason
	})
}

func sortRayRefs(refs []RayRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].BlockID != refs[j].BlockID {
			return refs[i].BlockID < refs[j].BlockID
		}
		if refs[i].Y != refs[j].Y {
			return refs[i].Y < refs[j].Y
		}
		return refs[i].X < refs[j].X
	})
}

// InvalidateVoxel returns only blocks with at least one ray traversing the
// exact edited voxel.
func (g *DependencyGraph) InvalidateVoxel(edit DensityEdit) InvalidationManifest {
	key := voxelKey(edit.MediumID, edit.X, edit.Y, edit.Z)
	refs := g.VoxelRefs[key]
	byBlock := map[int]*InvalidationItem{}
	for _, ref := range refs {
		item := byBlock[ref.BlockID]
		if item == nil {
			item = &InvalidationItem{
				BlockID: ref.BlockID, MediumID: edit.MediumID,
				Reason: "density-voxel-changed", FieldPath: fmt.Sprintf("field.values[%d,%d,%d]", edit.X, edit.Y, edit.Z),
				VoxelKey: key,
			}
			byBlock[ref.BlockID] = item
		}
		item.Pixels = append(item.Pixels, Point{X: ref.X, Y: ref.Y})
	}
	return manifestFromMap("density-edit", byBlock)
}

func (g *DependencyGraph) InvalidateMediumCoefficients(mediumID, field string) InvalidationManifest {
	byBlock := map[int]*InvalidationItem{}
	for _, ref := range g.MediumRefs[mediumID] {
		item := byBlock[ref.BlockID]
		if item == nil {
			item = &InvalidationItem{
				BlockID: ref.BlockID, MediumID: mediumID,
				Reason: "coefficient-changed", FieldPath: field,
			}
			byBlock[ref.BlockID] = item
		}
		item.Pixels = append(item.Pixels, Point{X: ref.X, Y: ref.Y})
	}
	return manifestFromMap("coefficient-edit", byBlock)
}

func (g *DependencyGraph) InvalidateAll(reason, field string) InvalidationManifest {
	byBlock := map[int]*InvalidationItem{}
	for _, block := range g.Viewport.Tiles {
		byBlock[block.ID] = &InvalidationItem{
			BlockID: block.ID, Reason: reason, FieldPath: field,
			Pixels: append([]Point(nil), g.BlockPixels[block.ID]...),
		}
	}
	return manifestFromMap(reason, byBlock)
}

func manifestFromMap(reason string, byBlock map[int]*InvalidationItem) InvalidationManifest {
	ids := make([]int, 0, len(byBlock))
	for id := range byBlock {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	items := make([]InvalidationItem, 0, len(ids))
	blocks := make([]int, 0, len(ids))
	for _, id := range ids {
		item := byBlock[id]
		sort.Slice(item.Pixels, func(i, j int) bool {
			if item.Pixels[i].Y != item.Pixels[j].Y {
				return item.Pixels[i].Y < item.Pixels[j].Y
			}
			return item.Pixels[i].X < item.Pixels[j].X
		})
		items = append(items, *item)
		blocks = append(blocks, id)
	}
	return InvalidationManifest{Reason: reason, Items: items, Blocks: blocks}
}

func DensityHash(m *Medium) string {
	return ContentHash("density", m.Field)
}

func CoefficientHash(m *Medium) string {
	return ContentHash("coeff", struct {
		Absorption float64 `json:"absorption"`
		Scattering float64 `json:"scattering"`
		Anisotropy float64 `json:"anisotropy"`
	}{m.Absorption, m.Scattering, m.Anisotropy})
}

func RecipeParameterHash(r Recipe) string {
	return ContentHash("recipe-params", struct {
		Steps                int       `json:"steps"`
		PreviewSteps         int       `json:"preview_steps"`
		ShadowSamples        int       `json:"shadow_samples"`
		PreviewShadowSamples int       `json:"preview_shadow_samples"`
		Phase                PhaseKind `json:"phase"`
		FixedScale           int64     `json:"fixed_scale"`
	}{r.Steps, r.PreviewSteps, r.ShadowSamples, r.PreviewShadowSamples, r.Phase, r.FixedScale})
}

type CacheIdentity struct {
	BlockID          int               `json:"block_id"`
	BlockKey         string            `json:"block_key"`
	SegmentKeys      []string          `json:"segment_keys"`
	RayDigest        string            `json:"ray_digest"`
	MediumHashes     map[string]string `json:"medium_hashes"`
	ReadSetHashes    map[string]string `json:"read_set_hashes"`
	SnapshotVersion  string            `json:"snapshot_version"`
	CameraVersion    string            `json:"camera_version"`
	RecipeVersion    string            `json:"recipe_version"`
	AlgorithmVersion string            `json:"algorithm_version"`
}

func BuildCacheIdentity(s *Scenario, block Tile, traces []TraceResult) CacheIdentity {
	mediumHashes := map[string]string{}
	readVoxels := map[string]map[string]string{}
	relevant := map[string]bool{}
	for _, tr := range traces {
		for _, iv := range tr.Intervals {
			relevant[iv.MediumID] = true
		}
		for _, span := range tr.Spans {
			if span.Kind != SpanMedium {
				continue
			}
			for mediumID, voxelHashes := range SpanReadSet(s, span, tr.Ray) {
				if readVoxels[mediumID] == nil {
					readVoxels[mediumID] = map[string]string{}
				}
				for key, hash := range voxelHashes {
					readVoxels[mediumID][key] = hash
				}
			}
		}
	}
	for _, m := range s.Media {
		if relevant[m.ID] {
			mediumHashes[m.ID] = m.ContentHash
		}
	}
	cam := s.ActiveCamera()
	segmentKeys := make([]string, 0)
	digests := make([]string, 0, len(traces))
	for _, tr := range traces {
		digests = append(digests, tr.Digest)
		for _, span := range tr.Spans {
			segmentKeys = append(segmentKeys, SegmentCacheKey(s, tr.Ray.ID, span, SpanReadSetHashes(s, span, tr.Ray)))
		}
	}
	sort.Strings(digests)
	rayDigest := ContentHash("block-rays", struct {
		BlockID int      `json:"block_id"`
		Digests []string `json:"digests"`
	}{block.ID, digests})
	readSetHashes := map[string]string{}
	for mediumID, voxels := range readVoxels {
		coeff := ""
		if m, ok := s.Medium(mediumID); ok {
			coeff = CoefficientHash(m)
		}
		readSetHashes[mediumID] = ContentHash("medium-read-set", struct {
			Coefficient string            `json:"coefficient"`
			Voxels      map[string]string `json:"voxels"`
		}{coeff, voxels})
	}
	blockKey := ContentHash("block-cache", struct {
		BlockID          int               `json:"block_id"`
		RayDigest        string            `json:"ray_digest"`
		SegmentKeys      []string          `json:"segment_keys"`
		ReadSetHashes    map[string]string `json:"read_set_hashes"`
		CameraVersion    string            `json:"camera_version"`
		RecipeVersion    string            `json:"recipe_version"`
		RecipeParameters string            `json:"recipe_parameters"`
		Algorithm        string            `json:"algorithm_version"`
	}{block.ID, rayDigest, segmentKeys, readSetHashes, cam.Version, s.Recipe.Version, RecipeParameterHash(s.Recipe), AlgorithmVersion})
	return CacheIdentity{
		BlockID: block.ID, BlockKey: blockKey, SegmentKeys: segmentKeys,
		RayDigest: rayDigest, MediumHashes: mediumHashes, ReadSetHashes: readSetHashes,
		SnapshotVersion: SnapshotVersion(s),
		CameraVersion:   cam.Version, RecipeVersion: s.Recipe.Version,
		AlgorithmVersion: AlgorithmVersion,
	}
}

// SpanReadSet returns medium -> voxel key -> voxel content hash. Only voxels
// actually traversed by this span are included.
func SpanReadSet(s *Scenario, span RaySpan, ray Ray) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, mediumID := range span.MediumIDs {
		m, ok := s.Medium(mediumID)
		if !ok {
			continue
		}
		voxels := map[string]string{}
		for _, seg := range DensitySegments(m, ray, span.TMin, span.TMax) {
			key := voxelKey(mediumID, seg.X, seg.Y, seg.Z)
			voxels[key] = ContentHash("voxel-value", seg.Density)
		}
		out[mediumID] = voxels
	}
	return out
}

func SpanReadSetHashes(s *Scenario, span RaySpan, ray Ray) map[string]string {
	readSet := SpanReadSet(s, span, ray)
	out := map[string]string{}
	for mediumID, voxels := range readSet {
		coeff := ""
		if m, ok := s.Medium(mediumID); ok {
			coeff = CoefficientHash(m)
		}
		out[mediumID] = ContentHash("segment-read-set", struct {
			Coefficient string            `json:"coefficient"`
			Voxels      map[string]string `json:"voxels"`
		}{coeff, voxels})
	}
	return out
}

func SegmentCacheKey(s *Scenario, rayID string, span RaySpan, readSetHashes map[string]string) string {
	upstream := make([]string, 0, len(span.MediumIDs))
	for _, id := range span.MediumIDs {
		upstream = append(upstream, readSetHashes[id])
	}
	sort.Strings(upstream)
	return ContentHash("segment-cache", struct {
		RayID            string   `json:"ray_id"`
		SpanIndex        int      `json:"span_index"`
		TMinQ            int64    `json:"t_min_q"`
		TMaxQ            int64    `json:"t_max_q"`
		MediumIDs        []string `json:"medium_ids"`
		UpstreamKeys     []string `json:"upstream_keys"`
		CameraVersion    string   `json:"camera_version"`
		RecipeVersion    string   `json:"recipe_version"`
		RecipeParameters string   `json:"recipe_parameters"`
		Algorithm        string   `json:"algorithm"`
	}{rayID, span.Index, span.TMinQ, span.TMaxQ, span.MediumIDs, upstream, s.ActiveCamera().Version, s.Recipe.Version, RecipeParameterHash(s.Recipe), AlgorithmVersion})
}
