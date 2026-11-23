package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"loudnessworkbench/apperr"
	"loudnessworkbench/audio"
	"loudnessworkbench/checkpoint"
	"loudnessworkbench/dsp"
	"loudnessworkbench/project"
)

type Session struct {
	runMu   sync.Mutex
	stateMu sync.Mutex
	program *project.Program
	store   *checkpoint.Store
	states  []*stateMachine
	last    *Result
}

func NewSession(program *project.Program, store *checkpoint.Store) (*Session, error) {
	if program == nil {
		return nil, apperr.New(apperr.CodeParameter, "program is required")
	}
	if err := program.Validate(); err != nil {
		return nil, err
	}
	session := &Session{
		program: program,
		store:   store,
		states:  make([]*stateMachine, len(program.Segments)),
	}
	for i, segment := range program.Segments {
		session.states[i] = newStateMachine(i, segment.ID)
	}
	return session, nil
}

func (s *Session) Program() *project.Program {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.program
}

func (s *Session) LastResult() *Result {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.last
}

type Status struct {
	Index int          `json:"index"`
	ID    string       `json:"id"`
	State SegmentState `json:"state"`
}

func (s *Session) Statuses() []Status {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	statuses := make([]Status, len(s.states))
	for i, state := range s.states {
		statuses[i] = Status{Index: i, ID: s.program.Segments[i].ID, State: state.Current()}
	}
	return statuses
}

// UpdateChain is allowed only before any segment has been committed. This keeps
// committed exports immutable while allowing pending segments to be tuned.
func (s *Session) UpdateChain(chain dsp.ChainConfig) error {
	if !s.runMu.TryLock() {
		return apperr.New(apperr.CodeState, "cannot change parameters while a run is active").WithField("state")
	}
	defer s.runMu.Unlock()
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	for _, state := range s.states {
		if state.Current() == StateCommitted {
			return apperr.New(apperr.CodeState, "committed segments are immutable; reset the session to recalculate").
				WithField("state").WithExpected(StatePending).WithActual(StateCommitted)
		}
	}
	if err := chain.Validate(s.program.SampleRateHz); err != nil {
		return err
	}
	clone := s.program.CloneWithChain(chain)
	if err := clone.Validate(); err != nil {
		return err
	}
	if s.store != nil {
		if err := s.store.Invalidate(); err != nil {
			return err
		}
	}
	for _, state := range s.states {
		switch state.Current() {
		case StateProcessing, StateFailed:
			_ = state.Transition(StatePending)
		}
	}
	s.program = clone
	s.last = nil
	return nil
}

func (s *Session) Reset() (*Session, error) {
	s.stateMu.Lock()
	program := s.program.CloneWithChain(s.program.ChainConfig())
	store := s.store
	s.stateMu.Unlock()
	if store != nil {
		if err := store.Invalidate(); err != nil {
			return nil, err
		}
	}
	return NewSession(program, store)
}

type prepResult struct {
	index        int
	source       dsp.Measurement
	compressed   dsp.Measurement
	signal       audio.Signal
	gainDB       []float64
	maxReduction float64
	err          *apperr.Error
}

// Run executes the bounded parallel analysis/finalization pipeline. It computes
// all committed loudness statistics before making the global normalization
// decision, then commits each segment through the serialized snapshot store.
func (s *Session) Run(ctx context.Context, options RunOptions) (*Result, error) {
	if err := options.Normalize(); err != nil {
		return nil, err
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeCancelled, "run cancelled before start", err)
	}
	s.stateMu.Lock()
	program := s.program
	states := s.states
	store := s.store
	committedCount := 0
	for _, state := range states {
		switch state.Current() {
		case StateProcessing, StateFailed:
			if err := state.Transition(StatePending); err != nil {
				s.stateMu.Unlock()
				return nil, err
			}
		case StateCommitted:
			committedCount++
		}
	}
	s.stateMu.Unlock()
	if committedCount == len(states) {
		return nil, apperr.New(apperr.CodeState, "all segments are committed; reset to run again").
			WithField("state").WithActual(StateCommitted)
	}
	if committedCount > 0 && store == nil {
		return nil, apperr.New(apperr.CodeState, "retrying a partially committed session requires a checkpoint store").
			WithField("state").WithActual(StateCommitted)
	}

	for i, segment := range program.Segments {
		required := requiredBytes(segment.Frames(), segment.Channels)
		if required > options.PerWorkerBufferBytes {
			return nil, apperr.New(apperr.CodeParameter, "segment exceeds per-worker buffer limit").
				WithField("per_worker_buffer_bytes").
				WithExpected(required).WithActual(options.PerWorkerBufferBytes).
				WithSegment(i, segment.ID)
		}
	}

	result := &Result{
		ProjectID:            program.ProjectID,
		SourceVersion:        program.SourceVersion,
		ParametersHash:       program.DecisionParamsHash(),
		AlgorithmVersion:     checkpoint.AlgorithmVersion,
		Mode:                 options.Mode,
		MaxParallel:          options.MaxParallel,
		PerWorkerBufferBytes: options.PerWorkerBufferBytes,
		TargetLUFS:           program.TargetLUFS,
		Segments:             make([]*SegmentResult, len(program.Segments)),
		Recovery: RecoveryInfo{
			Requested: options.Mode == ModeResume,
		},
	}
	for i, segment := range program.Segments {
		result.Segments[i] = &SegmentResult{
			Index: i, ID: segment.ID, State: states[i].Current(),
			Channels: segment.Channels, Frames: segment.Frames(),
			SourceHash: segment.ContentSHA256,
		}
	}

	var snapshot *checkpoint.Snapshot
	if options.Mode == ModeResume || committedCount > 0 {
		loaded, reason := s.loadSnapshot(program, store)
		if loaded != nil {
			snapshot = loaded
			result.Recovery.CheckpointFound = true
			result.Recovery.CheckpointUsed = true
		} else if reason != "" {
			result.Recovery.Reason = reason
			result.Recovery.Invalidated = store != nil
			if store != nil {
				_ = store.Invalidate()
			}
		}
		if committedCount > 0 && snapshot == nil {
			err := apperr.New(apperr.CodeCheckpoint, "partial committed session has no valid checkpoint to continue").
				WithField("checkpoint").WithActual(result.Recovery.Reason)
			result.FirstError = err
			s.setLast(result)
			return result, err
		}
	}

	semaphore := newWeightedSemaphore(int64(options.MaxParallel) * options.PerWorkerBufferBytes)
	preps := s.runPreparation(ctx, program, states, options, semaphore)
	result.BufferPeakBytes = semaphore.Peak()
	firstError := (*apperr.Error)(nil)
	allPrepared := true
	for i, prep := range preps {
		segment := result.Segments[i]
		if prep.err != nil {
			segment.Error = prep.err
			segment.State = states[i].Current()
			if firstError == nil {
				firstError = prep.err
			}
			allPrepared = false
			continue
		}
		segment.SourceMeasurement = prep.source
		segment.CompressedMeasurement = prep.compressed
		segment.Compressed = prep.signal
		segment.compressorGainDB = prep.gainDB
		segment.CompressionMaxReductionDB = prep.maxReduction
		if math.IsInf(prep.source.IntegratedLUFS, -1) {
			segment.SegmentIdealGainDB = 0
		} else {
			segment.SegmentIdealGainDB = program.TargetLUFS - prep.source.IntegratedLUFS
		}
		segment.State = states[i].Current()
	}
	if !allPrepared {
		result.FirstError = firstError
		s.setLast(result)
		return result, firstError
	}

	sourceParts := make([]dsp.Measurement, len(preps))
	compressedParts := make([]dsp.Measurement, len(preps))
	for i, prep := range preps {
		sourceParts[i] = prep.source
		compressedParts[i] = prep.compressed
	}
	sourceProgram, err := dsp.CombineMeasurements(sourceParts, program.Gate)
	if err != nil {
		return result, err
	}
	compressedProgram, err := dsp.CombineMeasurements(compressedParts, program.Gate)
	if err != nil {
		return result, err
	}
	result.SourceIntegratedLUFS = sourceProgram.IntegratedLUFS
	result.CompressedIntegratedLUFS = compressedProgram.IntegratedLUFS
	result.ProgramTruePeakDBTP = sourceProgram.TruePeakDBTP
	if math.IsInf(compressedProgram.IntegratedLUFS, -1) {
		err := apperr.New(apperr.CodeParameter, "program contains no frames above the absolute gate").
			WithField("program_loudness_lufs")
		result.FirstError = err
		s.setLast(result)
		return result, err
	}
	initialGain := program.TargetLUFS - compressedProgram.IntegratedLUFS
	if math.IsNaN(initialGain) || math.IsInf(initialGain, 0) {
		err := apperr.New(apperr.CodeParameter, "normalization gain is not finite").
			WithField("target_lufs").WithActual(program.TargetLUFS)
		result.FirstError = err
		s.setLast(result)
		return result, err
	}
	if err := ctx.Err(); err != nil {
		cancelErr := apperr.Wrap(apperr.CodeCancelled, "run cancelled before normalization", err)
		result.FirstError = cancelErr
		s.setLast(result)
		return result, cancelErr
	}
	compressedSignals := make([]audio.Signal, len(preps))
	for i, prep := range preps {
		compressedSignals[i] = prep.signal
	}
	normalization, normalizeErr := dsp.NormalizeProgram(
		compressedSignals, initialGain, program.MeasureConfig(), program.Limiter, program.TargetLUFS,
	)
	if normalizeErr != nil {
		if e, ok := apperr.As(normalizeErr); ok && e.SegmentIndex != nil {
			_ = states[*e.SegmentIndex].Transition(StateFailed)
			result.Segments[*e.SegmentIndex].Error = e
			result.Segments[*e.SegmentIndex].State = states[*e.SegmentIndex].Current()
		}
		result.FirstError = asAppError(normalizeErr, -1, "")
		s.setLast(result)
		return result, normalizeErr
	}
	globalGain := normalization.GainDB
	result.GlobalGainDB = globalGain
	result.FinalProgramLoudnessLUFS = normalization.Program.IntegratedLUFS
	result.ProgramTruePeakDBTP = normalization.Program.TruePeakDBTP
	result.NormalizationIterations = normalization.Iterations
	result.NormalizationConverged = normalization.Converged

	if snapshot != nil {
		if reason := validateSnapshot(snapshot, program, compressedProgram.IntegratedLUFS, globalGain); reason != "" {
			result.Recovery.CheckpointUsed = false
			result.Recovery.Invalidated = true
			result.Recovery.Reason = reason
			if store != nil {
				_ = store.Invalidate()
			}
			snapshot = nil
		}
	}

	allFinalized := true
	for i, segment := range result.Segments {
		normalized := normalization.Segments[i]
		segment.Output = normalized.Output
		segment.OutputMeasurement = normalized.Measurement
		segment.OutputFrames = normalized.Output.Frames()
		segment.OutputChannels = normalized.Output.Channels
		segment.NormalizationGainDB = globalGain
		segment.LimiterSafetyTrimDB = normalized.SafetyTrimDB
		segment.OutputHash = audio.ContentHash(normalized.Output)
		segment.CompressionGainCurveDB = downsampleDB(segment.compressorGainDB, 512)
	}

	baseRecords := map[int]checkpoint.Record{}
	if snapshot != nil {
		for _, record := range snapshot.Records {
			baseRecords[record.Index] = record
		}
		for i, segment := range result.Segments {
			record, ok := baseRecords[i]
			if !ok {
				continue
			}
			if segment.Error != nil || segment.OutputHash != record.OutputHash {
				result.Recovery.CheckpointUsed = false
				result.Recovery.Invalidated = true
				result.Recovery.Reason = fmt.Sprintf("output hash mismatch at segment %d", i)
				baseRecords = map[int]checkpoint.Record{}
				snapshot = nil
				if store != nil {
					_ = store.Invalidate()
				}
				break
			}
		}
	}

	snapshotState := checkpoint.Snapshot{
		Schema:              checkpoint.Schema,
		AlgorithmVersion:    checkpoint.AlgorithmVersion,
		ProjectID:           program.ProjectID,
		SourceVersion:       program.SourceVersion,
		ParametersHash:      program.DecisionParamsHash(),
		TargetLUFS:          program.TargetLUFS,
		ProgramLoudnessLUFS: compressedProgram.IntegratedLUFS,
		ProgramTruePeakDBTP: compressedProgram.TruePeakDBTP,
		GlobalGainDB:        globalGain,
	}
	for _, record := range baseRecords {
		snapshotState.Records = append(snapshotState.Records, record)
	}

	for i, segment := range result.Segments {
		if segment.Error != nil {
			segment.State = states[i].Current()
			continue
		}
		if states[i].Current() == StateCommitted {
			record, ok := baseRecords[i]
			if !ok || record.OutputHash != segment.OutputHash {
				err := apperr.New(apperr.CodeState, "committed segment output changed during retry").
					WithField("output_hash").WithSegment(i, segment.ID).
					WithExpected(record.OutputHash).WithActual(segment.OutputHash)
				segment.Error = err
				segment.State = states[i].Current()
				if firstError == nil {
					firstError = err
				}
				allFinalized = false
				continue
			}
			segment.FromCheckpoint = true
			result.Recovery.ReusedSegments++
			segment.State = states[i].Current()
			continue
		}
		record, reused := baseRecords[i]
		if !reused {
			record = checkpoint.Record{
				Index:                     i,
				SegmentID:                 segment.ID,
				State:                     string(StateCommitted),
				SourceVersion:             program.SourceVersion,
				ParametersHash:            program.DecisionParamsHash(),
				AlgorithmVersion:          checkpoint.AlgorithmVersion,
				InputHash:                 segment.SourceHash,
				OutputHash:                segment.OutputHash,
				NormalizationGainDB:       globalGain,
				SegmentIdealGainDB:        segment.SegmentIdealGainDB,
				CompressionMaxReductionDB: segment.CompressionMaxReductionDB,
				LimiterSafetyTrimDB:       segment.LimiterSafetyTrimDB,
				SourceMeasurement:         segment.SourceMeasurement,
				CompressedMeasurement:     segment.CompressedMeasurement,
				OutputMeasurement:         segment.OutputMeasurement,
				OutputFrames:              segment.OutputFrames,
				OutputChannels:            segment.OutputChannels,
			}
			baseRecords[i] = record
		} else {
			segment.FromCheckpoint = true
			result.Recovery.ReusedSegments++
		}
		if !reused && store != nil {
			snapshotState.Records = snapshotState.Records[:0]
			for _, existing := range baseRecords {
				snapshotState.Records = append(snapshotState.Records, existing)
			}
			if err := store.Save(snapshotState); err != nil {
				segment.Error = asAppError(err, segment.Index, segment.ID)
				firstError = segment.Error
				allFinalized = false
				if tErr := states[i].Transition(StateFailed); tErr != nil {
					return result, tErr
				}
				segment.State = states[i].Current()
				continue
			}
		}
		if err := states[i].Transition(StateCommitted); err != nil {
			segment.Error = asAppError(err, segment.Index, segment.ID)
			if firstError == nil {
				firstError = segment.Error
			}
			allFinalized = false
			segment.State = states[i].Current()
			continue
		}
		segment.State = states[i].Current()
	}

	if allFinalized {
		mixSegments := make([]audio.Signal, len(result.Segments))
		for i, segment := range result.Segments {
			mixSegments[i] = segment.Output
		}
		mix, err := audio.Concat(mixSegments)
		if err != nil {
			result.FirstError = asAppError(err, -1, "")
			s.setLast(result)
			return result, err
		}
		result.Mix = mix
		result.MixFrames = mix.Frames()
		result.MixChannels = mix.Channels
		result.MixHash = audio.ContentHash(mix)
		cursor := int64(0)
		for i, segment := range result.Segments {
			result.Offsets = append(result.Offsets, MixSpan{
				Index: i, ID: segment.ID, StartFrame: cursor,
				FrameCount: int64(segment.Frames), Channels: segment.Channels,
			})
			cursor += int64(segment.Frames)
		}
	}
	result.FirstError = firstError
	s.setLast(result)
	if firstError != nil {
		return result, firstError
	}
	return result, nil
}

func (s *Session) runPreparation(ctx context.Context, program *project.Program, states []*stateMachine, options RunOptions, semaphore *weightedSemaphore) []prepResult {
	results := make([]prepResult, len(program.Segments))
	if options.MaxParallel <= 1 {
		for i := range program.Segments {
			results[i] = prepareOne(ctx, program, states[i], options, semaphore)
		}
		return results
	}
	jobChannel := make(chan int, options.MaxParallel*2)
	resultChannel := make(chan prepResult, options.MaxParallel)
	var workers sync.WaitGroup
	for worker := 0; worker < options.MaxParallel; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobChannel {
				resultChannel <- prepareOne(ctx, program, states[index], options, semaphore)
			}
		}()
	}
	go func() {
		defer close(jobChannel)
		for i := range program.Segments {
			select {
			case <-ctx.Done():
				return
			case jobChannel <- i:
			}
		}
	}()
	go func() {
		workers.Wait()
		close(resultChannel)
	}()
	seen := make([]bool, len(program.Segments))
	for prepared := range resultChannel {
		results[prepared.index] = prepared
		seen[prepared.index] = true
	}
	for i := range results {
		if !seen[i] {
			cause := ctx.Err()
			if cause == nil {
				cause = errors.New("job was not scheduled")
			}
			results[i] = prepResult{
				index: i,
				err: apperr.Wrap(apperr.CodeCancelled, "segment preparation was cancelled", cause).
					WithSegment(i, program.Segments[i].ID),
			}
		}
	}
	return results
}

func prepareOne(ctx context.Context, program *project.Program, state *stateMachine, options RunOptions, semaphore *weightedSemaphore) prepResult {
	index := state.index
	id := state.id
	wasCommitted := state.Current() == StateCommitted
	segment := program.Segments[index]
	required := requiredBytes(segment.Frames(), segment.Channels)
	if required > options.PerWorkerBufferBytes {
		err := apperr.New(apperr.CodeParameter, "segment exceeds per-worker buffer limit").
			WithField("per_worker_buffer_bytes").WithExpected(required).WithActual(options.PerWorkerBufferBytes).
			WithSegment(index, id)
		return prepResult{index: index, err: err}
	}
	if err := semaphore.Acquire(ctx, required); err != nil {
		e := apperr.Wrap(apperr.CodeCancelled, "blocked waiting for worker buffer", err).WithSegment(index, id)
		return prepResult{index: index, err: e}
	}
	defer semaphore.Release(required)
	if !wasCommitted {
		if err := state.Transition(StateProcessing); err != nil {
			return prepResult{index: index, err: err.(*apperr.Error)}
		}
	}
	if err := ctx.Err(); err != nil {
		if !wasCommitted {
			_ = state.Transition(StateFailed)
		}
		return prepResult{index: index, err: apperr.Wrap(apperr.CodeCancelled, "segment preparation cancelled", err).WithSegment(index, id)}
	}
	sourceMeasurement, err := dsp.Measure(segment.Source, program.MeasureConfig())
	if err != nil {
		if !wasCommitted {
			_ = state.Transition(StateFailed)
		}
		return prepResult{index: index, err: enrichSegmentError(err, index, id)}
	}
	compressed, err := dsp.Compress(segment.Source, program.Compressor)
	if err != nil {
		if !wasCommitted {
			_ = state.Transition(StateFailed)
		}
		return prepResult{index: index, err: enrichSegmentError(err, index, id)}
	}
	compressedMeasurement, err := dsp.Measure(compressed.Signal, program.MeasureConfig())
	if err != nil {
		if !wasCommitted {
			_ = state.Transition(StateFailed)
		}
		return prepResult{index: index, err: enrichSegmentError(err, index, id)}
	}
	return prepResult{
		index: index, source: sourceMeasurement, compressed: compressedMeasurement,
		signal: compressed.Signal, gainDB: compressed.GainDB, maxReduction: compressed.MaxReductionDB,
	}
}

func enrichSegmentError(err error, index int, id string) *apperr.Error {
	if e, ok := apperr.As(err); ok {
		e.WithSegment(index, id)
		return e
	}
	return apperr.Wrap(apperr.CodeInternal, err.Error(), err).WithSegment(index, id)
}

func asAppError(err error, index int, id string) *apperr.Error {
	if e, ok := apperr.As(err); ok {
		if index >= 0 {
			e.WithSegment(index, id)
		}
		return e
	}
	e := apperr.Wrap(apperr.CodeInternal, err.Error(), err)
	if index >= 0 {
		e.WithSegment(index, id)
	}
	return e
}

func downsampleDB(values []float64, limit int) []float64 {
	if len(values) == 0 || limit <= 0 {
		return nil
	}
	if len(values) <= limit {
		out := make([]float64, len(values))
		for i, value := range values {
			out[i] = dsp.LinearToDB(dsp.DBToLinear(value))
		}
		return out
	}
	out := make([]float64, limit)
	for i := 0; i < limit; i++ {
		index := int(int64(i) * int64(len(values)-1) / int64(limit-1))
		out[i] = dsp.LinearToDB(dsp.DBToLinear(values[index]))
	}
	return out
}

func (s *Session) loadSnapshot(program *project.Program, store *checkpoint.Store) (*checkpoint.Snapshot, string) {
	if store == nil {
		return nil, "resume requested without a checkpoint store"
	}
	snapshot, err := store.Load()
	if errors.Is(err, checkpoint.ErrNoSnapshot) {
		return nil, "no checkpoint found"
	}
	if err != nil {
		return nil, "checkpoint load failed: " + err.Error()
	}
	if snapshot.ProjectID != program.ProjectID ||
		snapshot.SourceVersion != program.SourceVersion ||
		snapshot.ParametersHash != program.DecisionParamsHash() ||
		snapshot.AlgorithmVersion != checkpoint.AlgorithmVersion {
		return nil, "checkpoint project/parameter/algorithm version mismatch"
	}
	for _, record := range snapshot.Records {
		if record.Index < 0 || record.Index >= len(program.Segments) {
			return nil, "checkpoint segment index is out of range"
		}
		segment := program.Segments[record.Index]
		if record.SegmentID != segment.ID || record.InputHash != segment.ContentSHA256 {
			return nil, fmt.Sprintf("checkpoint content hash mismatch at segment %d", record.Index)
		}
		if record.SourceVersion != program.SourceVersion ||
			record.ParametersHash != program.DecisionParamsHash() ||
			record.AlgorithmVersion != checkpoint.AlgorithmVersion {
			return nil, fmt.Sprintf("checkpoint version mismatch at segment %d", record.Index)
		}
		if record.OutputFrames != segment.Frames() {
			return nil, fmt.Sprintf("checkpoint frame count mismatch at segment %d", record.Index)
		}
	}
	return snapshot, ""
}

func validateSnapshot(snapshot *checkpoint.Snapshot, program *project.Program, programLoudness, globalGain float64) string {
	if snapshot.ProjectID != program.ProjectID || snapshot.SourceVersion != program.SourceVersion {
		return "checkpoint project or source version mismatch"
	}
	if snapshot.ParametersHash != program.DecisionParamsHash() {
		return "checkpoint parameter hash mismatch"
	}
	if snapshot.AlgorithmVersion != checkpoint.AlgorithmVersion {
		return "checkpoint algorithm version mismatch"
	}
	if math.Abs(snapshot.ProgramLoudnessLUFS-programLoudness) > 1e-9 {
		return "checkpoint program loudness mismatch"
	}
	if math.Abs(snapshot.GlobalGainDB-globalGain) > 1e-9 {
		return "checkpoint normalization gain mismatch"
	}
	seen := map[int]bool{}
	for _, record := range snapshot.Records {
		if seen[record.Index] {
			return fmt.Sprintf("duplicate checkpoint record for segment %d", record.Index)
		}
		seen[record.Index] = true
		if record.Index < 0 || record.Index >= len(program.Segments) {
			return "checkpoint segment index out of range"
		}
		segment := program.Segments[record.Index]
		if record.SegmentID != segment.ID || record.InputHash != segment.ContentSHA256 {
			return fmt.Sprintf("checkpoint input hash mismatch at segment %d", record.Index)
		}
		if math.Abs(record.NormalizationGainDB-globalGain) > 1e-9 {
			return fmt.Sprintf("checkpoint gain decision mismatch at segment %d", record.Index)
		}
		if record.OutputFrames != segment.Frames() || record.OutputChannels != segment.Channels {
			return fmt.Sprintf("checkpoint output shape mismatch at segment %d", record.Index)
		}
	}
	return ""
}

func (s *Session) setLast(result *Result) {
	s.stateMu.Lock()
	s.last = result
	s.stateMu.Unlock()
}
