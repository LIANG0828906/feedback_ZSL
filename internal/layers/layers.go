package layers

import (
	"encoding/binary"
	"fmt"
	"math"

	"iconseg/internal/core"
)

type Layer string

const (
	Gradient Layer = "gradient"
	Marker   Layer = "marker"
	Flood    Layer = "flood_merge"
	Stats    Layer = "stats"
)

var Order = []Layer{Gradient, Marker, Flood, Stats}

type Block struct {
	Index int
	X     int
	Y     int
	W     int
	H     int
}

func Split(width, height, blockSize int) []Block {
	blocks := make([]Block, 0)
	index := 0
	for y := 0; y < height; y += blockSize {
		h := min(blockSize, height-y)
		for x := 0; x < width; x += blockSize {
			w := min(blockSize, width-x)
			blocks = append(blocks, Block{Index: index, X: x, Y: y, W: w, H: h})
			index++
		}
	}
	return blocks
}

func EncodeGradient(values []float64) []byte {
	body := make([]byte, 2*len(values))
	for i, value := range values {
		scaled := int(math.Round(value * 32))
		if scaled < 0 {
			scaled = 0
		}
		if scaled > 65535 {
			scaled = 65535
		}
		binary.LittleEndian.PutUint16(body[i*2:], uint16(scaled))
	}
	return body
}

func EncodeInt32(values []int32) []byte {
	body := make([]byte, 4*len(values))
	for i, value := range values {
		binary.LittleEndian.PutUint32(body[i*4:], uint32(value))
	}
	return body
}

func DecodeInt32(body []byte) ([]int32, error) {
	if len(body)%4 != 0 {
		return nil, core.DecodeError("label layer length is not divisible by four")
	}
	out := make([]int32, len(body)/4)
	for i := range out {
		out[i] = int32(binary.LittleEndian.Uint32(body[i*4:]))
	}
	return out, nil
}

func CropFloat(values []float64, width int, block Block) []float64 {
	out := make([]float64, block.W*block.H)
	for y := 0; y < block.H; y++ {
		copy(out[y*block.W:(y+1)*block.W], values[(block.Y+y)*width+block.X:(block.Y+y)*width+block.X+block.W])
	}
	return out
}

func CropInt32(values []int32, width int, block Block) []int32 {
	out := make([]int32, block.W*block.H)
	for y := 0; y < block.H; y++ {
		copy(out[y*block.W:(y+1)*block.W], values[(block.Y+y)*width+block.X:(block.Y+y)*width+block.X+block.W])
	}
	return out
}

func CropMask(values []byte, width int, block Block) []byte {
	out := make([]byte, block.W*block.H)
	for y := 0; y < block.H; y++ {
		copy(out[y*block.W:(y+1)*block.W], values[(block.Y+y)*width+block.X:(block.Y+y)*width+block.X+block.W])
	}
	return out
}

func CopyIntoUint8(dst []byte, dstWidth int, block Block, src []byte) {
	for y := 0; y < block.H; y++ {
		copy(dst[(block.Y+y)*dstWidth+block.X:(block.Y+y)*dstWidth+block.X+block.W],
			src[y*block.W:(y+1)*block.W])
	}
}

func CopyIntoInt32(dst []int32, dstWidth int, block Block, src []int32) {
	for y := 0; y < block.H; y++ {
		copy(dst[(block.Y+y)*dstWidth+block.X:(block.Y+y)*dstWidth+block.X+block.W],
			src[y*block.W:(y+1)*block.W])
	}
}

func Summary(layer Layer, block Block) string {
	return fmt.Sprintf("%s block=%d x=%d y=%d w=%d h=%d", layer, block.Index, block.X, block.Y, block.W, block.H)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
