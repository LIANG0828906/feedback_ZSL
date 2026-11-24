package verify

import (
	"context"
	"fmt"
	"path/filepath"

	"spatialworkbench/internal/acoustics"
	"spatialworkbench/internal/checkpoint"
	"spatialworkbench/internal/model"
	"spatialworkbench/internal/session"
	"spatialworkbench/internal/werr"
)

type Report struct {
	Equal     bool               `json:"equal"`
	ResumeAt  int                `json:"resumeAt"`
	Realtime  session.RunOutput  `json:"realtime"`
	Resume    session.RunOutput  `json:"resume"`
	Recompute session.RunOutput  `json:"recompute"`
	Compared  []ComparisonResult `json:"comparisons"`
}

type ComparisonResult struct {
	Left  string `json:"left"`
	Right string `json:"right"`
	Equal bool   `json:"equal"`
	Error string `json:"error,omitempty"`
}

func RunThreePaths(ctx context.Context, sample model.Sample, params model.Params, dir string, resumeAt int) (Report, *werr.Error) {
	total := params.TotalFrames(len(sample.Dry.Samples))
	if resumeAt <= 0 {
		resumeAt = total / 2
	}
	if resumeAt >= total {
		resumeAt = total - 1
	}
	if resumeAt < 0 {
		resumeAt = 0
	}
	realtimeDB := filepath.Join(dir, "realtime.sqlite")
	realtimeStore, err := checkpoint.Open(realtimeDB)
	if err != nil {
		return Report{}, err
	}
	realtimeSession, err := session.Open(sample, params, realtimeStore, session.Injection{})
	if err != nil {
		realtimeStore.Close()
		return Report{}, err
	}
	if _, err := realtimeSession.ProcessAndCommitAll(ctx); err != nil {
		realtimeSession.Close()
		return Report{}, err
	}
	realtimeOutput, err := realtimeSession.Output()
	if err != nil {
		realtimeSession.Close()
		return Report{}, err
	}
	realtimeGeneration := realtimeSession.Generation()
	realtimeSession.Close()

	resumeDB := filepath.Join(dir, "resume.sqlite")
	resumeStore, err := checkpoint.Open(resumeDB)
	if err != nil {
		return Report{}, err
	}
	resumeSession, err := session.Open(sample, params, resumeStore, session.Injection{})
	if err != nil {
		resumeStore.Close()
		return Report{}, err
	}
	for i := 0; i < resumeAt; i++ {
		if _, err := resumeSession.ProcessAndCommitNext(ctx); err != nil {
			resumeSession.Close()
			return Report{}, err
		}
	}
	resumeSession.Close()
	resumeStore, err = checkpoint.Open(resumeDB)
	if err != nil {
		return Report{}, err
	}
	resumeSession, err = session.Open(sample, params, resumeStore, session.Injection{})
	if err != nil {
		resumeStore.Close()
		return Report{}, err
	}
	loaded := resumeSession.ResumeInfo()
	if loaded.Invalidated || loaded.Loaded != resumeAt {
		resumeSession.Close()
		return Report{}, werr.New(werr.CodeState, "resume loaded %d frames (invalidated=%v, reason=%s), expected %d", loaded.Loaded, loaded.Invalidated, loaded.Reason, resumeAt)
	}
	if _, err := resumeSession.ProcessAndCommitAll(ctx); err != nil {
		resumeSession.Close()
		return Report{}, err
	}
	resumeOutput, err := resumeSession.Output()
	if err != nil {
		resumeSession.Close()
		return Report{}, err
	}
	resumeSession.Close()

	recomputeDB := filepath.Join(dir, "recompute.sqlite")
	recomputeStore, err := checkpoint.Open(recomputeDB)
	if err != nil {
		return Report{}, err
	}
	persistedStore, err := checkpoint.Open(realtimeDB)
	if err != nil {
		recomputeStore.Close()
		return Report{}, err
	}
	persisted, err := persistedStore.Load(ctx, realtimeGeneration)
	if err != nil {
		persistedStore.Close()
		recomputeStore.Close()
		return Report{}, err
	}
	if persisted.Invalidated || len(persisted.Records) != total {
		persistedStore.Close()
		recomputeStore.Close()
		return Report{}, werr.New(werr.CodeState, "persisted recompute could not load %d frames", total)
	}
	persistedParams := persisted.Records[0].Params
	persistedStore.Close()
	recomputeSession, err := session.Open(sample, persistedParams, recomputeStore, session.Injection{})
	if err != nil {
		recomputeStore.Close()
		return Report{}, err
	}
	for {
		progress := recomputeSession.Progress()
		if progress.Committed == progress.Total {
			break
		}
		committed, err := recomputeSession.ProcessAndCommitBatch(ctx, params.MaxWorkers*2)
		if err != nil {
			recomputeSession.Close()
			return Report{}, err
		}
		if committed == 0 && recomputeSession.Progress().Committed != progress.Total {
			recomputeSession.Close()
			return Report{}, werr.New(werr.CodeState, "persisted recompute batch made no progress")
		}
	}
	recomputeOutput, err := recomputeSession.Output()
	if err != nil {
		recomputeSession.Close()
		return Report{}, err
	}
	recomputeSession.Close()

	report := Report{
		Equal:     true,
		ResumeAt:  resumeAt,
		Realtime:  realtimeOutput,
		Resume:    resumeOutput,
		Recompute: recomputeOutput,
	}
	pairs := []struct {
		leftName  string
		rightName string
		left      session.RunOutput
		right     session.RunOutput
	}{
		{string(PathRealtime), string(PathResume), realtimeOutput, resumeOutput},
		{string(PathRealtime), string(PathRecompute), realtimeOutput, recomputeOutput},
		{string(PathResume), string(PathRecompute), resumeOutput, recomputeOutput},
	}
	for _, pair := range pairs {
		comparison := ComparisonResult{Left: pair.leftName, Right: pair.rightName, Equal: true}
		if diff := Compare(pair.leftName, pair.rightName, pair.left, pair.right); diff != nil {
			comparison.Equal = false
			comparison.Error = diff.Error()
			report.Equal = false
			report.Compared = append(report.Compared, comparison)
			return report, diff
		}
		report.Compared = append(report.Compared, comparison)
	}
	return report, nil
}

func RenderAll(sample model.Sample, params model.Params) (session.RunOutput, *werr.Error) {
	s, err := session.Open(sample, params, nil, session.Injection{})
	if err != nil {
		return session.RunOutput{}, err
	}
	defer s.Close()
	if _, err := s.ProcessAndCommitAll(context.Background()); err != nil {
		return session.RunOutput{}, err
	}
	return s.Output()
}

func FirstRenderMismatch(frame int, left, right [][]float64) *werr.Error {
	if len(left) != len(right) {
		return diff(frame, "", "render.channels", len(left), len(right))
	}
	for ch := range left {
		if len(left[ch]) != len(right[ch]) {
			return diff(frame, fmt.Sprintf("ch%d", ch), "render.samples", len(left[ch]), len(right[ch]))
		}
		for n := range left[ch] {
			if !floatEqual(left[ch][n], right[ch][n]) {
				return diff(frame, fmt.Sprintf("ch%d", ch), fmt.Sprintf("render.sample[%d]", n), left[ch][n], right[ch][n])
			}
		}
	}
	return nil
}

func CompareFrameResults(frame int, left, right acoustics.FrameResult) *werr.Error {
	return compareFrame(frame, left, right)
}
