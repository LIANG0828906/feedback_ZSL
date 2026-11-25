package engine

import (
	"context"
	"os"
	"path/filepath"

	"ms-workbench/internal/pipeline"
	"ms-workbench/internal/scene"
	"ms-workbench/internal/store"
)

type PathResult struct {
	Mode             RunMode `json:"mode"`
	Committed        int     `json:"committedSegments"`
	Total            int     `json:"totalSegments"`
	CommitSequence   []int   `json:"commitSequence"`
	PeakRadiance     float64 `json:"peakRadiance"`
	OutOfGamutCount  int     `json:"outOfGamutCount"`
	SnapshotReset    string  `json:"snapshotReset,omitempty"`
	ResumedFromStore bool    `json:"resumedFromStore"`
}

type VerificationReport struct {
	Equal                bool                        `json:"equal"`
	Live                 PathResult                  `json:"live"`
	Resume               PathResult                  `json:"resume"`
	Full                 PathResult                  `json:"full"`
	LiveVsFull           DifferenceReport            `json:"liveVsFull"`
	ResumeVsFull         DifferenceReport            `json:"resumeVsFull"`
	Heatmap              []float64                   `json:"heatmap"`
	Patches              []pipeline.PatchMeasurement `json:"patches"`
	TruthPeakRadiance    float64                     `json:"truthPeakRadiance"`
	PeakRadianceDelta    float64                     `json:"peakRadianceDelta"`
	CommitSequencesEqual bool                        `json:"commitSequencesEqual"`
}

// Verify runs the realtime, persisted-resume, and one-shot full paths in
// isolated SQLite databases and compares every output value exactly.
func Verify(ctx context.Context, s *scene.Scene, cfg Config) (*VerificationReport, error) {
	dir, err := os.MkdirTemp("", "spectra-verify-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	cfg.ChunkDelayMS = 0

	liveComp, liveResult, err := runVerificationPath(ctx, s, cfg, filepath.Join(dir, "live.db"), ModeLive, false)
	if err != nil {
		return nil, err
	}
	resumeComp, resumeResult, err := runVerificationPath(ctx, s, cfg, filepath.Join(dir, "resume.db"), ModeResume, true)
	if err != nil {
		return nil, err
	}
	fullComp, fullResult, err := runVerificationPath(ctx, s, cfg, filepath.Join(dir, "full.db"), ModeFull, false)
	if err != nil {
		return nil, err
	}
	liveDiff := CompareComposites(liveComp, fullComp, cfg)
	resumeDiff := CompareComposites(resumeComp, fullComp, cfg)
	heatmap := make([]float64, s.Width*s.Height)
	for i := range heatmap {
		if i < len(liveDiff.Heatmap) {
			heatmap[i] = liveDiff.Heatmap[i]
		}
		if i < len(resumeDiff.Heatmap) && resumeDiff.Heatmap[i] > heatmap[i] {
			heatmap[i] = resumeDiff.Heatmap[i]
		}
	}
	patches := pipeline.MeasurePatches(s, fullComp.LinearRGB)
	peakDelta := fullComp.PeakRadiance - s.Truth.PeakRadiance
	if peakDelta < 0 {
		peakDelta = -peakDelta
	}
	report := &VerificationReport{
		Equal:                liveDiff.Equal && resumeDiff.Equal,
		Live:                 liveResult,
		Resume:               resumeResult,
		Full:                 fullResult,
		LiveVsFull:           liveDiff,
		ResumeVsFull:         resumeDiff,
		Heatmap:              heatmap,
		Patches:              patches,
		TruthPeakRadiance:    s.Truth.PeakRadiance,
		PeakRadianceDelta:    peakDelta,
		CommitSequencesEqual: sameInts(liveResult.CommitSequence, fullResult.CommitSequence) && sameInts(resumeResult.CommitSequence, fullResult.CommitSequence),
	}
	return report, nil
}

func runVerificationPath(ctx context.Context, s *scene.Scene, cfg Config, path string, mode RunMode, forceResume bool) (*Composite, PathResult, error) {
	if forceResume {
		cp, err := store.Open(path)
		if err != nil {
			return nil, PathResult{}, err
		}
		first, err := New(s, cp, cfg)
		if err != nil {
			cp.Close()
			return nil, PathResult{}, err
		}
		half := first.View().Total / 2
		if half < 1 {
			half = 1
		}
		if _, err := first.Run(ctx, RunOptions{Mode: ModeLive, MaxNewCommits: half}); err != nil {
			cp.Close()
			return nil, PathResult{}, err
		}
		if err := cp.Close(); err != nil {
			return nil, PathResult{}, err
		}
		cp, err = store.Open(path)
		if err != nil {
			return nil, PathResult{}, err
		}
		e, err := New(s, cp, cfg)
		if err != nil {
			cp.Close()
			return nil, PathResult{}, err
		}
		report, err := e.Run(ctx, RunOptions{Mode: ModeResume})
		if err != nil {
			cp.Close()
			return nil, PathResult{}, err
		}
		comp := e.Composite()
		pathResult := PathResult{
			Mode:             ModeResume,
			Committed:        report.Committed,
			Total:            report.Total,
			CommitSequence:   report.CommitSequence,
			PeakRadiance:     comp.PeakRadiance,
			OutOfGamutCount:  len(comp.OutOfGamut),
			SnapshotReset:    report.SnapshotReset,
			ResumedFromStore: e.LoadedFromCheckpoint(),
		}
		cp.Close()
		return comp, pathResult, nil
	}

	cp, err := store.Open(path)
	if err != nil {
		return nil, PathResult{}, err
	}
	defer cp.Close()
	e, err := New(s, cp, cfg)
	if err != nil {
		return nil, PathResult{}, err
	}
	report, err := e.Run(ctx, RunOptions{Mode: mode})
	if err != nil {
		return nil, PathResult{}, err
	}
	comp := e.Composite()
	return comp, PathResult{
		Mode:            mode,
		Committed:       report.Committed,
		Total:           report.Total,
		CommitSequence:  report.CommitSequence,
		PeakRadiance:    comp.PeakRadiance,
		OutOfGamutCount: len(comp.OutOfGamut),
		SnapshotReset:   report.SnapshotReset,
	}, nil
}
