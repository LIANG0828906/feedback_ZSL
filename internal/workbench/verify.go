package workbench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"time"

	"sre-timeline/internal/bizerr"
	"sre-timeline/internal/checkpoint"
	"sre-timeline/internal/concurrency"
	"sre-timeline/internal/engine"
	"sre-timeline/internal/model"
	"sre-timeline/internal/sample"
	"sre-timeline/internal/schemaio"
	"sre-timeline/internal/scoring"
	"sre-timeline/internal/topology"
)

type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Code   string `json:"code,omitempty"`
	Detail any    `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
}

type VerificationReport struct {
	Seed          int64                `json:"seed"`
	GoVersion     string               `json:"go_version"`
	Passed        bool                 `json:"passed"`
	Checks        []Check              `json:"checks"`
	Files         *sample.WrittenFiles `json:"files,omitempty"`
	ReportPath    string               `json:"report_path,omitempty"`
	TopCandidate  string               `json:"top_candidate,omitempty"`
	IntervalCount int                  `json:"interval_count,omitempty"`
}

type VerifyOptions struct {
	Seed    int64
	Dir     string
	Workers int
	Buffer  int
}

func RunVerification(ctx context.Context, opts VerifyOptions) (*VerificationReport, error) {
	if opts.Seed == 0 {
		opts.Seed = sample.DefaultSeed
	}
	if opts.Dir == "" {
		opts.Dir = filepath.Join("artifacts", "verify")
	}
	cfg := concurrency.Config{Workers: opts.Workers, BufferLimit: opts.Buffer}.Normalize()
	report := &VerificationReport{Seed: opts.Seed, GoVersion: runtime.Version(), Passed: true}

	p, files, err := sample.WriteWorkspace(filepath.Join(opts.Dir, "samples"), opts.Seed)
	if err != nil {
		return nil, err
	}
	report.Files = files

	add := func(name string, fn func() (any, error)) {
		detail, err := fn()
		check := Check{Name: name, Detail: detail}
		if err != nil {
			check.Pass = false
			check.Code = bizerr.CodeOf(err)
			check.Error = err.Error()
			report.Passed = false
		} else {
			check.Pass = true
		}
		report.Checks = append(report.Checks, check)
	}

	var variants map[string]*model.Project
	add("generate_and_load_three_schemas", func() (any, error) {
		variants = map[string]*model.Project{}
		for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
			data, encodeErr := schemaio.Encode(p, version)
			if encodeErr != nil {
				return nil, encodeErr
			}
			loaded, loadErr := schemaio.Load(data)
			if loadErr != nil {
				return nil, loadErr
			}
			variants[version] = loaded
		}
		return map[string]int{"schemas": len(variants), "events": len(p.Events), "services": len(p.Services)}, nil
	})

	add("cross_version_normalized_equivalence", func() (any, error) {
		for version, loaded := range variants {
			if !reflect.DeepEqual(loaded.ServiceIDs(), p.ServiceIDs()) {
				return nil, bizerr.New("E_VERIFY_SERVICE_MISMATCH", version, "service set mismatch")
			}
			if !reflect.DeepEqual(loaded.EdgesSorted(), p.EdgesSorted()) {
				return nil, bizerr.New("E_VERIFY_EDGE_MISMATCH", version, "edge set mismatch")
			}
			wantEvents := p.SortedEvents()
			gotEvents := loaded.SortedEvents()
			if len(gotEvents) != len(wantEvents) {
				return nil, bizerr.New("E_VERIFY_EVENT_MISMATCH", version, "event count mismatch")
			}
			for i := range wantEvents {
				if wantEvents[i].ID != gotEvents[i].ID || wantEvents[i].OccurredMS != gotEvents[i].OccurredMS || wantEvents[i].ServiceID != gotEvents[i].ServiceID {
					return nil, bizerr.New("E_VERIFY_TIME_MISMATCH", version+"#"+wantEvents[i].ID, "normalized event mismatch")
				}
			}
		}
		return "service/edge/event-time sets equal", nil
	})

	var analysis *engine.Analysis
	eng, err := engine.New(p, engine.Options{Concurrency: cfg})
	if err != nil {
		return nil, err
	}
	add("parallel_interval_and_score_analysis", func() (any, error) {
		var analyzeErr error
		analysis, analyzeErr = eng.Analyze(ctx)
		if analyzeErr != nil {
			return nil, analyzeErr
		}
		report.TopCandidate = analysis.Scoring.Candidates[0].EventID
		report.IntervalCount = len(analysis.Targets)
		return map[string]any{
			"intervals":       len(analysis.Targets),
			"candidates":      len(analysis.Scoring.Candidates),
			"worker_budget":   analysis.Diagnostics.WorkerBudget,
			"buffer_limit":    analysis.Diagnostics.BufferLimit,
			"service_timings": analysis.Diagnostics.ServiceDurationsNS,
		}, nil
	})

	add("truth_alignment_interval_candidate", func() (any, error) {
		if report.TopCandidate != "chg-pay-deploy" {
			return nil, bizerr.New("E_VERIFY_ROOT_CAUSE", "candidates[0]", "top candidate is %s", report.TopCandidate)
		}
		if len(analysis.Targets) != len(p.Truth.Intervals) {
			return nil, bizerr.New("E_VERIFY_INTERVAL_COUNT", "targets", "got %d want %d", len(analysis.Targets), len(p.Truth.Intervals))
		}
		for i, candidate := range analysis.Scoring.Candidates {
			want := p.Truth.Candidates[i]
			if candidate.EventID != want.EventID || candidate.Rank != want.Rank {
				return nil, bizerr.New("E_VERIFY_RANK", candidate.EventID, "rank/order mismatch")
			}
			diff := candidate.TotalRaw - want.ScoreRaw
			if diff < 0 {
				diff = -diff
			}
			if diff > p.Policy.ScoreTolerance {
				return nil, bizerr.New("E_VERIFY_SCORE", candidate.EventID, "score diff %d > tolerance %d", diff, p.Policy.ScoreTolerance)
			}
		}
		return map[string]any{"top_candidate": report.TopCandidate, "tolerance_raw": p.Policy.ScoreTolerance}, nil
	})

	add("incremental_threshold_matches_full", func() (any, error) {
		if _, err := eng.EditThreshold("payment", 900); err != nil {
			return nil, err
		}
		inc, err := eng.Analyze(ctx)
		if err != nil {
			return nil, err
		}
		fresh, err := engine.New(eng.Project(), engine.Options{Concurrency: cfg})
		if err != nil {
			return nil, err
		}
		full, err := fresh.Analyze(ctx)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(summarizeCandidates(inc), summarizeCandidates(full)) {
			return nil, bizerr.New("E_VERIFY_INCREMENTAL", "candidates", "incremental differs from full recompute")
		}
		return inc.Diagnostics, nil
	})

	add("checkpoint_commit_and_version_recovery", func() (any, error) {
		dbPath := filepath.Join(opts.Dir, "checkpoint.db")
		_ = os.Remove(dbPath)
		store, err := checkpoint.Open(ctx, dbPath)
		if err != nil {
			return nil, err
		}
		round := checkpoint.Round{
			RoundNo: 1, ProjectID: p.ID, ProjectVersion: eng.Hash(), SchemaVariant: model.SchemaV1,
			CommittedAtMS: time.Now().UnixMilli(),
		}
		for serviceID, result := range analysis.Intervals {
			payload, _ := json.Marshal(result)
			round.ServiceStates = append(round.ServiceStates, checkpoint.ServiceState{ServiceID: serviceID, StateJSON: string(payload)})
		}
		contribsByKey := map[string][]scoring.Contribution{}
		for _, contribution := range analysis.Scoring.Contributions {
			contribsByKey[contribution.IntervalKey] = append(contribsByKey[contribution.IntervalKey], contribution)
		}
		for _, target := range analysis.Targets {
			payload, _ := json.Marshal(contribsByKey[target.Data.Key])
			round.ContributionCaches = append(round.ContributionCaches, checkpoint.ContributionCache{IntervalKey: target.Data.Key, IntervalID: target.ID, PayloadJSON: string(payload)})
		}
		if err := store.SaveRound(ctx, round); err != nil {
			_ = store.Close()
			return nil, err
		}
		loaded, err := store.LoadLatest(ctx, eng.Hash())
		if err != nil {
			_ = store.Close()
			return nil, err
		}
		if _, mismatchErr := store.LoadLatest(ctx, "stale-version"); !bizerr.IsCode(mismatchErr, bizerr.CodeCheckpointVersion) {
			_ = store.Close()
			return nil, bizerr.New("E_VERIFY_CHECKPOINT_VERSION", "load", "mismatch error = %v", mismatchErr)
		}
		if err := store.Close(); err != nil {
			return nil, err
		}
		return map[string]any{"round": loaded.RoundNo, "services": len(loaded.ServiceStates), "contributions": len(loaded.ContributionCaches), "db": dbPath}, nil
	})

	add("stable_injection_codes", func() (any, error) {
		codes := map[string]string{}
		bad, _ := engine.CloneProject(p)
		bad.Services[0].ErrorThresholdBP = -1
		if err := schemaio.ValidateCanonical(bad); err != nil {
			codes["negative_threshold"] = bizerr.CodeOf(err)
		} else {
			return nil, bizerr.New("E_VERIFY_INJECTION", "negative_threshold", "expected error")
		}
		cycle, _ := engine.CloneProject(p)
		cycle.Edges = append(cycle.Edges, model.Edge{Caller: "inventory", Callee: "order"})
		topo, err := topology.New(cycle.Services, cycle.Edges)
		if err == nil {
			err = topo.ValidateAcyclic()
		}
		if bizerr.CodeOf(err) != bizerr.CodeTopologyCycle {
			return nil, err
		}
		codes["cycle"] = bizerr.CodeTopologyCycle
		return codes, nil
	})

	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return report, err
	}
	reportData, _ := json.MarshalIndent(report, "", "  ")
	reportPath := filepath.Join(opts.Dir, "verification-report.json")
	if err := os.WriteFile(reportPath, reportData, 0o644); err != nil {
		return report, err
	}
	report.ReportPath = reportPath
	return report, nil
}

type compactCandidate struct {
	Rank  int    `json:"rank"`
	Event string `json:"event"`
	Raw   int64  `json:"raw"`
}

func summarizeCandidates(a *engine.Analysis) []compactCandidate {
	out := make([]compactCandidate, 0, len(a.Scoring.Candidates))
	for _, c := range a.Scoring.Candidates {
		out = append(out, compactCandidate{Rank: c.Rank, Event: c.EventID, Raw: c.TotalRaw})
	}
	return out
}
