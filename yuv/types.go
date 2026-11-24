// Package yuv implements deterministic planar YUV 4:2:0 sample generation,
// strict loading/validation, chroma upsampling, and display helpers.
package yuv

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"

	"yuvench/errs"
)

const SchemaVersion = "yuv420-workbench/v1"

type Quantization struct {
	Range string `json:"range"`
	Min   int    `json:"min"`
	Max   int    `json:"max"`
}

type PlaneSpec struct {
	File     string `json:"file"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Channels int    `json:"channels"`
	BitDepth int    `json:"bitDepth"`
}

type BlockTruth struct {
	Name    string `json:"name"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	CenterX int    `json:"centerX"`
	CenterY int    `json:"centerY"`
	YValue  uint16 `json:"yValue"`
	UValue  uint16 `json:"uValue"`
	VValue  uint16 `json:"vValue"`
}

type SampleTruth struct {
	X       int    `json:"x"`
	Y       int    `json:"y"`
	ChromaX int    `json:"chromaX"`
	ChromaY int    `json:"chromaY"`
	YValue  uint16 `json:"yValue"`
	UValue  uint16 `json:"uValue"`
	VValue  uint16 `json:"vValue"`
}

type ImageMeta struct {
	Name            string                 `json:"name"`
	Width           int                    `json:"width"`
	Height          int                    `json:"height"`
	BitDepth        int                    `json:"bitDepth"`
	Channels        int                    `json:"channels"`
	ChromaAlignment string                 `json:"chromaAlignment"`
	Upsample        string                 `json:"upsample"`
	Endianness      string                 `json:"endianness"`
	Layout          string                 `json:"layout"`
	Quantization    Quantization           `json:"quantization"`
	Planes          map[string]PlaneSpec   `json:"planes"`
	Blocks          []BlockTruth           `json:"blocks"`
	Samples         []SampleTruth          `json:"samples"`
	Extra           map[string]interface{} `json:"-"`
}

type Manifest struct {
	SchemaVersion string      `json:"schemaVersion"`
	Generator     string      `json:"generator"`
	Seed          int64       `json:"seed"`
	Images        []ImageMeta `json:"images"`
}

// Plane stores canonical unsigned code values, even for 8-bit inputs. It keeps
// one channel and the exact source dimensions.
type Plane struct {
	Name     string
	Width    int
	Height   int
	Channels int
	BitDepth int
	MinCode  uint16
	MaxCode  uint16
	Data     []uint16
}

type Image struct {
	Meta          ImageMeta
	ManifestPath  string
	ManifestHash  string
	InputHash     string
	Y             *Plane
	U             *Plane
	V             *Plane
	PlaneHashes   map[string]string
	ChromaVersion string
}

// Composite is a full-resolution YUV 4:4:4 image. It is the interleaved,
// dimension-preserving result of chroma upsampling.
type Composite struct {
	Width      int
	Height     int
	BitDepth   int
	QuantRange string
	YMin       uint16
	YMax       uint16
	UMin       uint16
	UMax       uint16
	VMin       uint16
	VMax       uint16
	Y          []uint16
	U          []uint16
	V          []uint16
}

func (p *Plane) Clone() *Plane {
	if p == nil {
		return nil
	}
	out := *p
	out.Data = append([]uint16(nil), p.Data...)
	return &out
}

func (p *Plane) Pixel(x, y int) uint16 {
	if p == nil || x < 0 || y < 0 || x >= p.Width || y >= p.Height {
		return 0
	}
	return p.Data[y*p.Width+x]
}

func (p *Plane) InBounds(x, y int) bool {
	return p != nil && x >= 0 && y >= 0 && x < p.Width && y < p.Height
}

func (p *Plane) EncodeRaw() []byte {
	if p == nil {
		return nil
	}
	if p.BitDepth <= 8 {
		out := make([]byte, len(p.Data))
		for i, value := range p.Data {
			out[i] = byte(value)
		}
		return out
	}
	out := make([]byte, len(p.Data)*2)
	for i, value := range p.Data {
		binary.LittleEndian.PutUint16(out[i*2:], value)
	}
	return out
}

func (p *Plane) Summary() string {
	if p == nil {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "plane:%s:%d:%d:%d:%d:%d:", p.Name, p.Width, p.Height, p.BitDepth, p.MinCode, p.MaxCode)
	h.Write(p.EncodeRaw())
	return hex.EncodeToString(h.Sum(nil))
}

func (p *Plane) ContentHash() string {
	return p.Summary()
}

func (p *Plane) SamePixels(other *Plane) bool {
	if p == nil || other == nil {
		return p == other
	}
	if p.Width != other.Width || p.Height != other.Height || p.BitDepth != other.BitDepth || len(p.Data) != len(other.Data) {
		return false
	}
	for i := range p.Data {
		if p.Data[i] != other.Data[i] {
			return false
		}
	}
	return true
}

func (c *Composite) Clone() *Composite {
	if c == nil {
		return nil
	}
	out := *c
	out.Y = append([]uint16(nil), c.Y...)
	out.U = append([]uint16(nil), c.U...)
	out.V = append([]uint16(nil), c.V...)
	return &out
}

func (c *Composite) Channel(name string) []uint16 {
	switch name {
	case "Y":
		return c.Y
	case "U":
		return c.U
	case "V":
		return c.V
	default:
		return nil
	}
}

func (c *Composite) EncodeRaw16LE() []byte {
	if c == nil {
		return nil
	}
	out := make([]byte, (len(c.Y)+len(c.U)+len(c.V))*2)
	offset := 0
	for _, plane := range [][]uint16{c.Y, c.U, c.V} {
		for _, value := range plane {
			binary.LittleEndian.PutUint16(out[offset:], value)
			offset += 2
		}
	}
	return out
}

func (c *Composite) Summary() string {
	if c == nil {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "composite:%d:%d:%d:%s:%d:%d:%d:%d:%d:%d:", c.Width, c.Height, c.BitDepth, c.QuantRange, c.YMin, c.YMax, c.UMin, c.UMax, c.VMin, c.VMax)
	h.Write(c.EncodeRaw16LE())
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Composite) SamePixels(other *Composite) bool {
	if c == nil || other == nil {
		return c == other
	}
	if c.Width != other.Width || c.Height != other.Height || c.BitDepth != other.BitDepth {
		return false
	}
	for _, pair := range [][2][]uint16{{c.Y, other.Y}, {c.U, other.U}, {c.V, other.V}} {
		if len(pair[0]) != len(pair[1]) {
			return false
		}
		for i := range pair[0] {
			if pair[0][i] != pair[1][i] {
				return false
			}
		}
	}
	return true
}

func (m ImageMeta) MaxCode() uint16 {
	if m.BitDepth <= 0 || m.BitDepth > 16 {
		return 0
	}
	if m.BitDepth == 16 {
		return math.MaxUint16
	}
	return uint16((1 << m.BitDepth) - 1)
}

func (m ImageMeta) PlaneLimits(plane string) (uint16, uint16) {
	maxCode := m.MaxCode()
	if m.Quantization.Range == "video" {
		scale := uint16(1)
		if m.BitDepth > 8 {
			scale = 1 << (m.BitDepth - 8)
		}
		if plane == "Y" {
			return 16 * scale, 235 * scale
		}
		return 16 * scale, 240 * scale
	}
	return 0, maxCode
}

func HashBytes(parts ...[]byte) string {
	h := sha256.New()
	for _, part := range parts {
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(part)))
		h.Write(size[:])
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func HashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func ValidateParameter(condition bool, field, message string) error {
	if condition {
		return nil
	}
	return errs.WithField(errs.CodeParameter, field, message)
}
