package forensic

import (
	"encoding/json"
	"fmt"
)

const (
	StageHash     = "hash"
	StageAnalysis = "analysis"
	StageSummary  = "summary"
	StageHeatmap  = "heatmap"
	StageReport   = "report"
)

type ParamVersionPayload struct {
	Schema           string  `json:"schema"`
	AlgorithmVersion string  `json:"algorithmVersion"`
	Width            int     `json:"width"`
	Height           int     `json:"height"`
	Channels         int     `json:"channels"`
	HashAlgorithm    string  `json:"hashAlgorithm"`
	FingerprintGrid  int     `json:"fingerprintGrid"`
	Threshold        float64 `json:"threshold"`
	Window           int     `json:"window"`
	BlockSize        int     `json:"blockSize"`
}

func ParamsVersion(img *Image, p Params) string {
	payload := ParamVersionPayload{
		Schema: SchemaVersion, AlgorithmVersion: AlgorithmVersion,
		Width: img.Width, Height: img.Height, Channels: img.Channels,
		HashAlgorithm: p.HashAlgorithm, FingerprintGrid: p.FingerprintGrid,
		Threshold: p.Threshold, Window: p.Window, BlockSize: p.BlockSize,
	}
	return hashString(string(canonicalJSON(payload)))
}

func HashKey(contentDigest string, p Params) string {
	return hashString("hash", contentDigest, p.HashAlgorithm, fmt.Sprint(p.FingerprintGrid), AlgorithmVersion)
}

func StatsKey(hashKey string, p Params) string {
	return hashString("analysis/stats", hashKey, fmt.Sprint(p.Window), AlgorithmVersion)
}

func VerdictKey(statsKey string, threshold float64) string {
	return hashString("analysis/verdict", statsKey, formatFloat(threshold), AlgorithmVersion)
}

func SummaryKey(verdictKey string) string {
	return hashString("summary", verdictKey, AlgorithmVersion)
}

func HeatmapKey(summaryKeys []string) string {
	h := hashString("heatmap/v1", AlgorithmVersion)
	for _, key := range summaryKeys {
		h = hashString("heatmap/next", h, key)
	}
	return h
}

// SameExceptThreshold is the exact reuse gate for threshold-only updates.
func SameExceptThreshold(a, b Params) bool {
	a.Threshold, b.Threshold = 0, 0
	a.WorkerCount, b.WorkerCount = 0, 0
	a.PixelBufferLimit, b.PixelBufferLimit = 0, 0
	a.QueueCapacity, b.QueueCapacity = 0, 0
	a.MaskTolerance, b.MaskTolerance = 0, 0
	return a == b
}

// RuntimeEquivalent ignores parameters that cannot affect deterministic output.
func RuntimeEquivalent(a, b Params) bool {
	a.Threshold, b.Threshold = 0, 0
	a.WorkerCount, b.WorkerCount = 0, 0
	a.PixelBufferLimit, b.PixelBufferLimit = 0, 0
	a.QueueCapacity, b.QueueCapacity = 0, 0
	a.MaskTolerance, b.MaskTolerance = 0, 0
	return a == b
}

type DependencyNode struct {
	ID       string   `json:"id"`
	Stage    string   `json:"stage"`
	Block    int      `json:"block"`
	CacheKey string   `json:"cacheKey"`
	Upstream []string `json:"upstream,omitempty"`
}

type Invalidation struct {
	BlockIndex int    `json:"blockIndex"`
	Stage      string `json:"stage"`
	Reason     string `json:"reason"`
	CacheKey   string `json:"cacheKey,omitempty"`
}

type ReuseRecord struct {
	BlockIndex int    `json:"blockIndex"`
	Stage      string `json:"stage"`
	Reason     string `json:"reason"`
}

type DependencyGraph struct {
	Nodes []DependencyNode `json:"nodes"`
	Edges [][2]string      `json:"edges"`
}

func BuildDependencyGraph(states []BlockState, heatKey string) DependencyGraph {
	g := DependencyGraph{}
	for _, s := range states {
		hashNode := fmt.Sprintf("block/%d/hash", s.BlockIndex)
		statsNode := fmt.Sprintf("block/%d/analysis", s.BlockIndex)
		verdictNode := fmt.Sprintf("block/%d/verdict", s.BlockIndex)
		summaryNode := fmt.Sprintf("block/%d/summary", s.BlockIndex)
		g.Nodes = append(g.Nodes,
			DependencyNode{ID: hashNode, Stage: StageHash, Block: s.BlockIndex, CacheKey: s.HashKey},
			DependencyNode{ID: statsNode, Stage: StageAnalysis, Block: s.BlockIndex, CacheKey: s.StatsKey, Upstream: []string{hashNode}},
			DependencyNode{ID: verdictNode, Stage: StageAnalysis, Block: s.BlockIndex, CacheKey: s.VerdictKey, Upstream: []string{statsNode}},
			DependencyNode{ID: summaryNode, Stage: StageSummary, Block: s.BlockIndex, CacheKey: s.SummaryKey, Upstream: []string{verdictNode}},
		)
		g.Edges = append(g.Edges, [2]string{hashNode, statsNode}, [2]string{statsNode, verdictNode}, [2]string{verdictNode, summaryNode})
	}
	g.Nodes = append(g.Nodes, DependencyNode{ID: "final/heatmap", Stage: StageHeatmap, Block: -1, CacheKey: heatKey})
	for _, s := range states {
		g.Edges = append(g.Edges, [2]string{fmt.Sprintf("block/%d/summary", s.BlockIndex), "final/heatmap"})
	}
	return g
}

func marshalParams(p Params) string {
	data, _ := json.Marshal(p)
	return string(data)
}
