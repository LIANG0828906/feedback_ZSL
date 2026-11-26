// Package pipeline composes schema-domain state, bounded measurement,
// calibration, persistent cache reuse, and atomic clip commits.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"dialogueaudio/audioerr"
	"dialogueaudio/checkpoint"
	"dialogueaudio/engine"
	"dialogueaudio/graph"
	"dialogueaudio/model"
	"dialogueaudio/resultcache"
	"dialogueaudio/state"
)

const (
	DefaultQuantVersion      = "q16-integer-v1"
	DefaultConversionVersion = "schema-conversion-v1"
	DefaultAlgorithmVersion  = "envelope-cal-v1"
	DefaultToleranceQ16      = int64(256)
)

// Options control a Runner. Directories are optional; omitting a checkpoint
// directory yields an in-memory equivalent with the same commit semantics.
type Options struct {
	CheckpointDir     string              `json:"checkpoint_dir,omitempty"`
	CacheDir          string              `json:"cache_dir,omitempty"`
	QuantVersion      string              `json:"quant_version"`
	ConversionVersion string              `json:"conversion_version"`
	AlgorithmVersion  string              `json:"algorithm_version"`
	ToleranceQ16      int64               `json:"tolerance_q16"`
	Batch             engine.BatchOptions `json:"batch"`
	MaxNewCommits     int                 `json:"max_new_commits,omitempty"`
}

func (o Options) normalized() Options {
	if o.QuantVersion == "" {
		o.QuantVersion = DefaultQuantVersion
	}
	if o.ConversionVersion == "" {
		o.ConversionVersion = DefaultConversionVersion
	}
	if o.AlgorithmVersion == "" {
		o.AlgorithmVersion = DefaultAlgorithmVersion
	}
	if o.ToleranceQ16 == 0 {
		o.ToleranceQ16 = DefaultToleranceQ16
	}
	if o.Batch.MaxParallel <= 0 {
		o.Batch.MaxParallel = 2
	}
	if o.Batch.QueueBudget <= 0 {
		o.Batch.QueueBudget = 2
	}
	return o
}

// RunResult is deterministic except QueueEvents.WaitedMicros.
type RunResult struct {
	Label                      string                            `json:"label,omitempty"`
	Envelopes                  map[string]engine.ClipEnvelope    `json:"envelopes"`
	Calibrations               map[string]engine.ClipCalibration `json:"calibrations"`
	CommitOrder                []string                          `json:"commit_order"`
	CommittedFromCheckpoint    int                               `json:"committed_from_checkpoint"`
	NewCommits                 int                               `json:"new_commits"`
	Progress                   state.Progress                    `json:"progress"`
	States                     []state.Snapshot                  `json:"states"`
	QueueEvents                []engine.QueueEvent               `json:"queue_events,omitempty"`
	RecomputedEnvelopeClips    []string                          `json:"recomputed_envelope_clips"`
	ReusedEnvelopeClips        []string                          `json:"reused_envelope_clips"`
	RecomputedCalibrationClips []string                          `json:"recomputed_calibration_clips"`
	ReusedCalibrationClips     []string                          `json:"reused_calibration_clips"`
	Rebuilt                    bool                              `json:"rebuilt"`
	StoppedEarly               bool                              `json:"stopped_early"`
	Complete                   bool                              `json:"complete"`
}

// Runner coordinates one immutable project revision.
type Runner struct {
	project         model.Project
	opts            Options
	graph           *graph.Graph
	store           *checkpoint.Store
	cache           *resultcache.Cache
	identity        checkpoint.Identity
	envelopeKeys    map[string]string
	calibrationKeys map[string]string
	clipIDs         []string
}

// NewRunner prepares graph keys and durable stores.
func NewRunner(project model.Project, opts Options) (*Runner, error) {
	if err := project.Validate(); err != nil {
		return nil, audioerr.Schema("project", err.Error())
	}
	opts = opts.normalized()
	if opts.ToleranceQ16 < 0 {
		return nil, audioerr.Schema("tolerance_q16", "must not be negative")
	}
	project = project.Clone()
	project.SortClips()
	effectiveAlgorithmVersion := fmt.Sprintf("%s|tol=%d", opts.AlgorithmVersion, opts.ToleranceQ16)
	g, err := graph.Build(project, opts.QuantVersion+"|"+effectiveAlgorithmVersion)
	if err != nil {
		return nil, err
	}
	identity := checkpoint.IdentityFor(project, opts.QuantVersion, opts.ConversionVersion, effectiveAlgorithmVersion)
	r := &Runner{
		project: project, opts: opts, graph: g, identity: identity,
		envelopeKeys:    make(map[string]string, len(project.Clips)),
		calibrationKeys: make(map[string]string, len(project.Clips)),
		clipIDs:         append([]string(nil), identity.ClipIDs...),
	}
	for _, clip := range project.Clips {
		envNode, ok := g.Node("clip:" + clip.ID + ":envelope")
		if !ok {
			return nil, audioerr.Schema("graph", "missing envelope node for "+clip.ID)
		}
		calNode, ok := g.Node("clip:" + clip.ID + ":calibration")
		if !ok {
			return nil, audioerr.Schema("graph", "missing calibration node for "+clip.ID)
		}
		r.envelopeKeys[clip.ID] = envNode.Key
		r.calibrationKeys[clip.ID] = calNode.Key
	}
	if opts.CheckpointDir != "" {
		r.store, err = checkpoint.New(opts.CheckpointDir, identity)
		if err != nil {
			return nil, err
		}
	}
	if opts.CacheDir != "" {
		r.cache, err = resultcache.Open(opts.CacheDir)
		if err != nil {
			return nil, audioerr.New(audioerr.CodeIO, opts.CacheDir, err.Error())
		}
	}
	return r, nil
}

// Graph exposes the immutable dependency graph.
func (r *Runner) Graph() *graph.Graph { return r.graph }

// Identity exposes the checkpoint identity.
func (r *Runner) Identity() checkpoint.Identity { return r.identity }

// Run measures, calibrates, and commits in canonical clip order. Existing
// records are replayed first; cache hits avoid recomputing unaffected nodes.
func (r *Runner) Run(ctx context.Context) (result RunResult, runErr error) {
	result = RunResult{
		Envelopes:                  make(map[string]engine.ClipEnvelope, len(r.clipIDs)),
		Calibrations:               make(map[string]engine.ClipCalibration, len(r.clipIDs)),
		RecomputedEnvelopeClips:    []string{},
		ReusedEnvelopeClips:        []string{},
		RecomputedCalibrationClips: []string{},
		ReusedCalibrationClips:     []string{},
	}
	manager, err := state.New(r.clipIDs)
	if err != nil {
		return result, err
	}
	defer func() {
		result.Progress = manager.Progress()
		result.States = manager.Snapshot()
		result.Complete = result.Progress.Committed == result.Progress.Total
		if result.NewCommits == 0 {
			result.NewCommits = len(result.CommitOrder) - result.CommittedFromCheckpoint
		}
	}()
	var records []checkpoint.Record
	if r.store != nil {
		records, err = r.store.Load()
		if err != nil {
			if checkpoint.IsRebuildRequired(err) {
				if resetErr := r.store.Reset(); resetErr != nil {
					return result, resetErr
				}
				records = nil
				result.Rebuilt = true
			} else {
				return result, err
			}
		}
	}
	if len(records) > len(r.clipIDs) {
		return result, audioerr.New(audioerr.CodeCheckpoint, "checkpoint.records", "more records than clips")
	}
	for i, record := range records {
		if record.ClipID != r.clipIDs[i] {
			return result, audioerr.New(audioerr.CodeCheckpoint, "checkpoint.order",
				fmt.Sprintf("record %d clip %s, expected %s", i+1, record.ClipID, r.clipIDs[i]))
		}
		if err := replayTransitions(manager, record.ClipID); err != nil {
			return result, err
		}
		result.Envelopes[record.ClipID] = record.Envelope
		result.Calibrations[record.ClipID] = record.Calibration
		result.CommitOrder = append(result.CommitOrder, record.ClipID)
	}
	result.CommittedFromCheckpoint = len(records)

	remaining := append([]string(nil), r.clipIDs[len(records):]...)
	missingEnvelopes := make([]model.Clip, 0)
	cachedEnvelopes := make(map[string]engine.ClipEnvelope)
	for _, clipID := range remaining {
		clip, _ := r.project.ClipByID(clipID)
		if r.cache != nil {
			if env, ok := r.cache.GetEnvelope(r.envelopeKeys[clipID]); ok {
				cachedEnvelopes[clipID] = env
				result.ReusedEnvelopeClips = append(result.ReusedEnvelopeClips, clipID)
				continue
			}
		}
		missingEnvelopes = append(missingEnvelopes, clip)
	}
	if err := ctx.Err(); err != nil {
		return result, audioerr.New(audioerr.CodeCancelled, "pipeline.run", err.Error())
	}
	measured := make(map[string]engine.ClipEnvelope, len(missingEnvelopes))
	if len(missingEnvelopes) > 0 {
		params := engine.FrameParams{
			SampleRate: r.project.SampleRate, FrameLen: r.project.FrameLenSamples,
			Hop: r.project.HopSamples, SessionGainQ16: r.project.SessionGainQ16,
		}
		batch, err := engine.MeasureClips(ctx, missingEnvelopes, params, r.opts.Batch)
		if err != nil {
			return result, err
		}
		measured = batch.Envelopes
		result.QueueEvents = batch.QueueEvents
		for _, clip := range missingEnvelopes {
			result.RecomputedEnvelopeClips = append(result.RecomputedEnvelopeClips, clip.ID)
		}
	}
	sort.Strings(result.RecomputedEnvelopeClips)
	sort.Strings(result.ReusedEnvelopeClips)

	newCommits := 0
	for _, clipID := range remaining {
		if err := ctx.Err(); err != nil {
			return result, audioerr.New(audioerr.CodeCancelled, "pipeline.commit", err.Error())
		}
		if r.opts.MaxNewCommits > 0 && newCommits >= r.opts.MaxNewCommits {
			result.StoppedEarly = true
			break
		}
		clip, _ := r.project.ClipByID(clipID)
		if err := manager.Transition(clipID, state.Measuring); err != nil {
			return result, err
		}
		if err := manager.Transition(clipID, state.Calibrating); err != nil {
			return result, err
		}
		envelope, ok := measured[clipID]
		if !ok {
			envelope, ok = cachedEnvelopes[clipID]
		}
		if !ok {
			_ = manager.Transition(clipID, state.Failed)
			return result, audioerr.Schema("clips."+clipID+".envelope", "envelope unavailable")
		}
		calibration, cached := engine.ClipCalibration{}, false
		if r.cache != nil {
			calibration, cached = r.cache.GetCalibration(r.calibrationKeys[clipID])
			if cached {
				result.ReusedCalibrationClips = append(result.ReusedCalibrationClips, clipID)
			}
		}
		if !cached {
			subset := r.project.Clone()
			subset.Clips = []model.Clip{clip}
			subsetEnvelopes := map[string]engine.ClipEnvelope{clipID: envelope}
			calibrationResult, err := engine.CalibrateProject(subset, subsetEnvelopes, r.opts.ToleranceQ16)
			if err != nil {
				_ = manager.Transition(clipID, state.Failed)
				return result, err
			}
			calibration = calibrationResult.Clips[0]
			result.RecomputedCalibrationClips = append(result.RecomputedCalibrationClips, clipID)
			if r.cache != nil {
				if err := r.cache.PutCalibration(r.calibrationKeys[clipID], calibration); err != nil {
					_ = manager.Transition(clipID, state.Failed)
					return result, err
				}
			}
		}
		if r.cache != nil {
			if _, recomputed := measured[clipID]; recomputed {
				if err := r.cache.PutEnvelope(r.envelopeKeys[clipID], envelope); err != nil {
					_ = manager.Transition(clipID, state.Failed)
					return result, err
				}
			}
		}
		if r.store != nil {
			if _, err := r.store.Commit(checkpoint.CommitInput{
				ClipID: clipID, EnvelopeKey: r.envelopeKeys[clipID],
				CalibrationKey: r.calibrationKeys[clipID],
				Envelope:       envelope, Calibration: calibration,
			}); err != nil {
				_ = manager.Transition(clipID, state.Failed)
				return result, err
			}
		}
		if err := manager.Transition(clipID, state.Committed); err != nil {
			return result, err
		}
		result.Envelopes[clipID] = envelope
		result.Calibrations[clipID] = calibration
		result.CommitOrder = append(result.CommitOrder, clipID)
		newCommits++
	}
	sort.Strings(result.RecomputedCalibrationClips)
	sort.Strings(result.ReusedCalibrationClips)
	result.NewCommits = newCommits
	return result, nil
}

func replayTransitions(manager *state.Manager, clipID string) error {
	if err := manager.Transition(clipID, state.Measuring); err != nil {
		return err
	}
	if err := manager.Transition(clipID, state.Calibrating); err != nil {
		return err
	}
	return manager.Transition(clipID, state.Committed)
}

// RecomputeAfterEdit is a convenience for the persistent-cache invalidation
// path. It uses no checkpoint because committed clips are frozen by contract.
func RecomputeAfterEdit(ctx context.Context, project model.Project, cacheDir string, opts Options) (RunResult, error) {
	opts.CheckpointDir = ""
	opts.CacheDir = cacheDir
	runner, err := NewRunner(project, opts)
	if err != nil {
		return RunResult{}, err
	}
	return runner.Run(ctx)
}

// Canceled reports the stable cancellation code.
func Canceled(err error) bool {
	return errors.Is(err, context.Canceled) || audioerr.IsCode(err, audioerr.CodeCancelled)
}
