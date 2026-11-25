package runtime

import (
	"context"
	"encoding/json"
	"sort"

	"presentkernel/internal/checkpoint"
	"presentkernel/internal/domain"
	"presentkernel/internal/engine"
	"presentkernel/internal/scheduler"
)

type RunConfig struct {
	Scheduler     scheduler.Config
	UseCache      bool
	MemoryCache   *engine.MemoryCache
	Store         *checkpoint.Store
	RunID         string
	Faults        map[string]string
	CommitToStore bool
}

type Snapshot struct {
	Path             string                    `json:"path"`
	FPS              int                       `json:"fps"`
	Results          []*domain.PartitionResult `json:"results"`
	Sequence         []string                  `json:"sequence"`
	CacheHits        []string                  `json:"cache_hits"`
	Rebuilt          []string                  `json:"rebuilt"`
	Errors           map[string]*domain.Error  `json:"errors,omitempty"`
	SchedulerReport  *scheduler.Report         `json:"scheduler_report,omitempty"`
	CheckpointStatus *checkpoint.Status        `json:"checkpoint_status,omitempty"`
}

func CloneProject(p *domain.Project) *domain.Project {
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	var out domain.Project
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return &out
}

func Refs(p *domain.Project) []domain.PartitionRef {
	refs := []domain.PartitionRef{}
	for di := range p.Dialogues {
		d := p.Dialogues[di]
		for si := range d.Segments {
			s := d.Segments[si]
			refs = append(refs, domain.PartitionRef{DialogueID: d.ID, PartitionID: domain.MakePartitionID(d.ID, s.Index), PartitionIndex: s.Index})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].PartitionID < refs[j].PartitionID })
	return refs
}

func ExpectedCacheKeys(p *domain.Project, fps int) map[string]string {
	out := map[string]string{}
	for di := range p.Dialogues {
		d := p.Dialogues[di]
		for si := range d.Segments {
			pid := domain.MakePartitionID(d.ID, d.Segments[si].Index)
			_, _, key := engine.PartitionCacheKey(p, di, si, fps)
			out[pid] = key
		}
	}
	return out
}

func RunParallel(ctx context.Context, p *domain.Project, pathName string, rc RunConfig) (*Snapshot, error) {
	fps := p.TargetFPS
	refs := Refs(p)
	snap := &Snapshot{Path: pathName, FPS: fps, Errors: map[string]*domain.Error{}}
	results := map[string]*domain.PartitionResult{}
	jobs := make([]scheduler.Job, 0, len(refs))

	for idx, ref := range refs {
		di := engine.FindDialogueIndex(p, ref.DialogueID)
		si := ref.PartitionIndex
		_, _, key := engine.PartitionCacheKey(p, di, si, fps)
		if rc.UseCache && rc.MemoryCache != nil {
			if cached := rc.MemoryCache.Get(key); cached != nil {
				results[ref.PartitionID] = cached
				snap.CacheHits = append(snap.CacheHits, ref.PartitionID)
				continue
			}
		}
		if rc.UseCache && rc.Store != nil {
			if cached, ok, err := rc.Store.GetCached(key); err != nil {
				return nil, err
			} else if ok {
				results[ref.PartitionID] = cached
				snap.CacheHits = append(snap.CacheHits, ref.PartitionID)
				if rc.MemoryCache != nil {
					rc.MemoryCache.Put(cached)
				}
				continue
			}
		}
		refCopy := ref
		diCopy, siCopy := di, si
		fault := ""
		if rc.Faults != nil {
			fault = rc.Faults[ref.PartitionID]
		}
		jobs = append(jobs, scheduler.Job{
			Index: idx, Ref: refCopy,
			Run: func(ctx context.Context) (*domain.PartitionResult, error) {
				_ = ctx
				return engine.BuildPartition(p, diCopy, siCopy, engine.Options{FPS: fps, Fault: fault})
			},
		})
		snap.Rebuilt = append(snap.Rebuilt, ref.PartitionID)
	}

	if len(jobs) > 0 {
		report := scheduler.RunJobs(ctx, jobs, rc.Scheduler)
		snap.SchedulerReport = report
		for _, out := range report.Outcomes {
			if out.Error != nil {
				snap.Errors[out.Ref.PartitionID] = out.Error
				continue
			}
			results[out.Ref.PartitionID] = out.Result
			if rc.MemoryCache != nil {
				rc.MemoryCache.Put(out.Result)
			}
		}
	}

	seq := 1
	for _, ref := range refs {
		r, ok := results[ref.PartitionID]
		if !ok {
			continue
		}
		r.CommitSequence = seq
		snap.Results = append(snap.Results, r)
		snap.Sequence = append(snap.Sequence, ref.PartitionID)
		if rc.Store != nil && rc.CommitToStore {
			runID := rc.RunID
			if runID == "" {
				runID = "default"
			}
			if err := rc.Store.CommitPartition(runID, seq, r); err != nil {
				return snap, err
			}
		}
		seq++
	}
	if rc.Store != nil && rc.RunID != "" {
		st, err := rc.Store.Status(rc.RunID)
		if err != nil {
			return snap, err
		}
		snap.CheckpointStatus = st
	}
	if len(snap.Errors) == 0 {
		snap.Errors = nil
	}
	return snap, nil
}

func RunRealtime(p *domain.Project, cache *engine.MemoryCache, cfg scheduler.Config) (*Snapshot, error) {
	cfg.MaxParallel = 2
	if cfg.QueueBudget <= 0 {
		cfg = cfg.Normalize()
		cfg.QueueBudget = 2
		cfg.PerWorkerBuffer = 1
	}
	return RunParallel(context.Background(), p, "realtime", RunConfig{Scheduler: cfg, UseCache: true, MemoryCache: cache})
}

func RunFull(p *domain.Project, cfg scheduler.Config) (*Snapshot, error) {
	return RunParallel(context.Background(), p, "full", RunConfig{Scheduler: cfg, UseCache: false})
}
