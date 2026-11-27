package grid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

type TileCacheOutcome struct {
	Tile        TileID `json:"tile"`
	Seq         int    `json:"seq"`
	Reused      bool   `json:"reused"`
	Reason      string `json:"reason"`
	ContentHash string `json:"contentHash"`
	CacheKey    string `json:"cacheKey"`
}

type CacheRunReport struct {
	Outcomes      []TileCacheOutcome `json:"outcomes"`
	Reused        int                `json:"reused"`
	Recomputed    int                `json:"recomputed"`
	AssembledHash string             `json:"assembledHash"`
}

// RunWithCache reuses recoverable committed tiles and commits all misses.
// Fresh projection always provides the comparison oracle; reused buffers are
// checked pixel-for-pixel before they are accepted.
func RunWithCache(store *SnapshotStore, scene *SceneTiles, cam Camera, versions SemanticVersions, forced map[string]bool) (*CacheRunReport, error) {
	report := &CacheRunReport{}
	for _, fresh := range scene.Tiles {
		key := CacheKeyForTile(fresh.ContentHash, versions, cam.CameraVersion)
		outcome := TileCacheOutcome{Tile: fresh.ID, Seq: fresh.Seq, ContentHash: fresh.ContentHash, CacheKey: key}
		idKey := fresh.ID.String()

		var cached *CachedTile
		if store != nil {
			c, err := store.LoadTile(fresh.ID.Col, fresh.ID.Row)
			if err != nil {
				return report, err
			}
			cached = c
		}

		if forced[idKey] {
			outcome.Reason = "forced-invalidation"
		} else if cached != nil {
			ok, mismatchField := cached.Recoverable(fresh, versions, cam.CameraVersion)
			if ok {
				if err := cached.PixelEqual(fresh); err != nil {
					return report, err
				}
				fresh.Color = append([]byte(nil), cached.Color...)
				fresh.Depth = append([]uint32(nil), cached.Depth...)
				outcome.Reused = true
				outcome.Reason = "cache-hit-pixel-identical"
				report.Reused++
			} else {
				outcome.Reason = "stale:" + mismatchField
			}
		} else {
			outcome.Reason = "snapshot-miss"
		}

		if !outcome.Reused {
			if store != nil {
				if err := store.CommitTile(fresh, cam, versions, scene.HitStats); err != nil {
					return report, err
				}
			}
			report.Recomputed++
		}
		report.Outcomes = append(report.Outcomes, outcome)
	}
	report.AssembledHash = AssembledColorHash(scene)
	return report, nil
}

func CommitScene(store *SnapshotStore, scene *SceneTiles, cam Camera, versions SemanticVersions) error {
	for _, t := range scene.Tiles {
		if err := store.CommitTile(t, cam, versions, scene.HitStats); err != nil {
			return err
		}
	}
	return nil
}

func AssembledColorHash(scene *SceneTiles) string {
	h := sha256.New()
	tiles := append([]*Tile(nil), scene.Tiles...)
	sort.Slice(tiles, func(i, j int) bool { return tiles[i].Seq < tiles[j].Seq })
	for _, t := range tiles {
		h.Write(t.Color)
		h.Write(DepthBytes(t.Depth))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CompareScenes compares the acceptance-required state in stable tile order.
func CompareScenes(a *SceneTiles, b *SceneTiles) *Error {
	if len(a.Tiles) != len(b.Tiles) {
		return &Error{Code: CodeDiff, Field: "tiles.length", Message: fmt.Sprintf("%d != %d", len(a.Tiles), len(b.Tiles))}
	}
	byID := map[string]*Tile{}
	for _, t := range b.Tiles {
		byID[t.ID.String()] = t
	}
	for _, at := range a.Tiles {
		bt := byID[at.ID.String()]
		if bt == nil {
			return &Error{Code: CodeDiff, Tile: &at.ID, Field: "tile.missing", Message: "tile missing in second path"}
		}
		if at.ContentHash != bt.ContentHash {
			return diffError(at.ID, "contentHash", at.ContentHash, bt.ContentHash)
		}
		if at.CoveragePermille != bt.CoveragePermille {
			return diffError(at.ID, "coveragePermille", at.CoveragePermille, bt.CoveragePermille)
		}
		if at.PolyCoverage != bt.PolyCoverage {
			return diffError(at.ID, "polyCoveragePixels", at.PolyCoverage, bt.PolyCoverage)
		}
		if at.CoveragePixels != bt.CoveragePixels {
			return diffError(at.ID, "coveragePixels", at.CoveragePixels, bt.CoveragePixels)
		}
		if at.ColorDigest != bt.ColorDigest {
			return diffError(at.ID, "colorDigest", at.ColorDigest, bt.ColorDigest)
		}
		if at.DepthDigest != bt.DepthDigest {
			return diffError(at.ID, "depthDigest", at.DepthDigest, bt.DepthDigest)
		}
		if err := compareBytes(at.ID, "colorBuffer", at.Color, bt.Color); err != nil {
			return err
		}
		if err := compareBytes(at.ID, "depthBuffer", DepthBytes(at.Depth), DepthBytes(bt.Depth)); err != nil {
			return err
		}
	}
	if !equalIntMaps(a.HitStats, b.HitStats) {
		return &Error{Code: CodeDiff, Field: "hitStats", OldValue: fmt.Sprint(a.HitStats), Message: fmt.Sprintf("hit stats differ: %v != %v", a.HitStats, b.HitStats)}
	}
	if len(a.RequestErrors) != len(b.RequestErrors) {
		return &Error{Code: CodeDiff, Field: "requestErrors", Message: "request error counts differ"}
	}
	return nil
}

func compareBytes(id TileID, field string, a, b []byte) *Error {
	if len(a) != len(b) {
		return diffError(id, field+".length", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			return diffError(id, fmt.Sprintf("%s[%d]", field, i), a[i], b[i])
		}
	}
	return nil
}

func equalIntMaps(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
