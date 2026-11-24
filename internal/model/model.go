// Package model contains the branch-neutral domain model shared by the loader,
// synthesizer, packetizer, persistence layer, and device simulator.
package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type Rounding string

const (
	RoundFloor   Rounding = "floor"
	RoundCeil    Rounding = "ceil"
	RoundNearest Rounding = "nearest"
)

type TimeUnit string

const (
	TimeTick        TimeUnit = "tick"
	TimeMillisecond TimeUnit = "ms"
)

type ValueSemantic string

const (
	SemanticIntensity       ValueSemantic = "intensity"
	SemanticDurationSamples ValueSemantic = "duration_samples"
)

type ListSemantic string

const (
	SemanticEventTypes    ListSemantic = "event_types"
	SemanticChannelRoutes ListSemantic = "channel_routes"
)

// BranchSpec describes the field semantics of one release branch.
type BranchSpec struct {
	Name                    string        `json:"name"`
	TimeUnit                TimeUnit      `json:"time_unit"`
	TickRate                int64         `json:"tick_rate,omitempty"`
	TimeRounding            Rounding      `json:"time_rounding"`
	ValueSemantic           ValueSemantic `json:"value_semantic"`
	ListSemantic            ListSemantic  `json:"list_semantic"`
	IntensityQuantumSamples int64         `json:"intensity_quantum_samples"`
}

func (s BranchSpec) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("branch name is empty")
	}
	if s.TimeUnit != TimeTick && s.TimeUnit != TimeMillisecond {
		return fmt.Errorf("unsupported time unit %q", s.TimeUnit)
	}
	if s.TimeUnit == TimeTick && s.TickRate <= 0 {
		return fmt.Errorf("tick rate must be positive")
	}
	switch s.TimeRounding {
	case RoundFloor, RoundCeil, RoundNearest:
	default:
		return fmt.Errorf("unsupported rounding %q", s.TimeRounding)
	}
	if s.ValueSemantic != SemanticIntensity && s.ValueSemantic != SemanticDurationSamples {
		return fmt.Errorf("unsupported value semantic %q", s.ValueSemantic)
	}
	if s.ListSemantic != SemanticEventTypes && s.ListSemantic != SemanticChannelRoutes {
		return fmt.Errorf("unsupported list semantic %q", s.ListSemantic)
	}
	if s.IntensityQuantumSamples <= 0 {
		return fmt.Errorf("intensity quantum samples must be positive")
	}
	return nil
}

// SourcePaths maps internal field names back to their source JSON paths.
type SourcePaths map[string]string

// Event is the branch-neutral, sample-domain event representation.
type Event struct {
	ID              string      `json:"id"`
	SourceBranch    string      `json:"source_branch"`
	SourceOrdinal   int         `json:"source_ordinal"`
	StartSample     int64       `json:"start_sample"`
	Priority        int         `json:"priority"`
	Blocking        bool        `json:"blocking"`
	Conditional     bool        `json:"conditional"`
	Gate            bool        `json:"gate"`
	Active          bool        `json:"active"`
	Intensity       int64       `json:"intensity"`
	DurationSamples int64       `json:"duration_samples"`
	EventTypes      []string    `json:"event_types"`
	Routes          []int       `json:"routes"`
	SourcePaths     SourcePaths `json:"source_paths"`
}

func (e Event) EndSample() int64 { return e.StartSample + e.DurationSamples }

func (e Event) Clone() Event {
	c := e
	c.EventTypes = append([]string(nil), e.EventTypes...)
	c.Routes = append([]int(nil), e.Routes...)
	c.SourcePaths = make(SourcePaths, len(e.SourcePaths))
	for k, v := range e.SourcePaths {
		c.SourcePaths[k] = v
	}
	return c
}

// CanonicalEvent strips source-only metadata for cross-branch equivalence.
func (e Event) CanonicalEvent() Event {
	c := e.Clone()
	c.SourceBranch = ""
	c.SourceOrdinal = 0
	c.SourcePaths = nil
	sort.Strings(c.EventTypes)
	sort.Ints(c.Routes)
	return c
}

func CanonicalEqual(a, b Event) bool {
	left, err := json.Marshal(a.CanonicalEvent())
	if err != nil {
		return false
	}
	right, err := json.Marshal(b.CanonicalEvent())
	if err != nil {
		return false
	}
	return string(left) == string(right)
}

type Stream struct {
	Spec       BranchSpec `json:"spec"`
	SourceFile string     `json:"source_file"`
	Events     []Event    `json:"events"`
}

type TimingWindow struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

type ChannelSpec struct {
	Index        int     `json:"index"`
	Template     []int64 `json:"template"`
	TemplateHash string  `json:"template_hash"`
	GainPermille int64   `json:"gain_permille"`
}

type Capability struct {
	Version          string        `json:"version"`
	ChannelCount     int           `json:"channel_count"`
	SampleRate       int64         `json:"sample_rate"`
	Channels         []ChannelSpec `json:"channels"`
	QuantUnit        int64         `json:"quant_unit"`
	QuantRounding    Rounding      `json:"quant_rounding"`
	MinEventInterval int64         `json:"min_event_interval"`
	MaxPacketSamples int64         `json:"max_packet_samples"`
	TimingWindow     TimingWindow  `json:"timing_window"`
}

func (c Capability) Validate() error {
	if c.ChannelCount <= 0 {
		return fmt.Errorf("channel count must be positive")
	}
	if len(c.Channels) != c.ChannelCount {
		return fmt.Errorf("channel count %d does not match %d channel templates", c.ChannelCount, len(c.Channels))
	}
	if c.SampleRate <= 0 {
		return fmt.Errorf("sample rate must be positive")
	}
	if c.QuantUnit <= 0 {
		return fmt.Errorf("quantization unit must be positive")
	}
	switch c.QuantRounding {
	case RoundFloor, RoundCeil, RoundNearest:
	default:
		return fmt.Errorf("unsupported quantization rounding %q", c.QuantRounding)
	}
	if c.MinEventInterval < 0 {
		return fmt.Errorf("minimum event interval must not be negative")
	}
	if c.MaxPacketSamples <= 0 {
		return fmt.Errorf("maximum packet samples must be positive")
	}
	if c.TimingWindow.Min > c.TimingWindow.Max {
		return fmt.Errorf("timing window min exceeds max")
	}
	for i, ch := range c.Channels {
		if ch.Index != i {
			return fmt.Errorf("channel %d has index %d", i, ch.Index)
		}
		if len(ch.Template) == 0 {
			return fmt.Errorf("channel %d has an empty envelope template", i)
		}
		if ch.GainPermille < 0 {
			return fmt.Errorf("channel %d has a negative gain", i)
		}
		for j, v := range ch.Template {
			if v < 0 {
				return fmt.Errorf("channel %d template point %d is negative", i, j)
			}
		}
	}
	return nil
}

func HashInt64Slice(values []int64) string {
	h := sha256.New()
	var buf [8]byte
	for _, v := range values {
		binary.BigEndian.PutUint64(buf[:], uint64(v))
		_, _ = h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func EnsureTemplateHashes(c *Capability) {
	for i := range c.Channels {
		c.Channels[i].TemplateHash = HashInt64Slice(c.Channels[i].Template)
	}
}

type EventSpan struct {
	EventID  string `json:"event_id"`
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
	Priority int    `json:"priority"`
}

type EventOffset struct {
	EventID string `json:"event_id"`
	Offset  int64  `json:"offset"`
}

type EnvelopeFrame struct {
	Channel int         `json:"channel"`
	Samples []int64     `json:"samples"`
	Spans   []EventSpan `json:"spans"`
}

func (f EnvelopeFrame) Clone() EnvelopeFrame {
	return EnvelopeFrame{
		Channel: f.Channel,
		Samples: append([]int64(nil), f.Samples...),
		Spans:   append([]EventSpan(nil), f.Spans...),
	}
}

type Packet struct {
	Seq             int      `json:"seq"`
	Channel         int      `json:"channel"`
	StartSample     int64    `json:"start_sample"`
	SampleCount     int      `json:"sample_count"`
	TimingMark      int64    `json:"timing_mark"`
	PreviousOffset  int64    `json:"previous_offset"`
	Samples         []int64  `json:"samples"`
	DependsOnEvents []string `json:"depends_on_events"`
	ConsumedOffsets []int64  `json:"consumed_offsets"`
	ContentHash     string   `json:"content_hash"`
}

func (p Packet) EndSample() int64 { return p.StartSample + int64(len(p.Samples)) }

func (p Packet) Clone() Packet {
	p.Samples = append([]int64(nil), p.Samples...)
	p.DependsOnEvents = append([]string(nil), p.DependsOnEvents...)
	p.ConsumedOffsets = append([]int64(nil), p.ConsumedOffsets...)
	return p
}

// HashPacket returns a stable digest of all decision-relevant packet fields.
func HashPacket(p Packet) string {
	h := sha256.New()
	var buf [8]byte
	writeI64 := func(v int64) {
		binary.BigEndian.PutUint64(buf[:], uint64(v))
		_, _ = h.Write(buf[:])
	}
	writeU32 := func(v uint32) {
		binary.BigEndian.PutUint32(buf[:4], v)
		_, _ = h.Write(buf[:4])
	}
	writeU32(1)
	writeI64(int64(p.Seq))
	writeI64(int64(p.Channel))
	writeI64(p.StartSample)
	writeI64(int64(p.SampleCount))
	writeI64(p.TimingMark)
	writeI64(p.PreviousOffset)
	writeI64(int64(len(p.Samples)))
	for _, v := range p.Samples {
		writeI64(v)
	}
	writeI64(int64(len(p.DependsOnEvents)))
	for _, id := range p.DependsOnEvents {
		writeU32(uint32(len(id)))
		_, _ = h.Write([]byte(id))
	}
	writeI64(int64(len(p.ConsumedOffsets)))
	for _, v := range p.ConsumedOffsets {
		writeI64(v)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (p *Packet) RefreshHash() {
	p.SampleCount = len(p.Samples)
	p.ContentHash = HashPacket(*p)
}

type PacketDecision struct {
	EventID string `json:"event_id"`
	Channel int    `json:"channel"`
	Action  string `json:"action"`
	Reason  string `json:"reason"`
}

type CommitRecord struct {
	WindowIndex      int      `json:"window_index"`
	Packets          []Packet `json:"packets"`
	ParamVersion     string   `json:"param_version"`
	AlgorithmVersion string   `json:"algorithm_version"`
	CommittedAt      string   `json:"committed_at"`
}

type BuildParameters struct {
	ParamVersion     string `json:"param_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	CommitWindow     int    `json:"commit_window"`
	Workers          int    `json:"workers"`
	BufferPerWorker  int    `json:"buffer_per_worker"`
}

func (p BuildParameters) Validate() error {
	if p.ParamVersion == "" {
		return fmt.Errorf("parameter version is empty")
	}
	if p.AlgorithmVersion == "" {
		return fmt.Errorf("algorithm version is empty")
	}
	if p.CommitWindow <= 0 {
		return fmt.Errorf("commit window must be positive")
	}
	if p.Workers <= 0 {
		return fmt.Errorf("workers must be positive")
	}
	if p.BufferPerWorker <= 0 {
		return fmt.Errorf("buffer per worker must be positive")
	}
	return nil
}

func SortedEventOffsets(spans []EventSpan, start, end int64) ([]string, []int64) {
	seen := make(map[string]int64)
	for _, span := range spans {
		if span.Start < end && span.End > start {
			if prior, ok := seen[span.EventID]; !ok || span.Start < prior {
				seen[span.EventID] = span.Start
			}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	offsets := make([]int64, 0, len(ids))
	for _, id := range ids {
		offsets = append(offsets, seen[id])
	}
	return ids, offsets
}
