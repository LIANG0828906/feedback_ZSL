package fog

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

type ThreePathOptions struct {
	ResumeFraction float64
	ShadowBudget   int
	MaxParallel    int
}

type ThreePathReport struct {
	RealTime        *Frame    `json:"real_time"`
	Resume          *Frame    `json:"resume"`
	Full            *Frame    `json:"full"`
	PixelsEqual     bool      `json:"pixels_equal"`
	SummariesEqual  bool      `json:"summaries_equal"`
	CommitEqual     bool      `json:"commit_sequences_equal"`
	CacheHits       [3]int    `json:"cache_hits"`
	FirstPixelDiff  *PixelRef `json:"first_pixel_diff,omitempty"`
	PixelDiff       float64   `json:"pixel_diff"`
	CommitSequence  []string  `json:"commit_sequence"`
	RealTimeStore   string    `json:"real_time_store"`
	ResumeStore     string    `json:"resume_store"`
	FullStore       string    `json:"full_store"`
	ResumeCommitted int       `json:"resume_phase1_commits"`
	ResumeReloaded  int       `json:"resume_phase2_cache_hits"`
	Errors          []string  `json:"errors,omitempty"`
}

func RunThreePath(scene *Scene, baseDir string, opts ThreePathOptions) (*ThreePathReport, error) {
	if f := scene.Validate(); f != nil {
		return nil, f
	}
	if opts.ResumeFraction <= 0 || opts.ResumeFraction >= 1 {
		opts.ResumeFraction = 0.5
	}
	if baseDir == "" {
		baseDir = os.TempDir()
	}
	root, err := os.MkdirTemp(baseDir, "vfog-3way-")
	if err != nil {
		return nil, err
	}
	rtPath := filepath.Join(root, "realtime.vfogj")
	rsPath := filepath.Join(root, "resume.vfogj")
	fullPath := filepath.Join(root, "full.vfogj")
	rep := &ThreePathReport{RealTimeStore: rtPath, ResumeStore: rsPath, FullStore: fullPath}
	renderOpts := RenderOptions{
		Mode: RenderExact, ShadowBudget: opts.ShadowBudget,
		MaxParallelTiles: opts.MaxParallel,
	}
	rtStore, err := OpenCheckpointStore(rtPath)
	if err != nil {
		return nil, err
	}
	rtEngine := NewEngine(scene, rtStore)
	rt, rtErr := rtEngine.Render(context.Background(), renderOpts)
	if rtErr != nil {
		rep.Errors = append(rep.Errors, "real-time: "+rtErr.Error())
	}
	if rt != nil {
		rep.RealTime = rt
		rep.CacheHits[0] = rt.Stats.CacheHits
	}
	if err := rtStore.Close(); err != nil {
		return nil, err
	}

	totalTiles := 0
	for _, t := range rt.Tiles {
		_ = t
		totalTiles++
	}
	first := int(math.Ceil(float64(totalTiles) * opts.ResumeFraction))
	if first < 1 {
		first = 1
	}
	if first >= totalTiles {
		first = totalTiles - 1
	}
	rsStore, err := OpenCheckpointStore(rsPath)
	if err != nil {
		return nil, err
	}
	rsEngine := NewEngine(scene, rsStore)
	phase1Opts := renderOpts
	phase1Opts.TileLimit = first
	phase1, err := rsEngine.Render(context.Background(), phase1Opts)
	if err != nil {
		rep.Errors = append(rep.Errors, "resume phase1: "+err.Error())
	}
	if phase1 != nil {
		rep.ResumeCommitted = len(phase1.Stats.CommittedTiles)
	}
	if err := rsStore.Close(); err != nil {
		return nil, err
	}
	rsStore2, err := OpenCheckpointStore(rsPath)
	if err != nil {
		return nil, err
	}
	rsEngine2 := NewEngine(scene, rsStore2)
	resume, resumeErr := rsEngine2.Render(context.Background(), renderOpts)
	if resumeErr != nil {
		rep.Errors = append(rep.Errors, "resume phase2: "+resumeErr.Error())
	}
	if resume != nil {
		rep.Resume = resume
		rep.CacheHits[1] = resume.Stats.CacheHits
		rep.ResumeReloaded = resume.Stats.CacheHits
	}
	if err := rsStore2.Close(); err != nil {
		return nil, err
	}

	fullStore, err := OpenCheckpointStore(fullPath)
	if err != nil {
		return nil, err
	}
	fullEngine := NewEngine(scene, fullStore)
	fullOpts := renderOpts
	fullOpts.ForceRecompute = true
	full, fullErr := fullEngine.Render(context.Background(), fullOpts)
	if fullErr != nil {
		rep.Errors = append(rep.Errors, "full: "+fullErr.Error())
	}
	if full != nil {
		rep.Full = full
		rep.CacheHits[2] = full.Stats.CacheHits
	}
	if err := fullStore.Close(); err != nil {
		return nil, err
	}
	if rep.RealTime == nil || rep.Resume == nil || rep.Full == nil {
		return rep, fmt.Errorf("three-path render incomplete: %v", rep.Errors)
	}
	rep.PixelsEqual = true
	rep.SummariesEqual = framesHaveSameSummaries(rep.RealTime, rep.Resume) &&
		framesHaveSameSummaries(rep.RealTime, rep.Full)
	for i := range rep.RealTime.Pixels {
		a, b, c := rep.RealTime.Pixels[i], rep.Resume.Pixels[i], rep.Full.Pixels[i]
		if a.X != b.X || a.Y != b.Y || a.X != c.X || a.Y != c.Y ||
			a.Radiance != b.Radiance || a.Radiance != c.Radiance ||
			a.Transmittance != b.Transmittance || a.Transmittance != c.Transmittance ||
			a.RayDigest != b.RayDigest || a.RayDigest != c.RayDigest ||
			a.CacheKey != b.CacheKey || a.CacheKey != c.CacheKey {
			rep.PixelsEqual = false
			ref := PixelRef{X: a.X, Y: a.Y}
			rep.FirstPixelDiff = &ref
			rep.PixelDiff = math.Max(math.Abs(a.Radiance-b.Radiance), math.Abs(a.Radiance-c.Radiance))
			break
		}
	}
	rep.CommitEqual = stringSlicesEqual(rep.RealTime.CommitSequence, rep.Resume.CommitSequence) &&
		stringSlicesEqual(rep.RealTime.CommitSequence, rep.Full.CommitSequence)
	rep.CommitSequence = append([]string(nil), rep.RealTime.CommitSequence...)
	if !rep.PixelsEqual || !rep.SummariesEqual || !rep.CommitEqual {
		return rep, &Fault{
			Code: EDiverge, Path: "three_path",
			Actual: map[string]any{
				"pixels_equal": rep.PixelsEqual, "summaries_equal": rep.SummariesEqual,
				"commit_equal": rep.CommitEqual, "first_pixel_diff": rep.FirstPixelDiff,
			},
			Expected: "all true", Message: "three render paths diverged",
		}
	}
	return rep, nil
}

func framesHaveSameSummaries(a, b *Frame) bool {
	if len(a.Tiles) != len(b.Tiles) {
		return false
	}
	for i := range a.Tiles {
		if a.Tiles[i].ID != b.Tiles[i].ID || a.Tiles[i].SegmentDigest != b.Tiles[i].SegmentDigest ||
			a.Tiles[i].ContentHash != b.Tiles[i].ContentHash {
			return false
		}
		if len(a.Tiles[i].Rays) != len(b.Tiles[i].Rays) {
			return false
		}
		for j := range a.Tiles[i].Rays {
			if a.Tiles[i].Rays[j].Digest != b.Tiles[i].Rays[j].Digest {
				return false
			}
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
