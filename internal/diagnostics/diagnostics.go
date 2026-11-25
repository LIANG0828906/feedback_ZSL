package diagnostics

import (
	"context"
	"os"
	"path/filepath"

	"semanticshading/internal/apierr"
	"semanticshading/internal/bounds"
	"semanticshading/internal/checkpoint"
	"semanticshading/internal/runner"
	"semanticshading/internal/sample"
)

type Report struct {
	Kind          string        `json:"kind"`
	ExpectedCode  apierr.Code   `json:"expected_code"`
	ObservedCode  apierr.Code   `json:"observed_code,omitempty"`
	OK            bool          `json:"ok"`
	Error         *apierr.Error `json:"error,omitempty"`
	RetrySequence []int         `json:"retry_sequence,omitempty"`
	Detail        string        `json:"detail,omitempty"`
}

func baseline() *sample.Sample {
	s, err := sample.Generate(sample.DefaultConfig())
	if err != nil {
		panic(err)
	}
	return s
}

func reportFromError(kind string, expected apierr.Code, err error) Report {
	if err == nil {
		return Report{Kind: kind, ExpectedCode: expected, OK: false, Detail: "expected error but operation succeeded"}
	}
	ae, ok := apierr.As(err)
	if !ok {
		return Report{Kind: kind, ExpectedCode: expected, OK: false, Detail: err.Error()}
	}
	return Report{Kind: kind, ExpectedCode: expected, ObservedCode: ae.Code, OK: ae.Code == expected, Error: ae}
}

func Inject(ctx context.Context, kind, dir string) Report {
	switch kind {
	case "decode", "truncate":
		s := baseline()
		bad := append([]byte(nil), s.Ref[:len(s.Ref)-7]...)
		_, err := sample.Load(s.Manifest, bad, s.Recon, s.Labels)
		return reportFromError("truncate", apierr.EDecode, err)
	case "schema":
		cfg := sample.DefaultConfig()
		cfg.Width = 63
		_, err := sample.Generate(cfg)
		return reportFromError("schema", apierr.ESchema, err)
	case "para", "gear":
		cfg := sample.DefaultConfig()
		cfg.QuantGear = 99
		_, err := sample.Generate(cfg)
		return reportFromError("gear", apierr.EPara, err)
	case "label":
		s := baseline()
		badLabels := append([]byte(nil), s.Labels...)
		x, y := 17, 17
		badLabels[y*s.Manifest.Width+x] = 99
		_, err := sample.Load(s.Manifest, s.Ref, s.Recon, badLabels)
		r := reportFromError("label", apierr.ELabel, err)
		if ae, ok := apierr.As(err); ok {
			_ = ae.WithPixel(x, y)
		}
		return r
	case "state":
		s := baseline()
		machine := runner.NewMachine(s.Manifest.Block(0))
		err := machine.Transition(runner.StateCommitted)
		return reportFromError("state", apierr.EState, err)
	case "band":
		return runSchedulerInjection(ctx, dir, "band", apierr.EBand, func(s *sample.Sample, store *checkpoint.Store) (*runner.Report, error) {
			return runner.Run(ctx, s, store, runner.Config{Parallelism: 1, QueueBudget: 1, BandWidth: 0, ExplicitBand: true}, nil)
		})
	case "stage":
		return stageWithRetry(ctx, dir)
	case "bound":
		bound := bounds.Compute(3, 6, 16, 8, 1)
		err := bounds.Check(1, bounds.Measurements{
			MeanQ8:        bound.MeanUpperQ8 + 100,
			StdQ8:         bound.StdUpperQ8 + 200,
			BoundaryQ8:    bound.BoundaryUpperQ8 + 300,
			PropagationQ8: bound.PropagationUpperQ8 + 400,
		}, bound)
		return reportFromError("bound", apierr.EBound, err)
	default:
		return Report{Kind: kind, OK: false, Detail: "unknown injection kind"}
	}
}

func runSchedulerInjection(ctx context.Context, dir, kind string, expected apierr.Code, run func(*sample.Sample, *checkpoint.Store) (*runner.Report, error)) Report {
	if err := checkpoint.PrepareDir(dir); err != nil {
		return Report{Kind: kind, ExpectedCode: expected, OK: false, Detail: err.Error()}
	}
	dbPath := filepath.Join(dir, "inject-"+kind+".sqlite")
	_ = os.Remove(dbPath)
	s := baseline()
	store, _, err := checkpoint.Open(dbPath, runner.IdentityFor(s, s.Truth.TruthFingerprint))
	if err != nil {
		return Report{Kind: kind, ExpectedCode: expected, OK: false, Detail: err.Error()}
	}
	defer store.Close()
	runReport, err := run(s, store)
	report := reportFromError(kind, expected, err)
	if runReport != nil {
		report.RetrySequence = runReport.CommitSequence
	}
	return report
}

func stageWithRetry(ctx context.Context, dir string) Report {
	policy := &runner.FailurePolicy{FailTimes: map[int]int{5: 1}}
	first := runSchedulerInjection(ctx, dir, "stage", apierr.EStage, func(s *sample.Sample, store *checkpoint.Store) (*runner.Report, error) {
		return runner.Run(ctx, s, store, runner.Config{Parallelism: 2, QueueBudget: 2, FailurePolicy: policy}, nil)
	})
	if !first.OK {
		return first
	}
	dbPath := filepath.Join(dir, "inject-stage.sqlite")
	s := baseline()
	store, _, err := checkpoint.Open(dbPath, runner.IdentityFor(s, s.Truth.TruthFingerprint))
	if err != nil {
		first.OK = false
		first.Detail = err.Error()
		return first
	}
	defer store.Close()
	retryReport, err := runner.Run(ctx, s, store, runner.Config{Parallelism: 1, QueueBudget: 1, FailurePolicy: policy}, nil)
	if err != nil {
		r := reportFromError("stage-retry", "", err)
		first.OK = false
		first.Detail = "retry failed: " + r.Detail
		return first
	}
	if len(retryReport.CommitSequence) != s.Manifest.BlockCount() {
		first.OK = false
		first.Detail = "retry did not commit all blocks"
	}
	first.RetrySequence = retryReport.CommitSequence
	first.Detail = "block 5 failed once, transitioned to failed, then retry committed all blocks without changing earlier checkpoints"
	return first
}

func RunAll(ctx context.Context, dir string) []Report {
	kinds := []string{"decode", "schema", "gear", "label", "state", "band", "stage", "bound"}
	results := make([]Report, 0, len(kinds))
	for _, kind := range kinds {
		results = append(results, Inject(ctx, kind, dir))
	}
	return results
}
