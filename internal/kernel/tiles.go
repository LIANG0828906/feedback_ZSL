package kernel

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

type BounceMarker struct {
	RequestID string `json:"request_id"`
	Index     int    `json:"index"`
	SegmentID string `json:"segment_id"`
	Pixel     Pixel  `json:"pixel"`
	First     bool   `json:"first"`
}

type TileMapping struct {
	Tile               Tile           `json:"tile"`
	ContentHash        string         `json:"content_hash"`
	CoverageBp         int            `json:"coverage_bp"`
	ObstacleCoverageBp int            `json:"obstacle_coverage_bp"`
	PolylineCoverageBp int            `json:"polyline_coverage_bp"`
	PolylineShareBp    int            `json:"polyline_share_bp"`
	ObstaclePixelCount int            `json:"obstacle_pixel_count"`
	PolylinePixelCount int            `json:"polyline_pixel_count"`
	UnionPixelCount    int            `json:"union_pixel_count"`
	SegmentIDs         []string       `json:"segment_ids"`
	RequestIDs         []string       `json:"request_ids"`
	BounceMarkers      []BounceMarker `json:"bounce_markers"`
	ObstacleMask       []byte         `json:"-"`
	PolylineMask       []byte         `json:"-"`
	BounceMask         []byte         `json:"-"`
	UnionMask          []byte         `json:"-"`
}

type Mapping struct {
	Camera              Camera                  `json:"camera"`
	Tiles               map[string]*TileMapping `json:"tiles"`
	TileOrder           []string                `json:"tile_order"`
	TotalPolylinePixels int                     `json:"total_polyline_pixels"`
	SegmentTiles        map[string][]string     `json:"segment_tiles"`
	RequestTiles        map[string][]string     `json:"request_tiles"`
}

type maskRef struct {
	tile  *TileMapping
	index int
}

func BuildMapping(scene *Scene, report *PredictionReport, camera Camera) (*Mapping, error) {
	if err := PrepareScene(scene); err != nil {
		return nil, err
	}
	mapping := &Mapping{
		Camera:       camera,
		Tiles:        map[string]*TileMapping{},
		TileOrder:    []string{},
		SegmentTiles: map[string][]string{},
		RequestTiles: map[string][]string{},
	}
	pixelCount := camera.TileW * camera.TileH
	maskBytes := (pixelCount + 7) / 8
	for _, tile := range camera.Tiles() {
		tm := &TileMapping{
			Tile:         tile,
			ObstacleMask: make([]byte, maskBytes),
			PolylineMask: make([]byte, maskBytes),
			BounceMask:   make([]byte, maskBytes),
			UnionMask:    make([]byte, maskBytes),
		}
		mapping.Tiles[tile.ID] = tm
		mapping.TileOrder = append(mapping.TileOrder, tile.ID)
	}

	segmentTileSet := map[string]map[string]struct{}{}
	requestTileSet := map[string]map[string]struct{}{}

	addPixel := func(p Pixel, category string, ownerID string) *maskRef {
		if p.X < 0 || p.Y < 0 || p.X >= int64(camera.ViewportW) || p.Y >= int64(camera.ViewportH) {
			return nil
		}
		coord := camera.TileAt(p)
		tileID := fmt.Sprintf("tx%02d_ty%02d", coord.X, coord.Y)
		tm := mapping.Tiles[tileID]
		if tm == nil {
			return nil
		}
		localX := p.X - int64(coord.X*camera.TileW)
		localY := p.Y - int64(coord.Y*camera.TileH)
		index := int(localY*int64(camera.TileW) + localX)
		byteIndex := index / 8
		bit := byte(1 << uint(index%8))
		switch category {
		case "obstacle":
			tm.ObstacleMask[byteIndex] |= bit
			if ownerID != "" {
				if segmentTileSet[ownerID] == nil {
					segmentTileSet[ownerID] = map[string]struct{}{}
				}
				segmentTileSet[ownerID][tileID] = struct{}{}
			}
		case "polyline":
			tm.PolylineMask[byteIndex] |= bit
			if ownerID != "" {
				if requestTileSet[ownerID] == nil {
					requestTileSet[ownerID] = map[string]struct{}{}
				}
				requestTileSet[ownerID][tileID] = struct{}{}
			}
		case "bounce":
			tm.BounceMask[byteIndex] |= bit
		}
		tm.UnionMask[byteIndex] |= bit
		return &maskRef{tile: tm, index: index}
	}

	for _, seg := range scene.Segments {
		a, err := camera.Project(seg.A)
		if err != nil {
			return nil, err
		}
		b, err := camera.Project(seg.B)
		if err != nil {
			return nil, err
		}
		rasterLine(a, b, func(p Pixel) {
			addPixel(p, "obstacle", seg.ID)
		})
	}

	for _, traj := range report.Trajectories {
		requestID := traj.RequestID
		var previous *Pixel
		for _, sample := range traj.Samples {
			p, err := camera.Project(sample.Position)
			if err != nil {
				return nil, err
			}
			if previous != nil {
				rasterLine(*previous, p, func(point Pixel) {
					addPixel(point, "polyline", requestID)
				})
			}
			previous = &p
		}
		for _, bounce := range traj.Bounces {
			center, err := camera.Project(bounce.Position)
			if err != nil {
				return nil, err
			}
			marker := BounceMarker{
				RequestID: requestID,
				Index:     bounce.Index,
				SegmentID: bounce.SegmentID,
				Pixel:     center,
				First:     bounce.Index == 0,
			}
			if coord := camera.TileAt(center); true {
				tileID := coordID(coord)
				if tm := mapping.Tiles[tileID]; tm != nil {
					tm.BounceMarkers = append(tm.BounceMarkers, marker)
				}
			}
			for dy := int64(-3); dy <= 3; dy++ {
				for dx := int64(-3); dx <= 3; dx++ {
					if dx*dx+dy*dy <= 10 {
						addPixel(Pixel{X: center.X + dx, Y: center.Y + dy}, "bounce", requestID)
					}
				}
			}
		}
	}

	for id, set := range segmentTileSet {
		mapping.SegmentTiles[id] = sortedKeys(set)
	}
	for id, set := range requestTileSet {
		mapping.RequestTiles[id] = sortedKeys(set)
	}

	totalPolyline := 0
	for _, tileID := range mapping.TileOrder {
		tm := mapping.Tiles[tileID]
		tm.SegmentIDs = idsFromSet(segmentTileSet, tileID)
		tm.RequestIDs = idsFromSet(requestTileSet, tileID)
		sort.Slice(tm.BounceMarkers, func(i, j int) bool {
			if tm.BounceMarkers[i].RequestID != tm.BounceMarkers[j].RequestID {
				return tm.BounceMarkers[i].RequestID < tm.BounceMarkers[j].RequestID
			}
			return tm.BounceMarkers[i].Index < tm.BounceMarkers[j].Index
		})
		tm.ObstaclePixelCount = countBits(tm.ObstacleMask)
		tm.PolylinePixelCount = countBits(tm.PolylineMask)
		tm.UnionPixelCount = countBits(tm.UnionMask)
		tm.ObstacleCoverageBp = tm.ObstaclePixelCount * 10000 / pixelCount
		tm.PolylineCoverageBp = tm.PolylinePixelCount * 10000 / pixelCount
		tm.CoverageBp = tm.UnionPixelCount * 10000 / pixelCount
		totalPolyline += tm.PolylinePixelCount
	}
	mapping.TotalPolylinePixels = totalPolyline
	for _, tileID := range mapping.TileOrder {
		tm := mapping.Tiles[tileID]
		if totalPolyline > 0 {
			tm.PolylineShareBp = tm.PolylinePixelCount * 10000 / totalPolyline
		}
		tm.ContentHash = hashTileContent(tm, scene)
	}
	return mapping, nil
}

func coordID(coord TileCoord) string {
	return fmt.Sprintf("tx%02d_ty%02d", coord.X, coord.Y)
}

func rasterLine(a, b Pixel, emit func(Pixel)) {
	x0, y0 := a.X, a.Y
	x1, y1 := b.X, b.Y
	dx := absInt64(x1 - x0)
	dy := -absInt64(y1 - y0)
	sx := int64(1)
	if x0 > x1 {
		sx = -1
	}
	sy := int64(1)
	if y0 > y1 {
		sy = -1
	}
	err := dx + dy
	for {
		emit(Pixel{X: x0, Y: y0})
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

func RasterLine(a, b Pixel, emit func(Pixel)) {
	rasterLine(a, b, emit)
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func countBits(mask []byte) int {
	count := 0
	for _, b := range mask {
		for b != 0 {
			count += int(b & 1)
			b >>= 1
		}
	}
	return count
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func idsFromSet(byOwner map[string]map[string]struct{}, tileID string) []string {
	ids := []string{}
	for owner, tiles := range byOwner {
		if _, ok := tiles[tileID]; ok {
			ids = append(ids, owner)
		}
	}
	sort.Strings(ids)
	return ids
}

func hashTileContent(tm *TileMapping, scene *Scene) string {
	segmentByID := map[string]Segment{}
	for _, seg := range scene.Segments {
		segmentByID[seg.ID] = seg
	}
	h := sha256.New()
	write := func(parts ...any) {
		for _, part := range parts {
			switch value := part.(type) {
			case string:
				h.Write([]byte(value))
			case []byte:
				h.Write(value)
			case int:
				var buf [8]byte
				binary.LittleEndian.PutUint64(buf[:], uint64(value))
				h.Write(buf[:])
			case int64:
				var buf [8]byte
				binary.LittleEndian.PutUint64(buf[:], uint64(value))
				h.Write(buf[:])
			case bool:
				if value {
					h.Write([]byte{1})
				} else {
					h.Write([]byte{0})
				}
			}
			h.Write([]byte{0})
		}
		h.Write([]byte{0x1f})
	}

	write("geometry-content-v1")
	for _, id := range tm.SegmentIDs {
		seg := segmentByID[id]
		write("segment", id, seg.MaterialID, seg.MaterialVersion, seg.Restitution,
			seg.NormalSide, seg.Penetrable)
	}
	write("obstacle-mask", tm.ObstacleMask)
	write("polyline-requests", strings.Join(tm.RequestIDs, ","))
	write("polyline-mask", tm.PolylineMask)
	for _, marker := range tm.BounceMarkers {
		write("bounce", marker.RequestID, marker.Index, marker.SegmentID,
			marker.Pixel.X, marker.Pixel.Y, marker.First)
	}
	write("bounce-mask", tm.BounceMask)
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:])
}

func (m *Mapping) TileHash(tileID string) string {
	if tm := m.Tiles[tileID]; tm != nil {
		return tm.ContentHash
	}
	return ""
}
