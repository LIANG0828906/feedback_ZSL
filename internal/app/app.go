package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"meshdoctor/internal/errs"
	"meshdoctor/internal/pipeline"
	"meshdoctor/internal/sample"
	"meshdoctor/internal/store"
)

type App struct {
	Root   string
	Params pipeline.Params

	mu      sync.RWMutex
	Report  *Report
	runs    map[string]*RunStatus
	cancels map[string]context.CancelFunc
}

type RunStatus struct {
	SampleID  string    `json:"sample_id"`
	Mode      string    `json:"mode"`
	Running   bool      `json:"running"`
	Stage     string    `json:"stage,omitempty"`
	Message   string    `json:"message,omitempty"`
	ErrorCode string    `json:"error_code,omitempty"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Report struct {
	OK             bool                 `json:"ok"`
	GeneratedAt    string               `json:"generated_at"`
	Root           string               `json:"root"`
	Seed           int64                `json:"seed"`
	Params         pipeline.Params      `json:"params"`
	Parallel       bool                 `json:"parallel"`
	Samples        []SampleVerification `json:"samples"`
	BoundaryChecks []BoundaryCheck      `json:"boundary_checks"`
	CacheStats     map[string]any       `json:"cache_stats"`
}

type SampleVerification struct {
	SampleID       string                   `json:"sample_id"`
	Name           string                   `json:"name"`
	OK             bool                     `json:"ok"`
	ErrorCode      string                   `json:"error_code,omitempty"`
	Error          string                   `json:"error,omitempty"`
	CancelAfter    int                      `json:"cancel_after_stage"`
	Comparison     *pipeline.ThreeWayReport `json:"comparison,omitempty"`
	OneShot        *pipeline.PathResult     `json:"one_shot,omitempty"`
	Segmented      *pipeline.PathResult     `json:"segmented,omitempty"`
	Resume         *pipeline.PathResult     `json:"resume,omitempty"`
	SegmentedCache map[string]any           `json:"segmented_cache,omitempty"`
	ResumeCache    map[string]any           `json:"resume_cache,omitempty"`
}

func New(root string, params pipeline.Params) (*App, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	params = params.WithDefaults()
	a := &App{Root: abs, Params: params, runs: map[string]*RunStatus{}, cancels: map[string]context.CancelFunc{}}
	if err := a.EnsureWorkspace(context.Background()); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *App) EnsureWorkspace(ctx context.Context) error {
	manifest := filepath.Join(a.Root, "workspace_manifest.json")
	if _, err := os.Stat(manifest); errors.Is(err, fs.ErrNotExist) {
		if _, err := sample.WriteWorkspace(a.Root, sample.DefaultSeed); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	_ = ctx
	return nil
}

func (a *App) Samples() ([]sample.Sample, error) { return sample.LoadAll(a.Root) }

func (a *App) Manifest(id string) (*sample.Manifest, *sample.Sample, error) {
	s, err := sample.LoadSample(a.Root, id)
	if err != nil {
		return nil, nil, err
	}
	return &s.Manifest, s, nil
}

func (a *App) Sample(id string) (*sample.Sample, error) {
	return sample.LoadSample(a.Root, id)
}

func (a *App) Verify(ctx context.Context, parallel bool) (*Report, error) {
	samples, err := a.Samples()
	if err != nil {
		return nil, err
	}
	params := a.Params
	params.MaxParallelism = a.Params.MaxParallelism
	report := &Report{GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), Root: a.Root, Seed: sample.DefaultSeed, Params: params, Parallel: parallel}

	verifyOne := func(ctx context.Context, i int, id string) (SampleVerification, error) {
		return VerifySamplePaths(ctx, samples[i], params, filepath.Join(a.Root, "checkpoints", "verify")), nil
	}

	if parallel {
		names := make([]string, len(samples))
		for i, s := range samples {
			names[i] = s.Manifest.ID
		}
		results, _ := pipeline.RunOrdered(ctx, names, params, verifyOne)
		report.Samples = results
	} else {
		for _, s := range samples {
			report.Samples = append(report.Samples, VerifySamplePaths(ctx, s, params, filepath.Join(a.Root, "checkpoints", "verify-serial")))
		}
	}

	report.BoundaryChecks = RunBoundaryChecks(samples, params)
	var hits, misses int64
	for _, s := range report.Samples {
		if v, ok := s.ResumeCache["cache_hits"].(int64); ok {
			hits += v
		}
		if v, ok := s.ResumeCache["cache_misses"].(int64); ok {
			misses += v
		}
	}
	report.CacheStats = map[string]any{"resume_cache_hits": hits, "resume_cache_misses": misses, "samples": len(report.Samples)}
	report.OK = true
	for _, s := range report.Samples {
		if !s.OK {
			report.OK = false
		}
	}
	for _, b := range report.BoundaryChecks {
		if !b.OK {
			report.OK = false
		}
	}
	a.mu.Lock()
	a.Report = report
	a.mu.Unlock()
	return report, nil
}

func VerifySamplePaths(ctx context.Context, s sample.Sample, params pipeline.Params, dbDir string) SampleVerification {
	v := SampleVerification{SampleID: s.Manifest.ID, Name: s.Manifest.Name, CancelAfter: 2, OK: false}
	orch := pipeline.NewOrchestrator()
	var err error

	v.OneShot, err = orch.RunOneShot(s.Manifest.ID, &s.Mesh, &s.Manifest, params)
	if err != nil {
		v.ErrorCode = errs.Code(err)
		v.Error = err.Error()
		return v
	}

	segPath := filepath.Join(dbDir, s.Manifest.ID+"-segmented.db")
	removeDB(segPath)
	segStore, err := store.Open(segPath)
	if err != nil {
		v.ErrorCode = "E_STORE"
		v.Error = err.Error()
		return v
	}
	v.Segmented, err = orch.RunOrResume(ctx, s.Manifest.ID, &s.Mesh, &s.Manifest, params, segStore, pipeline.RunOptions{Path: "segmented"})
	v.SegmentedCache = segStore.CacheStats()
	_ = segStore.Close()
	if err != nil {
		v.ErrorCode = errs.Code(err)
		v.Error = err.Error()
		return v
	}

	resumePath := filepath.Join(dbDir, s.Manifest.ID+"-resume.db")
	removeDB(resumePath)
	resumeStore, err := store.Open(resumePath)
	if err != nil {
		v.ErrorCode = "E_STORE"
		v.Error = err.Error()
		return v
	}
	prefix, err := orch.RunOrResume(ctx, s.Manifest.ID, &s.Mesh, &s.Manifest, params, resumeStore, pipeline.RunOptions{
		Path: "resume-prefix", StopAfter: v.CancelAfter,
	})
	_ = resumeStore.Close()
	if err != nil {
		v.ErrorCode = errs.Code(err)
		v.Error = err.Error()
		return v
	}
	if len(prefix.CommittedSequence) != v.CancelAfter {
		v.ErrorCode = "E_STATE"
		v.Error = "cancel prefix did not stop at committed boundary"
		return v
	}

	reopened, err := store.Open(resumePath)
	if err != nil {
		v.ErrorCode = "E_STORE"
		v.Error = err.Error()
		return v
	}
	v.Resume, err = orch.RunOrResume(ctx, s.Manifest.ID, &s.Mesh, &s.Manifest, params, reopened, pipeline.RunOptions{Path: "resume"})
	v.ResumeCache = reopened.CacheStats()
	_ = reopened.Close()
	if err != nil {
		v.ErrorCode = errs.Code(err)
		v.Error = err.Error()
		return v
	}

	v.Comparison, err = pipeline.CompareThree(v.Segmented, v.Resume, v.OneShot)
	if err != nil {
		v.ErrorCode = errs.Code(err)
		v.Error = err.Error()
		return v
	}
	v.OK = v.Comparison.AllIdentical
	return v
}

func removeDB(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
}

func (a *App) SharedStore() (*store.Store, error) {
	return store.Open(filepath.Join(a.Root, "checkpoints", "meshpipeline.db"))
}
