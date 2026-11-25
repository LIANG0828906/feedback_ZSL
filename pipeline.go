package audiofp

import (
	"context"
	"errors"
)

var ErrStopAfterWindow = errors.New("audiofp: stop after checkpoint window")

type AnnotationResult struct {
	Path             string             `json:"path"`
	MaterialID       string             `json:"material_id"`
	Identity         CheckpointIdentity `json:"identity"`
	Params           Params             `json:"params"`
	Limits           ResourceLimits     `json:"limits"`
	Frames           []Fingerprint      `json:"frames"`
	Scores           []FrameScore       `json:"scores,omitempty"`
	Intervals        []Interval         `json:"intervals"`
	Quality          *QualityReport     `json:"quality,omitempty"`
	QualityError     *Error             `json:"quality_error,omitempty"`
	WindowStates     []Window           `json:"window_states,omitempty"`
	RecoveredWindows int                `json:"recovered_windows,omitempty"`
	SnapshotReset    bool               `json:"snapshot_reset,omitempty"`
	ResetReason      string             `json:"reset_reason,omitempty"`
}

func identityFor(m *Material, p Params) CheckpointIdentity {
	return CheckpointIdentity{
		MaterialHash:     m.ContentHash(),
		ParamVersion:     p.Version(),
		AlgorithmVersion: AlgorithmVersion,
	}
}

func buildAnnotationResult(path string, m *Material, p Params, limits ResourceLimits, frames []Fingerprint, machine *WindowMachine) (*AnnotationResult, error) {
	templates, err := BuildTemplates(m.Manifest.Categories, m.PCM.SampleRate, p)
	if err != nil {
		return nil, err
	}
	intervals, scores, err := Aggregate(frames, templates, p)
	if err != nil {
		return nil, err
	}
	report, qerr := EvaluateQuality(m.Manifest, intervals, p, m.Manifest.Quality.ToleranceSamples)
	result := &AnnotationResult{
		Path: path, MaterialID: m.PCM.ID, Identity: identityFor(m, p),
		Params: p, Limits: limits, Frames: frames, Scores: scores,
		Intervals: intervals, Quality: report,
	}
	if machine != nil {
		result.WindowStates = machine.Snapshot()
	}
	if qerr != nil {
		if de, ok := AsError(qerr); ok {
			result.QualityError = de
		}
		return result, qerr
	}
	return result, nil
}

// BuildAnnotationResult aggregates caller-owned frames and evaluates quality.
// It lets adapters such as the HTTP session reuse the domain pipeline.
func BuildAnnotationResult(path string, m *Material, p Params, limits ResourceLimits,
	frames []Fingerprint, machine *WindowMachine) (*AnnotationResult, error) {
	return buildAnnotationResult(path, m, p, limits, frames, machine)
}

// AnnotateFull performs one deterministic full recomputation with bounded
// parallel extraction.
func AnnotateFull(ctx context.Context, m *Material, p Params, limits ResourceLimits) (*AnnotationResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p = p.Normalize()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	limits = limits.Normalize()
	frames, err := ExtractFrames(m.PCM.Samples, m.PCM.SampleRate, p, limits)
	if err != nil {
		return nil, err
	}
	return buildAnnotationResult("full", m, p, limits, frames, nil)
}

// AnnotateRealtime advances the window state machine one window at a time.
// The reusable buffer is bounded by ResourceLimits.PerWorkerBufferSamples.
func AnnotateRealtime(ctx context.Context, m *Material, p Params, limits ResourceLimits) (*AnnotationResult, error) {
	return runRealtime(ctx, m, p, limits, nil, nil, "realtime")
}

// CheckpointRealtime runs the ordered state machine and durably commits each
// window. afterWindow may return ErrStopAfterWindow to simulate a restart.
func CheckpointRealtime(ctx context.Context, m *Material, p Params, limits ResourceLimits,
	store *CheckpointStore, afterWindow func(index int) error) (*AnnotationResult, error) {
	return runRealtime(ctx, m, p, limits, store, afterWindow, "realtime-checkpoint")
}

func runRealtime(ctx context.Context, m *Material, p Params, limits ResourceLimits,
	store *CheckpointStore, afterWindow func(index int) error, path string) (*AnnotationResult, error) {
	p = p.Normalize()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	limits = limits.Normalize()
	if err := limits.Validate(p); err != nil {
		return nil, err
	}
	machine := NewFrameWindowMachine(m.PCM.SampleCount, p)
	count := machine.Total()
	buffer := make([]float64, p.FrameLength, limits.PerWorkerBufferSamples)

	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := machine.Start(i); err != nil {
			return nil, err
		}
		start := FrameStart(i, p)
		copy(buffer[:p.FrameLength], m.PCM.Samples[start:start+p.FrameLength])
		fp := ExtractFrameBuffer(buffer[:p.FrameLength], i, start, m.PCM.SampleRate, p)

		if store != nil {
			rec := CheckpointRecord{
				WindowIndex: i, StartSample: start, Fingerprint: fp,
				FingerprintDigest: fp.Digest(), ParamVersion: p.Version(),
				MaterialHash: m.ContentHash(), AlgorithmVersion: AlgorithmVersion,
			}
			if err := store.SaveWindow(ctx, rec); err != nil {
				_ = machine.Fail(i, err)
				return nil, err
			}
		}
		if afterWindow != nil {
			if err := afterWindow(i); err != nil {
				_ = machine.Fail(i, err)
				committed, _ := machine.Progress()
				frames, ferr := machine.OrderedCommittedFingerprints()
				if ferr != nil {
					return nil, ferr
				}
				result, berr := buildAnnotationResult(path+"-stopped", m, p, limits, frames, machine)
				if berr != nil && !HasCode(berr, CodeQual) {
					return nil, berr
				}
				result.RecoveredWindows = committed
				return result, err
			}
		}
		if err := machine.Commit(i, fp, p); err != nil {
			return nil, err
		}
	}
	frames, err := machine.OrderedCommittedFingerprints()
	if err != nil {
		return nil, err
	}
	result, qerr := buildAnnotationResult(path, m, p, limits, frames, machine)
	if qerr != nil {
		return result, qerr
	}
	return result, nil
}

// AnnotateResume opens a versioned SQLite checkpoint. Compatible windows are
// loaded frozen; incompatible checkpoints are atomically reset and fully
// recomputed.
func AnnotateResume(ctx context.Context, m *Material, p Params, limits ResourceLimits, dbPath string) (*AnnotationResult, error) {
	p = p.Normalize()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	limits = limits.Normalize()
	identity := identityFor(m, p)
	store, snapshot, err := OpenAndResumeCheckpoint(ctx, dbPath, identity)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	machine := NewFrameWindowMachine(m.PCM.SampleCount, p)
	for _, rec := range snapshot.Records {
		if err := machine.Start(rec.WindowIndex); err != nil {
			return nil, err
		}
		if err := machine.Commit(rec.WindowIndex, rec.Fingerprint, p); err != nil {
			return nil, err
		}
	}

	count := machine.Total()
	buffer := make([]float64, p.FrameLength, limits.PerWorkerBufferSamples)
	for i := len(snapshot.Records); i < count; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := machine.Start(i); err != nil {
			return nil, err
		}
		start := FrameStart(i, p)
		copy(buffer[:p.FrameLength], m.PCM.Samples[start:start+p.FrameLength])
		fp := ExtractFrameBuffer(buffer[:p.FrameLength], i, start, m.PCM.SampleRate, p)
		rec := CheckpointRecord{
			WindowIndex: i, StartSample: start, Fingerprint: fp, FingerprintDigest: fp.Digest(),
			ParamVersion: p.Version(), MaterialHash: identity.MaterialHash,
			AlgorithmVersion: AlgorithmVersion,
		}
		if err := store.SaveWindow(ctx, rec); err != nil {
			_ = machine.Fail(i, err)
			return nil, err
		}
		if err := machine.Commit(i, fp, p); err != nil {
			return nil, err
		}
	}

	frames, err := machine.OrderedCommittedFingerprints()
	if err != nil {
		return nil, err
	}
	result, qerr := buildAnnotationResult("resume", m, p, limits, frames, machine)
	if result != nil {
		result.RecoveredWindows = len(snapshot.Records)
		result.SnapshotReset = snapshot.Reset
		result.ResetReason = snapshot.ResetReason
	}
	if qerr != nil {
		return result, qerr
	}
	return result, nil
}
