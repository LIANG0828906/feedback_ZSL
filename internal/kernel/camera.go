package kernel

import "fmt"

type Camera struct {
	ID         string `json:"id"`
	Version    int64  `json:"version"`
	FocalXPX   int64  `json:"focal_x_px"`
	FocalYPX   int64  `json:"focal_y_px"`
	PrincipalX int64  `json:"principal_x"`
	PrincipalY int64  `json:"principal_y"`
	Position   Vec2   `json:"position"`
	ViewportW  int    `json:"viewport_w"`
	ViewportH  int    `json:"viewport_h"`
	TileW      int    `json:"tile_w"`
	TileH      int    `json:"tile_h"`
}

type Pixel struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type TileCoord struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Tile struct {
	Coord TileCoord `json:"coord"`
	ID    string    `json:"id"`
	Index int       `json:"index"`
}

func DefaultCamera() Camera {
	return Camera{
		ID:        "ortho-720p-diagnostic",
		Version:   1,
		FocalXPX:  768,
		FocalYPX:  768,
		ViewportW: 960,
		ViewportH: 576,
		TileW:     64,
		TileH:     64,
	}
}

func (c Camera) Project(p Vec2) (Pixel, error) {
	relativeX, ok := Sub64(p.X, c.Position.X)
	if !ok {
		return Pixel{}, overflowError("$.camera.position.x", c.ID, 0, p.X, c.Position.X)
	}
	relativeY, ok := Sub64(p.Y, c.Position.Y)
	if !ok {
		return Pixel{}, overflowError("$.camera.position.y", c.ID, 0, p.Y, c.Position.Y)
	}
	x, _, err := MulDiv(relativeX, c.FocalXPX, PosScale*PosScale, RoundingNearest)
	if err != nil {
		return Pixel{}, err
	}
	y, _, err := MulDiv(relativeY, c.FocalYPX, PosScale*PosScale, RoundingNearest)
	if err != nil {
		return Pixel{}, err
	}
	return Pixel{X: x + c.PrincipalX, Y: y + c.PrincipalY}, nil
}

func floorDiv(v, size int64) int64 {
	q := v / size
	r := v % size
	if r != 0 && ((r < 0) != (size < 0)) {
		q--
	}
	return q
}

func (c Camera) TileAt(p Pixel) TileCoord {
	return TileCoord{X: int(floorDiv(p.X, int64(c.TileW))), Y: int(floorDiv(p.Y, int64(c.TileH)))}
}

func (c Camera) TileColumns() int {
	return (c.ViewportW + c.TileW - 1) / c.TileW
}

func (c Camera) TileRows() int {
	return (c.ViewportH + c.TileH - 1) / c.TileH
}

func (c Camera) Tiles() []Tile {
	cols := c.TileColumns()
	rows := c.TileRows()
	tiles := make([]Tile, 0, cols*rows)
	index := 0
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			tiles = append(tiles, Tile{
				Coord: TileCoord{X: x, Y: y},
				ID:    fmt.Sprintf("tx%02d_ty%02d", x, y),
				Index: index,
			})
			index++
		}
	}
	return tiles
}

func (c Camera) CameraVersionKey() string {
	return fmt.Sprintf("%s-v%d-f%d,%d-p%d,%d-pos%d,%d-vp%dx%d-tile%dx%d",
		c.ID, c.Version, c.FocalXPX, c.FocalYPX, c.PrincipalX, c.PrincipalY,
		c.Position.X, c.Position.Y, c.ViewportW, c.ViewportH, c.TileW, c.TileH)
}
