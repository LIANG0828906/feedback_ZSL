package generator

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"graphlab/internal/model"
)

func refEsc(s string) string {
	if s == "" {
		return "_empty"
	}
	return url.QueryEscape(s)
}

func refFloorDiv(value, divisor int64) int64 {
	if divisor <= 0 {
		return 0
	}
	if value >= 0 {
		return value / divisor
	}
	return int64(math.Floor(float64(value) / float64(divisor)))
}

func refSortedUnique(in []string) []string {
	sort.Strings(in)
	last := ""
	first := true
	out := make([]string, 0, len(in))
	for _, s := range in {
		if first || s != last {
			out = append(out, s)
			last = s
			first = false
		}
	}
	return out
}

func refVertexIdentity(id string) string { return "v:vertex:" + refEsc(id) }
func refEdgeIdentity(id string) string   { return "e:edge:" + refEsc(id) }
func refVertexEntity(id string) string   { return "v:" + id }
func refEdgeEntity(id string) string     { return "e:" + id }
func refShardDep(id string) string       { return "s:" + id }
func refOutKey(id string) string         { return "e:out:" + refEsc(id) }
func refLabelKey(label string) string    { return "v:label:" + refEsc(label) }

func refVertexKeys(v model.Vertex, rules []model.IndexRule) []string {
	keys := []string{refVertexIdentity(v.ID)}
	for _, label := range v.Labels {
		keys = append(keys, refLabelKey(label))
	}
	for _, rule := range rules {
		value, ok := v.Attrs[rule.Attribute]
		if !ok || value.Kind != rule.Kind {
			continue
		}
		switch value.Kind {
		case model.AttrInt:
			keys = append(keys, fmt.Sprintf("v:int:%s:b%d", refEsc(rule.Attribute), refFloorDiv(value.Int, rule.Bucket)))
		case model.AttrFixed:
			keys = append(keys, fmt.Sprintf("v:fixed:%s:b%d", refEsc(rule.Attribute), refFloorDiv(value.FixedMicros, rule.Bucket)))
		case model.AttrString:
			runes := []rune(value.String)
			n := rule.PrefixLen
			if n > len(runes) {
				n = len(runes)
			}
			prefix := string(runes[:n])
			if prefix == "" {
				prefix = "_empty"
			}
			keys = append(keys, "v:string:"+refEsc(rule.Attribute)+":p:"+refEsc(prefix))
		}
	}
	return refSortedUnique(keys)
}

func refEdgeKeys(e model.Edge, p *model.Project) []string {
	keys := []string{refEdgeIdentity(e.ID), refOutKey(e.From)}
	if p.HierarchyEnabled {
		for _, level := range e.Levels {
			keys = append(keys, "e:out:"+refEsc(e.From)+":lvl:"+refEsc(level))
		}
	}
	if !e.Directed {
		keys = append(keys, refOutKey(e.To))
	}
	return refSortedUnique(keys)
}

func refShardTruth(p *model.Project) map[string]model.ShardTruth {
	out := make(map[string]model.ShardTruth)
	for _, sh := range p.Shards {
		var vertices []model.Vertex
		var edges []model.Edge
		for _, v := range p.Vertices {
			if v.ShardID == sh.ID {
				vertices = append(vertices, v)
			}
		}
		for _, e := range p.Edges {
			if e.ShardID == sh.ID {
				edges = append(edges, e)
			}
		}
		sort.Slice(vertices, func(i, j int) bool {
			if vertices[i].DeclOrder == vertices[j].DeclOrder {
				return vertices[i].ID < vertices[j].ID
			}
			return vertices[i].DeclOrder < vertices[j].DeclOrder
		})
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].DeclOrder == edges[j].DeclOrder {
				return edges[i].ID < edges[j].ID
			}
			return edges[i].DeclOrder < edges[j].DeclOrder
		})
		tr := model.ShardTruth{
			VertexOrder: make([]string, 0, len(vertices)),
			EdgeOrder:   make([]string, 0, len(edges)),
		}
		keySet := make(map[string]bool)
		for _, v := range vertices {
			tr.VertexOrder = append(tr.VertexOrder, v.ID)
			for _, key := range refVertexKeys(v, p.IndexRules) {
				keySet[key] = true
			}
		}
		for _, e := range edges {
			tr.EdgeOrder = append(tr.EdgeOrder, e.ID)
			for _, key := range refEdgeKeys(e, p) {
				keySet[key] = true
			}
		}
		tr.IndexKeys = make([]string, 0, len(keySet))
		for key := range keySet {
			tr.IndexKeys = append(tr.IndexKeys, key)
		}
		sort.Strings(tr.IndexKeys)
		out[sh.ID] = tr
	}
	return out
}

func refHasLabels(v model.Vertex, labels []string) bool {
	set := make(map[string]bool, len(v.Labels))
	for _, label := range v.Labels {
		set[label] = true
	}
	for _, label := range labels {
		if !set[label] {
			return false
		}
	}
	return true
}

func refHasLabelsByID(p *model.Project, id string, labels []string) bool {
	for _, v := range p.Vertices {
		if v.ID == id {
			return refHasLabels(v, labels)
		}
	}
	return false
}

func refEvaluateQuery(p *model.Project, q model.Query) (model.QueryTruth, []string) {
	depSet := make(map[string]bool)
	for _, label := range q.Labels {
		depSet[refLabelKey(label)] = true
	}
	var visitedVertices, visitedEdges, resultVertices, pathVertices, pathEdges []string
	status := model.OutcomeOK
	startExists := false
	for _, v := range p.Vertices {
		if v.ID == q.Start {
			startExists = true
			break
		}
	}
	if !startExists {
		status = model.OutcomeStartMissing
		depSet[refVertexIdentity(q.Start)] = true
		return model.QueryTruth{Status: status, VisitedVertices: []string{}, VisitedEdges: []string{}, ResultVertices: []string{}}, sortedRefDeps(depSet)
	}
	visited := map[string]bool{q.Start: true}
	if q.MaxDepth == 0 {
		status = model.OutcomeDepthZero
		visitedVertices = []string{q.Start}
		refAddVertexDeps(p, q.Start, depSet)
		return model.QueryTruth{
			Status: status, VisitedVertices: visitedVertices,
			VisitedEdges: []string{}, ResultVertices: []string{},
		}, sortedRefDeps(depSet)
	}
	visitedVertices = []string{q.Start}
	refAddVertexDeps(p, q.Start, depSet)
	frontier := []string{q.Start}
	parentVertex := make(map[string]string)
	parentEdge := make(map[string]string)
	foundTarget := q.Target == q.Start && refHasLabelsByID(p, q.Start, q.Labels)
	shortest := q.Kind == model.QueryShortestPath
	outputs := make(map[string][]model.Edge)
	for _, e := range p.Edges {
		outputs[e.From] = append(outputs[e.From], e)
		if !e.Directed {
			outputs[e.To] = append(outputs[e.To], e)
		}
	}
	for _, edges := range outputs {
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].DeclOrder == edges[j].DeclOrder {
				return edges[i].ID < edges[j].ID
			}
			return edges[i].DeclOrder < edges[j].DeclOrder
		})
	}
	for depth := 0; depth < q.MaxDepth && len(frontier) > 0 && !foundTarget; depth++ {
		var next []string
		for _, vertexID := range frontier {
			depSet[refOutKey(vertexID)] = true
			for _, e := range outputs[vertexID] {
				depSet[refEdgeIdentity(e.ID)] = true
				depSet[refShardDep(e.ShardID)] = true
				other := e.To
				if e.From != vertexID {
					other = e.From
				}
				visitedEdges = append(visitedEdges, e.ID)
				if shortest && other == q.Target && refHasLabelsByID(p, q.Target, q.Labels) {
					parentVertex[other] = vertexID
					parentEdge[other] = e.ID
					foundTarget = true
					break
				}
				if visited[other] {
					continue
				}
				visited[other] = true
				parentVertex[other] = vertexID
				parentEdge[other] = e.ID
				visitedVertices = append(visitedVertices, other)
				refAddVertexDeps(p, other, depSet)
				next = append(next, other)
			}
			if shortest && foundTarget {
				break
			}
		}
		frontier = next
	}
	if shortest {
		if q.Target == q.Start && refHasLabelsByID(p, q.Start, q.Labels) {
			status = model.OutcomeOK
			pathVertices = []string{q.Start}
			pathEdges = []string{}
		} else if foundTarget {
			status = model.OutcomeOK
			cur := q.Target
			pathVertices = []string{cur}
			for cur != q.Start {
				eid, ok := parentEdge[cur]
				if !ok {
					break
				}
				pathEdges = append(pathEdges, eid)
				cur = parentVertex[cur]
				pathVertices = append(pathVertices, cur)
			}
			for i, j := 0, len(pathVertices)-1; i < j; i, j = i+1, j-1 {
				pathVertices[i], pathVertices[j] = pathVertices[j], pathVertices[i]
			}
			for i, j := 0, len(pathEdges)-1; i < j; i, j = i+1, j-1 {
				pathEdges[i], pathEdges[j] = pathEdges[j], pathEdges[i]
			}
			resultVertices = []string{q.Target}
		} else {
			status = model.OutcomeTargetUnreachable
		}
	} else {
		rank := make(map[string]int, len(p.Vertices))
		for i, v := range p.Vertices {
			rank[v.ID] = i
		}
		seen := make(map[string]bool)
		for _, id := range visitedVertices {
			if id == q.Start || seen[id] || !refHasLabelsByID(p, id, q.Labels) {
				continue
			}
			seen[id] = true
			resultVertices = append(resultVertices, id)
		}
		sort.Slice(resultVertices, func(i, j int) bool {
			if rank[resultVertices[i]] == rank[resultVertices[j]] {
				return resultVertices[i] < resultVertices[j]
			}
			return rank[resultVertices[i]] < rank[resultVertices[j]]
		})
	}
	if visitedVertices == nil {
		visitedVertices = []string{}
	}
	if visitedEdges == nil {
		visitedEdges = []string{}
	}
	if resultVertices == nil {
		resultVertices = []string{}
	}
	return model.QueryTruth{
		Status: status, VisitedVertices: visitedVertices, VisitedEdges: visitedEdges,
		ResultVertices: resultVertices, PathVertices: pathVertices, PathEdges: pathEdges,
	}, sortedRefDeps(depSet)
}

func refAddVertexDeps(p *model.Project, id string, deps map[string]bool) {
	deps[refVertexIdentity(id)] = true
	for _, v := range p.Vertices {
		if v.ID == id {
			deps[refShardDep(v.ShardID)] = true
			break
		}
	}
}

func sortedRefDeps(deps map[string]bool) []string {
	out := make([]string, 0, len(deps))
	for dep := range deps {
		out = append(out, dep)
	}
	sort.Strings(out)
	return out
}

type refEntity struct {
	keys      []string
	shardID   string
	canonical string
}

func refProjectEntities(p *model.Project) map[string]refEntity {
	out := make(map[string]refEntity, len(p.Vertices)+len(p.Edges))
	for _, v := range p.Vertices {
		b, _ := json.Marshal(v)
		out[refVertexEntity(v.ID)] = refEntity{
			keys: refVertexKeys(v, p.IndexRules), shardID: v.ShardID, canonical: string(b),
		}
	}
	for _, e := range p.Edges {
		b, _ := json.Marshal(e)
		out[refEdgeEntity(e.ID)] = refEntity{
			keys: refEdgeKeys(e, p), shardID: e.ShardID, canonical: string(b),
		}
	}
	return out
}

func refDiffProjects(old, next *model.Project) (entities, shards, keys []string, shardDeps bool) {
	oldEnts := refProjectEntities(old)
	newEnts := refProjectEntities(next)
	entitySet := make(map[string]bool)
	shardSet := make(map[string]bool)
	keySet := make(map[string]bool)
	ids := make([]string, 0, len(oldEnts)+len(newEnts))
	for id := range oldEnts {
		ids = append(ids, id)
	}
	for id := range newEnts {
		if _, ok := oldEnts[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		oe, hasOld := oldEnts[id]
		ne, hasNew := newEnts[id]
		if hasOld && hasNew && oe.canonical == ne.canonical {
			continue
		}
		entitySet[id] = true
		if hasOld {
			shardSet[oe.shardID] = true
			for _, key := range oe.keys {
				keySet[key] = true
			}
		}
		if hasNew {
			shardSet[ne.shardID] = true
			for _, key := range ne.keys {
				keySet[key] = true
			}
		}
	}
	oldShards := make(map[string]model.Shard)
	newShards := make(map[string]model.Shard)
	for _, sh := range old.Shards {
		oldShards[sh.ID] = sh
	}
	for _, sh := range next.Shards {
		newShards[sh.ID] = sh
	}
	for id, sh := range oldShards {
		if ns, ok := newShards[id]; !ok || ns != sh {
			shardSet[id] = true
		}
	}
	for id, sh := range newShards {
		if os, ok := oldShards[id]; !ok || os != sh {
			shardSet[id] = true
		}
	}
	return sortedSet(entitySet), sortedSet(shardSet), sortedSet(keySet), false
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func refClone(p *model.Project) *model.Project { return model.CloneProject(p) }

func refApplyOperation(p *model.Project, op model.Operation) error {
	switch op.Kind {
	case model.OpVertexAdd:
		v := model.CloneVertex(*op.Vertex)
		for _, existing := range p.Vertices {
			if existing.ID == v.ID {
				return fmt.Errorf("duplicate vertex %s", v.ID)
			}
		}
		p.Vertices = append(p.Vertices, v)
	case model.OpVertexUpdate:
		found := false
		if len(p.Vertices) == 0 {
			return fmt.Errorf("vertex %s missing", op.Vertex.ID)
		}
		v := model.CloneVertex(*op.Vertex)
		for i := range p.Vertices {
			if p.Vertices[i].ID == v.ID {
				p.Vertices[i] = v
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("vertex %s missing", op.Vertex.ID)
		}
	case model.OpVertexDelete:
		idx := -1
		for i := range p.Vertices {
			if p.Vertices[i].ID == op.VertexID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("vertex %s missing", op.VertexID)
		}
		p.Vertices = append(p.Vertices[:idx], p.Vertices[idx+1:]...)
		kept := p.Edges[:0]
		for _, e := range p.Edges {
			if e.From != op.VertexID && e.To != op.VertexID {
				kept = append(kept, e)
			}
		}
		p.Edges = kept
	case model.OpEdgeAdd:
		e := model.CloneEdge(*op.Edge)
		p.Edges = append(p.Edges, e)
	case model.OpEdgeUpdate:
		found := false
		for i := range p.Edges {
			if p.Edges[i].ID == op.Edge.ID {
				p.Edges[i] = model.CloneEdge(*op.Edge)
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("edge %s missing", op.Edge.ID)
		}
	case model.OpEdgeDelete:
		idx := -1
		for i := range p.Edges {
			if p.Edges[i].ID == op.EdgeID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("edge %s missing", op.EdgeID)
		}
		p.Edges = append(p.Edges[:idx], p.Edges[idx+1:]...)
	case model.OpReshard:
		for i := range p.Vertices {
			p.Vertices[i].ShardID = ""
		}
		for i := range p.Edges {
			p.Edges[i].ShardID = ""
		}
		p.Shards = append([]model.Shard(nil), op.Shards...)
	default:
		return fmt.Errorf("unsupported operation %s", op.Kind)
	}
	p.Version++
	model.SortProject(p)
	if err := model.AssignShards(p); err != nil {
		return err
	}
	model.SortProject(p)
	return nil
}

func refCacheID(q model.Query) string {
	sum := sha1.Sum([]byte(q.Canonical()))
	return "C-" + hex.EncodeToString(sum[:])[:12]
}

func BuildTruth(p *model.Project) (*model.Truth, error) {
	shards := refShardTruth(p)
	queries := make(map[string]model.QueryTruth, len(p.Queries))
	cacheDeps := make(map[string][]string, len(p.Queries))
	for _, q := range p.Queries {
		truth, depsList := refEvaluateQuery(p, q)
		queries[q.ID] = truth
		cacheDeps[refCacheID(q)] = depsList
	}
	truth := &model.Truth{
		Seed: p.Seed, ProjectID: p.ID, InitialVersion: p.Version,
		CoreDigest: model.CoreDigest(p), Shards: shards, Queries: queries,
	}
	state := refClone(p)
	cache := make(map[string]string, len(p.Queries))
	for _, q := range p.Queries {
		cache[refCacheID(q)] = q.ID
	}
	for _, op := range p.Operations {
		old := refClone(state)
		if err := refApplyOperation(state, op); err != nil {
			return nil, fmt.Errorf("operation %d: %w", op.Seq, err)
		}
		changedEntities, affectedShards, affectedKeys, shardDeps := refDiffProjects(old, state)
		affected := make(map[string]bool)
		for _, key := range affectedKeys {
			affected[key] = true
		}
		for _, ent := range changedEntities {
			if strings.HasPrefix(ent, "v:") {
				affected[refVertexIdentity(strings.TrimPrefix(ent, "v:"))] = true
			} else if strings.HasPrefix(ent, "e:") {
				affected[refEdgeIdentity(strings.TrimPrefix(ent, "e:"))] = true
			}
		}
		if op.Kind == model.OpReshard {
			shardDeps = true
		}
		if shardDeps {
			for _, shard := range affectedShards {
				affected[refShardDep(shard)] = true
			}
		}
		var invalidated, reused []string
		for cacheID := range cache {
			_, reasons := refIntersect(cacheDeps[cacheID], affected)
			if len(reasons) > 0 {
				invalidated = append(invalidated, cacheID)
			} else {
				reused = append(reused, cacheID)
			}
		}
		sort.Strings(invalidated)
		sort.Strings(reused)
		opQueries := make(map[string]model.QueryTruth, len(state.Queries))
		for _, q := range state.Queries {
			qt, newDeps := refEvaluateQuery(state, q)
			opQueries[q.ID] = qt
			cacheDeps[refCacheID(q)] = newDeps
		}
		truth.Operations = append(truth.Operations, model.OperationTruth{
			Seq: op.Seq, Kind: op.Kind, ProjectVersion: state.Version,
			AffectedShards: affectedShards, ChangedEntities: changedEntities,
			AffectedIndexKeys:   affectedKeys,
			InvalidatedCacheIDs: invalidated, ReusedCacheIDs: reused,
			Queries: opQueries,
		})
	}
	return truth, nil
}

func refIntersect(deps []string, affected map[string]bool) (bool, []string) {
	var reasons []string
	for _, dep := range deps {
		if affected[dep] {
			reasons = append(reasons, dep)
		}
	}
	if len(reasons) == 0 {
		return false, nil
	}
	sort.Strings(reasons)
	return true, reasons
}

func refRuneCount(s string) int { return utf8.RuneCountInString(s) }

func refParseBucket(key string) int64 {
	i := strings.LastIndex(key, ":b")
	if i < 0 {
		return 0
	}
	n, _ := strconv.ParseInt(key[i+2:], 10, 64)
	return n
}
