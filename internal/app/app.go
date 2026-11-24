// Package app wires generated fixtures, branch loading, synthesis, persistence,
// three execution paths, invalidation examples, and loopback echo diagnostics.
package app

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"audioforge/internal/capability"
	"audioforge/internal/config"
	"audioforge/internal/deps"
	"audioforge/internal/device"
	"audioforge/internal/engine"
	"audioforge/internal/errs"
	"audioforge/internal/generator"
	"audioforge/internal/model"
	"audioforge/internal/semantics"
	"audioforge/internal/store"
	"audioforge/internal/verify"
)

type TraceabilitySample struct {
	EventID string            `json:"event_id"`
	Branch  string            `json:"branch"`
	Paths   map[string]string `json:"paths"`
}

type BranchInfo struct {
	Name          string             `json:"name"`
	File          string             `json:"file"`
	TimeUnit      string             `json:"time_unit"`
	ValueSemantic string             `json:"value_semantic"`
	ListSemantic  string             `json:"list_semantic"`
	Rounding      string             `json:"rounding"`
	EventCount    int                `json:"event_count"`
	Traceability  TraceabilitySample `json:"traceability"`
}

type PacketView struct {
	Seq             int      `json:"seq"`
	Channel         int      `json:"channel"`
	Start           int64    `json:"start"`
	End             int64    `json:"end"`
	TimingMark      int64    `json:"timing_mark"`
	PreviousOffset  int64    `json:"previous_offset"`
	Samples         []int64  `json:"samples"`
	ContentHash     string   `json:"content_hash"`
	DependsOnEvents []string `json:"depends_on_events"`
}

type ChannelReport struct {
	Channel      int            `json:"channel"`
	FrameSamples int            `json:"frame_samples"`
	Template     []int64        `json:"template"`
	TemplateHash string         `json:"template_hash"`
	Packets      []PacketView   `json:"packets"`
	Matches      []device.Match `json:"matches"`
	Issues       []*errs.Error  `json:"issues"`
	State        map[string]int `json:"state_counts"`
}

type CheckpointReport struct {
	Path             string             `json:"path"`
	Complete         bool               `json:"complete"`
	Windows          int                `json:"windows"`
	Packets          int                `json:"packets"`
	ParamVersion     string             `json:"param_version"`
	AlgorithmVersion string             `json:"algorithm_version"`
	CommitSequence   []CommitWindowView `json:"commit_sequence"`
}

type CommitWindowView struct {
	Index      int      `json:"index"`
	PacketKeys []string `json:"packet_keys"`
}

type InvalidationReport struct {
	Name        string             `json:"name"`
	Change      deps.Change        `json:"change"`
	Invalidated []deps.Invalidated `json:"invalidated"`
	ReusedCount int                `json:"reused_count"`
	Total       int                `json:"total_packets"`
}

type PathCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type TruthReport struct {
	Verification string `json:"verification"`
	SamplePoints int    `json:"sample_points"`
	PacketRanges int    `json:"packet_ranges"`
	Conditions   int    `json:"conditions"`
}

type Report struct {
	GeneratedAt    time.Time              `json:"generated_at"`
	Seed           int64                  `json:"seed"`
	SelectedBranch string                 `json:"selected_branch"`
	DeviceURL      string                 `json:"device_url"`
	EventCount     int                    `json:"event_count"`
	Branches       []BranchInfo           `json:"branches"`
	Conditions     map[string]bool        `json:"conditions"`
	Channels       []ChannelReport        `json:"channels"`
	CleanAlignment device.Alignment       `json:"clean_alignment"`
	FaultAlignment device.Alignment       `json:"fault_alignment"`
	FaultEchoes    []device.Echo          `json:"fault_echoes"`
	Checkpoint     CheckpointReport       `json:"checkpoint"`
	Invalidations  []InvalidationReport   `json:"invalidations"`
	PathChecks     []PathCheck            `json:"path_checks"`
	Truth          TruthReport            `json:"truth"`
	Decisions      []model.PacketDecision `json:"decisions"`
	ForcedStopCode string                 `json:"forced_stop_code"`
	Workers        int                    `json:"workers"`
	WorkerBuffer   int                    `json:"worker_buffer"`
}

type Runner struct {
	mu     sync.RWMutex
	cfg    config.Config
	report *Report
	store  *store.Store
	device *device.Service
	bundle *generator.Bundle
}

func Start(ctx context.Context, cfg config.Config) (*Runner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, errs.New(errs.EVersion, "invalid configuration: %v", err)
	}
	runner := &Runner{cfg: cfg}
	cleanup := true
	defer func() {
		if cleanup {
			runner.Close()
		}
	}()
	bundle, err := generator.Generate(cfg.Seed)
	if err != nil {
		return nil, err
	}
	runner.bundle = bundle
	if err := bundle.Write(cfg.SampleDir); err != nil {
		return nil, errs.Wrap(errs.EVersion, err, "write generated samples")
	}
	capabilityValue, err := capability.Load(filepath.Join(cfg.SampleDir, "capability.json"))
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(bundle.Streams))
	for _, stream := range bundle.Streams {
		paths = append(paths, filepath.Join(cfg.SampleDir, stream.File))
	}
	streams, err := semantics.LoadFiles(paths, capabilityValue.SampleRate, bundle.Registry)
	if err != nil {
		return nil, err
	}
	if err := semantics.CompareStreams(streams); err != nil {
		return nil, err
	}
	selected, err := selectStream(streams, cfg.SelectedBranch)
	if err != nil {
		return nil, err
	}
	if err := verify.Conditions(selected.Events, bundle.Truth); err != nil {
		return nil, err
	}
	checkpointStore, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	runner.store = checkpointStore
	params := model.BuildParameters{
		ParamVersion:     cfg.ParamVersion,
		AlgorithmVersion: cfg.AlgorithmVersion,
		CommitWindow:     cfg.CommitWindowPackets,
		Workers:          cfg.Workers,
		BufferPerWorker:  cfg.BufferPerWorker,
	}
	runnerImpl := &engine.Engine{Store: checkpointStore}
	realtime, err := runnerImpl.Run(ctx, "diagnostic-realtime", selected.Events, capabilityValue, params, engine.RunOptions{Persist: true})
	if err != nil {
		return nil, err
	}
	if err := verify.Truth(realtime, bundle.Truth, capabilityValue); err != nil {
		return nil, err
	}
	pathChecks := []PathCheck{{
		Name:   "realtime",
		OK:     true,
		Detail: fmt.Sprintf("%d packets, %d committed windows", len(realtime.Packets), len(realtime.Commits)),
	}}
	replayed, err := runnerImpl.Replay("diagnostic-realtime", params)
	if err != nil {
		return nil, err
	}
	if err := engine.Compare("realtime", realtime, "replay", replayed); err != nil {
		return nil, err
	}
	pathChecks = append(pathChecks, PathCheck{Name: "replay", OK: true, Detail: "persisted decisions reproduced all frames and packets"})

	forcedCode := ""
	resumeDetail := "forced restart test skipped (single commit window)"
	forceCount := len(realtime.Commits) / 2
	if forceCount < 1 && len(realtime.Commits) > 1 {
		forceCount = 1
	}
	if len(realtime.Commits) > 1 {
		if forceCount >= len(realtime.Commits) {
			forceCount = len(realtime.Commits) - 1
		}
		_, forceErr := runnerImpl.Run(ctx, "diagnostic-resume", selected.Events, capabilityValue, params, engine.RunOptions{
			Persist:           true,
			ForceAfterWindows: forceCount,
		})
		if !errs.IsCode(forceErr, errs.ETerminated) {
			return nil, errs.New(errs.ETerminated, "forced stop simulation returned %v", forceErr)
		}
		forcedCode = string(errs.ETerminated)
		resumed, err := runnerImpl.Resume(ctx, "diagnostic-resume", selected.Events, capabilityValue, params, 0)
		if err != nil {
			return nil, err
		}
		if err := engine.Compare("realtime", realtime, "resume", resumed); err != nil {
			return nil, err
		}
		resumeDetail = fmt.Sprintf("resumed after %d committed windows; remaining windows recomputed deterministically", forceCount)
	}
	pathChecks = append(pathChecks, PathCheck{Name: "resume", OK: true, Detail: resumeDetail})

	deviceService, err := device.NewService(capabilityValue)
	if err != nil {
		return nil, err
	}
	runner.device = deviceService
	deviceURL, err := deviceService.Start(cfg.DevicePort)
	if err != nil {
		return nil, err
	}
	client := device.NewClient(deviceURL)
	if err := client.Reset(ctx); err != nil {
		return nil, err
	}
	timeout := time.Duration(cfg.DeviceTimeoutMS) * time.Millisecond
	cleanExchange, err := client.Send(ctx, realtime.Packets)
	if err != nil {
		return nil, err
	}
	cleanAlignment := device.Align(cleanExchange, timeout, cleanExchange.CompletedAt)
	if len(cleanAlignment.Issues) != 0 {
		return nil, errs.New(errs.EDiff, "clean loopback alignment produced %d anomalies", len(cleanAlignment.Issues))
	}
	if len(cleanAlignment.Matches) != len(realtime.Packets) {
		return nil, errs.New(errs.EPack, "clean loopback aligned %d of %d packets", len(cleanAlignment.Matches), len(realtime.Packets))
	}

	faults := demoFaults(realtime.Packets, cfg.DeviceTimeoutMS)
	if err := client.Reset(ctx); err != nil {
		return nil, err
	}
	if err := client.SetFaults(ctx, faults); err != nil {
		return nil, err
	}
	faultExchange, err := client.Send(ctx, realtime.Packets)
	if err != nil {
		return nil, err
	}
	faultAlignment := device.Align(faultExchange, timeout, faultExchange.CompletedAt.Add(timeout))
	faultEchoes := deviceService.Echoes()

	snapshot, err := checkpointStore.Load("diagnostic-realtime", params.ParamVersion, params.AlgorithmVersion)
	if err != nil {
		return nil, err
	}
	commitSequence := make([]CommitWindowView, 0, len(realtime.Commits))
	for _, commit := range realtime.Commits {
		view := CommitWindowView{Index: commit.WindowIndex}
		for _, p := range commit.Packets {
			view.PacketKeys = append(view.PacketKeys, fmt.Sprintf("%d/%d", p.Channel, p.Seq))
		}
		commitSequence = append(commitSequence, view)
	}
	report := &Report{
		GeneratedAt:    time.Now().UTC(),
		Seed:           cfg.Seed,
		SelectedBranch: selected.Spec.Name,
		DeviceURL:      deviceURL,
		EventCount:     len(selected.Events),
		Conditions:     bundle.Truth.Conditions,
		CleanAlignment: cleanAlignment,
		FaultAlignment: faultAlignment,
		FaultEchoes:    faultEchoes,
		Checkpoint: CheckpointReport{
			Path:             cfg.DBPath,
			Complete:         snapshot.Complete,
			Windows:          len(snapshot.Windows),
			Packets:          len(realtime.Packets),
			ParamVersion:     snapshot.ParamVersion,
			AlgorithmVersion: snapshot.AlgorithmVersion,
			CommitSequence:   commitSequence,
		},
		Invalidations: buildInvalidationExamples(realtime, selected.Events, capabilityValue, params),
		PathChecks:    pathChecks,
		Truth: TruthReport{
			Verification: "generator truth matched",
			SamplePoints: countTruthPoints(bundle.Truth),
			PacketRanges: len(bundle.Truth.Packets),
			Conditions:   len(bundle.Truth.Conditions),
		},
		Decisions:      realtime.Decisions,
		ForcedStopCode: forcedCode,
		Workers:        realtime.Workers,
		WorkerBuffer:   realtime.Buffer,
	}
	report.Branches = buildBranches(streams)
	report.Channels = buildChannels(realtime, capabilityValue, cleanAlignment, faultAlignment)
	runner.report = report
	cleanup = false
	return runner, nil
}

func (r *Runner) Report() *Report {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.report
}

func (r *Runner) Config() config.Config { return r.cfg }

func (r *Runner) Bundle() *generator.Bundle { return r.bundle }

func (r *Runner) Close() {
	if r.device != nil {
		_ = r.device.Close()
		r.device = nil
	}
	if r.store != nil {
		_ = r.store.Close()
		r.store = nil
	}
}

func selectStream(streams []model.Stream, name string) (model.Stream, error) {
	for _, stream := range streams {
		if stream.Spec.Name == name {
			return stream, nil
		}
	}
	return model.Stream{}, errs.BranchField(name, "selected_branch", "configured branch is not available")
}

func buildBranches(streams []model.Stream) []BranchInfo {
	out := make([]BranchInfo, 0, len(streams))
	for _, stream := range streams {
		info := BranchInfo{
			Name:          stream.Spec.Name,
			File:          filepath.Base(stream.SourceFile),
			TimeUnit:      string(stream.Spec.TimeUnit),
			ValueSemantic: string(stream.Spec.ValueSemantic),
			ListSemantic:  string(stream.Spec.ListSemantic),
			Rounding:      string(stream.Spec.TimeRounding),
			EventCount:    len(stream.Events),
		}
		if len(stream.Events) > 0 {
			event := stream.Events[0]
			info.Traceability = TraceabilitySample{EventID: event.ID, Branch: event.SourceBranch, Paths: event.SourcePaths}
		}
		out = append(out, info)
	}
	return out
}

func buildChannels(result *engine.Result, capabilityValue model.Capability, clean, fault device.Alignment) []ChannelReport {
	packetsByChannel := make(map[int][]PacketView)
	for _, p := range result.Packets {
		packetsByChannel[p.Channel] = append(packetsByChannel[p.Channel], PacketView{
			Seq:             p.Seq,
			Channel:         p.Channel,
			Start:           p.StartSample,
			End:             p.EndSample(),
			TimingMark:      p.TimingMark,
			PreviousOffset:  p.PreviousOffset,
			Samples:         append([]int64(nil), p.Samples...),
			ContentHash:     p.ContentHash,
			DependsOnEvents: append([]string(nil), p.DependsOnEvents...),
		})
	}
	matchesByChannel := make(map[int][]device.Match)
	for _, match := range clean.Matches {
		matchesByChannel[match.Channel] = append(matchesByChannel[match.Channel], match)
	}
	issuesByChannel := make(map[int][]*errs.Error)
	for _, issue := range fault.Issues {
		issuesByChannel[issue.Channel] = append(issuesByChannel[issue.Channel], issue)
	}
	statesByChannel := make(map[int]map[string]int)
	for key, state := range result.States {
		var channel int
		_, _ = fmt.Sscanf(key, "%d/", &channel)
		if statesByChannel[channel] == nil {
			statesByChannel[channel] = make(map[string]int)
		}
		statesByChannel[channel][string(state)]++
	}
	channelSpecs := make(map[int]model.ChannelSpec, len(capabilityValue.Channels))
	for _, spec := range capabilityValue.Channels {
		channelSpecs[spec.Index] = spec
	}
	out := make([]ChannelReport, 0, capabilityValue.ChannelCount)
	for channel := 0; channel < capabilityValue.ChannelCount; channel++ {
		frameSamples := 0
		for _, frame := range result.Frames {
			if frame.Channel == channel {
				frameSamples = len(frame.Samples)
			}
		}
		spec := channelSpecs[channel]
		out = append(out, ChannelReport{
			Channel:      channel,
			FrameSamples: frameSamples,
			Template:     append([]int64(nil), spec.Template...),
			TemplateHash: spec.TemplateHash,
			Packets:      packetsByChannel[channel],
			Matches:      matchesByChannel[channel],
			Issues:       issuesByChannel[channel],
			State:        statesByChannel[channel],
		})
	}
	return out
}

func buildInvalidationExamples(result *engine.Result, events []model.Event, capabilityValue model.Capability, params model.BuildParameters) []InvalidationReport {
	baseline := deps.Build(result.Packets, capabilityValue)
	out := make([]InvalidationReport, 0, 4)
	add := func(name string, change deps.Change, editedEvents []model.Event, editedCapability model.Capability) {
		updated, err := (&engine.Engine{}).Build(editedEvents, editedCapability, params)
		if err != nil {
			return
		}
		invalidation := baseline.InvalidatedBy(deps.Build(updated.Packets, editedCapability), change)
		out = append(out, InvalidationReport{
			Name:        name,
			Change:      change,
			Invalidated: invalidation.Invalidated,
			ReusedCount: invalidation.ReusedCount,
			Total:       invalidation.TotalPackets,
		})
	}

	priorityEvents := cloneEvents(events)
	for i := range priorityEvents {
		if priorityEvents[i].ID == "evt-001" {
			old := priorityEvents[i].Priority
			priorityEvents[i].Priority = 10
			add("priority-edit", deps.Change{
				Kind: deps.ChangeEventPriority, EventID: "evt-001", Old: old, New: 10,
			}, priorityEvents, capabilityValue)
		}
	}
	quantized := capabilityValue
	oldQuant := quantized.QuantUnit
	quantized.QuantUnit *= 2
	add("quantization-edit", deps.Change{
		Kind: deps.ChangeQuantUnit, Old: oldQuant, New: quantized.QuantUnit,
	}, events, quantized)

	sized := capabilityValue
	oldMax := sized.MaxPacketSamples
	sized.MaxPacketSamples = 64
	add("packet-size-edit", deps.Change{
		Kind: deps.ChangeMaxPacketSamples, Old: oldMax, New: sized.MaxPacketSamples,
	}, events, sized)

	addedEvents := cloneEvents(events)
	addedEvents = append(addedEvents, model.Event{
		ID:              "evt-added",
		SourceOrdinal:   len(addedEvents),
		StartSample:     1008,
		Priority:        1,
		Active:          true,
		Intensity:       80,
		DurationSamples: 96,
		EventTypes:      []string{"interaction"},
		Routes:          []int{1},
	})
	add("event-added", deps.Change{Kind: deps.ChangeEventAdded, EventID: "evt-added"}, addedEvents, capabilityValue)
	return out
}

func demoFaults(packets []model.Packet, timeoutMS int64) device.FaultPlan {
	fault := device.FaultPlan{DelayMS: make(map[string]int64)}
	for _, p := range packets {
		if p.Channel == 0 && p.Seq == 1 {
			fault.Drop = append(fault.Drop, packetKey(p.Channel, p.Seq))
		}
		if p.Channel == 0 && p.Seq == 2 {
			fault.Duplicate = append(fault.Duplicate, packetKey(p.Channel, p.Seq))
		}
		if p.Channel == 1 && p.Seq == 0 {
			fault.DelayMS[packetKey(p.Channel, p.Seq)] = timeoutMS + 150
		}
	}
	return fault
}

func packetKey(channel, seq int) string { return fmt.Sprintf("%d/%d", channel, seq) }

func countTruthPoints(truth generator.Truth) int {
	count := 0
	for _, channel := range truth.Channels {
		count += len(channel.Samples)
	}
	return count
}

func cloneEvents(events []model.Event) []model.Event {
	out := make([]model.Event, len(events))
	for i := range events {
		out[i] = events[i].Clone()
	}
	return out
}
