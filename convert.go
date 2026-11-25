package texture

import (
	"fmt"
)

// Quantize rescales a source sample into a target bit depth. Zero and source
// maximum map exactly to zero and target maximum. The result is always clamped
// into the target range.
func Quantize(value uint16, srcBits, dstBits int, mode Rounding) (uint16, error) {
	if srcBits < 1 || srcBits > 16 {
		return 0, errf(CodePara, "$.src_bits", "src_bits", "must be in [1,16]")
	}
	if dstBits < 1 || dstBits > 16 {
		return 0, errf(CodePara, "$.dst_bits", "dst_bits", "must be in [1,16]")
	}
	if !validRounding(mode) {
		return 0, errf(CodePara, "$.rounding", "rounding", "unsupported rounding %q", mode)
	}
	srcMax := uint32((1 << srcBits) - 1)
	dstMax := uint32((1 << dstBits) - 1)
	if uint32(value) > srcMax {
		return 0, errf(CodePara, "$.value", "value", "value %d exceeds %d-bit maximum %d", value, srcBits, srcMax)
	}
	if value == 0 {
		return 0, nil
	}
	if uint32(value) == srcMax {
		return uint16(dstMax), nil
	}
	product := uint64(value) * uint64(dstMax)
	quotient := product / uint64(srcMax)
	remainder := product % uint64(srcMax)
	switch mode {
	case RoundNearest:
		if remainder*2 >= uint64(srcMax) {
			quotient++
		}
	case RoundCeil:
		if remainder != 0 {
			quotient++
		}
	}
	if quotient > uint64(dstMax) {
		quotient = uint64(dstMax)
	}
	return uint16(quotient), nil
}

// ConversionResult describes a deterministic entry application.
type ConversionResult struct {
	Image           *Image
	AppliedRect     Rect
	AppliedPixels   int
	DroppedChannels []Channel
	InputHash       string
	OutputHash      string
}

// ConvertPlan applies all entries sequentially. Each entry converts the whole
// image but only its coverage region is committed for partial same-format
// stages.
func ConvertPlan(source *Image, plan *Plan) (*Image, []ConversionResult, error) {
	if err := source.Validate(); err != nil {
		return nil, nil, err
	}
	if plan == nil {
		return nil, nil, errf(CodeSchema, "$", "plan", "plan is nil")
	}
	if err := plan.Validate(); err != nil {
		return nil, nil, err
	}
	if source.Width != plan.Source.Width || source.Height != plan.Source.Height ||
		source.BitDepth != plan.Source.BitDepth || !channelsEqual(source.Layout, plan.Source.Layout) {
		return nil, nil, errf(CodePara, "$.source", "source", "image metadata does not match plan source")
	}
	current := source.Clone()
	results := make([]ConversionResult, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		result, err := ApplyEntry(current, plan, entry)
		if err != nil {
			return nil, nil, err
		}
		current = result.Image
		results = append(results, *result)
	}
	return current, results, nil
}

// ApplyEntry converts one stage. For format-preserving partial coverage, only
// the covered rectangle is modified; all other pixels are copied exactly.
func ApplyEntry(input *Image, plan *Plan, entry Entry) (*ConversionResult, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, errf(CodeSchema, "$", "plan", "plan is nil")
	}
	if entry.Index < 0 || entry.Index >= len(plan.Entries) {
		return nil, errf(CodePara, "$.entry.index", "index", "entry index %d out of range", entry.Index)
	}
	if err := validateEntryForInput(entry, input); err != nil {
		return nil, err
	}
	formatChanges := input.BitDepth != entry.TargetBitDepth || !channelsEqual(input.Layout, entry.TargetChannels)
	region := entry.Coverage.Intersection(FullRect(input.Width, input.Height))
	if formatChanges && region != FullRect(input.Width, input.Height) {
		return nil, errf(CodePara, "$.entry.coverage", "coverage", "format-changing entries must cover the full image")
	}
	if region.Empty() {
		return &ConversionResult{Image: input.Clone(), AppliedRect: Rect{}, DroppedChannels: plan.DroppedChannels(entry.Index)}, nil
	}
	var out *Image
	var err error
	if formatChanges {
		out, err = NewImage(input.Width, input.Height, entry.TargetBitDepth, entry.TargetChannels)
		if err != nil {
			return nil, err
		}
	} else {
		out = input.Clone()
	}
	for y := region.Y; y < region.Bottom(); y++ {
		for x := region.X; x < region.Right(); x++ {
			if err := applyPixel(input, out, entry, x, y); err != nil {
				return nil, err
			}
		}
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return &ConversionResult{
		Image: out, AppliedRect: region,
		AppliedPixels:   region.Width * region.Height,
		DroppedChannels: plan.DroppedChannels(entry.Index),
		InputHash:       HashRegion(input, region), OutputHash: out.Hash(),
	}, nil
}

func validateEntryForInput(entry Entry, input *Image) error {
	if err := entry.Coverage.Validate("$.entry.coverage", input.Width, input.Height); err != nil {
		return err
	}
	if entry.TargetBitDepth < 1 || entry.TargetBitDepth > 16 {
		return errf(CodePara, "$.entry.target_bit_depth", "target_bit_depth", "must be in [1,16]")
	}
	if len(entry.TargetChannels) < 1 || len(entry.TargetChannels) > 4 {
		return errf(CodePara, "$.entry.target_channels", "target_channels", "must contain 1..4 channels")
	}
	seenTarget := make(map[Channel]bool)
	for _, ch := range entry.TargetChannels {
		if !validChannel(ch) {
			return errf(CodeSchema, "$.entry.target_channels", "target_channels", "unknown channel %q", ch)
		}
		if seenTarget[ch] {
			return errf(CodeSchema, "$.entry.target_channels", "target_channels", "duplicate channel %q", ch)
		}
		seenTarget[ch] = true
	}
	if len(entry.Mappings) != len(entry.TargetChannels) {
		return errf(CodeSchema, "$.entry.mappings", "mappings", "must have exactly %d entries", len(entry.TargetChannels))
	}
	for i, mapping := range entry.Mappings {
		path := fmt.Sprintf("$.entry.mappings[%d]", i)
		if mapping.Target != entry.TargetChannels[i] {
			return errf(CodeSchema, path+".target", "target", "mapping target %q does not match target channel %q", mapping.Target, entry.TargetChannels[i])
		}
		if (mapping.Fill == nil) == (mapping.Source == "") {
			return errf(CodeSchema, path, "mapping", "exactly one of source or fill is required")
		}
		if mapping.Source != "" && !containsChannel(input.Layout, mapping.Source) {
			return errf(CodePara, path+".source", "source", "input has no channel %q", mapping.Source)
		}
		if mapping.Fill != nil {
			if err := validateFill(*mapping.Fill, path+".fill", entry.TargetBitDepth); err != nil {
				return err
			}
			if mapping.Fill.Kind == FillFromChannel && !containsChannel(input.Layout, mapping.Fill.Channel) {
				return errf(CodePara, path+".fill.channel", "fill.channel", "input has no channel %q", mapping.Fill.Channel)
			}
		}
	}
	if !validRounding(entry.Rounding) {
		return errf(CodePara, "$.entry.rounding", "rounding", "unsupported rounding %q", entry.Rounding)
	}
	if !validAlpha(entry.Alpha) {
		return errf(CodePara, "$.entry.alpha", "alpha", "unsupported alpha mode %q", entry.Alpha)
	}
	if (entry.Alpha == AlphaPremultiply || entry.Alpha == AlphaUnpremultiply) && !containsChannel(entry.TargetChannels, ChA) {
		return errf(CodeSchema, "$.entry.alpha", "alpha", "mode %q requires an output A channel", entry.Alpha)
	}
	return nil
}

func applyPixel(input, out *Image, entry Entry, x, y int) error {
	dstMax := out.MaxValue()
	values := make(map[Channel]uint16, len(entry.Mappings))
	alpha := dstMax
	for _, mapping := range entry.Mappings {
		var v uint16
		if mapping.Fill != nil {
			fill := mapping.Fill
			switch fill.Kind {
			case FillConstant:
				if int(fill.Value) > int(dstMax) {
					return errf(CodePara, "$.fill.value", "fill.value", "value %d exceeds target maximum %d", fill.Value, dstMax)
				}
				v = fill.Value
			case FillZero:
				v = 0
			case FillOne:
				v = dstMax
			case FillFromChannel:
				src, ok := input.ChannelAt(x, y, fill.Channel)
				if !ok {
					return errf(CodePara, "$.fill.channel", "fill.channel", "input has no channel %q", fill.Channel)
				}
				quantized, err := Quantize(src, input.BitDepth, entry.TargetBitDepth, entry.Rounding)
				if err != nil {
					return err
				}
				v = quantized
			default:
				return errf(CodeSchema, "$.fill.kind", "fill.kind", "unsupported fill kind %q", fill.Kind)
			}
		} else {
			src, ok := input.ChannelAt(x, y, mapping.Source)
			if !ok {
				return errf(CodePara, "$.mapping.source", "source", "input has no channel %q", mapping.Source)
			}
			quantized, err := Quantize(src, input.BitDepth, entry.TargetBitDepth, entry.Rounding)
			if err != nil {
				return err
			}
			v = quantized
		}
		values[mapping.Target] = v
		if mapping.Target == ChA {
			alpha = v
		}
	}
	switch entry.Alpha {
	case AlphaDrop:
		if _, ok := values[ChA]; ok {
			values[ChA] = dstMax
			alpha = dstMax
		}
	case AlphaPremultiply:
		for _, ch := range []Channel{ChR, ChG, ChB} {
			if v, ok := values[ch]; ok {
				values[ch] = scaleByAlpha(v, alpha, dstMax)
			}
		}
	case AlphaUnpremultiply:
		if alpha == 0 {
			for _, ch := range []Channel{ChR, ChG, ChB} {
				if _, ok := values[ch]; ok {
					values[ch] = 0
				}
			}
		} else {
			for _, ch := range []Channel{ChR, ChG, ChB} {
				if v, ok := values[ch]; ok {
					values[ch] = unscaleByAlpha(v, alpha, dstMax)
				}
			}
		}
	}
	for c, ch := range out.Layout {
		out.Set(x, y, c, values[ch])
	}
	return nil
}

func scaleByAlpha(value, alpha, maxValue uint16) uint16 {
	if value == 0 || alpha == 0 {
		return 0
	}
	product := uint64(value) * uint64(alpha)
	denominator := uint64(maxValue)
	rounded := (product + denominator/2) / denominator
	if rounded > uint64(maxValue) {
		rounded = uint64(maxValue)
	}
	return uint16(rounded)
}

func unscaleByAlpha(value, alpha, maxValue uint16) uint16 {
	if value == 0 || alpha == 0 {
		return 0
	}
	product := uint64(value)*uint64(maxValue) + uint64(alpha)/2
	result := product / uint64(alpha)
	if result > uint64(maxValue) {
		result = uint64(maxValue)
	}
	return uint16(result)
}

// DescribeEntry returns a stable human-readable summary used in reports.
func DescribeEntry(entry Entry) string {
	parts := make([]string, 0, len(entry.Mappings))
	for _, mapping := range entry.Mappings {
		if mapping.Fill == nil {
			parts = append(parts, fmt.Sprintf("%s<-%s", mapping.Target, mapping.Source))
		} else {
			parts = append(parts, fmt.Sprintf("%s<-%s", mapping.Target, canonicalFill(*mapping.Fill)))
		}
	}
	return fmt.Sprintf("entry %d: %v @ %dx%d+%d+%d depth=%d alpha=%s rounding=%s",
		entry.Index, parts, entry.Coverage.Width, entry.Coverage.Height,
		entry.Coverage.X, entry.Coverage.Y, entry.TargetBitDepth, entry.Alpha, entry.Rounding)
}
