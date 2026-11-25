package diag

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"lipsync/arbitration"
	"lipsync/checkpoint"
	"lipsync/deps"
	"lipsync/domain"
	"lipsync/engine"
	"lipsync/formats"
	"lipsync/scenario"
)

type VersionView struct {
	Version          string               `json:"version"`
	FieldNaming      string               `json:"field_naming"`
	TimeBase         string               `json:"time_base"`
	BudgetScale      string               `json:"budget_scale"`
	ChannelLayout    string               `json:"channel_layout"`
	PreemptionFields bool                 `json:"preemption_fields"`
	ContentHash      string               `json:"content_hash"`
	Channels         []ChannelView        `json:"channels"`
	Order            []string             `json:"arbitration_order"`
	DefaultFills     []domain.DefaultFill `json:"default_fills"`
	Losses           *formats.LossReport  `json:"losses,omitempty"`
}

type ChannelView struct {
	ID           string `json:"id"`
	Scene        string `json:"scene"`
	Slots        int    `json:"slots"`
	Budget       int64  `json:"frame_budget_basis_points"`
	SourceBudget int64  `json:"source_budget"`
	SourceScale  string `json:"source_scale"`
	SourcePath   string `json:"source_path"`
}

type FaultView struct {
	Kind      string      `json:"kind"`
	Code      domain.Code `json:"code"`
	Path      string      `json:"path,omitempty"`
	ChannelID string      `json:"channel_id,omitempty"`
	PacketID  string      `json:"packet_id,omitempty"`
	Message   string      `json:"message"`
}

type State struct {
	Seed          int64                               `json:"seed"`
	Project       string                              `json:"project"`
	ContentHash   string                              `json:"content_hash"`
	Versions      []VersionView                       `json:"versions"`
	Truth         domain.ArbitrationResult            `json:"truth"`
	Run           *engine.RunReport                   `json:"run"`
	TruthMatches  bool                                `json:"truth_matches"`
	Invalidations map[string]*deps.InvalidationReport `json:"invalidations"`
	Faults        map[string]FaultView                `json:"faults"`
	Database      string                              `json:"database"`
	Limits        map[string]int                      `json:"limits"`
}

type BuildOptions struct {
	Seed            int64
	DBPath          string
	Fresh           bool
	Window          int
	MaxWorkers      int
	QueueBudget     int
	CandidateBuffer int
}

func DefaultBuildOptions() BuildOptions {
	return BuildOptions{
		Seed: 42, DBPath: filepath.Join("artifacts", "lipsync-diagnostic.db"),
		Fresh: true, Window: 4, MaxWorkers: 4, QueueBudget: 8, CandidateBuffer: 3,
	}
}

func BuildState(ctx context.Context, options BuildOptions) (*State, error) {
	if options.DBPath == "" {
		return nil, fmt.Errorf("diagnostic db path is empty")
	}
	if options.Fresh {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(options.DBPath + suffix)
		}
	}
	if err := os.MkdirAll(filepath.Dir(options.DBPath), 0o755); err != nil {
		return nil, err
	}
	artifacts, err := scenario.Generate(options.Seed)
	if err != nil {
		return nil, err
	}
	projects, err := artifacts.LoadAll()
	if err != nil {
		return nil, err
	}
	state := &State{
		Seed: options.Seed, Project: artifacts.Canonical.Name,
		ContentHash:   artifacts.Canonical.ContentHash,
		Versions:      make([]VersionView, 0, len(domain.SupportedVersions)),
		Invalidations: map[string]*deps.InvalidationReport{},
		Faults:        map[string]FaultView{},
		Database:      options.DBPath,
		Limits: map[string]int{
			"max_workers": options.MaxWorkers, "queue_budget": options.QueueBudget,
			"candidate_buffer": options.CandidateBuffer, "window_frames": options.Window,
		},
		Truth: artifacts.Truth,
	}
	for _, version := range domain.SupportedVersions {
		project := projects[version]
		view := describeVersion(version, project, artifacts.LossReports[version])
		state.Versions = append(state.Versions, view)
	}
	store, err := checkpoint.Open(options.DBPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	engineOptions := engine.DefaultOptions()
	engineOptions.WindowSize = options.Window
	engineOptions.MaxWorkers = options.MaxWorkers
	engineOptions.QueueBudget = options.QueueBudget
	engineOptions.CandidateBuffer = options.CandidateBuffer
	runner, err := engine.New(artifacts.Canonical, store, engineOptions)
	if err != nil {
		return nil, err
	}
	report, err := runner.Run(ctx)
	runner.Close()
	if err != nil {
		return nil, err
	}
	state.Run = report
	state.TruthMatches = sameTruth(report, artifacts.Truth)

	budget := int64(7000)
	edits := map[string]deps.Change{
		"budget": {Kind: deps.ChangeBudget, ChannelID: "spk_main", Budget: budget},
		"order":  {Kind: deps.ChangeOrder, Order: []string{"spk_alt", "spk_wide", "spk_main"}},
		"recovery": {Kind: deps.ChangeRecovery, Recovery: &domain.RecoveryPolicy{
			Strategy: "next_window", RecoveryOffsetFrame: 1, MinContinuousFrames: 2,
			AllowAbandon: true, WindowFrames: 4,
		}},
	}
	editOrder := []string{"budget", "order", "recovery"}
	for _, name := range editOrder {
		_, _, _, editReport, err := deps.ApplyEdit(artifacts.Canonical, edits[name], store)
		if err != nil {
			return nil, fmt.Errorf("invalidation %s: %w", name, err)
		}
		state.Invalidations[name] = editReport
	}
	state.Faults["decode"] = runFault("decode", func() error {
		_, err := formats.DecodeAndLoad([]byte(`{"schema_version":`))
		return err
	})
	state.Faults["order"] = runFault("order", func() error {
		_, err := arbitration.RunFrames(artifacts.Canonical, []int64{0, 2, 1})
		return err
	})
	state.Faults["cap"] = runFault("cap", func() error {
		broken := artifacts.Canonical
		broken.Channels = append([]domain.Channel(nil), broken.Channels...)
		broken.Channels[0].FrameBudgetBasisPoints = 100
		broken.ContentHash = domain.ComputeContentHash(broken)
		return arbitration.ValidateProject(broken, true)
	})
	return state, nil
}

func describeVersion(version string, project *domain.Project, losses *formats.LossReport) VersionView {
	view := VersionView{
		Version: version, ContentHash: project.ContentHash,
		Order:        append([]string(nil), project.ArbitrationOrder...),
		DefaultFills: append([]domain.DefaultFill(nil), project.DefaultFills...),
		Losses:       losses,
	}
	switch version {
	case "v1":
		view.FieldNaming, view.TimeBase, view.BudgetScale = "snake_case", "samples", "permille"
		view.ChannelLayout, view.PreemptionFields = "flat", false
	case "v2":
		view.FieldNaming, view.TimeBase, view.BudgetScale = "camelCase", "millis", "basis_points"
		view.ChannelLayout, view.PreemptionFields = "scene_tree", true
	case "v3":
		view.FieldNaming, view.TimeBase, view.BudgetScale = "snake_case", "samples", "basis_points"
		view.ChannelLayout, view.PreemptionFields = "scene_tree", true
	}
	channels := append([]domain.Channel(nil), project.Channels...)
	sort.Slice(channels, func(i, j int) bool { return channels[i].ID < channels[j].ID })
	for _, channel := range channels {
		view.Channels = append(view.Channels, ChannelView{
			ID: channel.ID, Scene: channel.Scene, Slots: channel.SlotLimit,
			Budget: channel.FrameBudgetBasisPoints, SourceBudget: channel.SourceBudgetValue,
			SourceScale: channel.SourceBudgetScale, SourcePath: channel.SourcePath,
		})
	}
	return view
}

func sameTruth(report *engine.RunReport, truth domain.ArbitrationResult) bool {
	return reflect.DeepEqual(report.Frames, truth.Frames) &&
		reflect.DeepEqual(report.Preemptions, truth.Preemptions) &&
		reflect.DeepEqual(report.Resumes, truth.Resumes) &&
		reflect.DeepEqual(report.Abandonments, truth.Abandonments) &&
		reflect.DeepEqual(report.PacketStates, truth.PacketStates)
}

func runFault(kind string, run func() error) FaultView {
	err := run()
	if err == nil {
		return FaultView{Kind: kind, Message: "no fault generated"}
	}
	view := FaultView{Kind: kind, Code: domain.CodeOf(err), Message: err.Error()}
	var domainErr *domain.Error
	if errorsAs(err, &domainErr) {
		view.Path = domainErr.Path
		view.ChannelID = domainErr.ChannelID
		view.PacketID = domainErr.PacketID
	}
	return view
}

func errorsAs(err error, target **domain.Error) bool {
	for err != nil {
		if typed, ok := err.(*domain.Error); ok {
			*target = typed
			return true
		}
		next, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = next.Unwrap()
	}
	return false
}

func (s *State) JSON() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}
