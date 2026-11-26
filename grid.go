package strokelab

import "sort"

type Block struct {
	ID   int  `json:"id"`
	Row  int  `json:"row"`
	Col  int  `json:"col"`
	Rect Rect `json:"rect"`
}

func BuildBlocks(width, height, blockSize int) []Block {
	if blockSize <= 0 {
		blockSize = DefaultBlockSize
	}
	var out []Block
	id := 0
	for y := 0; y < height; y += blockSize {
		row := y / blockSize
		for x := 0; x < width; x += blockSize {
			col := x / blockSize
			w := min(blockSize, width-x)
			h := min(blockSize, height-y)
			out = append(out, Block{ID: id, Row: row, Col: col,
				Rect: Rect{X: x, Y: y, W: w, H: h}})
			id++
		}
	}
	return out
}

func blockByID(blocks []Block) map[int]Block {
	out := make(map[int]Block, len(blocks))
	for _, b := range blocks {
		out[b.ID] = b
	}
	return out
}

func blocksForRect(blocks []Block, rect Rect) []int {
	var out []int
	for _, b := range blocks {
		if b.Rect.Intersects(rect) {
			out = append(out, b.ID)
		}
	}
	return out
}

func participantsForBlock(ordered []OrderedStroke, block Rect) []OrderedStroke {
	var out []OrderedStroke
	for _, os := range ordered {
		if os.Stroke.Bounds().Intersects(block) {
			out = append(out, os)
		}
	}
	return out
}

func sortedBlockIDs(set map[int]string) []int {
	out := make([]int, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}
