package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

type NodeKind string

const (
	NodeContent  NodeKind = "content"
	NodeEnvelope NodeKind = "phoneme_envelope"
	NodeAccent   NodeKind = "accent_landing"
	NodeStretch  NodeKind = "stretch_coefficient"
	NodeRange    NodeKind = "stretched_range"
	NodeWarp     NodeKind = "warped_accent"
)

type Node struct {
	Key          string   `json:"key"`
	Kind         NodeKind `json:"kind"`
	DialogID     string   `json:"dialog_id"`
	PhonemeIndex *int     `json:"phoneme_index,omitempty"`
	DependsOn    []string `json:"depends_on"`
}

type Graph struct {
	nodes      map[string]Node
	dependents map[string][]string
}

func NewGraph() *Graph {
	return &Graph{nodes: map[string]Node{}, dependents: map[string][]string{}}
}

func (g *Graph) Add(n Node) Node {
	deps := append([]string(nil), n.DependsOn...)
	sort.Strings(deps)
	n.DependsOn = deps
	g.nodes[n.Key] = n
	for _, dep := range deps {
		g.dependents[dep] = append(g.dependents[dep], n.Key)
	}
	return n
}

func (g *Graph) Node(key string) (Node, bool) {
	n, ok := g.nodes[key]
	return n, ok
}

func (g *Graph) Keys() []string {
	keys := make([]string, 0, len(g.nodes))
	for k := range g.nodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (g *Graph) Invalidate(changed ...string) []string {
	invalid := map[string]struct{}{}
	queue := append([]string(nil), changed...)
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if _, ok := invalid[key]; ok {
			continue
		}
		if _, exists := g.nodes[key]; exists {
			invalid[key] = struct{}{}
		}
		for _, down := range g.dependents[key] {
			if _, ok := invalid[down]; !ok {
				queue = append(queue, down)
			}
		}
	}
	out := make([]string, 0, len(invalid))
	for k := range invalid {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func CacheKey(contentHash, phonemeID, parameterVersion string, kind NodeKind, upstream ...string) string {
	up := append([]string(nil), upstream...)
	sort.Strings(up)
	upSum := sha256.Sum256([]byte(strings.Join(up, "\x1f")))
	raw := strings.Join([]string{contentHash, phonemeID, parameterVersion, string(kind), hex.EncodeToString(upSum[:])}, "\x1e")
	sum := sha256.Sum256([]byte(raw))
	return string(kind) + ":" + hex.EncodeToString(sum[:12])
}

func ContentKey(contentHash string) string {
	return "content:" + contentHash[:min(16, len(contentHash))]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
