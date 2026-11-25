package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

const (
	TimeBaseSamples = "samples"
	TimeBaseMillis  = "millis"

	BudgetScalePermille   = "permille"
	BudgetScaleBasisPoint = "basis_points"

	StrategyNextFrame  = "next_frame"
	StrategyNextWindow = "next_window"
	StrategyFixedDelay = "fixed_delay"
)

var SupportedVersions = []string{"v1", "v2", "v3"}

// PCMDescription is the source-independent descriptor of a mono dialogue.
type PCMDescription struct {
	SampleRate int64 `json:"sample_rate"`
	Samples    int64 `json:"samples"`
	DurationMs int64 `json:"duration_ms"`
	Channels   int   `json:"channels"`
}

// Phoneme is one timed annotation. Frame positions are the canonical timeline.
type Phoneme struct {
	ID              string `json:"id"`
	Symbol          string `json:"symbol"`
	StartSample     int64  `json:"start_sample"`
	DurationSamples int64  `json:"duration_samples"`
	StartMs         int64  `json:"start_ms"`
	DurationMs      int64  `json:"duration_ms"`
	StartFrame      int64  `json:"start_frame"`
	DurationFrames  int64  `json:"duration_frames"`
	CostBasisPoints int64  `json:"cost_basis_points"`
}

// Dialogue groups timed phoneme annotations for one voice on one channel.
type Dialogue struct {
	ID          string         `json:"id"`
	ChannelID   string         `json:"channel_id"`
	VoiceID     string         `json:"voice_id"`
	Priority    int            `json:"priority"`
	StartFrame  int64          `json:"start_frame"`
	PCM         PCMDescription `json:"pcm"`
	Phonemes    []Phoneme      `json:"phonemes"`
	ContentHash string         `json:"content_hash"`
}

// Channel is a flat internal speaker channel. Scene survives only as metadata.
type Channel struct {
	ID                     string `json:"id"`
	Scene                  string `json:"scene"`
	SlotLimit              int    `json:"slot_limit"`
	FrameBudgetBasisPoints int64  `json:"frame_budget_basis_points"`
	SourceBudgetValue      int64  `json:"source_budget_value,omitempty"`
	SourceBudgetScale      string `json:"source_budget_scale,omitempty"`
	SourcePath             string `json:"source_path,omitempty"`
}

// RecoveryPolicy describes when and how a preempted lip packet may return.
type RecoveryPolicy struct {
	Strategy            string `json:"strategy"`
	RecoveryOffsetFrame int64  `json:"recovery_offset_frames"`
	MinContinuousFrames int    `json:"min_continuous_frames"`
	AllowAbandon        bool   `json:"allow_abandon"`
	WindowFrames        int64  `json:"window_frames"`
}

// DefaultFill records a value supplied while upgrading an older document.
type DefaultFill struct {
	Path         string `json:"path"`
	OldValue     any    `json:"old_value"`
	DefaultValue any    `json:"default_value"`
	Source       string `json:"source"`
	Version      string `json:"version"`
}

// Truncation records every integer division with a non-zero remainder.
type Truncation struct {
	Path        string `json:"path"`
	FromUnit    string `json:"from_unit"`
	ToUnit      string `json:"to_unit"`
	Input       int64  `json:"input"`
	Output      int64  `json:"output"`
	Remainder   int64  `json:"remainder"`
	Numerator   int64  `json:"numerator"`
	Denominator int64  `json:"denominator"`
	Direction   string `json:"direction"`
	ChannelID   string `json:"channel_id,omitempty"`
	PacketID    string `json:"packet_id,omitempty"`
}

// Project is the canonical in-memory representation. All time positions are
// integer frames; all costs are integer basis points.
type Project struct {
	SchemaVersion     string         `json:"schema_version"`
	Seed              int64          `json:"seed"`
	Name              string         `json:"name"`
	SampleRate        int64          `json:"sample_rate"`
	FrameRate         int64          `json:"frame_rate"`
	SourceTimeBase    string         `json:"source_time_base"`
	SourceBudgetScale string         `json:"source_budget_scale"`
	Rounding          string         `json:"rounding"`
	Channels          []Channel      `json:"channels"`
	ArbitrationOrder  []string       `json:"arbitration_order"`
	Recovery          RecoveryPolicy `json:"recovery"`
	Dialogues         []Dialogue     `json:"dialogues"`
	ContentHash       string         `json:"content_hash"`

	DefaultFills []DefaultFill `json:"default_fills,omitempty"`
	Truncations  []Truncation  `json:"truncations,omitempty"`
	SourceBytes  int           `json:"source_bytes,omitempty"`
}

// Packet is the arbitration unit derived from one phoneme annotation.
type Packet struct {
	ID              string `json:"id"`
	DialogueID      string `json:"dialogue_id"`
	ChannelID       string `json:"channel_id"`
	PhonemeID       string `json:"phoneme_id"`
	Priority        int    `json:"priority"`
	StartFrame      int64  `json:"start_frame"`
	DurationFrames  int64  `json:"duration_frames"`
	DeadlineFrame   int64  `json:"deadline_frame"`
	CostBasisPoints int64  `json:"cost_basis_points"`
	ContentHash     string `json:"content_hash"`
}

// FrameChannelResult is the stable public unit for one channel on one frame.
type FrameChannelResult struct {
	ChannelID    string   `json:"channel_id"`
	Accepted     []string `json:"accepted"`
	Delayed      []string `json:"delayed"`
	Preempted    []string `json:"preempted"`
	Resumed      []string `json:"resumed"`
	OccupiedSlot int      `json:"occupied_slots"`
	UsedBudget   int64    `json:"used_budget_basis_points"`
}

// FrameResult is one frame of deterministic arbitration output.
type FrameResult struct {
	Frame    int64                `json:"frame"`
	Channels []FrameChannelResult `json:"channels"`
}

type PreemptEvent struct {
	Frame           int64  `json:"frame"`
	ChannelID       string `json:"channel_id"`
	PacketID        string `json:"packet_id"`
	ByPacketID      string `json:"by_packet_id"`
	DeliveredFrames int64  `json:"delivered_frames"`
	RemainingFrames int64  `json:"remaining_frames"`
	RecoveryFrame   int64  `json:"recovery_frame"`
}

type ResumeEvent struct {
	Frame          int64  `json:"frame"`
	ChannelID      string `json:"channel_id"`
	PacketID       string `json:"packet_id"`
	ProtectedUntil int64  `json:"protected_until"`
}

type AbandonEvent struct {
	Frame           int64  `json:"frame"`
	ChannelID       string `json:"channel_id"`
	PacketID        string `json:"packet_id"`
	Reason          string `json:"reason"`
	AvailableFrames int64  `json:"available_frames"`
	RequiredFrames  int64  `json:"required_frames"`
}

type PacketState struct {
	ID             string `json:"id"`
	ChannelID      string `json:"channel_id"`
	Status         string `json:"status"`
	Delivered      int64  `json:"delivered_frames"`
	Remaining      int64  `json:"remaining_frames"`
	EligibleFrame  int64  `json:"eligible_frame"`
	PreemptedAt    int64  `json:"preempted_at,omitempty"`
	RecoveryFrame  int64  `json:"recovery_frame,omitempty"`
	ProtectedUntil int64  `json:"protected_until,omitempty"`
	LastDelivery   int64  `json:"last_delivery_frame,omitempty"`
	AbandonedAt    int64  `json:"abandoned_at,omitempty"`
	AbandonReason  string `json:"abandon_reason,omitempty"`
}

// ArbitrationResult is identical across live, resumed and recomputed paths.
type ArbitrationResult struct {
	Frames       []FrameResult  `json:"frames"`
	Preemptions  []PreemptEvent `json:"preemptions"`
	Resumes      []ResumeEvent  `json:"resumes"`
	Abandonments []AbandonEvent `json:"abandonments"`
	PacketStates []PacketState  `json:"packet_states"`
}

// Packets expands the dialogue phoneme annotations into arbitration units.
func (p Project) Packets() []Packet {
	grace := p.Recovery.WindowFrames
	var packets []Packet
	for _, dialogue := range p.Dialogues {
		for _, phoneme := range dialogue.Phonemes {
			start := dialogue.StartFrame + phoneme.StartFrame
			duration := phoneme.DurationFrames
			if duration < 1 {
				duration = 1
			}
			packets = append(packets, Packet{
				ID:              phoneme.ID,
				DialogueID:      dialogue.ID,
				ChannelID:       dialogue.ChannelID,
				PhonemeID:       phoneme.ID,
				Priority:        dialogue.Priority,
				StartFrame:      start,
				DurationFrames:  duration,
				DeadlineFrame:   start + duration + grace,
				CostBasisPoints: phoneme.CostBasisPoints,
				ContentHash:     dialogue.ContentHash,
			})
		}
	}
	sort.Slice(packets, func(i, j int) bool {
		if packets[i].StartFrame != packets[j].StartFrame {
			return packets[i].StartFrame < packets[j].StartFrame
		}
		if packets[i].Priority != packets[j].Priority {
			return packets[i].Priority > packets[j].Priority
		}
		return packets[i].ID < packets[j].ID
	})
	return packets
}

func (p Project) ChannelByID(id string) (Channel, bool) {
	for _, channel := range p.Channels {
		if channel.ID == id {
			return channel, true
		}
	}
	return Channel{}, false
}

// ComputeContentHash hashes only semantic dialogue/channel content. Source
// formatting, version and migration diagnostics deliberately do not affect it.
func ComputeContentHash(p Project) string {
	type hashChannel struct {
		ID                     string `json:"id"`
		Scene                  string `json:"scene"`
		SlotLimit              int    `json:"slot_limit"`
		FrameBudgetBasisPoints int64  `json:"frame_budget_basis_points"`
	}
	type hashPhoneme struct {
		ID              string `json:"id"`
		Symbol          string `json:"symbol"`
		StartFrame      int64  `json:"start_frame"`
		DurationFrames  int64  `json:"duration_frames"`
		CostBasisPoints int64  `json:"cost_basis_points"`
	}
	type hashDialogue struct {
		ID         string        `json:"id"`
		ChannelID  string        `json:"channel_id"`
		VoiceID    string        `json:"voice_id"`
		Priority   int           `json:"priority"`
		StartFrame int64         `json:"start_frame"`
		Phonemes   []hashPhoneme `json:"phonemes"`
	}
	type hashInput struct {
		Name             string         `json:"name"`
		SampleRate       int64          `json:"sample_rate"`
		FrameRate        int64          `json:"frame_rate"`
		Channels         []hashChannel  `json:"channels"`
		ArbitrationOrder []string       `json:"arbitration_order"`
		Recovery         RecoveryPolicy `json:"recovery"`
		Dialogues        []hashDialogue `json:"dialogues"`
	}
	sourceChannels := append([]Channel(nil), p.Channels...)
	sort.Slice(sourceChannels, func(i, j int) bool { return sourceChannels[i].ID < sourceChannels[j].ID })
	channels := make([]hashChannel, 0, len(sourceChannels))
	for _, channel := range sourceChannels {
		channels = append(channels, hashChannel{
			ID: channel.ID, Scene: channel.Scene, SlotLimit: channel.SlotLimit,
			FrameBudgetBasisPoints: channel.FrameBudgetBasisPoints,
		})
	}
	dialogues := append([]Dialogue(nil), p.Dialogues...)
	sort.Slice(dialogues, func(i, j int) bool { return dialogues[i].ID < dialogues[j].ID })
	hashDialogues := make([]hashDialogue, 0, len(dialogues))
	for _, dialogue := range dialogues {
		dialoguePhonemes := append([]Phoneme(nil), dialogue.Phonemes...)
		sort.Slice(dialoguePhonemes, func(i, j int) bool { return dialoguePhonemes[i].ID < dialoguePhonemes[j].ID })
		phonemes := make([]hashPhoneme, 0, len(dialoguePhonemes))
		for _, phoneme := range dialoguePhonemes {
			phonemes = append(phonemes, hashPhoneme{
				ID: phoneme.ID, Symbol: phoneme.Symbol, StartFrame: phoneme.StartFrame,
				DurationFrames: phoneme.DurationFrames, CostBasisPoints: phoneme.CostBasisPoints,
			})
		}
		hashDialogues = append(hashDialogues, hashDialogue{
			ID: dialogue.ID, ChannelID: dialogue.ChannelID, VoiceID: dialogue.VoiceID,
			Priority: dialogue.Priority, StartFrame: dialogue.StartFrame, Phonemes: phonemes,
		})
	}
	input := hashInput{
		Name:             p.Name,
		SampleRate:       p.SampleRate,
		FrameRate:        p.FrameRate,
		Channels:         channels,
		ArbitrationOrder: append([]string(nil), p.ArbitrationOrder...),
		Recovery:         p.Recovery,
		Dialogues:        hashDialogues,
	}
	data, _ := json.Marshal(input)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ComputeDialogueHash(d Dialogue) string {
	type hashPhoneme struct {
		ID              string `json:"id"`
		Symbol          string `json:"symbol"`
		StartFrame      int64  `json:"start_frame"`
		DurationFrames  int64  `json:"duration_frames"`
		CostBasisPoints int64  `json:"cost_basis_points"`
	}
	phonemes := append([]Phoneme(nil), d.Phonemes...)
	sort.Slice(phonemes, func(i, j int) bool { return phonemes[i].ID < phonemes[j].ID })
	normalized := struct {
		ID         string        `json:"id"`
		ChannelID  string        `json:"channel_id"`
		VoiceID    string        `json:"voice_id"`
		Priority   int           `json:"priority"`
		StartFrame int64         `json:"start_frame"`
		Phonemes   []hashPhoneme `json:"phonemes"`
	}{ID: d.ID, ChannelID: d.ChannelID, VoiceID: d.VoiceID, Priority: d.Priority, StartFrame: d.StartFrame}
	for _, phoneme := range phonemes {
		normalized.Phonemes = append(normalized.Phonemes, hashPhoneme{
			ID: phoneme.ID, Symbol: phoneme.Symbol, StartFrame: phoneme.StartFrame,
			DurationFrames: phoneme.DurationFrames, CostBasisPoints: phoneme.CostBasisPoints,
		})
	}
	data, _ := json.Marshal(normalized)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func StableID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:12])
}
