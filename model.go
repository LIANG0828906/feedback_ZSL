package texture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	AlgorithmVersion    = "texture-kernel/1"
	DefaultParamVersion = "params/1"
)

// Channel names are semantic channel identifiers, not byte offsets.
type Channel string

const (
	ChR Channel = "R"
	ChG Channel = "G"
	ChB Channel = "B"
	ChA Channel = "A"
)

func ParseChannel(s string) (Channel, bool) {
	ch := Channel(strings.ToUpper(strings.TrimSpace(s)))
	switch ch {
	case ChR, ChG, ChB, ChA:
		return ch, true
	default:
		return "", false
	}
}

// Rect uses a half-open coordinate system: [X,X+Width) x [Y,Y+Height).
type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func FullRect(width, height int) Rect {
	return Rect{X: 0, Y: 0, Width: width, Height: height}
}

func (r Rect) Empty() bool { return r.Width <= 0 || r.Height <= 0 }

func (r Rect) Right() int  { return r.X + r.Width }
func (r Rect) Bottom() int { return r.Y + r.Height }

func (r Rect) Intersects(o Rect) bool {
	return r.X < o.Right() && o.X < r.Right() && r.Y < o.Bottom() && o.Y < r.Bottom()
}

func (r Rect) Intersection(o Rect) Rect {
	x0 := max(r.X, o.X)
	y0 := max(r.Y, o.Y)
	x1 := min(r.Right(), o.Right())
	y1 := min(r.Bottom(), o.Bottom())
	if x1 <= x0 || y1 <= y0 {
		return Rect{}
	}
	return Rect{X: x0, Y: y0, Width: x1 - x0, Height: y1 - y0}
}

func (r Rect) Contains(o Rect) bool {
	return o.X >= r.X && o.Y >= r.Y && o.Right() <= r.Right() && o.Bottom() <= r.Bottom()
}

func (r Rect) Validate(path string, width, height int) error {
	if r.X < 0 {
		return errf(CodePara, path+".x", "x", "must be >= 0")
	}
	if r.Y < 0 {
		return errf(CodePara, path+".y", "y", "must be >= 0")
	}
	if r.Width <= 0 {
		return errf(CodePara, path+".width", "width", "must be > 0")
	}
	if r.Height <= 0 {
		return errf(CodePara, path+".height", "height", "must be > 0")
	}
	if r.Right() > width || r.Bottom() > height {
		return errf(CodePara, path, "coverage", "rectangle exceeds image bounds %dx%d", width, height)
	}
	return nil
}

type FillKind string

const (
	FillConstant    FillKind = "constant"
	FillZero        FillKind = "zero"
	FillOne         FillKind = "one"
	FillFromChannel FillKind = "source"
)

// FillSource describes how a target channel that is not copied from the same
// named input channel obtains its value.
type FillSource struct {
	Kind    FillKind `json:"kind"`
	Value   uint16   `json:"value,omitempty"`
	Channel Channel  `json:"channel,omitempty"`
}

// ChannelMapping maps one target channel to either an input channel or a fill.
// Exactly one of Source and Fill is active.
type ChannelMapping struct {
	Target Channel     `json:"target"`
	Source Channel     `json:"source,omitempty"`
	Fill   *FillSource `json:"fill,omitempty"`
}

type AlphaMode string

const (
	AlphaCopy          AlphaMode = "copy"
	AlphaDrop          AlphaMode = "drop"
	AlphaStraight      AlphaMode = "straight"
	AlphaPremultiply   AlphaMode = "premultiply"
	AlphaUnpremultiply AlphaMode = "unpremultiply"
)

type Rounding string

const (
	RoundNearest Rounding = "nearest"
	RoundFloor   Rounding = "floor"
	RoundCeil    Rounding = "ceil"
)

type FieldTrace struct {
	Target  string `json:"target"`
	Source  string `json:"source"`
	Version string `json:"version"`
	Note    string `json:"note,omitempty"`
}

// Source describes the source image in the normalized model.
type Source struct {
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	BitDepth int       `json:"bit_depth"`
	Layout   []Channel `json:"layout"`
}

// Entry is one deterministic conversion stage. TargetChannels is the ordered
// output layout. Mappings is parallel to TargetChannels.
type Entry struct {
	Index          int              `json:"index"`
	Coverage       Rect             `json:"coverage"`
	TargetBitDepth int              `json:"target_bit_depth"`
	TargetChannels []Channel        `json:"target_channels"`
	Mappings       []ChannelMapping `json:"mappings"`
	Rounding       Rounding         `json:"rounding"`
	Alpha          AlphaMode        `json:"alpha"`
	Traces         []FieldTrace     `json:"traces,omitempty"`
}

func (e Entry) ChannelTarget(name Channel) (Channel, bool) {
	for _, m := range e.Mappings {
		if m.Target == name {
			if m.Fill != nil {
				return "", false
			}
			return m.Source, true
		}
	}
	return "", false
}

func (e Entry) InputLayout(plan Source) []Channel {
	if e.Index == 0 {
		return append([]Channel(nil), plan.Layout...)
	}
	return nil
}

// Plan is the version-independent model. Version and Traces preserve exactly
// where each normalized field came from.
type Plan struct {
	Version     string       `json:"version"`
	Source      Source       `json:"source"`
	Entries     []Entry      `json:"entries"`
	Traces      []FieldTrace `json:"traces,omitempty"`
	Fingerprint string       `json:"fingerprint,omitempty"`
}

func (p *Plan) Clone() *Plan {
	if p == nil {
		return nil
	}
	b, _ := json.Marshal(p)
	var out Plan
	_ = json.Unmarshal(b, &out)
	return &out
}

// RecomputeFingerprint calculates a canonical content fingerprint. Traces are
// deliberately excluded because they are provenance, not conversion content.
func (p *Plan) RecomputeFingerprint() string {
	p.Fingerprint = p.FingerprintValue()
	return p.Fingerprint
}

// FingerprintValue computes the canonical fingerprint without mutating the
// plan, so execution workers can safely call it while commits proceed.
func (p *Plan) FingerprintValue() string {
	type canonicalEntry struct {
		Index          int              `json:"index"`
		Coverage       Rect             `json:"coverage"`
		TargetBitDepth int              `json:"target_bit_depth"`
		TargetChannels []Channel        `json:"target_channels"`
		Mappings       []ChannelMapping `json:"mappings"`
		Rounding       Rounding         `json:"rounding"`
		Alpha          AlphaMode        `json:"alpha"`
	}
	payload := struct {
		Source  Source           `json:"source"`
		Entries []canonicalEntry `json:"entries"`
	}{Source: p.Source}
	for _, e := range p.Entries {
		payload.Entries = append(payload.Entries, canonicalEntry{
			Index: e.Index, Coverage: e.Coverage, TargetBitDepth: e.TargetBitDepth,
			TargetChannels: e.TargetChannels, Mappings: e.Mappings,
			Rounding: e.Rounding, Alpha: e.Alpha,
		})
	}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p *Plan) InputLayout(entryIndex int) []Channel {
	if entryIndex <= 0 {
		return append([]Channel(nil), p.Source.Layout...)
	}
	return append([]Channel(nil), p.Entries[entryIndex-1].TargetChannels...)
}

func (p *Plan) InputBitDepth(entryIndex int) int {
	if entryIndex <= 0 {
		return p.Source.BitDepth
	}
	return p.Entries[entryIndex-1].TargetBitDepth
}

// DroppedChannels returns source channels omitted from an entry's mappings.
func (p *Plan) DroppedChannels(entryIndex int) []Channel {
	if entryIndex < 0 || entryIndex >= len(p.Entries) {
		return nil
	}
	input := p.InputLayout(entryIndex)
	used := make(map[Channel]bool)
	e := p.Entries[entryIndex]
	for _, m := range e.Mappings {
		if m.Fill == nil {
			used[m.Source] = true
		} else if m.Fill.Kind == FillFromChannel {
			used[m.Fill.Channel] = true
		}
	}
	var dropped []Channel
	for _, ch := range input {
		if !used[ch] {
			dropped = append(dropped, ch)
		}
	}
	return dropped
}

// Validate normalizes and validates a loaded plan. All failures identify the
// offending field path.
func (p *Plan) Validate() error {
	if p == nil {
		return errf(CodeSchema, "$", "plan", "plan is nil")
	}
	if p.Version == "" {
		return errf(CodeVersion, "$", "version", "source version is missing")
	}
	if err := validateSource(p.Source); err != nil {
		return err
	}
	if len(p.Entries) == 0 {
		return errf(CodeSchema, "$.entries", "entries", "at least one entry is required")
	}
	seenIndex := make(map[int]bool)
	for i := range p.Entries {
		e := &p.Entries[i]
		path := fmt.Sprintf("$.entries[%d]", i)
		if e.Index != i {
			return errf(CodeSchema, path+".index", "index", "entry indexes must be contiguous and ordered from 0; got %d at position %d", e.Index, i)
		}
		if seenIndex[e.Index] {
			return errf(CodeSchema, path+".index", "index", "duplicate entry index %d", e.Index)
		}
		seenIndex[e.Index] = true
		if e.Index < 0 {
			return errf(CodePara, path+".index", "index", "must be >= 0")
		}
		if err := e.Coverage.Validate(path+".coverage", p.Source.Width, p.Source.Height); err != nil {
			return err
		}
		if e.TargetBitDepth < 1 || e.TargetBitDepth > 16 {
			return errf(CodePara, path+".target_bit_depth", "target_bit_depth", "must be in [1,16]")
		}
		if e.Alpha == AlphaPremultiply || e.Alpha == AlphaUnpremultiply || e.Alpha == AlphaStraight {
			if e.Alpha == AlphaPremultiply || e.Alpha == AlphaUnpremultiply {
				if !containsChannel(e.TargetChannels, ChA) {
					return errf(CodeSchema, path+".alpha", "alpha", "mode %q requires an output A channel", e.Alpha)
				}
			}
		}
		if len(e.TargetChannels) < 1 || len(e.TargetChannels) > 4 {
			return errf(CodePara, path+".target_channels", "target_channels", "must contain 1..4 channels")
		}
		seenCh := make(map[Channel]bool)
		for _, ch := range e.TargetChannels {
			if !validChannel(ch) {
				return errf(CodeSchema, path+".target_channels", "target_channels", "unknown channel %q", ch)
			}
			if seenCh[ch] {
				return errf(CodeSchema, path+".target_channels", "target_channels", "duplicate channel %q", ch)
			}
			seenCh[ch] = true
		}
		if len(e.Mappings) != len(e.TargetChannels) {
			return errf(CodeSchema, path+".mappings", "mappings", "must have exactly %d entries", len(e.TargetChannels))
		}
		for j, m := range e.Mappings {
			mp := fmt.Sprintf("%s.mappings[%d]", path, j)
			if m.Target != e.TargetChannels[j] {
				return errf(CodeSchema, mp+".target", "target", "mapping target %q does not match target channel %q", m.Target, e.TargetChannels[j])
			}
			if (m.Fill == nil) == (m.Source == "") {
				return errf(CodeSchema, mp, "mapping", "exactly one of source or fill is required")
			}
			if m.Source != "" && !validChannel(m.Source) {
				return errf(CodeSchema, mp+".source", "source", "unknown channel %q", m.Source)
			}
			if m.Fill != nil {
				if err := validateFill(*m.Fill, mp+".fill", e.TargetBitDepth); err != nil {
					return err
				}
			}
		}
		if !validRounding(e.Rounding) {
			return errf(CodeSchema, path+".rounding", "rounding", "unsupported rounding %q", e.Rounding)
		}
		if !validAlpha(e.Alpha) {
			return errf(CodeSchema, path+".alpha", "alpha", "unsupported alpha mode %q", e.Alpha)
		}
		inputLayout := p.InputLayout(i)
		for _, m := range e.Mappings {
			if m.Source != "" && !containsChannel(inputLayout, m.Source) {
				return errf(CodePara, path+".mappings", "source", "input layout %v has no channel %q", inputLayout, m.Source)
			}
			if m.Fill != nil && m.Fill.Kind == FillFromChannel && !containsChannel(inputLayout, m.Fill.Channel) {
				return errf(CodePara, path+".fill", "fill.channel", "input layout %v has no channel %q", inputLayout, m.Fill.Channel)
			}
		}
		inputDepth := p.InputBitDepth(i)
		formatChanges := inputDepth != e.TargetBitDepth || !channelsEqual(inputLayout, e.TargetChannels)
		if formatChanges && e.Coverage != FullRect(p.Source.Width, p.Source.Height) {
			return errf(CodePara, path+".coverage", "coverage", "format-changing entries must cover the full image")
		}
	}
	p.RecomputeFingerprint()
	return nil
}

func validateSource(s Source) error {
	if s.Width <= 0 {
		return errf(CodePara, "$.source.width", "width", "must be > 0")
	}
	if s.Height <= 0 {
		return errf(CodePara, "$.source.height", "height", "must be > 0")
	}
	if s.BitDepth != 8 && s.BitDepth != 16 {
		return errf(CodePara, "$.source.bit_depth", "bit_depth", "source bit depth must be 8 or 16")
	}
	if len(s.Layout) == 0 || len(s.Layout) > 4 {
		return errf(CodeSchema, "$.source.layout", "layout", "must contain 1..4 channels")
	}
	seen := make(map[Channel]bool)
	for _, ch := range s.Layout {
		if !validChannel(ch) {
			return errf(CodeSchema, "$.source.layout", "layout", "unknown channel %q", ch)
		}
		if seen[ch] {
			return errf(CodeSchema, "$.source.layout", "layout", "duplicate channel %q", ch)
		}
		seen[ch] = true
	}
	if !((len(s.Layout) == 4 && channelsEqual(s.Layout, []Channel{ChR, ChG, ChB, ChA})) ||
		(len(s.Layout) == 4 && channelsEqual(s.Layout, []Channel{ChB, ChG, ChR, ChA}))) {
		return errf(CodeSchema, "$.source.layout", "layout", "source layout must be RGBA or BGRA")
	}
	return nil
}

func validateFill(f FillSource, path string, targetBits int) error {
	if targetBits < 1 || targetBits > 16 {
		return errf(CodePara, path, "target_bit_depth", "invalid target bit depth")
	}
	maxValue := (1 << targetBits) - 1
	switch f.Kind {
	case FillConstant:
		if int(f.Value) > maxValue {
			return errf(CodePara, path+".value", "fill.value", "value %d exceeds %d-bit maximum %d", f.Value, targetBits, maxValue)
		}
	case FillZero, FillOne:
	case FillFromChannel:
		if !validChannel(f.Channel) {
			return errf(CodeSchema, path+".channel", "fill.channel", "unknown source channel %q", f.Channel)
		}
	default:
		return errf(CodeSchema, path+".kind", "fill.kind", "unsupported fill kind %q", f.Kind)
	}
	return nil
}

func validChannel(ch Channel) bool {
	switch ch {
	case ChR, ChG, ChB, ChA:
		return true
	default:
		return false
	}
}

func validRounding(r Rounding) bool {
	return r == RoundNearest || r == RoundFloor || r == RoundCeil
}

func validAlpha(a AlphaMode) bool {
	switch a {
	case AlphaCopy, AlphaDrop, AlphaStraight, AlphaPremultiply, AlphaUnpremultiply:
		return true
	default:
		return false
	}
}

func containsChannel(layout []Channel, ch Channel) bool {
	for _, candidate := range layout {
		if candidate == ch {
			return true
		}
	}
	return false
}

func channelsEqual(a, b []Channel) bool {
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

func LayoutString(layout []Channel) string {
	parts := make([]string, len(layout))
	for i, ch := range layout {
		parts[i] = string(ch)
	}
	return strings.Join(parts, "")
}

func ParseLayout(s string) ([]Channel, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	out := make([]Channel, 0, len(s))
	for _, r := range s {
		ch, ok := ParseChannel(string(r))
		if !ok {
			return nil, fmt.Errorf("unknown channel %q", string(r))
		}
		out = append(out, ch)
	}
	return out, nil
}

// CanonicalizeSorting is a small helper used by reports. It never mutates plan
// order because entry order is part of the conversion contract.
func sortedChannels(in []Channel) []Channel {
	out := append([]Channel(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
