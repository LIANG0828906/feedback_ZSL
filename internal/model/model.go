package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	CacheKeyAlgorithm = "layerforge-cache-v1"
	CheckpointVersion = "layerforge-checkpoint/v1"
)

type FileInput struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type Layer struct {
	ID        string `json:"id"`
	SizeBytes int64  `json:"sizeBytes"`
}

type BaseLayer struct {
	ID        string `json:"id"`
	SizeBytes int64  `json:"sizeBytes"`
	Digest    string `json:"digest"`
}

type Step struct {
	Number             int         `json:"number"`
	Name               string      `json:"name"`
	Inputs             []FileInput `json:"inputs"`
	CommandFingerprint string      `json:"commandFingerprint"`
	DependsOn          []string    `json:"dependsOn"`
	Output             Layer       `json:"output"`
}

func (s Step) MinimumMemoryBytes() int64 {
	// A layer is modelled as a buffered artifact. The generated layers keep
	// this value well below the default per-thread budget.
	min := s.Output.SizeBytes / 2
	if min < 128 {
		min = 128
	}
	return min
}

func (s Step) BuildCost() time.Duration {
	// Costs are derived only from declared output size. No wall-clock timing,
	// worker count or load order can affect the schedule cost.
	units := (s.Output.SizeBytes + 4095) / 4096
	if units < 1 {
		units = 1
	}
	return time.Duration(2+2*units) * time.Millisecond
}

type Share struct {
	LayerID   string `json:"layer"`
	Consumers []int  `json:"consumers"`
}

type Budget struct {
	Latency         time.Duration `json:"latency"`
	MaxRounds       int           `json:"maxRounds"`
	ThreadLimit     int           `json:"threadLimit"`
	MemoryPerThread int64         `json:"memoryPerThreadBytes"`
	BufferPerThread int           `json:"bufferPerThread"`
}

type Project struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Seed   int64       `json:"seed"`
	Bases  []BaseLayer `json:"bases"`
	Steps  []Step      `json:"steps"`
	Shares []Share     `json:"shares"`
	Budget Budget      `json:"budget"`
}

func (p Project) Clone() Project {
	out := p
	out.Bases = append([]BaseLayer(nil), p.Bases...)
	out.Steps = make([]Step, len(p.Steps))
	for i := range p.Steps {
		out.Steps[i] = p.Steps[i]
		out.Steps[i].Inputs = append([]FileInput(nil), p.Steps[i].Inputs...)
		out.Steps[i].DependsOn = append([]string(nil), p.Steps[i].DependsOn...)
	}
	out.Shares = make([]Share, len(p.Shares))
	for i := range p.Shares {
		out.Shares[i] = p.Shares[i]
		out.Shares[i].Consumers = append([]int(nil), p.Shares[i].Consumers...)
	}
	return out
}

type StepSnapshot struct {
	Inputs             []FileInput
	CommandFingerprint string
	DependsOn          []string
	Output             Layer
}

func (p Project) StepSnapshots() map[int]StepSnapshot {
	out := make(map[int]StepSnapshot, len(p.Steps))
	for _, step := range p.Steps {
		out[step.Number] = StepSnapshot{
			Inputs:             append([]FileInput(nil), step.Inputs...),
			CommandFingerprint: step.CommandFingerprint,
			DependsOn:          append([]string(nil), step.DependsOn...),
			Output:             step.Output,
		}
	}
	return out
}

func (s StepSnapshot) InputsEqual(other StepSnapshot) bool {
	a := append([]FileInput(nil), s.Inputs...)
	b := append([]FileInput(nil), other.Inputs...)
	sort.Slice(a, func(i, j int) bool { return a[i].Path < a[j].Path })
	sort.Slice(b, func(i, j int) bool { return b[i].Path < b[j].Path })
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s StepSnapshot) DependsEqual(other StepSnapshot) bool {
	a := append([]string(nil), s.DependsOn...)
	b := append([]string(nil), other.DependsOn...)
	sort.Strings(a)
	sort.Strings(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func SortedLayers(p Project) []Layer {
	layers := make([]Layer, 0, len(p.Bases)+len(p.Steps))
	for _, base := range p.Bases {
		layers = append(layers, Layer{ID: base.ID, SizeBytes: base.SizeBytes})
	}
	for _, step := range p.Steps {
		layers = append(layers, step.Output)
	}
	sort.Slice(layers, func(i, j int) bool { return layers[i].ID < layers[j].ID })
	return layers
}

func Normalize(p Project) Project {
	out := p.Clone()
	for i := range out.Bases {
		out.Bases[i].ID = strings.TrimSpace(out.Bases[i].ID)
	}
	for i := range out.Steps {
		out.Steps[i].Name = strings.TrimSpace(out.Steps[i].Name)
		out.Steps[i].CommandFingerprint = strings.TrimSpace(out.Steps[i].CommandFingerprint)
		sort.Slice(out.Steps[i].Inputs, func(a, b int) bool {
			return out.Steps[i].Inputs[a].Path < out.Steps[i].Inputs[b].Path
		})
		sort.Strings(out.Steps[i].DependsOn)
	}
	sort.Slice(out.Shares, func(i, j int) bool {
		if out.Shares[i].LayerID == out.Shares[j].LayerID {
			if len(out.Shares[i].Consumers) == 0 || len(out.Shares[j].Consumers) == 0 {
				return len(out.Shares[i].Consumers) < len(out.Shares[j].Consumers)
			}
			return out.Shares[i].Consumers[0] < out.Shares[j].Consumers[0]
		}
		return out.Shares[i].LayerID < out.Shares[j].LayerID
	})
	for i := range out.Shares {
		sort.Ints(out.Shares[i].Consumers)
	}
	return out
}

func SemanticHash(p Project) string {
	data, _ := json.Marshal(Normalize(p))
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func HashFields(tag string, fields ...string) string {
	h := sha256.New()
	writePart(h, tag)
	for _, field := range fields {
		writePart(h, field)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

type stringWriter interface {
	Write([]byte) (int, error)
}

func writePart(w stringWriter, part string) {
	fmt.Fprintf(w, "%d:", len(part))
	_, _ = w.Write([]byte(part))
	_, _ = w.Write([]byte{'\n'})
}
