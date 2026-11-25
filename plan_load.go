package texture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// LoadPlan loads any supported plan version. The version is selected only from
// document content (format/schema_version/version), never from a file name.
func LoadPlan(data []byte) (*Plan, error) {
	root, err := objectFromJSON(data, "$")
	if err != nil {
		return nil, err
	}
	if raw, ok := root["format"]; ok {
		var format string
		if err := json.Unmarshal(raw, &format); err != nil {
			return nil, errf(CodeVersion, "$.format", "format", "must be a string")
		}
		switch strings.ToLower(strings.TrimSpace(format)) {
		case "texture-plan/3":
			return loadV3(root)
		default:
			return nil, errf(CodeVersion, "$.format", "format", "unknown plan format %q", format)
		}
	}
	if raw, ok := root["schema_version"]; ok {
		v, err := intFromRaw(raw, "$.schema_version", "schema_version")
		if err != nil {
			return nil, errf(CodeVersion, "$.schema_version", "schema_version", "must be the integer 2")
		}
		if v != 2 {
			return nil, errf(CodeVersion, "$.schema_version", "schema_version", "unknown schema version %d", v)
		}
		return loadV2(root)
	}
	if raw, ok := root["version"]; ok {
		version, err := stringOrInt(raw, "$.version", "version")
		if err != nil {
			return nil, errf(CodeVersion, "$.version", "version", "must identify a supported version")
		}
		if version != "1" {
			return nil, errf(CodeVersion, "$.version", "version", "unknown plan version %q", version)
		}
		return loadV1(root)
	}
	return nil, errf(CodeVersion, "$", "version", "no format, schema_version, or version field")
}

func loadV1(root map[string]json.RawMessage) (*Plan, error) {
	p := &Plan{Version: "1"}
	sourceRaw, err := required(root, "source", "$")
	if err != nil {
		return nil, err
	}
	srcObj, err := objectFromRaw(sourceRaw, "$.source")
	if err != nil {
		return nil, err
	}
	width, err := requiredInt(srcObj, "width", "$.source")
	if err != nil {
		return nil, err
	}
	height, err := requiredInt(srcObj, "height", "$.source")
	if err != nil {
		return nil, err
	}
	bitDepth, err := requiredInt(srcObj, "bit_depth", "$.source")
	if err != nil {
		return nil, err
	}
	layoutStr, err := requiredString(srcObj, "layout", "$.source")
	if err != nil {
		return nil, err
	}
	layout, err := ParseLayout(layoutStr)
	if err != nil {
		return nil, errf(CodeSchema, "$.source.layout", "layout", "%v", err)
	}
	p.Source = Source{Width: width, Height: height, BitDepth: bitDepth, Layout: layout}
	p.Traces = append(p.Traces,
		trace("source.width", "$.source.width", "1"),
		trace("source.height", "$.source.height", "1"),
		trace("source.bit_depth", "$.source.bit_depth", "1"),
		trace("source.layout", "$.source.layout", "1"),
	)

	entriesRaw, err := required(root, "entries", "$")
	if err != nil {
		return nil, err
	}
	var rawEntries []json.RawMessage
	if err := json.Unmarshal(entriesRaw, &rawEntries); err != nil {
		return nil, errf(CodeSchema, "$.entries", "entries", "must be an array")
	}
	for i, rawEntry := range rawEntries {
		path := fmt.Sprintf("$.entries[%d]", i)
		obj, err := objectFromRaw(rawEntry, path)
		if err != nil {
			return nil, err
		}
		e, err := loadV1Entry(obj, path, i)
		if err != nil {
			return nil, err
		}
		p.Entries = append(p.Entries, e)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV1Entry(obj map[string]json.RawMessage, path string, ordinal int) (Entry, error) {
	e := Entry{Index: ordinal}
	index, err := requiredInt(obj, "index", path)
	if err != nil {
		return e, err
	}
	e.Index = index
	coverage, err := loadRectV1(obj, "coverage", path)
	if err != nil {
		return e, err
	}
	e.Coverage = coverage
	depth, err := requiredInt(obj, "target_bit_depth", path)
	if err != nil {
		return e, err
	}
	e.TargetBitDepth = depth
	orderRaw, err := required(obj, "channel_order", path)
	if err != nil {
		return e, err
	}
	var order []string
	if err := json.Unmarshal(orderRaw, &order); err != nil {
		return e, errf(CodeSchema, path+".channel_order", "channel_order", "must be an array of channel names")
	}
	for _, s := range order {
		ch, ok := ParseChannel(s)
		if !ok {
			return e, errf(CodeSchema, path+".channel_order", "channel_order", "unknown channel %q", s)
		}
		e.TargetChannels = append(e.TargetChannels, ch)
	}
	roundingRaw, err := requiredString(obj, "rounding", path)
	if err != nil {
		return e, err
	}
	e.Rounding = parseRoundingAlias(roundingRaw)
	if !validRounding(e.Rounding) {
		return e, errf(CodeSchema, path+".rounding", "rounding", "unsupported rounding %q", roundingRaw)
	}
	alphaRaw, err := requiredString(obj, "alpha", path)
	if err != nil {
		return e, err
	}
	switch strings.ToLower(alphaRaw) {
	case "copy", "keep":
		e.Alpha = AlphaCopy
	case "drop", "ignore":
		e.Alpha = AlphaDrop
	default:
		return e, errf(CodeSchema, path+".alpha", "alpha", "version 1 supports copy or drop, got %q", alphaRaw)
	}
	fills := make(map[Channel]FillSource)
	if fillRaw, ok := obj["fill"]; ok && !isJSONNull(fillRaw) {
		fillPath := path + ".fill"
		fillObj, err := objectFromRaw(fillRaw, fillPath)
		if err != nil {
			return e, err
		}
		mode, err := requiredString(fillObj, "mode", fillPath)
		if err != nil {
			return e, err
		}
		fill := FillSource{Kind: FillKind(strings.ToLower(mode))}
		switch fill.Kind {
		case FillConstant:
			value, err := requiredInt(fillObj, "value", fillPath)
			if err != nil {
				return e, err
			}
			if value < 0 || value > 65535 {
				return e, errf(CodePara, fillPath+".value", "fill.value", "must be in [0,65535]")
			}
			fill.Value = uint16(value)
		case FillZero, FillOne:
		case FillFromChannel:
			source, err := requiredString(fillObj, "source_channel", fillPath)
			if err != nil {
				return e, err
			}
			ch, ok := ParseChannel(source)
			if !ok {
				return e, errf(CodeSchema, fillPath+".source_channel", "fill.source_channel", "unknown channel %q", source)
			}
			fill.Channel = ch
		default:
			return e, errf(CodeSchema, fillPath+".mode", "fill.mode", "unsupported fill mode %q", mode)
		}
		channelsRaw, err := required(fillObj, "channels", fillPath)
		if err != nil {
			return e, err
		}
		var channelNames []string
		if err := json.Unmarshal(channelsRaw, &channelNames); err != nil {
			return e, errf(CodeSchema, fillPath+".channels", "fill.channels", "must be an array")
		}
		for _, name := range channelNames {
			ch, ok := ParseChannel(name)
			if !ok {
				return e, errf(CodeSchema, fillPath+".channels", "fill.channels", "unknown channel %q", name)
			}
			if !containsChannel(e.TargetChannels, ch) {
				return e, errf(CodeSchema, fillPath+".channels", "fill.channels", "channel %q is not in channel_order", name)
			}
			fills[ch] = fill
		}
	}
	for _, target := range e.TargetChannels {
		if fill, ok := fills[target]; ok {
			f := fill
			e.Mappings = append(e.Mappings, ChannelMapping{Target: target, Fill: &f})
		} else {
			e.Mappings = append(e.Mappings, ChannelMapping{Target: target, Source: target})
		}
	}
	if raw, ok := obj["source_order"]; ok && !isJSONNull(raw) {
		var sourceOrder []*string
		if err := json.Unmarshal(raw, &sourceOrder); err != nil {
			return e, errf(CodeSchema, path+".source_order", "source_order", "must be an array of channel names or null")
		}
		if len(sourceOrder) != len(e.TargetChannels) {
			return e, errf(CodeSchema, path+".source_order", "source_order", "must be parallel to channel_order")
		}
		for i, target := range e.TargetChannels {
			if _, hasFill := fills[target]; hasFill {
				if sourceOrder[i] != nil && *sourceOrder[i] != "" {
					return e, errf(CodeSchema, path+".source_order", "source_order", "filled channel %q cannot also have a source", target)
				}
				continue
			}
			if sourceOrder[i] == nil || *sourceOrder[i] == "" {
				return e, errf(CodeSchema, path+".source_order", "source_order", "channel %q requires a source", target)
			}
			src, ok := ParseChannel(*sourceOrder[i])
			if !ok {
				return e, errf(CodeSchema, path+".source_order", "source_order", "unknown channel %q", *sourceOrder[i])
			}
			e.Mappings[i].Source = src
		}
	}
	e.Traces = append(e.Traces,
		trace(fmt.Sprintf("entries[%d].index", ordinal), path+".index", "1"),
		trace(fmt.Sprintf("entries[%d].coverage", ordinal), path+".coverage", "1"),
		trace(fmt.Sprintf("entries[%d].target_bit_depth", ordinal), path+".target_bit_depth", "1"),
		trace(fmt.Sprintf("entries[%d].target_channels", ordinal), path+".channel_order", "1"),
		trace(fmt.Sprintf("entries[%d].mappings", ordinal), path+".source_order", "1"),
		trace(fmt.Sprintf("entries[%d].rounding", ordinal), path+".rounding", "1"),
		trace(fmt.Sprintf("entries[%d].alpha", ordinal), path+".alpha", "1"),
	)
	return e, nil
}

func loadRectV1(obj map[string]json.RawMessage, key, path string) (Rect, error) {
	raw, err := required(obj, key, path)
	if err != nil {
		return Rect{}, err
	}
	rp := path + "." + key
	robj, err := objectFromRaw(raw, rp)
	if err != nil {
		return Rect{}, err
	}
	x, err := requiredInt(robj, "x", rp)
	if err != nil {
		return Rect{}, err
	}
	y, err := requiredInt(robj, "y", rp)
	if err != nil {
		return Rect{}, err
	}
	w, err := requiredInt(robj, "width", rp)
	if err != nil {
		return Rect{}, err
	}
	h, err := requiredInt(robj, "height", rp)
	if err != nil {
		return Rect{}, err
	}
	return Rect{X: x, Y: y, Width: w, Height: h}, nil
}

func loadV2(root map[string]json.RawMessage) (*Plan, error) {
	p := &Plan{Version: "2"}
	imageRaw, err := required(root, "image", "$")
	if err != nil {
		return nil, err
	}
	imageObj, err := objectFromRaw(imageRaw, "$.image")
	if err != nil {
		return nil, err
	}
	width, err := requiredInt(imageObj, "w", "$.image")
	if err != nil {
		return nil, err
	}
	height, err := requiredInt(imageObj, "h", "$.image")
	if err != nil {
		return nil, err
	}
	depthRaw, err := required(imageObj, "depth", "$.image")
	if err != nil {
		return nil, err
	}
	depthObj, err := objectFromRaw(depthRaw, "$.image.depth")
	if err != nil {
		return nil, err
	}
	bitDepth, err := requiredInt(depthObj, "bits", "$.image.depth")
	if err != nil {
		return nil, err
	}
	channelsRaw, err := required(imageObj, "channels", "$.image")
	if err != nil {
		return nil, err
	}
	var channelNames []string
	if err := json.Unmarshal(channelsRaw, &channelNames); err != nil {
		return nil, errf(CodeSchema, "$.image.channels", "channels", "must be an array")
	}
	layout, err := parseChannels(channelNames, "$.image.channels")
	if err != nil {
		return nil, err
	}
	p.Source = Source{Width: width, Height: height, BitDepth: bitDepth, Layout: layout}
	p.Traces = append(p.Traces,
		trace("source.width", "$.image.w", "2"),
		trace("source.height", "$.image.h", "2"),
		trace("source.bit_depth", "$.image.depth.bits", "2"),
		trace("source.layout", "$.image.channels", "2"),
	)
	opsRaw, err := required(root, "operations", "$")
	if err != nil {
		return nil, err
	}
	var rawOps []json.RawMessage
	if err := json.Unmarshal(opsRaw, &rawOps); err != nil {
		return nil, errf(CodeSchema, "$.operations", "operations", "must be an array")
	}
	for i, rawOp := range rawOps {
		path := fmt.Sprintf("$.operations[%d]", i)
		obj, err := objectFromRaw(rawOp, path)
		if err != nil {
			return nil, err
		}
		e, err := loadV2Entry(obj, path, i)
		if err != nil {
			return nil, err
		}
		p.Entries = append(p.Entries, e)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV2Entry(obj map[string]json.RawMessage, path string, ordinal int) (Entry, error) {
	e := Entry{Index: ordinal}
	step, err := requiredInt(obj, "step", path)
	if err != nil {
		return e, err
	}
	e.Index = step
	regionRaw, err := required(obj, "region", path)
	if err != nil {
		return e, err
	}
	regionPath := path + ".region"
	regionObj, err := objectFromRaw(regionRaw, regionPath)
	if err != nil {
		return e, err
	}
	left, err := requiredInt(regionObj, "left", regionPath)
	if err != nil {
		return e, err
	}
	top, err := requiredInt(regionObj, "top", regionPath)
	if err != nil {
		return e, err
	}
	right, err := requiredInt(regionObj, "right", regionPath)
	if err != nil {
		return e, err
	}
	bottom, err := requiredInt(regionObj, "bottom", regionPath)
	if err != nil {
		return e, err
	}
	e.Coverage = Rect{X: left, Y: top, Width: right - left, Height: bottom - top}

	outputRaw, err := required(obj, "output", path)
	if err != nil {
		return e, err
	}
	outputPath := path + ".output"
	outputObj, err := objectFromRaw(outputRaw, outputPath)
	if err != nil {
		return e, err
	}
	depth, err := requiredInt(outputObj, "depth", outputPath)
	if err != nil {
		return e, err
	}
	e.TargetBitDepth = depth
	layoutRaw, err := required(outputObj, "layout", outputPath)
	if err != nil {
		return e, err
	}
	var layoutNames []string
	if err := json.Unmarshal(layoutRaw, &layoutNames); err != nil {
		return e, errf(CodeSchema, outputPath+".layout", "output.layout", "must be an array")
	}
	layout, err := parseChannels(layoutNames, outputPath+".layout")
	if err != nil {
		return e, err
	}
	e.TargetChannels = layout
	targetSet := make(map[Channel]bool, len(layout))
	for _, ch := range layout {
		targetSet[ch] = true
	}
	rounding, err := requiredString(outputObj, "rounding", outputPath)
	if err != nil {
		return e, err
	}
	e.Rounding = parseRoundingAlias(rounding)
	if !validRounding(e.Rounding) {
		return e, errf(CodeSchema, outputPath+".rounding", "output.rounding", "unsupported rounding %q", rounding)
	}

	channelMap := make(map[Channel]Channel)
	if raw, ok := obj["channel_map"]; ok && !isJSONNull(raw) {
		var byName map[string]string
		if err := json.Unmarshal(raw, &byName); err != nil {
			return e, errf(CodeSchema, path+".channel_map", "channel_map", "must be an object mapping target to source")
		}
		for targetName, sourceName := range byName {
			target, ok := ParseChannel(targetName)
			if !ok {
				return e, errf(CodeSchema, path+".channel_map", "channel_map", "unknown target channel %q", targetName)
			}
			if !targetSet[target] {
				return e, errf(CodeSchema, path+".channel_map", "channel_map", "target channel %q is not in output.layout", targetName)
			}
			source, ok := ParseChannel(sourceName)
			if !ok {
				return e, errf(CodeSchema, path+".channel_map", "channel_map", "unknown source channel %q", sourceName)
			}
			channelMap[target] = source
		}
	}
	fills := make(map[Channel]FillSource)
	if raw, ok := obj["fill_sources"]; ok && !isJSONNull(raw) {
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil {
			return e, errf(CodeSchema, path+".fill_sources", "fill_sources", "must be an array")
		}
		for j, item := range list {
			fp := fmt.Sprintf("%s.fill_sources[%d]", path, j)
			itemObj, err := objectFromRaw(item, fp)
			if err != nil {
				return e, err
			}
			channelName, err := requiredString(itemObj, "channel", fp)
			if err != nil {
				return e, err
			}
			channel, ok := ParseChannel(channelName)
			if !ok {
				return e, errf(CodeSchema, fp+".channel", "fill_sources.channel", "unknown channel %q", channelName)
			}
			if !targetSet[channel] {
				return e, errf(CodeSchema, fp+".channel", "fill_sources.channel", "channel %q is not in output.layout", channelName)
			}
			fill, err := loadV2Fill(itemObj, fp)
			if err != nil {
				return e, err
			}
			if _, exists := fills[channel]; exists {
				return e, errf(CodeSchema, fp+".channel", "fill_sources.channel", "duplicate fill for channel %q", channelName)
			}
			fills[channel] = fill
		}
	}
	for _, target := range e.TargetChannels {
		src, hasSource := channelMap[target]
		fill, hasFill := fills[target]
		if hasSource && hasFill {
			return e, errf(CodeSchema, path, "mapping", "channel %q is both mapped and filled", target)
		}
		if hasFill {
			f := fill
			e.Mappings = append(e.Mappings, ChannelMapping{Target: target, Fill: &f})
			continue
		}
		if !hasSource {
			src = target
		}
		e.Mappings = append(e.Mappings, ChannelMapping{Target: target, Source: src})
	}

	alphaRaw, err := required(obj, "alpha", path)
	if err != nil {
		return e, err
	}
	alphaPath := path + ".alpha"
	alphaObj, err := objectFromRaw(alphaRaw, alphaPath)
	if err != nil {
		return e, err
	}
	mode, err := requiredString(alphaObj, "mode", alphaPath)
	if err != nil {
		return e, err
	}
	e.Alpha = AlphaMode(strings.ToLower(mode))
	if e.Alpha != AlphaCopy && e.Alpha != AlphaDrop && e.Alpha != AlphaStraight {
		return e, errf(CodeSchema, alphaPath+".mode", "alpha.mode", "version 2 supports copy, drop, or straight, got %q", mode)
	}
	e.Traces = append(e.Traces,
		trace(fmt.Sprintf("entries[%d].index", ordinal), path+".step", "2"),
		trace(fmt.Sprintf("entries[%d].coverage", ordinal), path+".region", "2"),
		trace(fmt.Sprintf("entries[%d].target_bit_depth", ordinal), path+".output.depth", "2"),
		trace(fmt.Sprintf("entries[%d].target_channels", ordinal), path+".output.layout", "2"),
		trace(fmt.Sprintf("entries[%d].mappings", ordinal), path+".channel_map", "2"),
		trace(fmt.Sprintf("entries[%d].fill", ordinal), path+".fill_sources", "2"),
		trace(fmt.Sprintf("entries[%d].rounding", ordinal), path+".output.rounding", "2"),
		trace(fmt.Sprintf("entries[%d].alpha", ordinal), path+".alpha", "2"),
	)
	return e, nil
}

func loadV2Fill(obj map[string]json.RawMessage, path string) (FillSource, error) {
	kind, err := requiredString(obj, "kind", path)
	if err != nil {
		return FillSource{}, err
	}
	f := FillSource{Kind: FillKind(strings.ToLower(kind))}
	switch f.Kind {
	case FillConstant:
		value, err := requiredInt(obj, "value", path)
		if err != nil {
			return f, err
		}
		if value < 0 || value > 65535 {
			return f, errf(CodePara, path+".value", "fill_sources.value", "must be in [0,65535]")
		}
		f.Value = uint16(value)
	case FillZero, FillOne:
	case FillFromChannel:
		channelName, err := requiredString(obj, "source", path)
		if err != nil {
			return f, err
		}
		channel, ok := ParseChannel(channelName)
		if !ok {
			return f, errf(CodeSchema, path+".source", "fill_sources.source", "unknown channel %q", channelName)
		}
		f.Channel = channel
	default:
		return f, errf(CodeSchema, path+".kind", "fill_sources.kind", "unsupported fill kind %q", kind)
	}
	return f, nil
}

func loadV3(root map[string]json.RawMessage) (*Plan, error) {
	p := &Plan{Version: "3"}
	sourceRaw, err := required(root, "source", "$")
	if err != nil {
		return nil, err
	}
	srcPath := "$.source"
	srcObj, err := objectFromRaw(sourceRaw, srcPath)
	if err != nil {
		return nil, err
	}
	width, err := requiredInt(srcObj, "width", srcPath)
	if err != nil {
		return nil, err
	}
	height, err := requiredInt(srcObj, "height", srcPath)
	if err != nil {
		return nil, err
	}
	pixelFormat, err := requiredString(srcObj, "pixel_format", srcPath)
	if err != nil {
		return nil, err
	}
	layout, bits, err := parsePixelFormat(pixelFormat, srcPath+".pixel_format")
	if err != nil {
		return nil, err
	}
	p.Source = Source{Width: width, Height: height, BitDepth: bits, Layout: layout}
	p.Traces = append(p.Traces,
		trace("source.width", srcPath+".width", "3"),
		trace("source.height", srcPath+".height", "3"),
		trace("source.bit_depth,source.layout", srcPath+".pixel_format", "3"),
	)
	entriesRaw, err := required(root, "entries", "$")
	if err != nil {
		return nil, err
	}
	var rawEntries []json.RawMessage
	if err := json.Unmarshal(entriesRaw, &rawEntries); err != nil {
		return nil, errf(CodeSchema, "$.entries", "entries", "must be an array")
	}
	for i, rawEntry := range rawEntries {
		path := fmt.Sprintf("$.entries[%d]", i)
		obj, err := objectFromRaw(rawEntry, path)
		if err != nil {
			return nil, err
		}
		e, err := loadV3Entry(obj, path, i)
		if err != nil {
			return nil, err
		}
		p.Entries = append(p.Entries, e)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV3Entry(obj map[string]json.RawMessage, path string, ordinal int) (Entry, error) {
	e := Entry{Index: ordinal}
	seq, err := requiredInt(obj, "seq", path)
	if err != nil {
		return e, err
	}
	e.Index = seq
	areaRaw, err := required(obj, "area", path)
	if err != nil {
		return e, err
	}
	areaPath := path + ".area"
	areaObj, err := objectFromRaw(areaRaw, areaPath)
	if err != nil {
		return e, err
	}
	x, err := requiredInt(areaObj, "x", areaPath)
	if err != nil {
		return e, err
	}
	y, err := requiredInt(areaObj, "y", areaPath)
	if err != nil {
		return e, err
	}
	w, err := requiredInt(areaObj, "w", areaPath)
	if err != nil {
		return e, err
	}
	h, err := requiredInt(areaObj, "h", areaPath)
	if err != nil {
		return e, err
	}
	e.Coverage = Rect{X: x, Y: y, Width: w, Height: h}
	outputRaw, err := required(obj, "output", path)
	if err != nil {
		return e, err
	}
	outputPath := path + ".output"
	outputObj, err := objectFromRaw(outputRaw, outputPath)
	if err != nil {
		return e, err
	}
	depth, err := requiredInt(outputObj, "bit_depth", outputPath)
	if err != nil {
		return e, err
	}
	e.TargetBitDepth = depth
	channelsRaw, err := required(outputObj, "channels", outputPath)
	if err != nil {
		return e, err
	}
	var channelNames []string
	if err := json.Unmarshal(channelsRaw, &channelNames); err != nil {
		return e, errf(CodeSchema, outputPath+".channels", "output.channels", "must be an array")
	}
	layout, err := parseChannels(channelNames, outputPath+".channels")
	if err != nil {
		return e, err
	}
	e.TargetChannels = layout
	targetSet := make(map[Channel]bool, len(layout))
	for _, ch := range layout {
		targetSet[ch] = true
	}
	orderRaw, err := required(obj, "source_order", path)
	if err != nil {
		return e, err
	}
	var sourceOrder []*string
	if err := json.Unmarshal(orderRaw, &sourceOrder); err != nil {
		return e, errf(CodeSchema, path+".source_order", "source_order", "must be an array of channel names or null")
	}
	if len(sourceOrder) != len(e.TargetChannels) {
		return e, errf(CodeSchema, path+".source_order", "source_order", "must be parallel to output.channels")
	}
	fills := make(map[Channel]FillSource)
	if raw, ok := obj["padding"]; ok && !isJSONNull(raw) {
		var groups []json.RawMessage
		if err := json.Unmarshal(raw, &groups); err != nil {
			return e, errf(CodeSchema, path+".padding", "padding", "must be an array of groups")
		}
		for j, groupRaw := range groups {
			gp := fmt.Sprintf("%s.padding[%d]", path, j)
			group, err := objectFromRaw(groupRaw, gp)
			if err != nil {
				return e, err
			}
			fill, err := loadV3Fill(group, gp)
			if err != nil {
				return e, err
			}
			channelsList, err := required(group, "channels", gp)
			if err != nil {
				return e, err
			}
			var names []string
			if err := json.Unmarshal(channelsList, &names); err != nil {
				return e, errf(CodeSchema, gp+".channels", "padding.channels", "must be an array")
			}
			for _, name := range names {
				ch, ok := ParseChannel(name)
				if !ok {
					return e, errf(CodeSchema, gp+".channels", "padding.channels", "unknown channel %q", name)
				}
				if !targetSet[ch] {
					return e, errf(CodeSchema, gp+".channels", "padding.channels", "channel %q is not in output.channels", name)
				}
				if _, exists := fills[ch]; exists {
					return e, errf(CodeSchema, gp+".channels", "padding.channels", "duplicate padding for channel %q", name)
				}
				fills[ch] = fill
			}
		}
	}
	for i, target := range e.TargetChannels {
		sourceName := sourceOrder[i]
		fill, hasFill := fills[target]
		if sourceName != nil && hasFill {
			return e, errf(CodeSchema, path, "mapping", "channel %q has both source_order and padding", target)
		}
		if hasFill {
			f := fill
			e.Mappings = append(e.Mappings, ChannelMapping{Target: target, Fill: &f})
			continue
		}
		if sourceName == nil {
			return e, errf(CodeSchema, path+".source_order", "source_order", "channel %q has neither source nor padding", target)
		}
		src, ok := ParseChannel(*sourceName)
		if !ok {
			return e, errf(CodeSchema, path+".source_order", "source_order", "unknown channel %q", *sourceName)
		}
		e.Mappings = append(e.Mappings, ChannelMapping{Target: target, Source: src})
	}
	rounding, err := requiredString(obj, "quantization", path)
	if err != nil {
		return e, err
	}
	e.Rounding = parseRoundingAlias(rounding)
	if !validRounding(e.Rounding) {
		return e, errf(CodeSchema, path+".quantization", "quantization", "unsupported direction %q", rounding)
	}
	alphaRaw, err := required(obj, "alpha_policy", path)
	if err != nil {
		return e, err
	}
	alphaPath := path + ".alpha_policy"
	if len(alphaRaw) > 0 && alphaRaw[0] == '"' {
		var mode string
		if err := json.Unmarshal(alphaRaw, &mode); err != nil {
			return e, errf(CodeSchema, alphaPath, "alpha_policy", "must be a string or object")
		}
		e.Alpha = AlphaMode(strings.ToLower(mode))
	} else {
		alphaObj, err := objectFromRaw(alphaRaw, alphaPath)
		if err != nil {
			return e, err
		}
		mode, err := requiredString(alphaObj, "mode", alphaPath)
		if err != nil {
			return e, err
		}
		e.Alpha = AlphaMode(strings.ToLower(mode))
	}
	if !validAlpha(e.Alpha) {
		return e, errf(CodeSchema, alphaPath, "alpha_policy", "unsupported alpha policy %q", e.Alpha)
	}
	e.Traces = append(e.Traces,
		trace(fmt.Sprintf("entries[%d].index", ordinal), path+".seq", "3"),
		trace(fmt.Sprintf("entries[%d].coverage", ordinal), path+".area", "3"),
		trace(fmt.Sprintf("entries[%d].target_bit_depth", ordinal), path+".output.bit_depth", "3"),
		trace(fmt.Sprintf("entries[%d].target_channels", ordinal), path+".output.channels", "3"),
		trace(fmt.Sprintf("entries[%d].mappings", ordinal), path+".source_order", "3"),
		trace(fmt.Sprintf("entries[%d].fill", ordinal), path+".padding", "3"),
		trace(fmt.Sprintf("entries[%d].rounding", ordinal), path+".quantization", "3"),
		trace(fmt.Sprintf("entries[%d].alpha", ordinal), path+".alpha_policy", "3"),
	)
	return e, nil
}

func loadV3Fill(obj map[string]json.RawMessage, path string) (FillSource, error) {
	sourceRaw, err := required(obj, "source", path)
	if err != nil {
		return FillSource{}, err
	}
	sourcePath := path + ".source"
	sourceObj, err := objectFromRaw(sourceRaw, sourcePath)
	if err != nil {
		return FillSource{}, err
	}
	kind, err := requiredString(sourceObj, "kind", sourcePath)
	if err != nil {
		return FillSource{}, err
	}
	f := FillSource{Kind: FillKind(strings.ToLower(kind))}
	switch f.Kind {
	case FillConstant:
		value, err := requiredInt(sourceObj, "value", sourcePath)
		if err != nil {
			return f, err
		}
		if value < 0 || value > 65535 {
			return f, errf(CodePara, sourcePath+".value", "padding.source.value", "must be in [0,65535]")
		}
		f.Value = uint16(value)
	case FillZero, FillOne:
	case FillFromChannel:
		name, err := requiredString(sourceObj, "channel", sourcePath)
		if err != nil {
			return f, err
		}
		ch, ok := ParseChannel(name)
		if !ok {
			return f, errf(CodeSchema, sourcePath+".channel", "padding.source.channel", "unknown channel %q", name)
		}
		f.Channel = ch
	default:
		return f, errf(CodeSchema, sourcePath+".kind", "padding.source.kind", "unsupported fill kind %q", kind)
	}
	return f, nil
}

func parsePixelFormat(format, path string) ([]Channel, int, error) {
	s := strings.ToLower(strings.TrimSpace(format))
	for _, bits := range []int{8, 16} {
		suffix := strconv.Itoa(bits)
		if !strings.HasSuffix(s, suffix) {
			continue
		}
		layoutName := strings.TrimSuffix(s, suffix)
		layoutName = strings.TrimSuffix(layoutName, "_")
		layout, err := ParseLayout(layoutName)
		if err != nil || len(layout) != 4 {
			return nil, 0, errf(CodeSchema, path, "pixel_format", "pixel format must be rgba8, rgba16, bgra8, or bgra16")
		}
		return layout, bits, nil
	}
	return nil, 0, errf(CodeSchema, path, "pixel_format", "pixel format must end in 8 or 16")
}

func parseRoundingAlias(s string) Rounding {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "nearest", "round", "round_half_up":
		return RoundNearest
	case "floor", "down", "toward_zero":
		return RoundFloor
	case "ceil", "up", "away_from_zero":
		return RoundCeil
	default:
		return Rounding(strings.ToLower(strings.TrimSpace(s)))
	}
}

func trace(target, source, version string) FieldTrace {
	return FieldTrace{Target: target, Source: source, Version: version}
}

func parseChannels(names []string, path string) ([]Channel, error) {
	out := make([]Channel, 0, len(names))
	for _, name := range names {
		ch, ok := ParseChannel(name)
		if !ok {
			return nil, errf(CodeSchema, path, "channels", "unknown channel %q", name)
		}
		out = append(out, ch)
	}
	return out, nil
}

func objectFromJSON(data []byte, path string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil {
		return nil, errf(CodeSchema, path, "document", "invalid JSON object: %v", err)
	}
	if obj == nil {
		return nil, errf(CodeSchema, path, "document", "must be a JSON object")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, errf(CodeSchema, path, "document", "invalid trailing JSON: %v", err)
		}
		return nil, errf(CodeSchema, path, "document", "trailing JSON content")
	}
	return obj, nil
}

func objectFromRaw(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	if isJSONNull(raw) {
		return nil, errf(CodeSchema, path, "object", "required object is null")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, errf(CodeSchema, path, "object", "must be an object")
	}
	return obj, nil
}

func required(obj map[string]json.RawMessage, key, path string) (json.RawMessage, error) {
	raw, ok := obj[key]
	if !ok || isJSONNull(raw) {
		return nil, errf(CodeSchema, path+"."+key, key, "required field is missing")
	}
	return raw, nil
}

func requiredInt(obj map[string]json.RawMessage, key, path string) (int, error) {
	raw, err := required(obj, key, path)
	if err != nil {
		return 0, err
	}
	return intFromRaw(raw, path+"."+key, key)
}

func intFromRaw(raw json.RawMessage, path, field string) (int, error) {
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, errf(CodeSchema, path, field, "must be an integer")
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, errf(CodeSchema, path, field, "must be an integer")
	}
	if int64(int(value)) != value {
		return 0, errf(CodePara, path, field, "integer is out of range")
	}
	return int(value), nil
}

func requiredString(obj map[string]json.RawMessage, key, path string) (string, error) {
	raw, err := required(obj, key, path)
	if err != nil {
		return "", err
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errf(CodeSchema, path+"."+key, key, "must be a string")
	}
	return value, nil
}

func stringOrInt(raw json.RawMessage, path, field string) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String(), nil
	}
	return "", errf(CodeSchema, path, field, "must be a string or integer")
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
