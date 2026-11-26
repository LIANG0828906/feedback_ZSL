// Package engine implements deterministic integer frame measurement and
// envelope calibration.
package engine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"dialogueaudio/audioerr"
	"dialogueaudio/fixed"
	"dialogueaudio/model"
)

// FrameParams are the quantization parameters for a measurement run.
type FrameParams struct {
	SampleRate     int   `json:"sample_rate"`
	FrameLen       int64 `json:"frame_len_samples"`
	Hop            int64 `json:"hop_samples"`
	SessionGainQ16 int64 `json:"session_gain_q16"`
}

// FrameEnvelope records one logical frame. All energy values are retained so
// every truncation step can be audited.
type FrameEnvelope struct {
	ClipID         string        `json:"clip_id"`
	FrameIndex     int           `json:"frame_index"`
	StartSample    int64         `json:"start_sample"`
	EndSample      int64         `json:"end_sample"`
	SampleCount    int           `json:"sample_count"`
	SumSquares     int64         `json:"sum_squares"`
	RawEnergy      int64         `json:"raw_energy"`
	SessionEnergy  int64         `json:"session_energy"`
	ClipGainEnergy int64         `json:"clip_gain_energy"`
	WeightedEnergy int64         `json:"weighted_energy"`
	PeakOffset     int           `json:"peak_offset"`
	PeakAbs        int64         `json:"peak_abs"`
	PeakSample     int16         `json:"peak_sample"`
	EnvelopeQ16    int64         `json:"envelope_q16"`
	Traces         []fixed.Trace `json:"traces,omitempty"`
}

// ClipEnvelope is the segmented result for one clip. SegmentEnergy is the
// arithmetic mean of weighted frame energy; SegmentEnvelopeQ16 is its fixed
// dB representation.
type ClipEnvelope struct {
	ClipID             string          `json:"clip_id"`
	ContentHash        string          `json:"content_hash"`
	FrameLen           int64           `json:"frame_len_samples"`
	Hop                int64           `json:"hop_samples"`
	SessionGainQ16     int64           `json:"session_gain_q16"`
	ClipGainQ16        int64           `json:"clip_gain_q16"`
	LoudnessWeightQ16  int64           `json:"loudness_weight_q16"`
	Frames             []FrameEnvelope `json:"frames"`
	SegmentEnergy      int64           `json:"segment_energy"`
	SegmentEnvelopeQ16 int64           `json:"segment_envelope_q16"`
	PeakOffset         int64           `json:"peak_offset"`
	PeakAbs            int64           `json:"peak_abs"`
	PeakSample         int16           `json:"peak_sample"`
	SummaryTraces      []fixed.Trace   `json:"summary_traces,omitempty"`
}

// MeasureClip computes frames from clip-relative sample zero. chunkSamples is
// the per-worker sampling-buffer budget; changing it changes only traversal,
// never the result.
func MeasureClip(clip model.Clip, params FrameParams, chunkSamples int) (ClipEnvelope, error) {
	env := ClipEnvelope{
		ClipID:            clip.ID,
		ContentHash:       clip.ContentHash,
		FrameLen:          params.FrameLen,
		Hop:               params.Hop,
		SessionGainQ16:    params.SessionGainQ16,
		ClipGainQ16:       clip.GainQ16,
		LoudnessWeightQ16: clip.LoudnessWeightQ16,
	}
	if params.SampleRate <= 0 || params.FrameLen <= 0 || params.Hop <= 0 {
		return env, audioerr.Schema("clip."+clip.ID+".frame_params", "sample rate, frame length and hop must be positive")
	}
	duration := clip.DurationSamples()
	if duration <= 0 {
		return env, audioerr.Schema("clip."+clip.ID+".boundary", "invalid clip boundary")
	}
	if int64(len(clip.PCM)) != duration {
		return env, audioerr.Schema("clip."+clip.ID+".pcm",
			fmt.Sprintf("PCM sample count %d does not match duration %d", len(clip.PCM), duration))
	}
	if params.SessionGainQ16 <= 0 || clip.GainQ16 <= 0 || clip.LoudnessWeightQ16 <= 0 {
		return env, audioerr.Schema("clip."+clip.ID+".gain", "gains and weight must be positive")
	}
	framesTotal := 0
	for start := int64(0); start < duration; start += params.Hop {
		framesTotal++
	}
	env.Frames = make([]FrameEnvelope, 0, framesTotal)
	framesPerChunk := 1
	if chunkSamples > 0 && int64(chunkSamples) >= params.FrameLen {
		framesPerChunk = int(int64(chunkSamples) / params.FrameLen)
	}
	frameIndex := 0
	for chunkStart := 0; chunkStart < framesTotal; chunkStart += framesPerChunk {
		chunkEnd := chunkStart + framesPerChunk
		if chunkEnd > framesTotal {
			chunkEnd = framesTotal
		}
		for fi := chunkStart; fi < chunkEnd; fi++ {
			start := int64(fi) * params.Hop
			end := start + params.FrameLen
			if end > duration {
				end = duration
			}
			frame, err := measureFrame(clip, params, start, end, frameIndex)
			if err != nil {
				return env, err
			}
			env.Frames = append(env.Frames, frame)
			frameIndex++
		}
	}
	if len(env.Frames) == 0 {
		return env, audioerr.Schema("clip."+clip.ID+".frames", "no logical frames were produced")
	}
	var energySum int64
	for _, frame := range env.Frames {
		var err error
		energySum, err = fixed.CheckedAdd(energySum, frame.WeightedEnergy, "clip."+clip.ID+".segment_energy")
		if err != nil {
			return env, err
		}
		if frame.PeakAbs > env.PeakAbs {
			env.PeakAbs = frame.PeakAbs
			env.PeakOffset = frame.StartSample + int64(frame.PeakOffset)
			env.PeakSample = frame.PeakSample
		}
	}
	var tr fixed.Trace
	var err error
	env.SegmentEnergy, tr, err = fixed.Rescale(energySum, int64(len(env.Frames)), 1, fixed.RoundTrunc,
		"clip."+clip.ID+".segment_energy")
	if err != nil {
		return env, err
	}
	env.SummaryTraces = append(env.SummaryTraces, tr)
	env.SegmentEnvelopeQ16, tr, err = fixed.EnergyToDBQ16(env.SegmentEnergy, "clip."+clip.ID+".segment_envelope_q16")
	if err != nil {
		return env, err
	}
	env.SummaryTraces = append(env.SummaryTraces, tr)
	return env, nil
}

func measureFrame(clip model.Clip, params FrameParams, start, end int64, frameIndex int) (FrameEnvelope, error) {
	frame := FrameEnvelope{
		ClipID:      clip.ID,
		FrameIndex:  frameIndex,
		StartSample: clip.StartSample + start,
		EndSample:   clip.StartSample + end,
		SampleCount: int(end - start),
	}
	if start < 0 || end > int64(len(clip.PCM)) || end <= start {
		return frame, audioerr.Schema("clip."+clip.ID+".frame_boundary", "frame lies outside PCM")
	}
	samples := clip.PCM[start:end]
	for offset, value := range samples {
		s64 := int64(value)
		square, err := fixed.CheckedMul(s64, s64, "clip."+clip.ID+".sum_squares")
		if err != nil {
			return frame, err
		}
		frame.SumSquares, err = fixed.CheckedAdd(frame.SumSquares, square, "clip."+clip.ID+".sum_squares")
		if err != nil {
			return frame, err
		}
		abs := s64
		if abs < 0 {
			abs = -abs
		}
		if abs > frame.PeakAbs {
			frame.PeakAbs = abs
			frame.PeakOffset = offset
			frame.PeakSample = value
		}
	}
	var tr fixed.Trace
	var err error
	frame.RawEnergy, tr, err = fixed.Rescale(frame.SumSquares, int64(frame.SampleCount), 1, fixed.RoundTrunc,
		fmt.Sprintf("clip.%s.frames[%d].raw_energy", clip.ID, frameIndex))
	if err != nil {
		return frame, err
	}
	frame.Traces = append(frame.Traces, tr)
	frame.SessionEnergy, err = fixed.ApplyGainToEnergy(frame.RawEnergy, params.SessionGainQ16,
		fmt.Sprintf("clip.%s.frames[%d].session_gain", clip.ID, frameIndex))
	if err != nil {
		return frame, err
	}
	frame.Traces = append(frame.Traces, fixed.Trace{
		Operation: "apply_linear_gain_twice", Field: fmt.Sprintf("clip.%s.frames[%d].session_gain", clip.ID, frameIndex),
		Input: frame.RawEnergy, Output: frame.SessionEnergy, Rounding: fixed.RoundTrunc,
		Truncated: true, TruncationAt: "muldiv_q16",
	})
	frame.ClipGainEnergy, err = fixed.ApplyGainToEnergy(frame.SessionEnergy, clip.GainQ16,
		fmt.Sprintf("clip.%s.frames[%d].clip_gain", clip.ID, frameIndex))
	if err != nil {
		return frame, err
	}
	frame.Traces = append(frame.Traces, fixed.Trace{
		Operation: "apply_linear_gain_twice", Field: fmt.Sprintf("clip.%s.frames[%d].clip_gain", clip.ID, frameIndex),
		Input: frame.SessionEnergy, Output: frame.ClipGainEnergy, Rounding: fixed.RoundTrunc,
		Truncated: true, TruncationAt: "muldiv_q16",
	})
	frame.WeightedEnergy, tr, err = fixed.Rescale(frame.ClipGainEnergy, fixed.Q16, clip.LoudnessWeightQ16,
		fixed.RoundTrunc, fmt.Sprintf("clip.%s.frames[%d].weighted_energy", clip.ID, frameIndex))
	if err != nil {
		return frame, err
	}
	frame.Traces = append(frame.Traces, tr)
	frame.EnvelopeQ16, tr, err = fixed.EnergyToDBQ16(frame.WeightedEnergy,
		fmt.Sprintf("clip.%s.frames[%d].envelope_q16", clip.ID, frameIndex))
	if err != nil {
		return frame, err
	}
	frame.Traces = append(frame.Traces, tr)
	return frame, nil
}

// QueueEvent records bounded-pool backpressure that actually occurred.
type QueueEvent struct {
	ClipID       string `json:"clip_id"`
	Reason       string `json:"reason"`
	WaitedMicros int64  `json:"waited_micros"`
}

// BatchOptions bound worker parallelism, per-worker sample buffering, and the
// pending clip queue.
type BatchOptions struct {
	MaxParallel         int `json:"max_parallel"`
	SampleBufferSamples int `json:"sample_buffer_samples"`
	QueueBudget         int `json:"queue_budget"`
}

// BatchResult preserves deterministic per-clip results while exposing
// non-deterministic wait durations only as diagnostics.
type BatchResult struct {
	Envelopes   map[string]ClipEnvelope `json:"envelopes"`
	QueueEvents []QueueEvent            `json:"queue_events,omitempty"`
}

type measureJob struct {
	clip model.Clip
}

type measureOutcome struct {
	envelope ClipEnvelope
	err      error
}

// MeasureClips measures clips with a bounded queue and worker pool. Results
// are keyed by ID, so scheduling cannot affect the sample values.
func MeasureClips(ctx context.Context, clips []model.Clip, params FrameParams, opt BatchOptions) (BatchResult, error) {
	result := BatchResult{Envelopes: make(map[string]ClipEnvelope, len(clips))}
	if opt.MaxParallel <= 0 {
		opt.MaxParallel = 1
	}
	if opt.QueueBudget <= 0 {
		opt.QueueBudget = 1
	}
	if opt.SampleBufferSamples <= 0 {
		opt.SampleBufferSamples = int(params.FrameLen)
	}
	jobs := make(chan measureJob, opt.QueueBudget)
	outcomes := make(chan measureOutcome, len(clips))
	var wg sync.WaitGroup
	for worker := 0; worker < opt.MaxParallel; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}
					env, err := MeasureClip(job.clip, params, opt.SampleBufferSamples)
					outcomes <- measureOutcome{envelope: env, err: err}
				}
			}
		}()
	}
	events := make([]QueueEvent, 0)
	enqueueDone := make(chan struct{})
	go func() {
		defer close(enqueueDone)
		defer close(jobs)
		for _, clip := range clips {
			select {
			case jobs <- measureJob{clip: clip}:
				continue
			case <-ctx.Done():
				return
			default:
			}
			start := time.Now()
			select {
			case jobs <- measureJob{clip: clip}:
				events = append(events, QueueEvent{
					ClipID: clip.ID, Reason: "queue_budget_exhausted",
					WaitedMicros: time.Since(start).Microseconds(),
				})
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		<-enqueueDone
		wg.Wait()
		close(outcomes)
	}()
	var firstErr error
	expected := len(clips)
	for outcome := range outcomes {
		if outcome.err != nil {
			if firstErr == nil {
				firstErr = outcome.err
			}
			continue
		}
		result.Envelopes[outcome.envelope.ClipID] = outcome.envelope
	}
	if ctx.Err() != nil {
		return result, audioerr.New(audioerr.CodeCancelled, "measure", ctx.Err().Error())
	}
	if firstErr != nil {
		return result, firstErr
	}
	if len(result.Envelopes) != expected {
		return result, audioerr.New(audioerr.CodeCancelled, "measure", "worker pool terminated before all clips completed")
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].ClipID != events[j].ClipID {
			return events[i].ClipID < events[j].ClipID
		}
		return events[i].WaitedMicros < events[j].WaitedMicros
	})
	result.QueueEvents = events
	return result, nil
}

// AbsInt64 returns an absolute integer value.
func AbsInt64(v int64) int64 {
	if v == math.MinInt64 {
		return math.MaxInt64
	}
	if v < 0 {
		return -v
	}
	return v
}
