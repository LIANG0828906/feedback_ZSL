package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"terrainpipeline/internal/bake"
	"terrainpipeline/internal/budget"
	"terrainpipeline/internal/checkpoint"
	"terrainpipeline/internal/depgraph"
	"terrainpipeline/internal/model"
	"terrainpipeline/internal/seams"
)

var ErrCrash = errors.New("E_CRASH: injected interruption after durable commit")

type BlockStat struct {
	BlockID      string `json:"block_id"`
	CacheHit     bool   `json:"cache_hit"`
	Recovered    bool   `json:"recovered"`
	Rebaked      bool   `json:"rebaked"`
	Milliseconds int64  `json:"milliseconds"`
	PixelCount   int    `json:"pixel_count"`
	NormalBytes  int64  `json:"normal_bytes"`
}

type RunOptions struct {
	Edits             []depgraph.Edit   `json:"edits,omitempty"`
	Generation        string            `json:"generation,omitempty"`
	Resume            bool              `json:"resume,omitempty"`
	CrashAfterCommits int               `json:"crash_after_commits,omitempty"`
	FailBlocks        map[string]string `json:"fail_blocks,omitempty"`
	Budget            budget.Options    `json:"budget,omitempty"`
}

type Outcome struct {
	Generation   string                        `json:"generation"`
	ProjectHash  string                        `json:"project_hash"`
	CacheHashes  map[string]string             `json:"cache_hashes"`
	Graph        *depgraph.Graph               `json:"graph"`
	Plan         *depgraph.Plan                `json:"plan"`
	Seams        []*model.SeamReport           `json:"seams"`
	SeamErrors   []*model.Error                `json:"seam_errors,omitempty"`
	FailedBlocks map[string]*model.Error       `json:"failed_blocks,omitempty"`
	QueueEvents  []budget.Event                `json:"queue_events,omitempty"`
	Commit       []checkpoint.CommitEntry      `json:"commit,omitempty"`
	Stats        map[string]BlockStat          `json:"stats"`
	Crash        bool                          `json:"crash,omitempty"`
	Results      map[string]*model.BlockResult `json:"-"`
	Raw          map[string]*model.BlockResult `json:"-"`
}

type Service struct {
	root  string
	store *checkpoint.Store
	mu    sync.Mutex
}

func NewService(root string, store *checkpoint.Store) *Service {
	return &Service{root: root, store: store}
}

func (s *Service) Run(ctx context.Context, project *model.Project, opts RunOptions) (*Outcome, error) {
	budgetOpts := opts.Budget.WithDefaults()
	projectHash := model.ProjectContentHash(project)
	generation := opts.Generation
	if generation == "" {
		generation = "gen-" + projectHash[:20]
	}
	cacheHashes := map[string]string{}
	for _, block := range project.Blocks {
		cacheHashes[block.ID] = model.BlockCacheHash(project, block)
	}
	graph := depgraph.Build(project)

	validCache := map[string]bool{}
	var heads []checkpoint.CommitEntry
	if s.store != nil {
		var err error
		heads, err = s.store.HeadSequence()
		if err != nil {
			return nil, err
		}
		for _, head := range heads {
			if head.ParamVersion != project.ParamVersion || head.AlgoVersion != model.AlgorithmVersion {
				continue
			}
			if head.ContentHash != cacheHashes[head.BlockID] {
				continue
			}
			if fileExists(s.rawCachePath(head.ContentHash)) && fileExists(s.finalCachePath(head.ResultHash)) {
				validCache[head.BlockID] = true
			}
		}
	}

	plan := graph.Plan(opts.Edits, validCache)
	outcome := &Outcome{
		Generation: generation, ProjectHash: projectHash, CacheHashes: cacheHashes,
		Graph: graph, Plan: plan, Stats: map[string]BlockStat{},
		FailedBlocks: map[string]*model.Error{},
		Results:      map[string]*model.BlockResult{}, Raw: map[string]*model.BlockResult{},
	}

	committed := map[string]string{}
	if s.store != nil && opts.Resume {
		var err error
		committed, err = s.store.CommittedHashes(generation)
		if err != nil {
			return nil, err
		}
	}

	recovered := map[string]bool{}
	var pending []string
	for _, id := range plan.WillBake {
		if opts.Resume && committed[id] == cacheHashes[id] && fileExists(s.rawCachePath(cacheHashes[id])) {
			recovered[id] = true
			continue
		}
		pending = append(pending, id)
	}
	sort.Strings(pending)
	pending = depgraph.TopologicalBlockOrder(project, graph, pending)

	scheduler := budget.NewScheduler(budgetOpts)
	tasks := make([]budget.Task, 0, len(pending))
	for _, id := range pending {
		block := project.BlockByID(id)
		tasks = append(tasks, budget.Task{
			ID: id, PixelCount: block.NX * block.NZ,
			NormalBytes: int64(block.NX*block.NZ) * 32,
			Run: func(scratch []model.PixelBake) error {
				return s.bakeTask(project, block, cacheHashes[id], scratch, opts.FailBlocks, outcome)
			},
		})
	}
	taskErrs := scheduler.Run(ctx, tasks)
	outcome.QueueEvents = scheduler.Events()
	for id, err := range taskErrs {
		te := &model.Error{Code: model.CodeIO, BlockID: id, Field: "bake", Message: err.Error()}
		var budgetErr *model.Error
		if errors.As(err, &budgetErr) {
			te = budgetErr
		}
		outcome.FailedBlocks[id] = te
	}

	for _, id := range plan.WillBake {
		stat := outcome.Stats[id]
		stat.Recovered = recovered[id]
		outcome.Stats[id] = stat
	}

	for _, block := range project.Blocks {
		id := block.ID
		if outcome.Raw[id] != nil {
			continue
		}
		if _, failed := outcome.FailedBlocks[id]; failed {
			continue
		}
		raw, err := loadJSON[model.BlockResult](s.rawCachePath(cacheHashes[id]))
		if err != nil {
			outcome.FailedBlocks[id] = model.NewError(model.CodeIO, id, "cache.raw", "原始法线缓存不可读: %v", err)
			continue
		}
		outcome.Raw[id] = raw
		if recovered[id] {
			stat := outcome.Stats[id]
			stat.Recovered = true
			stat.CacheHit = true
			outcome.Stats[id] = stat
		}
	}

	reports, seamErrs := seams.Analyze(project, outcome.Raw)
	outcome.Seams = reports
	outcome.SeamErrors = seamErrs

	for _, block := range project.Blocks {
		id := block.ID
		if _, failed := outcome.FailedBlocks[id]; failed || outcome.Raw[id] == nil {
			continue
		}
		if plan.Entries == nil {
			// no-op
		}
		entry := findPlanEntry(plan, id)
		if entry.Reused && validCache[id] {
			head := findHead(heads, id)
			final, err := loadJSON[model.BlockResult](s.finalCachePath(head.ResultHash))
			if err == nil {
				outcome.Results[id] = final
				stat := outcome.Stats[id]
				stat.CacheHit = true
				outcome.Stats[id] = stat
				continue
			}
		}

		final := cloneBlockResult(outcome.Raw[id])
		seams.ComposeBlock(project, id, final, outcome.Raw, reports)
		bake.Finalize(final)
		if err := writeJSONAtomic(s.finalCachePath(final.ResultHash), final); err != nil {
			return outcome, err
		}
		outcome.Results[id] = final
	}
	seams.RecomputeWeldedAngles(project, outcome.Results, reports)

	if s.store != nil {
		for _, id := range pending {
			result := outcome.Results[id]
			if result == nil {
				continue
			}
			var incident []*model.SeamReport
			for _, report := range reports {
				if report.BlockA == id || report.BlockB == id {
					incident = append(incident, report)
				}
			}
			seamJSON, err := json.Marshal(incident)
			if err != nil {
				return outcome, err
			}
			seamDigest := model.DigestBytes("seam-json", seamJSON)
			rec := checkpoint.BlockRecord{
				Generation: outcome.Generation, BlockID: id, ContentHash: cacheHashes[id],
				ParamVersion: project.ParamVersion, AlgoVersion: model.AlgorithmVersion,
				NormalDigest: result.NormalDigest, SlopeDigest: result.SlopeDigest,
				WeightDigest: result.WeightDigest, ResultHash: result.ResultHash,
				SeamDigest: seamDigest, SeamJSON: string(seamJSON),
				Dependencies: graph.BlockDependencies[id],
			}
			entry, err := s.store.CommitBlock(rec)
			if err != nil {
				return outcome, err
			}
			outcome.Commit = append(outcome.Commit, entry)
			if opts.CrashAfterCommits > 0 && len(outcome.Commit) >= opts.CrashAfterCommits {
				outcome.Crash = true
				return outcome, ErrCrash
			}
		}
	}

	return outcome, nil
}

func (s *Service) bakeTask(project *model.Project, block *model.Block, cacheHash string, scratch []model.PixelBake, failures map[string]string, outcome *Outcome) (err error) {
	start := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			err = model.NewError(model.CodeIO, block.ID, "bake.panic", "%v", recovered)
		}
	}()
	if msg, ok := failures[block.ID]; ok {
		return errors.New(msg)
	}
	result := bake.BakeBlockTiled(project, block, scratch)
	if err := writeJSONAtomic(s.rawCachePath(cacheHash), result); err != nil {
		return err
	}
	s.mu.Lock()
	outcome.Raw[block.ID] = result
	outcome.Stats[block.ID] = BlockStat{
		BlockID: block.ID, Rebaked: true, Milliseconds: time.Since(start).Milliseconds(),
		PixelCount: block.NX * block.NZ, NormalBytes: result.NormalBytes,
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) rawCachePath(hash string) string {
	return filepath.Join(s.root, "cache", "raw", hash+".json")
}

func (s *Service) finalCachePath(hash string) string {
	return filepath.Join(s.root, "cache", "final", hash+".json")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func findPlanEntry(plan *depgraph.Plan, id string) depgraph.PlanEntry {
	for _, entry := range plan.Entries {
		if entry.BlockID == id {
			return entry
		}
	}
	return depgraph.PlanEntry{BlockID: id}
}

func findHead(heads []checkpoint.CommitEntry, id string) checkpoint.CommitEntry {
	for _, head := range heads {
		if head.BlockID == id {
			return head
		}
	}
	return checkpoint.CommitEntry{}
}

func loadJSON[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func cloneBlockResult(in *model.BlockResult) *model.BlockResult {
	data, _ := json.Marshal(in)
	var out model.BlockResult
	_ = json.Unmarshal(data, &out)
	return &out
}
