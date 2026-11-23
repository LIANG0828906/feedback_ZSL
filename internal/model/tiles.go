package model

type Stage string

const (
	StageGrowth  Stage = "growth"
	StageCleanup Stage = "cleanup"
	StageStats   Stage = "stats"
)

var Stages = []Stage{StageGrowth, StageCleanup, StageStats}

func (s Stage) Valid() bool {
	switch s {
	case StageGrowth, StageCleanup, StageStats:
		return true
	default:
		return false
	}
}

type State string

const (
	StatePending    State = "pending"
	StateInProgress State = "in_progress"
	StateCommitted  State = "committed"
	StateFailed     State = "failed"
)

func (s State) Valid() bool {
	switch s {
	case StatePending, StateInProgress, StateCommitted, StateFailed:
		return true
	default:
		return false
	}
}

func LegalTransition(from, to State) bool {
	switch from {
	case StatePending:
		return to == StateInProgress
	case StateInProgress:
		return to == StateCommitted || to == StateFailed
	case StateFailed:
		return to == StateInProgress
	default:
		return false
	}
}

type Tile struct {
	Index  int
	X      int
	Y      int
	Width  int
	Height int
}

func TileLayout(width, height, tileSize int) []Tile {
	if width <= 0 || height <= 0 || tileSize <= 0 {
		return nil
	}
	tilesX := (width + tileSize - 1) / tileSize
	tilesY := (height + tileSize - 1) / tileSize
	out := make([]Tile, 0, tilesX*tilesY)
	idx := 0
	for ty := 0; ty < tilesY; ty++ {
		for tx := 0; tx < tilesX; tx++ {
			x, y := tx*tileSize, ty*tileSize
			w := tileSize
			if x+w > width {
				w = width - x
			}
			h := tileSize
			if y+h > height {
				h = height - y
			}
			out = append(out, Tile{Index: idx, X: x, Y: y, Width: w, Height: h})
			idx++
		}
	}
	return out
}

func CropLabels(labels []byte, width, height, x, y, w, h int) []byte {
	out := make([]byte, w*h)
	for row := 0; row < h; row++ {
		src := (y+row)*width + x
		dst := row * w
		copy(out[dst:dst+w], labels[src:src+w])
	}
	return out
}

func PasteLabels(dst []byte, dstW, dstH int, tile Tile, src []byte) error {
	if len(src) != tile.Width*tile.Height {
		return errInvalidTile
	}
	for row := 0; row < tile.Height; row++ {
		copy(dst[(tile.Y+row)*dstW+tile.X:], src[row*tile.Width:(row+1)*tile.Width])
	}
	return nil
}

type simpleError string

func (e simpleError) Error() string { return string(e) }

const errInvalidTile simpleError = "invalid tile buffer"
