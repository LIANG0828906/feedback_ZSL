package tex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	FillZero         = "zero"
	FillRepeatAlpha  = "repeat_alpha"
	AlgorithmVersion = "quant-v1"
)

type Descriptor struct {
	ID             string `json:"id"`
	SourceImage    string `json:"source_image,omitempty"`
	SourceLayout   Layout `json:"source_layout"`
	TargetLayout   Layout `json:"target_layout"`
	TargetBitDepth int    `json:"target_bit_depth"`
	Permutation    []int  `json:"permutation"`
	Fill           string `json:"fill"`
}

func (d Descriptor) Normalize() Descriptor {
	if d.ID == "" {
		d.ID = "descriptor"
	}
	if d.SourceLayout != "" {
		d.SourceLayout = Layout(strings.ToLower(strings.TrimSpace(string(d.SourceLayout))))
	}
	if d.TargetLayout != "" {
		d.TargetLayout = Layout(strings.ToLower(strings.TrimSpace(string(d.TargetLayout))))
	}
	d.Fill = strings.ToLower(strings.TrimSpace(d.Fill))
	return d
}

func (d Descriptor) Validate() error {
	d = d.Normalize()
	if d.SourceLayout == "" {
		return ErrPara("source_layout", "required field is missing")
	}
	if !d.SourceLayout.Valid() {
		return ErrPara("source_layout", fmt.Sprintf("unsupported channel layout %q", d.SourceLayout))
	}
	if d.TargetLayout == "" {
		return ErrPara("target_layout", "required field is missing")
	}
	if !d.TargetLayout.Valid() {
		return ErrPara("target_layout", fmt.Sprintf("unsupported channel layout %q", d.TargetLayout))
	}
	if d.TargetBitDepth == 0 {
		return ErrPara("target_bit_depth", "required field is missing")
	}
	if d.TargetBitDepth != 8 && d.TargetBitDepth != 16 {
		return ErrPara("target_bit_depth", fmt.Sprintf("must be 8 or 16, got %d", d.TargetBitDepth))
	}
	if d.Fill == "" {
		return ErrPara("fill", "required field is missing")
	}
	if d.Fill != FillZero && d.Fill != FillRepeatAlpha {
		return ErrPara("fill", fmt.Sprintf("must be %q or %q, got %q", FillZero, FillRepeatAlpha, d.Fill))
	}
	if len(d.Permutation) == 0 {
		return ErrPara("permutation", "required field is missing")
	}
	sourceChannels := d.SourceLayout.ChannelCount()
	if len(d.Permutation) != sourceChannels {
		return ErrLayout("permutation", -1, fmt.Sprintf("length must equal source channel count %d", sourceChannels))
	}
	seen := make(map[int]bool, sourceChannels)
	for _, channel := range d.Permutation {
		if channel < 0 || channel >= sourceChannels {
			return ErrLayout("permutation", channel, fmt.Sprintf("index outside source channel range [0,%d)", sourceChannels))
		}
		if seen[channel] {
			return ErrLayout("permutation", channel, "duplicate channel index in permutation")
		}
		seen[channel] = true
	}
	targetChannels := d.TargetLayout.ChannelCount()
	if d.Fill == FillRepeatAlpha {
		if !d.TargetLayout.HasAlpha() {
			return ErrPara("fill", "repeat_alpha requires a target layout with an alpha channel")
		}
		if targetChannels < sourceChannels {
			return ErrPara("fill", "repeat_alpha is only compatible with non-decreasing target channel count")
		}
	}
	return nil
}

func (d Descriptor) CanonicalBytes() []byte {
	d = d.Normalize()
	payload := struct {
		ID             string `json:"id"`
		SourceImage    string `json:"source_image,omitempty"`
		SourceLayout   Layout `json:"source_layout"`
		TargetLayout   Layout `json:"target_layout"`
		TargetBitDepth int    `json:"target_bit_depth"`
		Permutation    []int  `json:"permutation"`
		Fill           string `json:"fill"`
	}{
		ID:             d.ID,
		SourceImage:    d.SourceImage,
		SourceLayout:   d.SourceLayout,
		TargetLayout:   d.TargetLayout,
		TargetBitDepth: d.TargetBitDepth,
		Permutation:    append([]int(nil), d.Permutation...),
		Fill:           d.Fill,
	}
	data, _ := json.Marshal(payload)
	return data
}

func (d Descriptor) Version() string {
	sum := sha256.Sum256(d.CanonicalBytes())
	return hex.EncodeToString(sum[:])
}

func (d Descriptor) ValidateForImage(img *Image) error {
	d = d.Normalize()
	if err := d.Validate(); err != nil {
		return err
	}
	if d.SourceLayout != img.Layout {
		return ErrPara("source_layout", fmt.Sprintf("descriptor says %s but image is %s", d.SourceLayout, img.Layout))
	}
	if strings.TrimSpace(d.SourceImage) != "" && img.ID != "" && d.SourceImage != img.ID {
		return ErrPara("source_image", fmt.Sprintf("descriptor targets %q but image id is %q", d.SourceImage, img.ID))
	}
	return nil
}

type ChannelLink struct {
	Target ChannelRef  `json:"target"`
	Source *ChannelRef `json:"source,omitempty"`
	Kind   string      `json:"kind"`
}

type Mapping struct {
	SourceLayout   Layout        `json:"source_layout"`
	TargetLayout   Layout        `json:"target_layout"`
	TargetToSource []int         `json:"target_to_source"`
	Links          []ChannelLink `json:"links"`
	Dropped        []ChannelRef  `json:"dropped_channels"`
	AlphaSource    *ChannelRef   `json:"alpha_source,omitempty"`
	Fill           string        `json:"fill"`
	Reversible     bool          `json:"reversible"`
}

func (m Mapping) SourceFromTarget() ([]int, error) {
	if len(m.Dropped) > 0 {
		return nil, ErrLoss(m.Dropped)
	}
	sourceChannels := m.SourceLayout.ChannelCount()
	recovered := make([]int, sourceChannels)
	for i := range recovered {
		recovered[i] = -1
	}
	for targetIndex, sourceIndex := range m.TargetToSource {
		if sourceIndex >= 0 {
			recovered[sourceIndex] = targetIndex
		}
	}
	for sourceIndex, targetIndex := range recovered {
		if targetIndex < 0 {
			name, _ := m.SourceLayout.ChannelAt(sourceIndex)
			return nil, ErrLoss([]ChannelRef{{Index: sourceIndex, Name: name}})
		}
	}
	return recovered, nil
}

func BuildMapping(desc Descriptor) (Mapping, error) {
	desc = desc.Normalize()
	if err := desc.Validate(); err != nil {
		return Mapping{}, err
	}
	sourceChannels := desc.SourceLayout.ChannelCount()
	targetChannels := desc.TargetLayout.ChannelCount()
	mappedCount := sourceChannels
	if targetChannels < mappedCount {
		mappedCount = targetChannels
	}
	mapping := Mapping{
		SourceLayout:   desc.SourceLayout,
		TargetLayout:   desc.TargetLayout,
		TargetToSource: make([]int, targetChannels),
		Fill:           desc.Fill,
	}
	for i := range mapping.TargetToSource {
		mapping.TargetToSource[i] = -1
	}
	used := make([]bool, sourceChannels)
	for targetIndex := 0; targetIndex < mappedCount; targetIndex++ {
		sourceIndex := desc.Permutation[targetIndex]
		mapping.TargetToSource[targetIndex] = sourceIndex
		used[sourceIndex] = true
		targetName, _ := desc.TargetLayout.ChannelAt(targetIndex)
		sourceName, _ := desc.SourceLayout.ChannelAt(sourceIndex)
		src := ChannelRef{Index: sourceIndex, Name: sourceName}
		mapping.Links = append(mapping.Links, ChannelLink{
			Target: ChannelRef{Index: targetIndex, Name: targetName},
			Source: &src,
			Kind:   "mapped",
		})
	}
	for sourceIndex, wasUsed := range used {
		if !wasUsed {
			name, _ := desc.SourceLayout.ChannelAt(sourceIndex)
			mapping.Dropped = append(mapping.Dropped, ChannelRef{Index: sourceIndex, Name: name})
		}
	}
	sort.Slice(mapping.Dropped, func(i, j int) bool {
		return mapping.Dropped[i].Index < mapping.Dropped[j].Index
	})

	if desc.Fill == FillRepeatAlpha {
		alphaSource := nearestAlphaSource(desc.SourceLayout, desc.TargetLayout)
		mapping.AlphaSource = &alphaSource
	}
	for targetIndex := mappedCount; targetIndex < targetChannels; targetIndex++ {
		targetName, _ := desc.TargetLayout.ChannelAt(targetIndex)
		link := ChannelLink{
			Target: ChannelRef{Index: targetIndex, Name: targetName},
			Kind:   "fill_zero",
		}
		if desc.Fill == FillRepeatAlpha && mapping.AlphaSource != nil {
			src := *mapping.AlphaSource
			link.Source = &src
			link.Kind = "fill_repeat_alpha"
		}
		mapping.Links = append(mapping.Links, link)
	}
	mapping.Reversible = len(mapping.Dropped) == 0
	return mapping, nil
}

func nearestAlphaSource(sourceLayout, targetLayout Layout) ChannelRef {
	if sourceLayout.HasAlpha() {
		index := sourceLayout.AlphaIndex()
		name, _ := sourceLayout.ChannelAt(index)
		return ChannelRef{Index: index, Name: name}
	}
	targetAlpha := targetLayout.AlphaIndex()
	if targetAlpha > 0 {
		previousName, _ := targetLayout.ChannelAt(targetAlpha - 1)
		if sourceIndex := sourceLayout.IndexOf(previousName); sourceIndex >= 0 {
			name, _ := sourceLayout.ChannelAt(sourceIndex)
			return ChannelRef{Index: sourceIndex, Name: name}
		}
	}
	name, _ := sourceLayout.ChannelAt(0)
	return ChannelRef{Index: 0, Name: name}
}
