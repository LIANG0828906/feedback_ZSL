package engine

import (
	"fmt"
	"sort"

	"presentkernel/internal/domain"
	"presentkernel/internal/quant"
)

type Node struct {
	ID             string   `json:"id"`
	Kind           string   `json:"kind"`
	DialogueID     string   `json:"dialogue_id,omitempty"`
	PartitionID    string   `json:"partition_id,omitempty"`
	PartitionIndex int      `json:"partition_index,omitempty"`
	Hash           string   `json:"hash"`
	Dependencies   []string `json:"dependencies"`
	Downstream     []string `json:"downstream"`
}

type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type DependencyGraph struct {
	ProjectID string           `json:"project_id"`
	FPS       int              `json:"fps"`
	Nodes     map[string]*Node `json:"nodes"`
	Edges     []Edge           `json:"edges"`
}

type Edit struct {
	Kind           string `json:"kind"`
	DialogueID     string `json:"dialogue_id,omitempty"`
	PartitionIndex int    `json:"partition_index,omitempty"`
	CharIndex      int    `json:"char_index,omitempty"`
	PhonemeIndex   int    `json:"phoneme_index,omitempty"`
	Value          int64  `json:"value,omitempty"`
}

type InvalidationPlan struct {
	Edit         Edit                  `json:"edit"`
	ChangedNodes []string              `json:"changed_nodes"`
	Invalid      []domain.PartitionRef `json:"invalid"`
	Reused       []domain.PartitionRef `json:"reused"`
	Reasons      map[string]string     `json:"reasons"`
}

func BuildGraph(p *domain.Project, fps int) *DependencyGraph {
	if fps <= 0 {
		fps = p.TargetFPS
	}
	g := &DependencyGraph{ProjectID: p.ProjectID, FPS: fps, Nodes: map[string]*Node{}}
	g.addNode(&Node{ID: "quant-rule", Kind: "quantization_rule", Hash: quant.ParamVersion + ":" + p.RoundingMode})
	fpsID := fmt.Sprintf("fps:%d", fps)
	g.addNode(&Node{ID: fpsID, Kind: "frame_rate", Hash: FPSKey(fps), Dependencies: []string{"quant-rule"}})

	for di := range p.Dialogues {
		d := p.Dialogues[di]
		for si := range d.Segments {
			s := d.Segments[si]
			pid := domain.MakePartitionID(d.ID, s.Index)
			content := ContentHash(p, di, si)
			durationID := pid + ":duration"
			weightsID := pid + ":char-weights"
			boundaryID := pid + ":phoneme-boundaries"
			charID := pid + ":char-frames"
			lipID := pid + ":lip-frames"
			transitionID := pid + ":transitions"
			resultID := pid + ":result"
			ref := domain.PartitionRef{DialogueID: d.ID, PartitionID: pid, PartitionIndex: s.Index}
			_ = ref
			g.addNode(&Node{ID: durationID, Kind: "segment_duration", DialogueID: d.ID, PartitionID: pid, PartitionIndex: s.Index, Hash: content[:16]})
			g.addNode(&Node{ID: weightsID, Kind: "char_weights", DialogueID: d.ID, PartitionID: pid, PartitionIndex: s.Index, Hash: content[16:32]})
			g.addNode(&Node{ID: boundaryID, Kind: "phoneme_boundaries", DialogueID: d.ID, PartitionID: pid, PartitionIndex: s.Index, Hash: content[32:48]})
			g.addNode(&Node{ID: charID, Kind: "downstream_char_intervals", DialogueID: d.ID, PartitionID: pid, PartitionIndex: s.Index, Hash: content, Dependencies: []string{durationID, weightsID, boundaryID, fpsID, "quant-rule"}})
			g.addNode(&Node{ID: lipID, Kind: "downstream_lip_frames", DialogueID: d.ID, PartitionID: pid, PartitionIndex: s.Index, Hash: content, Dependencies: []string{boundaryID, fpsID, "quant-rule"}})
			g.addNode(&Node{ID: transitionID, Kind: "downstream_transitions", DialogueID: d.ID, PartitionID: pid, PartitionIndex: s.Index, Hash: content, Dependencies: []string{lipID, boundaryID, fpsID}})
			g.addNode(&Node{ID: resultID, Kind: "partition_result", DialogueID: d.ID, PartitionID: pid, PartitionIndex: s.Index, Hash: content, Dependencies: []string{charID, lipID, transitionID}})
			g.linkDependencies()
		}
	}
	g.linkDependencies()
	return g
}

func (g *DependencyGraph) addNode(n *Node) {
	g.Nodes[n.ID] = n
}

func (g *DependencyGraph) linkDependencies() {
	g.Edges = nil
	for _, n := range g.Nodes {
		n.Downstream = nil
	}
	for _, n := range g.Nodes {
		for _, dep := range n.Dependencies {
			if parent, ok := g.Nodes[dep]; ok {
				parent.Downstream = append(parent.Downstream, n.ID)
				g.Edges = append(g.Edges, Edge{From: dep, To: n.ID, Kind: "depends_on"})
			}
		}
	}
	for _, n := range g.Nodes {
		sort.Strings(n.Downstream)
	}
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		return g.Edges[i].To < g.Edges[j].To
	})
}

func (g *DependencyGraph) affectedPartitionIDs(start ...string) map[string]domain.PartitionRef {
	seen := map[string]bool{}
	queue := append([]string{}, start...)
	out := map[string]domain.PartitionRef{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		n, ok := g.Nodes[id]
		if !ok {
			continue
		}
		if n.Kind == "partition_result" {
			out[n.PartitionID] = domain.PartitionRef{DialogueID: n.DialogueID, PartitionID: n.PartitionID, PartitionIndex: n.PartitionIndex}
		}
		queue = append(queue, n.Downstream...)
	}
	return out
}

func (g *DependencyGraph) PlanEdit(p *domain.Project, edit Edit) *InvalidationPlan {
	plan := &InvalidationPlan{Edit: edit, Reasons: map[string]string{}}
	start := []string{}
	switch edit.Kind {
	case "target_fps", "frame_rate":
		start = []string{fmt.Sprintf("fps:%d", p.TargetFPS)}
		plan.ChangedNodes = start
	case "segment_duration", "duration":
		pid := domain.MakePartitionID(edit.DialogueID, edit.PartitionIndex)
		id := pid + ":duration"
		start = []string{id}
		plan.ChangedNodes = start
	case "char_weight", "weight":
		pid := domain.MakePartitionID(edit.DialogueID, edit.PartitionIndex)
		id := pid + ":char-weights"
		start = []string{id}
		plan.ChangedNodes = start
	case "phoneme_boundary", "boundary":
		pid := domain.MakePartitionID(edit.DialogueID, edit.PartitionIndex)
		id := pid + ":phoneme-boundaries"
		start = []string{id}
		plan.ChangedNodes = start
	default:
		start = []string{"quant-rule"}
		plan.ChangedNodes = start
	}
	affected := g.affectedPartitionIDs(start...)
	all := g.AllPartitionRefs()
	for _, ref := range all {
		if bad, ok := affected[ref.PartitionID]; ok {
			plan.Invalid = append(plan.Invalid, bad)
			plan.Reasons[ref.PartitionID] = "reachable from " + fmt.Sprint(plan.ChangedNodes)
		} else {
			plan.Reused = append(plan.Reused, ref)
		}
	}
	sortRefs(plan.Invalid)
	sortRefs(plan.Reused)
	return plan
}

func (g *DependencyGraph) AllPartitionRefs() []domain.PartitionRef {
	out := []domain.PartitionRef{}
	for _, n := range g.Nodes {
		if n.Kind == "partition_result" {
			out = append(out, domain.PartitionRef{DialogueID: n.DialogueID, PartitionID: n.PartitionID, PartitionIndex: n.PartitionIndex})
		}
	}
	sortRefs(out)
	return out
}

func sortRefs(refs []domain.PartitionRef) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].PartitionID < refs[j].PartitionID })
}

func FindDialogueIndex(p *domain.Project, dialogueID string) int {
	for i := range p.Dialogues {
		if p.Dialogues[i].ID == dialogueID {
			return i
		}
	}
	return -1
}

func ApplyEdit(p *domain.Project, edit Edit) error {
	di := FindDialogueIndex(p, edit.DialogueID)
	switch edit.Kind {
	case "target_fps", "frame_rate":
		if edit.Value <= 0 {
			return domain.SchemaError("target_fps", "fps must be positive")
		}
		p.TargetFPS = int(edit.Value)
		return nil
	case "segment_duration", "duration", "char_weight", "weight", "phoneme_boundary", "boundary":
		if di < 0 || edit.PartitionIndex < 0 || edit.PartitionIndex >= len(p.Dialogues[di].Segments) {
			return domain.SchemaError("partition", "dialogue/partition not found: %s P%d", edit.DialogueID, edit.PartitionIndex)
		}
		si := edit.PartitionIndex
		seg := &p.Dialogues[di].Segments[si]
		switch edit.Kind {
		case "segment_duration", "duration":
			frames := int(edit.Value)
			if frames < len([]rune(seg.Text)) {
				return domain.ReadError(nil, "edited duration has fewer frames than characters")
			}
			seg.DurationFrames = frames
			if p.SampleRateHz > 0 && p.TargetFPS > 0 {
				seg.DurationSamples = int64(frames) * int64(p.SampleRateHz) / int64(p.TargetFPS)
			}
		case "char_weight", "weight":
			if edit.CharIndex < 0 || edit.CharIndex >= len(seg.CharWeights) {
				return domain.SchemaError("char_index", "char index out of range")
			}
			seg.CharWeights[edit.CharIndex] = int(edit.Value)
		case "phoneme_boundary", "boundary":
			if edit.PhonemeIndex < 0 || edit.PhonemeIndex >= len(seg.Phonemes) {
				return domain.SchemaError("phoneme_index", "phoneme index out of range")
			}
			ph := &seg.Phonemes[edit.PhonemeIndex]
			delta := edit.Value
			ph.EndSample += delta
			ph.EndFrame = int(ph.EndSample * int64(p.TargetFPS) / int64(p.SampleRateHz))
		}
	}
	return nil
}

func PartitionCacheKey(p *domain.Project, dialogueIndex, segmentIndex, fps int) (contentHash, upstream, cacheKey string) {
	contentHash = ContentHash(p, dialogueIndex, segmentIndex)
	fpsKey := FPSKey(fps)
	upstream = UpstreamKey(contentHash, fpsKey)
	cacheKey = CacheKey(contentHash, upstream)
	return
}

type MemoryCache struct {
	ByCacheKey   map[string]*domain.PartitionResult `json:"by_cache_key"`
	LatestByPart map[string]string                  `json:"latest_by_partition"`
}

func NewMemoryCache() *MemoryCache {
	return &MemoryCache{ByCacheKey: map[string]*domain.PartitionResult{}, LatestByPart: map[string]string{}}
}

func (c *MemoryCache) Get(cacheKey string) *domain.PartitionResult {
	if c == nil {
		return nil
	}
	return c.ByCacheKey[cacheKey]
}

func (c *MemoryCache) Put(r *domain.PartitionResult) {
	c.ByCacheKey[r.CacheKey] = r
	c.LatestByPart[r.PartitionID] = r.CacheKey
}
