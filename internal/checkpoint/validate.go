package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"iconbench/internal/model"
)

func (s *Store) ValidateSnapshot(ctx context.Context, imageID string, p model.Params) (*Violation, error) {
	records, err := s.CommittedLayers(ctx, imageID)
	if err != nil {
		return nil, err
	}
	ordered := make([]LayerRecord, 0, len(records))
	for _, rec := range records {
		ordered = append(ordered, rec)
	}
	layerRank := map[string]int{LayerGradient: 0, LayerMarkers: 1, LayerFloodMerge: 2, LayerStats: 3}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].BlockIndex != ordered[j].BlockIndex {
			return ordered[i].BlockIndex < ordered[j].BlockIndex
		}
		return layerRank[ordered[i].Layer] < layerRank[ordered[j].Layer]
	})
	for _, rec := range ordered {
		sum := sha256.Sum256([]byte(rec.ArtifactJSON))
		actualHash := hex.EncodeToString(sum[:])
		if rec.ContentHash == "" || actualHash != rec.ContentHash {
			return &Violation{BlockX: rec.BlockX, BlockY: rec.BlockY, Layer: rec.Layer, Field: "content_hash", Expected: rec.ContentHash, Actual: actualHash}, nil
		}
		if rec.ParamVersion != p.ParamVersion {
			return &Violation{BlockX: rec.BlockX, BlockY: rec.BlockY, Layer: rec.Layer, Field: "param_version", Expected: p.ParamVersion, Actual: rec.ParamVersion}, nil
		}
		if rec.ParamHash != p.CanonicalHash() {
			return &Violation{BlockX: rec.BlockX, BlockY: rec.BlockY, Layer: rec.Layer, Field: "param_hash", Expected: p.CanonicalHash(), Actual: rec.ParamHash}, nil
		}
		if rec.AlgorithmVersion != model.AlgorithmVersion {
			return &Violation{BlockX: rec.BlockX, BlockY: rec.BlockY, Layer: rec.Layer, Field: "algorithm_version", Expected: model.AlgorithmVersion, Actual: rec.AlgorithmVersion}, nil
		}
		if rec.SchemaVersion != model.SchemaVersion {
			return &Violation{BlockX: rec.BlockX, BlockY: rec.BlockY, Layer: rec.Layer, Field: "schema_version", Expected: model.SchemaVersion, Actual: rec.SchemaVersion}, nil
		}
	}
	return nil, nil
}
