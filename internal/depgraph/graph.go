// Package depgraph models explicit dependencies and precise cache invalidation.
package depgraph

import (
	"fmt"
	"sort"

	"wardrobekernel/internal/hashx"
	"wardrobekernel/internal/model"
)

const (
	KindAttach     = "attach_point"
	KindPart       = "part"
	KindMaterial   = "material_slot"
	KindConstraint = "constraints"
	KindBatch      = "batch"
	KindBudget     = "budget"
	KindOffset     = "offset"
	KindCommit     = "commit"
)

// Node is one cacheable dependency vertex.
type Node struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	CacheKey    string   `json:"cache_key"`
	ContentHash string   `json:"content_hash,omitempty"`
	Version     string   `json:"version,omitempty"`
	Parents     []string `json:"parents,omitempty"`
	Children    []string `json:"children,omitempty"`
}

// Graph contains explicit upstream/downstream edges.
type Graph struct {
	Nodes map[string]*Node `json:"nodes"`
}

// PartHash returns the deterministic content hash for one part declaration.
func PartHash(part model.Part) string {
	return hashx.Of(struct {
		model.Part
	}{Part: part})
}

// ConstraintHash hashes all batching and arbitration parameters.
func ConstraintHash(c model.BatchConstraints) string {
	return hashx.Of(c)
}

// Enrich assigns content hashes, dependency IDs and cache keys to a plan.
func Enrich(project *model.Project, plan *model.Plan) *Graph {
	graph := &Graph{Nodes: map[string]*Node{}}
	constraintHash := ConstraintHash(project.Constraints)
	constraintNode := "constraints:" + project.ConstraintVersion
	graph.put(&Node{
		ID: constraintNode, Kind: KindConstraint, Version: project.ConstraintVersion,
		ContentHash: constraintHash,
		CacheKey:    hashx.Key("constraints", project.ConstraintVersion, constraintHash),
	})

	for _, attach := range project.AttachPoints {
		id := "attach:" + attach.ID
		graph.put(&Node{ID: id, Kind: KindAttach, Version: attach.ID, CacheKey: hashx.Key("attach", attach.ID)})
	}

	partByID := map[string]model.Part{}
	for _, part := range project.Parts {
		partByID[part.ID] = part
		partNode := "part:" + part.ID
		partHash := PartHash(part)
		graph.put(&Node{
			ID: partNode, Kind: KindPart, ContentHash: partHash,
			CacheKey: hashx.Key("part", partHash),
		})
		for _, slot := range part.MaterialSlots {
			materialNode := fmt.Sprintf("material:%s:%s", part.ID, slot.SlotID)
			graph.put(&Node{
				ID: materialNode, Kind: KindMaterial, Version: slot.MaterialVersion,
				ContentHash: hashx.Of(slot),
				CacheKey:    hashx.Key("material", slot.SlotID, slot.MaterialID, slot.MaterialVersion),
			})
			graph.addEdge(materialNode, partNode)
		}
		if attachID := "attach:" + part.AttachPointID; part.AttachPointID != "" {
			graph.addEdge(attachID, partNode)
		}
	}

	for i := range plan.Batches {
		batch := &plan.Batches[i]
		batch.AlgorithmVersion = project.AlgorithmVersion
		batch.ConstraintVersion = project.ConstraintVersion
		if batch.PartHashes == nil {
			batch.PartHashes = map[string]string{}
		}

		batchNode := fmt.Sprintf("batch:%d", batch.Number)
		budgetNode := fmt.Sprintf("budget:%d", batch.Number)
		offsetNode := fmt.Sprintf("offset:%d", batch.Number)
		commitNode := fmt.Sprintf("commit:%d", batch.Number)

		parents := []string{constraintNode}
		deps := []string{constraintNode}
		upstreamKeys := []string{graph.Nodes[constraintNode].CacheKey}

		for _, partID := range batch.PartIDs {
			part, ok := partByID[partID]
			if !ok {
				continue
			}
			partHash := PartHash(part)
			batch.PartHashes[partID] = partHash
			partNode := "part:" + partID
			parents = append(parents, partNode)
			deps = append(deps, partNode)
			upstreamKeys = append(upstreamKeys, graph.Nodes[partNode].CacheKey)

			attachNode := "attach:" + part.AttachPointID
			if graph.Nodes[attachNode] != nil {
				deps = append(deps, attachNode)
				parents = append(parents, attachNode)
				upstreamKeys = append(upstreamKeys, graph.Nodes[attachNode].CacheKey)
			}
			for _, slot := range part.MaterialSlots {
				materialNode := fmt.Sprintf("material:%s:%s", partID, slot.SlotID)
				if node := graph.Nodes[materialNode]; node != nil {
					deps = append(deps, materialNode)
					parents = append(parents, materialNode)
					upstreamKeys = append(upstreamKeys, node.CacheKey)
				}
			}
		}

		deps = uniqueSorted(deps)
		parents = uniqueSorted(parents)
		upstreamKeys = uniqueSorted(upstreamKeys)
		fingerprint := NewBatchFingerprint(batch)
		batch.ContentHash = hashx.Of(fingerprint)
		batch.CacheKey = hashx.Key(append([]string{
			"batch", project.AlgorithmVersion, project.ConstraintVersion, batch.ContentHash,
		}, upstreamKeys...)...)
		batch.Dependencies = deps

		graph.put(&Node{
			ID: batchNode, Kind: KindBatch, ContentHash: batch.ContentHash, CacheKey: batch.CacheKey,
			Parents: parents,
		})
		for _, parent := range parents {
			graph.addEdge(parent, batchNode)
		}

		remainingVertices := project.Constraints.MaxVertexBudget - batch.VertexUsed
		remainingIndices := project.Constraints.MaxIndexBudget - batch.IndexUsed
		graph.put(&Node{
			ID: budgetNode, Kind: KindBudget,
			ContentHash: hashx.Of(map[string]int{
				"vertex_used": batch.VertexUsed, "vertex_remaining": remainingVertices,
				"index_used": batch.IndexUsed, "index_remaining": remainingIndices,
			}),
			CacheKey: hashx.Key("budget", batch.CacheKey, fmt.Sprintf("%d,%d", remainingVertices, remainingIndices)),
			Parents:  []string{batchNode, constraintNode},
		})
		graph.addEdge(batchNode, budgetNode)
		graph.addEdge(constraintNode, budgetNode)

		offsetHash := hashx.Of(batch.Placements)
		offsetKey := hashx.Key("offset", batch.CacheKey, offsetHash)
		graph.put(&Node{
			ID: offsetNode, Kind: KindOffset, ContentHash: offsetHash, CacheKey: offsetKey,
			Parents: []string{batchNode},
		})
		graph.addEdge(batchNode, offsetNode)

		graph.put(&Node{
			ID: commitNode, Kind: KindCommit, CacheKey: hashx.Key("commit", offsetKey),
			Parents: []string{offsetNode},
		})
		graph.addEdge(offsetNode, commitNode)
	}
	return graph
}

// BatchFingerprint contains exactly the batch fields that define partition.
type BatchFingerprint struct {
	Number     int                    `json:"batch_number"`
	SkinKey    string                 `json:"skin_key"`
	PartIDs    []string               `json:"part_ids"`
	Slots      []model.SlotAllocation `json:"slots"`
	VertexUsed int                    `json:"vertex_used"`
	IndexUsed  int                    `json:"index_used"`
	Placements []model.PartPlacement  `json:"placements"`
}

func NewBatchFingerprint(b *model.Batch) BatchFingerprint {
	return BatchFingerprint{
		Number: b.Number, SkinKey: b.SkinKey, PartIDs: append([]string(nil), b.PartIDs...),
		Slots: append([]model.SlotAllocation(nil), b.Slots...), VertexUsed: b.VertexUsed,
		IndexUsed: b.IndexUsed, Placements: append([]model.PartPlacement(nil), b.Placements...),
	}
}

// ChangedParts compares content hashes of old/new part declarations.
func ChangedParts(oldProject, newProject *model.Project) []string {
	oldHashes := map[string]string{}
	for _, part := range oldProject.Parts {
		oldHashes[part.ID] = PartHash(part)
	}
	out := []string{}
	for _, part := range newProject.Parts {
		if oldHashes[part.ID] != PartHash(part) {
			out = append(out, part.ID)
		}
	}
	sort.Strings(out)
	return out
}

// Invalidation is the precise propagation result.
type Invalidation struct {
	ChangedNodes   []string `json:"changed_nodes"`
	InvalidNodes   []string `json:"invalid_nodes"`
	InvalidBatches []int    `json:"invalid_batches"`
	InvalidOffsets []int    `json:"invalid_offsets"`
	ReusedBatches  []int    `json:"reused_batches"`
}

// Propagate marks only graph descendants of changed part/material/constraint
// nodes. Batch numbers not reached are safe to reuse from persistent cache.
func (g *Graph) Propagate(changedPartIDs []string, constraintsChanged bool) Invalidation {
	changed := make([]string, 0)
	queue := make([]string, 0)
	seen := map[string]struct{}{}
	enqueue := func(id string) {
		if id == "" || g.Nodes[id] == nil {
			return
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			queue = append(queue, id)
			changed = append(changed, id)
		}
	}

	for _, partID := range changedPartIDs {
		enqueue("part:" + partID)
		for id := range g.Nodes {
			if node, ok := g.Nodes[id]; ok && node.Kind == KindMaterial {
				prefix := "material:" + partID + ":"
				if len(id) > len(prefix) && id[:len(prefix)] == prefix {
					enqueue(id)
				}
			}
		}
	}
	if constraintsChanged {
		for id, node := range g.Nodes {
			if node.Kind == KindConstraint {
				enqueue(id)
			}
		}
	}

	invalidNodeSet := map[string]struct{}{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		invalidNodeSet[id] = struct{}{}
		node := g.Nodes[id]
		if node == nil {
			continue
		}
		for _, child := range node.Children {
			if _, ok := invalidNodeSet[child]; !ok {
				invalidNodeSet[child] = struct{}{}
				queue = append(queue, child)
			}
		}
	}

	inv := Invalidation{}
	batchSet := map[int]struct{}{}
	offsetSet := map[int]struct{}{}
	for id := range invalidNodeSet {
		inv.InvalidNodes = append(inv.InvalidNodes, id)
		if n, err := fmt.Sscanf(id, "batch:%d", new(int)); err == nil && n == 1 {
			var num int
			fmt.Sscanf(id, "batch:%d", &num)
			batchSet[num] = struct{}{}
		}
		if n, err := fmt.Sscanf(id, "offset:%d", new(int)); err == nil && n == 1 {
			var num int
			fmt.Sscanf(id, "offset:%d", &num)
			offsetSet[num] = struct{}{}
		}
	}
	for num := range batchSet {
		inv.InvalidBatches = append(inv.InvalidBatches, num)
	}
	for num := range offsetSet {
		inv.InvalidOffsets = append(inv.InvalidOffsets, num)
	}
	for id, node := range g.Nodes {
		if node.Kind == KindBatch {
			num := batchNumberOf(id)
			if num > 0 {
				if _, bad := batchSet[num]; !bad {
					inv.ReusedBatches = append(inv.ReusedBatches, num)
				}
			}
		}
	}
	sort.Ints(inv.InvalidBatches)
	sort.Ints(inv.InvalidOffsets)
	sort.Ints(inv.ReusedBatches)
	sort.Strings(inv.InvalidNodes)
	sort.Strings(changed)
	inv.ChangedNodes = changed
	return inv
}

// TopologicalBatches returns invalid batch numbers in dependency order.
func (g *Graph) TopologicalBatches(nums []int) ([]int, error) {
	want := map[int]struct{}{}
	for _, n := range nums {
		want[n] = struct{}{}
	}
	indegree := map[string]int{}
	edges := map[string][]string{}
	for _, num := range nums {
		id := fmt.Sprintf("batch:%d", num)
		node := g.Nodes[id]
		if node == nil {
			return nil, &missingNodeError{id: id}
		}
		for _, parent := range node.Parents {
			if _, affected := want[batchNumberOf(parent)]; affected && batchNumberOf(parent) > 0 {
				edges[parent] = append(edges[parent], id)
				indegree[id]++
			}
		}
		if _, ok := indegree[id]; !ok {
			indegree[id] = 0
		}
	}
	ready := []string{}
	for id, deg := range indegree {
		if deg == 0 && isWantedBatch(id, want) {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	out := []int{}
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, batchNumberOf(id))
		children := append([]string(nil), edges[id]...)
		sort.Strings(children)
		for _, child := range children {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
				sort.Strings(ready)
			}
		}
	}
	if len(out) != len(nums) {
		return nil, &cycleError{}
	}
	return out, nil
}

type missingNodeError struct{ id string }

func (e *missingNodeError) Error() string { return "depgraph missing node " + e.id }

type cycleError struct{}

func (e *cycleError) Error() string { return "dependency cycle among invalid batches" }

func batchNumberOf(id string) int {
	var num int
	if _, err := fmt.Sscanf(id, "batch:%d", &num); err == nil {
		return num
	}
	return 0
}

func isWantedBatch(id string, want map[int]struct{}) bool {
	n := batchNumberOf(id)
	_, ok := want[n]
	return ok
}

func (g *Graph) put(node *Node) {
	g.Nodes[node.ID] = node
}

func (g *Graph) addEdge(parent, child string) {
	p := g.Nodes[parent]
	c := g.Nodes[child]
	if p == nil || c == nil {
		return
	}
	if !contains(p.Children, child) {
		p.Children = append(p.Children, child)
		sort.Strings(p.Children)
	}
	if !contains(c.Parents, parent) {
		c.Parents = append(c.Parents, parent)
		sort.Strings(c.Parents)
	}
}

func contains(xs []string, target string) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}

func uniqueSorted(xs []string) []string {
	set := map[string]struct{}{}
	for _, x := range xs {
		set[x] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}
