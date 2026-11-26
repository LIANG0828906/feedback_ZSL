package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"iconflow/model"
)

const (
	ThresholdNode = "config/threshold_scale"
	QuantNode     = "config/quant_step"
	OrderNode     = "config/channel_order"
	OperatorNode  = "config/operator_version"
)

type Edge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type ImpactItem struct {
	BlockKey        string   `json:"block"`
	Field           string   `json:"field"`
	InvalidatedNode string   `json:"invalidated_node"`
	Reason          string   `json:"reason"`
	DependencyPath  []string `json:"dependency_path"`
}

type Manifest struct {
	Fields []string     `json:"fields"`
	Blocks []string     `json:"blocks"`
	Items  []ImpactItem `json:"items"`
}

func (m *Manifest) BlockCount() int { return len(m.Blocks) }

type Graph struct {
	Nodes map[string]struct{}
	Edges []Edge
	out   map[string][]Edge
}

func New() *Graph {
	return &Graph{Nodes: map[string]struct{}{}, out: map[string][]Edge{}}
}

func (g *Graph) addNode(id string) { g.Nodes[id] = struct{}{} }

func (g *Graph) addEdge(from, to, reason string) {
	g.addNode(from)
	g.addNode(to)
	g.Edges = append(g.Edges, Edge{From: from, To: to, Reason: reason})
	g.out[from] = append(g.out[from], Edge{From: from, To: to, Reason: reason})
}

func (g *Graph) sortedOut(id string) []Edge {
	edges := append([]Edge(nil), g.out[id]...)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		return edges[i].Reason < edges[j].Reason
	})
	return edges
}

func ProjectGraph(project *model.Project) *Graph {
	g := New()
	for _, node := range []string{ThresholdNode, QuantNode, OrderNode, OperatorNode} {
		g.addNode(node)
	}
	for _, coord := range project.SortedBlockCoords() {
		key := coord.Key()
		input := "block/" + key + "/input"
		gradient := "block/" + key + "/gradient"
		threshold := "block/" + key + "/dynamic_threshold"
		markers := "block/" + key + "/markers"
		stats := "block/" + key + "/stats"
		for _, node := range []string{input, gradient, threshold, markers, stats} {
			g.addNode(node)
		}
		g.addEdge(input, gradient, "raw block pixels feed gradient")
		g.addEdge(OrderNode, gradient, "channel order changes plane offsets and luma")
		g.addEdge(OperatorNode, gradient, "operator kernel version changes amplitudes")
		g.addEdge(QuantNode, gradient, "quantization changes sampled gray levels")
		g.addEdge(QuantNode, threshold, "quantization parameter participates in cache identity")
		g.addEdge(gradient, threshold, "integer gradient distribution feeds quantile")
		g.addEdge(ThresholdNode, threshold, "threshold scale selects integer quantile")
		g.addEdge(threshold, markers, "dynamic threshold decides foreground markers")
		g.addEdge(markers, stats, "markers feed connected components and topology stats")
	}
	return g
}

func FieldNode(field string) string {
	switch strings.ToLower(field) {
	case "threshold", "threshold_scale", "threshold_permille", "threshold_permyriad", "$.threshold_permille", "$.scale.threshold_per_10k":
		return ThresholdNode
	case "quant", "quant_step", "quantization", "$.quantization.step":
		return QuantNode
	case "channels", "channel_order", "channel_sequence", "$.color.order":
		return OrderNode
	case "operator", "gradient_operator", "operator_version", "algo_version":
		return OperatorNode
	default:
		return ""
	}
}

func (g *Graph) Impact(fields []string, onlyBlocks map[string]bool) *Manifest {
	fieldSet := map[string]bool{}
	startNodes := map[string]string{}
	for _, field := range fields {
		node := FieldNode(field)
		if node == "" {
			continue
		}
		fieldSet[field] = true
		startNodes[node] = field
	}

	itemMap := map[string]ImpactItem{}
	for startNode, field := range startNodes {
		g.collect(startNode, field, []string{startNode}, onlyBlocks, itemMap)
	}

	items := make([]ImpactItem, 0, len(itemMap))
	for _, item := range itemMap {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.BlockKey != b.BlockKey {
			return a.BlockKey < b.BlockKey
		}
		if a.InvalidatedNode != b.InvalidatedNode {
			return a.InvalidatedNode < b.InvalidatedNode
		}
		return strings.Join(a.DependencyPath, "->") < strings.Join(b.DependencyPath, "->")
	})

	blockSet := map[string]bool{}
	fieldList := make([]string, 0, len(fieldSet))
	for field := range fieldSet {
		fieldList = append(fieldList, field)
	}
	sort.Strings(fieldList)
	for _, item := range items {
		blockSet[item.BlockKey] = true
	}
	blocks := make([]string, 0, len(blockSet))
	for block := range blockSet {
		blocks = append(blocks, block)
	}
	sort.Strings(blocks)
	return &Manifest{Fields: fieldList, Blocks: blocks, Items: items}
}

func ImpactDirty(g *Graph, dirty []model.DirtyRecord) *Manifest {
	merged := map[string]ImpactItem{}
	fieldSet := map[string]bool{}
	for _, record := range dirty {
		for _, field := range record.Fields {
			fieldSet[field] = true
		}
		only := map[string]bool{}
		if record.Coord != "" {
			only[record.Coord] = true
		}
		manifest := g.Impact(record.Fields, only)
		for _, item := range manifest.Items {
			merged[item.Field+"|"+item.InvalidatedNode+"|"+strings.Join(item.DependencyPath, "->")] = item
		}
	}
	items := make([]ImpactItem, 0, len(merged))
	for _, item := range merged {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].BlockKey != items[j].BlockKey {
			return items[i].BlockKey < items[j].BlockKey
		}
		if items[i].Field != items[j].Field {
			return items[i].Field < items[j].Field
		}
		return items[i].InvalidatedNode < items[j].InvalidatedNode
	})
	blockSet := map[string]bool{}
	for _, item := range items {
		blockSet[item.BlockKey] = true
	}
	blocks := make([]string, 0, len(blockSet))
	for block := range blockSet {
		blocks = append(blocks, block)
	}
	sort.Strings(blocks)
	fields := make([]string, 0, len(fieldSet))
	for field := range fieldSet {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return &Manifest{Fields: fields, Blocks: blocks, Items: items}
}

func (g *Graph) collect(node, field string, path []string, onlyBlocks map[string]bool, items map[string]ImpactItem) {
	for _, edge := range g.sortedOut(node) {
		nextPath := append(append([]string(nil), path...), edge.To)
		if strings.HasPrefix(edge.To, "block/") {
			parts := strings.SplitN(edge.To, "/", 3)
			if len(parts) >= 2 {
				blockKey := parts[1]
				if len(onlyBlocks) == 0 || onlyBlocks[blockKey] {
					key := field + "|" + edge.To + "|" + strings.Join(nextPath, "->")
					items[key] = ImpactItem{
						BlockKey: blockKey, Field: field, InvalidatedNode: edge.To,
						Reason: edge.Reason, DependencyPath: nextPath,
					}
				}
			}
		}
		g.collect(edge.To, field, nextPath, onlyBlocks, items)
	}
}

func SHA256Hex(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func HashJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return SHA256Hex(fmt.Sprintf("%v", value))
	}
	return SHA256Hex(string(data))
}

func OrderHash(order []string, layout string) string {
	return SHA256Hex("order", strings.Join(order, ","), "layout", layout)
}

func ParamHash(project *model.Project, algoVersion string) string {
	return SHA256Hex(
		"threshold", fmt.Sprint(project.ThresholdPermyriad),
		"quant", fmt.Sprintf("%d|%s|%t", project.Quant.Step, project.Quant.Rounding, project.Quant.ExtentDivisible),
		"rules", HashJSON(project.Rules),
		"param", project.ParamVersion,
		"algo", algoVersion,
	)
}

func BlockInputHash(img *model.Image, coord model.BlockCoord) string {
	h := sha256.New()
	_, _ = h.Write([]byte(coord.Key()))
	_, _ = h.Write([]byte(fmt.Sprintf("|%d,%d,%d,%d|", coord.OriginX, coord.OriginY, coord.Width, coord.Height)))
	if img != nil && img.Raster != nil {
		for ch, plane := range img.Raster.Planes {
			_, _ = h.Write([]byte(fmt.Sprintf("ch%d:", ch)))
			for y := 0; y < coord.Height; y++ {
				for x := 0; x < coord.Width; x++ {
					idx := (coord.OriginY+y)*img.Raster.Width + (coord.OriginX + x)
					_, _ = h.Write([]byte(fmt.Sprintf("%d,", plane[idx])))
				}
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

type CacheKeyInput struct {
	Stage            string
	InputHash        string
	ThresholdVersion string
	QuantVersion     string
	OrderHash        string
	AlgoVersion      string
	UpstreamKey      string
}

func StageCacheKey(input CacheKeyInput) string {
	return SHA256Hex(
		"stage", input.Stage,
		"input", input.InputHash,
		"threshold_version", input.ThresholdVersion,
		"quant_version", input.QuantVersion,
		"order_hash", input.OrderHash,
		"algo_version", input.AlgoVersion,
		"upstream", input.UpstreamKey,
	)
}
