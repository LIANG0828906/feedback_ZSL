package tex

import "fmt"

type Layout string

const (
	LayoutGray Layout = "gray"
	LayoutRGB  Layout = "rgb"
	LayoutRGBA Layout = "rgba"
	LayoutBGR  Layout = "bgr"
	LayoutBGRA Layout = "bgra"
)

var layoutChannels = map[Layout][]string{
	LayoutGray: {"Y"},
	LayoutRGB:  {"R", "G", "B"},
	LayoutRGBA: {"R", "G", "B", "A"},
	LayoutBGR:  {"B", "G", "R"},
	LayoutBGRA: {"B", "G", "R", "A"},
}

type ChannelRef struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
}

func (c ChannelRef) String() string {
	if c.Name == "" {
		return fmt.Sprintf("src[%d]", c.Index)
	}
	return fmt.Sprintf("%s(src[%d])", c.Name, c.Index)
}

func (l Layout) Valid() bool {
	_, ok := layoutChannels[l]
	return ok
}

func (l Layout) Channels() []string {
	channels := layoutChannels[l]
	out := make([]string, len(channels))
	copy(out, channels)
	return out
}

func (l Layout) ChannelCount() int {
	return len(layoutChannels[l])
}

func (l Layout) HasAlpha() bool {
	for _, name := range layoutChannels[l] {
		if name == "A" {
			return true
		}
	}
	return false
}

func (l Layout) AlphaIndex() int {
	for i, name := range layoutChannels[l] {
		if name == "A" {
			return i
		}
	}
	return -1
}

func (l Layout) ChannelAt(index int) (string, bool) {
	channels := layoutChannels[l]
	if index < 0 || index >= len(channels) {
		return "", false
	}
	return channels[index], true
}

func (l Layout) IndexOf(name string) int {
	for i, channel := range layoutChannels[l] {
		if channel == name {
			return i
		}
	}
	return -1
}

func ParseLayout(value string) (Layout, error) {
	layout := Layout(value)
	if !layout.Valid() {
		return "", ErrPara("layout", fmt.Sprintf("unsupported channel layout %q", value))
	}
	return layout, nil
}
