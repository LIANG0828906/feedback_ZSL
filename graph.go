package quantforensics

import (
	"fmt"
	"sort"
)

type DependencyNode struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Label     string   `json:"label"`
	CacheKey  string   `json:"cacheKey,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

type DependencyEdge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Relation string `json:"relation"`
}

type InvalidationItem struct {
	NodeID  string     `json:"nodeId"`
	BlockID string     `json:"blockId,omitempty"`
	Stage   BlockStage `json:"stage,omitempty"`
	Reason  string     `json:"reason"`
}

type InvalidationReport struct {
	ChangedNodes   []string           `json:"changedNodes"`
	Items          []InvalidationItem `json:"items"`
	AffectedBlocks []string           `json:"affectedBlocks"`
	ReusedBlocks   []string           `json:"reusedBlocks"`
	BlockReasons   map[string]string  `json:"blockReasons"`
}

type DependencyGraph struct {
	Nodes map[string]*DependencyNode `json:"nodes"`
	Edges []DependencyEdge           `json:"edges"`

	dependents map[string][]string
}

func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{Nodes: map[string]*DependencyNode{}, dependents: map[string][]string{}}
}

func (g *DependencyGraph) addNode(id, kind, label, cacheKey string, deps ...string) {
	if _, ok := g.Nodes[id]; !ok {
		g.Nodes[id] = &DependencyNode{ID: id, Kind: kind, Label: label, CacheKey: cacheKey}
	}
	g.Nodes[id].DependsOn = append(g.Nodes[id].DependsOn, deps...)
	for _, dep := range deps {
		g.Edges = append(g.Edges, DependencyEdge{From: dep, To: id, Relation: "depends_on"})
		g.dependents[dep] = append(g.dependents[dep], id)
	}
}

func PaletteNodeID(index int) string { return fmt.Sprintf("palette:entry:%d", index) }
func GeometryBlockNodeID(blockID string) string {
	return "geometry:block:" + blockID
}
func DitherGlobalNodeID() string { return "dither:global" }
func DitherBlockNodeID(blockID string) string {
	return "dither:block:" + blockID
}
func LoadNodeID(blockID string) string   { return "load:" + blockID }
func IndexNodeID(blockID string) string  { return "index:" + blockID }
func ReconNodeID(blockID string) string  { return "recon:" + blockID }
func VerifyNodeID(blockID string) string { return "verify:" + blockID }

// ComputeCacheKey implements the declared key formula. Upstream keys are
// appended in caller-controlled order and canonicalized as JSON.
func ComputeCacheKey(contentHash, paletteVersion, coordVersion string, paramValues any, upstreamKeys ...string) string {
	payload := struct {
		ContentHash    string   `json:"contentHash"`
		PaletteVersion string   `json:"paletteVersion"`
		CoordVersion   string   `json:"coordDeriveVersion"`
		Params         any      `json:"params"`
		UpstreamKeys   []string `json:"upstreamKeys"`
	}{contentHash, paletteVersion, coordVersion, paramValues, append([]string{}, upstreamKeys...)}
	return CanonicalHash(payload)
}

func BuildDependencyGraph(p *Project) *DependencyGraph {
	g := NewDependencyGraph()
	g.addNode(DitherGlobalNodeID(), "dither_parameter", "global dither", ComputeCacheKey("", p.Palette.Version, p.Params.CoordDeriveVersion, map[string]any{"globalDitherPPM": p.Params.GlobalDitherPPM, "algo": p.Params.AlgoVersion}))
	g.addNode("geometry:partition", "geometry_parameter", "origin and tile", ComputeCacheKey("", p.Palette.Version, p.Params.CoordDeriveVersion, map[string]any{"originX": p.OriginX, "originY": p.OriginY, "tileW": p.TileW, "tileH": p.TileH}))
	for _, e := range p.Palette.Entries {
		g.addNode(PaletteNodeID(e.Index), "palette_entry", e.ID, ComputeCacheKey("", p.Palette.Version, p.Params.CoordDeriveVersion, map[string]any{"color": e.Color, "gainPPM": e.GainPPM}))
	}
	imageByID := map[string]Image{}
	for _, im := range p.Images {
		imageByID[im.ID] = im
	}
	for _, b := range p.Blocks {
		contentHash := ""
		if im, ok := imageByID[b.ImageID]; ok {
			contentHash = BlockContentHash(im, b)
		}
		partitionKey := g.Nodes["geometry:partition"].CacheKey
		geoKey := ComputeCacheKey(contentHash, p.Palette.Version, p.Params.CoordDeriveVersion, map[string]any{"x": b.X, "y": b.Y, "w": b.W, "h": b.H, "col": b.Col, "row": b.Row}, partitionKey)
		g.addNode(GeometryBlockNodeID(b.ID), "geometry_coordinate", b.ID, geoKey, "geometry:partition")
		ditherID := DitherBlockNodeID(b.ID)
		ditherDeps := []string{}
		ditherValue := p.Params.GlobalDitherPPM
		if b.HasDitherOverride {
			ditherValue = b.DitherPPM
		} else {
			ditherDeps = append(ditherDeps, DitherGlobalNodeID())
		}
		ditherUpstreamKeys := []string{}
		if len(ditherDeps) > 0 {
			ditherUpstreamKeys = append(ditherUpstreamKeys, g.Nodes[DitherGlobalNodeID()].CacheKey)
		}
		ditherKey := ComputeCacheKey(contentHash, p.Palette.Version, p.Params.CoordDeriveVersion, map[string]any{"ditherPPM": ditherValue, "override": b.HasDitherOverride}, ditherUpstreamKeys...)
		g.addNode(ditherID, "dither_parameter", b.ID, ditherKey, ditherDeps...)
		loadKey := ComputeCacheKey(contentHash, p.Palette.Version, p.Params.CoordDeriveVersion, map[string]any{"blockID": b.ID, "x": b.X, "y": b.Y, "w": b.W, "h": b.H}, geoKey)
		g.addNode(LoadNodeID(b.ID), "stage_load", b.ID, loadKey, GeometryBlockNodeID(b.ID))

		paletteDeps := make([]string, 0, len(p.Palette.Entries)+2)
		paletteUpstreamKeys := make([]string, 0, len(p.Palette.Entries)+2)
		for _, e := range p.Palette.Entries {
			nodeID := PaletteNodeID(e.Index)
			paletteDeps = append(paletteDeps, nodeID)
			paletteUpstreamKeys = append(paletteUpstreamKeys, g.Nodes[nodeID].CacheKey)
		}
		paletteDeps = append(paletteDeps, LoadNodeID(b.ID), ditherID)
		paletteUpstreamKeys = append(paletteUpstreamKeys, loadKey, ditherKey)
		indexKey := ComputeCacheKey(contentHash, p.Palette.Version, p.Params.CoordDeriveVersion, map[string]any{"metric": p.Params.Metric, "quantScalePPM": p.Params.QuantScalePPM, "gainPPM": p.Params.GainPPM, "algo": p.Params.AlgoVersion, "paramVersion": p.Params.ParamVersion}, paletteUpstreamKeys...)
		g.addNode(IndexNodeID(b.ID), "stage_index", b.ID, indexKey, paletteDeps...)
		reconKey := ComputeCacheKey(contentHash, p.Palette.Version, p.Params.CoordDeriveVersion, map[string]any{"stage": "reconstruct", "algo": p.Params.AlgoVersion}, indexKey)
		g.addNode(ReconNodeID(b.ID), "stage_reconstruct", b.ID, reconKey, IndexNodeID(b.ID))
		verifyKey := ComputeCacheKey(contentHash, p.Palette.Version, p.Params.CoordDeriveVersion, map[string]any{"tolerance": p.Tolerance, "metric": p.Params.Metric}, reconKey)
		g.addNode(VerifyNodeID(b.ID), "stage_verify", b.ID, verifyKey, ReconNodeID(b.ID))
	}
	return g
}

func (g *DependencyGraph) Downstream(seeds ...string) []string {
	seen := map[string]bool{}
	queue := append([]string{}, seeds...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, next := range g.dependents[id] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// InvalidateBlocks turns exact semantic block impact into graph stage nodes.
// Geometry edits include load; palette/dither edits start at the index stage.
func (g *DependencyGraph) InvalidateBlocks(project *Project, affected map[string]string, includeLoad bool) InvalidationReport {
	report := InvalidationReport{BlockReasons: map[string]string{}}
	for _, b := range project.Blocks {
		reason, ok := affected[b.ID]
		if !ok {
			report.ReusedBlocks = append(report.ReusedBlocks, b.ID)
			continue
		}
		report.AffectedBlocks = append(report.AffectedBlocks, b.ID)
		report.BlockReasons[b.ID] = reason
		if includeLoad {
			report.Items = append(report.Items, InvalidationItem{NodeID: LoadNodeID(b.ID), BlockID: b.ID, Stage: StageLoad, Reason: reason})
		}
		report.Items = append(report.Items,
			InvalidationItem{NodeID: IndexNodeID(b.ID), BlockID: b.ID, Stage: StageQuantize, Reason: reason},
			InvalidationItem{NodeID: ReconNodeID(b.ID), BlockID: b.ID, Stage: StageReconstruct, Reason: reason},
			InvalidationItem{NodeID: VerifyNodeID(b.ID), BlockID: b.ID, Stage: StageVerify, Reason: reason},
		)
	}
	sort.Strings(report.AffectedBlocks)
	sort.Strings(report.ReusedBlocks)
	return report
}

func (r *InvalidationReport) ApplyToStateMachine(sm *StateMachine) error {
	for _, item := range r.Items {
		if err := sm.InvalidateBlock(item.BlockID, item.Stage, item.Reason); err != nil {
			return err
		}
	}
	return nil
}

// AnalyzePaletteEdit returns exactly the blocks whose old result can differ
// after newPalette[entryIndex] changes. Old worked pixels make the candidate
// test exact: if no local winner changes, error diffusion cannot diverge.
func AnalyzePaletteEdit(project *Project, quants map[string]QuantOutput, entryIndex int, newPalette Palette) map[string]string {
	affected := map[string]string{}
	for _, b := range project.Blocks {
		q, ok := quants[b.ID]
		if !ok {
			affected[b.ID] = "missing persisted quantization result"
			continue
		}
		newEntry, ok := newPalette.ByIndex(entryIndex)
		if !ok {
			affected[b.ID] = "edited palette entry no longer exists"
			continue
		}
		for local, oldIdx := range q.Indices {
			if oldIdx == entryIndex {
				affected[b.ID] = "block contains pixels selected from edited palette entry"
				break
			}
			if local >= len(q.WorkedPixels) {
				affected[b.ID] = "persisted worked pixels are incomplete"
				break
			}
			oldEntry, ok := project.Palette.ByIndex(oldIdx)
			if !ok {
				affected[b.ID] = "old palette index is invalid"
				break
			}
			worked := q.WorkedPixels[local]
			oldDist := ColorDistanceSq(worked, oldEntry.EffectiveColor(), project.Params.Metric)
			newDist := ColorDistanceSq(worked, newEntry.EffectiveColor(), project.Params.Metric)
			tieWins := entryIndex < oldIdx
			if newDist < oldDist || (newDist == oldDist && tieWins) {
				affected[b.ID] = "edited entry can become nearest for a worked pixel"
				break
			}
		}
	}
	return affected
}
