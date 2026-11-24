package mfxfmt

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

const Magic = "MFXPKG01"

type Header struct {
	Format           string            `json:"format"`
	Width            int               `json:"width"`
	Height           int               `json:"height"`
	ImageChannels    int               `json:"imageChannels"`
	ImageBitDepth    int               `json:"imageBitDepth"`
	ChromaSampling   string            `json:"chromaSampling"`
	PixelCount       int64             `json:"pixelCount"`
	ImageSampleBytes int64             `json:"imageSampleBytes"`
	AudioSampleRate  int               `json:"audioSampleRate"`
	AudioBitDepth    int               `json:"audioBitDepth"`
	AudioChannels    int               `json:"audioChannels"`
	AudioSamples     int64             `json:"audioSamples"`
	AudioSampleBytes int64             `json:"audioSampleBytes"`
	Metadata         map[string]string `json:"metadata"`
	MetadataFields   []string          `json:"metadataFields"`
	AlgorithmVersion string            `json:"algorithmVersion"`
}

type Package struct {
	Header       Header
	ImageOffset  int
	AudioOffset  int
	TotalSize    int
	TrailingSize int
}

func SortedFields(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func EncodeHeader(h Header) ([]byte, error) {
	if h.Metadata == nil {
		h.Metadata = map[string]string{}
	}
	h.MetadataFields = SortedFields(h.Metadata)
	return json.Marshal(h)
}

func WritePrefix(dst io.Writer, h Header) error {
	header, err := EncodeHeader(h)
	if err != nil {
		return err
	}
	if len(header) > int(^uint32(0)) {
		return fmt.Errorf("header too large")
	}
	if _, err := io.WriteString(dst, Magic); err != nil {
		return err
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(header)))
	if _, err := dst.Write(length[:]); err != nil {
		return err
	}
	_, err = dst.Write(header)
	return err
}

func Parse(data []byte) (Package, error) {
	var out Package
	if len(data) < 12 {
		return out, fmt.Errorf("package is shorter than header prefix")
	}
	if string(data[:8]) != Magic {
		return out, fmt.Errorf("bad magic %q", string(data[:8]))
	}
	headerLen := int(binary.BigEndian.Uint32(data[8:12]))
	if headerLen <= 0 || 12+headerLen > len(data) {
		return out, fmt.Errorf("invalid header length %d", headerLen)
	}
	if err := json.Unmarshal(data[12:12+headerLen], &out.Header); err != nil {
		return out, fmt.Errorf("decode header: %w", err)
	}
	h := out.Header
	if h.Format != "MFX1" {
		return out, fmt.Errorf("unsupported format %q", h.Format)
	}
	if h.Width <= 0 || h.Height <= 0 {
		return out, fmt.Errorf("invalid dimensions %dx%d", h.Width, h.Height)
	}
	if h.PixelCount != int64(h.Width)*int64(h.Height) {
		return out, fmt.Errorf("pixel count mismatch")
	}
	if h.ImageChannels != 3 && h.ImageChannels != 4 {
		return out, fmt.Errorf("invalid channel count %d", h.ImageChannels)
	}
	if h.ImageSampleBytes != h.PixelCount*int64(h.ImageChannels)*2 {
		return out, fmt.Errorf("image byte count mismatch")
	}
	if h.AudioChannels <= 0 || h.AudioChannels > 2 {
		return out, fmt.Errorf("invalid audio channel count %d", h.AudioChannels)
	}
	if h.AudioSampleBytes != h.AudioSamples*int64(h.AudioChannels)*4 {
		return out, fmt.Errorf("audio byte count mismatch")
	}
	out.ImageOffset = 12 + headerLen
	out.AudioOffset = out.ImageOffset + int(h.ImageSampleBytes)
	out.TotalSize = out.AudioOffset + int(h.AudioSampleBytes)
	out.TrailingSize = len(data) - out.TotalSize
	if out.ImageOffset > len(data) || out.AudioOffset > len(data) || out.TotalSize > len(data) {
		return out, fmt.Errorf("package payload is truncated")
	}
	if out.TrailingSize != 0 {
		return out, fmt.Errorf("package has %d trailing bytes", out.TrailingSize)
	}
	if h.MetadataFields == nil {
		h.MetadataFields = []string{}
	}
	if !equalStrings(h.MetadataFields, SortedFields(h.Metadata)) {
		return out, fmt.Errorf("metadata field declaration is not canonical")
	}
	out.Header = h
	return out, nil
}

func Summary(data []byte) (map[string]any, error) {
	pkg, err := Parse(data)
	if err != nil {
		return nil, err
	}
	h := pkg.Header
	return map[string]any{
		"width":            h.Width,
		"height":           h.Height,
		"channels":         h.ImageChannels,
		"pixelCount":       h.PixelCount,
		"audioSamples":     h.AudioSamples,
		"audioChannels":    h.AudioChannels,
		"imageBitDepth":    h.ImageBitDepth,
		"chromaSampling":   h.ChromaSampling,
		"audioSampleRate":  h.AudioSampleRate,
		"audioBitDepth":    h.AudioBitDepth,
		"metadataFields":   h.MetadataFields,
		"algorithmVersion": h.AlgorithmVersion,
		"sizeBytes":        len(data),
	}, nil
}

func SHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func equalStrings(a, b []string) bool {
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

func ImageSamples(data []byte, pkg Package) []uint16 {
	if pkg.ImageOffset < 0 || pkg.AudioOffset > len(data) {
		return nil
	}
	n := (pkg.AudioOffset - pkg.ImageOffset) / 2
	out := make([]uint16, n)
	for i := 0; i < n; i++ {
		out[i] = binary.LittleEndian.Uint16(data[pkg.ImageOffset+i*2:])
	}
	return out
}

func AudioSamples(data []byte, pkg Package) []uint32 {
	if pkg.AudioOffset < 0 || pkg.TotalSize > len(data) {
		return nil
	}
	n := (pkg.TotalSize - pkg.AudioOffset) / 4
	out := make([]uint32, n)
	for i := 0; i < n; i++ {
		out[i] = binary.LittleEndian.Uint32(data[pkg.AudioOffset+i*4:])
	}
	return out
}

func DiffImage(a, b []byte) (maxDelta int, changed int, cells []int, width, height, grid int) {
	pa, errA := Parse(a)
	pb, errB := Parse(b)
	if errA != nil || errB != nil {
		return -1, 0, nil, 0, 0, 0
	}
	if pa.Header.Width != pb.Header.Width || pa.Header.Height != pb.Header.Height ||
		pa.Header.ImageChannels != pb.Header.ImageChannels {
		return -1, 0, nil, 0, 0, 0
	}
	width, height = pa.Header.Width, pa.Header.Height
	grid = 16
	cells = make([]int, grid*grid)
	ia := ImageSamples(a, pa)
	ib := ImageSamples(b, pb)
	channels := pa.Header.ImageChannels
	for i := 0; i < len(ia) && i < len(ib); i++ {
		d := int(ia[i]) - int(ib[i])
		if d < 0 {
			d = -d
		}
		if d > maxDelta {
			maxDelta = d
		}
		if d != 0 {
			changed++
			pixel := i / channels
			x := pixel % width
			y := pixel / width
			cell := (y*grid/height)*grid + x*grid/width
			if cell < 0 {
				cell = 0
			}
			if cell >= len(cells) {
				cell = len(cells) - 1
			}
			cells[cell]++
		}
	}
	return maxDelta, changed, cells, width, height, grid
}

func CanonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}
