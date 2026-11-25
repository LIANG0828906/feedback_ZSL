package textureconv

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"sort"
)

type planWire struct {
	SchemaVersion *int         `json:"schemaVersion"`
	Entries       *[]entryWire `json:"entries"`
}

type entryWire struct {
	Seq            *int        `json:"seq"`
	TargetDepth    *int        `json:"targetDepth"`
	TargetChannels *int        `json:"targetChannels"`
	Permutation    *[]int      `json:"permutation"`
	Fill           *[]fillWire `json:"fill"`
	Drop           *[]int      `json:"drop"`
	Quantization   *string     `json:"quantization"`
	Alpha          *string     `json:"alpha"`
	Region         *rectWire   `json:"region"`
}

type fillWire struct {
	Channel *int   `json:"channel"`
	Value   *int64 `json:"value"`
}

type rectWire struct {
	X *int `json:"x"`
	Y *int `json:"y"`
	W *int `json:"w"`
	H *int `json:"h"`
}

// ParsePlan strictly decodes a plan. Unknown fields and missing required
// fields produce E_SCHEMA.
func ParsePlan(data []byte) (*Plan, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var w planWire
	if err := dec.Decode(&w); err != nil {
		return nil, schemaErr(0, "plan", "invalid JSON: %v", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, schemaErr(0, "plan", "trailing JSON value")
		}
		return nil, schemaErr(0, "plan", "invalid trailing JSON: %v", err)
	}
	if w.SchemaVersion == nil {
		return nil, schemaErr(0, "schemaVersion", "required field is missing")
	}
	if w.Entries == nil {
		return nil, schemaErr(0, "entries", "required field is missing")
	}
	p := &Plan{SchemaVersion: *w.SchemaVersion, Entries: make([]Entry, 0, len(*w.Entries))}
	for i, ew := range *w.Entries {
		entry, err := decodeEntry(ew, i)
		if err != nil {
			return nil, err
		}
		p.Entries = append(p.Entries, entry)
	}
	if err := p.ValidateShape(); err != nil {
		return nil, err
	}
	return p, nil
}

func decodeEntry(w entryWire, idx int) (Entry, error) {
	var e Entry
	field := func(name string, ptr any, missing bool) error {
		if missing {
			return schemaErr(e.Seq, name, "required field is missing at entries[%d]", idx)
		}
		return nil
	}
	if err := field("seq", w.Seq, w.Seq == nil); err != nil {
		return e, err
	}
	e.Seq = *w.Seq
	if err := field("targetDepth", w.TargetDepth, w.TargetDepth == nil); err != nil {
		return e, err
	}
	e.TargetDepth = BitDepth(*w.TargetDepth)
	if err := field("targetChannels", w.TargetChannels, w.TargetChannels == nil); err != nil {
		return e, err
	}
	e.TargetChannels = *w.TargetChannels
	if err := field("permutation", w.Permutation, w.Permutation == nil); err != nil {
		return e, err
	}
	e.Permutation = append([]int(nil), (*w.Permutation)...)
	if err := field("fill", w.Fill, w.Fill == nil); err != nil {
		return e, err
	}
	for j, fw := range *w.Fill {
		if fw.Channel == nil {
			return e, schemaErr(e.Seq, "fill.channel", "required field is missing at fill[%d]", j)
		}
		if fw.Value == nil {
			return e, schemaErr(e.Seq, "fill.value", "required field is missing at fill[%d]", j)
		}
		if *fw.Value < 0 || *fw.Value > 65535 {
			return e, paramErr(e.Seq, "fill.value", "value %d is outside [0,65535]", *fw.Value)
		}
		e.Fill = append(e.Fill, FillSpec{Channel: *fw.Channel, Value: uint16(*fw.Value)})
	}
	if err := field("drop", w.Drop, w.Drop == nil); err != nil {
		return e, err
	}
	e.Drop = append([]int(nil), (*w.Drop)...)
	if err := field("quantization", w.Quantization, w.Quantization == nil); err != nil {
		return e, err
	}
	e.Quantization = Quantization(*w.Quantization)
	if err := field("alpha", w.Alpha, w.Alpha == nil); err != nil {
		return e, err
	}
	e.Alpha = AlphaMode(*w.Alpha)
	if w.Region != nil {
		rw := w.Region
		if rw.X == nil {
			return e, schemaErr(e.Seq, "region.x", "required field is missing")
		}
		if rw.Y == nil {
			return e, schemaErr(e.Seq, "region.y", "required field is missing")
		}
		if rw.W == nil {
			return e, schemaErr(e.Seq, "region.w", "required field is missing")
		}
		if rw.H == nil {
			return e, schemaErr(e.Seq, "region.h", "required field is missing")
		}
		e.Region = &Rect{X: *rw.X, Y: *rw.Y, W: *rw.W, H: *rw.H}
	}
	return e, nil
}

// LoadPlan reads and strictly decodes a plan file.
func LoadPlan(path string) (*Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, ioErr("readPlan", err)
	}
	return ParsePlan(data)
}

// ValidateShape checks all entry-local properties that do not depend on the
// source image. Sequence numbers must be contiguous and start at one.
func (p *Plan) ValidateShape() error {
	if p.SchemaVersion != SchemaVersion {
		return schemaErr(0, "schemaVersion", "got %d, want %d", p.SchemaVersion, SchemaVersion)
	}
	if len(p.Entries) == 0 {
		return schemaErr(0, "entries", "plan must contain at least one entry")
	}
	for i := range p.Entries {
		e := p.Entries[i]
		if e.Seq != i+1 {
			return schemaErr(e.Seq, "seq", "entries[%d] has seq %d, want %d", i, e.Seq, i+1)
		}
		if !e.TargetDepth.Valid() {
			return paramErr(e.Seq, "targetDepth", "target depth must be 8 or 16, got %d", e.TargetDepth)
		}
		if e.TargetChannels < 1 || e.TargetChannels > 4 {
			return paramErr(e.Seq, "targetChannels", "must be in [1,4], got %d", e.TargetChannels)
		}
		if len(e.Permutation) != e.TargetChannels {
			return paramErr(e.Seq, "permutation", "length %d does not match targetChannels %d", len(e.Permutation), e.TargetChannels)
		}
		if !e.Quantization.Valid() {
			return paramErr(e.Seq, "quantization", "unsupported quantization %q", e.Quantization)
		}
		if !e.Alpha.Valid() {
			return paramErr(e.Seq, "alpha", "unsupported alpha mode %q", e.Alpha)
		}
		seen := map[int]bool{}
		fillSlots := map[int]bool{}
		for _, idx := range e.Permutation {
			if idx < -1 || idx > 3 {
				return paramErr(e.Seq, "permutation", "source index %d is outside [-1,3]", idx)
			}
			if idx >= 0 {
				if seen[idx] {
					return paramErr(e.Seq, "permutation", "source index %d is repeated", idx)
				}
				seen[idx] = true
			}
		}
		for j, idx := range e.Permutation {
			if idx == -1 {
				fillSlots[j] = true
			}
		}
		if len(e.Fill) != len(fillSlots) {
			return paramErr(e.Seq, "fill", "got %d fill specs for %d missing slots", len(e.Fill), len(fillSlots))
		}
		fillSeen := map[int]bool{}
		for _, f := range e.Fill {
			if f.Channel < 0 || f.Channel >= e.TargetChannels {
				return paramErr(e.Seq, "fill.channel", "destination %d is outside [0,%d)", f.Channel, e.TargetChannels)
			}
			if !fillSlots[f.Channel] {
				return paramErr(e.Seq, "fill.channel", "destination %d is not marked by permutation -1", f.Channel)
			}
			if fillSeen[f.Channel] {
				return paramErr(e.Seq, "fill.channel", "destination %d has multiple fills", f.Channel)
			}
			fillSeen[f.Channel] = true
			if f.Value > e.TargetDepth.Max() {
				return paramErr(e.Seq, "fill.value", "value %d exceeds %d-bit maximum %d", f.Value, e.TargetDepth, e.TargetDepth.Max())
			}
		}
		dropSet := map[int]bool{}
		for _, d := range e.Drop {
			if d < 0 || d > 3 {
				return paramErr(e.Seq, "drop", "channel %d is outside [0,3]", d)
			}
			if dropSet[d] {
				return paramErr(e.Seq, "drop", "channel %d is repeated", d)
			}
			if seen[d] {
				return paramErr(e.Seq, "drop", "channel %d is both retained and dropped", d)
			}
			dropSet[d] = true
		}
		if e.Region != nil {
			if e.Region.W <= 0 || e.Region.H <= 0 {
				return paramErr(e.Seq, "region", "width and height must be positive")
			}
			if e.Region.X < 0 || e.Region.Y < 0 {
				return paramErr(e.Seq, "region", "origin must be non-negative")
			}
		}
	}
	return nil
}

// ValidateForImage validates channel-count-dependent rules by propagating each
// stage's output channel count and alpha location.
func (p *Plan) ValidateForImage(im *Image) error {
	if err := im.Validate(); err != nil {
		return err
	}
	if err := p.ValidateShape(); err != nil {
		return err
	}
	srcChannels := 4
	srcAlpha := im.AlphaIndex()
	srcDepth := im.BitDepth
	for i := range p.Entries {
		e := &p.Entries[i]
		real := make([]bool, srcChannels)
		for _, idx := range e.Permutation {
			if idx >= srcChannels {
				return paramErr(e.Seq, "permutation", "source index %d is outside current channel count %d", idx, srcChannels)
			}
			if idx >= 0 {
				real[idx] = true
			}
		}
		var wantDrop []int
		for c := 0; c < srcChannels; c++ {
			if !real[c] {
				wantDrop = append(wantDrop, c)
			}
		}
		gotDrop := append([]int(nil), e.Drop...)
		sort.Ints(gotDrop)
		if !equalInts(gotDrop, wantDrop) {
			return paramErr(e.Seq, "drop", "got %v, want complement %v", gotDrop, wantDrop)
		}
		if e.TargetChannels > srcChannels {
			if len(e.Fill) != e.TargetChannels-srcChannels {
				return paramErr(e.Seq, "fill", "expansion needs %d fills, got %d", e.TargetChannels-srcChannels, len(e.Fill))
			}
		}
		if e.Region != nil {
			full := Rect{W: im.Width, H: im.Height}
			if e.Region.Intersection(full) != *e.Region {
				return paramErr(e.Seq, "region", "region %+v is outside %dx%d image", *e.Region, im.Width, im.Height)
			}
			if e.TargetDepth != srcDepth {
				return paramErr(e.Seq, "region", "region entries cannot change bit depth (%d -> %d)", srcDepth, e.TargetDepth)
			}
			if e.TargetChannels != srcChannels {
				return paramErr(e.Seq, "region", "region entries cannot change channel count (%d -> %d)", srcChannels, e.TargetChannels)
			}
			if srcAlpha >= 0 {
				if srcAlpha >= len(e.Permutation) || e.Permutation[srcAlpha] != srcAlpha {
					return paramErr(e.Seq, "permutation", "region entry must retain source alpha at slot %d", srcAlpha)
				}
			}
		}
		targetAlpha := -1
		if srcAlpha >= 0 {
			for dst, src := range e.Permutation {
				if src == srcAlpha {
					targetAlpha = dst
					break
				}
			}
		}
		srcChannels = e.TargetChannels
		srcAlpha = targetAlpha
		srcDepth = e.TargetDepth
	}
	return nil
}

func equalInts(a, b []int) bool {
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

// Fingerprint hashes the normalized plan structure, not incidental JSON
// whitespace or fill/drop ordering.
func (p *Plan) Fingerprint() string {
	type canonicalPlan struct {
		SchemaVersion int     `json:"schemaVersion"`
		Entries       []Entry `json:"entries"`
	}
	cp := canonicalPlan{SchemaVersion: p.SchemaVersion, Entries: make([]Entry, len(p.Entries))}
	for i, e := range p.Entries {
		cp.Entries[i] = normalizedEntry(e)
	}
	data, _ := json.Marshal(cp)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// EntryFingerprint hashes one normalized entry.
func EntryFingerprint(e Entry) string {
	data, _ := json.Marshal(normalizedEntry(e))
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func WritePlanFile(path string, p *Plan) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return ioErr("marshalPlan", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return ioErr("writePlan", err)
	}
	return nil
}

func validateSourceRecord(r SourceRecord, im *Image, hash string) error {
	if r.ID == "" {
		return schemaErr(0, "images.id", "required field is missing")
	}
	if r.Path == "" {
		return schemaErr(0, "images.path", "required field is missing")
	}
	if r.Width <= 0 {
		return schemaErr(0, "images.width", "required field is missing or invalid")
	}
	if r.Height <= 0 {
		return schemaErr(0, "images.height", "required field is missing or invalid")
	}
	if r.BitDepth == 0 {
		return schemaErr(0, "images.bitDepth", "required field is missing")
	}
	if r.Layout == "" {
		return schemaErr(0, "images.layout", "required field is missing")
	}
	if r.SHA256 == "" {
		return schemaErr(0, "images.sha256", "required field is missing")
	}
	if r.Width != im.Width || r.Height != im.Height {
		return paramErr(0, "geometry", "manifest says %dx%d, image is %dx%d", r.Width, r.Height, im.Width, im.Height)
	}
	if r.BitDepth != im.BitDepth {
		return paramErr(0, "bitDepth", "manifest says %d, image is %d", r.BitDepth, im.BitDepth)
	}
	if r.Layout != im.Layout {
		return paramErr(0, "layout", "manifest says %s, image is %s", r.Layout, im.Layout)
	}
	if r.SHA256 != "" && r.SHA256 != hash {
		return paramErr(0, "sha256", "manifest hash does not match image file")
	}
	return nil
}
