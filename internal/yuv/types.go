package yuv

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

const (
	AlignCenter   = "center"
	AlignTopLeft  = "top_left"
	UpsampleNear  = "nearest"
	UpsampleBilin = "bilinear"
)

type PlaneMeta struct {
	File     string `json:"file"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	BitDepth int    `json:"bit_depth"`
}

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

type BlockTruth struct {
	Name      string `json:"name"`
	CenterX   int    `json:"center_x"`
	CenterY   int    `json:"center_y"`
	Rect      Rect   `json:"rect"`
	Y         uint16 `json:"y"`
	U         uint16 `json:"u"`
	V         uint16 `json:"v"`
	ChromaX   int    `json:"chroma_x"`
	ChromaY   int    `json:"chroma_y"`
	LowDetail bool   `json:"low_detail,omitempty"`
}

type LumaTruth struct {
	X        int    `json:"x"`
	Y        int    `json:"y"`
	Expected uint16 `json:"expected"`
}

type ChromaTruth struct {
	Plane    string `json:"plane"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	Expected uint16 `json:"expected"`
}

type InterleavedTruth struct {
	X        int    `json:"x"`
	Y        int    `json:"y"`
	Expected uint16 `json:"expected_y"`
	U        uint16 `json:"expected_u"`
	V        uint16 `json:"expected_v"`
	Source   string `json:"source"`
}

type Metadata struct {
	FormatVersion   int                  `json:"format_version"`
	ID              string               `json:"id"`
	Width           int                  `json:"width"`
	Height          int                  `json:"height"`
	BitDepth        int                  `json:"bit_depth"`
	QuantMin        uint16               `json:"quant_min"`
	QuantMax        uint16               `json:"quant_max"`
	ChromaAlignment string               `json:"chroma_alignment"`
	Upsample        string               `json:"upsample"`
	Planes          map[string]PlaneMeta `json:"planes"`
	InputHashes     map[string]string    `json:"input_hashes,omitempty"`
	Blocks          []BlockTruth         `json:"blocks"`
	LumaSamples     []LumaTruth          `json:"luma_samples"`
	ChromaSamples   []ChromaTruth        `json:"chroma_samples"`
	Interleaved     []InterleavedTruth   `json:"interleaved_samples"`
}

type Plane struct {
	Name     string
	Width    int
	Height   int
	BitDepth int
	QuantMin uint16
	QuantMax uint16
	Data     []uint16
}

type Pixel struct {
	Y uint16 `json:"y"`
	U uint16 `json:"u"`
	V uint16 `json:"v"`
}

type Image struct {
	Width    int
	Height   int
	BitDepth int
	QuantMin uint16
	QuantMax uint16
	Pixels   []Pixel
}

type Sample struct {
	Metadata Metadata
	Y        *Plane
	U        *Plane
	V        *Plane
}

func (p *Plane) At(x, y int) uint16 {
	return p.Data[y*p.Width+x]
}

func (p *Plane) Set(x, y int, v uint16) {
	p.Data[y*p.Width+x] = v
}

func (p *Plane) Clone() *Plane {
	out := *p
	out.Data = append([]uint16(nil), p.Data...)
	return &out
}

// ContentHash covers geometry, depth and every sample, not the path or file name.
func (p *Plane) ContentHash() string {
	h := sha256.New()
	fmt.Fprintf(h, "plane:%s:%d:%d:%d:%d:%d\n", p.Name, p.Width, p.Height, p.BitDepth, p.QuantMin, p.QuantMax)
	buf := make([]byte, 2)
	for _, v := range p.Data {
		binary.BigEndian.PutUint16(buf, v)
		_, _ = h.Write(buf)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func (img *Image) At(x, y int) Pixel {
	return img.Pixels[y*img.Width+x]
}

func (img *Image) Digest() string {
	h := sha256.New()
	fmt.Fprintf(h, "image:%d:%d:%d:%d:%d\n", img.Width, img.Height, img.BitDepth, img.QuantMin, img.QuantMax)
	buf := make([]byte, 6)
	for _, p := range img.Pixels {
		binary.BigEndian.PutUint16(buf[0:2], p.Y)
		binary.BigEndian.PutUint16(buf[2:4], p.U)
		binary.BigEndian.PutUint16(buf[4:6], p.V)
		_, _ = h.Write(buf)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func FrameBytes(width, height int) int {
	if width <= 0 || height <= 0 {
		return 0
	}
	return width * height * 4
}
