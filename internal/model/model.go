package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const (
	AlgorithmVersion = "texlab-core-1.0.0"
	CheckpointSchema = "texlab-checkpoint-v1"
	TruthSchema      = "texlab-truth-v1"
	RunSchema        = "texlab-run-v1"
	ReportSchema     = "texlab-report-v1"
)

type Rect struct {
	ID string `json:"id"`
	X  int    `json:"x"`
	Y  int    `json:"y"`
	W  int    `json:"w"`
	H  int    `json:"h"`
}

func (r Rect) Area() int {
	if r.W <= 0 || r.H <= 0 {
		return 0
	}
	return r.W * r.H
}

func (r Rect) Intersect(other Rect) (Rect, bool) {
	x0 := max(r.X, other.X)
	y0 := max(r.Y, other.Y)
	x1 := min(r.X+r.W, other.X+other.W)
	y1 := min(r.Y+r.H, other.Y+other.H)
	if x1 <= x0 || y1 <= y0 {
		return Rect{}, false
	}
	return Rect{ID: r.ID + "/" + other.ID, X: x0, Y: y0, W: x1 - x0, H: y1 - y0}, true
}

func (r Rect) Contains(x, y int) bool {
	return x >= r.X && y >= r.Y && x < r.X+r.W && y < r.Y+r.H
}

type FillSpec struct {
	TargetIndex   int    `json:"target_index"`
	Source        string `json:"source"`
	SourceChannel int    `json:"source_channel,omitempty"`
	Channel       string `json:"channel"`
	Value         int    `json:"value,omitempty"`
}

type Entry struct {
	Index          int        `json:"index"`
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	TargetDepth    int        `json:"target_depth"`
	TargetChannels int        `json:"target_channels"`
	Permutation    []int      `json:"permutation"`
	Fill           []FillSpec `json:"fill"`
	Drop           []int      `json:"drop"`
	Alpha          string     `json:"alpha"`
	Quantize       string     `json:"quantize"`
	Regions        []string   `json:"regions"`
}

// ParamSignature excludes index and ID so a pure reorder can reuse unchanged
// segment data while still changing the stage cache key.
func (e Entry) ParamSignature() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d|%d|", e.TargetDepth, e.TargetChannels)
	for _, v := range e.Permutation {
		fmt.Fprintf(&b, "%d,", v)
	}
	b.WriteByte('|')
	for _, f := range e.Fill {
		fmt.Fprintf(&b, "%d:%s:%d:%s:%d,", f.TargetIndex, f.Source, f.SourceChannel, f.Channel, f.Value)
	}
	b.WriteByte('|')
	for _, v := range e.Drop {
		fmt.Fprintf(&b, "%d,", v)
	}
	fmt.Fprintf(&b, "|%s|%s|", e.Alpha, e.Quantize)
	b.WriteString(strings.Join(e.Regions, ","))
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func (e Entry) ParamSnapshot() Entry {
	out := e
	out.Permutation = append([]int(nil), e.Permutation...)
	out.Fill = append([]FillSpec(nil), e.Fill...)
	out.Drop = append([]int(nil), e.Drop...)
	out.Regions = append([]string(nil), e.Regions...)
	return out
}

type SegmentSpec struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type Plan struct {
	Schema   string      `json:"schema"`
	Name     string      `json:"name"`
	Revision int         `json:"revision"`
	Segments SegmentSpec `json:"segments"`
	Regions  []Rect      `json:"regions"`
	Entries  []Entry     `json:"entries"`
}

func (p Plan) Clone() Plan {
	out := p
	out.Regions = append([]Rect(nil), p.Regions...)
	out.Entries = make([]Entry, len(p.Entries))
	for i := range p.Entries {
		out.Entries[i] = p.Entries[i].ParamSnapshot()
	}
	return out
}

type Image struct {
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	Depth    int      `json:"depth"`
	Channels int      `json:"channels"`
	Layout   string   `json:"layout"`
	Alpha    string   `json:"alpha"`
	Pixels   []uint16 `json:"-"`
}

func NewImage(width, height, depth, channels int, layout, alpha string) Image {
	return Image{
		Width: width, Height: height, Depth: depth, Channels: channels,
		Layout: layout, Alpha: alpha, Pixels: make([]uint16, width*height*4),
	}
}

func (im Image) Clone() Image {
	out := im
	out.Pixels = append([]uint16(nil), im.Pixels...)
	return out
}

func (im Image) MaxValue() int {
	if im.Depth == 8 {
		return 255
	}
	return 65535
}

func (im Image) SemanticPixel(x, y int) [4]uint16 {
	i := (y*im.Width + x) * 4
	if i < 0 || i+3 >= len(im.Pixels) {
		return [4]uint16{}
	}
	return [4]uint16{im.Pixels[i], im.Pixels[i+1], im.Pixels[i+2], im.Pixels[i+3]}
}

func (im Image) SetSemanticPixel(x, y int, p [4]uint16) {
	i := (y*im.Width + x) * 4
	copy(im.Pixels[i:i+4], p[:])
}

func (im Image) ContentHash() string {
	return ImageHash(im, Rect{ID: "full", X: 0, Y: 0, W: im.Width, H: im.Height})
}

func ImageHash(im Image, region Rect) string {
	h := sha256.New()
	fmt.Fprintf(h, "texlab-image-v1|%d|%d|%d|%d|%s|%s|", im.Width, im.Height, im.Depth, im.Channels, im.Layout, im.Alpha)
	var tmp [8]byte
	binary.LittleEndian.PutUint32(tmp[0:4], uint32(region.X))
	binary.LittleEndian.PutUint32(tmp[4:8], uint32(region.Y))
	h.Write(tmp[:])
	for y := region.Y; y < region.Y+region.H; y++ {
		start := (y*im.Width + region.X) * 4
		end := (y*im.Width + region.X + region.W) * 4
		if start < 0 || end > len(im.Pixels) || start > end {
			continue
		}
		for _, v := range im.Pixels[start:end] {
			binary.LittleEndian.PutUint16(tmp[:2], v)
			h.Write(tmp[:2])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func HashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		var n [4]byte
		binary.LittleEndian.PutUint32(n[:], uint32(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func CacheKey(inputRegionHash string, entryIndex int, paramHash, upstreamKey string) string {
	return HashParts("texlab-cache-key-v1", inputRegionHash, fmt.Sprintf("%d", entryIndex), paramHash, upstreamKey)
}

func PlanFingerprint(p Plan) string {
	clone := p.Clone()
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%d|%d|%d|", clone.Schema, clone.Name, clone.Revision, clone.Segments.Cols, clone.Segments.Rows)
	for _, r := range clone.Regions {
		fmt.Fprintf(&b, "%s:%d:%d:%d:%d|", r.ID, r.X, r.Y, r.W, r.H)
	}
	for _, e := range clone.Entries {
		fmt.Fprintf(&b, "%d:%s:%s:%d:%d:", e.Index, e.ID, e.Name, e.TargetDepth, e.TargetChannels)
		for _, v := range e.Permutation {
			fmt.Fprintf(&b, "%d,", v)
		}
		b.WriteByte(';')
		for _, f := range e.Fill {
			fmt.Fprintf(&b, "%d:%s:%d:%s:%d,", f.TargetIndex, f.Source, f.SourceChannel, f.Channel, f.Value)
		}
		b.WriteByte(';')
		for _, v := range e.Drop {
			fmt.Fprintf(&b, "%d,", v)
		}
		fmt.Fprintf(&b, ";%s;%s;%s|", e.Alpha, e.Quantize, strings.Join(e.Regions, ","))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

type Segment struct {
	Index  int `json:"index"`
	Row    int `json:"row"`
	Col    int `json:"col"`
	X      int `json:"x"`
	Y      int `json:"y"`
	W      int `json:"w"`
	H      int `json:"h"`
	Pixels int `json:"pixels"`
}

func (s Segment) Rect() Rect {
	return Rect{ID: fmt.Sprintf("seg-%02d", s.Index), X: s.X, Y: s.Y, W: s.W, H: s.H}
}

func BuildSegments(width, height int, spec SegmentSpec) []Segment {
	if spec.Cols <= 0 {
		spec.Cols = 4
	}
	if spec.Rows <= 0 {
		spec.Rows = 3
	}
	out := make([]Segment, 0, spec.Cols*spec.Rows)
	for row := 0; row < spec.Rows; row++ {
		y0 := row * height / spec.Rows
		y1 := (row + 1) * height / spec.Rows
		for col := 0; col < spec.Cols; col++ {
			x0 := col * width / spec.Cols
			x1 := (col + 1) * width / spec.Cols
			out = append(out, Segment{
				Index: len(out), Row: row, Col: col,
				X: x0, Y: y0, W: x1 - x0, H: y1 - y0, Pixels: (x1 - x0) * (y1 - y0),
			})
		}
	}
	return out
}

func (p Plan) RegionByID(id string) (Rect, bool) {
	for _, r := range p.Regions {
		if r.ID == id {
			return r, true
		}
	}
	return Rect{}, false
}

func (p Plan) EntryRegions(e Entry) []Rect {
	out := make([]Rect, 0, len(e.Regions))
	for _, id := range e.Regions {
		if r, ok := p.RegionByID(id); ok {
			out = append(out, r)
		}
	}
	return out
}

func (p Plan) Coverage(e Entry, seg Segment) []Rect {
	var out []Rect
	for _, r := range p.EntryRegions(e) {
		if inter, ok := r.Intersect(seg.Rect()); ok {
			out = append(out, inter)
		}
	}
	// Adjacent region declarations can overlap. Canonicalise the list so a
	// plan reorder does not change a coverage hash.
	sort.Slice(out, func(i, j int) bool {
		if out[i].X != out[j].X {
			return out[i].X < out[j].X
		}
		if out[i].Y != out[j].Y {
			return out[i].Y < out[j].Y
		}
		if out[i].W != out[j].W {
			return out[i].W < out[j].W
		}
		return out[i].H < out[j].H
	})
	merged := out[:0]
	for _, r := range out {
		if n := len(merged); n > 0 && merged[n-1].X == r.X && merged[n-1].Y == r.Y &&
			merged[n-1].W == r.W && merged[n-1].H == r.H {
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

func CoverageHash(im Image, coverage []Rect) string {
	if len(coverage) == 0 {
		return HashParts("texlab-empty-coverage-v1")
	}
	parts := make([]string, 0, len(coverage)+1)
	for _, r := range coverage {
		parts = append(parts, ImageHash(im, r))
	}
	return HashParts(parts...)
}

func CoverageSignature(coverage []Rect) string {
	if len(coverage) == 0 {
		return "empty"
	}
	var b strings.Builder
	for _, r := range coverage {
		fmt.Fprintf(&b, "%d:%d:%d:%d,", r.X, r.Y, r.W, r.H)
	}
	return b.String()
}

type Diff struct {
	X          int      `json:"x"`
	Y          int      `json:"y"`
	EntryIndex int      `json:"entry_index"`
	EntryID    string   `json:"entry_id"`
	Segment    int      `json:"segment"`
	Field      string   `json:"field"`
	Expected   []uint16 `json:"expected"`
	Actual     []uint16 `json:"actual"`
}

func FirstImageDiff(expected, actual Image, entry Entry, segment int) *Diff {
	if expected.Width != actual.Width || expected.Height != actual.Height {
		return &Diff{EntryIndex: entry.Index, EntryID: entry.ID, Segment: segment, Field: "dimensions"}
	}
	for y := 0; y < expected.Height; y++ {
		for x := 0; x < expected.Width; x++ {
			e := expected.SemanticPixel(x, y)
			a := actual.SemanticPixel(x, y)
			if e != a {
				return &Diff{
					X: x, Y: y, EntryIndex: entry.Index, EntryID: entry.ID, Segment: segment,
					Field: "pixels", Expected: e[:], Actual: a[:],
				}
			}
		}
	}
	return nil
}
