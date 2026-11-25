package engine

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"graphlab/internal/model"
)

type CacheInvalidation struct {
	CacheID string   `json:"cache_id"`
	QueryID string   `json:"query_id"`
	Reasons []string `json:"reasons"`
}

type OperationReport struct {
	Seq                int                 `json:"seq"`
	Kind               string              `json:"kind"`
	ProjectVersion     int                 `json:"project_version"`
	CommitSeq          int                 `json:"commit_seq"`
	AffectedShards     []string            `json:"affected_shards"`
	ChangedEntities    []string            `json:"changed_entities"`
	AffectedIndexKeys  []string            `json:"affected_index_keys"`
	PropagationClosure []string            `json:"propagation_closure"`
	InvalidatedCaches  []CacheInvalidation `json:"invalidated_caches"`
	ReusedCacheIDs     []string            `json:"reused_cache_ids"`
	RebuiltCacheIDs    []string            `json:"rebuilt_cache_ids"`
	IndexKeys          []IndexKeyView      `json:"index_keys"`
	Shards             []ShardIndex        `json:"shards"`
	QueryResults       []model.QueryResult `json:"query_results"`
	ApplyMicros        int64               `json:"apply_micros"`
	ShardMetrics       []ShardMetric       `json:"shard_metrics,omitempty"`
}

type mutationEffects struct {
	shards       map[string]bool
	entities     map[string]bool
	keys         map[string]bool
	cascadeEdges []string
	metrics      []ShardMetric
	shardDeps    bool
}

func newEffects() *mutationEffects {
	return &mutationEffects{
		shards: make(map[string]bool), entities: make(map[string]bool),
		keys: make(map[string]bool),
	}
}

func findVertexIndex(p *model.Project, id string) int {
	for i := range p.Vertices {
		if p.Vertices[i].ID == id {
			return i
		}
	}
	return -1
}

func findEdgeIndex(p *model.Project, id string) int {
	for i := range p.Edges {
		if p.Edges[i].ID == id {
			return i
		}
	}
	return -1
}

func keySet(keys []string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		out[key] = true
	}
	return out
}

func (e *mutationEffects) unionKeys(keys ...[]string) {
	for _, group := range keys {
		for _, key := range group {
			e.keys[key] = true
		}
	}
}

func (e *mutationEffects) diffKeys(oldKeys, newKeys []string) {
	oldSet, newSet := keySet(oldKeys), keySet(newKeys)
	for key := range oldSet {
		if !newSet[key] {
			e.keys[key] = true
		}
	}
	for key := range newSet {
		if !oldSet[key] {
			e.keys[key] = true
		}
	}
}

func ensureGlobalKey(idx *IndexStore, key string) *IndexKey {
	ik := idx.Keys[key]
	if ik == nil {
		ik = &IndexKey{Key: key}
		idx.Keys[key] = ik
	}
	return ik
}

func addContribution(idx *IndexStore, shardID, key, entity string) error {
	si := idx.Shards[shardID]
	if si == nil {
		return model.Err(model.EShardNotFound, "index shard does not exist", map[string]any{
			"shard_id": shardID, "key": key,
		})
	}
	if si.Contributions == nil {
		si.Contributions = make(map[string][]string)
	}
	si.Contributions[key] = sortedUnique(append(si.Contributions[key], entity))
	ik := ensureGlobalKey(idx, key)
	ik.Entities = sortedUnique(append(ik.Entities, entity))
	ik.RefShards = sortedUnique(append(ik.RefShards, shardID))
	return nil
}

func removeContribution(idx *IndexStore, shardID, key, entity string) {
	si := idx.Shards[shardID]
	if si == nil {
		return
	}
	ents := si.Contributions[key]
	next := make([]string, 0, len(ents))
	for _, ent := range ents {
		if ent != entity {
			next = append(next, ent)
		}
	}
	if len(next) == 0 {
		delete(si.Contributions, key)
	} else {
		si.Contributions[key] = next
	}
	ik := idx.Keys[key]
	if ik == nil {
		return
	}
	if len(next) == 0 {
		ik.RefShards = removeString(ik.RefShards, shardID)
	}
	ik.Entities = removeString(ik.Entities, entity)
	if len(ik.RefShards) == 0 || len(ik.Entities) == 0 {
		delete(idx.Keys, key)
	}
}

func removeString(in []string, value string) []string {
	out := in[:0]
	for _, s := range in {
		if s != value {
			out = append(out, s)
		}
	}
	return out
}

func bumpKeyVersions(idx *IndexStore, keys map[string]bool) {
	for key := range keys {
		ik := idx.Keys[key]
		if ik == nil {
			continue
		}
		if ik.Version == 0 {
			ik.Version = 1
		} else {
			ik.Version++
		}
	}
}

func removeShardNoBump(idx *IndexStore, shardID string) map[string]bool {
	keys := make(map[string]bool)
	si := idx.Shards[shardID]
	if si == nil {
		return keys
	}
	for key, entities := range si.Contributions {
		keys[key] = true
		for _, entity := range entities {
			removeContribution(idx, shardID, key, entity)
		}
	}
	delete(idx.Shards, shardID)
	return keys
}

func addShardNoBump(idx *IndexStore, si *ShardIndex) map[string]bool {
	keys := make(map[string]bool)
	idx.Shards[si.ID] = si
	for key, entities := range si.Contributions {
		keys[key] = true
		for _, entity := range entities {
			_ = addContribution(idx, si.ID, key, entity)
		}
	}
	return keys
}

func normalizeVertex(v *model.Vertex) {
	if v == nil {
		return
	}
	sort.Strings(v.Labels)
	out := v.Labels[:0]
	for i, label := range v.Labels {
		if i == 0 || label != out[len(out)-1] {
			out = append(out, label)
		}
	}
	v.Labels = out
}

func validateAttrValues(v *model.Vertex) error {
	for name, value := range v.Attrs {
		switch value.Kind {
		case model.AttrInt, model.AttrFixed, model.AttrString:
		default:
			return model.Err(model.EUnsupportedAttributeKind, "unsupported attribute kind in operation",
				map[string]any{"vertex_id": v.ID, "attribute": name, "kind": value.Kind})
		}
	}
	return nil
}

func validateVertexPayload(p *model.Project, v *model.Vertex) error {
	if v == nil || v.ID == "" {
		return model.Err(model.EInvalidOperation, "vertex operation payload is missing its id", map[string]any{})
	}
	if v.DeclOrder < 0 {
		return model.Err(model.EInvalidOperation, "vertex declaration order is negative",
			map[string]any{"vertex_id": v.ID, "decl_order": v.DeclOrder})
	}
	if err := validateAttrValues(v); err != nil {
		return err
	}
	sh, err := model.ShardForOperation(p.Shards, v.ID)
	if err != nil {
		return err
	}
	if v.ShardID != "" && v.ShardID != sh.ID {
		return model.Err(model.EAmbiguousField, "operation vertex declares a conflicting shard",
			map[string]any{"vertex_id": v.ID, "declared": v.ShardID, "resolved": sh.ID})
	}
	v.ShardID = sh.ID
	normalizeVertex(v)
	return nil
}

func validateEdgePayload(p *model.Project, e *model.Edge) error {
	if e == nil || e.ID == "" {
		return model.Err(model.EInvalidOperation, "edge operation payload is missing its id", map[string]any{})
	}
	if e.DeclOrder < 0 {
		return model.Err(model.EInvalidOperation, "edge declaration order is negative",
			map[string]any{"edge_id": e.ID, "decl_order": e.DeclOrder})
	}
	if findVertexIndex(p, e.From) < 0 {
		return model.Err(model.EMissingVertex, "edge source vertex is missing from operation graph",
			map[string]any{"edge_id": e.ID, "vertex_id": e.From, "role": "source"})
	}
	if findVertexIndex(p, e.To) < 0 {
		return model.Err(model.EMissingVertex, "edge target vertex is missing from operation graph",
			map[string]any{"edge_id": e.ID, "vertex_id": e.To, "role": "target"})
	}
	sh, err := model.ShardForOperation(p.Shards, e.From)
	if err != nil {
		return err
	}
	if e.ShardID != "" && e.ShardID != sh.ID {
		return model.Err(model.EAmbiguousField, "operation edge declares a conflicting source shard",
			map[string]any{"edge_id": e.ID, "declared": e.ShardID, "resolved": sh.ID})
	}
	e.ShardID = sh.ID
	sort.Strings(e.Levels)
	return nil
}

func applyOperationToState(p *model.Project, idx *IndexStore, op model.Operation, bufferLimit, workers int) (*mutationEffects, error) {
	eff := newEffects()
	priorKeyVersions := make(map[string]int, len(idx.Keys))
	for key, ik := range idx.Keys {
		priorKeyVersions[key] = ik.Version
	}
	switch op.Kind {
	case model.OpVertexAdd:
		v := model.CloneVertex(*op.Vertex)
		if findVertexIndex(p, v.ID) >= 0 {
			return nil, model.Err(model.EDuplicateID, "vertex already exists", map[string]any{"vertex_id": v.ID})
		}
		if err := validateVertexPayload(p, &v); err != nil {
			return nil, err
		}
		p.Vertices = append(p.Vertices, v)
		keys := VertexKeys(v, p.IndexRules)
		eff.unionKeys(keys)
		eff.entities[VertexEntity(v.ID)] = true
		eff.shards[v.ShardID] = true
		for _, key := range keys {
			if err := addContribution(idx, v.ShardID, key, VertexEntity(v.ID)); err != nil {
				return nil, err
			}
		}

	case model.OpVertexUpdate:
		i := findVertexIndex(p, op.Vertex.ID)
		if i < 0 {
			return nil, model.Err(model.EMissingVertex, "vertex to update does not exist",
				map[string]any{"vertex_id": op.Vertex.ID})
		}
		old := model.CloneVertex(p.Vertices[i])
		next := model.CloneVertex(*op.Vertex)
		if next.ID != old.ID {
			return nil, model.Err(model.EInvalidOperation, "vertex update changed its id",
				map[string]any{"vertex_id": old.ID, "new_vertex_id": next.ID})
		}
		if err := validateVertexPayload(p, &next); err != nil {
			return nil, err
		}
		oldKeys := VertexKeys(old, p.IndexRules)
		newKeys := VertexKeys(next, p.IndexRules)
		eff.diffKeys(oldKeys, newKeys)
		eff.keys[VertexIdentityKey(old.ID)] = true
		eff.unionKeys(oldKeys, newKeys)
		eff.entities[VertexEntity(old.ID)] = true
		eff.shards[old.ShardID] = true
		removeOnlyDiff(idx, old.ShardID, oldKeys, newKeys, VertexEntity(old.ID))
		addOnlyDiff(idx, next.ShardID, oldKeys, newKeys, VertexEntity(next.ID))
		p.Vertices[i] = next

	case model.OpVertexDelete:
		i := findVertexIndex(p, op.VertexID)
		if i < 0 {
			return nil, model.Err(model.EMissingVertex, "vertex to delete does not exist",
				map[string]any{"vertex_id": op.VertexID})
		}
		old := model.CloneVertex(p.Vertices[i])
		keys := VertexKeys(old, p.IndexRules)
		eff.unionKeys(keys)
		eff.entities[VertexEntity(old.ID)] = true
		eff.shards[old.ShardID] = true
		for _, key := range keys {
			removeContribution(idx, old.ShardID, key, VertexEntity(old.ID))
		}
		var kept []model.Edge
		for _, e := range p.Edges {
			if e.From == old.ID || e.To == old.ID {
				edgeKeys := EdgeKeys(e, p)
				eff.unionKeys(edgeKeys)
				eff.entities[EdgeEntity(e.ID)] = true
				eff.shards[e.ShardID] = true
				eff.cascadeEdges = append(eff.cascadeEdges, e.ID)
				for _, key := range edgeKeys {
					removeContribution(idx, e.ShardID, key, EdgeEntity(e.ID))
				}
				continue
			}
			kept = append(kept, e)
		}
		p.Edges = kept
		p.Vertices = append(p.Vertices[:i], p.Vertices[i+1:]...)

	case model.OpEdgeAdd:
		e := model.CloneEdge(*op.Edge)
		if findEdgeIndex(p, e.ID) >= 0 {
			return nil, model.Err(model.EDuplicateID, "edge already exists", map[string]any{"edge_id": e.ID})
		}
		if err := validateEdgePayload(p, &e); err != nil {
			return nil, err
		}
		p.Edges = append(p.Edges, e)
		keys := EdgeKeys(e, p)
		eff.unionKeys(keys)
		eff.entities[EdgeEntity(e.ID)] = true
		eff.shards[e.ShardID] = true
		for _, key := range keys {
			if err := addContribution(idx, e.ShardID, key, EdgeEntity(e.ID)); err != nil {
				return nil, err
			}
		}

	case model.OpEdgeUpdate:
		i := findEdgeIndex(p, op.Edge.ID)
		if i < 0 {
			return nil, model.Err(model.EInvalidEdge, "edge to update does not exist",
				map[string]any{"edge_id": op.Edge.ID})
		}
		old := model.CloneEdge(p.Edges[i])
		next := model.CloneEdge(*op.Edge)
		if next.ID != old.ID {
			return nil, model.Err(model.EInvalidOperation, "edge update changed its id",
				map[string]any{"edge_id": old.ID, "new_edge_id": next.ID})
		}
		if err := validateEdgePayload(p, &next); err != nil {
			return nil, err
		}
		oldKeys, newKeys := EdgeKeys(old, p), EdgeKeys(next, p)
		eff.unionKeys(oldKeys, newKeys)
		eff.diffKeys(oldKeys, newKeys)
		eff.keys[EdgeIdentityKey(old.ID)] = true
		eff.entities[EdgeEntity(old.ID)] = true
		eff.shards[old.ShardID] = true
		eff.shards[next.ShardID] = true
		if old.ShardID == next.ShardID {
			removeOnlyDiff(idx, old.ShardID, oldKeys, newKeys, EdgeEntity(old.ID))
			addOnlyDiff(idx, next.ShardID, oldKeys, newKeys, EdgeEntity(next.ID))
		} else {
			for _, key := range oldKeys {
				removeContribution(idx, old.ShardID, key, EdgeEntity(old.ID))
			}
			for _, key := range newKeys {
				if err := addContribution(idx, next.ShardID, key, EdgeEntity(next.ID)); err != nil {
					return nil, err
				}
			}
		}
		p.Edges[i] = next

	case model.OpEdgeDelete:
		i := findEdgeIndex(p, op.EdgeID)
		if i < 0 {
			return nil, model.Err(model.EInvalidEdge, "edge to delete does not exist",
				map[string]any{"edge_id": op.EdgeID})
		}
		old := model.CloneEdge(p.Edges[i])
		keys := EdgeKeys(old, p)
		eff.unionKeys(keys)
		eff.entities[EdgeEntity(old.ID)] = true
		eff.shards[old.ShardID] = true
		for _, key := range keys {
			removeContribution(idx, old.ShardID, key, EdgeEntity(old.ID))
		}
		p.Edges = append(p.Edges[:i], p.Edges[i+1:]...)

	case model.OpReshard:
		if err := applyReshard(p, idx, op, eff, bufferLimit, workers); err != nil {
			return nil, err
		}

	default:
		return nil, model.Err(model.EInvalidOperation, "unsupported operation kind",
			map[string]any{"kind": op.Kind, "seq": op.Seq})
	}
	for shardID := range eff.shards {
		if si := idx.Shards[shardID]; si != nil {
			si.Version++
		}
	}
	// If a key was temporarily removed and reintroduced during a shard
	// replacement, restore its old version base before applying this operation's
	// single version increment.
	for key := range eff.keys {
		if ik := idx.Keys[key]; ik != nil {
			if prior := priorKeyVersions[key]; prior > 0 {
				ik.Version = prior
			}
		}
	}
	bumpKeyVersions(idx, eff.keys)
	return eff, nil
}

func removeOnlyDiff(idx *IndexStore, shardID string, oldKeys, newKeys []string, entity string) {
	newSet := keySet(newKeys)
	for _, key := range oldKeys {
		if !newSet[key] {
			removeContribution(idx, shardID, key, entity)
		}
	}
}

func addOnlyDiff(idx *IndexStore, shardID string, oldKeys, newKeys []string, entity string) {
	oldSet := keySet(oldKeys)
	for _, key := range newKeys {
		if !oldSet[key] {
			_ = addContribution(idx, shardID, key, entity)
		}
	}
}

func applyReshard(p *model.Project, idx *IndexStore, op model.Operation, eff *mutationEffects, bufferLimit, workers int) error {
	if len(op.Shards) == 0 {
		return model.Err(model.EInvalidOperation, "reshard operation declares no shards",
			map[string]any{"seq": op.Seq})
	}
	newShards := append([]model.Shard(nil), op.Shards...)
	if err := model.ValidateShardRanges(newShards); err != nil {
		return err
	}
	oldShards := append([]model.Shard(nil), p.Shards...)
	oldAssign := make(map[string]string, len(p.Vertices)+len(p.Edges))
	oldVer := make(map[string]int, len(oldShards))
	for _, sh := range oldShards {
		if si := idx.Shards[sh.ID]; si != nil {
			oldVer[sh.ID] = si.Version
		}
	}
	for _, v := range p.Vertices {
		oldAssign[v.ID] = v.ShardID
	}
	for _, e := range p.Edges {
		oldAssign[e.ID] = e.ShardID
	}
	for i := range p.Vertices {
		p.Vertices[i].ShardID = ""
	}
	for i := range p.Edges {
		p.Edges[i].ShardID = ""
	}
	p.Shards = newShards
	if err := model.AssignShards(p); err != nil {
		return err
	}

	oldSet := make(map[string]model.Shard, len(oldShards))
	newSet := make(map[string]model.Shard, len(newShards))
	for _, sh := range oldShards {
		oldSet[sh.ID] = sh
	}
	for _, sh := range newShards {
		newSet[sh.ID] = sh
	}
	var affectedOld, affectedNew []string
	for id, sh := range oldSet {
		if ns, ok := newSet[id]; !ok || ns != sh {
			affectedOld = append(affectedOld, id)
		}
	}
	for id, sh := range newSet {
		if os, ok := oldSet[id]; !ok || os != sh {
			affectedNew = append(affectedNew, id)
		}
	}
	sort.Strings(affectedOld)
	sort.Strings(affectedNew)

	maxOldVersion := 0
	for _, id := range affectedOld {
		if v := oldVer[id]; v > maxOldVersion {
			maxOldVersion = v
		}
		eff.shards[id] = true
		eff.unionKeys(keySetToSlice(removeShardNoBump(idx, id)))
	}
	for _, v := range p.Vertices {
		if oldAssign[v.ID] != v.ShardID {
			eff.entities[VertexEntity(v.ID)] = true
		}
	}
	for _, e := range p.Edges {
		if oldAssign[e.ID] != e.ShardID {
			eff.entities[EdgeEntity(e.ID)] = true
		}
	}
	type builtShard struct {
		id     string
		index  *ShardIndex
		metric ShardMetric
		err    error
	}
	built := make([]builtShard, len(affectedNew))
	for _, id := range affectedNew {
		eff.shards[id] = true
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	workerCount := workers
	if workerCount > len(affectedNew) {
		workerCount = len(affectedNew)
	}
	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := range jobs {
				id := affectedNew[i]
				start := time.Now()
				si, err := buildShard(p, id, bufferLimit)
				built[i] = builtShard{id: id, index: si, err: err}
				if si != nil {
					built[i].metric = ShardMetric{
						ShardID: id, Micros: time.Since(start).Microseconds(),
						KeyCount: len(si.Contributions), Worker: workerID,
					}
					for _, ents := range si.Contributions {
						built[i].metric.EntityRef += len(ents)
					}
				}
			}
		}(w)
	}
	for i := range affectedNew {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for i := range built {
		if built[i].err != nil {
			return built[i].err
		}
		id := built[i].id
		si := built[i].index
		if oldVersion, ok := oldVer[id]; ok {
			si.Version = oldVersion + 1
		} else if maxOldVersion > 0 {
			si.Version = maxOldVersion + 1
		} else {
			si.Version = 1
		}
		eff.unionKeys(keySetToSlice(addShardNoBump(idx, si)))
		eff.metrics = append(eff.metrics, built[i].metric)
	}
	eff.shardDeps = true
	return nil
}

func keySetToSlice(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func invalidateCaches(cache *CacheStore, affected map[string]bool) []CacheInvalidation {
	ids := make([]string, 0, len(cache.Entries))
	for id := range cache.Entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []CacheInvalidation
	for _, id := range ids {
		entry := cache.Entries[id]
		if hit, reasons := depsIntersect(entry.Dependencies, affected); hit {
			out = append(out, CacheInvalidation{
				CacheID: entry.CacheID, QueryID: entry.QueryID, Reasons: reasons,
			})
			delete(cache.Entries, id)
		}
	}
	return out
}

func buildCacheAffected(eff *mutationEffects) map[string]bool {
	affected := make(map[string]bool)
	for key := range eff.keys {
		affected[key] = true
	}
	for entity := range eff.entities {
		if key := EntityIdentityKey(entity); key != "" {
			affected[key] = true
		}
	}
	if eff.shardDeps {
		for shard := range eff.shards {
			affected[ShardDependency(shard)] = true
		}
	}
	return affected
}

func (r *Runtime) ApplyOperation(op model.Operation, layout string) (OperationReport, error) {
	start := time.Now()
	if op.Seq <= 0 {
		return OperationReport{}, model.Err(model.EInvalidOperation, "operation sequence must be positive",
			map[string]any{"seq": op.Seq})
	}
	if op.Seq != r.CommitSeq+1 {
		return OperationReport{}, model.Err(model.EInvalidOperation,
			"operation sequence is not the next committed sequence", map[string]any{
				"seq": op.Seq, "expected": r.CommitSeq + 1,
			})
	}
	if layout != LayoutIndex && layout != LayoutScan {
		layout = LayoutIndex
	}
	p := model.CloneProject(r.Project)
	idx := r.Index.Clone()
	cache := r.Cache.Clone()
	eff, err := applyOperationToState(p, idx, op, r.Options.BufferPerWorker, r.Options.Workers)
	if err != nil {
		return OperationReport{}, err
	}
	p.Version++
	model.SortProject(p)
	affected := buildCacheAffected(eff)
	invalidated := invalidateCaches(cache, affected)
	candidate := &Runtime{
		Project: p, Options: r.Options, Index: idx, Cache: cache,
		CommitSeq: op.Seq, Metrics: eff.metrics,
	}
	queries, err := candidate.EvaluateAll(layout)
	if err != nil {
		return OperationReport{}, err
	}
	rep := OperationReport{
		Seq: op.Seq, Kind: op.Kind, ProjectVersion: p.Version,
		CommitSeq: candidate.CommitSeq, AffectedShards: keySetToSlice(eff.shards),
		ChangedEntities:    keySetToSlice(eff.entities),
		AffectedIndexKeys:  keySetToSlice(eff.keys),
		PropagationClosure: keySetToSlice(affected),
		InvalidatedCaches:  invalidated,
		IndexKeys:          candidate.KeyViewsWithCaches(),
		Shards:             candidate.ShardViews(nil),
		QueryResults:       queries,
		ApplyMicros:        time.Since(start).Microseconds(),
		ShardMetrics:       eff.metrics,
	}
	for _, entry := range candidate.Cache.EntriesSorted() {
		if entry.Reused {
			rep.ReusedCacheIDs = append(rep.ReusedCacheIDs, entry.CacheID)
		} else {
			rep.RebuiltCacheIDs = append(rep.RebuiltCacheIDs, entry.CacheID)
		}
	}
	r.Project, r.Index, r.Cache = candidate.Project, candidate.Index, candidate.Cache
	r.CommitSeq = candidate.CommitSeq
	r.Metrics = candidate.Metrics
	return rep, nil
}

func (r *Runtime) ApplyOperations(ops []model.Operation, layout string) ([]OperationReport, error) {
	reports := make([]OperationReport, 0, len(ops))
	for _, op := range ops {
		rep, err := r.ApplyOperation(op, layout)
		if err != nil {
			return reports, err
		}
		reports = append(reports, rep)
	}
	return reports, nil
}

func FormatShardMetric(m ShardMetric) string {
	return fmt.Sprintf("%s=%dus/keys:%d/refs:%d/worker:%d",
		m.ShardID, m.Micros, m.KeyCount, m.EntityRef, m.Worker)
}
