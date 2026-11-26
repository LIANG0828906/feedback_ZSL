// Package depgraph records band -> block-band quant -> block reconstruction dependencies.
package graph

import (
	"fmt"
	"sort"

	"msworkbench/internal/conv"
	"msworkbench/internal/domain"
	"msworkbench/internal/e"
	"msworkbench/internal/quant"
	"msworkbench/internal/sample"
)

const (
	KindSchema = "schema"
	KindParam  = "parameter"
	KindBand   = "band_metadata"
	KindInput  = "block_input"
	KindQuant  = "block_band_quant"
	KindRecon  = "block_reconstruction"
)

type Node struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Block        string   `json:"block,omitempty"`
	Band         string   `json:"band,omitempty"`
	CacheKey     string   `json:"cache_key"`
	ContentHash  string   `json:"content_hash,omitempty"`
	ParamVersion string   `json:"param_version"`
	Upstream     []string `json:"upstream_cache_keys"`
	Dirty        bool     `json:"dirty"`
}

type Invalidation struct {
	NodeID string `json:"node_id"`
	Kind   string `json:"kind"`
	Block  string `json:"block,omitempty"`
	Band   string `json:"band,omitempty"`
	Reason string `json:"reason"`
	Basis  string `json:"basis"`
}

type Reuse struct {
	NodeID string `json:"node_id"`
	Block  string `json:"block,omitempty"`
	Band   string `json:"band,omitempty"`
	Reason string `json:"reason"`
}

type Manifest struct {
	Edit         string         `json:"edit"`
	Invalidated  []Invalidation `json:"invalidated"`
	Reused       []Reuse        `json:"reused"`
	CacheKeyAlgo string         `json:"cache_key_algo"`
}

type Graph struct {
	Project    *domain.Project
	Sample     *sample.SampleSet
	Profile    domain.SampleScale
	Thresholds quant.Thresholds
	Nodes      map[string]*Node
	Dependents map[string][]string
	Active     map[string]bool
	Decisions  map[string]quant.Decision
}

func quantNodeID(block, band string) string {
	return fmt.Sprintf("quant:%s:%s", block, band)
}

func reconNodeID(block string) string { return "recon:" + block }
func bandNodeID(band string) string   { return "band:" + band }

func Build(p *domain.Project, set *sample.SampleSet, profile domain.SampleScale, thresholds quant.Thresholds) *Graph {
	thresholds = thresholds.Normalize()
	g := &Graph{
		Project: p, Sample: set, Profile: profile, Thresholds: thresholds,
		Nodes: map[string]*Node{}, Dependents: map[string][]string{},
		Active: map[string]bool{}, Decisions: map[string]quant.Decision{},
	}
	schemaNode := &Node{ID: "schema:" + p.SourceSchema, Kind: KindSchema, CacheKey: p.CanonicalHash(), ParamVersion: domain.ParamVersion}
	paramNode := &Node{ID: "param:adaptive", Kind: KindParam, ParamVersion: domain.ParamVersion, CacheKey: parameterKey(profile, thresholds, p.QuantScale)}
	g.Nodes[schemaNode.ID] = schemaNode
	g.Nodes[paramNode.ID] = paramNode

	for _, b := range p.Bands {
		n := &Node{ID: bandNodeID(b.ID), Kind: KindBand, Band: b.ID, ParamVersion: domain.ParamVersion, CacheKey: bandKey(b)}
		g.Nodes[n.ID] = n
	}

	summaryByBlock := map[string]*sample.BlockSummary{}
	for _, list := range set.Summaries[profile] {
		summaryByBlock[list.Block] = list
	}

	for _, coord := range p.Coords() {
		block := coord.String()
		contentHash := set.BlockHashes[block]
		inputID := "input:" + block
		g.Nodes[inputID] = &Node{ID: inputID, Kind: KindInput, Block: block, ContentHash: contentHash, CacheKey: contentHash}
		summary := summaryByBlock[block]
		obs := len(p.Bands) * p.BlockWidth * p.BlockHeight
		if summary != nil {
			obs = summary.ObservationCount
		}
		decision := quant.Select(profile, obs, thresholds)
		decision.Block = block
		g.Decisions[block] = decision

		reconUpstream := []string{}
		for bi, b := range p.Bands {
			id := quantNodeID(block, b.ID)
			active := blockBandActive(set, bi, coord, p)
			g.Active[id] = active
			upstreamIDs := []string{inputID, bandNodeID(b.ID), paramNode.ID, schemaNode.ID}
			upstreamKeys := []string{contentHash, g.Nodes[bandNodeID(b.ID)].CacheKey, paramNode.CacheKey, schemaNode.CacheKey}
			key := domain.HashJSON(map[string]any{
				"content_hash": contentHash, "band_id": b.ID,
				"param_version": domain.ParamVersion, "algo_version": domain.AlgorithmVersion,
				"decision": decision, "upstream_keys": upstreamKeys,
			})
			n := &Node{ID: id, Kind: KindQuant, Block: block, Band: b.ID, CacheKey: key, ContentHash: contentHash, ParamVersion: domain.ParamVersion, Upstream: upstreamKeys}
			g.Nodes[id] = n
			for _, up := range upstreamIDs {
				g.Dependents[up] = append(g.Dependents[up], id)
			}
			reconUpstream = append(reconUpstream, key)
		}
		rkey := domain.HashJSON(map[string]any{"block": block, "param_version": domain.ParamVersion, "algo_version": domain.AlgorithmVersion, "quant_keys": reconUpstream, "white": p.WhitePointBP})
		rn := &Node{ID: reconNodeID(block), Kind: KindRecon, Block: block, CacheKey: rkey, ParamVersion: domain.ParamVersion, Upstream: reconUpstream}
		g.Nodes[rn.ID] = rn
		for _, b := range p.Bands {
			qid := quantNodeID(block, b.ID)
			g.Dependents[qid] = append(g.Dependents[qid], rn.ID)
		}
	}
	return g
}

func parameterKey(profile domain.SampleScale, t quant.Thresholds, scale int64) string {
	return domain.HashJSON(map[string]any{"profile": profile, "thresholds": t, "quant_scale": scale, "param_version": domain.ParamVersion, "algo_version": domain.AlgorithmVersion})
}

func bandKey(b domain.Band) string {
	return domain.HashJSON(map[string]any{"id": b.ID, "gain_ppm": b.GainPPM, "response": b.Response, "center": b.CenterNM, "group_path": b.GroupPath})
}

func blockBandActive(set *sample.SampleSet, bandIndex int, coord domain.BlockCoord, p *domain.Project) bool {
	if bandIndex >= len(set.Radiance) {
		return false
	}
	indices, _, _ := p.BlockPixels(coord)
	for _, gi := range indices {
		if set.Radiance[bandIndex].Values[gi] != 0 {
			return true
		}
	}
	return false
}

func (g *Graph) BlockDirty(block string) bool {
	if n := g.Nodes[reconNodeID(block)]; n != nil && n.Dirty {
		return true
	}
	for _, b := range g.Project.Bands {
		if n := g.Nodes[quantNodeID(block, b.ID)]; n != nil && n.Dirty {
			return true
		}
	}
	return false
}

func (g *Graph) ClearDirty() {
	for _, n := range g.Nodes {
		n.Dirty = false
	}
}

// EditGain mutates the canonical gain and propagates only semantically active dependencies.
func (g *Graph) EditGain(bandID string, gainPPM int64) (*Manifest, error) {
	field := fmt.Sprintf("project.bands[%s].gain_ppm", bandID)
	if err := conv.ValidateGainPPM(field, gainPPM); err != nil {
		return nil, err
	}
	idx := g.Project.BandIndex(bandID)
	if idx < 0 {
		return nil, e.Param(field, "existing band id", "unknown band %s", bandID)
	}
	old := g.Project.Bands[idx].GainPPM
	g.Project.Bands[idx].GainPPM = gainPPM
	g.Project.Bands[idx].GainExplicit = true
	bandN := g.Nodes[bandNodeID(bandID)]
	bandN.CacheKey = bandKey(g.Project.Bands[idx])
	m := &Manifest{Edit: fmt.Sprintf("gain %s %d->%d ppm", bandID, old, gainPPM), CacheKeyAlgo: "sha256(content_hash|band_id|param_version|decision|upstream_cache_keys)"}
	g.propagateGain(bandN.ID, bandID, fmt.Sprintf("band gain changed %d->%d ppm", old, gainPPM), m)
	g.collectReuse(m)
	sortManifest(m)
	return m, nil
}

func (g *Graph) propagateGain(start, bandID, reason string, m *Manifest) {
	queue := []string{start}
	seen := map[string]bool{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		n := g.Nodes[id]
		if n != nil && id != start {
			n.Dirty = true
			m.Invalidated = append(m.Invalidated, Invalidation{NodeID: id, Kind: n.Kind, Block: n.Block, Band: n.Band, Reason: reason, Basis: "direct dependency edge"})
		}
		for _, dep := range g.Dependents[id] {
			dn := g.Nodes[dep]
			if dn == nil {
				continue
			}
			if dn.Kind == KindQuant && !g.Active[dep] {
				m.Reused = append(m.Reused, Reuse{NodeID: dep, Block: dn.Block, Band: dn.Band, Reason: "block-band radiance is all zero; integer gain is invariant at q=0"})
				continue
			}
			if dn.Kind == KindRecon {
				queue = append(queue, dep)
				continue
			}
			queue = append(queue, dep)
		}
	}
}

// EditProfile changes sample tier and invalidates only blocks whose selected method changed.
func (g *Graph) EditProfile(profile domain.SampleScale, thresholds quant.Thresholds) (*Manifest, error) {
	thresholds = thresholds.Normalize()
	m := &Manifest{Edit: fmt.Sprintf("sample profile %s->%s", g.Profile, profile), CacheKeyAlgo: "sha256(content_hash|band_id|param_version|decision|upstream_cache_keys)"}
	summaryByBlock := map[string]*sample.BlockSummary{}
	for _, list := range g.Sample.Summaries[profile] {
		summaryByBlock[list.Block] = list
	}
	paramN := g.Nodes["param:adaptive"]
	g.Profile = profile
	g.Thresholds = thresholds
	if paramN != nil {
		paramN.CacheKey = parameterKey(profile, thresholds, g.Project.QuantScale)
	}
	for _, coord := range g.Project.Coords() {
		block := coord.String()
		old := g.Decisions[block]
		summary := summaryByBlock[block]
		obs := old.ObservationCount
		if summary != nil {
			obs = summary.ObservationCount
		}
		next := quant.Select(profile, obs, thresholds)
		next.Block = block
		g.Decisions[block] = next
		if old.Method == next.Method && old.SparseThreshold == next.SparseThreshold && old.MediumThreshold == next.MediumThreshold {
			for _, b := range g.Project.Bands {
				id := quantNodeID(block, b.ID)
				m.Reused = append(m.Reused, Reuse{NodeID: id, Block: block, Band: b.ID, Reason: "selected method and injected thresholds unchanged"})
			}
			m.Reused = append(m.Reused, Reuse{NodeID: reconNodeID(block), Block: block, Reason: "upstream quant keys remain valid"})
			continue
		}
		reason := fmt.Sprintf("method %s->%s; observations=%d thresholds=%d/%d", old.Method, next.Method, obs, next.SparseThreshold, next.MediumThreshold)
		for _, b := range g.Project.Bands {
			id := quantNodeID(block, b.ID)
			if n := g.Nodes[id]; n != nil {
				n.Dirty = true
				m.Invalidated = append(m.Invalidated, Invalidation{NodeID: id, Kind: KindQuant, Block: block, Band: b.ID, Reason: reason, Basis: "sample tier/threshold parameter dependency"})
			}
		}
		if n := g.Nodes[reconNodeID(block)]; n != nil {
			n.Dirty = true
			m.Invalidated = append(m.Invalidated, Invalidation{NodeID: n.ID, Kind: KindRecon, Block: block, Reason: reason, Basis: "all block-band quant nodes are upstream"})
		}
	}
	g.collectReuse(m)
	sortManifest(m)
	return m, nil
}

// SwitchVersion reuses all block results when canonical semantics are unchanged.
func (g *Graph) SwitchVersion(next *domain.Project) *Manifest {
	m := &Manifest{Edit: fmt.Sprintf("schema switch %s->%s", g.Project.SourceSchema, next.SourceSchema), CacheKeyAlgo: "sha256(content_hash|band_id|param_version|decision|upstream_cache_keys)"}
	oldHash := g.Project.CanonicalHash()
	newHash := next.CanonicalHash()
	if oldHash == newHash {
		schemaN := g.Nodes["schema:"+g.Project.SourceSchema]
		if schemaN != nil {
			schemaN.Dirty = true
			m.Invalidated = append(m.Invalidated, Invalidation{NodeID: schemaN.ID, Kind: KindSchema, Reason: "source version/provenance changed", Basis: "source_version_dependency"})
		}
		newSchema := &Node{ID: "schema:" + next.SourceSchema, Kind: KindSchema, CacheKey: newHash, ParamVersion: domain.ParamVersion, Dirty: true}
		g.Nodes[newSchema.ID] = newSchema
		g.Project = next
		for _, coord := range next.Coords() {
			block := coord.String()
			m.Reused = append(m.Reused, Reuse{NodeID: reconNodeID(block), Block: block, Reason: "canonical semantic hash unchanged across schema versions"})
		}
		sortManifest(m)
		return m
	}
	g.Project = next
	for _, b := range next.Bands {
		id := bandNodeID(b.ID)
		if n := g.Nodes[id]; n != nil {
			n.CacheKey = bandKey(b)
			n.Dirty = true
			g.propagateGain(id, b.ID, "canonical band semantics changed after version conversion", m)
		}
	}
	g.collectReuse(m)
	sortManifest(m)
	return m
}

func (g *Graph) InvalidateContent(coord domain.BlockCoord, reason string) *Manifest {
	block := coord.String()
	m := &Manifest{Edit: "block input edit " + block, CacheKeyAlgo: "sha256(content_hash|band_id|param_version|decision|upstream_cache_keys)"}
	inputID := "input:" + block
	if n := g.Nodes[inputID]; n != nil {
		n.Dirty = true
		m.Invalidated = append(m.Invalidated, Invalidation{NodeID: inputID, Kind: KindInput, Block: block, Reason: reason, Basis: "block content hash changed"})
	}
	for _, b := range g.Project.Bands {
		id := quantNodeID(block, b.ID)
		if n := g.Nodes[id]; n != nil {
			n.Dirty = true
			m.Invalidated = append(m.Invalidated, Invalidation{NodeID: id, Kind: KindQuant, Block: block, Band: b.ID, Reason: reason, Basis: "block input content hash dependency"})
		}
	}
	if n := g.Nodes[reconNodeID(block)]; n != nil {
		n.Dirty = true
		m.Invalidated = append(m.Invalidated, Invalidation{NodeID: n.ID, Kind: KindRecon, Block: block, Reason: reason, Basis: "quant upstream changed"})
	}
	g.collectReuse(m)
	sortManifest(m)
	return m
}

func (g *Graph) collectReuse(m *Manifest) {
	invalid := map[string]bool{}
	for _, x := range m.Invalidated {
		invalid[x.NodeID] = true
	}
	for id, n := range g.Nodes {
		if invalid[id] || n.Kind == KindBand || n.Kind == KindParam || n.Kind == KindSchema || n.Kind == KindInput {
			continue
		}
		if n.Kind == KindQuant {
			m.Reused = append(m.Reused, Reuse{NodeID: id, Block: n.Block, Band: n.Band, Reason: "dependency key unchanged; persistent cache entry is valid"})
		}
		if n.Kind == KindRecon {
			m.Reused = append(m.Reused, Reuse{NodeID: id, Block: n.Block, Reason: "no dirty upstream quant node"})
		}
	}
}

func sortManifest(m *Manifest) {
	seenReuse := map[string]bool{}
	reused := m.Reused[:0]
	for _, r := range m.Reused {
		if seenReuse[r.NodeID] {
			continue
		}
		seenReuse[r.NodeID] = true
		reused = append(reused, r)
	}
	m.Reused = reused
	sort.SliceStable(m.Invalidated, func(i, j int) bool {
		if m.Invalidated[i].Block != m.Invalidated[j].Block {
			return blockLess(m.Invalidated[i].Block, m.Invalidated[j].Block)
		}
		if m.Invalidated[i].Band != m.Invalidated[j].Band {
			return m.Invalidated[i].Band < m.Invalidated[j].Band
		}
		return m.Invalidated[i].NodeID < m.Invalidated[j].NodeID
	})
	sort.SliceStable(m.Reused, func(i, j int) bool {
		if m.Reused[i].Block != m.Reused[j].Block {
			return blockLess(m.Reused[i].Block, m.Reused[j].Block)
		}
		if m.Reused[i].Band != m.Reused[j].Band {
			return m.Reused[i].Band < m.Reused[j].Band
		}
		return m.Reused[i].NodeID < m.Reused[j].NodeID
	})
}

func blockLess(a, b string) bool {
	ca, err1 := domain.ParseBlockCoord(a)
	cb, err2 := domain.ParseBlockCoord(b)
	if err1 != nil || err2 != nil {
		return a < b
	}
	if ca.BY != cb.BY {
		return ca.BY < cb.BY
	}
	return ca.BX < cb.BX
}
