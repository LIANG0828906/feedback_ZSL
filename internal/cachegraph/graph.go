package cachegraph

import (
	"fmt"
	"sort"

	"lip-pipeline-prototype/internal/domain"
	"lip-pipeline-prototype/internal/hashx"
)

type NodeKind string

const (
	NodeParam    NodeKind = "param"
	NodePhoneme  NodeKind = "phoneme"
	NodePack     NodeKind = "pack"
	NodeFrames   NodeKind = "frames"
	NodeDispatch NodeKind = "dispatch"
)

type Node struct {
	ID            string               `json:"id"`
	Kind          NodeKind             `json:"kind"`
	CacheKey      string               `json:"cache_key"`
	PackIndex     int                  `json:"pack_index"`
	Version       string               `json:"version"`
	Digest        string               `json:"digest,omitempty"`
	Upstream      []string             `json:"upstream,omitempty"`
	SourceVersion domain.SchemaVersion `json:"source_version,omitempty"`
}

type Edge struct {
	From   string   `json:"from"`
	To     string   `json:"to"`
	Reason string   `json:"reason"`
	Fields []string `json:"fields,omitempty"`
}

type Graph struct {
	DialogueID string          `json:"dialogue_id"`
	ShardID    string          `json:"shard_id"`
	Nodes      map[string]Node `json:"nodes"`
	Edges      []Edge          `json:"edges"`
}

func CacheKey(dialogueHash, entityID string, packIndex int, paramVersion string, upstreamKeys ...string) string {
	sort.Strings(upstreamKeys)
	upstream := hashx.Strings(upstreamKeys...)
	full := hashx.Strings(dialogueHash, entityID, fmt.Sprintf("%d", packIndex), paramVersion, upstream)
	return "ck_" + hashx.Short(full, 24)
}

func ParamVersion(params domain.Params) string {
	copyParams := params
	copyParams.ParamVersion = ""
	return "pv_" + hashx.Short(hashx.JSON(copyParams), 16)
}

func NewGraph() *Graph {
	return &Graph{Nodes: map[string]Node{}}
}

func BuildShardGraph(dialogueHash string, shard domain.Shard, result *domain.ShardResult) *Graph {
	g := &Graph{DialogueID: shard.DialogueID, ShardID: shard.ID, Nodes: map[string]Node{}}
	paramID := "param:" + result.ParamVersion
	g.Nodes[paramID] = Node{ID: paramID, Kind: NodeParam, CacheKey: CacheKey(dialogueHash, "param", -1, result.ParamVersion), Version: result.ParamVersion}

	phonemeKeys := map[string]string{}
	for _, ph := range shard.Phonemes {
		id := "phoneme:" + ph.ID
		key := CacheKey(dialogueHash, ph.ID, -1, result.ParamVersion)
		phonemeKeys[ph.ID] = key
		g.Nodes[id] = Node{ID: id, Kind: NodePhoneme, CacheKey: key, PackIndex: -1, Version: result.ParamVersion, Digest: hashx.JSON(ph), SourceVersion: result.SourceVersion}
	}

	frameKeysByPack := map[int]string{}
	for _, pack := range result.Packs {
		packNodeID := fmt.Sprintf("pack:%s:%d", shard.ID, pack.Index)
		upstream := []string{}
		for _, pid := range pack.PhonemeIDs {
			upstream = append(upstream, phonemeKeys[pid])
			g.Edges = append(g.Edges, Edge{
				From: "phoneme:" + pid, To: packNodeID, Reason: "phoneme_member_to_pack",
				Fields: []string{"start_sample", "end_sample", "weight_permyriad", "affinity_to_next", "category"},
			})
		}
		packKey := CacheKey(dialogueHash, packNodeID, pack.Index, result.ParamVersion, upstream...)
		pack.CacheKey = packKey
		pack.UpstreamKeys = upstream
		result.Packs[pack.Index].CacheKey = packKey
		result.Packs[pack.Index].UpstreamKeys = upstream
		g.Nodes[packNodeID] = Node{ID: packNodeID, Kind: NodePack, CacheKey: packKey, PackIndex: pack.Index, Version: result.ParamVersion, Digest: pack.Digest, Upstream: upstream, SourceVersion: result.SourceVersion}

		frameNodeID := fmt.Sprintf("frames:%s:%d", shard.ID, pack.Index)
		frameKey := CacheKey(dialogueHash, frameNodeID, pack.Index, result.ParamVersion, packKey, g.Nodes[paramID].CacheKey)
		frameKeysByPack[pack.Index] = frameKey
		g.Nodes[frameNodeID] = Node{ID: frameNodeID, Kind: NodeFrames, CacheKey: frameKey, PackIndex: pack.Index, Version: result.ParamVersion, Upstream: []string{packKey, g.Nodes[paramID].CacheKey}, SourceVersion: result.SourceVersion}
		g.Edges = append(g.Edges, Edge{From: packNodeID, To: frameNodeID, Reason: "pack_to_quantized_frames", Fields: []string{"fps", "quantization_rule", "transition_tolerance"}})
		g.Edges = append(g.Edges, Edge{From: paramID, To: frameNodeID, Reason: "render_params_to_frames", Fields: []string{"fps", "transition_tolerance", "preview_stride"}})
	}

	dispatchID := "dispatch:" + shard.ID
	upstreamFrames := []string{}
	for idx := range frameKeysByPack {
		upstreamFrames = append(upstreamFrames, frameKeysByPack[idx])
	}
	dispatchKey := CacheKey(dialogueHash, dispatchID, -1, result.ParamVersion, upstreamFrames...)
	g.Nodes[dispatchID] = Node{ID: dispatchID, Kind: NodeDispatch, CacheKey: dispatchKey, PackIndex: -1, Version: result.ParamVersion, Upstream: upstreamFrames, SourceVersion: result.SourceVersion}
	for _, key := range upstreamFrames {
		var frameID string
		for id, node := range g.Nodes {
			if node.CacheKey == key && node.Kind == NodeFrames {
				frameID = id
				break
			}
		}
		if frameID != "" {
			g.Edges = append(g.Edges, Edge{From: frameID, To: dispatchID, Reason: "frames_to_dispatch", Fields: []string{"frame_index", "mouth", "kind"}})
		}
	}
	return g
}

type InvalidationItem struct {
	NodeID        string               `json:"node_id"`
	Kind          NodeKind             `json:"kind"`
	PackIndex     int                  `json:"pack_index"`
	CacheKey      string               `json:"cache_key"`
	OldCacheKey   string               `json:"old_cache_key,omitempty"`
	SourceVersion domain.SchemaVersion `json:"source_version"`
	Reason        string               `json:"reason"`
	Fields        []string             `json:"fields,omitempty"`
	PersistentHit bool                 `json:"persistent_hit"`
}

type InvalidationReport struct {
	DialogueID        string             `json:"dialogue_id"`
	ShardID           string             `json:"shard_id"`
	OldContentHash    string             `json:"old_content_hash"`
	NewContentHash    string             `json:"new_content_hash"`
	OldParamVersion   string             `json:"old_param_version"`
	NewParamVersion   string             `json:"new_param_version"`
	Invalidated       []InvalidationItem `json:"invalidated"`
	Reused            []InvalidationItem `json:"reused"`
	CommitInvalidated bool               `json:"commit_invalidated"`
}

func frameDigestForPack(result *domain.ShardResult, packIndex int) string {
	type compact struct {
		Index int64
		Mouth domain.MouthClass
		Kind  domain.FrameKind
		From  domain.MouthClass
		To    domain.MouthClass
	}
	frames := []compact{}
	for _, f := range result.Frames {
		if f.PackIndex == packIndex {
			frames = append(frames, compact{f.Index, f.Mouth, f.Kind, f.TransitionFrom, f.TransitionTo})
		}
	}
	marks := []domain.TransitionMark{}
	for _, m := range result.Transitions {
		if m.PackIndex == packIndex {
			marks = append(marks, m)
		}
	}
	return hashx.JSON(struct {
		Frames []compact
		Marks  []domain.TransitionMark
	}{frames, marks})
}

func DiffAndPlan(oldGraph *Graph, oldResult, newResult *domain.ShardResult, newGraph *Graph, source domain.SchemaVersion) *InvalidationReport {
	report := &InvalidationReport{
		DialogueID: oldResult.DialogueID, ShardID: oldResult.ShardID,
		OldContentHash: oldResult.ContentHash, NewContentHash: newResult.ContentHash,
		OldParamVersion: oldResult.ParamVersion, NewParamVersion: newResult.ParamVersion,
	}
	oldPacks := map[int]domain.Pack{}
	newPacks := map[int]domain.Pack{}
	for _, p := range oldResult.Packs {
		oldPacks[p.Index] = p
	}
	for _, p := range newResult.Packs {
		newPacks[p.Index] = p
	}
	maxPack := len(oldResult.Packs)
	if len(newResult.Packs) > maxPack {
		maxPack = len(newResult.Packs)
	}

	invalidPacks := map[int]bool{}
	for i := 0; i < maxPack; i++ {
		op, ook := oldPacks[i]
		np, nok := newPacks[i]
		if ook != nok || !ook || !nok || op.Digest != np.Digest || !equalStrings(op.PhonemeIDs, np.PhonemeIDs) {
			invalidPacks[i] = true
			reason := "pack_digest_or_membership_changed"
			if ook != nok {
				reason = "pack_added_or_removed"
			}
			report.Invalidated = append(report.Invalidated, InvalidationItem{
				NodeID: fmt.Sprintf("pack:%s:%d", oldResult.ShardID, i), Kind: NodePack, PackIndex: i,
				CacheKey: keyFromGraph(newGraph, NodePack, i), OldCacheKey: op.CacheKey,
				SourceVersion: source, Reason: reason,
				Fields: []string{"start_sample", "end_sample", "weight_permyriad", "affinity_to_next"},
			})
			continue
		}
		oldFrameDigest := frameDigestForPack(oldResult, i)
		newFrameDigest := frameDigestForPack(newResult, i)
		if oldFrameDigest != newFrameDigest {
			invalidPacks[i] = true
			report.Invalidated = append(report.Invalidated, InvalidationItem{
				NodeID: fmt.Sprintf("frames:%s:%d", oldResult.ShardID, i), Kind: NodeFrames, PackIndex: i,
				CacheKey: keyFromGraph(newGraph, NodeFrames, i), OldCacheKey: keyFromGraph(oldGraph, NodeFrames, i),
				SourceVersion: source, Reason: "quantized_frames_changed", Fields: []string{"fps", "frame_index", "mouth", "transition"},
			})
		} else {
			report.Reused = append(report.Reused, InvalidationItem{
				NodeID: fmt.Sprintf("pack:%s:%d", oldResult.ShardID, i), Kind: NodePack, PackIndex: i,
				CacheKey: np.CacheKey, OldCacheKey: op.CacheKey, SourceVersion: source,
				Reason: "pack_digest_unchanged",
			})
			report.Reused = append(report.Reused, InvalidationItem{
				NodeID: fmt.Sprintf("frames:%s:%d", oldResult.ShardID, i), Kind: NodeFrames, PackIndex: i,
				CacheKey: keyFromGraph(newGraph, NodeFrames, i), OldCacheKey: keyFromGraph(oldGraph, NodeFrames, i),
				SourceVersion: source, Reason: "frame_digest_unchanged",
			})
		}
	}

	if len(report.Invalidated) > 0 || oldResult.ParamVersion != newResult.ParamVersion {
		report.CommitInvalidated = true
		report.Invalidated = append(report.Invalidated, InvalidationItem{
			NodeID: "dispatch:" + oldResult.ShardID, Kind: NodeDispatch, PackIndex: -1,
			CacheKey: keyFromGraph(newGraph, NodeDispatch, -1), OldCacheKey: keyFromGraph(oldGraph, NodeDispatch, -1),
			SourceVersion: source, Reason: "downstream_dispatch_requires_commit_refresh",
		})
	} else {
		report.Reused = append(report.Reused, InvalidationItem{
			NodeID: "dispatch:" + oldResult.ShardID, Kind: NodeDispatch, PackIndex: -1,
			CacheKey: keyFromGraph(newGraph, NodeDispatch, -1), SourceVersion: source, Reason: "dispatch_unchanged",
		})
	}
	return report
}

func keyFromGraph(g *Graph, kind NodeKind, packIndex int) string {
	if g == nil {
		return ""
	}
	for _, node := range g.Nodes {
		if node.Kind == kind && node.PackIndex == packIndex {
			return node.CacheKey
		}
	}
	return ""
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
