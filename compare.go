package textureconv

import (
	"fmt"
)

// CompareSnapshots finds the first pixel or summary/sequence mismatch and
// returns E_DIFF with entry, tile, coordinate, and both sampled values.
func CompareSnapshots(a, b *RunSnapshot) error {
	if a == nil || b == nil {
		return &Error{Code: CodeDiff, Tile: -1, Field: "snapshot", Msg: "nil snapshot"}
	}
	if len(a.Stages) != len(b.Stages) {
		return &Error{Code: CodeDiff, Tile: -1, Field: "stages",
			Msg: fmt.Sprintf("stage count differs: %d vs %d", len(a.Stages), len(b.Stages))}
	}
	for i := range a.Stages {
		if err := compareStage(a.Stages[i], b.Stages[i], a.Tiles, a.Entries[i].Entry); err != nil {
			return err
		}
	}
	if len(a.Submissions) != len(b.Submissions) {
		return &Error{Code: CodeDiff, Tile: -1, Field: "submissions",
			Msg: fmt.Sprintf("submission count differs: %d vs %d", len(a.Submissions), len(b.Submissions))}
	}
	for i := range a.Submissions {
		x, y := a.Submissions[i], b.Submissions[i]
		if x.Tile != y.Tile || x.Entry != y.Entry || x.CacheKey != y.CacheKey || x.ContentHash != y.ContentHash {
			return &Error{Code: CodeDiff, Entry: x.Entry, Tile: x.Tile, Field: "submissions",
				Msg: fmt.Sprintf("submission %d differs: %+v vs %+v", i, x, y)}
		}
	}
	return nil
}

func compareStage(a, b *StageImage, tiles []Tile, entry int) error {
	if a.Width != b.Width || a.Height != b.Height || a.BitDepth != b.BitDepth ||
		a.Channels != b.Channels || a.AlphaIndex != b.AlphaIndex {
		return &Error{Code: CodeDiff, Entry: entry, Tile: -1, Field: "header",
			Msg: fmt.Sprintf("stage geometry/metadata differs: %dx%dx%d/%d/a%d vs %dx%dx%d/%d/a%d",
				a.Width, a.Height, a.BitDepth, a.Channels, a.AlphaIndex,
				b.Width, b.Height, b.BitDepth, b.Channels, b.AlphaIndex)}
	}
	if len(a.Dropped) != len(b.Dropped) {
		return &Error{Code: CodeDiff, Entry: entry, Tile: -1, Field: "dropped",
			Msg: "dropped-channel metadata differs"}
	}
	for i := range a.Dropped {
		if a.Dropped[i] != b.Dropped[i] {
			return &Error{Code: CodeDiff, Entry: entry, Tile: -1, Field: "dropped",
				Msg: fmt.Sprintf("dropped metadata %d differs: %+v vs %+v", i, a.Dropped[i], b.Dropped[i])}
		}
	}
	components := a.Width * a.Height * a.Channels
	for i := 0; i < components; i++ {
		if a.Pixels[i] == b.Pixels[i] {
			continue
		}
		pixel := i / a.Channels
		x, y := pixel%a.Width, pixel/a.Width
		field := fmt.Sprintf("channels[%d]", i%a.Channels)
		left := append([]uint16(nil), a.Pixel(x, y)...)
		right := append([]uint16(nil), b.Pixel(x, y)...)
		return &Error{
			Code: CodeDiff, Entry: entry, Tile: tileAt(tiles, x, y), Field: field,
			Pixel: Point{X: x, Y: y}, HasPixel: true,
			Msg:  fmt.Sprintf("first difference at pixel (%d,%d): incremental=%d full=%d", x, y, left[i%a.Channels], right[i%a.Channels]),
			Left: left, Right: right,
		}
	}
	return nil
}

func tileAt(tiles []Tile, x, y int) int {
	for _, t := range tiles {
		if x >= t.X && y >= t.Y && x < t.X+t.W && y < t.Y+t.H {
			return t.Index
		}
	}
	return -1
}
