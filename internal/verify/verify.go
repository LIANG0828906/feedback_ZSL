package verify

import (
	"os"
	"path/filepath"
	"sort"

	"mocapworkbench/internal/model"
	"mocapworkbench/internal/pipeline"
	"mocapworkbench/internal/retarget"
)

type PathComparison struct {
	Name      string               `json:"name"`
	Equal     bool                 `json:"equal"`
	FirstDiff *pipeline.Difference `json:"first_diff,omitempty"`
	Heatmap   []float64            `json:"heatmap"`
}

type Report struct {
	ClipID           string           `json:"clip_id"`
	TargetID         string           `json:"target_id"`
	Mode             string           `json:"mode"`
	Strategy         string           `json:"strategy"`
	Plan             model.Plan       `json:"plan"`
	ParameterVersion string           `json:"parameter_version"`
	RealtimeFrames   int              `json:"realtime_frames"`
	ResumeFirstPass  int              `json:"resume_first_pass_committed"`
	ResumeFrames     int              `json:"resume_frames"`
	FullFrames       int              `json:"full_frames"`
	ResumeAction     string           `json:"resume_action"`
	Comparisons      []PathComparison `json:"comparisons"`
	AllEqual         bool             `json:"all_equal"`
	ScaleAudit       *ScaleAudit      `json:"scale_audit,omitempty"`
	DBPath           string           `json:"db_path"`
	Summary          model.Summary    `json:"summary"`
}

type ScaleAudit struct {
	Performed       bool    `json:"performed"`
	ExactFrames     int     `json:"exact_frames"`
	SampledFrames   int     `json:"sampled_frames"`
	ContactDelta    int     `json:"contact_frame_delta"`
	RootStartDelta  float64 `json:"root_start_delta"`
	RootEndDelta    float64 `json:"root_end_delta"`
	WithinTolerance bool    `json:"within_tolerance"`
}

func Run(clip model.Clip, target model.TargetSkeleton, cfg model.Config, dbPath string) (*Report, error) {
	if dbPath == "" {
		dir, err := os.MkdirTemp("", "mocap-verify-*")
		if err != nil {
			return nil, err
		}
		dbPath = filepath.Join(dir, "resume.db")
	}
	cleanSQLite(dbPath)

	realtime, err := retarget.Run(clip, target, cfg)
	if err != nil {
		return nil, err
	}
	firstPass := cfg
	firstPass.Mode = cfg.Mode
	stopAfter := len(clip.Frames) / 3
	if stopAfter < 1 {
		stopAfter = 1
	}
	partial, err := pipeline.Run(clip, target, firstPass, pipeline.Options{
		StorePath: dbPath, StopAfter: stopAfter, ForceFull: true,
	})
	if err != nil {
		return nil, err
	}
	resumed, err := pipeline.Run(clip, target, cfg, pipeline.Options{StorePath: dbPath})
	if err != nil {
		return nil, err
	}
	full, err := retarget.Run(clip, target, cfg)
	if err != nil {
		return nil, err
	}
	comparisons := []PathComparison{
		compare("realtime_vs_resume", realtime, resumed, target),
		compare("realtime_vs_full", realtime, full, target),
		compare("resume_vs_full", resumed, full, target),
	}
	report := &Report{
		ClipID: clip.ID, TargetID: target.ID, Mode: realtime.Plan.Mode,
		Strategy: realtime.Plan.Strategy, Plan: realtime.Plan,
		ParameterVersion: realtime.ParameterVersion,
		RealtimeFrames:   len(realtime.Frames), ResumeFirstPass: len(partial.Frames),
		ResumeFrames: len(resumed.Frames), FullFrames: len(full.Frames),
		ResumeAction: resumed.SnapshotAction, Comparisons: comparisons,
		AllEqual: true, DBPath: dbPath, Summary: realtime.Summary,
	}
	for _, comparison := range comparisons {
		if !comparison.Equal {
			report.AllEqual = false
		}
	}
	if realtime.Plan.Mode == "sampled" {
		exactCfg := cfg
		exactCfg.Mode = "exact"
		exact, err := retarget.Run(clip, target, exactCfg)
		if err != nil {
			return report, err
		}
		audit := ScaleAudit{
			Performed: true, ExactFrames: len(exact.Frames), SampledFrames: len(realtime.Frames),
			ContactDelta:   contactDelta(exact.ContactFrames, realtime.ContactFrames),
			RootStartDelta: exact.RootStart.Sub(realtime.RootStart).Length(),
			RootEndDelta:   exact.RootEnd.Sub(realtime.RootEnd).Length(),
		}
		audit.WithinTolerance = audit.ContactDelta <= clip.Truth.Tolerances.ScaleContactSlack &&
			audit.RootStartDelta <= clip.Truth.Tolerances.ScaleRoot &&
			audit.RootEndDelta <= clip.Truth.Tolerances.ScaleRoot
		report.ScaleAudit = &audit
		if err := retarget.CheckScaleConsistency(exact, realtime, clip.Truth.Tolerances); err != nil {
			return report, err
		}
	}
	return report, nil
}

func compare(name string, a, b *model.Animation, target model.TargetSkeleton) PathComparison {
	diff := pipeline.FirstDiff(a.Frames, b.Frames, target)
	heatmap := make([]float64, len(a.Frames))
	for i := range heatmap {
		if i >= len(b.Frames) {
			heatmap[i] = 1
			continue
		}
		value := 0.0
		for j := range a.Frames[i].Rotations {
			if j >= len(b.Frames[i].Rotations) {
				value = 1
				break
			}
			d := a.Frames[i].Rotations[j].Sub(b.Frames[i].Rotations[j]).Norm()
			if d > value {
				value = d
			}
		}
		rootDiff := a.Frames[i].RootTranslation.Sub(b.Frames[i].RootTranslation).Length()
		if rootDiff*20 > value {
			value = rootDiff * 20
		}
		if value > 1 {
			value = 1
		}
		heatmap[i] = value
	}
	return PathComparison{Name: name, Equal: diff == nil, FirstDiff: diff, Heatmap: heatmap}
}

func contactDelta(a, b []int) int {
	setA := make(map[int]bool, len(a))
	setB := make(map[int]bool, len(b))
	for _, f := range a {
		setA[f] = true
	}
	for _, f := range b {
		setB[f] = true
	}
	union := make(map[int]bool, len(setA)+len(setB))
	for f := range setA {
		union[f] = true
	}
	for f := range setB {
		union[f] = true
	}
	keys := make([]int, 0, len(union))
	for f := range union {
		keys = append(keys, f)
	}
	sort.Ints(keys)
	delta := 0
	for _, f := range keys {
		if setA[f] != setB[f] {
			delta++
		}
	}
	return delta
}

func cleanSQLite(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}
