package engine

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"graphlab/internal/model"
)

const MaxWorkers = 32

type BuildOptions struct {
	Workers         int
	BufferPerWorker int
}

func DefaultBuildOptions() BuildOptions {
	return BuildOptions{Workers: 4, BufferPerWorker: 4096}
}

func (o BuildOptions) Validate() error {
	if o.Workers <= 0 || o.Workers > MaxWorkers {
		return model.Err(model.EThreadBudget, "worker budget is outside the supported range", map[string]any{
			"workers": o.Workers, "min": 1, "max": MaxWorkers,
		})
	}
	if o.BufferPerWorker <= 0 {
		return model.Err(model.EBufferBudget, "per-worker index buffer budget must be positive", map[string]any{
			"buffer_per_worker": o.BufferPerWorker,
		})
	}
	return nil
}

type ShardIndex struct {
	ID            string              `json:"id"`
	Version       int                 `json:"version"`
	VertexOrder   []string            `json:"vertex_order"`
	EdgeOrder     []string            `json:"edge_order"`
	Contributions map[string][]string `json:"contributions"`
}

type IndexKey struct {
	Key       string   `json:"key"`
	Version   int      `json:"version"`
	RefShards []string `json:"ref_shards"`
	Entities  []string `json:"entities"`
}

type IndexStore struct {
	Shards map[string]*ShardIndex
	Keys   map[string]*IndexKey
}

type ShardMetric struct {
	ShardID   string `json:"shard_id"`
	Micros    int64  `json:"micros"`
	KeyCount  int    `json:"key_count"`
	EntityRef int    `json:"entity_refs"`
	Worker    int    `json:"worker"`
}

type IndexKeyView struct {
	Key       string   `json:"key"`
	Version   int      `json:"version"`
	RefShards []string `json:"ref_shards"`
	Entities  []string `json:"entities"`
	CacheIDs  []string `json:"cache_ids,omitempty"`
}

func buildShard(p *model.Project, id string, bufferLimit int) (*ShardIndex, error) {
	si := &ShardIndex{
		ID: id, Version: 1,
		Contributions: make(map[string][]string),
	}
	var vertices []model.Vertex
	var edges []model.Edge
	for _, v := range p.Vertices {
		if v.ShardID == id {
			vertices = append(vertices, v)
		}
	}
	for _, e := range p.Edges {
		if e.ShardID == id {
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
	si.VertexOrder = make([]string, len(vertices))
	for i, v := range vertices {
		si.VertexOrder[i] = v.ID
		for _, key := range VertexKeys(v, p.IndexRules) {
			si.Contributions[key] = append(si.Contributions[key], VertexEntity(v.ID))
			if len(si.Contributions) > bufferLimit {
				return nil, model.Err(model.EBufferBudget,
					"per-worker index build buffer exhausted", map[string]any{
						"shard_id": id, "limit": bufferLimit,
						"observed_keys": len(si.Contributions), "stage": "vertices",
					})
			}
		}
	}
	si.EdgeOrder = make([]string, len(edges))
	for i, e := range edges {
		si.EdgeOrder[i] = e.ID
		for _, key := range EdgeKeys(e, p) {
			si.Contributions[key] = append(si.Contributions[key], EdgeEntity(e.ID))
			if len(si.Contributions) > bufferLimit {
				return nil, model.Err(model.EBufferBudget,
					"per-worker index build buffer exhausted", map[string]any{
						"shard_id": id, "limit": bufferLimit,
						"observed_keys": len(si.Contributions), "stage": "edges",
					})
			}
		}
	}
	for key, entities := range si.Contributions {
		si.Contributions[key] = sortedUnique(entities)
	}
	return si, nil
}

func BuildIndex(p *model.Project, opts BuildOptions) (*IndexStore, []ShardMetric, error) {
	if err := opts.Validate(); err != nil {
		return nil, nil, err
	}
	if err := model.AssignShards(p); err != nil {
		return nil, nil, err
	}
	shardIDs := model.ShardIDs(p)
	store := &IndexStore{
		Shards: make(map[string]*ShardIndex, len(shardIDs)),
		Keys:   make(map[string]*IndexKey),
	}
	type result struct {
		index  *ShardIndex
		metric ShardMetric
		err    error
	}
	results := make([]result, len(shardIDs))
	jobs := make(chan int)
	var wg sync.WaitGroup
	workerCount := opts.Workers
	if workerCount > len(shardIDs) {
		workerCount = len(shardIDs)
	}
	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := range jobs {
				start := time.Now()
				si, err := buildShard(p, shardIDs[i], opts.BufferPerWorker)
				results[i].index = si
				results[i].err = err
				results[i].metric = ShardMetric{
					ShardID: shardIDs[i],
					Micros:  time.Since(start).Microseconds(),
					Worker:  workerID,
				}
				if si != nil {
					results[i].metric.KeyCount = len(si.Contributions)
					for _, ents := range si.Contributions {
						results[i].metric.EntityRef += len(ents)
					}
				}
			}
		}(w)
	}
	for i := range shardIDs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	metrics := make([]ShardMetric, 0, len(shardIDs))
	for i := range results {
		if results[i].err != nil {
			return nil, nil, results[i].err
		}
		store.Shards[shardIDs[i]] = results[i].index
		metrics = append(metrics, results[i].metric)
	}
	// Merge in sorted shard order after parallel builds. This is the commit
	// barrier: concurrency and batch arrival order do not influence references.
	for _, id := range shardIDs {
		si := store.Shards[id]
		for key, entities := range si.Contributions {
			ik := store.Keys[key]
			if ik == nil {
				ik = &IndexKey{Key: key, RefShards: []string{}}
				store.Keys[key] = ik
			}
			ik.RefShards = append(ik.RefShards, id)
			ik.Entities = append(ik.Entities, entities...)
		}
	}
	for _, ik := range store.Keys {
		ik.Version = 1
		ik.RefShards = sortedUnique(ik.RefShards)
		ik.Entities = sortedUnique(ik.Entities)
	}
	return store, metrics, nil
}

func (s *IndexStore) KeyViews() []IndexKeyView {
	keys := make([]string, 0, len(s.Keys))
	for key := range s.Keys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]IndexKeyView, 0, len(keys))
	for _, key := range keys {
		ik := s.Keys[key]
		out = append(out, IndexKeyView{
			Key: ik.Key, Version: ik.Version,
			RefShards: append([]string(nil), ik.RefShards...),
			Entities:  append([]string(nil), ik.Entities...),
		})
	}
	return out
}

func (s *IndexStore) ShardKeys(id string) []string {
	si := s.Shards[id]
	if si == nil {
		return nil
	}
	keys := make([]string, 0, len(si.Contributions))
	for key := range si.Contributions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (s *IndexStore) Clone() *IndexStore {
	out := &IndexStore{
		Shards: make(map[string]*ShardIndex, len(s.Shards)),
		Keys:   make(map[string]*IndexKey, len(s.Keys)),
	}
	for id, si := range s.Shards {
		cp := &ShardIndex{
			ID: si.ID, Version: si.Version,
			VertexOrder:   append([]string(nil), si.VertexOrder...),
			EdgeOrder:     append([]string(nil), si.EdgeOrder...),
			Contributions: make(map[string][]string, len(si.Contributions)),
		}
		for key, ents := range si.Contributions {
			cp.Contributions[key] = append([]string(nil), ents...)
		}
		out.Shards[id] = cp
	}
	for key, ik := range s.Keys {
		out.Keys[key] = &IndexKey{
			Key: ik.Key, Version: ik.Version,
			RefShards: append([]string(nil), ik.RefShards...),
			Entities:  append([]string(nil), ik.Entities...),
		}
	}
	return out
}

func (s *IndexStore) StringSummary() string {
	return fmt.Sprintf("shards=%d keys=%d", len(s.Shards), len(s.Keys))
}
