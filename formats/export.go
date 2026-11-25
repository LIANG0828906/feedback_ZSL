package formats

import (
	"encoding/json"
	"fmt"
	"sort"

	"lipsync/domain"
	"lipsync/units"
)

// LossItem is one explicit downgrade disposition. It never silently claims
// that a lower-version field preserved unavailable semantics.
type LossItem struct {
	Path         string `json:"path"`
	ChannelID    string `json:"channel_id,omitempty"`
	PacketID     string `json:"packet_id,omitempty"`
	Kind         string `json:"kind"`
	OldValue     any    `json:"old_value"`
	EmittedValue any    `json:"emitted_value"`
	Disposition  string `json:"disposition"`
}

type LossReport struct {
	Code        domain.Code         `json:"code"`
	Version     string              `json:"version"`
	Items       []LossItem          `json:"items"`
	Truncations []domain.Truncation `json:"truncations,omitempty"`
}

func (r *LossReport) HasLosses() bool {
	return r != nil && len(r.Items) > 0
}

func newLossReport(version string) *LossReport {
	return &LossReport{Code: domain.CodeLossy, Version: version}
}

func (r *LossReport) add(item LossItem) {
	r.Items = append(r.Items, item)
}

func (r *LossReport) record(c units.Conversion, path, from, to, channel, packet string) {
	if !c.Truncated() {
		return
	}
	units.RecordTruncation(&r.Truncations, path, from, to, c, channel, packet)
	r.add(LossItem{
		Path: path, ChannelID: channel, PacketID: packet, Kind: "rounding",
		OldValue: c.Input, EmittedValue: c.Output,
		Disposition: fmt.Sprintf("%s rounding; remainder=%d numerator=%d denominator=%d", c.Direction, c.Remainder, c.Numerator, c.Denominator),
	})
}

// Export serializes the canonical model into one of the three declared
// protocols. Losses are returned separately and are never hidden in the data.
func Export(project domain.Project, version string) ([]byte, *LossReport, error) {
	switch version {
	case "v1":
		return exportV1(project)
	case "v2":
		return exportV2(project)
	case "v3":
		return exportV3(project)
	default:
		return nil, nil, &domain.Error{Code: domain.CodeSchema, Path: "version", Message: "unknown export version"}
	}
}

type v1PCM struct {
	SampleRate int64 `json:"sample_rate"`
	Samples    int64 `json:"samples"`
	DurationMs int64 `json:"duration_ms"`
	Channels   int   `json:"channels"`
}

type v1Phoneme struct {
	ID              string `json:"phoneme_id"`
	Symbol          string `json:"symbol"`
	StartSample     int64  `json:"start_sample"`
	DurationSamples int64  `json:"duration_samples"`
	CostPermille    int64  `json:"cost_permille"`
}

type v1Dialogue struct {
	ID          string      `json:"dialogue_id"`
	ChannelID   string      `json:"channel_id"`
	VoiceID     string      `json:"voice_id"`
	Priority    int         `json:"priority"`
	StartSample int64       `json:"start_sample"`
	PCM         v1PCM       `json:"pcm"`
	Phonemes    []v1Phoneme `json:"phonemes"`
}

type v1Channel struct {
	ID          string `json:"channel_id"`
	SceneID     string `json:"scene_id,omitempty"`
	SlotLimit   int    `json:"slot_limit"`
	FrameBudget int64  `json:"frame_budget"`
}

type v1Document struct {
	SchemaVersion    string       `json:"schema_version"`
	Seed             int64        `json:"seed"`
	Name             string       `json:"name"`
	TimeBase         string       `json:"time_base"`
	BudgetScale      string       `json:"budget_scale"`
	Rounding         string       `json:"rounding"`
	SampleRate       int64        `json:"sample_rate"`
	FrameRate        int64        `json:"frame_rate"`
	Channels         []v1Channel  `json:"channels"`
	ArbitrationOrder []string     `json:"arbitration_order"`
	Dialogues        []v1Dialogue `json:"dialogues"`
}

func exportV1(project domain.Project) ([]byte, *LossReport, error) {
	report := newLossReport("v1")
	channels := make([]v1Channel, 0, len(project.Channels))
	for _, channel := range project.Channels {
		budget, err := units.BasisPointsToPermille(channel.FrameBudgetBasisPoints, units.RoundFloor, "channel."+channel.ID+".frame_budget")
		if err != nil {
			return nil, nil, err
		}
		report.record(budget, "channels["+channel.ID+"].frame_budget", domain.BudgetScaleBasisPoint, domain.BudgetScalePermille, channel.ID, "")
		channels = append(channels, v1Channel{
			ID: channel.ID, SceneID: channel.Scene, SlotLimit: channel.SlotLimit, FrameBudget: budget.Output,
		})
		if channel.Scene != "" {
			report.add(LossItem{
				Path: "scenes[" + channel.Scene + "]", ChannelID: channel.ID, Kind: "scene_tree",
				OldValue: channel.Scene, EmittedValue: channel.Scene,
				Disposition: "scene tree flattened; scene retained as optional channel scene_id",
			})
		}
	}
	dialogues := make([]v1Dialogue, 0, len(project.Dialogues))
	for _, dialogue := range project.Dialogues {
		start, err := units.FramesToSamples(dialogue.StartFrame, project.SampleRate, project.FrameRate, units.RoundFloor, "dialogue."+dialogue.ID+".start_frame")
		if err != nil {
			return nil, nil, err
		}
		report.record(start, "dialogues["+dialogue.ID+"].start_frame", "frames", domain.TimeBaseSamples, dialogue.ChannelID, "")
		phonemes := make([]v1Phoneme, 0, len(dialogue.Phonemes))
		for _, phoneme := range dialogue.Phonemes {
			phStart, err := units.FramesToSamples(phoneme.StartFrame, project.SampleRate, project.FrameRate, units.RoundFloor, "phoneme."+phoneme.ID+".start_frame")
			if err != nil {
				return nil, nil, err
			}
			report.record(phStart, "dialogues["+dialogue.ID+"].phonemes["+phoneme.ID+"].start_frame", "frames", domain.TimeBaseSamples, dialogue.ChannelID, phoneme.ID)
			phDuration, err := units.FramesToSamples(phoneme.DurationFrames, project.SampleRate, project.FrameRate, units.RoundFloor, "phoneme."+phoneme.ID+".duration_frames")
			if err != nil {
				return nil, nil, err
			}
			report.record(phDuration, "dialogues["+dialogue.ID+"].phonemes["+phoneme.ID+"].duration_frames", "frames", domain.TimeBaseSamples, dialogue.ChannelID, phoneme.ID)
			cost, err := units.BasisPointsToPermille(phoneme.CostBasisPoints, units.RoundFloor, "phoneme."+phoneme.ID+".cost")
			if err != nil {
				return nil, nil, err
			}
			report.record(cost, "dialogues["+dialogue.ID+"].phonemes["+phoneme.ID+"].cost", domain.BudgetScaleBasisPoint, domain.BudgetScalePermille, dialogue.ChannelID, phoneme.ID)
			phonemes = append(phonemes, v1Phoneme{
				ID: phoneme.ID, Symbol: phoneme.Symbol, StartSample: phStart.Output,
				DurationSamples: phDuration.Output, CostPermille: cost.Output,
			})
		}
		dialogues = append(dialogues, v1Dialogue{
			ID: dialogue.ID, ChannelID: dialogue.ChannelID, VoiceID: dialogue.VoiceID,
			Priority: dialogue.Priority, StartSample: start.Output,
			PCM: v1PCM{
				SampleRate: dialogue.PCM.SampleRate, Samples: dialogue.PCM.Samples,
				DurationMs: dialogue.PCM.DurationMs, Channels: dialogue.PCM.Channels,
			},
			Phonemes: phonemes,
		})
	}
	report.add(LossItem{
		Path: "preemption", Kind: "preemption_policy",
		OldValue: project.Recovery, EmittedValue: nil,
		Disposition: "v1 cannot express preemption; loader applies protocol:v1-default recovery policy",
	})
	doc := v1Document{
		SchemaVersion: "v1", Seed: project.Seed, Name: project.Name,
		TimeBase: domain.TimeBaseSamples, BudgetScale: domain.BudgetScalePermille, Rounding: "floor",
		SampleRate: project.SampleRate, FrameRate: project.FrameRate,
		Channels: channels, ArbitrationOrder: project.ArbitrationOrder, Dialogues: dialogues,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	return data, report, err
}

type v2PCM struct {
	SampleRate int64 `json:"sampleRate"`
	Samples    int64 `json:"samples"`
	DurationMs int64 `json:"durationMs"`
	Channels   int   `json:"channels"`
}

type v2Phoneme struct {
	ID              string `json:"id"`
	Symbol          string `json:"symbol"`
	StartMs         int64  `json:"startMs"`
	DurationMs      int64  `json:"durationMs"`
	CostBasisPoints int64  `json:"costBasisPoints"`
}

type v2Dialogue struct {
	ID        string      `json:"id"`
	ChannelID string      `json:"channelId"`
	VoiceID   string      `json:"voiceId"`
	Priority  int         `json:"priority"`
	StartMs   int64       `json:"startMs"`
	PCM       v2PCM       `json:"pcm"`
	Phonemes  []v2Phoneme `json:"phonemes"`
}

type v2Channel struct {
	ID          string `json:"id"`
	SlotLimit   int    `json:"slots"`
	FrameBudget int64  `json:"frameBudget"`
}

type v2Scene struct {
	SceneName string      `json:"sceneName"`
	Channels  []v2Channel `json:"channels"`
}

type v2Preemption struct {
	Strategy            string `json:"strategy"`
	MinContinuousFrames int    `json:"minContinuousFrames"`
	WindowFrames        int64  `json:"windowFrames"`
}

type v2Document struct {
	SchemaVersion string `json:"schemaVersion"`
	Seed          int64  `json:"seed"`
	Name          string `json:"name"`
	TimeBase      string `json:"timeBase"`
	BudgetScale   string `json:"budgetScale"`
	Rounding      string `json:"rounding"`
	Framing       struct {
		SampleRate int64 `json:"sampleRate"`
		FrameRate  int64 `json:"frameRate"`
	} `json:"framing"`
	Scenes           []v2Scene    `json:"scenes"`
	ArbitrationOrder []string     `json:"arbitrationOrder"`
	Preemption       v2Preemption `json:"preemption"`
	Dialogues        []v2Dialogue `json:"dialogues"`
}

func exportV2(project domain.Project) ([]byte, *LossReport, error) {
	report := newLossReport("v2")
	sceneChannels := map[string][]v2Channel{}
	var sceneNames []string
	for _, channel := range project.Channels {
		scene := channel.Scene
		if scene == "" {
			scene = "default"
		}
		if _, ok := sceneChannels[scene]; !ok {
			sceneNames = append(sceneNames, scene)
		}
		sceneChannels[scene] = append(sceneChannels[scene], v2Channel{
			ID: channel.ID, SlotLimit: channel.SlotLimit, FrameBudget: channel.FrameBudgetBasisPoints,
		})
	}
	sort.Strings(sceneNames)
	scenes := make([]v2Scene, 0, len(sceneNames))
	for _, scene := range sceneNames {
		channels := sceneChannels[scene]
		sort.Slice(channels, func(i, j int) bool { return channels[i].ID < channels[j].ID })
		scenes = append(scenes, v2Scene{SceneName: scene, Channels: channels})
	}
	dialogues := make([]v2Dialogue, 0, len(project.Dialogues))
	for _, dialogue := range project.Dialogues {
		start, err := units.FramesToMillis(dialogue.StartFrame, project.FrameRate, units.RoundFloor, "dialogue."+dialogue.ID+".start_frame")
		if err != nil {
			return nil, nil, err
		}
		report.record(start, "dialogues["+dialogue.ID+"].start", "frames", domain.TimeBaseMillis, dialogue.ChannelID, "")
		phonemes := make([]v2Phoneme, 0, len(dialogue.Phonemes))
		for _, phoneme := range dialogue.Phonemes {
			startMs, err := units.FramesToMillis(phoneme.StartFrame, project.FrameRate, units.RoundFloor, "phoneme."+phoneme.ID+".start_frame")
			if err != nil {
				return nil, nil, err
			}
			report.record(startMs, "dialogues["+dialogue.ID+"].phonemes["+phoneme.ID+"].start", "frames", domain.TimeBaseMillis, dialogue.ChannelID, phoneme.ID)
			durationMs, err := units.FramesToMillis(phoneme.DurationFrames, project.FrameRate, units.RoundFloor, "phoneme."+phoneme.ID+".duration_frames")
			if err != nil {
				return nil, nil, err
			}
			report.record(durationMs, "dialogues["+dialogue.ID+"].phonemes["+phoneme.ID+"].duration", "frames", domain.TimeBaseMillis, dialogue.ChannelID, phoneme.ID)
			phonemes = append(phonemes, v2Phoneme{
				ID: phoneme.ID, Symbol: phoneme.Symbol, StartMs: startMs.Output,
				DurationMs: durationMs.Output, CostBasisPoints: phoneme.CostBasisPoints,
			})
		}
		dialogues = append(dialogues, v2Dialogue{
			ID: dialogue.ID, ChannelID: dialogue.ChannelID, VoiceID: dialogue.VoiceID,
			Priority: dialogue.Priority, StartMs: start.Output,
			PCM: v2PCM{
				SampleRate: dialogue.PCM.SampleRate, Samples: dialogue.PCM.Samples,
				DurationMs: dialogue.PCM.DurationMs, Channels: dialogue.PCM.Channels,
			},
			Phonemes: phonemes,
		})
	}
	if project.Recovery.RecoveryOffsetFrame != 1 {
		report.add(LossItem{
			Path: "preemption.recovery_offset_frames", Kind: "recovery_offset",
			OldValue: project.Recovery.RecoveryOffsetFrame, EmittedValue: 1,
			Disposition: "v2 cannot express recovery offset; loader defaults to 1",
		})
	}
	if project.Recovery.AllowAbandon != true {
		report.add(LossItem{
			Path: "preemption.allow_abandon", Kind: "allow_abandon",
			OldValue: project.Recovery.AllowAbandon, EmittedValue: true,
			Disposition: "v2 cannot express allow_abandon; loader defaults to true",
		})
	}
	doc := v2Document{
		SchemaVersion: "v2", Seed: project.Seed, Name: project.Name,
		TimeBase: domain.TimeBaseMillis, BudgetScale: domain.BudgetScaleBasisPoint, Rounding: "floor",
		Scenes: scenes, ArbitrationOrder: project.ArbitrationOrder,
		Preemption: v2Preemption{
			Strategy: project.Recovery.Strategy, MinContinuousFrames: project.Recovery.MinContinuousFrames,
			WindowFrames: project.Recovery.WindowFrames,
		},
		Dialogues: dialogues,
	}
	doc.Framing.SampleRate = project.SampleRate
	doc.Framing.FrameRate = project.FrameRate
	data, err := json.MarshalIndent(doc, "", "  ")
	return data, report, err
}

type v3PCM struct {
	SampleRate int64 `json:"sample_rate"`
	Samples    int64 `json:"samples"`
	DurationMs int64 `json:"duration_ms"`
	Channels   int   `json:"channels"`
}

type v3Phoneme struct {
	ID              string `json:"phoneme_id"`
	Symbol          string `json:"symbol"`
	StartSample     int64  `json:"start_sample"`
	DurationSamples int64  `json:"duration_samples"`
	CostBasisPoints int64  `json:"cost_basis_points"`
}

type v3Dialogue struct {
	ID          string      `json:"dialogue_id"`
	ChannelID   string      `json:"channel_id"`
	VoiceID     string      `json:"voice_id"`
	Priority    int         `json:"priority"`
	StartSample int64       `json:"start_sample"`
	PCM         v3PCM       `json:"pcm"`
	Phonemes    []v3Phoneme `json:"phonemes"`
}

type v3Channel struct {
	ID          string `json:"channel_id"`
	SlotLimit   int    `json:"slot_limit"`
	FrameBudget int64  `json:"frame_budget_basis_points"`
}

type v3Scene struct {
	SceneName string      `json:"scene_name"`
	Channels  []v3Channel `json:"channels"`
}

type v3Preemption struct {
	Strategy            string `json:"strategy"`
	RecoveryOffsetFrame int64  `json:"recovery_offset_frames"`
	MinContinuousFrames int    `json:"min_continuous_frames"`
	AllowAbandon        bool   `json:"allow_abandon"`
	WindowFrames        int64  `json:"window_frames"`
}

type v3Document struct {
	SchemaVersion    string       `json:"schema_version"`
	Seed             int64        `json:"seed"`
	Name             string       `json:"name"`
	TimeBase         string       `json:"time_base"`
	BudgetScale      string       `json:"budget_scale"`
	Rounding         string       `json:"rounding"`
	SampleRate       int64        `json:"sample_rate"`
	FrameRate        int64        `json:"frame_rate"`
	Scenes           []v3Scene    `json:"scenes"`
	ArbitrationOrder []string     `json:"arbitration_order"`
	Preemption       v3Preemption `json:"preemption"`
	Dialogues        []v3Dialogue `json:"dialogues"`
}

func exportV3(project domain.Project) ([]byte, *LossReport, error) {
	report := newLossReport("v3")
	sceneChannels := map[string][]v3Channel{}
	var sceneNames []string
	for _, channel := range project.Channels {
		scene := channel.Scene
		if scene == "" {
			scene = "default"
		}
		if _, ok := sceneChannels[scene]; !ok {
			sceneNames = append(sceneNames, scene)
		}
		sceneChannels[scene] = append(sceneChannels[scene], v3Channel{
			ID: channel.ID, SlotLimit: channel.SlotLimit, FrameBudget: channel.FrameBudgetBasisPoints,
		})
	}
	sort.Strings(sceneNames)
	scenes := make([]v3Scene, 0, len(sceneNames))
	for _, scene := range sceneNames {
		channels := sceneChannels[scene]
		sort.Slice(channels, func(i, j int) bool { return channels[i].ID < channels[j].ID })
		scenes = append(scenes, v3Scene{SceneName: scene, Channels: channels})
	}
	dialogues := make([]v3Dialogue, 0, len(project.Dialogues))
	for _, dialogue := range project.Dialogues {
		start, err := units.FramesToSamples(dialogue.StartFrame, project.SampleRate, project.FrameRate, units.RoundFloor, "dialogue."+dialogue.ID+".start_frame")
		if err != nil {
			return nil, nil, err
		}
		report.record(start, "dialogues["+dialogue.ID+"].start_frame", "frames", domain.TimeBaseSamples, dialogue.ChannelID, "")
		phonemes := make([]v3Phoneme, 0, len(dialogue.Phonemes))
		for _, phoneme := range dialogue.Phonemes {
			startSample, err := units.FramesToSamples(phoneme.StartFrame, project.SampleRate, project.FrameRate, units.RoundFloor, "phoneme."+phoneme.ID+".start_frame")
			if err != nil {
				return nil, nil, err
			}
			report.record(startSample, "dialogues["+dialogue.ID+"].phonemes["+phoneme.ID+"].start_frame", "frames", domain.TimeBaseSamples, dialogue.ChannelID, phoneme.ID)
			durationSamples, err := units.FramesToSamples(phoneme.DurationFrames, project.SampleRate, project.FrameRate, units.RoundFloor, "phoneme."+phoneme.ID+".duration_frames")
			if err != nil {
				return nil, nil, err
			}
			report.record(durationSamples, "dialogues["+dialogue.ID+"].phonemes["+phoneme.ID+"].duration_frames", "frames", domain.TimeBaseSamples, dialogue.ChannelID, phoneme.ID)
			phonemes = append(phonemes, v3Phoneme{
				ID: phoneme.ID, Symbol: phoneme.Symbol, StartSample: startSample.Output,
				DurationSamples: durationSamples.Output, CostBasisPoints: phoneme.CostBasisPoints,
			})
		}
		dialogues = append(dialogues, v3Dialogue{
			ID: dialogue.ID, ChannelID: dialogue.ChannelID, VoiceID: dialogue.VoiceID,
			Priority: dialogue.Priority, StartSample: start.Output,
			PCM: v3PCM{
				SampleRate: dialogue.PCM.SampleRate, Samples: dialogue.PCM.Samples,
				DurationMs: dialogue.PCM.DurationMs, Channels: dialogue.PCM.Channels,
			},
			Phonemes: phonemes,
		})
	}
	doc := v3Document{
		SchemaVersion: "v3", Seed: project.Seed, Name: project.Name,
		TimeBase: domain.TimeBaseSamples, BudgetScale: domain.BudgetScaleBasisPoint, Rounding: "floor",
		SampleRate: project.SampleRate, FrameRate: project.FrameRate,
		Scenes: scenes, ArbitrationOrder: project.ArbitrationOrder,
		Preemption: v3Preemption{
			Strategy: project.Recovery.Strategy, RecoveryOffsetFrame: project.Recovery.RecoveryOffsetFrame,
			MinContinuousFrames: project.Recovery.MinContinuousFrames, AllowAbandon: project.Recovery.AllowAbandon,
			WindowFrames: project.Recovery.WindowFrames,
		},
		Dialogues: dialogues,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	return data, report, err
}
