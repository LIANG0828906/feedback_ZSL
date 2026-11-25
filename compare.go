package iconcut

import (
	"context"
	"fmt"
	"reflect"
)

type Difference struct {
	TileIndex int    `json:"tile_index"`
	TX        int    `json:"tx"`
	TY        int    `json:"ty"`
	Stage     string `json:"stage"`
	Field     string `json:"field"`
	X         int    `json:"x,omitempty"`
	Y         int    `json:"y,omitempty"`
	Message   string `json:"message"`
}

func CompareLabelMasks(a, b *LabelMask, tileW, tileH int) *Difference {
	if a == nil || b == nil {
		return &Difference{Stage: "mask", Field: "mask", Message: "one mask is nil"}
	}
	if a.W != b.W || a.H != b.H {
		return &Difference{Stage: "mask", Field: "bounds", Message: fmt.Sprintf("mask bounds differ: %dx%d vs %dx%d", a.W, a.H, b.W, b.H)}
	}
	for y := 0; y < a.H; y++ {
		for x := 0; x < a.W; x++ {
			va, vb := a.At(x, y), b.At(x, y)
			if va != vb {
				tx, ty := 0, 0
				idx := -1
				if tileW > 0 && tileH > 0 {
					tx = x / tileW
					ty = y / tileH
					nx := a.W / tileW
					idx = ty*nx + tx
				}
				return &Difference{TileIndex: idx, TX: tx, TY: ty, Stage: StageClean, Field: "labels", X: x, Y: y,
					Message: fmt.Sprintf("label differs: %d != %d", va, vb)}
			}
		}
	}
	return nil
}

func CompareStats(a, b []RegionStats) *Difference {
	if len(a) != len(b) {
		return &Difference{Stage: StageStats, Field: "region_count", Message: fmt.Sprintf("%d != %d", len(a), len(b))}
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return &Difference{Stage: StageStats, Field: fmt.Sprintf("regions[%d].id", i), Message: fmt.Sprintf("%d != %d", a[i].ID, b[i].ID)}
		}
		if a[i].Area != b[i].Area {
			return &Difference{Stage: StageStats, TileIndex: -1, Field: fmt.Sprintf("regions[%d].area", a[i].ID), Message: fmt.Sprintf("%d != %d", a[i].Area, b[i].Area)}
		}
		if a[i].Holes != b[i].Holes {
			return &Difference{Stage: StageStats, Field: fmt.Sprintf("regions[%d].holes", a[i].ID), Message: fmt.Sprintf("%d != %d", a[i].Holes, b[i].Holes)}
		}
		if a[i].Centroid != b[i].Centroid {
			return &Difference{Stage: StageStats, Field: fmt.Sprintf("regions[%d].centroid", a[i].ID), Message: fmt.Sprintf("%v != %v", a[i].Centroid, b[i].Centroid)}
		}
		if !reflect.DeepEqual(a[i].BBox, b[i].BBox) {
			return &Difference{Stage: StageStats, Field: fmt.Sprintf("regions[%d].bbox", a[i].ID), Message: fmt.Sprintf("%v != %v", a[i].BBox, b[i].BBox)}
		}
	}
	return nil
}

func logicalCommitRecords(rows []CommitRecord) []CommitRecord {
	out := append([]CommitRecord(nil), rows...)
	sortRecordsLogical(out)
	return out
}

func sortRecordsLogical(rows []CommitRecord) {
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			ri, rj := rows[i], rows[j]
			if stageRank(rj.Stage) < stageRank(ri.Stage) || (ri.Stage == rj.Stage && rj.TileIndex < ri.TileIndex) {
				rows[i], rows[j] = rj, ri
			}
		}
	}
}

func CompareCommitSequences(full, candidate []CommitRecord) *Difference {
	a := logicalCommitRecords(full)
	b := logicalCommitRecords(candidate)
	if len(a) != len(b) {
		return &Difference{Stage: "commits", Field: "commit_count", Message: fmt.Sprintf("%d != %d", len(a), len(b))}
	}
	for i := range a {
		if a[i].TileIndex != b[i].TileIndex || a[i].Stage != b[i].Stage {
			return &Difference{TileIndex: b[i].TileIndex, TX: b[i].TX, TY: b[i].TY, Stage: b[i].Stage, Field: "commit_sequence",
				Message: fmt.Sprintf("position %d: expected %d:%s got %d:%s", i, a[i].TileIndex, a[i].Stage, b[i].TileIndex, b[i].Stage)}
		}
		if a[i].ContentHash != b[i].ContentHash {
			return &Difference{TileIndex: b[i].TileIndex, TX: b[i].TX, TY: b[i].TY, Stage: b[i].Stage, Field: "content_hash",
				Message: fmt.Sprintf("expected %s got %s", a[i].ContentHash[:12], b[i].ContentHash[:12])}
		}
	}
	return nil
}

func CompareReports(full, candidate *Report) *Difference {
	if d := CompareLabelMasks(full.Mask, candidate.Mask, full.ManifestTileW(), full.ManifestTileH()); d != nil {
		return d
	}
	if d := CompareStats(full.Stats, candidate.Stats); d != nil {
		return d
	}
	return nil
}

func (r *Report) ManifestTileW() int {
	if len(r.Tiles) == 0 {
		return 0
	}
	return r.Tiles[0].Core.W
}

func (r *Report) ManifestTileH() int {
	if len(r.Tiles) == 0 {
		return 0
	}
	return r.Tiles[0].Core.H
}

func CompareDatabases(ctx context.Context, fullDB, candidateDB, imageID string) (*Difference, error) {
	full, err := LoadCommitOrder(ctx, fullDB, imageID)
	if err != nil {
		return nil, err
	}
	cand, err := LoadCommitOrder(ctx, candidateDB, imageID)
	if err != nil {
		return nil, err
	}
	return CompareCommitSequences(full, cand), nil
}
