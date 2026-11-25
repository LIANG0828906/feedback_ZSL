package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"maskqa/internal/checkpoint"
	"maskqa/internal/model"
	"maskqa/internal/topology"
)

type ThreePathReport struct {
	ProjectID  string               `json:"project_id"`
	KillAfter  int                  `json:"kill_after"`
	Comparison model.PathComparison `json:"comparison"`
	Realtime   *model.RunResult     `json:"realtime,omitempty"`
	Killed     *model.RunResult     `json:"killed,omitempty"`
	Resume     *model.RunResult     `json:"resume,omitempty"`
	Full       *model.RunResult     `json:"full,omitempty"`
	Paths      map[string]string    `json:"paths"`
}

func ExecuteThreePaths(ctx context.Context, p *model.Project, dir string, cfg Config) (*ThreePathReport, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	killAfter := cfg.KillAfterCommits
	if killAfter <= 0 {
		killAfter = minInt(7, maxInt(1, len(p.Strokes)/8))
	}
	paths := map[string]string{
		"realtime": filepath.Join(dir, p.ID+".realtime.db"),
		"resume":   filepath.Join(dir, p.ID+".resume.db"),
		"full":     filepath.Join(dir, p.ID+".full.db"),
	}
	for _, path := range paths {
		removeDBFiles(path)
	}

	realtimeStore, err := checkpoint.Open(paths["realtime"])
	if err != nil {
		return nil, err
	}
	realtimeCfg := cfg
	realtimeCfg.Mode = "realtime"
	realtimeCfg.KillAfterCommits = 0
	realtime, err := Run(ctx, p, realtimeStore, realtimeCfg, nil)
	_ = realtimeStore.Close()
	if err != nil {
		return nil, err
	}

	resumeStore, err := checkpoint.Open(paths["resume"])
	if err != nil {
		return nil, err
	}
	killCfg := cfg
	killCfg.Mode = "realtime-until-forced-boundary"
	killCfg.KillAfterCommits = killAfter
	killed, killErr := Run(ctx, p, resumeStore, killCfg, nil)
	if killErr != nil && killErr != ErrForceTerminated {
		_ = resumeStore.Close()
		return nil, killErr
	}
	if err := resumeStore.Close(); err != nil {
		return nil, err
	}

	resumeStore2, err := checkpoint.Open(paths["resume"])
	if err != nil {
		return nil, err
	}
	resumeCfg := cfg
	resumeCfg.Mode = "checkpoint-resume"
	resumeCfg.KillAfterCommits = 0
	resumed, err := Run(ctx, p, resumeStore2, resumeCfg, nil)
	_ = resumeStore2.Close()
	if err != nil {
		return nil, err
	}

	fullStore, err := checkpoint.Open(paths["full"])
	if err != nil {
		return nil, err
	}
	fullCfg := cfg
	fullCfg.Mode = "empty-cache-full-recompute"
	fullCfg.KillAfterCommits = 0
	full, err := Run(ctx, p, fullStore, fullCfg, nil)
	_ = fullStore.Close()
	if err != nil {
		return nil, err
	}

	cmp := CompareResults("full", full, "resume", resumed)
	if cmp.Equal {
		cmp2 := CompareResults("full", full, "realtime", realtime)
		if !cmp2.Equal {
			cmp = cmp2
		}
	}
	return &ThreePathReport{
		ProjectID: p.ID, KillAfter: killAfter, Comparison: cmp,
		Realtime: realtime, Killed: killed, Resume: resumed, Full: full, Paths: paths,
	}, nil
}

func CompareResults(wantName string, want *model.RunResult, gotName string, got *model.RunResult) model.PathComparison {
	cmp := model.PathComparison{Equal: true, CoverageEqual: true, TopologyEqual: true, CommitSequenceEqual: true}
	if want == nil || got == nil {
		cmp.Equal = false
		cmp.Message = "one run result is nil"
		return cmp
	}
	cmp.RealtimeKey = model.HashCanonical(want.FinalCoverage.Pix)[:16]
	cmp.ResumeKey = model.HashCanonical(got.FinalCoverage.Pix)[:16]
	cmp.FullKey = model.HashCanonical(want.FinalMask.Pix)[:16]
	if want.FinalCoverage.Width != got.FinalCoverage.Width || want.FinalCoverage.Height != got.FinalCoverage.Height {
		cmp.Equal = false
		cmp.CoverageEqual = false
		cmp.Message = fmtCompare("coverage dimensions differ: %s=%dx%d %s=%dx%d", wantName, want.FinalCoverage.Width, want.FinalCoverage.Height, gotName, got.FinalCoverage.Width, got.FinalCoverage.Height)
		return cmp
	}
	for i := 0; i < len(want.FinalCoverage.Pix) && i < len(got.FinalCoverage.Pix); i++ {
		if want.FinalCoverage.Pix[i] != got.FinalCoverage.Pix[i] {
			x := i % want.FinalCoverage.Width
			y := i / want.FinalCoverage.Width
			cmp.Equal = false
			cmp.CoverageEqual = false
			cmp.FirstPixelX = &x
			cmp.FirstPixelY = &y
			cmp.Message = fmtCompare("first coverage mismatch between %s and %s at (%d,%d): %d != %d", wantName, gotName, x, y, want.FinalCoverage.Pix[i], got.FinalCoverage.Pix[i])
			return cmp
		}
	}
	if len(want.FinalCoverage.Pix) != len(got.FinalCoverage.Pix) {
		cmp.Equal = false
		cmp.CoverageEqual = false
		cmp.Message = "coverage buffer length differs"
		return cmp
	}
	if d := topology.FirstDifference(want.Topology, got.Topology); d != -1 {
		domain := d
		cmp.Equal = false
		cmp.TopologyEqual = false
		cmp.FirstDomain = &domain
		cmp.Message = fmtCompare("first topology domain mismatch between %s and %s at domain D%02d", wantName, gotName, d)
		return cmp
	}
	if !reflect.DeepEqual(want.CommitSequence, got.CommitSequence) {
		cmp.Equal = false
		cmp.CommitSequenceEqual = false
		cmp.Message = fmtCompare("commit sequence mismatch: %s len=%d %s len=%d", wantName, len(want.CommitSequence), gotName, len(got.CommitSequence))
		return cmp
	}
	cmp.Message = fmtCompare("%s, %s and checkpoint boundary paths match pixel-by-pixel, topology and commit sequence", wantName, gotName)
	return cmp
}

func removeDBFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func fmtCompare(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}
