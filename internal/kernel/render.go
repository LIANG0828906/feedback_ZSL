package kernel

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

const AlgorithmVersion = "fixedpoint-bresenham-depth-v2"

type TileResult struct {
	TileID              string   `json:"tile_id"`
	TileIndex           int      `json:"tile_index"`
	ContentHash         string   `json:"content_hash"`
	AlgorithmVersion    string   `json:"algorithm_version"`
	ColorDigest         string   `json:"color_digest"`
	DepthDigest         string   `json:"depth_digest"`
	CoverageBp          int      `json:"coverage_bp"`
	ObstacleCoverageBp  int      `json:"obstacle_coverage_bp"`
	PolylineCoverageBp  int      `json:"polyline_coverage_bp"`
	PolylineShareBp     int      `json:"polyline_share_bp"`
	ObstaclePixelCount  int      `json:"obstacle_pixel_count"`
	PolylinePixelCount  int      `json:"polyline_pixel_count"`
	BounceCount         int      `json:"bounce_count"`
	FirstHitMarkerCount int      `json:"first_hit_marker_count"`
	SegmentIDs          []string `json:"segment_ids"`
	RequestIDs          []string `json:"request_ids"`
	Color               []byte   `json:"-"`
	Depth               []uint32 `json:"-"`
}

type FrameComparison struct {
	PixelEqual          bool   `json:"pixel_equal"`
	FirstMismatchPixel  Pixel  `json:"first_mismatch_pixel"`
	FirstMismatchTile   string `json:"first_mismatch_tile,omitempty"`
	MismatchedPixels    int    `json:"mismatched_pixels"`
	TileMetricsEqual    bool   `json:"tile_metrics_equal"`
	HashEqual           bool   `json:"hash_equal"`
	HitStatsEqual       bool   `json:"hit_stats_equal"`
	FirstMetricMismatch string `json:"first_metric_mismatch,omitempty"`
}

var (
	colorBackground = [4]byte{15, 23, 42, 255}
	colorObstacle   = [4]byte{245, 158, 11, 255}
	colorPolyline   = [4]byte{34, 211, 238, 255}
	colorBounce     = [4]byte{244, 63, 94, 255}
	colorFirstHit   = [4]byte{74, 222, 128, 255}
)

func RenderTile(mapping *Mapping, tileID string, failTileIDs map[string]bool) (*TileResult, error) {
	if failTileIDs != nil && failTileIDs[tileID] {
		return nil, blockError(tileID, "injected single tile internal failure")
	}
	tm := mapping.Tiles[tileID]
	if tm == nil {
		return nil, blockError(tileID, "tile not found in mapping")
	}
	camera := mapping.Camera
	width := camera.TileW
	height := camera.TileH
	pixelN := width * height
	color := make([]byte, pixelN*4)
	depth := make([]uint32, pixelN)
	for i := 0; i < pixelN; i++ {
		copy(color[i*4:i*4+4], colorBackground[:])
		depth[i] = ^uint32(0)
	}

	paint := func(mask []byte, rgba [4]byte, depthValue uint32) {
		for i := 0; i < pixelN; i++ {
			if mask[i/8]&(1<<uint(i%8)) == 0 {
				continue
			}
			if depthValue <= depth[i] {
				copy(color[i*4:i*4+4], rgba[:])
				depth[i] = depthValue
			}
		}
	}
	paint(tm.ObstacleMask, colorObstacle, 3000)
	paint(tm.PolylineMask, colorPolyline, 2000)
	paint(tm.BounceMask, colorBounce, 1000)
	for _, marker := range tm.BounceMarkers {
		if !marker.First {
			continue
		}
		localX := marker.Pixel.X - int64(tm.Tile.Coord.X*camera.TileW)
		localY := marker.Pixel.Y - int64(tm.Tile.Coord.Y*camera.TileH)
		if localX < 0 || localY < 0 || localX >= int64(width) || localY >= int64(height) {
			continue
		}
		index := int(localY*int64(width) + localX)
		copy(color[index*4:index*4+4], colorFirstHit[:])
		depth[index] = 500
	}

	colorSum := sha256.Sum256(color)
	depthBlob := make([]byte, pixelN*4)
	for i, value := range depth {
		binary.LittleEndian.PutUint32(depthBlob[i*4:i*4+4], value)
	}
	depthSum := sha256.Sum256(depthBlob)
	firstHits := 0
	for _, marker := range tm.BounceMarkers {
		if marker.First {
			firstHits++
		}
	}

	return &TileResult{
		TileID:              tileID,
		TileIndex:           tm.Tile.Index,
		ContentHash:         tm.ContentHash,
		AlgorithmVersion:    AlgorithmVersion,
		ColorDigest:         hex.EncodeToString(colorSum[:]),
		DepthDigest:         hex.EncodeToString(depthSum[:]),
		CoverageBp:          tm.CoverageBp,
		ObstacleCoverageBp:  tm.ObstacleCoverageBp,
		PolylineCoverageBp:  tm.PolylineCoverageBp,
		PolylineShareBp:     tm.PolylineShareBp,
		ObstaclePixelCount:  tm.ObstaclePixelCount,
		PolylinePixelCount:  tm.PolylinePixelCount,
		BounceCount:         len(tm.BounceMarkers),
		FirstHitMarkerCount: firstHits,
		SegmentIDs:          append([]string{}, tm.SegmentIDs...),
		RequestIDs:          append([]string{}, tm.RequestIDs...),
		Color:               color,
		Depth:               depth,
	}, nil
}

func AssembleFrame(mapping *Mapping, results map[string]*TileResult) ([]byte, error) {
	camera := mapping.Camera
	frame := make([]byte, camera.ViewportW*camera.ViewportH*4)
	for _, tileID := range mapping.TileOrder {
		result := results[tileID]
		if result == nil || result.Color == nil {
			return nil, fmt.Errorf("missing rendered tile %s", tileID)
		}
		tile := mapping.Tiles[tileID].Tile
		for y := 0; y < camera.TileH; y++ {
			srcStart := y * camera.TileW * 4
			srcEnd := srcStart + camera.TileW*4
			dstY := tile.Coord.Y*camera.TileH + y
			dstX := tile.Coord.X * camera.TileW
			dstStart := (dstY*camera.ViewportW + dstX) * 4
			copy(frame[dstStart:dstStart+camera.TileW*4], result.Color[srcStart:srcEnd])
		}
	}
	return frame, nil
}

func ComparePixels(a, b []byte, camera Camera) FrameComparison {
	comparison := FrameComparison{PixelEqual: true, TileMetricsEqual: true, HashEqual: true, HitStatsEqual: true}
	n := camera.ViewportW * camera.ViewportH * 4
	if len(a) != n || len(b) != n {
		comparison.PixelEqual = false
		comparison.FirstMismatchTile = "$.frame"
		comparison.FirstMetricMismatch = fmt.Sprintf("buffer length %d vs %d", len(a), len(b))
		return comparison
	}
	for i := 0; i < n; i++ {
		if a[i] == b[i] {
			continue
		}
		pixelIndex := i / 4
		comparison.PixelEqual = false
		comparison.MismatchedPixels++
		if comparison.MismatchedPixels == 1 {
			px := pixelIndex % camera.ViewportW
			py := pixelIndex / camera.ViewportW
			comparison.FirstMismatchPixel = Pixel{X: int64(px), Y: int64(py)}
			tileX := px / camera.TileW
			tileY := py / camera.TileH
			comparison.FirstMismatchTile = coordID(TileCoord{X: tileX, Y: tileY})
		}
	}
	return comparison
}

func CompareTileResults(full, incremental map[string]*TileResult, order []string) FrameComparison {
	cmp := FrameComparison{PixelEqual: true, TileMetricsEqual: true, HashEqual: true, HitStatsEqual: true}
	for _, id := range order {
		a := full[id]
		b := incremental[id]
		if a == nil || b == nil {
			cmp.TileMetricsEqual = false
			cmp.HashEqual = false
			cmp.HitStatsEqual = false
			cmp.PixelEqual = false
			cmp.FirstMismatchTile = id
			cmp.FirstMetricMismatch = "missing tile result"
			return cmp
		}
		if a.ContentHash != b.ContentHash {
			cmp.HashEqual = false
			cmp.FirstMismatchTile = id
			cmp.FirstMetricMismatch = fmt.Sprintf("content hash %s != %s", a.ContentHash, b.ContentHash)
		}
		if a.CoverageBp != b.CoverageBp || a.ObstacleCoverageBp != b.ObstacleCoverageBp ||
			a.PolylineCoverageBp != b.PolylineCoverageBp || a.PolylineShareBp != b.PolylineShareBp {
			cmp.TileMetricsEqual = false
			if cmp.FirstMetricMismatch == "" {
				cmp.FirstMismatchTile = id
				cmp.FirstMetricMismatch = "coverage metrics differ"
			}
		}
		if a.BounceCount != b.BounceCount || a.FirstHitMarkerCount != b.FirstHitMarkerCount {
			cmp.HitStatsEqual = false
			if cmp.FirstMetricMismatch == "" {
				cmp.FirstMismatchTile = id
				cmp.FirstMetricMismatch = "hit statistics differ"
			}
		}
		if a.ColorDigest != b.ColorDigest || a.DepthDigest != b.DepthDigest {
			cmp.PixelEqual = false
			if cmp.FirstMismatchTile == "" {
				cmp.FirstMismatchTile = id
			}
		}
	}
	return cmp
}
