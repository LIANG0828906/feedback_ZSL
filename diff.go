package maskqa

import "fmt"

// DiffLocation identifies the first chunk/pixel/field mismatch.
type DiffLocation struct {
	TileX  int    `json:"tile_x"`
	TileY  int    `json:"tile_y"`
	PixelX int    `json:"pixel_x,omitempty"`
	PixelY int    `json:"pixel_y,omitempty"`
	Field  string `json:"field"`
}

// CompareResults checks pixels, topology, directions and commit sequence.
func CompareResults(expected, actual *ComputeResult) error {
	if expected == nil || actual == nil {
		return diffError(DiffLocation{Field: "result"}, expected, actual, "nil result")
	}
	if expected.Width != actual.Width || expected.Height != actual.Height {
		return diffError(DiffLocation{Field: "dimensions"},
			[2]int{expected.Width, expected.Height}, [2]int{actual.Width, actual.Height},
			"result dimensions differ")
	}
	if stringsJoin(expected.OrderedStrokes) != stringsJoin(actual.OrderedStrokes) {
		return diffError(DiffLocation{Field: "ordered_strokes"},
			expected.OrderedStrokes, actual.OrderedStrokes, "composition order differs")
	}

	actualTiles := map[TileCoord]TileResult{}
	for _, tile := range actual.Tiles {
		actualTiles[tile.Coord] = tile
	}
	for _, expectedTile := range expected.Tiles {
		actualTile, ok := actualTiles[expectedTile.Coord]
		loc := DiffLocation{TileX: expectedTile.Coord.TX, TileY: expectedTile.Coord.TY}
		if !ok {
			loc.Field = "tile"
			return diffError(loc, expectedTile.Coord, nil, "tile missing in actual result")
		}
		if len(expectedTile.Pixels) != len(actualTile.Pixels) {
			loc.Field = "accumulated_alpha.length"
			return diffError(loc, len(expectedTile.Pixels), len(actualTile.Pixels), "tile pixel length differs")
		}
		tileWidth := 16
		if expected.TileSize > 0 {
			tileWidth = expected.TileSize
		}
		for i := range expectedTile.Pixels {
			if expectedTile.Pixels[i] == actualTile.Pixels[i] {
				continue
			}
			localX := i % tileWidth
			localY := i / tileWidth
			loc.Field = "accumulated_alpha"
			loc.PixelX = localX
			loc.PixelY = localY
			return diffError(loc, expectedTile.Pixels[i], actualTile.Pixels[i],
				"first tile pixel mismatch at tile (%d,%d) local (%d,%d)",
				expectedTile.Coord.TX, expectedTile.Coord.TY, localX, localY)
		}
		if expectedTile.CacheKey != actualTile.CacheKey {
			loc.Field = "cache_key"
			return diffError(loc, expectedTile.CacheKey, actualTile.CacheKey, "tile cache key differs")
		}
	}

	if len(expected.CommitSequence) != len(actual.CommitSequence) {
		return diffError(DiffLocation{Field: "commit_sequence.length"},
			len(expected.CommitSequence), len(actual.CommitSequence), "commit sequence length differs")
	}
	for i := range expected.CommitSequence {
		e := expected.CommitSequence[i]
		a := actual.CommitSequence[i]
		loc := DiffLocation{TileX: e.Tile.TX, TileY: e.Tile.TY, Field: fmt.Sprintf("commit_sequence[%d]", i)}
		if e.Tile != a.Tile {
			loc.Field = fmt.Sprintf("commit_sequence[%d].tile", i)
			return diffError(loc, e.Tile, a.Tile, "commit tile order differs")
		}
		if e.CacheKey != a.CacheKey {
			loc.Field = fmt.Sprintf("commit_sequence[%d].cache_key", i)
			return diffError(loc, e.CacheKey, a.CacheKey, "commit cache key differs")
		}
		if e.ContentHash != a.ContentHash {
			loc.Field = fmt.Sprintf("commit_sequence[%d].content_hash", i)
			return diffError(loc, e.ContentHash, a.ContentHash, "commit content hash differs")
		}
	}
	return compareReports(expected.Report, actual.Report)
}

func compareReports(expected, actual *VerificationReport) error {
	if expected == nil || actual == nil {
		return diffError(DiffLocation{Field: "report"}, expected != nil, actual != nil, "verification report missing")
	}
	if len(expected.Domains) != len(actual.Domains) {
		return diffError(DiffLocation{Field: "domains.length"},
			len(expected.Domains), len(actual.Domains), "domain count differs")
	}
	for i := range expected.Domains {
		e := expected.Domains[i].Component
		a := actual.Domains[i].Component
		loc := DiffLocation{
			TileX: e.MinX / DefaultTileSize, TileY: e.MinY / DefaultTileSize,
			Field: fmt.Sprintf("domains[%d].area", i),
		}
		if e.Area != a.Area {
			return diffError(loc, e.Area, a.Area, "domain area differs")
		}
		loc.Field = fmt.Sprintf("domains[%d].centroid_x_milli", i)
		if e.CentroidXMilli != a.CentroidXMilli {
			return diffError(loc, e.CentroidXMilli, a.CentroidXMilli, "centroid x differs")
		}
		loc.Field = fmt.Sprintf("domains[%d].centroid_y_milli", i)
		if e.CentroidYMilli != a.CentroidYMilli {
			return diffError(loc, e.CentroidYMilli, a.CentroidYMilli, "centroid y differs")
		}
		loc.Field = fmt.Sprintf("domains[%d].holes", i)
		if e.Holes != a.Holes {
			return diffError(loc, e.Holes, a.Holes, "hole count differs")
		}
		loc.Field = fmt.Sprintf("domains[%d].band_width_milli", i)
		if e.BandWidthMilli != a.BandWidthMilli {
			return diffError(loc, e.BandWidthMilli, a.BandWidthMilli, "band width differs")
		}
		if (expected.Domains[i].Direction == nil) != (actual.Domains[i].Direction == nil) {
			loc.Field = fmt.Sprintf("domains[%d].direction", i)
			return diffError(loc, expected.Domains[i].Direction != nil, actual.Domains[i].Direction != nil,
				"direction report presence differs")
		}
		if expected.Domains[i].Direction != nil {
			loc.Field = fmt.Sprintf("domains[%d].direction.observed", i)
			if expected.Domains[i].Direction.Observed != actual.Domains[i].Direction.Observed {
				return diffError(loc, expected.Domains[i].Direction.Observed,
					actual.Domains[i].Direction.Observed, "direction category differs")
			}
		}
	}
	return nil
}

func diffError(location DiffLocation, expected, actual any, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	field := location.Field
	if location.TileX != 0 || location.TileY != 0 || location.Field == "accumulated_alpha" || location.Field == "cache_key" {
		if location.Field == "accumulated_alpha" {
			field = fmt.Sprintf("tiles[%d,%d].%s[pixel=%d,%d]",
				location.TileX, location.TileY, location.Field, location.PixelX, location.PixelY)
		} else if location.TileX != 0 || location.TileY != 0 {
			field = fmt.Sprintf("tiles[%d,%d].%s", location.TileX, location.TileY, location.Field)
		}
	}
	return &Error{
		Code: CodeDiff, Field: field, Message: message,
		Expected: expected, Actual: actual, Location: &location,
	}
}

func stringsJoin(values []string) string {
	out := ""
	for i, value := range values {
		if i > 0 {
			out += "|"
		}
		out += value
	}
	return out
}
