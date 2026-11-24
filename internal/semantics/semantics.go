// Package semantics loads release-branch event streams and maps their
// branch-specific field meanings into the branch-neutral internal model.
package semantics

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"audioforge/internal/errs"
	"audioforge/internal/model"
	"audioforge/internal/num"
)

type Registry struct {
	specs map[string]model.BranchSpec
	order []string
}

func NewRegistry(specs ...model.BranchSpec) (*Registry, error) {
	r := &Registry{specs: make(map[string]model.BranchSpec, len(specs))}
	for _, spec := range specs {
		if err := spec.Validate(); err != nil {
			return nil, errs.BranchField(spec.Name, "spec", "invalid branch specification: %v", err)
		}
		if _, exists := r.specs[spec.Name]; exists {
			return nil, errs.BranchField(spec.Name, "name", "duplicate branch specification")
		}
		r.specs[spec.Name] = spec
		r.order = append(r.order, spec.Name)
	}
	sort.Strings(r.order)
	return r, nil
}

func (r *Registry) Spec(name string) (model.BranchSpec, bool) {
	spec, ok := r.specs[name]
	return spec, ok
}

func (r *Registry) Names() []string {
	return append([]string(nil), r.order...)
}

type rawFile struct {
	Branch string     `json:"branch"`
	Events []rawEvent `json:"events"`
}

// Pointer fields distinguish a missing required field from an explicit zero.
type rawEvent struct {
	ID              *string            `json:"id"`
	Time            *int64             `json:"time"`
	Priority        *int               `json:"priority"`
	Blocking        *bool              `json:"blocking"`
	Conditional     *bool              `json:"conditional"`
	Gate            *bool              `json:"gate"`
	Value           *int64             `json:"value"`
	Sequence        *[]json.RawMessage `json:"sequence"`
	Route           *[]json.RawMessage `json:"route"`
	Kind            *[]json.RawMessage `json:"kind"`
	Intensity       *int64             `json:"intensity"`
	DurationSamples *int64             `json:"duration_samples"`
}

func LoadFile(path string, sampleRate int64, registry *Registry) (model.Stream, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.Stream{}, errs.Wrap(errs.EVersion, err, "read branch stream %s", path)
	}
	stream, err := DecodeWithSampleRate(data, path, sampleRate, registry)
	if err != nil {
		return model.Stream{}, err
	}
	return stream, nil
}

func LoadFiles(paths []string, sampleRate int64, registry *Registry) ([]model.Stream, error) {
	streams := make([]model.Stream, 0, len(paths))
	for _, path := range paths {
		stream, err := LoadFile(path, sampleRate, registry)
		if err != nil {
			return nil, err
		}
		streams = append(streams, stream)
	}
	sort.Slice(streams, func(i, j int) bool { return streams[i].Spec.Name < streams[j].Spec.Name })
	return streams, nil
}

func Decode(data []byte, sourceFile string, registry *Registry) (model.Stream, error) {
	var raw rawFile
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return model.Stream{}, errs.BranchField("", "json", "decode branch stream: %v", err)
	}
	if raw.Branch == "" {
		return model.Stream{}, errs.BranchField("", "branch", "missing required branch identifier")
	}
	spec, ok := registry.Spec(raw.Branch)
	if !ok {
		return model.Stream{}, errs.BranchField(raw.Branch, "branch", "unknown branch %q", raw.Branch)
	}
	events := make([]model.Event, 0, len(raw.Events))
	seenIDs := make(map[string]struct{}, len(raw.Events))
	for i, in := range raw.Events {
		event, err := decodeEvent(spec, i, in)
		if err != nil {
			return model.Stream{}, err
		}
		if _, exists := seenIDs[event.ID]; exists {
			return model.Stream{}, errs.BranchField(spec.Name, eventPath(i, "id"), "duplicate event id %q", event.ID)
		}
		seenIDs[event.ID] = struct{}{}
		events = append(events, event)
	}
	return model.Stream{Spec: spec, SourceFile: sourceFile, Events: events}, nil
}

func decodeEvent(spec model.BranchSpec, index int, in rawEvent) (model.Event, error) {
	base := fmt.Sprintf("events[%d]", index)
	if in.ID == nil {
		return model.Event{}, errs.BranchField(spec.Name, base+".id", "missing required field")
	}
	eventID := *in.ID
	if eventID == "" {
		return model.Event{}, errs.BranchField(spec.Name, base+".id", "event id must not be empty")
	}
	missing := ""
	switch {
	case in.Time == nil:
		missing = "time"
	case in.Priority == nil:
		missing = "priority"
	case in.Blocking == nil:
		missing = "blocking"
	case in.Conditional == nil:
		missing = "conditional"
	case in.Gate == nil:
		missing = "gate"
	case in.Value == nil:
		missing = "value"
	case in.Sequence == nil:
		missing = "sequence"
	}
	if missing != "" {
		return model.Event{}, errs.BranchField(spec.Name, base+"."+missing, "missing required field")
	}
	if *in.Time < 0 {
		return model.Event{}, errs.BranchField(spec.Name, base+".time", "time must not be negative")
	}
	if *in.Priority < 0 {
		return model.Event{}, errs.BranchField(spec.Name, base+".priority", "priority must not be negative")
	}
	if *in.Value < 0 {
		return model.Event{}, errs.BranchField(spec.Name, base+".value", "value must not be negative")
	}
	start, ok := num.TimeToSamples(*in.Time, sampleRatePlaceholder, spec.TickRate, spec.TimeUnit, spec.TimeRounding)
	if !ok {
		return model.Event{}, errs.Overflow(eventID, "time", "integer overflow converting %s to internal sample cycles", spec.TimeUnit)
	}
	// The loader does not own the device sample rate. Time conversion is
	// finalized by DecodeWithSampleRate, while this call catches overflow early.
	_ = start

	types := []string(nil)
	routes := []int(nil)
	sourcePaths := model.SourcePaths{
		"id":          base + ".id",
		"start":       base + ".time",
		"priority":    base + ".priority",
		"blocking":    base + ".blocking",
		"conditional": base + ".conditional",
		"gate":        base + ".gate",
		"value":       base + ".value",
	}
	switch spec.ListSemantic {
	case model.SemanticEventTypes:
		if in.Route == nil {
			return model.Event{}, errs.BranchField(spec.Name, base+".route", "missing route list required by event_types semantics")
		}
		var err error
		types, err = stringList(*in.Sequence, spec.Name, base+".sequence")
		if err != nil {
			return model.Event{}, err
		}
		routes, err = intList(*in.Route, spec.Name, base+".route")
		if err != nil {
			return model.Event{}, err
		}
		sourcePaths["types"] = base + ".sequence"
		sourcePaths["routes"] = base + ".route"
	case model.SemanticChannelRoutes:
		if in.Kind == nil {
			return model.Event{}, errs.BranchField(spec.Name, base+".kind", "missing event type list required by channel_routes semantics")
		}
		var err error
		routes, err = intList(*in.Sequence, spec.Name, base+".sequence")
		if err != nil {
			return model.Event{}, err
		}
		types, err = stringList(*in.Kind, spec.Name, base+".kind")
		if err != nil {
			return model.Event{}, err
		}
		sourcePaths["types"] = base + ".kind"
		sourcePaths["routes"] = base + ".sequence"
	default:
		return model.Event{}, errs.BranchField(spec.Name, base+".sequence", "unsupported list semantics")
	}
	if len(types) == 0 {
		return model.Event{}, errs.BranchField(spec.Name, sourcePaths["types"], "event type list must not be empty")
	}
	if len(routes) == 0 {
		return model.Event{}, errs.BranchField(spec.Name, sourcePaths["routes"], "channel route list must not be empty")
	}
	for _, route := range routes {
		if route < 0 {
			return model.Event{}, errs.BranchField(spec.Name, sourcePaths["routes"], "channel route must not be negative")
		}
	}

	intensity := int64(0)
	duration := int64(0)
	switch spec.ValueSemantic {
	case model.SemanticIntensity:
		intensity = *in.Value
		sourcePaths["intensity"] = base + ".value"
		if in.DurationSamples != nil {
			duration = *in.DurationSamples
			sourcePaths["duration"] = base + ".duration_samples"
		} else {
			var err error
			duration, err = num.IntensityToDuration(intensity, spec.IntensityQuantumSamples, spec.TimeRounding)
			if err != nil {
				return model.Event{}, errs.Overflow(eventID, "duration_samples", "derive duration from intensity: %v", err)
			}
			sourcePaths["duration"] = base + ".value(intensity->duration)"
		}
	case model.SemanticDurationSamples:
		duration = *in.Value
		sourcePaths["duration"] = base + ".value"
		if in.Intensity != nil {
			intensity = *in.Intensity
			sourcePaths["intensity"] = base + ".intensity"
		} else {
			var err error
			intensity, err = num.DurationToIntensity(duration, spec.IntensityQuantumSamples, spec.TimeRounding)
			if err != nil {
				return model.Event{}, errs.Overflow(eventID, "intensity", "derive intensity from duration: %v", err)
			}
			sourcePaths["intensity"] = base + ".value(duration->intensity)"
		}
	default:
		return model.Event{}, errs.BranchField(spec.Name, base+".value", "unsupported value semantics")
	}
	if duration < 0 || intensity < 0 {
		return model.Event{}, errs.BranchField(spec.Name, base+".value", "derived intensity and duration must not be negative")
	}

	return model.Event{
		ID:              eventID,
		SourceBranch:    spec.Name,
		SourceOrdinal:   index,
		StartSample:     0, // filled by DecodeWithSampleRate
		Priority:        *in.Priority,
		Blocking:        *in.Blocking,
		Conditional:     *in.Conditional,
		Gate:            *in.Gate,
		Active:          !*in.Conditional || *in.Gate,
		Intensity:       intensity,
		DurationSamples: duration,
		EventTypes:      types,
		Routes:          routes,
		SourcePaths:     sourcePaths,
	}, nil
}

// Keep Decode free of device assumptions; a sample rate of one is used only
// for early overflow detection. Callers use DecodeWithSampleRate for values.
const sampleRatePlaceholder int64 = 1

// DecodeWithSampleRate performs the same strict decoding as Decode, then
// resolves branch time values into the device sample-rate domain.
func DecodeWithSampleRate(data []byte, sourceFile string, sampleRate int64, registry *Registry) (model.Stream, error) {
	stream, err := Decode(data, sourceFile, registry)
	if err != nil {
		return model.Stream{}, err
	}
	if sampleRate <= 0 {
		return model.Stream{}, errs.New(errs.EVersion, "sample rate must be positive")
	}
	var raw rawFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return model.Stream{}, errs.BranchField(stream.Spec.Name, "json", "decode branch stream: %v", err)
	}
	for i := range stream.Events {
		if raw.Events[i].Time == nil {
			continue
		}
		start, ok := num.TimeToSamples(*raw.Events[i].Time, sampleRate, stream.Spec.TickRate, stream.Spec.TimeUnit, stream.Spec.TimeRounding)
		if !ok {
			return model.Stream{}, errs.Overflow(stream.Events[i].ID, "time", "integer overflow converting %s to sample cycles", stream.Spec.TimeUnit)
		}
		stream.Events[i].StartSample = start
	}
	return stream, nil
}

func LoadFileWithSampleRate(path string, sampleRate int64, registry *Registry) (model.Stream, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.Stream{}, errs.Wrap(errs.EVersion, err, "read branch stream %s", path)
	}
	return DecodeWithSampleRate(data, path, sampleRate, registry)
}

func stringList(values []json.RawMessage, branch, field string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, rawValue := range values {
		var value string
		if err := json.Unmarshal(rawValue, &value); err != nil {
			return nil, errs.BranchField(branch, field, "expected a list of strings: %v", err)
		}
		out = append(out, value)
	}
	return out, nil
}

func intList(values []json.RawMessage, branch, field string) ([]int, error) {
	out := make([]int, 0, len(values))
	for _, rawValue := range values {
		var value int
		if err := json.Unmarshal(rawValue, &value); err != nil {
			return nil, errs.BranchField(branch, field, "expected a list of integers: %v", err)
		}
		out = append(out, value)
	}
	return out, nil
}

type wireEvent struct {
	ID              string    `json:"id"`
	Time            int64     `json:"time"`
	Priority        int       `json:"priority"`
	Blocking        bool      `json:"blocking"`
	Conditional     bool      `json:"conditional"`
	Gate            bool      `json:"gate"`
	Value           int64     `json:"value"`
	Sequence        any       `json:"sequence"`
	Route           *[]int    `json:"route,omitempty"`
	Kind            *[]string `json:"kind,omitempty"`
	Intensity       *int64    `json:"intensity,omitempty"`
	DurationSamples *int64    `json:"duration_samples,omitempty"`
}

type wireFile struct {
	Branch string      `json:"branch"`
	Events []wireEvent `json:"events"`
}

// EncodeStreamAtRate encodes time in the branch's declared unit using the
// supplied device sample rate.
func EncodeStreamAtRate(spec model.BranchSpec, events []model.Event, sampleRate int64) ([]byte, error) {
	if err := spec.Validate(); err != nil {
		return nil, errs.BranchField(spec.Name, "spec", "invalid branch specification: %v", err)
	}
	if sampleRate <= 0 {
		return nil, errs.New(errs.EVersion, "sample rate must be positive")
	}
	out := wireFile{Branch: spec.Name, Events: make([]wireEvent, 0, len(events))}
	for i, event := range events {
		timeValue, ok := num.SamplesToTime(event.StartSample, sampleRate, spec.TickRate, spec.TimeUnit, spec.TimeRounding)
		if !ok {
			return nil, errs.Overflow(event.ID, "time", "integer overflow encoding event %d", i)
		}
		we := wireEvent{
			ID:          event.ID,
			Time:        timeValue,
			Priority:    event.Priority,
			Blocking:    event.Blocking,
			Conditional: event.Conditional,
			Gate:        event.Gate,
		}
		switch spec.ValueSemantic {
		case model.SemanticIntensity:
			we.Value = event.Intensity
			d := event.DurationSamples
			we.DurationSamples = &d
		case model.SemanticDurationSamples:
			we.Value = event.DurationSamples
			n := event.Intensity
			we.Intensity = &n
		}
		switch spec.ListSemantic {
		case model.SemanticEventTypes:
			we.Sequence = append([]string(nil), event.EventTypes...)
			route := append([]int(nil), event.Routes...)
			we.Route = &route
		case model.SemanticChannelRoutes:
			we.Sequence = append([]int(nil), event.Routes...)
			kind := append([]string(nil), event.EventTypes...)
			we.Kind = &kind
		}
		out.Events = append(out.Events, we)
	}
	return json.MarshalIndent(out, "", "  ")
}

// CompareStreams validates that all branch descriptions normalize to the same
// internal events while allowing each stream to keep its own source metadata.
func CompareStreams(streams []model.Stream) error {
	if len(streams) < 2 {
		return nil
	}
	base := streams[0]
	index := make(map[string]model.Event, len(base.Events))
	for _, event := range base.Events {
		index[event.ID] = event
	}
	for _, stream := range streams[1:] {
		if len(stream.Events) != len(base.Events) {
			return errs.BranchField(stream.Spec.Name, "events", "event count differs from branch %s", base.Spec.Name)
		}
		for _, event := range stream.Events {
			left, ok := index[event.ID]
			if !ok {
				return errs.BranchField(stream.Spec.Name, "events[].id", "event %q is absent from branch %s", event.ID, base.Spec.Name)
			}
			if !model.CanonicalEqual(left, event) {
				return errs.BranchField(stream.Spec.Name, "events["+event.ID+"]", "normalized event differs from branch %s", base.Spec.Name)
			}
		}
	}
	return nil
}

func eventPath(index int, field string) string {
	return fmt.Sprintf("events[%d].%s", index, field)
}
