package textureconv

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// Quantize converts one native component with the declared reduction
// direction. Equal input/output depths are an exact identity operation.
func Quantize(v uint16, from, to BitDepth, q Quantization) uint16 {
	if !from.Valid() || !to.Valid() || v > from.Max() {
		return 0
	}
	if from == to {
		return v
	}
	if from == Depth8 && to == Depth16 {
		return uint16(uint32(v) * 65535 / 255)
	}
	srcMax := uint64(from.Max())
	n := uint64(v) * uint64(to.Max())
	switch q {
	case QuantCeil:
		return uint16((n + srcMax - 1) / srcMax)
	case QuantFloor:
		return uint16(n / srcMax)
	default:
		return uint16((n + srcMax/2) / srcMax)
	}
}

// ConvertEntry applies one entry to a complete stage image. Region entries
// leave pixels outside Region exactly unchanged.
func ConvertEntry(in *StageImage, e Entry) (*StageImage, error) {
	if in == nil {
		return nil, schemaErr(e.Seq, "input", "nil stage image")
	}
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if err := validateEntryOnStage(e, in.Channels, in.BitDepth, in.AlphaIndex, in.Width, in.Height); err != nil {
		return nil, err
	}
	dropped := append([]DroppedChannel(nil), in.Dropped...)
	for _, c := range e.Drop {
		dropped = append(dropped, DroppedChannel{Entry: e.Seq, Channel: c})
	}
	out := &StageImage{
		Width: in.Width, Height: in.Height, BitDepth: e.TargetDepth,
		Channels: e.TargetChannels, Pixels: make([]uint16, in.Width*in.Height*e.TargetChannels),
		AlphaIndex: targetAlphaIndex(e, in.AlphaIndex), Dropped: dropped,
	}
	region := e.Region
	if region == nil {
		r := Rect{W: in.Width, H: in.Height}
		region = &r
	}
	fill := fillMap(e)
	for y := 0; y < in.Height; y++ {
		for x := 0; x < in.Width; x++ {
			srcOff := (y*in.Width + x) * in.Channels
			dstOff := (y*out.Width + x) * out.Channels
			if !region.Contains(x, y) {
				copyBaseChannels(out.Pixels[dstOff:dstOff+out.Channels], in.Pixels[srcOff:srcOff+in.Channels], e.TargetDepth)
				continue
			}
			convertPixel(in, e, fill, in.Pixels[srcOff:srcOff+in.Channels], out.Pixels[dstOff:dstOff+out.Channels])
		}
	}
	return out, nil
}

// ConvertEntryTile is the tile-oriented conversion entry point. Region tests
// still use absolute image coordinates from tile coordinates.
func ConvertEntryTile(in *StageImage, t Tile, e Entry) (*StageImage, error) {
	if in == nil {
		return nil, schemaErr(e.Seq, "input", "nil stage image")
	}
	if t.W <= 0 || t.H <= 0 || t.X < 0 || t.Y < 0 || t.X+t.W > in.Width || t.Y+t.H > in.Height {
		return nil, paramErr(e.Seq, "tile", "tile %d is outside stage", t.Index)
	}
	if err := validateEntryOnStage(e, in.Channels, in.BitDepth, in.AlphaIndex, in.Width, in.Height); err != nil {
		return nil, err
	}
	dropped := append([]DroppedChannel(nil), in.Dropped...)
	for _, c := range e.Drop {
		dropped = append(dropped, DroppedChannel{Entry: e.Seq, Channel: c})
	}
	out := &StageImage{
		Width: t.W, Height: t.H, BitDepth: e.TargetDepth,
		Channels: e.TargetChannels, Pixels: make([]uint16, t.W*t.H*e.TargetChannels),
		AlphaIndex: targetAlphaIndex(e, in.AlphaIndex), Dropped: dropped,
	}
	fill := fillMap(e)
	for y := t.Y; y < t.Y+t.H; y++ {
		for x := t.X; x < t.X+t.W; x++ {
			srcOff := (y*in.Width + x) * in.Channels
			dstOff := ((y-t.Y)*t.W + (x - t.X)) * out.Channels
			if e.Region != nil && !e.Region.Contains(x, y) {
				copyBaseChannels(out.Pixels[dstOff:dstOff+out.Channels], in.Pixels[srcOff:srcOff+in.Channels], e.TargetDepth)
				continue
			}
			convertPixel(in, e, fill, in.Pixels[srcOff:srcOff+in.Channels], out.Pixels[dstOff:dstOff+out.Channels])
		}
	}
	return out, nil
}

// convertEntryTileLocal applies an entry to a tile-sized input while region
// checks use the original full-image geometry.
func convertEntryTileLocal(in *StageImage, fullW, fullH int, t Tile, e Entry) (*StageImage, error) {
	if in == nil {
		return nil, schemaErr(e.Seq, "input", "nil stage image")
	}
	if in.Width != t.W || in.Height != t.H || in.Channels < 1 || in.Channels > 4 {
		return nil, schemaErr(e.Seq, "input", "tile input shape %dx%dx%d does not match tile %dx%d",
			in.Width, in.Height, in.Channels, t.W, t.H)
	}
	if err := validateEntryOnStage(e, in.Channels, in.BitDepth, in.AlphaIndex, fullW, fullH); err != nil {
		return nil, err
	}
	dropped := append([]DroppedChannel(nil), in.Dropped...)
	for _, c := range e.Drop {
		dropped = append(dropped, DroppedChannel{Entry: e.Seq, Channel: c})
	}
	out := &StageImage{
		Width: t.W, Height: t.H, BitDepth: e.TargetDepth, Channels: e.TargetChannels,
		Pixels:     make([]uint16, t.W*t.H*e.TargetChannels),
		AlphaIndex: targetAlphaIndex(e, in.AlphaIndex), Dropped: dropped,
	}
	fill := fillMap(e)
	for ly := 0; ly < t.H; ly++ {
		y := t.Y + ly
		for lx := 0; lx < t.W; lx++ {
			x := t.X + lx
			srcOff := (ly*t.W + lx) * in.Channels
			dstOff := (ly*t.W + lx) * out.Channels
			if e.Region != nil && !e.Region.Contains(x, y) {
				copyBaseChannels(out.Pixels[dstOff:dstOff+out.Channels], in.Pixels[srcOff:srcOff+in.Channels], e.TargetDepth)
				continue
			}
			convertPixel(in, e, fill, in.Pixels[srcOff:srcOff+in.Channels], out.Pixels[dstOff:dstOff+out.Channels])
		}
	}
	return out, nil
}

func convertPixel(in *StageImage, e Entry, fill map[int]uint16, src, dst []uint16) {
	var work [4]uint16
	copy(work[:], src)
	maxV := uint64(in.BitDepth.Max())
	if in.AlphaIndex >= 0 && in.AlphaIndex < in.Channels {
		a := uint64(work[in.AlphaIndex])
		switch e.Alpha {
		case AlphaOpaque:
			work[in.AlphaIndex] = uint16(maxV)
		case AlphaPremultiply:
			for c := 0; c < in.Channels; c++ {
				if c != in.AlphaIndex {
					work[c] = uint16((uint64(work[c])*a + maxV/2) / maxV)
				}
			}
		case AlphaStraighten:
			for c := 0; c < in.Channels; c++ {
				if c == in.AlphaIndex {
					continue
				}
				if a == 0 {
					work[c] = 0
					continue
				}
				v := (uint64(work[c])*maxV + a/2) / a
				if v > maxV {
					v = maxV
				}
				work[c] = uint16(v)
			}
		}
	}
	for j, srcIdx := range e.Permutation {
		if srcIdx == -1 {
			dst[j] = fill[j]
		} else {
			dst[j] = Quantize(work[srcIdx], in.BitDepth, e.TargetDepth, e.Quantization)
		}
	}
}

func copyBaseChannels(dst, src []uint16, target BitDepth) {
	n := min(len(dst), len(src))
	for i := 0; i < n; i++ {
		// Region entries preserve depth by validation; this branch is safe
		// for a future structural-region extension as well.
		dst[i] = src[i]
	}
	for i := n; i < len(dst); i++ {
		dst[i] = 0
	}
	_ = target
}

func fillMap(e Entry) map[int]uint16 {
	m := make(map[int]uint16, len(e.Fill))
	for _, f := range e.Fill {
		m[f.Channel] = f.Value
	}
	return m
}

func targetAlphaIndex(e Entry, srcAlpha int) int {
	if srcAlpha < 0 {
		return -1
	}
	for dst, src := range e.Permutation {
		if src == srcAlpha {
			return dst
		}
	}
	return -1
}

func validateEntryOnStage(e Entry, srcChannels int, srcDepth BitDepth, srcAlpha, width, height int) error {
	if e.Seq <= 0 {
		return schemaErr(e.Seq, "seq", "entry sequence must be positive")
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
		if idx < -1 || idx >= srcChannels {
			return paramErr(e.Seq, "permutation", "source index %d is outside current channel count %d", idx, srcChannels)
		}
		if idx >= 0 {
			if seen[idx] {
				return paramErr(e.Seq, "permutation", "source index %d is repeated", idx)
			}
			seen[idx] = true
		}
	}
	for dst, idx := range e.Permutation {
		if idx == -1 {
			fillSlots[dst] = true
		}
	}
	if len(e.Fill) != len(fillSlots) {
		return paramErr(e.Seq, "fill", "got %d fill specs for %d missing slots", len(e.Fill), len(fillSlots))
	}
	fillSeen := map[int]bool{}
	for _, f := range e.Fill {
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
	var wantDrop []int
	for c := 0; c < srcChannels; c++ {
		if !seen[c] {
			wantDrop = append(wantDrop, c)
		}
	}
	gotDrop := append([]int(nil), e.Drop...)
	sort.Ints(gotDrop)
	if !equalInts(gotDrop, wantDrop) {
		return paramErr(e.Seq, "drop", "got %v, want complement %v", gotDrop, wantDrop)
	}
	if e.Region != nil {
		if e.Region.W <= 0 || e.Region.H <= 0 || e.Region.X < 0 || e.Region.Y < 0 ||
			e.Region.X+e.Region.W > width || e.Region.Y+e.Region.H > height {
			return paramErr(e.Seq, "region", "region %+v is outside %dx%d image", *e.Region, width, height)
		}
		if e.TargetDepth != srcDepth {
			return paramErr(e.Seq, "region", "region entries cannot change bit depth (%d -> %d)", srcDepth, e.TargetDepth)
		}
		if e.TargetChannels != srcChannels {
			return paramErr(e.Seq, "region", "region entries cannot change channel count (%d -> %d)", srcChannels, e.TargetChannels)
		}
		if srcAlpha >= 0 && (srcAlpha >= len(e.Permutation) || e.Permutation[srcAlpha] != srcAlpha) {
			return paramErr(e.Seq, "permutation", "region entry must retain source alpha at slot %d", srcAlpha)
		}
	}
	return nil
}

// EffectiveEntryFingerprint includes the exact region mask intersection for a
// tile. Tiles wholly outside a region use a stable pass-through token.
func EffectiveEntryFingerprint(e Entry, t Tile) string {
	if e.Region != nil && !e.Region.Intersects(Rect{X: t.X, Y: t.Y, W: t.W, H: t.H}) {
		return "pass-through"
	}
	type effective struct {
		Entry  Entry `json:"entry"`
		Region *Rect `json:"regionMask,omitempty"`
		TileW  int   `json:"tileW"`
		TileH  int   `json:"tileH"`
	}
	var mask *Rect
	if e.Region != nil {
		inter := e.Region.Intersection(Rect{X: t.X, Y: t.Y, W: t.W, H: t.H})
		rel := Rect{X: inter.X - t.X, Y: inter.Y - t.Y, W: inter.W, H: inter.H}
		mask = &rel
	}
	data := fmt.Sprintf("%s|%s|%d|%d", EntryFingerprint(e), rectString(mask), t.W, t.H)
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func rectString(r *Rect) string {
	if r == nil {
		return "full"
	}
	return fmt.Sprintf("%d,%d,%d,%d", r.X, r.Y, r.W, r.H)
}
