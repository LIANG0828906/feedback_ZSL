// Package engine orchestrates synthesis, packetization, state transitions,
// persistence, recovery, replay, and path comparison.
package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"audioforge/internal/deps"
	"audioforge/internal/errs"
	"audioforge/internal/machine"
	"audioforge/internal/model"
	"audioforge/internal/packet"
	"audioforge/internal/store"
	"audioforge/internal/synth"
)

type Result struct {
	Events           []model.Event            `json:"events"`
	Frames           []model.EnvelopeFrame    `json:"frames"`
	Packets          []model.Packet           `json:"packets"`
	Commits          []model.CommitRecord     `json:"commits"`
	Decisions        []model.PacketDecision   `json:"decisions"`
	States           map[string]machine.State `json:"states"`
	Graph            *deps.Graph              `json:"dependency_graph"`
	Workers          int                      `json:"workers"`
	Buffer           int                      `json:"buffer"`
	ParamVersion     string                   `json:"param_version"`
	AlgorithmVersion string                   `json:"algorithm_version"`
	ResumedWindows   int                      `json:"resumed_windows"`
}

type Engine struct {
	Store *store.Store
}

type RunOptions struct {
	Persist           bool
	ForceAfterWindows int
}

type plan struct {
	result   *Result
	machines *machine.Manager
}

func (e *Engine) Build(events []model.Event, capability model.Capability, params model.BuildParameters) (*Result, error) {
	built, err := buildPlan(events, capability, params)
	if err != nil {
		return nil, err
	}
	return built.result, nil
}

func buildPlan(events []model.Event, capability model.Capability, params model.BuildParameters) (*plan, error) {
	if err := capability.Validate(); err != nil {
		return nil, errs.New(errs.EPack, "invalid capability: %v", err)
	}
	if err := params.Validate(); err != nil {
		return nil, errs.New(errs.EVersion, "invalid build parameters: %v", err)
	}
	synthesis, err := synth.Synthesize(events, capability, params.Workers, params.BufferPerWorker)
	if err != nil {
		return nil, err
	}
	packets, err := packet.Build(synthesis.Frames, capability)
	if err != nil {
		return nil, err
	}
	manager := machine.NewManager()
	for _, p := range packets {
		packetMachine := manager.Ensure(p.Channel, p.Seq)
		if err := packetMachine.Transition(machine.Quantizing); err != nil {
			return nil, err
		}
		if err := packetMachine.Transition(machine.Sliced); err != nil {
			return nil, err
		}
	}
	commits := make([]model.CommitRecord, 0)
	for start := 0; start < len(packets); start += params.CommitWindow {
		end := start + params.CommitWindow
		if end > len(packets) {
			end = len(packets)
		}
		commits = append(commits, model.CommitRecord{
			WindowIndex:      len(commits),
			Packets:          clonePackets(packets[start:end]),
			ParamVersion:     params.ParamVersion,
			AlgorithmVersion: params.AlgorithmVersion,
		})
	}
	result := &Result{
		Events:           cloneEvents(events),
		Frames:           synthesis.Frames,
		Packets:          packets,
		Commits:          commits,
		Decisions:        synthesis.Decisions,
		States:           manager.States(),
		Graph:            deps.Build(packets, capability),
		Workers:          synthesis.Workers,
		Buffer:           synthesis.Buffer,
		ParamVersion:     params.ParamVersion,
		AlgorithmVersion: params.AlgorithmVersion,
	}
	return &plan{result: result, machines: manager}, nil
}

// Run executes the realtime path. Context cancellation is reported as
// E_CANCEL, while ForceAfterWindows simulates an ungraceful stop as
// E_TERMINATED. Only committed windows survive a forced stop.
func (e *Engine) Run(ctx context.Context, jobID string, events []model.Event, capability model.Capability, params model.BuildParameters, options RunOptions) (*Result, error) {
	if options.Persist && e.Store == nil {
		return nil, errs.New(errs.ESnapshot, "persistence requested without a store")
	}
	if err := ctx.Err(); err != nil {
		return nil, errs.Wrap(errs.ECancel, err, "run cancelled before synthesis")
	}
	built, err := buildPlan(events, capability, params)
	if err != nil {
		return nil, err
	}
	if options.Persist {
		if err := e.Store.Begin(jobID, params.ParamVersion, params.AlgorithmVersion, capability.Version); err != nil {
			return nil, err
		}
	}
	committed := make([]model.CommitRecord, 0, len(built.result.Commits))
	for index, commit := range built.result.Commits {
		if options.ForceAfterWindows > 0 && index >= options.ForceAfterWindows {
			built.result.Commits = committed
			built.result.States = built.machines.States()
			return built.result, &errs.Error{
				Code:    errs.ETerminated,
				Message: fmt.Sprintf("forced termination after %d committed windows", len(committed)),
				Field:   "window_index",
			}
		}
		if err := ctx.Err(); err != nil {
			built.result.Commits = committed
			built.result.States = built.machines.States()
			return built.result, errs.Wrap(errs.ECancel, err, "run cancelled before window %d", index)
		}
		if options.Persist {
			if err := e.Store.CommitWindow(jobID, commit.WindowIndex, commit.Packets); err != nil {
				markFailed(built.machines, commit.Packets)
				built.result.Commits = committed
				built.result.States = built.machines.States()
				return built.result, err
			}
		}
		for _, p := range commit.Packets {
			packetMachine := built.machines.Ensure(p.Channel, p.Seq)
			if err := packetMachine.Transition(machine.Committed); err != nil {
				return nil, err
			}
		}
		commit.CommittedAt = time.Now().UTC().Format(time.RFC3339Nano)
		committed = append(committed, commit)
	}
	if options.Persist {
		if err := e.Store.Finalize(jobID); err != nil {
			return nil, err
		}
	}
	built.result.Commits = committed
	built.result.States = built.machines.States()
	return built.result, nil
}

// Resume reads and validates only committed windows, then continues the
// deterministic synthesis from the external event stream.
func (e *Engine) Resume(ctx context.Context, jobID string, events []model.Event, capability model.Capability, params model.BuildParameters, forceAfterAdditional int) (*Result, error) {
	if e.Store == nil {
		return nil, errs.New(errs.ESnapshot, "resume requires a store")
	}
	if err := ctx.Err(); err != nil {
		return nil, errs.Wrap(errs.ECancel, err, "resume cancelled")
	}
	snapshot, err := e.Store.Load(jobID, params.ParamVersion, params.AlgorithmVersion)
	if err != nil {
		return nil, err
	}
	built, err := buildPlan(events, capability, params)
	if err != nil {
		return nil, err
	}
	loadedPackets := flattenSnapshot(snapshot)
	if len(loadedPackets) > len(built.result.Packets) {
		return nil, &errs.Error{
			Code:    errs.EDiff,
			Message: "snapshot contains more packets than deterministic rebuild",
			Field:   "packet_count",
		}
	}
	for i, loaded := range loadedPackets {
		if err := comparePacket(loaded, built.result.Packets[i]); err != nil {
			return nil, err
		}
	}
	manager := machine.NewManager()
	for _, p := range loadedPackets {
		packetMachine := manager.Ensure(p.Channel, p.Seq)
		for _, state := range []machine.State{machine.Quantizing, machine.Sliced, machine.Committed} {
			if err := packetMachine.Transition(state); err != nil {
				return nil, err
			}
		}
	}
	for _, p := range built.result.Packets {
		if packetMachine, ok := manager.Get(p.Channel, p.Seq); ok && packetMachine.State() == machine.Committed {
			continue
		}
		packetMachine := manager.Ensure(p.Channel, p.Seq)
		for _, state := range []machine.State{machine.Quantizing, machine.Sliced} {
			if err := packetMachine.Transition(state); err != nil {
				return nil, err
			}
		}
	}
	loadedCommits := snapshotCommits(snapshot)
	commits := append([]model.CommitRecord(nil), loadedCommits...)
	startWindow := len(snapshot.Windows)
	if startWindow > len(built.result.Commits) {
		return nil, &errs.Error{Code: errs.EDiff, Message: "snapshot has more windows than deterministic rebuild", Field: "window_count"}
	}
	added := 0
	for index := startWindow; index < len(built.result.Commits); index++ {
		if forceAfterAdditional > 0 && added >= forceAfterAdditional {
			built.result.Commits = commits
			built.result.States = manager.States()
			built.result.ResumedWindows = startWindow
			return built.result, &errs.Error{
				Code:    errs.ETerminated,
				Message: "resume forced to stop after additional committed windows",
				Field:   "window_index",
			}
		}
		if err := ctx.Err(); err != nil {
			built.result.Commits = commits
			built.result.States = manager.States()
			built.result.ResumedWindows = startWindow
			return built.result, errs.Wrap(errs.ECancel, err, "resume cancelled before window %d", index)
		}
		commit := built.result.Commits[index]
		if err := e.Store.CommitWindow(jobID, commit.WindowIndex, commit.Packets); err != nil {
			markFailed(manager, commit.Packets)
			built.result.Commits = commits
			built.result.States = manager.States()
			built.result.ResumedWindows = startWindow
			return built.result, err
		}
		for _, p := range commit.Packets {
			if err := manager.Ensure(p.Channel, p.Seq).Transition(machine.Committed); err != nil {
				return nil, err
			}
		}
		commit.CommittedAt = time.Now().UTC().Format(time.RFC3339Nano)
		commits = append(commits, commit)
		added++
	}
	if err := e.Store.Finalize(jobID); err != nil {
		return nil, err
	}
	built.result.Commits = commits
	built.result.States = manager.States()
	built.result.ResumedWindows = startWindow
	return built.result, nil
}

// Replay reconstructs frames and packets only from persisted window decisions.
func (e *Engine) Replay(jobID string, params model.BuildParameters) (*Result, error) {
	if e.Store == nil {
		return nil, errs.New(errs.ESnapshot, "replay requires a store")
	}
	snapshot, err := e.Store.Load(jobID, params.ParamVersion, params.AlgorithmVersion)
	if err != nil {
		return nil, err
	}
	packets := flattenSnapshot(snapshot)
	frames := framesFromPackets(packets)
	return &Result{
		Frames:           frames,
		Packets:          packets,
		Commits:          snapshotCommits(snapshot),
		States:           map[string]machine.State{},
		ParamVersion:     params.ParamVersion,
		AlgorithmVersion: params.AlgorithmVersion,
	}, nil
}

func Compare(pathA string, a *Result, pathB string, b *Result) error {
	maxChannel := 0
	framesA := framesByChannel(a.Frames)
	framesB := framesByChannel(b.Frames)
	for channel := range framesA {
		if channel > maxChannel {
			maxChannel = channel
		}
	}
	for channel := range framesB {
		if channel > maxChannel {
			maxChannel = channel
		}
	}
	for channel := 0; channel <= maxChannel; channel++ {
		frameA := framesA[channel]
		frameB := framesB[channel]
		limit := len(frameA)
		if len(frameB) > limit {
			limit = len(frameB)
		}
		for offset := 0; offset < limit; offset++ {
			var valueA, valueB int64
			missingA := offset >= len(frameA)
			missingB := offset >= len(frameB)
			if !missingA {
				valueA = frameA[offset]
			}
			if !missingB {
				valueB = frameB[offset]
			}
			if missingA || missingB || valueA != valueB {
				return &errs.Error{
					Code:         errs.EDiff,
					Message:      fmt.Sprintf("envelope mismatch between %s and %s", pathA, pathB),
					Channel:      channel,
					SampleOffset: int64(offset),
					PacketSeq:    packetAt(a.Packets, channel, int64(offset)),
					Field:        "envelope",
				}
			}
		}
	}
	if len(a.Packets) != len(b.Packets) {
		seq := len(a.Packets)
		channel := 0
		if len(b.Packets) < len(a.Packets) {
			seq = len(b.Packets)
		}
		if seq < len(a.Packets) {
			channel = a.Packets[seq].Channel
		} else if seq < len(b.Packets) {
			channel = b.Packets[seq].Channel
		}
		return &errs.Error{
			Code:      errs.EDiff,
			Message:   fmt.Sprintf("packet count mismatch between %s and %s", pathA, pathB),
			Channel:   channel,
			PacketSeq: seq,
			Field:     "packet_count",
		}
	}
	for i := range a.Packets {
		if err := comparePacket(a.Packets[i], b.Packets[i]); err != nil {
			coded := err.(*errs.Error)
			coded.Message = fmt.Sprintf("packet mismatch between %s and %s", pathA, pathB)
			return coded
		}
	}
	if err := compareCommitSequence(pathA, a.Commits, pathB, b.Commits); err != nil {
		return err
	}
	return nil
}

func compareCommitSequence(pathA string, a []model.CommitRecord, pathB string, b []model.CommitRecord) error {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	for i := 0; i < limit; i++ {
		keysA := commitKeys(a[i])
		keysB := commitKeys(b[i])
		if keysA != keysB {
			seq := 0
			channel := 0
			if len(a[i].Packets) > 0 {
				seq = a[i].Packets[0].Seq
				channel = a[i].Packets[0].Channel
			}
			return &errs.Error{
				Code:      errs.EDiff,
				Message:   fmt.Sprintf("commit sequence mismatch at window %d between %s and %s", i, pathA, pathB),
				Channel:   channel,
				PacketSeq: seq,
				Field:     "commit_sequence",
			}
		}
	}
	if len(a) != len(b) {
		return &errs.Error{
			Code:    errs.EDiff,
			Message: fmt.Sprintf("commit window count mismatch between %s and %s", pathA, pathB),
			Field:   "commit_count",
		}
	}
	return nil
}

func commitKeys(record model.CommitRecord) string {
	var builder strings.Builder
	for _, p := range record.Packets {
		fmt.Fprintf(&builder, "%d/%d/%d;", p.Channel, p.Seq, p.StartSample)
	}
	return builder.String()
}

func comparePacket(a, b model.Packet) error {
	if a.Channel != b.Channel || a.Seq != b.Seq || a.StartSample != b.StartSample ||
		a.TimingMark != b.TimingMark || a.PreviousOffset != b.PreviousOffset ||
		a.SampleCount != b.SampleCount || a.ContentHash != b.ContentHash {
		return &errs.Error{
			Code:         errs.EDiff,
			Message:      "packet boundary or hash differs",
			PacketSeq:    b.Seq,
			Channel:      b.Channel,
			SampleOffset: b.StartSample,
			Field:        "packet",
		}
	}
	if len(a.Samples) != len(b.Samples) {
		return &errs.Error{
			Code:         errs.EDiff,
			Message:      "packet sample length differs",
			PacketSeq:    b.Seq,
			Channel:      b.Channel,
			SampleOffset: b.StartSample,
			Field:        "samples",
		}
	}
	for i := range a.Samples {
		if a.Samples[i] != b.Samples[i] {
			return &errs.Error{
				Code:         errs.EDiff,
				Message:      "packet sample differs",
				PacketSeq:    b.Seq,
				Channel:      b.Channel,
				SampleOffset: b.StartSample + int64(i),
				Field:        "samples",
			}
		}
	}
	return nil
}

func framesByChannel(frames []model.EnvelopeFrame) map[int][]int64 {
	out := make(map[int][]int64, len(frames))
	for _, frame := range frames {
		out[frame.Channel] = append([]int64(nil), frame.Samples...)
	}
	return out
}

func framesFromPackets(packets []model.Packet) []model.EnvelopeFrame {
	byChannel := make(map[int][]int64)
	for _, p := range packets {
		byChannel[p.Channel] = append(byChannel[p.Channel], p.Samples...)
	}
	channels := make([]int, 0, len(byChannel))
	for channel := range byChannel {
		channels = append(channels, channel)
	}
	sort.Ints(channels)
	frames := make([]model.EnvelopeFrame, 0, len(channels))
	for _, channel := range channels {
		frames = append(frames, model.EnvelopeFrame{Channel: channel, Samples: byChannel[channel]})
	}
	return frames
}

func packetAt(packets []model.Packet, channel int, offset int64) int {
	for _, p := range packets {
		if p.Channel == channel && offset >= p.StartSample && offset < p.EndSample() {
			return p.Seq
		}
	}
	return 0
}

func flattenSnapshot(snapshot *store.Snapshot) []model.Packet {
	packets := make([]model.Packet, 0)
	for _, window := range snapshot.Windows {
		for _, p := range window.Packets {
			packets = append(packets, p.Clone())
		}
	}
	sort.Slice(packets, func(i, j int) bool {
		if packets[i].Channel != packets[j].Channel {
			return packets[i].Channel < packets[j].Channel
		}
		return packets[i].Seq < packets[j].Seq
	})
	return packets
}

func snapshotCommits(snapshot *store.Snapshot) []model.CommitRecord {
	commits := make([]model.CommitRecord, 0, len(snapshot.Windows))
	for _, window := range snapshot.Windows {
		commits = append(commits, model.CommitRecord{
			WindowIndex:      window.Index,
			Packets:          clonePackets(window.Packets),
			ParamVersion:     window.ParamVersion,
			AlgorithmVersion: window.AlgorithmVersion,
			CommittedAt:      window.CommittedAt,
		})
	}
	return commits
}

func markFailed(manager *machine.Manager, packets []model.Packet) {
	for _, p := range packets {
		packetMachine := manager.Ensure(p.Channel, p.Seq)
		_ = packetMachine.Transition(machine.Failed)
	}
}

func clonePackets(packets []model.Packet) []model.Packet {
	out := make([]model.Packet, len(packets))
	for i, p := range packets {
		out[i] = p.Clone()
	}
	return out
}

func cloneEvents(events []model.Event) []model.Event {
	out := make([]model.Event, len(events))
	for i, event := range events {
		out[i] = event.Clone()
	}
	return out
}
