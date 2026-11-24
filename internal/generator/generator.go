// Package generator constructs deterministic local fixtures. Truth values are
// derived from the generator's construction, never from the synthesizer.
package generator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"audioforge/internal/capability"
	"audioforge/internal/model"
	"audioforge/internal/semantics"
)

const (
	DefaultSeed = int64(424242)
	SampleRate  = int64(48000)
)

type TruthSample struct {
	Offset int64 `json:"offset"`
	Value  int64 `json:"value"`
}

type TruthChannel struct {
	Channel       int           `json:"channel"`
	FrameSamples  int           `json:"frame_samples"`
	Accepted      []string      `json:"accepted_events"`
	Dropped       []string      `json:"dropped_events"`
	Samples       []TruthSample `json:"samples"`
	SampleWindows []int64       `json:"sample_windows"`
}

type TruthPacket struct {
	Seq        int   `json:"seq"`
	Channel    int   `json:"channel"`
	Start      int64 `json:"start"`
	End        int64 `json:"end"`
	TimingMark int64 `json:"timing_mark"`
}

type Truth struct {
	Seed          int64           `json:"seed"`
	CapabilitySHA string          `json:"capability_version"`
	Conditions    map[string]bool `json:"conditions"`
	Channels      []TruthChannel  `json:"channels"`
	Packets       []TruthPacket   `json:"packets"`
}

type BranchStream struct {
	Spec model.BranchSpec
	File string
	Data []byte
}

type Bundle struct {
	Seed            int64
	Registry        *semantics.Registry
	CanonicalEvents []model.Event
	Capability      model.Capability
	CapabilityData  []byte
	Streams         []BranchStream
	Truth           Truth
	TruthData       []byte
}

func DefaultSpecs() []model.BranchSpec {
	return []model.BranchSpec{
		{
			Name:                    "arena-tick",
			TimeUnit:                model.TimeTick,
			TickRate:                SampleRate,
			TimeRounding:            model.RoundFloor,
			ValueSemantic:           model.SemanticIntensity,
			ListSemantic:            model.SemanticEventTypes,
			IntensityQuantumSamples: 16,
		},
		{
			Name:                    "mobile-ms",
			TimeUnit:                model.TimeMillisecond,
			TickRate:                SampleRate,
			TimeRounding:            model.RoundFloor,
			ValueSemantic:           model.SemanticDurationSamples,
			ListSemantic:            model.SemanticChannelRoutes,
			IntensityQuantumSamples: 16,
		},
		{
			Name:                    "console-tick",
			TimeUnit:                model.TimeTick,
			TickRate:                SampleRate,
			TimeRounding:            model.RoundFloor,
			ValueSemantic:           model.SemanticDurationSamples,
			ListSemantic:            model.SemanticChannelRoutes,
			IntensityQuantumSamples: 16,
		},
	}
}

func DefaultCapability() model.Capability {
	c := model.Capability{
		Version:      "local-device-v1",
		ChannelCount: 3,
		SampleRate:   SampleRate,
		Channels: []model.ChannelSpec{
			{Index: 0, Template: []int64{0, 250, 500, 750, 1000, 800, 600, 400, 200, 0}, GainPermille: 1000},
			{Index: 1, Template: []int64{0, 400, 800, 1000, 700, 400, 100, 0}, GainPermille: 900},
			{Index: 2, Template: []int64{200, 600, 1000, 600, 200}, GainPermille: 1100},
		},
		QuantUnit:        4,
		QuantRounding:    model.RoundNearest,
		MinEventInterval: 24,
		MaxPacketSamples: 96,
		TimingWindow:     model.TimingWindow{Min: 0, Max: 4096},
	}
	model.EnsureTemplateHashes(&c)
	return c
}

func Generate(seed int64) (*Bundle, error) {
	if seed < 0 {
		seed = -seed
	}
	registry, err := semantics.NewRegistry(DefaultSpecs()...)
	if err != nil {
		return nil, err
	}
	capacity := DefaultCapability()
	events := canonicalEvents(seed, capacity)
	bundle := &Bundle{
		Seed:            seed,
		Registry:        registry,
		CanonicalEvents: events,
		Capability:      capacity,
	}
	capData, err := capability.Encode(capacity)
	if err != nil {
		return nil, err
	}
	bundle.CapabilityData = capData
	for _, spec := range DefaultSpecs() {
		data, err := semantics.EncodeStreamAtRate(spec, events, capacity.SampleRate)
		if err != nil {
			return nil, err
		}
		bundle.Streams = append(bundle.Streams, BranchStream{
			Spec: spec,
			File: spec.Name + ".events.json",
			Data: data,
		})
	}
	truth, err := buildTruth(seed, capacity, events)
	if err != nil {
		return nil, err
	}
	bundle.Truth = truth
	truthData, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, err
	}
	bundle.TruthData = truthData
	return bundle, nil
}

func (b *Bundle) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create sample directory: %w", err)
	}
	for _, stream := range b.Streams {
		if err := os.WriteFile(filepath.Join(dir, stream.File), stream.Data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", stream.File, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "capability.json"), b.CapabilityData, 0o644); err != nil {
		return fmt.Errorf("write capability: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), b.TruthData, 0o644); err != nil {
		return fmt.Errorf("write truth: %w", err)
	}
	return nil
}

func (b *Bundle) LoadStreams() ([]model.Stream, error) {
	streams := make([]model.Stream, 0, len(b.Streams))
	for _, stream := range b.Streams {
		loaded, err := semantics.DecodeWithSampleRate(stream.Data, stream.File, b.Capability.SampleRate, b.Registry)
		if err != nil {
			return nil, err
		}
		streams = append(streams, loaded)
	}
	sort.Slice(streams, func(i, j int) bool { return streams[i].Spec.Name < streams[j].Spec.Name })
	return streams, nil
}

func (b *Bundle) SelectStream(name string) (model.Stream, error) {
	streams, err := b.LoadStreams()
	if err != nil {
		return model.Stream{}, err
	}
	for _, stream := range streams {
		if stream.Spec.Name == name {
			return stream, nil
		}
	}
	return model.Stream{}, fmt.Errorf("stream %q not generated", name)
}

func canonicalEvents(seed int64, cap model.Capability) []model.Event {
	type eventSeed struct {
		id       string
		start    int64
		duration int64
		priority int
		blocking bool
		cond     bool
		gate     bool
		routes   []int
		types    []string
	}
	raw := []eventSeed{
		{id: "evt-001", start: 0, duration: 240, priority: 2, routes: []int{0}, types: []string{"impact", "combat"}},
		{id: "evt-002", start: 96, duration: 192, priority: 4, routes: []int{0}, types: []string{"critical", "combat"}},
		{id: "evt-003", start: 144, duration: 144, priority: 3, routes: []int{0}, types: []string{"impact"}},
		{id: "evt-004", start: 336, duration: 240, priority: 2, blocking: true, routes: []int{0}, types: []string{"guard"}},
		{id: "evt-005", start: 384, duration: 96, priority: 9, routes: []int{0}, types: []string{"burst"}},
		{id: "evt-006", start: 576, duration: 192, priority: 1, routes: []int{0}, types: []string{"footstep"}},
		{id: "evt-007", start: 48, duration: 288, priority: 5, routes: []int{1}, types: []string{"impact", "metal"}},
		{id: "evt-008", start: 192, duration: 144, priority: 5, cond: true, gate: false, routes: []int{1}, types: []string{"voice"}},
		{id: "evt-009", start: 384, duration: 192, priority: 1, routes: []int{1}, types: []string{"interaction"}},
		{id: "evt-010", start: 96, duration: 288, priority: 2, routes: []int{2}, types: []string{"ambient"}},
		{id: "evt-011", start: 432, duration: 192, priority: 2, cond: true, gate: true, routes: []int{2}, types: []string{"ambient", "combat"}},
		{id: "evt-012", start: 576, duration: 144, priority: 8, routes: []int{2}, types: []string{"critical"}},
		{id: "evt-013", start: 672, duration: 192, priority: 3, routes: []int{0, 2}, types: []string{"impact", "metal"}},
		{id: "evt-014", start: 720, duration: 192, priority: 4, routes: []int{1}, types: []string{"interaction"}},
	}
	out := make([]model.Event, 0, len(raw))
	for i, item := range raw {
		intensity := 60 + positiveMod(seed+int64(i*37), 141)
		intensity = (intensity / 10) * 10
		if intensity < 20 {
			intensity = 20
		}
		out = append(out, model.Event{
			ID:              item.id,
			SourceOrdinal:   i,
			StartSample:     item.start,
			Priority:        item.priority,
			Blocking:        item.blocking,
			Conditional:     item.cond,
			Gate:            item.gate,
			Active:          !item.cond || item.gate,
			Intensity:       intensity,
			DurationSamples: item.duration,
			EventTypes:      append([]string(nil), item.types...),
			Routes:          append([]int(nil), item.routes...),
		})
	}
	return out
}

func positiveMod(value, mod int64) int64 {
	if mod <= 0 {
		return 0
	}
	r := value % mod
	if r < 0 {
		r += mod
	}
	return r
}

type expectedSpan struct {
	id        string
	start     int64
	end       int64
	priority  int
	ordinal   int
	blocking  bool
	intensity int64
	duration  int64
}

func buildTruth(seed int64, cap model.Capability, events []model.Event) (Truth, error) {
	truth := Truth{
		Seed:          seed,
		CapabilitySHA: cap.Version,
		Conditions:    make(map[string]bool, len(events)),
	}
	for _, event := range events {
		truth.Conditions[event.ID] = event.Active
	}
	for channel := 0; channel < cap.ChannelCount; channel++ {
		accepted, dropped := expectedResolve(cap, events, channel)
		frameEnd := int64(0)
		for _, span := range accepted {
			if span.end > frameEnd {
				frameEnd = span.end
			}
		}
		truthChannel := TruthChannel{
			Channel:      channel,
			FrameSamples: int(frameEnd),
			Accepted:     make([]string, 0, len(accepted)),
			Dropped:      make([]string, 0, len(dropped)),
		}
		for _, span := range accepted {
			truthChannel.Accepted = append(truthChannel.Accepted, span.id)
		}
		for _, id := range dropped {
			truthChannel.Dropped = append(truthChannel.Dropped, id)
		}
		points := samplePoints(cap, frameEnd)
		for _, offset := range points {
			truthChannel.Samples = append(truthChannel.Samples, TruthSample{
				Offset: offset,
				Value:  expectedSample(cap, channel, offset, accepted),
			})
		}
		truthChannel.SampleWindows = points
		truth.Channels = append(truth.Channels, truthChannel)
		for seq, start := int(0), int64(0); start < frameEnd; seq++ {
			end := start + cap.MaxPacketSamples
			if end > frameEnd {
				end = frameEnd
			}
			truth.Packets = append(truth.Packets, TruthPacket{
				Seq:        seq,
				Channel:    channel,
				Start:      start,
				End:        end,
				TimingMark: start,
			})
			start = end
		}
	}
	return truth, nil
}

func expectedResolve(cap model.Capability, events []model.Event, channel int) ([]expectedSpan, []string) {
	candidates := make([]expectedSpan, 0)
	for _, event := range events {
		if !containsInt(event.Routes, channel) {
			continue
		}
		if !event.Active {
			continue
		}
		candidates = append(candidates, expectedSpan{
			id:        event.ID,
			start:     event.StartSample,
			end:       event.StartSample + event.DurationSamples,
			priority:  event.Priority,
			ordinal:   event.SourceOrdinal,
			blocking:  event.Blocking,
			intensity: event.Intensity,
			duration:  event.DurationSamples,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].start != candidates[j].start {
			return candidates[i].start < candidates[j].start
		}
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority > candidates[j].priority
		}
		if candidates[i].ordinal != candidates[j].ordinal {
			return candidates[i].ordinal < candidates[j].ordinal
		}
		return candidates[i].id < candidates[j].id
	})
	accepted := make([]expectedSpan, 0)
	dropped := make([]string, 0)
	current := -1
	for _, candidate := range candidates {
		if current >= 0 && candidate.start >= accepted[current].end {
			current = -1
		}
		if current < 0 {
			accepted = append(accepted, candidate)
			current = len(accepted) - 1
			continue
		}
		active := &accepted[current]
		tooSoon := candidate.start < active.start+cap.MinEventInterval
		switch {
		case active.blocking:
			dropped = append(dropped, candidate.id)
		case tooSoon && candidate.priority <= active.priority:
			dropped = append(dropped, candidate.id)
		case candidate.priority > active.priority:
			active.end = candidate.start
			accepted = append(accepted, candidate)
			current = len(accepted) - 1
		default:
			dropped = append(dropped, candidate.id)
		}
	}
	return accepted, dropped
}

func expectedSample(cap model.Capability, channel int, offset int64, accepted []expectedSpan) int64 {
	var raw int64
	for _, span := range accepted {
		if offset >= span.start && offset < span.end {
			template := cap.Channels[channel].Template
			pos := offset - span.start
			idx := int64(0)
			if span.duration > 1 {
				idx = pos * int64(len(template)-1) / (span.duration - 1)
			} else {
				idx = int64((len(template) - 1) / 2)
			}
			if idx < 0 {
				idx = 0
			}
			if idx >= int64(len(template)) {
				idx = int64(len(template) - 1)
			}
			raw += template[idx] * span.intensity / 1000
		}
	}
	raw = raw * cap.Channels[channel].GainPermille / 1000
	if cap.QuantUnit <= 1 {
		return raw
	}
	switch cap.QuantRounding {
	case model.RoundFloor:
		return floorDiv(raw, cap.QuantUnit)
	case model.RoundCeil:
		return ceilDiv(raw, cap.QuantUnit)
	default:
		return (raw + cap.QuantUnit/2) / cap.QuantUnit
	}
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func ceilDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) == (b < 0) {
		q++
	}
	return q
}

func samplePoints(cap model.Capability, frameEnd int64) []int64 {
	seen := map[int64]bool{0: true}
	var points []int64
	for start := int64(0); start < frameEnd; start += cap.MaxPacketSamples {
		seen[start] = true
		if start+24 < frameEnd {
			seen[start+24] = true
		}
		if start+cap.MaxPacketSamples-1 < frameEnd {
			seen[start+cap.MaxPacketSamples-1] = true
		}
	}
	if frameEnd > 0 {
		seen[frameEnd-1] = true
	}
	for point := range seen {
		points = append(points, point)
	}
	sort.Slice(points, func(i, j int) bool { return points[i] < points[j] })
	return points
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// EnvelopeDigest exposes a compact digest for reports without using truth
// values to synthesize output.
func EnvelopeDigest(points []TruthSample) string {
	var out string
	for _, point := range points {
		out += strconv.FormatInt(point.Offset, 10) + ":" + strconv.FormatInt(point.Value, 10) + ";"
	}
	return out
}
