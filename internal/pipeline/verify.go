package pipeline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"msworkbench/internal/checkpoint"
	"msworkbench/internal/domain"
	"msworkbench/internal/e"
	"msworkbench/internal/graph"
	"msworkbench/internal/projectio"
	"msworkbench/internal/quant"
	"msworkbench/internal/recon"
	"msworkbench/internal/sample"
)

type Verification struct {
	Project            *domain.Project    `json:"project"`
	BaseProject        *domain.Project    `json:"base_project"`
	Sample             *sample.SampleSet  `json:"sample"`
	Thresholds         quant.Thresholds   `json:"thresholds"`
	Sparse             *Report            `json:"sparse"`
	Medium             *Report            `json:"medium"`
	Dense              *Report            `json:"dense"`
	IncrementalInitial *Report            `json:"incremental_initial,omitempty"`
	IncrementalGain    *Report            `json:"incremental_gain,omitempty"`
	IncrementalProfile *Report            `json:"incremental_profile,omitempty"`
	IncrementalFinal   *Report            `json:"incremental_final"`
	Recovery           *Report            `json:"recovery"`
	Full               *Report            `json:"full"`
	GainManifest       *graph.Manifest    `json:"gain_manifest"`
	ProfileManifest    *graph.Manifest    `json:"profile_manifest"`
	VersionManifest    *graph.Manifest    `json:"version_manifest"`
	V1Loss             *domain.LossReport `json:"v1_loss,omitempty"`
	CrashAfterStages   int                `json:"crash_after_stages"`
}

func RunVerification(workDir string) (*Verification, error) {
	p := sample.NewCanonicalProject()
	set, err := sample.Generate(p)
	if err != nil {
		return nil, err
	}
	thresholds := quant.DefaultThresholds()
	budget := struct{ parallel, pixels, queue int }{2, 4096, 2}
	for _, name := range []string{"profile-sparse.db", "profile-medium.db", "profile-dense.db", "incremental.db", "recovery.db", "full.db"} {
		if err := os.Remove(filepath.Join(workDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	runProfile := func(scale domain.SampleScale, dbName, runID string) (*Report, error) {
		store, err := checkpoint.OpenFile(filepath.Join(workDir, dbName))
		if err != nil {
			return nil, err
		}
		return Execute(Options{Project: p, Sample: set, Store: store, RunID: runID, Profile: scale, Thresholds: thresholds, Parallelism: budget.parallel, PixelBufferPerWorker: budget.pixels, QueueCapacity: budget.queue})
	}
	sparse, err := runProfile(domain.ScaleSparse, "profile-sparse.db", "profile-sparse")
	if err != nil {
		return nil, err
	}
	medium, err := runProfile(domain.ScaleMedium, "profile-medium.db", "profile-medium")
	if err != nil {
		return nil, err
	}
	dense, err := runProfile(domain.ScaleDense, "profile-dense.db", "profile-dense")
	if err != nil {
		return nil, err
	}
	if err := ComparePixels(sparse, medium); err != nil {
		return nil, err
	}
	if err := ComparePixels(sparse, dense); err != nil {
		return nil, err
	}
	if err := VerifyTruth(p, set, dense); err != nil {
		return nil, err
	}
	if err := VerifyDriftBounds(p, sparse, dense); err != nil {
		return nil, err
	}

	incStore, err := checkpoint.OpenFile(filepath.Join(workDir, "incremental.db"))
	if err != nil {
		return nil, err
	}
	g := graph.Build(p, set, domain.ScaleSparse, thresholds)
	inc0, err := RunIncremental(Options{Project: p, Sample: set, Graph: g, Store: incStore, RunID: "inc-0", Thresholds: thresholds, Parallelism: budget.parallel, PixelBufferPerWorker: budget.pixels, QueueCapacity: budget.queue})
	if err != nil {
		return nil, err
	}
	gainManifest, err := g.EditGain("B05", 1_050_000)
	if err != nil {
		return nil, err
	}
	incGain, err := RunIncremental(Options{Project: p, Sample: set, Graph: g, Store: incStore, RunID: "inc-gain", Thresholds: thresholds, Parallelism: budget.parallel, PixelBufferPerWorker: budget.pixels, QueueCapacity: budget.queue})
	if err != nil {
		return nil, err
	}
	profileManifest, err := g.EditProfile(domain.ScaleDense, thresholds)
	if err != nil {
		return nil, err
	}
	incProfile, err := RunIncremental(Options{Project: p, Sample: set, Graph: g, Store: incStore, RunID: "inc-profile", Thresholds: thresholds, Parallelism: budget.parallel, PixelBufferPerWorker: budget.pixels, QueueCapacity: budget.queue})
	if err != nil {
		return nil, err
	}
	exported, err := projectio.Export(g.Project, domain.SchemaV1)
	if err != nil {
		return nil, err
	}
	loaded, err := projectio.Load(exported.Data)
	if err != nil {
		return nil, err
	}
	versionManifest := g.SwitchVersion(loaded.Project)
	incFinal, err := RunIncremental(Options{Project: loaded.Project, Sample: set, Graph: g, Store: incStore, RunID: "inc-final", Profile: domain.ScaleDense, Thresholds: thresholds, Parallelism: budget.parallel, PixelBufferPerWorker: budget.pixels, QueueCapacity: budget.queue})
	if err != nil {
		return nil, err
	}

	crashAfter := 5
	recoveryPath := filepath.Join(workDir, "recovery.db")
	crashStore, err := checkpoint.OpenFile(recoveryPath)
	if err != nil {
		return nil, err
	}
	_, crashErr := Execute(Options{Project: loaded.Project, Sample: set, Store: crashStore, RunID: "recovery-run", Profile: domain.ScaleDense, Thresholds: thresholds, Parallelism: budget.parallel, PixelBufferPerWorker: budget.pixels, QueueCapacity: budget.queue, CrashAfterStages: crashAfter})
	var crash *CrashError
	if !errors.As(crashErr, &crash) {
		if crashErr == nil {
			return nil, errors.New("expected simulated crash but run completed")
		}
		return nil, crashErr
	}
	recoveredStore, err := checkpoint.OpenFile(recoveryPath)
	if err != nil {
		return nil, err
	}
	recoveryReport, err := Execute(Options{Project: loaded.Project, Sample: set, Store: recoveredStore, RunID: "recovery-run", Profile: domain.ScaleDense, Thresholds: thresholds, Parallelism: budget.parallel, PixelBufferPerWorker: budget.pixels, QueueCapacity: budget.queue, Resume: true})
	if err != nil {
		return nil, err
	}

	fullStore, err := checkpoint.OpenFile(filepath.Join(workDir, "full.db"))
	if err != nil {
		return nil, err
	}
	fullReport, err := Execute(Options{Project: loaded.Project, Sample: set, Store: fullStore, RunID: "full-run", Profile: domain.ScaleDense, Thresholds: thresholds, Parallelism: budget.parallel, PixelBufferPerWorker: budget.pixels, QueueCapacity: budget.queue})
	if err != nil {
		return nil, err
	}
	if err := CompareReports(fullReport, recoveryReport); err != nil {
		return nil, err
	}
	if err := CompareReports(fullReport, incFinal); err != nil {
		return nil, err
	}

	return &Verification{
		Project: loaded.Project, BaseProject: p, Sample: set, Thresholds: thresholds,
		Sparse: sparse, Medium: medium, Dense: dense,
		IncrementalInitial: inc0, IncrementalGain: incGain, IncrementalProfile: incProfile, IncrementalFinal: incFinal,
		Recovery: recoveryReport, Full: fullReport,
		GainManifest: gainManifest, ProfileManifest: profileManifest, VersionManifest: versionManifest,
		V1Loss: exported.LossReport, CrashAfterStages: crashAfter,
	}, nil
}

func VerifyTruth(p *domain.Project, set *sample.SampleSet, r *Report) error {
	for _, out := range r.Blocks {
		truth := set.Truth[out.Block.String()]
		if truth == nil {
			return &e.Error{Code: e.Diff, Block: out.Block.String(), Message: "missing generator truth"}
		}
		for bandID, want := range truth.BandQuant {
			got := out.Quant.Bands[bandID]
			if got == nil {
				return &e.Error{Code: e.Diff, Block: out.Block.String(), Band: bandID, Message: "missing quant band"}
			}
			for i := range want.Values {
				if want.Values[i] != got.Values[i] {
					return &e.Error{Code: e.Diff, Block: out.Block.String(), Band: bandID, Field: "truth.value", Expected: fmt.Sprint(want.Values[i]), Actual: fmt.Sprint(got.Values[i]), Message: "quant value differs from generator truth"}
				}
				if want.Remainders[i] != got.Remainders[i] {
					return &e.Error{Code: e.Diff, Block: out.Block.String(), Band: bandID, Field: "truth.remainder", Expected: fmt.Sprint(want.Remainders[i]), Actual: fmt.Sprint(got.Remainders[i]), Message: "integer remainder differs from generator truth"}
				}
			}
		}
	}
	return nil
}

func VerifyDriftBounds(p *domain.Project, exact, candidate *Report) error {
	byBlock := map[string]*BlockOutput{}
	for _, b := range exact.Blocks {
		byBlock[b.Block.String()] = b
	}
	for _, cb := range candidate.Blocks {
		eb := byBlock[cb.Block.String()]
		if err := recon.CheckDrift(p, eb.Quant, cb.Quant, eb.Recon, cb.Recon); err != nil {
			return err
		}
	}
	return nil
}
