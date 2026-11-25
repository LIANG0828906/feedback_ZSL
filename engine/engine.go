package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"lipsync/arbitration"
	"lipsync/checkpoint"
	"lipsync/domain"
	"lipsync/parallel"
)

const AlgorithmVersion = "lipsync-window-arbitration-v1"

type Options struct {
	WindowSize      int
	MaxWindows      int
	MaxWorkers      int
	QueueBudget     int
	CandidateBuffer int
	Fault           func(channelID string, frame int64) error
	DisableParallel bool
}

func DefaultOptions() Options {
	return Options{
		WindowSize: 4, MaxWorkers: 4, QueueBudget: 8, CandidateBuffer: 3,
	}
}

type WindowTrace struct {
	Index          int    `json:"index"`
	StartFrame     int64  `json:"start_frame"`
	EndFrame       int64  `json:"end_frame"`
	State          string `json:"state"`
	CommitSequence int    `json:"commit_sequence,omitempty"`
	FrameCount     int    `json:"frame_count"`
	Preemptions    int    `json:"preemptions"`
}

type ChannelFailureTrace struct {
	ChannelID string      `json:"channel_id"`
	Frame     int64       `json:"frame"`
	Window    int         `json:"window_index"`
	Code      domain.Code `json:"code,omitempty"`
	Message   string      `json:"message"`
	Retried   bool        `json:"retried"`
	Recovered bool        `json:"recovered"`
}

type RunReport struct {
	Mode             string                `json:"mode"`
	Complete         bool                  `json:"complete"`
	ContentHash      string                `json:"content_hash"`
	ParamsVersion    string                `json:"params_version"`
	AlgorithmVersion string                `json:"algorithm_version"`
	Frames           []domain.FrameResult  `json:"frames"`
	Preemptions      []domain.PreemptEvent `json:"preemptions"`
	Resumes          []domain.ResumeEvent  `json:"resumes"`
	Abandonments     []domain.AbandonEvent `json:"abandonments"`
	PacketStates     []domain.PacketState  `json:"packet_states"`
	Consumed         map[string]int64      `json:"consumed_dialogue_frames"`
	Windows          []WindowTrace         `json:"windows"`
	CommitSequence   []int                 `json:"commit_sequence"`
	ChannelFailures  []ChannelFailureTrace `json:"channel_failures"`
	FailedChannels   []string              `json:"failed_channels"`
	ParallelStats    parallel.Stats        `json:"parallel_stats"`
	WaitEvents       []parallel.WaitEvent  `json:"wait_events"`
}

type Engine struct {
	project        domain.Project
	store          *checkpoint.Store
	options        Options
	runtime        *arbitration.Runtime
	pool           *parallel.Pool
	params         string
	consumed       map[string]int64
	frames         []domain.FrameResult
	preemptions    []domain.PreemptEvent
	resumes        []domain.ResumeEvent
	abandonments   []domain.AbandonEvent
	commits        []int
	windows        []WindowTrace
	failures       []ChannelFailureTrace
	failedChannels map[string]bool
	packetDialogue map[string]string
	nextWindow     int
	progress       domain.WindowProgress
	hasProgress    bool
	resumed        bool
}

func New(project domain.Project, store *checkpoint.Store, options Options) (*Engine, error) {
	if store == nil {
		return nil, &domain.Error{Code: domain.CodeState, Path: "checkpoint_store", Message: "SQLite checkpoint store is required"}
	}
	if project.ContentHash == "" {
		project.ContentHash = domain.ComputeContentHash(project)
	}
	if err := arbitration.ValidateProject(project, true); err != nil {
		return nil, err
	}
	if options.WindowSize == 0 {
		options.WindowSize = DefaultOptions().WindowSize
	}
	if options.MaxWorkers == 0 {
		options.MaxWorkers = DefaultOptions().MaxWorkers
	}
	if options.QueueBudget == 0 {
		options.QueueBudget = DefaultOptions().QueueBudget
	}
	if options.CandidateBuffer == 0 {
		options.CandidateBuffer = DefaultOptions().CandidateBuffer
	}
	if options.WindowSize < 1 {
		return nil, &domain.Error{Code: domain.CodeState, Path: "options.window_size", Message: "window size must be positive"}
	}
	params := ParamsVersion(project)
	e := &Engine{
		project: project, store: store, options: options, params: params,
		consumed: map[string]int64{},
		frames:   []domain.FrameResult{}, preemptions: []domain.PreemptEvent{},
		resumes: []domain.ResumeEvent{}, abandonments: []domain.AbandonEvent{},
		commits: []int{}, windows: []WindowTrace{}, failures: []ChannelFailureTrace{},
		failedChannels: map[string]bool{}, packetDialogue: map[string]string{},
	}
	for _, packet := range project.Packets() {
		e.packetDialogue[packet.ID] = packet.DialogueID
	}
	saved, err := store.Load(project.ContentHash, AlgorithmVersion)
	if err != nil {
		return nil, err
	}
	nextFrame := int64(0)
	var states []domain.PacketState
	if saved.Has {
		if saved.ParamsVersion != params {
			return nil, &domain.Error{Code: domain.CodeState, Path: "checkpoint.params_version", Message: "checkpoint arbitration parameters changed"}
		}
		nextFrame = saved.NextFrame
		states = saved.PacketStates
		e.frames = append(e.frames, saved.Frames...)
		e.preemptions = append(e.preemptions, saved.Preemptions...)
		e.resumes = append(e.resumes, saved.Resumes...)
		e.abandonments = append(e.abandonments, saved.Abandonments...)
		e.commits = append(e.commits, saved.CommitSequence...)
		e.consumed = saved.Consumed
		e.nextWindow = saved.LastWindow + 1
		e.resumed = true
		for _, window := range saved.Windows {
			e.windows = append(e.windows, WindowTrace{
				Index: window.Index, StartFrame: window.StartFrame, EndFrame: window.EndFrame,
				State: string(domain.WindowCommitted), CommitSequence: window.CommitSequence,
				FrameCount: int(window.EndFrame - window.StartFrame + 1), Preemptions: window.Preemptions,
			})
		}
	}
	runtime, err := arbitration.NewRuntime(project, states, nextFrame)
	if err != nil {
		return nil, err
	}
	e.runtime = runtime
	if !options.DisableParallel {
		pool, err := parallel.New(parallel.Config{
			MaxWorkers: options.MaxWorkers, QueueBudget: options.QueueBudget,
			CandidateBuffer: options.CandidateBuffer,
		})
		if err != nil {
			return nil, err
		}
		e.pool = pool
	}
	return e, nil
}

func ParamsVersion(project domain.Project) string {
	type params struct {
		Channels []struct {
			ID     string `json:"id"`
			Slots  int    `json:"slots"`
			Budget int64  `json:"budget"`
		} `json:"channels"`
		Order     []string              `json:"order"`
		Recovery  domain.RecoveryPolicy `json:"recovery"`
		FrameRate int64                 `json:"frame_rate"`
	}
	input := params{Order: project.ArbitrationOrder, Recovery: project.Recovery, FrameRate: project.FrameRate}
	channels := append([]domain.Channel(nil), project.Channels...)
	sort.Slice(channels, func(i, j int) bool { return channels[i].ID < channels[j].ID })
	for _, channel := range channels {
		input.Channels = append(input.Channels, struct {
			ID     string `json:"id"`
			Slots  int    `json:"slots"`
			Budget int64  `json:"budget"`
		}{ID: channel.ID, Slots: channel.SlotLimit, Budget: channel.FrameBudgetBasisPoints})
	}
	data, _ := json.Marshal(input)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Run advances until complete or MaxWindows new windows have committed. A
// failed window restores the exact pre-window state; calling Run again is the
// explicit retry path.
func (e *Engine) Run(ctx context.Context) (*RunReport, error) {
	if e.hasProgress && e.progress.State == domain.WindowFailed {
		if err := e.progress.Transition(domain.WindowPending); err != nil {
			return e.report(false), err
		}
	}
	windowsRun := 0
	for !e.runtime.AllResolved() {
		if e.options.MaxWindows > 0 && windowsRun >= e.options.MaxWindows {
			break
		}
		select {
		case <-ctx.Done():
			return e.report(false), ctx.Err()
		default:
		}
		start := e.runtime.NextFrame()
		end := start + int64(e.options.WindowSize) - 1
		windowStartStates := e.runtime.Snapshot()
		windowStartConsumed := cloneConsumed(e.consumed)
		progress := domain.NewWindowProgress(e.nextWindow, start, end)
		if err := progress.Transition(domain.WindowArbitrating); err != nil {
			return e.report(false), err
		}
		e.progress, e.hasProgress = progress, true
		var windowFrames []domain.FrameResult
		var windowPreemptions []domain.PreemptEvent
		var windowResumes []domain.ResumeEvent
		var windowAbandonments []domain.AbandonEvent
		for frame := start; frame <= end; frame++ {
			step, err := e.advanceFrame(frame)
			if err != nil {
				channelID := ""
				var channelFailure *arbitration.ChannelFailure
				if errors.As(err, &channelFailure) {
					channelID = channelFailure.ChannelID
					e.failedChannels[channelID] = true
				}
				e.failures = append(e.failures, ChannelFailureTrace{
					ChannelID: channelID, Frame: frame, Window: e.nextWindow,
					Code: domain.CodeOf(err), Message: err.Error(),
				})
				_ = e.runtime.RestoreAt(windowStartStates, start)
				e.consumed = windowStartConsumed
				_ = e.progress.Transition(domain.WindowFailed)
				return e.report(false), err
			}
			windowFrames = append(windowFrames, step.Result)
			windowPreemptions = append(windowPreemptions, step.Preemptions...)
			windowResumes = append(windowResumes, step.Resumes...)
			windowAbandonments = append(windowAbandonments, step.Abandonments...)
			e.consume(step)
		}
		if len(windowPreemptions) > 0 {
			if err := e.progress.Transition(domain.WindowPreempting); err != nil {
				return e.report(false), err
			}
		}
		commit := checkpoint.WindowCommit{
			Index: e.nextWindow, StartFrame: start, EndFrame: start + int64(len(windowFrames)) - 1,
			State: string(domain.WindowCommitted), ContentHash: e.project.ContentHash,
			ParamsVersion: e.params, AlgorithmVersion: AlgorithmVersion,
			Frames: windowFrames, Preemptions: windowPreemptions, Resumes: windowResumes,
			Abandonments: windowAbandonments, PacketStates: e.runtime.Snapshot(),
			Consumed: cloneConsumed(e.consumed),
		}
		sequence, err := e.store.SaveWindow(commit)
		if err != nil {
			_ = e.runtime.RestoreAt(windowStartStates, start)
			e.consumed = windowStartConsumed
			_ = e.progress.Transition(domain.WindowFailed)
			return e.report(false), err
		}
		if err := e.progress.Transition(domain.WindowCommitted); err != nil {
			return e.report(false), err
		}
		e.frames = append(e.frames, windowFrames...)
		e.preemptions = append(e.preemptions, windowPreemptions...)
		e.resumes = append(e.resumes, windowResumes...)
		e.abandonments = append(e.abandonments, windowAbandonments...)
		e.commits = append(e.commits, sequence)
		e.windows = append(e.windows, WindowTrace{
			Index: e.nextWindow, StartFrame: start, EndFrame: commit.EndFrame,
			State: string(domain.WindowCommitted), CommitSequence: sequence,
			FrameCount: len(windowFrames), Preemptions: len(windowPreemptions),
		})
		for index := range e.failures {
			if e.failures[index].Window == e.nextWindow && !e.failures[index].Recovered {
				e.failures[index].Retried = true
				e.failures[index].Recovered = true
			}
		}
		e.failedChannels = map[string]bool{}
		e.nextWindow++
		windowsRun++
	}
	return e.report(e.runtime.AllResolved()), nil
}

func (e *Engine) advanceFrame(frame int64) (arbitration.FrameStep, error) {
	if e.pool == nil {
		return e.runtime.Advance(frame)
	}
	return e.runtime.AdvanceParallel(frame, e.pool, e.options.Fault)
}

func (e *Engine) consume(step arbitration.FrameStep) {
	for _, channel := range step.Result.Channels {
		for _, packetID := range channel.Accepted {
			e.consumed[e.packetDialogue[packetID]]++
		}
	}
}

func (e *Engine) report(complete bool) *RunReport {
	stats := parallel.Stats{}
	var waits []parallel.WaitEvent
	if e.pool != nil {
		stats = e.pool.SnapshotStats()
		waits = e.pool.WaitEvents()
	}
	mode := "live"
	if e.resumed {
		mode = "resumed"
	}
	failedChannels := make([]string, 0, len(e.failedChannels))
	for channelID := range e.failedChannels {
		failedChannels = append(failedChannels, channelID)
	}
	sort.Strings(failedChannels)
	return &RunReport{
		Mode: mode, Complete: complete, ContentHash: e.project.ContentHash,
		ParamsVersion: e.params, AlgorithmVersion: AlgorithmVersion,
		Frames: cloneFrames(e.frames), Preemptions: append([]domain.PreemptEvent(nil), e.preemptions...),
		Resumes:      append([]domain.ResumeEvent(nil), e.resumes...),
		Abandonments: append([]domain.AbandonEvent(nil), e.abandonments...),
		PacketStates: e.runtime.Snapshot(), Consumed: cloneConsumed(e.consumed),
		Windows:         append([]WindowTrace(nil), e.windows...),
		CommitSequence:  append([]int(nil), e.commits...),
		ChannelFailures: append([]ChannelFailureTrace(nil), e.failures...),
		FailedChannels:  failedChannels,
		ParallelStats:   stats, WaitEvents: waits,
	}
}

func (e *Engine) Progress() (domain.WindowProgress, bool) {
	return e.progress, e.hasProgress
}

func (e *Engine) Close() {
	if e.pool != nil {
		e.pool.Close()
		e.pool = nil
	}
}

func cloneConsumed(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneFrames(in []domain.FrameResult) []domain.FrameResult {
	out := make([]domain.FrameResult, len(in))
	for i, frame := range in {
		out[i] = domain.FrameResult{Frame: frame.Frame, Channels: make([]domain.FrameChannelResult, len(frame.Channels))}
		for j, channel := range frame.Channels {
			out[i].Channels[j] = channel
			out[i].Channels[j].Accepted = cloneStrings(channel.Accepted)
			out[i].Channels[j].Delayed = cloneStrings(channel.Delayed)
			out[i].Channels[j].Preempted = cloneStrings(channel.Preempted)
			out[i].Channels[j].Resumed = cloneStrings(channel.Resumed)
		}
	}
	return out
}

func cloneStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func (e *Engine) String() string {
	return fmt.Sprintf("engine{%s window=%d next=%d}", e.project.Name, e.nextWindow, e.runtime.NextFrame())
}
