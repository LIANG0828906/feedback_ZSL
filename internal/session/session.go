package session

import (
	"context"
	"fmt"
	"math"
	"sync"

	"spatialworkbench/internal/acoustics"
	"spatialworkbench/internal/checkpoint"
	"spatialworkbench/internal/model"
	"spatialworkbench/internal/werr"
)

type Injection struct {
	MissingChannel       string
	InvertArrivalChannel string
	InvertArrivalFrame   *int
	GainOverflowChannel  string
	GainOverflowFrame    *int
	BadFrame             *int
}

type FrameSnapshot struct {
	Index  int                    `json:"index"`
	State  State                  `json:"state"`
	Seq    int                    `json:"seq"`
	Params model.Params           `json:"params"`
	Result *acoustics.FrameResult `json:"result,omitempty"`
	Error  *werr.Error            `json:"error,omitempty"`
}

type Progress struct {
	Committed int `json:"committed"`
	Total     int `json:"total"`
}

type ResumeInfo struct {
	Loaded      int    `json:"loaded"`
	Invalidated bool   `json:"invalidated"`
	Reason      string `json:"reason,omitempty"`
}

type RunOutput struct {
	Frames         []acoustics.FrameResult `json:"frames"`
	CommitSequence []int                   `json:"commitSequence"`
	Output         [][]float64             `json:"output"`
	Progress       Progress                `json:"progress"`
}

type frame struct {
	index  int
	state  State
	seq    int
	params model.Params
	result *acoustics.FrameResult
	err    *werr.Error
}

type Session struct {
	mu         sync.Mutex
	sample     model.Sample
	params     model.Params
	generation checkpoint.Generation
	frames     []*frame
	committed  int
	store      *checkpoint.Store
	injection  Injection
	pool       *workerPool
	resume     ResumeInfo
	closed     bool
}

func Open(sample model.Sample, params model.Params, store *checkpoint.Store, injection Injection) (*Session, *werr.Error) {
	params = params.Normalized(sample)
	if err := validateInput(sample); err != nil {
		return nil, err
	}
	if err := params.Validate(sample); err != nil {
		return nil, err
	}
	if injection.MissingChannel != "" {
		e := werr.New(werr.CodeChan, "injected missing channel %q", injection.MissingChannel)
		e.Field = "channels"
		e.Frame = 0
		e.Channel = injection.MissingChannel
		return nil, e
	}
	total := params.TotalFrames(len(sample.Dry.Samples))
	if total == 0 {
		return nil, werr.Decode("dry.samples must produce at least one frame")
	}
	sampleHash := model.SampleHash(sample)
	session := &Session{
		sample: sample.Clone(),
		params: params,
		generation: checkpoint.Generation{
			InputHash:  sampleHash,
			ParamsHash: params.Hash(sample),
			Algorithm:  model.AlgorithmVersion,
			LayoutHash: model.LayoutHash(sample.Layout),
		},
		store:     store,
		injection: injection,
		pool:      newWorkerPool(params.MaxWorkers, params.BufferSamplesPerWorker),
	}
	session.frames = make([]*frame, total)
	for i := 0; i < total; i++ {
		session.frames[i] = &frame{index: i, state: StatePending, params: params.Clone(), seq: -1}
	}
	if store != nil {
		loaded, err := store.Load(context.Background(), session.generation)
		if err != nil {
			return nil, err
		}
		session.resume = ResumeInfo{Loaded: len(loaded.Records), Invalidated: loaded.Invalidated, Reason: loaded.Reason}
		for i, record := range loaded.Records {
			if record.Frame != i || record.Frame >= total {
				return nil, werr.New(werr.CodeState, "restored frame %d is outside session frame range [0,%d)", record.Frame, total)
			}
			result := record.Result.Clone()
			if result.InputHash != session.generation.InputHash ||
				result.ParamsHash != record.Params.Hash(session.sample) ||
				result.Algorithm != session.generation.Algorithm {
				return nil, werr.New(werr.CodeState, "restored frame %d has inconsistent generation metadata", i)
			}
			result.Params = record.Params.Clone()
			target := session.frames[i]
			target.state = StateCommitted
			target.seq = record.Seq
			target.params = record.Params.Clone()
			target.result = &result
		}
		session.committed = len(loaded.Records)
	}
	return session, nil
}

func validateInput(sample model.Sample) *werr.Error {
	if sample.SampleRate < model.MinSampleRate || sample.SampleRate > model.MaxSampleRate {
		e := werr.New(werr.CodeRate, "unsupported sample rate %d; supported interval is [%d,%d]", sample.SampleRate, model.MinSampleRate, model.MaxSampleRate)
		e.Field = "sampleRate"
		e.Actual = sample.SampleRate
		min, max := float64(model.MinSampleRate), float64(model.MaxSampleRate)
		e.Min, e.Max = &min, &max
		return e
	}
	if sample.IRSamples <= 0 || len(sample.Dry.Samples) == 0 {
		return werr.Decode("sample requires irSamples and dry.samples")
	}
	if len(sample.Layout.Channels) != len(sample.Channels) || len(sample.Channels) == 0 {
		e := werr.New(werr.CodeChan, "layout has %d channels but input has %d", len(sample.Layout.Channels), len(sample.Channels))
		e.Field = "channels"
		return e
	}
	layoutSet := map[string]bool{}
	for _, id := range sample.Layout.Channels {
		if id == "" || layoutSet[id] {
			e := werr.New(werr.CodeChan, "layout channel id %q is empty or duplicated", id)
			e.Field = "layout.channels"
			e.Channel = id
			return e
		}
		layoutSet[id] = true
	}
	seen := map[string]bool{}
	for _, channel := range sample.Channels {
		if channel.ID == "" || seen[channel.ID] || !layoutSet[channel.ID] {
			e := werr.New(werr.CodeChan, "channel id %q is empty, duplicated, or absent from layout", channel.ID)
			e.Field = "channels.id"
			e.Channel = channel.ID
			return e
		}
		seen[channel.ID] = true
		if len(channel.Samples) != sample.IRSamples {
			e := werr.Decode("channel %q has %d samples, expected %d", channel.ID, len(channel.Samples), sample.IRSamples)
			e.Field = "channels.samples"
			e.Channel = channel.ID
			return e
		}
		if math.IsNaN(channel.Truth.DirectEnergy) || math.IsInf(channel.Truth.DirectEnergy, 0) || channel.Truth.DirectEnergy <= 0 {
			e := werr.Decode("channel %q direct energy is not finite and positive", channel.ID)
			e.Field = "truth.directEnergy"
			e.Channel = channel.ID
			return e
		}
		if math.IsNaN(channel.Truth.DRRDB) || math.IsInf(channel.Truth.DRRDB, 0) {
			e := werr.Decode("channel %q drrDb is not finite", channel.ID)
			e.Field = "truth.drrDb"
			e.Channel = channel.ID
			return e
		}
		for n, sampleValue := range channel.Samples {
			if math.IsNaN(sampleValue) || math.IsInf(sampleValue, 0) {
				e := werr.Decode("channel %q sample %d is not finite", channel.ID, n)
				e.Field = "channels.samples"
				e.Channel = channel.ID
				return e
			}
			if math.Abs(sampleValue) > 1+1e-12 {
				e := werr.Frame(werr.CodeClip, 0, channel.ID, "channel sample %d has amplitude %g outside [-1,1]", n, sampleValue)
				e.Field = "channels.samples"
				e.Actual = sampleValue
				return e
			}
		}
		previous := channel.Truth.DirectArrival
		if previous < 0 || previous >= len(channel.Samples) {
			e := werr.New(werr.CodeOrder, "channel %q direct arrival %d is outside impulse response", channel.ID, previous)
			e.Field = "truth.directArrival"
			e.Channel = channel.ID
			return e
		}
		for i, reflection := range channel.Truth.Reflections {
			if reflection.Arrival <= previous || reflection.Arrival >= len(channel.Samples) {
				e := werr.New(werr.CodeOrder, "channel %q reflection %d arrival %d is not monotonic after %d", channel.ID, i, reflection.Arrival, previous)
				e.Field = "truth.reflections"
				e.Channel = channel.ID
				e.Actual = reflection.Arrival
				e.Expected = fmt.Sprintf(">%d", previous)
				return e
			}
			if math.IsNaN(reflection.Energy) || math.IsInf(reflection.Energy, 0) || reflection.Energy < 0 {
				e := werr.Decode("channel %q reflection %d energy is invalid", channel.ID, i)
				e.Field = "truth.reflections.energy"
				e.Channel = channel.ID
				return e
			}
			previous = reflection.Arrival
		}
	}
	for _, id := range sample.Layout.Channels {
		if !seen[id] {
			e := werr.New(werr.CodeChan, "layout channel %q has no impulse response", id)
			e.Field = "channels"
			e.Channel = id
			return e
		}
	}
	for n, dry := range sample.Dry.Samples {
		if math.IsNaN(dry) || math.IsInf(dry, 0) {
			e := werr.Decode("dry sample %d is not finite", n)
			e.Field = "dry.samples"
			return e
		}
	}
	return nil
}

func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.store != nil {
		_ = s.store.Close()
	}
	s.closed = true
}

func (s *Session) Progress() Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Progress{Committed: s.committed, Total: len(s.frames)}
}

func (s *Session) ResumeInfo() ResumeInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resume
}

func (s *Session) Generation() checkpoint.Generation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

func (s *Session) Frames() []FrameSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FrameSnapshot, len(s.frames))
	for i, f := range s.frames {
		out[i] = s.snapshotLocked(f)
	}
	return out
}

func (s *Session) Frame(index int) (FrameSnapshot, *werr.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.frames) {
		return FrameSnapshot{}, werr.Frame(werr.CodeState, index, "", "frame is outside [0,%d)", len(s.frames))
	}
	return s.snapshotLocked(s.frames[index]), nil
}

func (s *Session) snapshotLocked(f *frame) FrameSnapshot {
	snapshot := FrameSnapshot{Index: f.index, State: f.state, Seq: f.seq, Params: f.params.Clone(), Error: f.err}
	if f.result != nil {
		result := f.result.Clone()
		snapshot.Result = &result
	}
	return snapshot
}

func (s *Session) ProcessNext(ctx context.Context) (FrameSnapshot, *werr.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed >= len(s.frames) {
		e := werr.New(werr.CodeState, "all %d frames are already committed", len(s.frames))
		e.Field = "state"
		return FrameSnapshot{}, e
	}
	if err := s.ensureOpenLocked(); err != nil {
		return FrameSnapshot{}, err
	}
	index := s.committed
	if s.frames[index].state == StateFailed {
		e := werr.New(werr.CodeState, "frame %d is failed; call RetryFrame before advancing", index)
		e.Field = "state"
		e.Frame = index
		return s.snapshotLocked(s.frames[index]), e
	}
	if err := s.processFrameLocked(ctx, index); err != nil {
		return s.snapshotLocked(s.frames[index]), err
	}
	return s.snapshotLocked(s.frames[index]), nil
}

func (s *Session) CommitNext(ctx context.Context) (FrameSnapshot, *werr.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed >= len(s.frames) {
		e := werr.New(werr.CodeState, "all %d frames are already committed", len(s.frames))
		e.Field = "state"
		return FrameSnapshot{}, e
	}
	if err := s.ensureOpenLocked(); err != nil {
		return FrameSnapshot{}, err
	}
	index := s.committed
	if err := s.commitFrameLocked(ctx, index); err != nil {
		return s.snapshotLocked(s.frames[index]), err
	}
	return s.snapshotLocked(s.frames[index]), nil
}

func (s *Session) CommitFrame(ctx context.Context, index int) (FrameSnapshot, *werr.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return FrameSnapshot{}, err
	}
	if index < 0 || index >= len(s.frames) {
		return FrameSnapshot{}, werr.Frame(werr.CodeState, index, "", "frame is outside [0,%d)", len(s.frames))
	}
	if err := s.commitFrameLocked(ctx, index); err != nil {
		return s.snapshotLocked(s.frames[index]), err
	}
	return s.snapshotLocked(s.frames[index]), nil
}

func (s *Session) ProcessAndCommitNext(ctx context.Context) (FrameSnapshot, *werr.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed >= len(s.frames) {
		e := werr.New(werr.CodeState, "all %d frames are already committed", len(s.frames))
		e.Field = "state"
		return FrameSnapshot{}, e
	}
	if err := s.ensureOpenLocked(); err != nil {
		return FrameSnapshot{}, err
	}
	index := s.committed
	f := s.frames[index]
	if f.state == StateFailed {
		e := werr.New(werr.CodeState, "frame %d is failed; call RetryFrame before advancing", index)
		e.Field = "state"
		e.Frame = index
		return s.snapshotLocked(f), e
	}
	if f.state != StateEstimated {
		if err := s.processFrameLocked(ctx, index); err != nil {
			return s.snapshotLocked(f), err
		}
	}
	if err := s.commitFrameLocked(ctx, index); err != nil {
		return s.snapshotLocked(f), err
	}
	return s.snapshotLocked(f), nil
}

func (s *Session) ProcessAndCommitAll(ctx context.Context) (Progress, *werr.Error) {
	for {
		s.mu.Lock()
		done := s.committed >= len(s.frames)
		s.mu.Unlock()
		if done {
			return s.Progress(), nil
		}
		if _, err := s.ProcessAndCommitNext(ctx); err != nil {
			return s.Progress(), err
		}
		if err := ctx.Err(); err != nil {
			e := werr.New(werr.CodeState, "processing canceled: %v", err)
			return s.Progress(), e
		}
	}
}

// ProcessAndCommitBatch estimates up to maxFrames in a bounded worker pool,
// then reconstructs, renders, and commits them in strict frame order.
func (s *Session) ProcessAndCommitBatch(ctx context.Context, maxFrames int) (int, *werr.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return 0, err
	}
	if maxFrames <= 0 {
		maxFrames = len(s.frames)
	}
	start := s.committed
	end := start + maxFrames
	if end > len(s.frames) {
		end = len(s.frames)
	}
	if start >= end {
		return 0, nil
	}
	tasks := make([]func(context.Context) (any, *werr.Error), 0, end-start)
	sampleCounts := make([]int, 0, end-start)
	for index := start; index < end; index++ {
		index := index
		f := s.frames[index]
		if f.state == StateFailed {
			return 0, werr.New(werr.CodeState, "frame %d is failed; call RetryFrame before batching", index)
		}
		if f.state == StateEstimated {
			if err := s.transitionLocked(f, StatePending); err != nil {
				return 0, err
			}
			f.result = nil
			f.err = nil
		}
		frameStart := index * s.params.Hop
		frameEnd := frameStart + s.params.FrameLength
		if frameEnd > s.sample.IRSamples {
			frameEnd = s.sample.IRSamples
		}
		span := frameEnd - frameStart
		if span <= 0 {
			span = 1
		}
		sampleCounts = append(sampleCounts, span*len(s.sample.Channels))
		tasks = append(tasks, func(ctx context.Context) (any, *werr.Error) {
			if err := injectionError(s.injection, index, s.sample); err != nil {
				return nil, err
			}
			estimate, err := acoustics.EstimateFrame(s.sample, s.params, index)
			if err != nil {
				return nil, err
			}
			return estimate, nil
		})
	}
	results, err := s.pool.Run(ctx, tasks, sampleCounts)
	if err != nil {
		badIndex := start
		for _, result := range results {
			if result.err != nil {
				badIndex = result.index + start
				break
			}
		}
		f := s.frames[badIndex]
		if f.state == StatePending {
			_ = s.transitionLocked(f, StateEstimated)
		}
		if f.state == StateEstimated {
			_ = s.transitionLocked(f, StateFailed)
		}
		f.err = err
		return 0, err
	}
	committedNow := 0
	for i, result := range results {
		index := start + i
		f := s.frames[index]
		estimate, ok := result.value.(acoustics.FrameEstimate)
		if !ok {
			return committedNow, werr.New(werr.CodeFrame, "worker returned an invalid estimate for frame %d", index)
		}
		if err := s.transitionLocked(f, StateEstimated); err != nil {
			return committedNow, err
		}
		previous := acoustics.ReconstructionState{}
		if index > 0 {
			previousFrame := s.frames[index-1]
			if previousFrame.result == nil {
				e := werr.Frame(werr.CodeFrame, index, "", "previous frame has no reconstruction state")
				f.err = e
				_ = s.transitionLocked(f, StateFailed)
				return committedNow, e
			}
			previous = previousFrame.result.Reconstruction.State
		}
		reconstruction, err := acoustics.Reconstruct(s.sample, f.params, estimate, previous)
		if err != nil {
			f.err = err
			_ = s.transitionLocked(f, StateFailed)
			return committedNow, err
		}
		render, err := acoustics.RenderFrame(s.sample, f.params, estimate, reconstruction)
		if err != nil {
			f.err = err
			_ = s.transitionLocked(f, StateFailed)
			return committedNow, err
		}
		resultFrame := acoustics.FrameResult{
			Frame:          index,
			Params:         f.params.Clone(),
			Estimate:       estimate,
			Reconstruction: reconstruction,
			Render:         render,
			InputHash:      s.generation.InputHash,
			ParamsHash:     f.params.Hash(s.sample),
			Algorithm:      s.generation.Algorithm,
		}
		f.result = &resultFrame
		if err := s.commitFrameLocked(ctx, index); err != nil {
			return committedNow, err
		}
		committedNow++
	}
	return committedNow, nil
}

func (s *Session) processFrameLocked(ctx context.Context, index int) *werr.Error {
	if err := ctx.Err(); err != nil {
		return werr.Frame(werr.CodeState, index, "", "processing canceled: %v", err)
	}
	if index < 0 || index >= len(s.frames) {
		return werr.Frame(werr.CodeState, index, "", "frame is outside [0,%d)", len(s.frames))
	}
	f := s.frames[index]
	if index < s.committed || f.state == StateCommitted {
		e := werr.New(werr.CodeFrozen, "committed frame %d cannot be re-estimated or rewritten", index)
		e.Frame = index
		e.Field = "frame"
		return e
	}
	if index != s.committed {
		e := werr.New(werr.CodeState, "frame %d cannot advance before committed prefix %d", index, s.committed)
		e.Frame = index
		e.Field = "state"
		return e
	}
	if f.state == StateFailed {
		e := werr.New(werr.CodeState, "failed frame %d must be retried explicitly", index)
		e.Frame = index
		e.Field = "state"
		return e
	}
	if f.state == StateEstimated {
		if err := s.transitionLocked(f, StatePending); err != nil {
			return err
		}
	}
	if err := s.transitionLocked(f, StateEstimated); err != nil {
		return err
	}
	f.err = nil
	f.result = nil
	if err := injectionError(s.injection, index, s.sample); err != nil {
		f.err = err
		_ = s.transitionLocked(f, StateFailed)
		return err
	}
	estimate, err := acoustics.EstimateFrame(s.sample, f.params, index)
	if err != nil {
		f.err = err
		_ = s.transitionLocked(f, StateFailed)
		return err
	}
	previous := acoustics.ReconstructionState{}
	if index > 0 {
		if s.frames[index-1].result == nil {
			e := werr.Frame(werr.CodeFrame, index, "", "previous frame has no reconstruction state")
			f.err = e
			_ = s.transitionLocked(f, StateFailed)
			return e
		}
		previous = s.frames[index-1].result.Reconstruction.State
	}
	reconstruction, err := acoustics.Reconstruct(s.sample, f.params, estimate, previous)
	if err != nil {
		f.err = err
		_ = s.transitionLocked(f, StateFailed)
		return err
	}
	render, err := acoustics.RenderFrame(s.sample, f.params, estimate, reconstruction)
	if err != nil {
		f.err = err
		_ = s.transitionLocked(f, StateFailed)
		return err
	}
	result := acoustics.FrameResult{
		Frame:          index,
		Params:         f.params.Clone(),
		Estimate:       estimate,
		Reconstruction: reconstruction,
		Render:         render,
		InputHash:      s.generation.InputHash,
		ParamsHash:     f.params.Hash(s.sample),
		Algorithm:      s.generation.Algorithm,
	}
	f.result = &result
	return nil
}

func (s *Session) commitFrameLocked(ctx context.Context, index int) *werr.Error {
	if index < 0 || index >= len(s.frames) {
		return werr.Frame(werr.CodeState, index, "", "frame is outside [0,%d)", len(s.frames))
	}
	f := s.frames[index]
	if f.state == StateCommitted {
		e := werr.New(werr.CodeFrozen, "committed frame %d is frozen", index)
		e.Frame = index
		e.Field = "frame"
		return e
	}
	if index != s.committed {
		e := werr.New(werr.CodeState, "frame %d cannot commit out of order; next commit is %d", index, s.committed)
		e.Frame = index
		e.Field = "seq"
		return e
	}
	if f.state != StateEstimated || f.result == nil {
		e := werr.New(werr.CodeState, "frame %d is %s; commit requires estimated", index, f.state)
		e.Frame = index
		e.Field = "state"
		return e
	}
	if s.store != nil {
		if err := s.store.Commit(ctx, s.generation, *f.result); err != nil {
			return err
		}
	}
	if err := s.transitionLocked(f, StateCommitted); err != nil {
		return err
	}
	f.seq = s.committed
	s.committed++
	return nil
}

func (s *Session) RetryFrame(index int) *werr.Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.frames) {
		return werr.Frame(werr.CodeState, index, "", "frame is outside [0,%d)", len(s.frames))
	}
	f := s.frames[index]
	if f.state != StateFailed {
		e := werr.New(werr.CodeState, "retry requires failed state, got %s", f.state)
		e.Frame = index
		e.Field = "state"
		return e
	}
	if err := s.transitionLocked(f, StatePending); err != nil {
		return err
	}
	f.err = nil
	f.result = nil
	return nil
}

func (s *Session) SetInjection(injection Injection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.injection = injection
}

// ReestimateUncommitted discards only uncommitted frame outputs. A committed
// target is rejected without altering any state.
func (s *Session) ReestimateUncommitted(start int) *werr.Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if start < 0 {
		start = 0
	}
	if start < s.committed {
		e := werr.New(werr.CodeFrozen, "frame %d is committed and cannot be re-estimated", start)
		e.Frame = start
		e.Field = "frame"
		return e
	}
	for index := start; index < len(s.frames); index++ {
		f := s.frames[index]
		switch f.state {
		case StateEstimated:
			if err := s.transitionLocked(f, StatePending); err != nil {
				return err
			}
		case StateFailed:
			if err := s.transitionLocked(f, StatePending); err != nil {
				return err
			}
		}
		f.result = nil
		f.err = nil
		f.seq = -1
	}
	return nil
}

// UpdateUncommittedParams changes the parameter snapshot of a non-committed
// frame. Once a checkpoint prefix exists, changing the generation would make a
// mixed checkpoint, so the request is rejected as frozen.
func (s *Session) UpdateUncommittedParams(index int, params model.Params) *werr.Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.frames) {
		return werr.Frame(werr.CodeState, index, "", "frame is outside [0,%d)", len(s.frames))
	}
	f := s.frames[index]
	if f.state == StateCommitted || index < s.committed {
		e := werr.New(werr.CodeFrozen, "committed frame %d is frozen", index)
		e.Frame = index
		e.Field = "frame"
		return e
	}
	params = params.Normalized(s.sample)
	if err := params.Validate(s.sample); err != nil {
		return err
	}
	newHash := params.Hash(s.sample)
	if s.committed > 0 && newHash != s.params.Hash(s.sample) {
		e := werr.New(werr.CodeFrozen, "parameter generation changed after frame 0 was committed; checkpoint generation cannot mix versions")
		e.Frame = 0
		e.Field = "params"
		return e
	}
	if s.committed == 0 && newHash != s.params.Hash(s.sample) {
		s.params = params.Clone()
		s.generation.ParamsHash = newHash
		for _, item := range s.frames {
			item.params = params.Clone()
			switch item.state {
			case StateEstimated:
				if err := s.transitionLocked(item, StatePending); err != nil {
					return err
				}
			case StateFailed:
				if err := s.transitionLocked(item, StatePending); err != nil {
					return err
				}
			}
			item.result = nil
			item.err = nil
			item.seq = -1
		}
		return nil
	}
	if f.state == StateEstimated {
		if err := s.transitionLocked(f, StatePending); err != nil {
			return err
		}
	}
	if f.state == StateFailed {
		if err := s.transitionLocked(f, StatePending); err != nil {
			return err
		}
	}
	f.params = params.Clone()
	f.result = nil
	f.err = nil
	return nil
}

func (s *Session) transitionLocked(f *frame, to State) *werr.Error {
	if !legalTransition(f.state, to) {
		return transitionError(f.index, f.state, to)
	}
	f.state = to
	return nil
}

func (s *Session) ensureOpenLocked() *werr.Error {
	if s.closed {
		return werr.New(werr.CodeState, "session is closed")
	}
	return nil
}

func (s *Session) BufferEvents() []BufferEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pool.Events()
}

func injectionError(injection Injection, frame int, sample model.Sample) *werr.Error {
	if injection.BadFrame != nil && *injection.BadFrame == frame {
		channel := ""
		if len(sample.Channels) > 0 {
			channel = sample.Channels[0].ID
		}
		e := werr.Frame(werr.CodeFrame, frame, channel, "injected bad frame")
		e.Field = "frame"
		return e
	}
	if injection.InvertArrivalChannel != "" {
		target := 0
		if injection.InvertArrivalFrame != nil {
			target = *injection.InvertArrivalFrame
		}
		if frame == target {
			e := werr.Frame(werr.CodeOrder, frame, injection.InvertArrivalChannel, "injected reversed direct/reflection arrival order")
			e.Field = "truth.reflections.arrival"
			return e
		}
	}
	if injection.GainOverflowChannel != "" {
		target := 0
		if injection.GainOverflowFrame != nil {
			target = *injection.GainOverflowFrame
		}
		if frame == target {
			e := werr.Frame(werr.CodeClip, frame, injection.GainOverflowChannel, "injected gain overflow")
			e.Field = "channelGain"
			return e
		}
	}
	return nil
}

func (s *Session) Output() (RunOutput, *werr.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed != len(s.frames) {
		e := werr.New(werr.CodeState, "cannot build output: %d/%d frames committed", s.committed, len(s.frames))
		e.Field = "progress"
		return RunOutput{}, e
	}
	output := make([][]float64, len(s.sample.Layout.Channels))
	for i := range output {
		output[i] = make([]float64, 0, len(s.sample.Dry.Samples))
	}
	frames := make([]acoustics.FrameResult, 0, len(s.frames))
	sequence := make([]int, 0, len(s.frames))
	for index, f := range s.frames {
		if f.state != StateCommitted || f.result == nil {
			e := werr.Frame(werr.CodeState, index, "", "committed frame has no result")
			return RunOutput{}, e
		}
		result := f.result.Clone()
		frames = append(frames, result)
		sequence = append(sequence, f.seq)
		if len(result.Render) != len(output) {
			e := werr.Frame(werr.CodeFrame, index, "", "render has %d channels, expected %d", len(result.Render), len(output))
			return RunOutput{}, e
		}
		for ch := range output {
			output[ch] = append(output[ch], result.Render[ch]...)
		}
	}
	for ch := range output {
		if len(output[ch]) != len(s.sample.Dry.Samples) {
			e := werr.New(werr.CodeFrame, "rendered channel %d has %d samples, dry input has %d", ch, len(output[ch]), len(s.sample.Dry.Samples))
			e.Field = "render"
			return RunOutput{}, e
		}
	}
	return RunOutput{
		Frames:         frames,
		CommitSequence: sequence,
		Output:         output,
		Progress:       Progress{Committed: s.committed, Total: len(s.frames)},
	}, nil
}

func (s *Session) CheckpointPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil {
		return ""
	}
	return s.store.Path()
}

func (s *Session) BufferUsage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return formatBufferUsage(s.pool.Events())
}

func exactFloatEqual(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b)
}
