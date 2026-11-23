package tex

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
)

// Image holds interleaved samples in the declared source channel order. Values
// are represented as uint16 regardless of bit depth; for 8-bit images every
// sample is in [0,255].
type Image struct {
	ID          string `json:"id"`
	Path        string `json:"path,omitempty"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	BitDepth    int    `json:"bit_depth"`
	Layout      Layout `json:"layout"`
	Pixels      []uint16
	ContentHash string `json:"content_hash"`
}

func (img *Image) Channels() int {
	return img.Layout.ChannelCount()
}

func (img *Image) MaxValue() uint16 {
	if img.BitDepth == 8 {
		return 255
	}
	return 65535
}

func (img *Image) PixelCount() int {
	return img.Width * img.Height
}

func (img *Image) Sample(x, y, channel int) uint16 {
	offset := (y*img.Width + x) * img.Channels()
	return img.Pixels[offset+channel]
}

func (img *Image) Validate() error {
	if img.Width <= 0 || img.Height <= 0 {
		return ErrDecode("image dimensions must be positive")
	}
	if img.BitDepth != 8 && img.BitDepth != 16 {
		return ErrDecode(fmt.Sprintf("unsupported source bit depth %d", img.BitDepth))
	}
	if !img.Layout.Valid() {
		return ErrDecode(fmt.Sprintf("unsupported source layout %q", img.Layout))
	}
	want := img.PixelCount() * img.Channels()
	if len(img.Pixels) != want {
		return ErrDecode(fmt.Sprintf("pixel buffer has %d samples, want %d", len(img.Pixels), want))
	}
	max := img.MaxValue()
	for i, value := range img.Pixels {
		if value > max {
			return ErrDecode(fmt.Sprintf("sample %d=%d exceeds %d-bit range", i, value, img.BitDepth))
		}
	}
	return nil
}

// CanonicalBytes returns a stable byte representation used for content hashes
// and checkpoint validation.
func (img *Image) CanonicalBytes() []byte {
	header := fmt.Sprintf("TWB-IMAGE\n%d\n%d\n%d\n%s\n", img.Width, img.Height, img.BitDepth, img.Layout)
	out := make([]byte, 0, len(header)+len(img.Pixels)*2)
	out = append(out, header...)
	var buf [2]byte
	for _, value := range img.Pixels {
		binary.BigEndian.PutUint16(buf[:], value)
		out = append(out, buf[:]...)
	}
	return out
}

func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (img *Image) ComputeContentHash() string {
	return HashBytes(img.CanonicalBytes())
}

func (img *Image) EnsureHash() {
	if img.ContentHash == "" {
		img.ContentHash = img.ComputeContentHash()
	}
}

func NewImage(id string, width, height, bitDepth int, layout Layout, pixels []uint16) (*Image, error) {
	img := &Image{
		ID:       id,
		Width:    width,
		Height:   height,
		BitDepth: bitDepth,
		Layout:   layout,
		Pixels:   pixels,
	}
	if err := img.Validate(); err != nil {
		return nil, err
	}
	img.ContentHash = img.ComputeContentHash()
	return img, nil
}

func NormalizeValue(value uint16, bitDepth int) float64 {
	max := 255.0
	if bitDepth == 16 {
		max = 65535.0
	}
	return math.Max(0, math.Min(1, float64(value)/max))
}
