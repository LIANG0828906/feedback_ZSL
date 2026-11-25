package dependency

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"lipsync.local/prototype/pkg/domain"
)

type NodeKind string

const (
	NodeRoot     NodeKind = "root"
	NodeBoundary NodeKind = "phoneme_boundary"
	NodeWeight   NodeKind = "duration_weight"
	NodeAffinity NodeKind = "transition_affinity"
	NodeQuant    NodeKind = "quantized_phoneme"
	NodePackage  NodeKind = "lip_package"
	NodeDispatch NodeKind = "frame_dispatch"
)

type Node struct {
	ID         string   `json:"id"`
	Kind       NodeKind `json:"kind"`
	DialogueID string   `json:"dialogue_id"`
	PhonemeID  string   `json:"phoneme_id,omitempty"`
	PhonemeIDs []string `json:"phoneme_ids,omitempty"`
	PackageID  string   `json:"package_id,omitempty"`
	Field      string   `json:"field,omitempty"`
	Upstream   []string `json:"upstream"`
	CacheKey   string   `json:"cache_key"`
	Payload    []byte   `json:"payload,omitempty"`
}

type Artifact struct {
	Dialogue  domain.Dialogue
	Quantized domain.QuantizeResult
	Lip       domain.LipResult
	Dispatch  domain.DispatchResult
}

type Graph struct {
	Nodes    map[string]*Node `json:"nodes"`
	Packages map[string]Node  `json:"packages"`
}

type Invalidation struct {
	InvalidatedNodes            []string          `json:"invalidated_nodes"`
	InvalidatedPackages         []string          `json:"invalidated_packages"`
	InvalidatedDispatchNodes    []string          `json:"invalidated_dispatch_nodes"`
	InvalidatedDispatchPackages []string          `json:"invalidated_dispatch_packages"`
	ReusedPackages              []string          `json:"reused_packages"`
	Reasons                     map[string]string `json:"reasons"`
}

// Build constructs an explicit dependency DAG. Every cache key combines the
// dialogue's static content hash, the node's phoneme/package identity, the
// recipe parameter version and all sorted upstream cache keys.
func Build(project *domain.Project, artifacts map[string]Artifact) (*Graph, error) {
	g := &Graph{
		Nodes:    make(map[string]*Node),
		Packages: make(map[string]Node),
	}
	if project == nil {
		return g, &domain.Error{Code: domain.ErrSchema, Path: "$", Reason: "project is nil"}
	}
	staticHashes := make(map[string]string, len(project.Dialogues))
	dialogues := make(map[string]domain.Dialogue, len(project.Dialogues))
	for _, d := range project.Dialogues {
		staticHashes[d.ID] = d.StaticContentHash()
		dialogues[d.ID] = d
	}

	for _, dialogueID := range sortedKeys(artifacts) {
		art := artifacts[dialogueID]
		d, ok := dialogues[dialogueID]
		if !ok {
			return nil, &domain.Error{Code: domain.ErrSchema, DialogueID: dialogueID, Reason: "artifact without dialogue"}
		}
		rootID := rootNodeID(dialogueID)
		root := &Node{
			ID: rootID, Kind: NodeRoot, DialogueID: dialogueID, Field: "recipe",
			Payload: mustJSON(struct {
				StaticHash string        `json:"static_hash"`
				Recipe     domain.Recipe `json:"recipe"`
			}{staticHashes[dialogueID], project.Recipe}),
		}
		root.CacheKey = cacheKey(g, project.Recipe.Version, root, staticHashes[dialogueID])
		g.Nodes[root.ID] = root

		boundaryIDs := make(map[string]string)
		weightIDs := make(map[string]string)
		quantIDs := make(map[string]string)
		for _, ph := range d.Phonemes {
			bID := fmt.Sprintf("%s/boundary/%s", dialogueID, ph.ID)
			boundaryIDs[ph.ID] = bID
			b := &Node{
				ID: bID, Kind: NodeBoundary, DialogueID: dialogueID,
				PhonemeID: ph.ID, Field: "start_sample,end_sample",
				Upstream: []string{rootID},
				Payload:  mustJSON([2]int64{ph.StartSample, ph.EndSample}),
			}
			b.CacheKey = cacheKey(g, project.Recipe.Version, b, staticHashes[dialogueID])
			g.Nodes[b.ID] = b

			wID := fmt.Sprintf("%s/weight/%s", dialogueID, ph.ID)
			weightIDs[ph.ID] = wID
			w := &Node{
				ID: wID, Kind: NodeWeight, DialogueID: dialogueID,
				PhonemeID: ph.ID, Field: "weight_permille",
				Upstream: []string{rootID},
				Payload:  mustJSON(ph.WeightPermille),
			}
			w.CacheKey = cacheKey(g, project.Recipe.Version, w, staticHashes[dialogueID])
			g.Nodes[w.ID] = w
		}

		quantByID := make(map[string]domain.QuantizedPhoneme, len(art.Quantized.Phonemes))
		for _, q := range art.Quantized.Phonemes {
			quantByID[q.PhonemeID] = q
			qID := fmt.Sprintf("%s/quant/%s", dialogueID, q.PhonemeID)
			quantIDs[q.PhonemeID] = qID
			upstream := []string{boundaryIDs[q.PhonemeID], weightIDs[q.PhonemeID]}
			n := &Node{
				ID: qID, Kind: NodeQuant, DialogueID: dialogueID,
				PhonemeID: q.PhonemeID, Field: "start_frame,end_frame,weight_basis_points",
				Upstream: upstream,
				Payload: mustJSON(struct {
					Start int   `json:"start_frame"`
					End   int   `json:"end_frame"`
					Basis int64 `json:"weight_basis_points"`
				}{q.StartFrame, q.EndFrame, q.WeightBasis}),
			}
			n.CacheKey = cacheKey(g, project.Recipe.Version, n, staticHashes[dialogueID])
			g.Nodes[n.ID] = n
		}

		affinityIDs := make(map[string]string)
		for i := 0; i+1 < len(d.Phonemes); i++ {
			left, right := d.Phonemes[i], d.Phonemes[i+1]
			aID := affinityNodeID(dialogueID, left.ID, right.ID)
			affinityIDs[left.ID+">"+right.ID] = aID
			n := &Node{
				ID: aID, Kind: NodeAffinity, DialogueID: dialogueID,
				PhonemeID: left.ID, PhonemeIDs: []string{left.ID, right.ID},
				Field: "affinity_to_next", Upstream: []string{rootID},
				Payload: mustJSON(left.AffinityToNext),
			}
			n.CacheKey = cacheKey(g, project.Recipe.Version, n, staticHashes[dialogueID])
			g.Nodes[n.ID] = n
		}

		for _, pkg := range art.Lip.Packages {
			upstreamSet := make(map[string]struct{})
			for _, id := range pkg.PhonemeIDs {
				upstreamSet[quantIDs[id]] = struct{}{}
			}
			for i := 0; i+1 < len(pkg.PhonemeIDs); i++ {
				key := pkg.PhonemeIDs[i] + ">" + pkg.PhonemeIDs[i+1]
				if id := affinityIDs[key]; id != "" {
					upstreamSet[id] = struct{}{}
				}
			}
			if idx := phonemeIndex(d.Phonemes, pkg.PhonemeIDs[len(pkg.PhonemeIDs)-1]); idx >= 0 && idx+1 < len(d.Phonemes) {
				key := d.Phonemes[idx].ID + ">" + d.Phonemes[idx+1].ID
				if id := affinityIDs[key]; id != "" {
					upstreamSet[id] = struct{}{}
				}
			}
			upstream := setKeys(upstreamSet)
			pID := fmt.Sprintf("%s/package/%s", dialogueID, pkg.ID)
			n := &Node{
				ID: pID, Kind: NodePackage, DialogueID: dialogueID,
				PhonemeID: pkg.PhonemeIDs[0], PhonemeIDs: append([]string(nil), pkg.PhonemeIDs...),
				PackageID: pkg.ID, Field: "package_partition",
				Upstream: upstream, Payload: mustJSON(pkg),
			}
			n.CacheKey = cacheKey(g, project.Recipe.Version, n, staticHashes[dialogueID])
			g.Nodes[n.ID] = n
			g.Packages[pkg.ID] = *n
		}

		for _, pkg := range art.Lip.Packages {
			pID := fmt.Sprintf("%s/package/%s", dialogueID, pkg.ID)
			dispID := fmt.Sprintf("%s/dispatch/%s", dialogueID, pkg.ID)
			payload := mustJSON(struct {
				DispatchFrame int                    `json:"dispatch_frame"`
				Accepted      bool                   `json:"accepted"`
				Deferred      bool                   `json:"deferred"`
				Rejected      bool                   `json:"rejected"`
				Events        []domain.DispatchEvent `json:"events"`
			}{
				DispatchFrame: art.Dispatch.DispatchFrame[pkg.ID],
				Accepted:      contains(art.Dispatch.Accepted, pkg.ID),
				Deferred:      contains(art.Dispatch.Deferred, pkg.ID),
				Rejected:      contains(art.Dispatch.Rejected, pkg.ID),
				Events:        eventsFor(art.Dispatch.Events, pkg.ID),
			})
			n := &Node{
				ID: dispID, Kind: NodeDispatch, DialogueID: dialogueID,
				PhonemeID: pkg.PhonemeIDs[0], PhonemeIDs: append([]string(nil), pkg.PhonemeIDs...),
				PackageID: pkg.ID, Field: "frame_dispatch",
				Upstream: []string{pID, rootID}, Payload: payload,
			}
			n.CacheKey = cacheKey(g, project.Recipe.Version, n, staticHashes[dialogueID])
			g.Nodes[n.ID] = n
		}
	}
	return g, nil
}

func Diff(oldGraph, newGraph *Graph) Invalidation {
	report := Invalidation{Reasons: make(map[string]string)}
	if oldGraph == nil {
		oldGraph = &Graph{Nodes: map[string]*Node{}}
	}
	if newGraph == nil {
		newGraph = &Graph{Nodes: map[string]*Node{}}
	}
	changed := make(map[string]struct{})
	for id, node := range newGraph.Nodes {
		old, ok := oldGraph.Nodes[id]
		if !ok {
			changed[id] = struct{}{}
			report.Reasons[id] = "node_added"
			continue
		}
		if old.CacheKey != node.CacheKey {
			changed[id] = struct{}{}
			report.Reasons[id] = "cache_key_changed"
		}
	}
	for id := range oldGraph.Nodes {
		if _, ok := newGraph.Nodes[id]; !ok {
			changed[id] = struct{}{}
			report.Reasons[id] = "node_removed"
		}
	}
	for id := range changed {
		report.InvalidatedNodes = append(report.InvalidatedNodes, id)
	}
	sort.Strings(report.InvalidatedNodes)

	invalidPackages := make(map[string]struct{})
	invalidDispatchNodes := make(map[string]struct{})
	invalidDispatchPackages := make(map[string]struct{})
	reusedPackages := make(map[string]struct{})
	for id, node := range newGraph.Nodes {
		_, bad := changed[id]
		switch node.Kind {
		case NodePackage:
			if bad {
				invalidPackages[node.PackageID] = struct{}{}
				continue
			}
			if old, ok := oldGraph.Nodes[id]; ok && old.CacheKey == node.CacheKey {
				reusedPackages[node.PackageID] = struct{}{}
			}
		case NodeDispatch:
			if bad {
				invalidDispatchNodes[id] = struct{}{}
				invalidDispatchPackages[node.PackageID] = struct{}{}
			}
		}
	}
	for id, node := range oldGraph.Nodes {
		if node.Kind == NodePackage {
			if _, exists := newGraph.Nodes[id]; !exists {
				invalidPackages[node.PackageID] = struct{}{}
			}
		} else if node.Kind == NodeDispatch {
			if _, exists := newGraph.Nodes[id]; !exists {
				invalidDispatchNodes[id] = struct{}{}
				invalidDispatchPackages[node.PackageID] = struct{}{}
			}
		}
	}
	for id := range invalidPackages {
		report.InvalidatedPackages = append(report.InvalidatedPackages, id)
	}
	for id := range reusedPackages {
		if _, bad := invalidPackages[id]; !bad {
			report.ReusedPackages = append(report.ReusedPackages, id)
		}
	}
	for id := range invalidDispatchNodes {
		report.InvalidatedDispatchNodes = append(report.InvalidatedDispatchNodes, id)
	}
	for id := range invalidDispatchPackages {
		report.InvalidatedDispatchPackages = append(report.InvalidatedDispatchPackages, id)
	}
	sort.Strings(report.InvalidatedPackages)
	sort.Strings(report.InvalidatedDispatchNodes)
	sort.Strings(report.InvalidatedDispatchPackages)
	sort.Strings(report.ReusedPackages)
	return report
}

func rootNodeID(dialogueID string) string {
	return dialogueID + "/root"
}

func affinityNodeID(dialogueID, left, right string) string {
	return fmt.Sprintf("%s/affinity/%s>%s", dialogueID, left, right)
}

func cacheKey(g *Graph, recipeVersion string, node *Node, staticHash string) string {
	upstreamKeys := make([]string, 0, len(node.Upstream))
	for _, id := range node.Upstream {
		if upstreamNode, ok := g.Nodes[id]; ok {
			upstreamKeys = append(upstreamKeys, upstreamNode.CacheKey)
		} else {
			upstreamKeys = append(upstreamKeys, "missing:"+id)
		}
	}
	sort.Strings(upstreamKeys)
	identity := node.PhonemeID
	if len(node.PhonemeIDs) > 0 {
		ids := append([]string(nil), node.PhonemeIDs...)
		sort.Strings(ids)
		identity = strings.Join(ids, ",")
	}
	parts := []string{
		"v1",
		staticHash,
		node.DialogueID,
		string(node.Kind),
		identity,
		recipeVersion,
		strings.Join(upstreamKeys, "|"),
	}
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	h.Write(node.Payload)
	return hex.EncodeToString(h.Sum(nil))
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func setKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func phonemeIndex(phonemes []domain.Phoneme, id string) int {
	for i := range phonemes {
		if phonemes[i].ID == id {
			return i
		}
	}
	return -1
}

func contains(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func eventsFor(events []domain.DispatchEvent, id string) []domain.DispatchEvent {
	var out []domain.DispatchEvent
	for _, event := range events {
		if event.PackageID == id {
			out = append(out, event)
		}
	}
	return out
}
