package diagnostic

import (
	"context"
	"os"
	"path/filepath"

	"edgerefine/internal/model"
	"edgerefine/internal/runner"
	"edgerefine/internal/sample"
	"edgerefine/internal/store"
	"edgerefine/internal/verify"
)

type Result struct {
	SampleID     string                    `json:"sample_id"`
	Comparison   *model.ThreePathReport    `json:"three_path"`
	Divergence   *model.DivergenceReport   `json:"divergence,omitempty"`
	Verification *model.VerificationReport `json:"verification,omitempty"`
	Batch        *model.RunReport          `json:"-"`
	Recovery     *model.RunReport          `json:"-"`
	Interactive  *model.RunReport          `json:"-"`
	Preview      *model.PreviewReport      `json:"preview,omitempty"`
}

func removeDB(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}

func RunThreePaths(samplesDir, sampleID, workDir string) (*Result, error) {
	b, err := sample.LoadBundle(filepath.Join(samplesDir, sampleID))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	fp := sample.Fingerprint(b.Manifest)
	p := b.Manifest.Params
	ctx := context.Background()

	batch, err := runner.Run(b, runner.Options{Workers: 4, Params: p, Ctx: ctx, StrictAccuracy: true})
	if err != nil {
		return nil, err
	}

	recoveryDB := filepath.Join(workDir, "recovery-"+sampleID+".db")
	removeDB(recoveryDB)
	st, err := store.Open(recoveryDB)
	if err != nil {
		return nil, err
	}
	tiles := (b.Manifest.Width / p.TileSize) * (b.Manifest.Height / p.TileSize)
	stopAfter := tiles + 2
	partial, err := runner.Run(b, runner.Options{
		Workers: 4, Params: p, Store: st, TruthFingerprint: fp, Persist: true,
		Ctx: ctx, StopAfterCommits: stopAfter,
	})
	if err != nil && err != runner.ErrStopAfterCommit {
		_ = st.Close()
		return nil, err
	}
	_ = partial
	if err := st.Close(); err != nil {
		return nil, err
	}

	st2, err := store.Open(recoveryDB)
	if err != nil {
		return nil, err
	}
	recovery, err := runner.Run(b, runner.Options{
		Workers: 3, Params: p, Store: st2, TruthFingerprint: fp, Persist: true,
		Resume: true, Ctx: ctx, StrictAccuracy: true,
	})
	if err != nil {
		_ = st2.Close()
		return nil, err
	}
	verification, err := verify.Verify(st2, b.Manifest, fp)
	if err != nil {
		_ = st2.Close()
		return nil, err
	}
	if err := st2.Close(); err != nil {
		return nil, err
	}

	preview, err := runner.RunPreview(b, p, nil, 4, nil, ctx)
	if err != nil {
		return nil, err
	}
	interactiveDB := filepath.Join(workDir, "interactive-"+sampleID+".db")
	removeDB(interactiveDB)
	ist, err := store.Open(interactiveDB)
	if err != nil {
		return nil, err
	}
	interactive, err := runner.Run(b, runner.Options{
		Workers: 2, Params: p, Store: ist, TruthFingerprint: fp, Persist: true,
		Snapshots: preview.Snapshots, Ctx: ctx, StrictAccuracy: true,
	})
	_ = ist.Close()
	if err != nil {
		return nil, err
	}
	divergence, err := runner.ComparePreviewExact(preview.Report, interactive, p)
	if err != nil {
		return nil, err
	}

	comparison := runner.CompareThreePaths(
		[]string{"one-shot-full", "checkpoint-recovery", "interactive-exact"},
		batch, recovery, interactive,
	)
	return &Result{
		SampleID: sampleID, Comparison: comparison, Divergence: divergence,
		Verification: verification, Batch: batch, Recovery: recovery,
		Interactive: interactive, Preview: preview.Report.Preview,
	}, nil
}
