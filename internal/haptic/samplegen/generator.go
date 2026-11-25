package samplegen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/liang/hapticfeedback/internal/haptic/boundary"
	"github.com/liang/hapticfeedback/internal/haptic/branches"
	"github.com/liang/hapticfeedback/internal/haptic/model"
	"github.com/liang/hapticfeedback/internal/haptic/reference"
)

const DefaultSeed int64 = 420243

func Capability() model.Capability {
	return model.Capability{
		Schema:                 "haptic.device-capabilities/v1",
		DeviceID:               "dev-local-01",
		Channels:               []string{"ch_left", "ch_right"},
		MaxSamplesPerPacket:    8,
		SamplePeriodMS:         10,
		PacketWindowMS:         80,
		QuantumLevel:           4,
		MaxAmplitudeLevel:      120,
		TailSamples:            2,
		MaxWorkers:             2,
		PerThreadBufferSamples: 32,
	}
}

func CanonicalStream() model.Stream {
	left := "ch_left"
	right := "ch_right"
	events := []model.Event{
		{FrameNo: 1, ArrivalOrder: 1, EventID: "evt-01-opening-impact", Channel: left, TypeCode: 1,
			TypeSequence: []int{1, 101}, ChannelSequence: []string{left, left}, Strength: 88, DurationSamples: 4,
			StartSample: 0, Priority: 5, Template: "click", JitterAmplitude: 18,
			MergeKey: "opening", DesignerNote: "designed stereo transient; note is not on wire branches"},
		{FrameNo: 2, ArrivalOrder: 2, EventID: "evt-02-heavy-protected", Channel: left, TypeCode: 2,
			TypeSequence: []int{2, 102}, ChannelSequence: []string{left, left}, Strength: 104, DurationSamples: 6,
			StartSample: 2, Priority: 9, NoInterrupt: true, Template: "rumble", JitterAmplitude: 12},
		{FrameNo: 3, ArrivalOrder: 3, EventID: "evt-03-weak-collision", Channel: left, TypeCode: 1,
			TypeSequence: []int{1, 103}, ChannelSequence: []string{left, left}, Strength: 40, DurationSamples: 4,
			StartSample: 3, Priority: 2, Template: "click", JitterAmplitude: 8},
		{FrameNo: 4, ArrivalOrder: 4, EventID: "evt-04-swipe-a", Channel: right, TypeCode: 4,
			TypeSequence: []int{4, 204}, ChannelSequence: []string{right, right}, Strength: 60, DurationSamples: 5,
			StartSample: 4, Priority: 4, Template: "wave", JitterAmplitude: 10, MergeKey: "swipe-merge"},
		{FrameNo: 5, ArrivalOrder: 5, EventID: "evt-05-swipe-b", Channel: right, TypeCode: 4,
			TypeSequence: []int{4, 205}, ChannelSequence: []string{right, right}, Strength: 66, DurationSamples: 4,
			StartSample: 6, Priority: 4, Template: "wave", JitterAmplitude: 10, MergeKey: "swipe-merge"},
		{FrameNo: 6, ArrivalOrder: 6, EventID: "evt-06-conditional-on", Channel: left, TypeCode: 3,
			TypeSequence: []int{3, 306}, ChannelSequence: []string{left, left}, Strength: 56, DurationSamples: 3,
			StartSample: 8, Priority: 4, Conditional: true, Flag: true, Template: "wave", JitterAmplitude: 6},
		{FrameNo: 7, ArrivalOrder: 7, EventID: "evt-07-conditional-off", Channel: left, TypeCode: 3,
			TypeSequence: []int{3, 307}, ChannelSequence: []string{left, left}, Strength: 70, DurationSamples: 4,
			StartSample: 10, Priority: 8, Conditional: true, Flag: false, Template: "wave", JitterAmplitude: 6},
		{FrameNo: 8, ArrivalOrder: 8, EventID: "evt-08-parry-block", Channel: right, TypeCode: 5,
			TypeSequence: []int{5, 508}, ChannelSequence: []string{right, right}, Strength: 96, DurationSamples: 5,
			StartSample: 10, Priority: 8, NoInterrupt: true, Template: "rumble", JitterAmplitude: 9},
		{FrameNo: 9, ArrivalOrder: 9, EventID: "evt-09-conditional-on-right", Channel: right, TypeCode: 3,
			TypeSequence: []int{3, 309}, ChannelSequence: []string{right, right}, Strength: 50, DurationSamples: 3,
			StartSample: 16, Priority: 5, Conditional: true, Flag: true, Template: "click", JitterAmplitude: 5},
		{FrameNo: 10, ArrivalOrder: 10, EventID: "evt-10-late-low", Channel: right, TypeCode: 1,
			TypeSequence: []int{1, 110}, ChannelSequence: []string{right, right}, Strength: 42, DurationSamples: 4,
			StartSample: 17, Priority: 2, Template: "click", JitterAmplitude: 7},
	}
	return model.Stream{
		StreamID: "battle-interaction-fixed-243", Seed: DefaultSeed, InternalUnit: "device_sample",
		SamplePeriodMS: 10, Events: events,
	}
}

type TruthPoint struct {
	Channel     string `json:"channel"`
	SampleIndex int    `json:"sample_index"`
	FrameQ8     int64  `json:"frame_q8"`
	Level       int    `json:"level"`
}

type TruthPacket struct {
	GlobalSeq   int    `json:"global_seq"`
	Channel     string `json:"channel"`
	PacketIndex int    `json:"packet_index"`
	StartSample int    `json:"start_sample"`
	EndSample   int    `json:"end_sample"`
	SampleCount int    `json:"sample_count"`
	EventCount  int    `json:"event_count"`
	FrameDigest string `json:"frame_digest"`
}

type Truth struct {
	Schema              string           `json:"schema"`
	Seed                int64            `json:"seed"`
	SemanticFingerprint string           `json:"semantic_fingerprint"`
	TruthFingerprint    string           `json:"truth_fingerprint"`
	RuleVersion         string           `json:"rule_version"`
	AlgorithmVersion    string           `json:"algorithm_version"`
	ChannelLengths      map[string]int   `json:"channel_lengths"`
	Points              []TruthPoint     `json:"points"`
	SelectedPoints      []TruthPoint     `json:"selected_points"`
	Packets             []TruthPacket    `json:"packets"`
	PacketSampleCounts  map[string][]int `json:"packet_sample_counts"`
}

type Manifest struct {
	Seed                int64             `json:"seed"`
	Files               map[string]string `json:"files"`
	SemanticFingerprint string            `json:"semantic_fingerprint"`
	TruthFingerprint    string            `json:"truth_fingerprint"`
}

func channelLength(stream model.Stream, channel string, cap model.Capability) int {
	length := cap.TailSamples
	for _, ev := range stream.Events {
		for _, ch := range envelopeChannels(ev) {
			if ch == channel {
				if end := ev.StartSample + ev.DurationSamples + cap.TailSamples; end > length {
					length = end
				}
			}
		}
	}
	return length
}

func envelopeChannels(ev model.Event) []string {
	if len(ev.ChannelSequence) > 0 {
		return ev.ChannelSequence
	}
	return []string{ev.Channel}
}

func BuildTruth(stream model.Stream, cap model.Capability) (*Truth, error) {
	rules := defaultSnapshot(cap)
	truth := &Truth{
		Schema: "haptic.truth/v1", Seed: stream.Seed,
		SemanticFingerprint: stream.Fingerprint(), RuleVersion: model.RuleVersion,
		AlgorithmVersion: model.AlgorithmVersion, ChannelLengths: map[string]int{},
		PacketSampleCounts: map[string][]int{},
	}
	type planned struct {
		boundary.Boundary
		frames []model.Frame
	}
	allPlanned := []planned{}
	for _, ch := range model.SortedChannels(cap.Channels) {
		length := channelLength(stream, ch, cap)
		truth.ChannelLengths[ch] = length
		frames := reference.RecomputeChannel(stream.Events, stream.Seed, ch, rules, length)
		for i, frame := range frames {
			truth.Points = append(truth.Points, TruthPoint{ch, i, frame.FrameQ8, frame.Level})
		}
		selected := map[int]bool{0: true, 2: true, 4: true, 8: true, 10: true, 16: true}
		for i, frame := range frames {
			if selected[i] {
				truth.SelectedPoints = append(truth.SelectedPoints, TruthPoint{ch, i, frame.FrameQ8, frame.Level})
			}
		}
		bounds := boundary.PlanChannel(ch, length, cap)
		counts := []int{}
		for _, b := range bounds {
			pf := append([]model.Frame(nil), frames[b.StartSample:b.EndSample]...)
			tmpPkt := model.Packet{Channel: ch, StartSample: b.StartSample, EndSample: b.EndSample, Events: stream.Events}
			allPlanned = append(allPlanned, planned{Boundary: b, frames: pf})
			counts = append(counts, b.SampleCount)
			truth.Packets = append(truth.Packets, TruthPacket{
				Channel: ch, PacketIndex: b.PacketIndex, StartSample: b.StartSample, EndSample: b.EndSample,
				SampleCount: b.SampleCount, EventCount: reference.PacketEventCount(tmpPkt),
				FrameDigest: model.DigestFrames(pf),
			})
		}
		truth.PacketSampleCounts[ch] = counts
	}
	sortTruthPackets(truth.Packets)
	for i := range truth.Packets {
		truth.Packets[i].GlobalSeq = i
	}

	canonical, err := json.Marshal(truth)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	truth.TruthFingerprint = hex.EncodeToString(sum[:])
	return truth, nil
}

func sortTruthPackets(packets []TruthPacket) {
	for i := 0; i < len(packets); i++ {
		for j := i + 1; j < len(packets); j++ {
			if packetLess(packets[j].StartSample, packets[j].Channel, packets[j].PacketIndex,
				packets[i].StartSample, packets[i].Channel, packets[i].PacketIndex) {
				packets[i], packets[j] = packets[j], packets[i]
			}
		}
	}
}

func packetLess(as int, ac string, ai int, bs int, bc string, bi int) bool {
	if as != bs {
		return as < bs
	}
	if ac != bc {
		return ac < bc
	}
	return ai < bi
}

func defaultSnapshot(cap model.Capability) model.RuleSnapshot {
	return model.RuleSnapshot{
		RuleVersion: model.RuleVersion, AlgorithmVersion: model.AlgorithmVersion,
		SamplePeriodMS: cap.SamplePeriodMS, QuantumLevel: cap.QuantumLevel,
		MaxAmplitude: cap.MaxAmplitudeLevel, MaxSamplesPerPacket: cap.MaxSamplesPerPacket,
		PacketWindowMS: cap.PacketWindowMS, FixedScale: model.FixedScale,
		Templates: map[string][]int{
			"click":  {1000, 420, 180, 60, 0, 0, 0, 0},
			"wave":   {250, 600, 900, 1000, 900, 600, 250, 80},
			"rumble": {650, 900, 1000, 950, 900, 820, 720, 600},
		},
	}
}

func Generate(dir string) (*Manifest, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lossyCanonical := CanonicalStream()
	stream := CanonicalStream()
	for i := range stream.Events {
		stream.Events[i].DesignerNote = ""
	}
	capability := Capability()
	truth, err := BuildTruth(stream, capability)
	if err != nil {
		return nil, err
	}
	files := map[string]string{}

	capBytes, _ := json.MarshalIndent(capability, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "device_capabilities.json"), capBytes, 0o644); err != nil {
		return nil, err
	}
	files["device_capabilities.json"] = sha(capBytes)

	lossReport := map[string][]model.LossItem{}
	for branch := 1; branch <= 3; branch++ {
		res, err := branches.Export(lossyCanonical, branch)
		if err != nil {
			return nil, err
		}
		name := filepath.Join(dir, "branch_"+itoa(branch)+".json")
		if err := os.WriteFile(name, res.Data, 0o644); err != nil {
			return nil, err
		}
		files[filepath.Base(name)] = sha(res.Data)
		lossReport[itoa(branch)] = res.Losses
	}
	lossBytes, _ := json.MarshalIndent(lossReport, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "export_losses.json"), lossBytes, 0o644); err != nil {
		return nil, err
	}
	files["export_losses.json"] = sha(lossBytes)

	truthBytes, _ := json.MarshalIndent(truth, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), truthBytes, 0o644); err != nil {
		return nil, err
	}
	files["truth.json"] = sha(truthBytes)

	manifest := &Manifest{Seed: DefaultSeed, Files: files, SemanticFingerprint: truth.SemanticFingerprint, TruthFingerprint: truth.TruthFingerprint}
	manifestBytes, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestBytes, 0o644); err != nil {
		return nil, err
	}
	return manifest, nil
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for v > 0 {
		n--
		b[n] = byte('0' + v%10)
		v /= 10
	}
	return string(b[n:])
}
