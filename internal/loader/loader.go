package loader

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"

	"spatialworkbench/internal/model"
	"spatialworkbench/internal/werr"
)

func LoadFile(path string) (model.Manifest, *werr.Error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.Manifest{}, werr.Decode("cannot read %s: %v", path, err)
	}
	return Parse(data)
}

func Parse(data []byte) (model.Manifest, *werr.Error) {
	var raw any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return model.Manifest{}, werr.Decode("invalid JSON: %v", err)
	}
	if err := ensureEOF(dec); err != nil {
		return model.Manifest{}, werr.Decode("invalid trailing JSON: %v", err)
	}
	root, ok := raw.(map[string]any)
	if !ok {
		return model.Manifest{}, werr.Decode("manifest root must be an object")
	}
	for _, field := range []string{"formatVersion", "generator", "samples"} {
		if _, exists := root[field]; !exists {
			return model.Manifest{}, missing("", field)
		}
	}

	var manifest model.Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&manifest); err != nil {
		return model.Manifest{}, werr.Decode("cannot decode manifest: %v", err)
	}
	if manifest.FormatVersion != model.FormatVersion {
		e := werr.Decode("unsupported formatVersion %d, expected %d", manifest.FormatVersion, model.FormatVersion)
		e.Field = "formatVersion"
		e.Actual = manifest.FormatVersion
		e.Expected = model.FormatVersion
		return model.Manifest{}, e
	}
	if len(manifest.Samples) == 0 {
		return model.Manifest{}, werr.Decode("samples must not be empty")
	}

	rawSamples, ok := root["samples"].([]any)
	if !ok || len(rawSamples) != len(manifest.Samples) {
		return model.Manifest{}, missing("samples", "samples")
	}
	seenSamples := map[string]bool{}
	for i := range manifest.Samples {
		rawSample, ok := rawSamples[i].(map[string]any)
		if !ok {
			return model.Manifest{}, werr.Decode("samples[%d] must be an object", i)
		}
		if err := validateSample(&manifest.Samples[i], rawSample, i); err != nil {
			return model.Manifest{}, err
		}
		if seenSamples[manifest.Samples[i].ID] {
			e := werr.Decode("duplicate sample id %q", manifest.Samples[i].ID)
			e.Field = fmt.Sprintf("samples[%d].id", i)
			return model.Manifest{}, e
		}
		seenSamples[manifest.Samples[i].ID] = true
	}
	return manifest, nil
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("additional value")
	}
	return err
}

func validateSample(sample *model.Sample, raw map[string]any, sampleIndex int) *werr.Error {
	prefix := fmt.Sprintf("samples[%d]", sampleIndex)
	for _, field := range []string{"id", "seed", "sampleRate", "layout", "irSamples", "channels", "dry"} {
		if _, exists := raw[field]; !exists {
			return missing(prefix, field)
		}
	}
	if sample.ID == "" {
		return missing(prefix, "id")
	}
	if sample.SampleRate < model.MinSampleRate || sample.SampleRate > model.MaxSampleRate {
		e := werr.New(werr.CodeRate, "unsupported sample rate %d; supported interval is [%d,%d]", sample.SampleRate, model.MinSampleRate, model.MaxSampleRate)
		e.Field = prefix + ".sampleRate"
		e.Actual = sample.SampleRate
		min, max := float64(model.MinSampleRate), float64(model.MaxSampleRate)
		e.Min, e.Max = &min, &max
		return e
	}
	layoutRaw, ok := raw["layout"].(map[string]any)
	if !ok {
		return missing(prefix, "layout")
	}
	for _, field := range []string{"name", "channels"} {
		if _, exists := layoutRaw[field]; !exists {
			return missing(prefix, "layout."+field)
		}
	}
	if sample.Layout.Name == "" || len(sample.Layout.Channels) == 0 {
		return missing(prefix, "layout.channels")
	}
	if err := validateUniqueIDs(sample.Layout.Channels, prefix+".layout.channels", ""); err != nil {
		return err
	}
	if len(sample.Layout.Channels) != len(sample.Channels) {
		e := werr.New(werr.CodeChan, "layout declares %d channels but manifest supplies %d", len(sample.Layout.Channels), len(sample.Channels))
		e.Field = prefix + ".layout.channels"
		return e
	}
	if sample.IRSamples <= 0 {
		return missing(prefix, "irSamples")
	}
	if len(sample.Channels) == 0 {
		return missing(prefix, "channels")
	}
	rawChannels, ok := raw["channels"].([]any)
	if !ok || len(rawChannels) != len(sample.Channels) {
		return missing(prefix, "channels")
	}
	layoutSet := make(map[string]bool, len(sample.Layout.Channels))
	for _, id := range sample.Layout.Channels {
		layoutSet[id] = true
	}
	seenChannels := map[string]bool{}
	for i := range sample.Channels {
		chRaw, ok := rawChannels[i].(map[string]any)
		if !ok {
			return werr.Decode("%s.channels[%d] must be an object", prefix, i)
		}
		ch := &sample.Channels[i]
		chPrefix := fmt.Sprintf("%s.channels[%d]", prefix, i)
		for _, field := range []string{"id", "samples", "truth"} {
			if _, exists := chRaw[field]; !exists {
				return missing(chPrefix, field)
			}
		}
		if ch.ID == "" {
			return missing(chPrefix, "id")
		}
		if !layoutSet[ch.ID] {
			e := werr.New(werr.CodeChan, "channel %q is not declared by layout", ch.ID)
			e.Field = chPrefix + ".id"
			e.Channel = ch.ID
			return e
		}
		if seenChannels[ch.ID] {
			e := werr.New(werr.CodeChan, "duplicate channel id %q", ch.ID)
			e.Field = chPrefix + ".id"
			e.Channel = ch.ID
			return e
		}
		seenChannels[ch.ID] = true
		if len(ch.Samples) != sample.IRSamples {
			e := werr.Decode("channel %q has %d samples, manifest irSamples is %d", ch.ID, len(ch.Samples), sample.IRSamples)
			e.Field = chPrefix + ".samples"
			e.Channel = ch.ID
			e.Actual = len(ch.Samples)
			e.Expected = sample.IRSamples
			return e
		}
		for n, v := range ch.Samples {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				e := werr.Decode("channel %q sample %d is not finite", ch.ID, n)
				e.Field = chPrefix + ".samples"
				e.Channel = ch.ID
				return e
			}
		}
		if err := validateTruth(ch, chRaw, chPrefix); err != nil {
			return err
		}
	}
	for _, id := range sample.Layout.Channels {
		if !seenChannels[id] {
			e := werr.New(werr.CodeChan, "layout channel %q is missing from channel list", id)
			e.Field = prefix + ".channels"
			e.Channel = id
			return e
		}
	}
	dryRaw, ok := raw["dry"].(map[string]any)
	if !ok {
		return missing(prefix, "dry")
	}
	for _, field := range []string{"description", "samples"} {
		if _, exists := dryRaw[field]; !exists {
			return missing(prefix, "dry."+field)
		}
	}
	if sample.Dry.Description == "" || len(sample.Dry.Samples) == 0 {
		return missing(prefix, "dry.samples")
	}
	for n, v := range sample.Dry.Samples {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			e := werr.Decode("dry sample %d is not finite", n)
			e.Field = prefix + ".dry.samples"
			return e
		}
	}
	return nil
}

func validateUniqueIDs(ids []string, field, channel string) *werr.Error {
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			return missing("", field)
		}
		if seen[id] {
			e := werr.New(werr.CodeChan, "duplicate channel id %q", id)
			e.Field = field
			e.Channel = channel
			return e
		}
		seen[id] = true
	}
	return nil
}

func validateTruth(ch *model.Channel, raw map[string]any, prefix string) *werr.Error {
	truthRaw, ok := raw["truth"].(map[string]any)
	if !ok {
		return missing(prefix, "truth")
	}
	for _, field := range []string{"directArrival", "directEnergy", "reflections", "drrDb", "itdSamples"} {
		if _, exists := truthRaw[field]; !exists {
			return missing(prefix, "truth."+field)
		}
	}
	direct := ch.Truth.DirectArrival
	if direct < 0 || direct >= len(ch.Samples) {
		e := werr.Decode("channel %q direct arrival %d is outside [0,%d)", ch.ID, direct, len(ch.Samples))
		e.Field = prefix + ".truth.directArrival"
		e.Channel = ch.ID
		return e
	}
	if math.IsNaN(ch.Truth.DirectEnergy) || math.IsInf(ch.Truth.DirectEnergy, 0) || ch.Truth.DirectEnergy <= 0 {
		e := werr.Decode("channel %q direct energy must be finite and positive", ch.ID)
		e.Field = prefix + ".truth.directEnergy"
		e.Channel = ch.ID
		return e
	}
	if len(ch.Truth.Reflections) == 0 {
		e := werr.Decode("channel %q must declare at least one reflection", ch.ID)
		e.Field = prefix + ".truth.reflections"
		e.Channel = ch.ID
		return e
	}
	previous := direct
	for i, refl := range ch.Truth.Reflections {
		if refl.Arrival <= previous || refl.Arrival < 0 || refl.Arrival >= len(ch.Samples) {
			e := werr.New(werr.CodeOrder, "channel %q reflection arrival %d is not strictly after %d or is outside the impulse response", ch.ID, refl.Arrival, previous)
			e.Field = fmt.Sprintf("%s.truth.reflections[%d].arrival", prefix, i)
			e.Channel = ch.ID
			e.Actual = refl.Arrival
			e.Expected = fmt.Sprintf(">%d and <len(samples)", previous)
			return e
		}
		if math.IsNaN(refl.Energy) || math.IsInf(refl.Energy, 0) || refl.Energy < 0 {
			e := werr.Decode("channel %q reflection %d energy is invalid", ch.ID, i)
			e.Field = fmt.Sprintf("%s.truth.reflections[%d].energy", prefix, i)
			e.Channel = ch.ID
			return e
		}
		previous = refl.Arrival
	}
	if math.IsNaN(ch.Truth.DRRDB) || math.IsInf(ch.Truth.DRRDB, 0) {
		e := werr.Decode("channel %q drrDb is not finite", ch.ID)
		e.Field = prefix + ".truth.drrDb"
		e.Channel = ch.ID
		return e
	}
	return nil
}

func missing(prefix, field string) *werr.Error {
	full := field
	if prefix != "" {
		full = prefix + "." + field
	}
	e := werr.Decode("required field %q is missing or empty", full)
	e.Field = full
	return e
}

// SortedChannelIDs is useful for deterministic diagnostics.
func SortedChannelIDs(sample model.Sample) []string {
	ids := make([]string, 0, len(sample.Channels))
	for _, ch := range sample.Channels {
		ids = append(ids, ch.ID)
	}
	sort.Strings(ids)
	return ids
}
