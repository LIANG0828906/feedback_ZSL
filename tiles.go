package textureconv

import (
	"fmt"
)

// Tile is a flattened row-major image tile.
type Tile struct {
	Index int `json:"index"`
	X     int `json:"x"`
	Y     int `json:"y"`
	W     int `json:"w"`
	H     int `json:"h"`
}

func (t Tile) Rect() Rect { return Rect{X: t.X, Y: t.Y, W: t.W, H: t.H} }

func MakeTiles(width, height, tileSize int) ([]Tile, error) {
	if width <= 0 || height <= 0 {
		return nil, paramErr(0, "geometry", "image must have positive dimensions")
	}
	if tileSize <= 0 {
		return nil, paramErr(0, "tileSize", "must be positive")
	}
	if tileSize > 8192 {
		return nil, paramErr(0, "tileSize", "must not exceed 8192")
	}
	var tiles []Tile
	idx := 0
	for y := 0; y < height; y += tileSize {
		for x := 0; x < width; x += tileSize {
			tiles = append(tiles, Tile{
				Index: idx, X: x, Y: y,
				W: min(tileSize, width-x), H: min(tileSize, height-y),
			})
			idx++
		}
	}
	return tiles, nil
}

func TilesForRect(tiles []Tile, r Rect) []int {
	if r.Empty() {
		return nil
	}
	var out []int
	for _, t := range tiles {
		if r.Intersects(t.Rect()) {
			out = append(out, t.Index)
		}
	}
	return out
}

func TileByIndex(tiles []Tile, idx int) (Tile, error) {
	if idx < 0 || idx >= len(tiles) {
		return Tile{}, paramErr(0, "tile", "tile index %d is outside [0,%d)", idx, len(tiles))
	}
	return tiles[idx], nil
}

func extractSourceTile(im *Image, t Tile) *StageImage {
	pix := make([]uint16, t.W*t.H*4)
	for y := 0; y < t.H; y++ {
		src := ((t.Y+y)*im.Width + t.X) * 4
		dst := y * t.W * 4
		copy(pix[dst:dst+t.W*4], im.Pixels[src:src+t.W*4])
	}
	return &StageImage{
		Width: t.W, Height: t.H, BitDepth: im.BitDepth, Channels: 4,
		AlphaIndex: 3, Pixels: pix,
	}
}

func extractStageTile(im *StageImage, t Tile) *StageImage {
	pix := make([]uint16, t.W*t.H*im.Channels)
	for y := 0; y < t.H; y++ {
		src := ((t.Y+y)*im.Width + t.X) * im.Channels
		dst := y * t.W * im.Channels
		copy(pix[dst:dst+t.W*im.Channels], im.Pixels[src:src+t.W*im.Channels])
	}
	return &StageImage{
		Width: t.W, Height: t.H, BitDepth: im.BitDepth, Channels: im.Channels,
		AlphaIndex: im.AlphaIndex, Pixels: pix, Dropped: append([]DroppedChannel(nil), im.Dropped...),
	}
}

func placeStageTile(dst *StageImage, t Tile, src *StageImage) error {
	if src.Width != t.W || src.Height != t.H || src.Channels != dst.Channels {
		return fmt.Errorf("tile shape mismatch: tile=%dx%dx%d stage=%dx%dx%d", t.W, t.H, src.Channels, dst.Width, dst.Height, dst.Channels)
	}
	for y := 0; y < t.H; y++ {
		d := ((t.Y+y)*dst.Width + t.X) * dst.Channels
		s := y * t.W * src.Channels
		copy(dst.Pixels[d:d+t.W*dst.Channels], src.Pixels[s:s+t.W*src.Channels])
	}
	return nil
}

func emptyStageLike(im *Image, depth BitDepth, channels, alpha int) *StageImage {
	return &StageImage{
		Width: im.Width, Height: im.Height, BitDepth: depth,
		Channels: channels, AlphaIndex: alpha,
		Pixels: make([]uint16, im.Width*im.Height*channels),
	}
}
