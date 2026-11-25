package fog

import "fmt"

type TileCoord struct {
	TX int `json:"tx"`
	TY int `json:"ty"`
}

type Tile struct {
	ID     int       `json:"id"`
	Coord  TileCoord `json:"coord"`
	X0     int       `json:"x0"`
	Y0     int       `json:"y0"`
	Width  int       `json:"width"`
	Height int       `json:"height"`
}

type Viewport struct {
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	TileSize int    `json:"tile_size"`
	TilesX   int    `json:"tiles_x"`
	TilesY   int    `json:"tiles_y"`
	Tiles    []Tile `json:"tiles"`
}

func NewViewport(width, height, tileSize int) Viewport {
	tilesX := (width + tileSize - 1) / tileSize
	tilesY := (height + tileSize - 1) / tileSize
	tiles := make([]Tile, 0, tilesX*tilesY)
	for ty := 0; ty < tilesY; ty++ {
		for tx := 0; tx < tilesX; tx++ {
			x0 := tx * tileSize
			y0 := ty * tileSize
			w := minInt(tileSize, width-x0)
			h := minInt(tileSize, height-y0)
			id := ty*tilesX + tx
			tiles = append(tiles, Tile{
				ID: id, Coord: TileCoord{TX: tx, TY: ty},
				X0: x0, Y0: y0, Width: w, Height: h,
			})
		}
	}
	return Viewport{Width: width, Height: height, TileSize: tileSize, TilesX: tilesX, TilesY: tilesY, Tiles: tiles}
}

// PixelTile returns the tile based on integer pixel ownership. Applying the
// +0.5 sample offset before this lookup would be incorrect at boundaries.
func (v Viewport) PixelTile(x, y int) int {
	return (y/v.TileSize)*v.TilesX + x/v.TileSize
}

func (v Viewport) Tile(id int) (Tile, error) {
	if id < 0 || id >= len(v.Tiles) {
		return Tile{}, &Error{Code: EBlock, BlockID: blockIDPtr(id), Field: "block_id", Message: "block id outside viewport"}
	}
	return v.Tiles[id], nil
}

func (t Tile) PixelIndex(x, y int) int {
	return (y-t.Y0)*t.Width + (x - t.X0)
}

func (t Tile) Contains(x, y int) bool {
	return x >= t.X0 && x < t.X0+t.Width && y >= t.Y0 && y < t.Y0+t.Height
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func tileLabel(id int) string { return fmt.Sprintf("block-%03d", id) }
