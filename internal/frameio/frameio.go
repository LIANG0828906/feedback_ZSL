package frameio

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gameintegrity/internal/apperr"
	"gameintegrity/internal/model"
)

const (
	HeaderSize      = 20
	FrameMagics     = "RGBX"
	FrameVersion    = 1
	FlagBadFrame    = 1 << 0
	WatermarkBits   = 16
	WatermarkCell   = 4
	WatermarkBorder = 2
	WatermarkWidth  = WatermarkBits*WatermarkCell + 2*WatermarkBorder
	WatermarkHeight = WatermarkCell + 2*WatermarkBorder
)

var (
	watermarkColor0 = [3]byte{24, 24, 24}
	watermarkColor1 = [3]byte{231, 231, 231}
	watermarkBorder = [3]byte{255, 0, 255}
)

type FrameHeader struct {
	Version  uint16
	Width    uint16
	Height   uint16
	Channels uint8
	Flags    uint8
	Frame    uint32
	Payload  uint32
}

type Frame struct {
	Width    int
	Height   int
	Channels int
	Index    int
	Pixels   []byte
}

type Dataset struct {
	Root          string
	Dir           string
	Manifest      model.Manifest
	Truth         model.Truth
	ManifestBytes []byte
	TruthBytes    []byte
	ContentHash   string
	watermarks    map[int]model.Watermark
	shapes        map[int][]model.ShapeBox
}

func EncodeFrame(width, height, channels, index, flags int, pixels []byte) ([]byte, error) {
	expected := width * height * channels
	if expected <= 0 || len(pixels) != expected {
		return nil, fmt.Errorf("invalid pixel length: got %d want %d", len(pixels), expected)
	}
	if width > 65535 || height > 65535 || channels > 255 {
		return nil, fmt.Errorf("frame dimensions exceed RGBX limits")
	}
	out := make([]byte, HeaderSize+len(pixels))
	copy(out[0:4], FrameMagics)
	binary.BigEndian.PutUint16(out[4:6], FrameVersion)
	binary.BigEndian.PutUint16(out[6:8], uint16(width))
	binary.BigEndian.PutUint16(out[8:10], uint16(height))
	out[10] = byte(channels)
	out[11] = byte(flags)
	binary.BigEndian.PutUint32(out[12:16], uint32(index))
	binary.BigEndian.PutUint32(out[16:20], uint32(len(pixels)))
	copy(out[HeaderSize:], pixels)
	return out, nil
}

func DecodeHeader(data []byte) (FrameHeader, error) {
	var h FrameHeader
	if len(data) < HeaderSize {
		e := apperr.New(apperr.Decode, "frame data is truncated before the RGBX header")
		e.Expected = HeaderSize
		e.Actual = len(data)
		return h, e
	}
	if string(data[0:4]) != FrameMagics {
		return h, apperr.New(apperr.Decode, "frame data has an invalid RGBX magic")
	}
	h.Version = binary.BigEndian.Uint16(data[4:6])
	h.Width = binary.BigEndian.Uint16(data[6:8])
	h.Height = binary.BigEndian.Uint16(data[8:10])
	h.Channels = data[10]
	h.Flags = data[11]
	h.Frame = binary.BigEndian.Uint32(data[12:16])
	h.Payload = binary.BigEndian.Uint32(data[16:20])
	if h.Version != FrameVersion {
		e := apperr.New(apperr.Decode, "unsupported RGBX frame version")
		e.Field = "format_version"
		e.Expected = FrameVersion
		e.Actual = h.Version
		return h, e
	}
	return h, nil
}

func FramePath(dir, pattern string, index int) string {
	if pattern == "" {
		pattern = "frames/frame-%06d.rgbx"
	}
	return filepath.Join(dir, fmt.Sprintf(pattern, index))
}

func BaselinePath(dir, pattern string, index int) string {
	if pattern == "" {
		pattern = "baseline/frame-%06d.rgb"
	}
	return filepath.Join(dir, fmt.Sprintf(pattern, index))
}

func Open(root, sampleID string) (*Dataset, error) {
	dir := filepath.Join(root, sampleID)
	manifestBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		e := apperr.Wrap(apperr.Decode, "frame sequence manifest is missing or unreadable", err)
		e.Field = "manifest.json"
		return nil, e
	}
	var manifest model.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		e := apperr.Wrap(apperr.Decode, "frame sequence manifest is not valid JSON", err)
		e.Field = "manifest.json"
		return nil, e
	}
	if manifest.SampleID == "" {
		manifest.SampleID = sampleID
	}
	truthBytes, err := os.ReadFile(filepath.Join(dir, "truth.json"))
	if err != nil {
		e := apperr.Wrap(apperr.Decode, "truth manifest is missing or unreadable", err)
		e.Field = "truth.json"
		return nil, e
	}
	var truth model.Truth
	if err := json.Unmarshal(truthBytes, &truth); err != nil {
		e := apperr.Wrap(apperr.Decode, "truth manifest is not valid JSON", err)
		e.Field = "truth.json"
		return nil, e
	}

	ds := &Dataset{
		Root: root, Dir: dir,
		Manifest: manifest, Truth: truth,
		ManifestBytes: manifestBytes, TruthBytes: truthBytes,
		watermarks: make(map[int]model.Watermark, len(truth.Watermarks)),
		shapes:     make(map[int][]model.ShapeBox, len(truth.Shapes)),
	}
	if err := ds.validateStructure(); err != nil {
		return nil, err
	}
	if err := ds.validateFrameFiles(); err != nil {
		return nil, err
	}
	if err := ds.validateWatermarks(); err != nil {
		return nil, err
	}
	if err := ds.validateBaselines(); err != nil {
		return nil, err
	}
	hash, err := ds.computeContentHash()
	if err != nil {
		return nil, err
	}
	ds.ContentHash = hash
	return ds, nil
}

func (d *Dataset) validateStructure() error {
	m, t := &d.Manifest, &d.Truth
	checks := []struct {
		field    string
		m, truth int
	}{
		{"width", m.Width, t.Width},
		{"height", m.Height, t.Height},
		{"channels", m.Channels, t.Channels},
		{"frame_count", m.FrameCount, t.FrameCount},
	}
	for _, c := range checks {
		if c.m <= 0 {
			e := apperr.New(apperr.Meta, "manifest field must be positive")
			e.Field = c.field
			e.Expected = ">0"
			e.Actual = c.m
			return e
		}
		if c.truth != c.m {
			e := apperr.New(apperr.Meta, "truth and frame manifest metadata disagree")
			e.Field = c.field
			e.Expected = c.m
			e.Actual = c.truth
			return e
		}
	}
	if m.FormatVersion != model.FormatVersion {
		e := apperr.New(apperr.Meta, "unsupported manifest format version")
		e.Field = "format_version"
		e.Expected = model.FormatVersion
		e.Actual = m.FormatVersion
		return e
	}
	if len(m.TimestampsUS) != m.FrameCount {
		e := apperr.New(apperr.Meta, "timestamp count does not match frame_count")
		e.Field = "frame_count"
		e.Expected = m.FrameCount
		e.Actual = len(m.TimestampsUS)
		return e
	}
	if len(t.Watermarks) != m.FrameCount {
		e := apperr.New(apperr.Meta, "watermark truth count does not match frame_count")
		e.Field = "frame_count"
		e.Expected = m.FrameCount
		e.Actual = len(t.Watermarks)
		return e
	}
	for i := range t.Watermarks {
		w := t.Watermarks[i]
		if w.Frame < 0 || w.Frame >= m.FrameCount {
			e := apperr.New(apperr.Meta, "watermark truth frame is outside the sequence")
			e.Field = "watermarks"
			e.Expected = fmt.Sprintf("0..%d", m.FrameCount-1)
			e.Actual = w.Frame
			return e
		}
		if w.X < 0 || w.Y < 0 || w.Width <= 0 || w.Height <= 0 ||
			w.X+w.Width > m.Width || w.Y+w.Height > m.Height {
			e := apperr.New(apperr.Meta, "watermark region is outside frame bounds")
			e.Field = "watermarks"
			e.Expected = fmt.Sprintf("inside %dx%d", m.Width, m.Height)
			e.Actual = []int{w.X, w.Y, w.Width, w.Height}
			return e
		}
		d.watermarks[w.Frame] = w
	}
	for i := 1; i < len(m.TimestampsUS); i++ {
		if m.TimestampsUS[i] <= m.TimestampsUS[i-1] {
			e := apperr.New(apperr.Order, "frame timestamps are not strictly increasing")
			e.Field = "timestamps_us"
			e.Expected = fmt.Sprintf(">%d", m.TimestampsUS[i-1])
			e.Actual = m.TimestampsUS[i]
			return apperr.At(e, i, d.WatermarkBlock(i))
		}
	}
	for i := range t.Shapes {
		f := t.Shapes[i]
		if f.Frame < 0 || f.Frame >= m.FrameCount {
			e := apperr.New(apperr.Meta, "shape truth frame is outside the sequence")
			e.Field = "shapes"
			e.Expected = fmt.Sprintf("0..%d", m.FrameCount-1)
			e.Actual = f.Frame
			return e
		}
		d.shapes[f.Frame] = f.Boxes
	}
	for _, r := range t.Replacements {
		if r.Start < 0 || r.End < r.Start || r.End >= m.FrameCount {
			e := apperr.New(apperr.Meta, "replacement truth interval is outside the sequence")
			e.Field = "replacements"
			e.Expected = fmt.Sprintf("0..%d", m.FrameCount-1)
			e.Actual = r
			return e
		}
	}
	if t.Tolerance < 0 {
		e := apperr.New(apperr.Meta, "replacement tolerance must not be negative")
		e.Field = "tolerance_frames"
		e.Expected = ">=0"
		e.Actual = t.Tolerance
		return e
	}
	if m.Tolerance == 0 {
		d.Manifest.Tolerance = t.Tolerance
	}
	return nil
}

func positiveOr(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}

func (d *Dataset) validateFrameFiles() error {
	for i := 0; i < d.Manifest.FrameCount; i++ {
		if err := d.validateFrameFile(i); err != nil {
			return err
		}
	}
	return nil
}

func (d *Dataset) validateFrameFile(index int) error {
	path := FramePath(d.Dir, d.Manifest.FramePattern, index)
	f, err := os.Open(path)
	if err != nil {
		e := apperr.Wrap(apperr.Decode, "frame file is missing or unreadable", err)
		e.Field = "frame"
		e.Frame = index
		return e
	}
	defer f.Close()
	headerBytes := make([]byte, HeaderSize)
	if _, err := io.ReadFull(f, headerBytes); err != nil {
		e := apperr.Wrap(apperr.Decode, "frame data is truncated in the header", err)
		e.Frame = index
		return e
	}
	h, err := DecodeHeader(headerBytes)
	if err != nil {
		if e := apperr.As(err); e != nil {
			e.Frame = index
		}
		return err
	}
	m := d.Manifest
	metaChecks := []struct {
		field         string
		expected, got int
	}{
		{"width", m.Width, int(h.Width)},
		{"height", m.Height, int(h.Height)},
		{"channels", m.Channels, int(h.Channels)},
	}
	for _, c := range metaChecks {
		if c.expected != c.got {
			e := apperr.New(apperr.Meta, "frame header metadata disagree with manifest")
			e.Field = c.field
			e.Expected = c.expected
			e.Actual = c.got
			return apperr.At(e, index, apperr.Block{})
		}
	}
	if int(h.Frame) != index {
		e := apperr.New(apperr.Order, "frame header index is out of order")
		e.Field = "frame_index"
		e.Expected = index
		e.Actual = h.Frame
		return apperr.At(e, index, d.WatermarkBlock(index))
	}
	expectedPayload := m.Width * m.Height * m.Channels
	info, err := f.Stat()
	if err != nil {
		e := apperr.Wrap(apperr.Decode, "cannot stat frame file", err)
		e.Frame = index
		return e
	}
	if int64(h.Payload) != int64(expectedPayload) {
		e := apperr.New(apperr.Meta, "frame payload size does not match dimensions and channels")
		e.Field = "payload"
		e.Expected = expectedPayload
		e.Actual = h.Payload
		return apperr.At(e, index, apperr.Block{})
	}
	if info.Size() < int64(HeaderSize)+int64(h.Payload) {
		e := apperr.New(apperr.Decode, "frame pixel payload is truncated")
		e.Expected = int64(HeaderSize) + int64(h.Payload)
		e.Actual = info.Size()
		return apperr.At(e, index, apperr.Block{})
	}
	if info.Size() > int64(HeaderSize)+int64(h.Payload) {
		e := apperr.New(apperr.Decode, "frame file contains trailing bytes")
		e.Expected = int64(HeaderSize) + int64(h.Payload)
		e.Actual = info.Size()
		return apperr.At(e, index, apperr.Block{})
	}
	if h.Flags&FlagBadFrame != 0 {
		e := apperr.New(apperr.Frame, "frame is marked as corrupt by the generator")
		e.Field = "frame_flags"
		e.Expected = 0
		e.Actual = h.Flags
		block := d.InjectionBlock(index)
		if block.W == 0 {
			block = model.BlockAtPixel(0, 0, m.Width, m.Height, positiveOr(m.BlockSize, 8))
		}
		return apperr.At(e, index, block.AppErr())
	}
	return nil
}

func (d *Dataset) validateWatermarks() error {
	for i := 0; i < d.Manifest.FrameCount; i++ {
		w := d.WatermarkFor(i)
		got, err := d.decodeWatermark(i, w)
		if err != nil {
			if e := apperr.As(err); e != nil {
				e.Frame = i
				e.Block = model.BlockAtPixel(w.X, w.Y, d.Manifest.Width, d.Manifest.Height, positiveOr(d.Manifest.BlockSize, 8)).AppErr()
			}
			return err
		}
		if got != w.Value {
			e := apperr.New(apperr.Mark, "frame number watermark decoded to an unexpected value")
			e.Field = "watermark_value"
			e.Expected = w.Value
			e.Actual = got
			return apperr.At(e, i, model.BlockAtPixel(w.X, w.Y, d.Manifest.Width, d.Manifest.Height, positiveOr(d.Manifest.BlockSize, 8)).AppErr())
		}
		if got != i {
			e := apperr.New(apperr.Order, "frame number watermark is not monotonic")
			e.Field = "watermark_value"
			e.Expected = i
			e.Actual = got
			return apperr.At(e, i, model.BlockAtPixel(w.X, w.Y, d.Manifest.Width, d.Manifest.Height, positiveOr(d.Manifest.BlockSize, 8)).AppErr())
		}
	}
	return nil
}

func (d *Dataset) decodeWatermark(index int, w model.Watermark) (int, error) {
	if w.Width < WatermarkWidth || w.Height < WatermarkHeight {
		return 0, apperr.New(apperr.Mark, "watermark region is too small")
	}
	path := FramePath(d.Dir, d.Manifest.FramePattern, index)
	f, err := os.Open(path)
	if err != nil {
		return 0, apperr.Wrap(apperr.Decode, "cannot open frame for watermark", err)
	}
	defer f.Close()
	rowBytes := w.Width * d.Manifest.Channels
	region := make([]byte, rowBytes*w.Height)
	base := int64(HeaderSize) + int64(w.Y*d.Manifest.Width*d.Manifest.Channels+w.X*d.Manifest.Channels)
	for row := 0; row < w.Height; row++ {
		off := base + int64(row*d.Manifest.Width*d.Manifest.Channels)
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return 0, apperr.Wrap(apperr.Decode, "cannot seek frame watermark", err)
		}
		if _, err := io.ReadFull(f, region[row*rowBytes:(row+1)*rowBytes]); err != nil {
			return 0, apperr.Wrap(apperr.Decode, "cannot read frame watermark", err)
		}
	}
	pixel := func(x, y int) [3]byte {
		off := y*rowBytes + x*d.Manifest.Channels
		var out [3]byte
		copy(out[:], region[off:off+min(3, d.Manifest.Channels)])
		return out
	}
	corner := pixel(0, 0)
	if !closeColor(corner, watermarkBorder, 8) {
		return 0, apperr.New(apperr.Mark, "watermark border is missing")
	}
	value := 0
	for bit := 0; bit < WatermarkBits; bit++ {
		cx := WatermarkBorder + bit*WatermarkCell + WatermarkCell/2
		cy := WatermarkBorder + WatermarkCell/2
		c := pixel(cx, cy)
		switch {
		case closeColor(c, watermarkColor0, 20):
		case closeColor(c, watermarkColor1, 20):
			value |= 1 << bit
		default:
			return 0, apperr.New(apperr.Mark, "watermark bit cell is missing")
		}
	}
	return value, nil
}

func closeColor(a, b [3]byte, tolerance int) bool {
	for i := 0; i < 3; i++ {
		d := int(a[i]) - int(b[i])
		if d < 0 {
			d = -d
		}
		if d > tolerance {
			return false
		}
	}
	return true
}

func (d *Dataset) validateBaselines() error {
	frameBytes := int64(d.Manifest.Width * d.Manifest.Height * d.Manifest.Channels)
	for i := 0; i < d.Manifest.FrameCount; i++ {
		path := d.BaselinePathFor(i)
		info, err := os.Stat(path)
		if err != nil {
			e := apperr.Wrap(apperr.Decode, "baseline frame is missing or unreadable", err)
			e.Field = "baseline"
			e.Frame = i
			return e
		}
		if info.Size() != frameBytes {
			e := apperr.New(apperr.Decode, "baseline frame pixel payload has the wrong size")
			e.Field = "baseline_bytes"
			e.Expected = frameBytes
			e.Actual = info.Size()
			return apperr.At(e, i, apperr.Block{})
		}
	}
	return nil
}

func (d *Dataset) computeContentHash() (string, error) {
	h := sha256.New()
	writeChunk := func(b []byte) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(b)))
		h.Write(length[:])
		h.Write(b)
	}
	writeChunk(d.ManifestBytes)
	writeChunk(d.TruthBytes)
	for i := 0; i < d.Manifest.FrameCount; i++ {
		for _, path := range []string{d.FramePath(i), d.BaselinePathFor(i)} {
			f, err := os.Open(path)
			if err != nil {
				return "", apperr.Wrap(apperr.Decode, "cannot hash input file", err)
			}
			if _, err := io.Copy(h, f); err != nil {
				f.Close()
				return "", apperr.Wrap(apperr.Decode, "cannot hash input file", err)
			}
			f.Close()
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (d *Dataset) FramePath(index int) string {
	return FramePath(d.Dir, d.Manifest.FramePattern, index)
}

func (d *Dataset) BaselinePathFor(index int) string {
	if index >= 0 && index < len(d.Truth.Baseline) && d.Truth.Baseline[index] != "" {
		return filepath.Join(d.Dir, filepath.FromSlash(d.Truth.Baseline[index]))
	}
	return BaselinePath(d.Dir, "", index)
}

func (d *Dataset) FrameBytes() int64 {
	return int64(d.Manifest.Width * d.Manifest.Height * d.Manifest.Channels)
}

func (d *Dataset) ReadFrame(index int) (*Frame, error) {
	if index < 0 || index >= d.Manifest.FrameCount {
		e := apperr.New(apperr.Meta, "frame index is outside the sequence")
		e.Field = "frame_count"
		e.Expected = fmt.Sprintf("0..%d", d.Manifest.FrameCount-1)
		e.Actual = index
		return nil, e
	}
	data, err := os.ReadFile(d.FramePath(index))
	if err != nil {
		e := apperr.Wrap(apperr.Decode, "cannot read frame", err)
		e.Frame = index
		return nil, e
	}
	h, err := DecodeHeader(data)
	if err != nil {
		if e := apperr.As(err); e != nil {
			e.Frame = index
		}
		return nil, err
	}
	payload := int(h.Payload)
	if len(data) < HeaderSize+payload {
		e := apperr.New(apperr.Decode, "frame data is truncated")
		e.Frame = index
		return nil, e
	}
	pixels := append([]byte(nil), data[HeaderSize:HeaderSize+payload]...)
	return &Frame{
		Width: int(h.Width), Height: int(h.Height), Channels: int(h.Channels),
		Index: int(h.Frame), Pixels: pixels,
	}, nil
}

func (d *Dataset) ReadBaseline(index int) ([]byte, error) {
	data, err := os.ReadFile(d.BaselinePathFor(index))
	if err != nil {
		return nil, apperr.Wrap(apperr.Decode, "cannot read baseline frame", err)
	}
	if int64(len(data)) != d.FrameBytes() {
		e := apperr.New(apperr.Decode, "baseline frame size mismatch")
		e.Expected = d.FrameBytes()
		e.Actual = len(data)
		e.Frame = index
		return nil, e
	}
	return data, nil
}

func (d *Dataset) WatermarkFor(index int) model.Watermark {
	if w, ok := d.watermarks[index]; ok {
		return w
	}
	return model.Watermark{Frame: index, X: 0, Y: 0, Width: WatermarkWidth, Height: WatermarkHeight}
}

func (d *Dataset) WatermarkBlock(index int) apperr.Block {
	w := d.WatermarkFor(index)
	return model.BlockAtPixel(w.X, w.Y, d.Manifest.Width, d.Manifest.Height, positiveOr(d.Manifest.BlockSize, 8)).AppErr()
}

func (d *Dataset) ShapesFor(index int) []model.ShapeBox {
	return d.shapes[index]
}

func (d *Dataset) DecodeWatermark(index int) (int, error) {
	return d.decodeWatermark(index, d.WatermarkFor(index))
}

func (d *Dataset) InjectionBlock(index int) model.BlockRef {
	for _, inj := range d.Manifest.Injections {
		if inj.Frame == index {
			return inj.Block
		}
	}
	return model.BlockRef{}
}

func ListSamples(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, apperr.Wrap(apperr.Decode, "cannot list sample root", err)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, entry.Name(), "manifest.json")); err == nil {
			out = append(out, entry.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func ReadCatalog(root string) (json.RawMessage, error) {
	path := filepath.Join(root, "catalog.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			ids, listErr := ListSamples(root)
			if listErr != nil {
				return nil, listErr
			}
			fallback := map[string]any{"samples": ids}
			return json.Marshal(fallback)
		}
		return nil, apperr.Wrap(apperr.Decode, "cannot read sample catalog", err)
	}
	if !json.Valid(data) {
		return nil, apperr.New(apperr.Decode, "sample catalog is not valid JSON")
	}
	return data, nil
}

func NormalizeSampleID(root, id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", apperr.New(apperr.Decode, "sample id is empty")
	}
	clean := filepath.Clean(id)
	if clean != id || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return "", apperr.New(apperr.Decode, "sample id contains an unsafe path")
	}
	if _, err := os.Stat(filepath.Join(root, id, "manifest.json")); err != nil {
		e := apperr.Wrap(apperr.Decode, "sample does not exist", err)
		e.Field = "sample"
		return "", e
	}
	return id, nil
}
