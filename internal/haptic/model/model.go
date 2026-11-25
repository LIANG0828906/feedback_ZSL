package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

const (
	RuleVersion      = "rules-v1.0.0"
	AlgorithmVersion = "envelope-q8-v1.0.0"
	FixedScale       = int64(256)
)

type Trace struct {
	FrameNo        int    `json:"frame_no"`
	Branch         int    `json:"branch"`
	BranchName     string `json:"branch_name"`
	InternalName   string `json:"internal_name"`
	SourcePath     string `json:"source_path"`
	SourceField    string `json:"source_field"`
	Rule           string `json:"rule"`
	RawValue       any    `json:"raw_value,omitempty"`
	ConvertedValue any    `json:"converted_value,omitempty"`
}

type LossItem struct {
	Code         string `json:"code"`
	Branch       int    `json:"branch"`
	Path         string `json:"path"`
	InternalName string `json:"internal_name"`
	Original     any    `json:"original"`
	Disposition  string `json:"disposition"`
	Detail       string `json:"detail,omitempty"`
}

type Event struct {
	FrameNo         int      `json:"frame_no"`
	ArrivalOrder    int      `json:"arrival_order"`
	EventID         string   `json:"event_id"`
	Channel         string   `json:"channel"`
	TypeCode        int      `json:"type_code"`
	TypeSequence    []int    `json:"type_sequence"`
	ChannelSequence []string `json:"channel_sequence"`
	Strength        int      `json:"strength"`
	DurationSamples int      `json:"duration_samples"`
	StartSample     int      `json:"start_sample"`
	Priority        int      `json:"priority"`
	NoInterrupt     bool     `json:"no_interrupt"`
	Conditional     bool     `json:"conditional"`
	Flag            bool     `json:"flag"`
	Template        string   `json:"template"`
	JitterAmplitude int      `json:"jitter_amplitude"`
	MergeKey        string   `json:"merge_key"`
	DesignerNote    string   `json:"designer_note,omitempty"`
	SourceBranch    int      `json:"source_branch"`
	TimeSourceValue int64    `json:"time_source_value"`
	TimeSourceBasis string   `json:"time_source_basis"`
	FlagSourceValue any      `json:"flag_source_value,omitempty"`
	Traces          []Trace  `json:"traces,omitempty"`
}

type Stream struct {
	StreamID       string  `json:"stream_id"`
	Seed           int64   `json:"seed"`
	InternalUnit   string  `json:"internal_unit"`
	SamplePeriodMS int     `json:"sample_period_ms"`
	Events         []Event `json:"events"`
	SourceBranch   int     `json:"source_branch"`
}

type Capability struct {
	Schema                 string   `json:"schema"`
	DeviceID               string   `json:"device_id"`
	Channels               []string `json:"channels"`
	MaxSamplesPerPacket    int      `json:"max_samples_per_packet"`
	SamplePeriodMS         int      `json:"sample_period_ms"`
	PacketWindowMS         int      `json:"packet_window_ms"`
	QuantumLevel           int      `json:"quantum_level"`
	MaxAmplitudeLevel      int      `json:"max_amplitude_level"`
	TailSamples            int      `json:"tail_samples"`
	MaxWorkers             int      `json:"max_workers"`
	PerThreadBufferSamples int      `json:"per_thread_buffer_samples"`
}

type Decision struct {
	FrameNo        int    `json:"frame_no"`
	EventID        string `json:"event_id"`
	Channel        string `json:"channel"`
	Action         string `json:"action"`
	Reason         string `json:"reason"`
	AgainstFrame   int    `json:"against_frame,omitempty"`
	StartSample    int    `json:"start_sample"`
	EndSample      int    `json:"end_sample"`
	EffectiveFrom  int    `json:"effective_from,omitempty"`
	EffectiveTo    int    `json:"effective_to,omitempty"`
	JitterPermille int    `json:"jitter_permille,omitempty"`
}

type Frame struct {
	SampleIndex int   `json:"sample_index"`
	FrameQ8     int64 `json:"frame_q8"`
	Level       int   `json:"level"`
}

type Packet struct {
	GlobalSeq           int          `json:"global_seq"`
	Channel             string       `json:"channel"`
	PacketIndex         int          `json:"packet_index"`
	StartSample         int          `json:"start_sample"`
	EndSample           int          `json:"end_sample"`
	SampleCount         int          `json:"sample_count"`
	TimestampMS         int64        `json:"timestamp_ms"`
	WindowStartMS       int64        `json:"window_start_ms"`
	WindowEndMS         int64        `json:"window_end_ms"`
	Frames              []Frame      `json:"frames"`
	FrameDigest         string       `json:"frame_digest"`
	ConsumedEventOffset int          `json:"consumed_event_offset"`
	EventCount          int          `json:"event_count"`
	EffectiveCount      int          `json:"effective_count"`
	StreamSeed          int64        `json:"stream_seed"`
	RNGProgress         int64        `json:"rng_progress"`
	FingerprintDelta    string       `json:"fingerprint_delta"`
	SemanticFingerprint string       `json:"semantic_fingerprint"`
	ContentHash         string       `json:"content_hash"`
	RuleVersion         string       `json:"rule_version"`
	AlgorithmVersion    string       `json:"algorithm_version"`
	BranchSources       []int        `json:"branch_sources"`
	SourceBranch        int          `json:"source_branch"`
	RuleSnapshot        RuleSnapshot `json:"rule_snapshot"`
	Decisions           []Decision   `json:"decisions"`
	Events              []Event      `json:"events,omitempty"`
}

type RuleSnapshot struct {
	RuleVersion         string           `json:"rule_version"`
	AlgorithmVersion    string           `json:"algorithm_version"`
	SamplePeriodMS      int              `json:"sample_period_ms"`
	QuantumLevel        int              `json:"quantum_level"`
	MaxAmplitude        int              `json:"max_amplitude"`
	MaxSamplesPerPacket int              `json:"max_samples_per_packet"`
	PacketWindowMS      int              `json:"packet_window_ms"`
	FixedScale          int64            `json:"fixed_scale"`
	Templates           map[string][]int `json:"templates"`
}

func (s Stream) CanonicalValues() any {
	events := make([]Event, len(s.Events))
	copy(events, s.Events)
	for i := range events {
		events[i].Traces = nil
		events[i].SourceBranch = 0
		events[i].TimeSourceValue = 0
		events[i].TimeSourceBasis = ""
		events[i].FlagSourceValue = nil
	}
	return struct {
		StreamID string  `json:"stream_id"`
		Seed     int64   `json:"seed"`
		Unit     string  `json:"internal_unit"`
		PeriodMS int     `json:"sample_period_ms"`
		Events   []Event `json:"events"`
	}{s.StreamID, s.Seed, s.InternalUnit, s.SamplePeriodMS, events}
}

func (s Stream) Fingerprint() string {
	b, _ := json.Marshal(s.CanonicalValues())
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func SortedChannels(channels []string) []string {
	out := append([]string(nil), channels...)
	sort.Strings(out)
	return out
}

func DigestFrames(frames []Frame) string {
	b, _ := json.Marshal(frames)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func FingerprintDelta(pkt Packet) string {
	b, _ := json.Marshal(struct {
		Channel string `json:"channel"`
		Index   int    `json:"packet_index"`
		Start   int    `json:"start"`
		Count   int    `json:"count"`
		Offset  int    `json:"offset"`
		Events  int    `json:"events"`
		Digest  string `json:"digest"`
	}{pkt.Channel, pkt.PacketIndex, pkt.StartSample, pkt.SampleCount, pkt.ConsumedEventOffset, pkt.EventCount, pkt.FrameDigest})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func PacketContentHash(pkt Packet) string {
	b, _ := json.Marshal(struct {
		Seq            int          `json:"seq"`
		Channel        string       `json:"channel"`
		Index          int          `json:"index"`
		Start          int          `json:"start"`
		End            int          `json:"end"`
		Count          int          `json:"count"`
		TimestampMS    int64        `json:"timestamp_ms"`
		WindowStart    int64        `json:"window_start_ms"`
		WindowEnd      int64        `json:"window_end_ms"`
		Digest         string       `json:"digest"`
		Offset         int          `json:"offset"`
		EventCount     int          `json:"event_count"`
		EffectiveCount int          `json:"effective_count"`
		Seed           int64        `json:"seed"`
		RNG            int64        `json:"rng"`
		FPDelta        string       `json:"fp_delta"`
		FP             string       `json:"fp"`
		Rule           string       `json:"rule"`
		Algo           string       `json:"algo"`
		SourceBranch   int          `json:"source_branch"`
		Branches       []int        `json:"branches"`
		Decisions      []Decision   `json:"decisions"`
		RuleSnapshot   RuleSnapshot `json:"rules"`
		Events         []Event      `json:"events"`
	}{pkt.GlobalSeq, pkt.Channel, pkt.PacketIndex, pkt.StartSample, pkt.EndSample, pkt.SampleCount,
		pkt.TimestampMS, pkt.WindowStartMS, pkt.WindowEndMS, pkt.FrameDigest,
		pkt.ConsumedEventOffset, pkt.EventCount, pkt.EffectiveCount, pkt.StreamSeed, pkt.RNGProgress,
		pkt.FingerprintDelta, pkt.SemanticFingerprint, pkt.RuleVersion, pkt.AlgorithmVersion,
		pkt.SourceBranch, pkt.BranchSources, pkt.Decisions, pkt.RuleSnapshot, pkt.Events})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
