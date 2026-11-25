package diagnostics

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"replaybench/internal/checkpoint"
	"replaybench/internal/diff"
	"replaybench/internal/eerr"
	"replaybench/internal/engine"
	"replaybench/internal/fixed"
	"replaybench/internal/generator"
	"replaybench/internal/graph"
	"replaybench/internal/loader"
	"replaybench/internal/model"
	"replaybench/internal/runtime"
	"replaybench/internal/scheduler"
)

type Service struct {
	mu     sync.Mutex
	set    *generator.DocumentSet
	loaded map[model.FormatID]*loader.Loaded
	store  *checkpoint.Store
	pool   *scheduler.Pool
	dbPath string
}

func NewService(dbPath string) (*Service, error) {
	set, err := generator.DeterministicSet(generator.DefaultSeed)
	if err != nil {
		return nil, err
	}
	loaded := map[model.FormatID]*loader.Loaded{}
	for f, raw := range set.JSON {
		l, err := loader.LoadJSON(raw)
		if err != nil {
			return nil, err
		}
		loaded[f] = l
	}
	if dbPath == "" {
		dir, err := os.MkdirTemp("", "replaybench-diag-*")
		if err != nil {
			return nil, err
		}
		dbPath = filepath.Join(dir, "diagnostics.db")
	}
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	pool := scheduler.New(scheduler.Config{MaxWorkers: 2, WorkerBuffer: 2, QueueBudget: 6})
	return &Service{set: set, loaded: loaded, store: store, pool: pool, dbPath: dbPath}, nil
}

func (s *Service) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.pool.Shutdown(ctx)
	return s.store.Close()
}

type Overview struct {
	Service             string   `json:"service"`
	Seed                int64    `json:"seed"`
	AlgorithmVersion    string   `json:"algorithm_version"`
	ParamsVersion       string   `json:"params_version"`
	SQLiteSchemaVersion string   `json:"sqlite_schema_version"`
	PointScale          int64    `json:"point_scale"`
	Formats             []string `json:"formats"`
	DBPath              string   `json:"db_path"`
	Endpoints           []string `json:"endpoints"`
}

func (s *Service) Overview() Overview {
	return Overview{
		Service: "replaybench diagnostics", Seed: s.set.Seed,
		AlgorithmVersion: model.AlgorithmVersion, ParamsVersion: model.ParamsVersion,
		SQLiteSchemaVersion: model.SQLiteSchemaVersion, PointScale: fixed.Scale,
		Formats:   []string{"1=weight/cast/round", "2=cooldown/queue/round", "3=weight/cast/timestamp"},
		DBPath:    s.dbPath,
		Endpoints: []string{"/healthz", "/api/overview", "/api/full?format=1", "/api/incremental", "/api/checkpoint", "/api/scheduler"},
	}
}

type RoundSummary struct {
	Round           int                  `json:"round"`
	ConsumedOffset  int                  `json:"consumed_offset"`
	EffectiveSkills []string             `json:"effective_skills"`
	HPWhole         map[string]int64     `json:"hp_whole"`
	ResourceWhole   map[string]int64     `json:"resource_whole"`
	Adjudications   []model.Adjudication `json:"adjudications"`
	StateDigest     string               `json:"state_digest"`
}

func Summarize(snaps []model.RoundSnapshot) []RoundSummary {
	out := make([]RoundSummary, 0, len(snaps))
	for _, snap := range snaps {
		hp := map[string]int64{}
		res := map[string]int64{}
		for _, st := range snap.States {
			hp[st.UnitID] = fixed.Whole(st.HP)
			res[st.UnitID] = fixed.Whole(st.Resource)
		}
		out = append(out, RoundSummary{
			Round: snap.Round, ConsumedOffset: snap.ConsumedOffset,
			EffectiveSkills: snap.EffectiveSkills, HPWhole: hp, ResourceWhole: res,
			Adjudications: snap.Adjudications, StateDigest: snap.StateDigest,
		})
	}
	return out
}

type FullReport struct {
	Format       model.FormatID `json:"format"`
	Verdict      string         `json:"verdict"`
	WinnerTeam   string         `json:"winner_team"`
	TruthMatched bool           `json:"truth_matched"`
	Rounds       []RoundSummary `json:"rounds"`
}

func (s *Service) RunFull(ctx context.Context, format model.FormatID) (*FullReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if format < model.FormatWeightCast || format > model.FormatTimestamp {
		return nil, eerr.Schema("$.query.format", "unknown format number %d", format)
	}
	loaded := s.loaded[format]
	if loaded == nil {
		loaded = s.loaded[model.FormatWeightCast]
	}
	res, err := engine.Run(ctx, loaded.Replay)
	if err != nil {
		return nil, err
	}
	truthExpected := truthSnapshots(loaded.Truth, res.Snapshots)
	match := diff.Snapshots(truthExpected, res.Snapshots) == nil
	return &FullReport{Format: format, Verdict: res.Verdict, WinnerTeam: res.WinnerTeam, TruthMatched: match, Rounds: Summarize(res.Snapshots)}, nil
}

type IncrementalReport struct {
	Plan            graph.Report `json:"plan"`
	CacheHitRounds  []int        `json:"cache_hit_rounds"`
	CacheMissRounds []int        `json:"cache_miss_rounds"`
	FullDiffMatched bool         `json:"full_diff_matched"`
	DiffError       string       `json:"diff_error,omitempty"`
	EditedWeight    model.Edit   `json:"edited_weight"`
}

func (s *Service) RunIncremental(ctx context.Context) (*IncrementalReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	loaded, err := loader.LoadJSON(s.set.JSON[model.FormatWeightCast])
	if err != nil {
		return nil, err
	}
	if err := s.store.ClearNodeCache(ctx); err != nil {
		return nil, err
	}
	oldGraph := graph.Build(loaded.Replay)
	if _, err := graph.RunIncremental(ctx, loaded.Replay, s.store); err != nil {
		return nil, err
	}
	edit := model.Edit{Type: "weight", Round: 2, UnitID: "beta", Value: 10}
	loaded.Replay.Edits = append(loaded.Replay.Edits, edit)
	plan := graph.PlanInvalidation(oldGraph, graph.Build(loaded.Replay))
	inc, err := graph.RunIncremental(ctx, loaded.Replay, s.store)
	if err != nil {
		return nil, err
	}
	full, err := engine.Run(ctx, loaded.Replay)
	if err != nil {
		return nil, err
	}
	diffErr := diff.Snapshots(full.Snapshots, inc.Snapshots)
	report := &IncrementalReport{
		Plan: plan, CacheHitRounds: inc.CacheHitRounds, CacheMissRounds: inc.CacheMissRounds,
		FullDiffMatched: diffErr == nil, EditedWeight: edit,
	}
	if diffErr != nil {
		report.DiffError = diffErr.Error()
	}
	return report, nil
}

type CheckpointReport struct {
	DBPath          string `json:"db_path"`
	FirstCommitSeq  []int  `json:"first_commit_sequence"`
	ResumeCommitSeq []int  `json:"resume_commit_sequence"`
	RecoveredIndex  int    `json:"recovered_index"`
	StartRound      int    `json:"start_round"`
	FullDiffMatched bool   `json:"full_diff_matched"`
	DiffError       string `json:"diff_error,omitempty"`
}

func (s *Service) RunCheckpointDemo(ctx context.Context) (*CheckpointReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := os.MkdirTemp("", "replaybench-checkpoint-*")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "demo.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	loaded, err := loader.LoadJSON(s.set.JSON[model.FormatWeightCast])
	if err != nil {
		return nil, err
	}
	full, err := engine.Run(ctx, loaded.Replay)
	if err != nil {
		return nil, err
	}
	mgr := &runtime.Manager{Store: store, Pool: s.pool, WindowSize: 2}
	first, err := mgr.Run(ctx, loaded.Replay)
	if err != nil {
		return nil, err
	}
	if err := store.Close(); err != nil {
		return nil, err
	}
	store2, err := checkpoint.Open(path)
	if err != nil {
		return nil, err
	}
	defer store2.Close()
	mgr2 := &runtime.Manager{Store: store2, WindowSize: 2}
	resume, err := mgr2.Run(ctx, loaded.Replay)
	if err != nil {
		return nil, err
	}
	diffErr := diff.Snapshots(full.Snapshots, resume.Result.Snapshots)
	recIndex := 0
	if resume.RecoveredWindow != nil {
		recIndex = resume.RecoveredWindow.Index
	}
	return &CheckpointReport{
		DBPath: path, FirstCommitSeq: first.CommitSequence, ResumeCommitSeq: resume.CommitSequence,
		RecoveredIndex: recIndex, StartRound: resume.StartRound, FullDiffMatched: diffErr == nil,
	}, nil
}

func (s *Service) SchedulerStats() scheduler.Stats {
	return s.pool.Stats()
}

func (s *Service) Handler() http.Handler { return NewHandler(s) }

func truthSnapshots(truth *model.Truth, actual []model.RoundSnapshot) []model.RoundSnapshot {
	out := make([]model.RoundSnapshot, len(actual))
	copy(out, actual)
	for i := range out {
		rt := truth.Rounds[i]
		out[i].EffectiveSkills = append([]string(nil), rt.EffectiveSkills...)
		out[i].Adjudications = append([]model.Adjudication(nil), rt.Adjudications...)
		for j := range out[i].States {
			st := &out[i].States[j]
			if hp, ok := rt.HP[st.UnitID]; ok {
				st.HP = fixed.MustWhole(hp)
			}
			if resource, ok := rt.Resource[st.UnitID]; ok {
				st.Resource = fixed.MustWhole(resource)
			}
		}
	}
	return out
}
