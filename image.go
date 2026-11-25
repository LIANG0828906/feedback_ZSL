package texture

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// Image stores native interleaved samples in row-major order. Samples are
// uint16 even for 8-bit images, which keeps conversions explicit and lossless
// until a declared quantization step.
type Image struct {
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	BitDepth int       `json:"bit_depth"`
	Layout   []Channel `json:"layout"`
	Pix      []uint16  `json:"-"`
}

func NewImage(width, height, bitDepth int, layout []Channel) (*Image, error) {
	if width <= 0 || height <= 0 {
		return nil, errf(CodePara, "$.image", "width/height", "must be positive")
	}
	if bitDepth < 1 || bitDepth > 16 {
		return nil, errf(CodePara, "$.image.bit_depth", "bit_depth", "must be in [1,16]")
	}
	if len(layout) == 0 || len(layout) > 4 {
		return nil, errf(CodePara, "$.image.layout", "layout", "must contain 1..4 channels")
	}
	return &Image{
		Width: width, Height: height, BitDepth: bitDepth,
		Layout: append([]Channel(nil), layout...),
		Pix:    make([]uint16, width*height*len(layout)),
	}, nil
}

func (im *Image) Channels() int {
	if im == nil {
		return 0
	}
	return len(im.Layout)
}

func (im *Image) MaxValue() uint16 {
	if im == nil || im.BitDepth <= 0 {
		return 0
	}
	if im.BitDepth >= 16 {
		return 65535
	}
	return uint16((1 << im.BitDepth) - 1)
}

func (im *Image) ChannelIndex(ch Channel) int {
	for i, c := range im.Layout {
		if c == ch {
			return i
		}
	}
	return -1
}

func (im *Image) Offset(x, y, ch int) int {
	return (y*im.Width+x)*len(im.Layout) + ch
}

func (im *Image) At(x, y, ch int) uint16 {
	return im.Pix[im.Offset(x, y, ch)]
}

func (im *Image) ChannelAt(x, y int, ch Channel) (uint16, bool) {
	idx := im.ChannelIndex(ch)
	if idx < 0 {
		return 0, false
	}
	return im.At(x, y, idx), true
}

func (im *Image) Set(x, y, ch int, v uint16) {
	im.Pix[im.Offset(x, y, ch)] = v
}

func (im *Image) Clone() *Image {
	if im == nil {
		return nil
	}
	out := &Image{
		Width: im.Width, Height: im.Height, BitDepth: im.BitDepth,
		Layout: append([]Channel(nil), im.Layout...),
		Pix:    append([]uint16(nil), im.Pix...),
	}
	return out
}

func (im *Image) Validate() error {
	if im == nil {
		return errf(CodeSchema, "$.image", "image", "image is nil")
	}
	if im.Width <= 0 || im.Height <= 0 {
		return errf(CodePara, "$.image", "width/height", "must be positive")
	}
	if im.BitDepth < 1 || im.BitDepth > 16 {
		return errf(CodePara, "$.image.bit_depth", "bit_depth", "must be in [1,16]")
	}
	if len(im.Layout) == 0 || len(im.Layout) > 4 {
		return errf(CodePara, "$.image.layout", "layout", "must contain 1..4 channels")
	}
	if len(im.Pix) != im.Width*im.Height*len(im.Layout) {
		return errf(CodeSchema, "$.image.pix", "pix", "sample count %d does not match %d", len(im.Pix), im.Width*im.Height*len(im.Layout))
	}
	maxValue := im.MaxValue()
	for i, v := range im.Pix {
		if v > maxValue {
			return errf(CodePara, fmt.Sprintf("$.image.pix[%d]", i), "pix", "value %d exceeds %d-bit maximum %d", v, im.BitDepth, maxValue)
		}
	}
	return nil
}

func (im *Image) Hash() string {
	if im == nil {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "%d:%d:%d:%s:", im.Width, im.Height, im.BitDepth, LayoutString(im.Layout))
	var buf [2]byte
	for _, v := range im.Pix {
		binary.LittleEndian.PutUint16(buf[:], v)
		h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func HashRegion(im *Image, rect Rect) string {
	if im == nil {
		return ""
	}
	r := rect.Intersection(FullRect(im.Width, im.Height))
	h := sha256.New()
	fmt.Fprintf(h, "%d:%d:%d:%s:%d,%d,%d,%d:",
		im.Width, im.Height, im.BitDepth, LayoutString(im.Layout),
		r.X, r.Y, r.Width, r.Height)
	var buf [2]byte
	for y := r.Y; y < r.Bottom(); y++ {
		for x := r.X; x < r.Right(); x++ {
			base := im.Offset(x, y, 0)
			for c := range im.Layout {
				binary.LittleEndian.PutUint16(buf[:], im.Pix[base+c])
				h.Write(buf[:])
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (im *Image) EqualWithDiff(other *Image) (bool, *PixelDiff) {
	if im == nil || other == nil || im.Width != other.Width || im.Height != other.Height ||
		im.BitDepth != other.BitDepth || !channelsEqual(im.Layout, other.Layout) {
		return false, &PixelDiff{Field: "image_metadata"}
	}
	for y := 0; y < im.Height; y++ {
		for x := 0; x < im.Width; x++ {
			for c := range im.Layout {
				i := im.Offset(x, y, c)
				if im.Pix[i] != other.Pix[i] {
					return false, &PixelDiff{
						X: x, Y: y, Channel: im.Layout[c],
						Field: fmt.Sprintf("image.%s", im.Layout[c]),
						Left:  im.Pix[i], Right: other.Pix[i],
					}
				}
			}
		}
	}
	return true, nil
}

type PixelDiff struct {
	X       int     `json:"x"`
	Y       int     `json:"y"`
	Channel Channel `json:"channel,omitempty"`
	Field   string  `json:"field"`
	Left    uint16  `json:"left"`
	Right   uint16  `json:"right"`
}

func (d *PixelDiff) String() string {
	if d == nil {
		return ""
	}
	if d.Channel != "" {
		return fmt.Sprintf("(%d,%d) %s: %d != %d", d.X, d.Y, d.Field, d.Left, d.Right)
	}
	return fmt.Sprintf("(%d,%d) %s", d.X, d.Y, d.Field)
}

// ImageDocument is the deterministic on-disk representation used by samples,
// chunk checkpoints, and the verifier.
type ImageDocument struct {
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	BitDepth int       `json:"bit_depth"`
	Layout   []Channel `json:"layout"`
	Encoding string    `json:"encoding"`
	Data     string    `json:"data"`
	Hash     string    `json:"hash"`
}

func (im *Image) Document() ImageDocument {
	raw := make([]byte, len(im.Pix)*2)
	for i, v := range im.Pix {
		binary.LittleEndian.PutUint16(raw[i*2:], v)
	}
	return ImageDocument{
		Width: im.Width, Height: im.Height, BitDepth: im.BitDepth,
		Layout: append([]Channel(nil), im.Layout...), Encoding: "u16le/base64",
		Data: base64.StdEncoding.EncodeToString(raw), Hash: im.Hash(),
	}
}

func ImageFromDocument(doc ImageDocument) (*Image, error) {
	if doc.Encoding != "" && doc.Encoding != "u16le/base64" {
		return nil, errf(CodeSchema, "$.image.encoding", "encoding", "unsupported encoding %q", doc.Encoding)
	}
	raw, err := base64.StdEncoding.DecodeString(doc.Data)
	if err != nil {
		return nil, errf(CodeSchema, "$.image.data", "data", "invalid base64: %v", err)
	}
	im, err := NewImage(doc.Width, doc.Height, doc.BitDepth, doc.Layout)
	if err != nil {
		return nil, err
	}
	if len(raw) != len(im.Pix)*2 {
		return nil, errf(CodeSchema, "$.image.data", "data", "byte length %d does not match image", len(raw))
	}
	for i := range im.Pix {
		im.Pix[i] = binary.LittleEndian.Uint16(raw[i*2:])
	}
	if err := im.Validate(); err != nil {
		return nil, err
	}
	if doc.Hash != "" && doc.Hash != im.Hash() {
		return nil, errf(CodeStore, "$.image.hash", "hash", "image hash mismatch")
	}
	return im, nil
}

func EncodeSamples(pix []uint16) []byte {
	raw := make([]byte, len(pix)*2)
	for i, v := range pix {
		binary.LittleEndian.PutUint16(raw[i*2:], v)
	}
	return raw
}

func DecodeSamples(raw []byte) ([]uint16, error) {
	if len(raw)%2 != 0 {
		return nil, errf(CodeStore, "$.pixels", "pixels", "odd byte length %d", len(raw))
	}
	out := make([]uint16, len(raw)/2)
	for i := range out {
		out[i] = binary.LittleEndian.Uint16(raw[i*2:])
	}
	return out, nil
}

func ReadImageFile(path string) (*Image, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc ImageDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return ImageFromDocument(doc)
}

func WriteImageFile(path string, im *Image) error {
	b, err := json.MarshalIndent(im.Document(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
