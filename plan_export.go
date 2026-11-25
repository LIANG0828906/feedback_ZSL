package texture

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ExportPlan serializes the normalized plan into a supported target version.
// When targetVersion cannot express a field, the returned bytes still contain a
// deterministic downgrade, and the returned error has code E_LOSSY.
func ExportPlan(p *Plan, targetVersion string) ([]byte, []Loss, error) {
	if p == nil {
		return nil, nil, errf(CodeSchema, "$", "plan", "plan is nil")
	}
	if err := p.Validate(); err != nil {
		return nil, nil, err
	}
	version, err := normalizeTargetVersion(targetVersion)
	if err != nil {
		return nil, nil, err
	}
	switch version {
	case "1":
		return exportV1(p)
	case "2":
		return exportV2(p)
	case "3":
		return exportV3(p)
	default:
		return nil, nil, errf(CodeVersion, "$.version", "version", "unknown target version %q", targetVersion)
	}
}

func normalizeTargetVersion(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "v1", "version1", "texture-plan/1":
		return "1", nil
	case "2", "v2", "version2", "texture-plan/2":
		return "2", nil
	case "3", "v3", "version3", "texture-plan/3":
		return "3", nil
	default:
		return "", errf(CodeVersion, "$.target_version", "target_version", "unsupported target version %q", v)
	}
}

func exportV1(p *Plan) ([]byte, []Loss, error) {
	var losses []Loss
	type v1Fill struct {
		Mode          string   `json:"mode"`
		Value         *int     `json:"value,omitempty"`
		SourceChannel string   `json:"source_channel,omitempty"`
		Channels      []string `json:"channels"`
	}
	type v1Entry struct {
		Index          int       `json:"index"`
		Coverage       Rect      `json:"coverage"`
		TargetBitDepth int       `json:"target_bit_depth"`
		ChannelOrder   []string  `json:"channel_order"`
		SourceOrder    []*string `json:"source_order"`
		Fill           *v1Fill   `json:"fill,omitempty"`
		Rounding       string    `json:"rounding"`
		Alpha          string    `json:"alpha"`
	}
	doc := struct {
		Version string    `json:"version"`
		Source  v1Source  `json:"source"`
		Entries []v1Entry `json:"entries"`
	}{
		Version: "1",
		Source: v1Source{
			Width: p.Source.Width, Height: p.Source.Height,
			BitDepth: p.Source.BitDepth, Layout: LayoutString(p.Source.Layout),
		},
	}
	for _, e := range p.Entries {
		item := v1Entry{
			Index: e.Index, Coverage: e.Coverage, TargetBitDepth: e.TargetBitDepth,
			Rounding: string(e.Rounding), Alpha: string(e.Alpha),
		}
		for _, ch := range e.TargetChannels {
			item.ChannelOrder = append(item.ChannelOrder, string(ch))
		}
		for _, mapping := range e.Mappings {
			if mapping.Fill != nil {
				item.SourceOrder = append(item.SourceOrder, nil)
				continue
			}
			source := string(mapping.Source)
			item.SourceOrder = append(item.SourceOrder, &source)
		}
		fillChannels, fillRules := collectFills(e)
		if len(fillChannels) > 0 {
			rule := fillRules[0]
			canonical := canonicalFill(rule)
			if len(fillRules) > 1 {
				for i := 1; i < len(fillRules); i++ {
					if canonicalFill(fillRules[i]) != canonical {
						losses = append(losses, Loss{
							Path:      fmt.Sprintf("$.entries[%d].mappings.fill", e.Index),
							Original:  fillRules[i],
							Downgrade: rule,
							Reason:    "version 1 supports one fill rule per entry",
						})
					}
				}
			}
			vf := &v1Fill{Mode: string(rule.Kind), Channels: channelStrings(fillChannels)}
			switch rule.Kind {
			case FillConstant:
				value := int(rule.Value)
				vf.Value = &value
			case FillFromChannel:
				vf.SourceChannel = string(rule.Channel)
			}
			item.Fill = vf
		}
		switch e.Alpha {
		case AlphaCopy, AlphaDrop:
		case AlphaStraight:
			losses = append(losses, Loss{
				Path: fmt.Sprintf("$.entries[%d].alpha", e.Index), Original: e.Alpha,
				Downgrade: AlphaCopy, Reason: "version 1 has no straight-alpha convention",
			})
			item.Alpha = string(AlphaCopy)
		default:
			losses = append(losses, Loss{
				Path: fmt.Sprintf("$.entries[%d].alpha", e.Index), Original: e.Alpha,
				Downgrade: AlphaCopy, Reason: "version 1 only supports copy/drop alpha",
			})
			item.Alpha = string(AlphaCopy)
		}
		doc.Entries = append(doc.Entries, item)
	}
	return finishExport(doc, losses, "1")
}

func exportV2(p *Plan) ([]byte, []Loss, error) {
	var losses []Loss
	type v2Fill struct {
		Channel string `json:"channel"`
		Kind    string `json:"kind"`
		Value   *int   `json:"value,omitempty"`
		Source  string `json:"source,omitempty"`
	}
	type v2Output struct {
		Depth    int      `json:"depth"`
		Layout   []string `json:"layout"`
		Rounding string   `json:"rounding"`
	}
	type v2Entry struct {
		Step       int               `json:"step"`
		Region     map[string]int    `json:"region"`
		Output     v2Output          `json:"output"`
		ChannelMap map[string]string `json:"channel_map,omitempty"`
		Fill       []v2Fill          `json:"fill_sources,omitempty"`
		Alpha      map[string]string `json:"alpha"`
	}
	doc := struct {
		SchemaVersion int       `json:"schema_version"`
		Image         v2Image   `json:"image"`
		Operations    []v2Entry `json:"operations"`
	}{
		SchemaVersion: 2,
		Image: v2Image{
			W: p.Source.Width, H: p.Source.Height,
			Depth:    map[string]int{"bits": p.Source.BitDepth},
			Channels: channelStrings(p.Source.Layout),
		},
	}
	for _, e := range p.Entries {
		item := v2Entry{
			Step: e.Index,
			Region: map[string]int{
				"left": e.Coverage.X, "top": e.Coverage.Y,
				"right": e.Coverage.Right(), "bottom": e.Coverage.Bottom(),
			},
			Output: v2Output{
				Depth: e.TargetBitDepth, Layout: channelStrings(e.TargetChannels),
				Rounding: string(e.Rounding),
			},
			ChannelMap: make(map[string]string),
			Alpha:      map[string]string{"mode": string(e.Alpha)},
		}
		for _, m := range e.Mappings {
			if m.Fill != nil {
				item.Fill = append(item.Fill, v2Fill{
					Channel: string(m.Target), Kind: string(m.Fill.Kind),
					Value: intPtrForFill(m.Fill), Source: string(m.Fill.Channel),
				})
				continue
			}
			item.ChannelMap[string(m.Target)] = string(m.Source)
		}
		if len(item.ChannelMap) == 0 {
			item.ChannelMap = nil
		}
		switch e.Alpha {
		case AlphaCopy, AlphaDrop, AlphaStraight:
		default:
			losses = append(losses, Loss{
				Path: fmt.Sprintf("$.entries[%d].alpha", e.Index), Original: e.Alpha,
				Downgrade: AlphaCopy, Reason: "version 2 has no premultiply/unpremultiply convention",
			})
			item.Alpha["mode"] = string(AlphaCopy)
		}
		doc.Operations = append(doc.Operations, item)
	}
	return finishExport(doc, losses, "2")
}

func exportV3(p *Plan) ([]byte, []Loss, error) {
	type v3FillSrc struct {
		Kind    string `json:"kind"`
		Value   *int   `json:"value,omitempty"`
		Channel string `json:"channel,omitempty"`
	}
	type v3Padding struct {
		Channels []string  `json:"channels"`
		Source   v3FillSrc `json:"source"`
	}
	type v3Output struct {
		BitDepth int      `json:"bit_depth"`
		Channels []string `json:"channels"`
	}
	type v3Entry struct {
		Seq          int               `json:"seq"`
		Area         map[string]int    `json:"area"`
		Output       v3Output          `json:"output"`
		SourceOrder  []*string         `json:"source_order"`
		Padding      []v3Padding       `json:"padding,omitempty"`
		Quantization string            `json:"quantization"`
		AlphaPolicy  map[string]string `json:"alpha_policy"`
	}
	doc := struct {
		Format  string    `json:"format"`
		Source  v3Source  `json:"source"`
		Entries []v3Entry `json:"entries"`
	}{
		Format: "texture-plan/3",
		Source: v3Source{
			Width: p.Source.Width, Height: p.Source.Height,
			PixelFormat: strings.ToLower(LayoutString(p.Source.Layout)) + fmt.Sprint(p.Source.BitDepth),
		},
	}
	for _, e := range p.Entries {
		item := v3Entry{
			Seq: e.Index,
			Area: map[string]int{
				"x": e.Coverage.X, "y": e.Coverage.Y,
				"w": e.Coverage.Width, "h": e.Coverage.Height,
			},
			Output:       v3Output{BitDepth: e.TargetBitDepth, Channels: channelStrings(e.TargetChannels)},
			Quantization: string(e.Rounding),
			AlphaPolicy:  map[string]string{"mode": string(e.Alpha)},
		}
		groupIndex := make(map[string]int)
		for i, target := range e.TargetChannels {
			m := e.Mappings[i]
			if m.Fill == nil {
				source := string(m.Source)
				item.SourceOrder = append(item.SourceOrder, &source)
				continue
			}
			item.SourceOrder = append(item.SourceOrder, nil)
			key := canonicalFill(*m.Fill)
			idx, ok := groupIndex[key]
			if !ok {
				idx = len(item.Padding)
				groupIndex[key] = idx
				item.Padding = append(item.Padding, v3Padding{
					Source: v3FillSrc{
						Kind: string(m.Fill.Kind), Value: intPtrForFill(m.Fill),
						Channel: string(m.Fill.Channel),
					},
				})
			}
			item.Padding[idx].Channels = append(item.Padding[idx].Channels, string(target))
		}
		doc.Entries = append(doc.Entries, item)
	}
	return finishExport(doc, nil, "3")
}

type v1Source struct {
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	BitDepth int    `json:"bit_depth"`
	Layout   string `json:"layout"`
}

type v2Image struct {
	W        int            `json:"w"`
	H        int            `json:"h"`
	Depth    map[string]int `json:"depth"`
	Channels []string       `json:"channels"`
}

type v3Source struct {
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	PixelFormat string `json:"pixel_format"`
}

func finishExport(doc any, losses []Loss, version string) ([]byte, []Loss, error) {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, nil, errf(CodeSchema, "$", "document", "cannot marshal version %s: %v", version, err)
	}
	data = append(data, '\n')
	if _, err := LoadPlan(data); err != nil {
		return nil, losses, err
	}
	if len(losses) > 0 {
		return data, losses, &KernelError{
			Code: CodeLossy, Path: "$", Message: fmt.Sprintf("version %s export degraded %d field(s)", version, len(losses)),
			Losses: losses,
		}
	}
	return data, nil, nil
}

func collectFills(e Entry) ([]Channel, []FillSource) {
	var channels []Channel
	var rules []FillSource
	seen := make(map[string]bool)
	for _, m := range e.Mappings {
		if m.Fill == nil {
			continue
		}
		channels = append(channels, m.Target)
		key := canonicalFill(*m.Fill)
		if !seen[key] {
			seen[key] = true
			rules = append(rules, *m.Fill)
		}
	}
	return channels, rules
}

func canonicalFill(f FillSource) string {
	return fmt.Sprintf("%s/%d/%s", f.Kind, f.Value, f.Channel)
}

func channelStrings(layout []Channel) []string {
	out := make([]string, len(layout))
	for i, ch := range layout {
		out[i] = string(ch)
	}
	return out
}

func intPtrForFill(f *FillSource) *int {
	if f == nil || f.Kind != FillConstant {
		return nil
	}
	v := int(f.Value)
	return &v
}

// EquivalentForExport compares the fields that every version must preserve:
// target depth, ordered channels, mappings, fills, and rounding.
func EquivalentForExport(a, b *Plan) error {
	if a == nil || b == nil {
		return errf(CodeSchema, "$", "plan", "nil plan")
	}
	if !sourceEqual(a.Source, b.Source) {
		return errf(CodeStore, "$.source", "source", "source metadata differs after round trip")
	}
	if len(a.Entries) != len(b.Entries) {
		return errf(CodeStore, "$.entries", "entries", "entry count differs")
	}
	for i := range a.Entries {
		ae, be := a.Entries[i], b.Entries[i]
		path := fmt.Sprintf("$.entries[%d]", i)
		if ae.Index != be.Index || ae.Coverage != be.Coverage || ae.TargetBitDepth != be.TargetBitDepth ||
			!channelsEqual(ae.TargetChannels, be.TargetChannels) || ae.Rounding != be.Rounding ||
			len(ae.Mappings) != len(be.Mappings) {
			return errf(CodeStore, path, "entry", "entry metadata differs after round trip")
		}
		for j := range ae.Mappings {
			am, bm := ae.Mappings[j], be.Mappings[j]
			if am.Target != bm.Target || am.Source != bm.Source {
				return errf(CodeStore, path+".mappings", "mappings", "mapping differs after round trip")
			}
			if (am.Fill == nil) != (bm.Fill == nil) {
				return errf(CodeStore, path+".mappings", "fill", "fill presence differs after round trip")
			}
			if am.Fill != nil && canonicalFill(*am.Fill) != canonicalFill(*bm.Fill) {
				return errf(CodeStore, path+".mappings", "fill", "fill rule differs after round trip")
			}
		}
	}
	return nil
}

func sourceEqual(a, b Source) bool {
	return a.Width == b.Width && a.Height == b.Height && a.BitDepth == b.BitDepth &&
		channelsEqual(a.Layout, b.Layout)
}
