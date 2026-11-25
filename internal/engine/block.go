package engine

import (
	"fmt"
	"sort"
)

type Block struct {
	ID     int `json:"id"`
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func SplitBlocks(width, height, blockSize int) ([]Block, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("invalid image dimensions %dx%d", width, height)
	}
	if blockSize <= 0 {
		return nil, fmt.Errorf("invalid block size %d", blockSize)
	}
	var blocks []Block
	id := 1
	for y := 0; y < height; y += blockSize {
		for x := 0; x < width; x += blockSize {
			w := blockSize
			if x+w > width {
				w = width - x
			}
			h := blockSize
			if y+h > height {
				h = height - y
			}
			blocks = append(blocks, Block{ID: id, X: x, Y: y, Width: w, Height: h})
			id++
		}
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].ID < blocks[j].ID })
	return blocks, nil
}

func cropMask(mask []uint8, imageWidth int, block Block) []uint8 {
	out := make([]uint8, block.Width*block.Height)
	for y := 0; y < block.Height; y++ {
		copy(out[y*block.Width:(y+1)*block.Width], mask[(block.Y+y)*imageWidth+block.X:(block.Y+y)*imageWidth+block.X+block.Width])
	}
	return out
}

func placeMask(dst []uint8, imageWidth int, block Block, mask []uint8) error {
	if len(mask) != block.Width*block.Height {
		return fmt.Errorf("block %d mask length %d, want %d", block.ID, len(mask), block.Width*block.Height)
	}
	for y := 0; y < block.Height; y++ {
		copy(dst[(block.Y+y)*imageWidth+block.X:(block.Y+y)*imageWidth+block.X+block.Width], mask[y*block.Width:(y+1)*block.Width])
	}
	return nil
}
