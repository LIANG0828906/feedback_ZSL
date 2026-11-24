package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"runtime"

	"spatialworkbench/internal/werr"
)

const (
	FormatVersion    = 1
	AlgorithmVersion = "spatial-reconstruct-2026.11.1"
	MinSampleRate    = 8000
	MaxSampleRate    = 192000
)

type Manifest struct {
	FormatVersion int      `json:"formatVersion"`
	Generator     Metadata `json:"generator"`
	Samples       []Sample `json:"samples"`
}

type Metadata struct {
	Name    string `json:"name"`
	Seed    int64  `json:"seed"`
	Build   string `json:"build"`
	Created string `json:"created"`
}

type Sample struct {
	ID         string    `json:"id"`
	Seed       int64     `json:"seed"`
	SampleRate int       `json:"sampleRate"`
	Layout     Layout    `json:"layout"`
	IRSamples  int       `json:"irSamples"`
	Channels   []Channel `json:"channels"`
	Dry        Dry       `json:"dry"`
}

type Layout struct {
	Name     string   `json:"name"`
	Channels []string `json:"channels"`
}

type Channel struct {
	ID      string       `json:"id"`
	Samples []float64    `json:"samples"`
	Truth   ChannelTruth `json:"truth"`
}

type ChannelTruth struct {
	DirectArrival int          `json:"directArrival"`
	DirectEnergy  float64      `json:"directEnergy"`
	Reflections   []Reflection `json:"reflections"`
	DRRDB         float64      `json:"drrDb"`
	ITDSamples    int          `json:"itdSamples"`
	TailEnergy    float64      `json:"tailEnergy"`
}

type Reflection struct {
	Arrival int     `json:"arrival"`
	Energy  float64 `json:"energy"`
}

type Dry struct {
	Description string    `json:"description"`
	Samples     []float64 `json:"samples"`
}

// Params are immutable for a checkpoint generation. Frame-specific snapshots
// are stored with each committed frame.
type Params struct {
	FrameLength                int                `json:"frameLength"`
	Hop                        int                `json:"hop"`
	WindowLength               int                `json:"windowLength"`
	QuantStep                  int                `json:"quantStep"`
	InterpOrder                int                `json:"interpOrder"`
	RenderGain                 float64            `json:"renderGain"`
	ChannelGain                map[string]float64 `json:"channelGain,omitempty"`
	Routing                    map[string]int     `json:"routing,omitempty"`
	ErrorTolerance             float64            `json:"errorTolerance"`
	MaxWorkers                 int                `json:"maxWorkers"`
	BufferSamplesPerWorker     int                `json:"bufferSamplesPerWorker"`
	EarlyEnergySmoothingWeight float64            `json:"earlyEnergySmoothingWeight"`
}

func DefaultParams(sample Sample) Params {
	workers := runtime.GOMAXPROCS(0)
	if workers > 4 {
		workers = 4
	}
	if workers < 1 {
		workers = 1
	}
	return Params{
		FrameLength:                512,
		Hop:                        256,
		WindowLength:               192,
		QuantStep:                  8,
		InterpOrder:                3,
		RenderGain:                 0.85,
		ChannelGain:                map[string]float64{},
		Routing:                    IdentityRouting(sample.Layout),
		ErrorTolerance:             4096,
		MaxWorkers:                 workers,
		BufferSamplesPerWorker:     65536,
		EarlyEnergySmoothingWeight: 0.25,
	}
}

func IdentityRouting(layout Layout) map[string]int {
	routing := make(map[string]int, len(layout.Channels))
	for i, id := range layout.Channels {
		routing[id] = i
	}
	return routing
}

func (p Params) Clone() Params {
	out := p
	out.ChannelGain = cloneFloatMap(p.ChannelGain)
	out.Routing = cloneIntMap(p.Routing)
	return out
}

func (p Params) Normalized(sample Sample) Params {
	out := p.Clone()
	if out.Routing == nil {
		out.Routing = IdentityRouting(sample.Layout)
	}
	if out.ChannelGain == nil {
		out.ChannelGain = map[string]float64{}
	}
	return out
}

func (p Params) Validate(sample Sample) *werr.Error {
	if p.FrameLength <= 0 || p.FrameLength > 1<<20 {
		return fieldPara("frameLength", "must be in [1,1048576], got %d", p.FrameLength)
	}
	if sample.IRSamples > 0 && p.FrameLength > sample.IRSamples {
		return fieldPara("frameLength", "must not exceed impulse-response length %d, got %d", sample.IRSamples, p.FrameLength)
	}
	if p.Hop <= 0 {
		return fieldPara("hop", "must be positive, got %d", p.Hop)
	}
	if p.Hop > p.FrameLength {
		return fieldPara("hop", "must not exceed frameLength (%d), got %d", p.FrameLength, p.Hop)
	}
	if p.WindowLength <= 0 {
		return fieldPara("windowLength", "must be positive, got %d", p.WindowLength)
	}
	if p.WindowLength > p.FrameLength {
		return fieldPara("windowLength", "must not exceed frameLength (%d), got %d", p.FrameLength, p.WindowLength)
	}
	if p.QuantStep <= 0 || p.QuantStep > p.FrameLength {
		return fieldPara("quantStep", "must be in [1,frameLength=%d], got %d", p.FrameLength, p.QuantStep)
	}
	if p.InterpOrder < 1 || p.InterpOrder > 5 {
		return fieldPara("interpOrder", "must be in [1,5], got %d", p.InterpOrder)
	}
	if p.MaxWorkers <= 0 || p.MaxWorkers > 64 {
		return fieldPara("maxWorkers", "must be in [1,64], got %d", p.MaxWorkers)
	}
	if p.BufferSamplesPerWorker <= 0 {
		return fieldPara("bufferSamplesPerWorker", "must be positive, got %d", p.BufferSamplesPerWorker)
	}
	if math.IsNaN(p.ErrorTolerance) || math.IsInf(p.ErrorTolerance, 0) || p.ErrorTolerance < 0 {
		return fieldPara("errorTolerance", "must be a finite non-negative value, got %v", p.ErrorTolerance)
	}
	if math.IsNaN(p.EarlyEnergySmoothingWeight) || p.EarlyEnergySmoothingWeight < 0 || p.EarlyEnergySmoothingWeight > 1 {
		return fieldPara("earlyEnergySmoothingWeight", "must be in [0,1], got %v", p.EarlyEnergySmoothingWeight)
	}
	if len(p.Routing) == 0 {
		return NewPara("routing", "requires one entry per source channel")
	}
	seenTargets := make(map[int]string, len(p.Routing))
	for _, ch := range sample.Channels {
		target, ok := p.Routing[ch.ID]
		if !ok {
			e := werr.New(werr.CodeLayout, "routing is missing source channel %q", ch.ID)
			e.Field = "routing"
			e.Channel = ch.ID
			return e
		}
		if target < 0 || target >= len(sample.Layout.Channels) {
			e := werr.New(werr.CodeLayout, "routing target %d is outside output layout [0,%d)", target, len(sample.Layout.Channels))
			e.Field = "routing"
			e.Channel = ch.ID
			e.Actual = target
			return e
		}
		if other, exists := seenTargets[target]; exists {
			e := werr.New(werr.CodeLayout, "source channels %q and %q share output channel %d", other, ch.ID, target)
			e.Field = "routing"
			e.Channel = ch.ID
			return e
		}
		seenTargets[target] = ch.ID
	}
	if math.IsNaN(p.RenderGain) || math.IsInf(p.RenderGain, 0) || p.RenderGain < 0 || p.RenderGain > 1 {
		e := werr.New(werr.CodeClip, "render gain is outside safe range [0,1]: %v", p.RenderGain)
		e.Field = "renderGain"
		e.Frame = 0
		if len(sample.Channels) > 0 {
			e.Channel = sample.Channels[0].ID
		}
		e.Actual = p.RenderGain
		return e
	}
	for id, gain := range p.ChannelGain {
		if math.IsNaN(gain) || math.IsInf(gain, 0) || gain < 0 || gain > 1 {
			e := werr.New(werr.CodeClip, "channel gain for %q is outside safe range [0,1]: %v", id, gain)
			e.Field = "channelGain"
			e.Frame = 0
			e.Channel = id
			e.Actual = gain
			return e
		}
	}
	if len(sample.Dry.Samples) == 0 {
		return werr.Decode("dry.samples must not be empty")
	}
	if len(sample.Channels) == 0 {
		return werr.Decode("channels must not be empty")
	}
	return nil
}

func fieldPara(field, format string, args ...any) *werr.Error {
	e := werr.Para(format, args...)
	e.Field = field
	return e
}

func NewPara(field, format string, args ...any) *werr.Error {
	return fieldPara(field, format, args...)
}

func (p Params) TotalFrames(drySamples int) int {
	if p.Hop <= 0 || drySamples <= 0 {
		return 0
	}
	return (drySamples + p.Hop - 1) / p.Hop
}

func (p Params) Hash(sample Sample) string {
	n := p.Normalized(sample)
	payload := struct {
		Params Params `json:"params"`
		Layout Layout `json:"layout"`
	}{Params: n, Layout: sample.Layout}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p Params) Version(sample Sample) string {
	return "p1:" + p.Hash(sample)[:24]
}

func SampleHash(sample Sample) string {
	b, _ := json.Marshal(sample)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func LayoutHash(layout Layout) string {
	b, _ := json.Marshal(layout)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s Sample) Channel(id string) (Channel, int, bool) {
	for i, ch := range s.Channels {
		if ch.ID == id {
			return ch, i, true
		}
	}
	return Channel{}, -1, false
}

func (s Sample) Clone() Sample {
	out := s
	out.Layout.Channels = append([]string(nil), s.Layout.Channels...)
	out.Channels = make([]Channel, len(s.Channels))
	for i, ch := range s.Channels {
		out.Channels[i] = ch
		out.Channels[i].Samples = append([]float64(nil), ch.Samples...)
		out.Channels[i].Truth.Reflections = append([]Reflection(nil), ch.Truth.Reflections...)
	}
	out.Dry.Samples = append([]float64(nil), s.Dry.Samples...)
	return out
}

func cloneFloatMap(in map[string]float64) map[string]float64 {
	if in == nil {
		return nil
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneIntMap(in map[string]int) map[string]int {
	if in == nil {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func FormatFloat(v float64) string {
	return fmt.Sprintf("%.9g", v)
}
