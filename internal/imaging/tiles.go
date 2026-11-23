package imaging

import (
	"ocrworkbench/internal/domain"
)

func BuildTiles(width, height, tileSize, band int) ([]domain.Tile, *domain.AppError) {
	if tileSize <= 0 {
		return nil, domain.ParaError("tile_size", ">0", tileSize)
	}
	if band <= 0 {
		return nil, domain.ParaError("band_width", ">0", band)
	}
	if band >= tileSize/2 {
		return nil, domain.ParaError("band_width", "< tile_size/2", band)
	}
	if width <= 0 || height <= 0 {
		return nil, domain.ParaError("image_size", ">0x0", "0")
	}
	stride := tileSize - 2*band
	if stride <= 0 {
		return nil, domain.ParaError("band_width", "tile_size - 2*band > 0", band)
	}
	startsX := starts(width, tileSize, stride)
	startsY := starts(height, tileSize, stride)
	tiles := make([]domain.Tile, 0, len(startsX)*len(startsY))
	index := 0
	for row, y := range startsY {
		for col, x := range startsX {
			x2 := minInt(width, x+tileSize)
			y2 := minInt(height, y+tileSize)
			coreX := x + band
			if x == 0 {
				coreX = 0
			}
			coreY := y + band
			if y == 0 {
				coreY = 0
			}
			coreX2 := minInt(width, x+tileSize-band)
			coreY2 := minInt(height, y+tileSize-band)
			if coreX2 <= coreX || coreY2 <= coreY {
				continue
			}
			tiles = append(tiles, domain.Tile{
				Index:     index,
				Row:       row,
				Col:       col,
				X:         x,
				Y:         y,
				W:         x2 - x,
				H:         y2 - y,
				CoreX:     coreX,
				CoreY:     coreY,
				CoreW:     coreX2 - coreX,
				CoreH:     coreY2 - coreY,
				BandWidth: band,
			})
			index++
		}
	}
	return tiles, nil
}

func starts(length, tileSize, stride int) []int {
	if length <= tileSize {
		return []int{0}
	}
	result := make([]int, 0, length/stride+1)
	for x := 0; x < length; x += stride {
		result = append(result, x)
		if x+tileSize >= length {
			break
		}
	}
	return result
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
