package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"lip-pipeline-prototype/internal/cachegraph"
	"lip-pipeline-prototype/internal/checkpoint"
	"lip-pipeline-prototype/internal/domain"
	"lip-pipeline-prototype/internal/generate"
	"lip-pipeline-prototype/internal/hashx"
	"lip-pipeline-prototype/internal/pipeline"
)

type Service struct {
	mu      sync.RWMutex
	sample  *generate.Sample
	project *domain.Project
	store   *checkpoint.Store

	order   []string
	results map[string]*domain.ShardResult
	graphs  map[string]*cachegraph.Graph
}

func New(ctx context.Context, dbPath string) (*Service, error) {
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." && dbPath != ":memory:" {
		if err := mkdirAll(dir); err != nil {
			return nil, err
		}
	}
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	sample, err := generate.BuildSample()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	svc := &Service{
		sample: sample, project: sample.Project, store: store,
		results: map[string]*domain.ShardResult{}, graphs: map[string]*cachegraph.Graph{},
	}
	for _, id := range sample.TruthOrder {
		truth := sample.TruthByShard[id]
		_, shard, err := svc.findShard(id)
		if err != nil {
			return nil, err
		}
		graph := cachegraph.BuildShardGraph(truth.ContentHash, *shard, truth)
		svc.order = append(svc.order, id)
		svc.results[id] = truth
		svc.graphs[id] = graph
		if err := svc.persistGraph(ctx, graph); err != nil {
			return nil, err
		}
	}
	return svc, nil
}

func (s *Service) Close() error { return s.store.Close() }

func (s *Service) Store() *checkpoint.Store { return s.store }

func (s *Service) Sample() *generate.Sample { return s.sample }

func (s *Service) Project() *domain.Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.project
}

func (s *Service) findShard(shardID string) (*domain.Dialogue, *domain.Shard, error) {
	for di := range s.project.Dialogues {
		dlg := &s.project.Dialogues[di]
		for si := range dlg.Shards {
			if dlg.Shards[si].ID == shardID {
				return dlg, &dlg.Shards[si], nil
			}
		}
	}
	return nil, nil, &domain.Error{Code: domain.CodeNotFound, Message: "shard not found", Path: shardID}
}

func (s *Service) persistGraph(ctx context.Context, graph *cachegraph.Graph) error {
	for _, node := range graph.Nodes {
		payload, err := json.Marshal(node)
		if err != nil {
			return err
		}
		if err := s.store.PutCache(ctx, checkpoint.CacheEntry{
			CacheKey: node.CacheKey, NodeID: node.ID, Kind: string(node.Kind),
			ShardID: graph.ShardID, SourceVersion: string(node.SourceVersion),
			PayloadJSON: string(payload),
		}); err != nil {
			return err
		}
	}
	return nil
}

type Overview struct {
	Project      *domain.Project  `json:"project"`
	Versions     []VersionSummary `json:"versions"`
	TruthShards  int              `json:"truth_shards"`
	TruthPacks   int              `json:"truth_packs"`
	TruthFrames  int              `json:"truth_frames"`
	CommitOrder  []string         `json:"commit_order"`
	CacheEntries int              `json:"cache_entries"`
}

type VersionSummary struct {
	Version     domain.SchemaVersion `json:"version"`
	Fills       int                  `json:"fills"`
	Conversions int                  `json:"conversions"`
	Losses      int                  `json:"losses"`
	LoadOK      bool                 `json:"load_ok"`
}

func (s *Service) Overview(ctx context.Context) (*Overview, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ov := &Overview{Project: s.project, TruthShards: len(s.order)}
	for _, v := range []domain.SchemaVersion{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3} {
		loaded := s.sample.Loaded[v]
		summary := VersionSummary{Version: v, LoadOK: loaded != nil}
		if loaded != nil {
			summary.Fills = len(loaded.Report.Fills)
			summary.Conversions = len(loaded.Report.Conversions)
		}
		summary.Losses = len(s.sample.Exports[v].Losses)
		ov.Versions = append(ov.Versions, summary)
	}
	for _, id := range s.order {
		r := s.results[id]
		ov.TruthPacks += len(r.Packs)
		ov.TruthFrames += len(r.Frames)
	}
	order, err := s.store.CommitOrder(ctx)
	if err != nil {
		return nil, err
	}
	ov.CommitOrder = order
	cache, err := s.store.ListCache(ctx, "")
	if err != nil {
		return nil, err
	}
	ov.CacheEntries = len(cache)
	return ov, nil
}

func (s *Service) Document(version domain.SchemaVersion) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.sample.Documents[version]
	if !ok {
		return nil, &domain.Error{Code: domain.CodeNotFound, Message: "document not found"}
	}
	return data, nil
}

type ShardDetail struct {
	Shard   *domain.Shard           `json:"shard"`
	Result  *domain.ShardResult     `json:"result"`
	Graph   *cachegraph.Graph       `json:"graph"`
	Truth   *domain.ShardResult     `json:"truth,omitempty"`
	Machine *checkpoint.Machine     `json:"machine,omitempty"`
	Cache   []checkpoint.CacheEntry `json:"cache,omitempty"`
}

func (s *Service) ShardDetail(ctx context.Context, shardID string) (*ShardDetail, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, shard, err := s.findShard(shardID)
	if err != nil {
		return nil, err
	}
	machine, err := s.store.LoadMachine(ctx, shardID)
	if err != nil {
		return nil, err
	}
	cache, err := s.store.ListCache(ctx, shardID)
	if err != nil {
		return nil, err
	}
	return &ShardDetail{
		Shard: shard, Result: s.results[shardID], Graph: s.graphs[shardID],
		Truth: s.sample.TruthByShard[shardID], Machine: machine, Cache: cache,
	}, nil
}

func (s *Service) DialogueDetail(dialogueID string) (*domain.Dialogue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.project.Dialogues {
		if s.project.Dialogues[i].ID == dialogueID {
			return &s.project.Dialogues[i], nil
		}
	}
	return nil, &domain.Error{Code: domain.CodeNotFound, Message: "dialogue not found", Path: dialogueID}
}

type PreviewRequest struct {
	ShardID     string `json:"shard_id"`
	FPS         *int   `json:"fps,omitempty"`
	MaxPhonemes *int   `json:"max_phonemes_per_pack,omitempty"`
	Tolerance   *int   `json:"transition_tolerance,omitempty"`
	FrameStride *int   `json:"frame_stride,omitempty"`
}

type PreviewResponse struct {
	Preview    *domain.PreviewResult `json:"preview"`
	Exact      *domain.ShardResult   `json:"exact"`
	Consistent bool                  `json:"consistent"`
	Divergence *domain.Error         `json:"divergence,omitempty"`
}

func (s *Service) Preview(req PreviewRequest) (*PreviewResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	dlg, shard, err := s.findShard(req.ShardID)
	if err != nil {
		return nil, err
	}
	params := s.project.Params
	if req.FPS != nil {
		params.FPS = *req.FPS
	}
	if req.MaxPhonemes != nil {
		params.MaxPhonemesPerPack = *req.MaxPhonemes
	}
	if req.Tolerance != nil {
		params.TransitionTolerance = *req.Tolerance
	}
	if req.FrameStride != nil {
		params.PreviewFrameStride = *req.FrameStride
	}
	params.ParamVersion = cachegraph.ParamVersion(params)
	preview, err := pipeline.BuildPreview(*shard, params)
	if err != nil {
		return nil, err
	}
	exact, err := pipeline.BuildShard(*shard, params, dlg.ContentHash, dlg.SourceVersion)
	if err != nil {
		return nil, err
	}
	resp := &PreviewResponse{Preview: preview, Exact: exact, Consistent: true}
	if err := pipeline.ComparePreviewExact(preview, exact); err != nil {
		if de, ok := domain.AsError(err); ok && de.Code == domain.CodeDiverge {
			resp.Consistent = false
			resp.Divergence = de
		} else {
			return nil, err
		}
	}
	return resp, nil
}

type EditRequest struct {
	PhonemeID string `json:"phoneme_id,omitempty"`
	Field     string `json:"field,omitempty"`
	Value     int64  `json:"value,omitempty"`
	FPS       int    `json:"fps,omitempty"`
}

type EditResponse struct {
	Reports []*cachegraph.InvalidationReport `json:"reports"`
	Project *domain.Project                  `json:"project"`
}

func (s *Service) Edit(ctx context.Context, req EditRequest) (*EditResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.FPS > 0 {
		s.project.Params.FPS = req.FPS
		s.project.Params.ParamVersion = cachegraph.ParamVersion(s.project.Params)
		resp := &EditResponse{Project: s.project}
		for _, id := range s.order {
			dlg, shard, err := s.findShard(id)
			if err != nil {
				return nil, err
			}
			report, err := s.rebuildShardLocked(ctx, dlg, shard)
			if err != nil {
				return nil, err
			}
			resp.Reports = append(resp.Reports, report)
		}
		return resp, nil
	}

	if req.PhonemeID == "" || req.Field == "" {
		return nil, &domain.Error{Code: domain.CodeSchema, Message: "phoneme_id and field or positive fps required", Path: "edit"}
	}
	dlg, shard, phonemeIndex, err := s.findPhoneme(req.PhonemeID)
	if err != nil {
		return nil, err
	}
	oldValue, applyErr := s.applyEdit(&shard.Phonemes[phonemeIndex], req.Field, req.Value)
	if applyErr != nil {
		return nil, applyErr
	}
	oldTotal := shard.TotalSamples
	oldPCMCount := shard.PCM.SampleCount
	if req.Field == "end_sample" && phonemeIndex == len(shard.Phonemes)-1 {
		shard.TotalSamples = shard.Phonemes[phonemeIndex].EndSample
		shard.PCM.SampleCount = shard.TotalSamples
		shard.PCM.ByteCount = shard.TotalSamples * 2
	}
	dlg.ContentHash = hashx.JSON(struct {
		ID     string
		Shards []domain.Shard
	}{dlg.ID, dlg.Shards})
	report, err := s.rebuildShardLocked(ctx, dlg, shard)
	if err != nil {
		s.revertEdit(&shard.Phonemes[phonemeIndex], req.Field, oldValue)
		shard.TotalSamples = oldTotal
		shard.PCM.SampleCount = oldPCMCount
		shard.PCM.ByteCount = oldPCMCount * 2
		dlg.ContentHash = hashx.JSON(struct {
			ID     string
			Shards []domain.Shard
		}{dlg.ID, dlg.Shards})
		_, _ = s.rebuildShardLocked(ctx, dlg, shard)
		return nil, err
	}
	return &EditResponse{Reports: []*cachegraph.InvalidationReport{report}, Project: s.project}, nil
}

func (s *Service) findPhoneme(phonemeID string) (*domain.Dialogue, *domain.Shard, int, error) {
	for di := range s.project.Dialogues {
		dlg := &s.project.Dialogues[di]
		for si := range dlg.Shards {
			shard := &dlg.Shards[si]
			for pi := range shard.Phonemes {
				if shard.Phonemes[pi].ID == phonemeID {
					return dlg, shard, pi, nil
				}
			}
		}
	}
	return nil, nil, -1, &domain.Error{Code: domain.CodeNotFound, Message: "phoneme not found", Path: phonemeID}
}

func (s *Service) applyEdit(ph *domain.Phoneme, field string, value int64) (any, error) {
	switch field {
	case "start_sample":
		old := ph.StartSample
		ph.StartSample = value
		return old, nil
	case "end_sample":
		old := ph.EndSample
		ph.EndSample = value
		return old, nil
	case "weight_permyriad":
		if value < 0 || value > 10000 {
			return nil, &domain.Error{Code: domain.CodeVal, Message: "weight must be [0,10000]", Path: field, Actual: value}
		}
		old := ph.WeightPermyriad
		ph.WeightPermyriad = int(value)
		return old, nil
	case "affinity_to_next":
		if value < 0 || value > 10000 {
			return nil, &domain.Error{Code: domain.CodeVal, Message: "affinity must be [0,10000]", Path: field, Actual: value}
		}
		old := ph.AffinityToNext
		ph.AffinityToNext = int(value)
		ph.AffinityExplicit = true
		return old, nil
	default:
		return nil, &domain.Error{Code: domain.CodeSchema, Message: "unsupported edit field", Path: "field", Actual: field}
	}
}

func (s *Service) revertEdit(ph *domain.Phoneme, field string, old any) {
	switch field {
	case "start_sample":
		ph.StartSample = old.(int64)
	case "end_sample":
		ph.EndSample = old.(int64)
	case "weight_permyriad":
		ph.WeightPermyriad = old.(int)
	case "affinity_to_next":
		ph.AffinityToNext = old.(int)
	}
}

func (s *Service) rebuildShardLocked(ctx context.Context, dlg *domain.Dialogue, shard *domain.Shard) (*cachegraph.InvalidationReport, error) {
	oldResult := s.results[shard.ID]
	oldGraph := s.graphs[shard.ID]
	newResult, err := pipeline.BuildShard(*shard, s.project.Params, dlg.ContentHash, dlg.SourceVersion)
	if err != nil {
		return nil, err
	}
	newGraph := cachegraph.BuildShardGraph(dlg.ContentHash, *shard, newResult)
	report := cachegraph.DiffAndPlan(oldGraph, oldResult, newResult, newGraph, dlg.SourceVersion)

	for i := range report.Reused {
		item := &report.Reused[i]
		if item.OldCacheKey != "" {
			entry, gerr := s.store.GetCache(ctx, item.OldCacheKey)
			if gerr == nil && entry != nil {
				item.PersistentHit = true
				entry.CacheKey = item.CacheKey
				_ = s.store.PutCache(ctx, *entry)
			}
		}
	}
	for _, item := range report.Invalidated {
		if item.OldCacheKey != "" {
			_ = s.store.DeleteCache(ctx, item.OldCacheKey)
		}
	}
	if err := s.persistGraph(ctx, newGraph); err != nil {
		return nil, err
	}
	s.results[shard.ID] = newResult
	s.graphs[shard.ID] = newGraph
	return report, nil
}

func mkdirAll(path string) error {
	return os.MkdirAll(path, 0o755)
}
