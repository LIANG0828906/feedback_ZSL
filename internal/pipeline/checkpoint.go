package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"maskpipe/internal/model"
	"maskpipe/internal/store"
)

type SnapshotValidation struct {
	Valid     bool                `json:"valid"`
	Reason    string              `json:"reason,omitempty"`
	Violation *SnapshotViolation  `json:"violation,omitempty"`
	Records   []store.StageRecord `json:"-"`
}

type SnapshotViolation struct {
	ChunkX int         `json:"chunk_x"`
	ChunkY int         `json:"chunk_y"`
	Stage  model.Stage `json:"stage"`
	Field  string      `json:"field"`
	Want   string      `json:"want,omitempty"`
	Got    string      `json:"got,omitempty"`
}

func ValidateSnapshot(ctx context.Context, st *store.Store, info store.SnapshotInfo, expected store.SnapshotInfo) SnapshotValidation {
	stored, err := st.SnapshotInfo(ctx, expected.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return SnapshotValidation{Valid: false, Reason: "missing_snapshot_meta"}
	}
	if err != nil {
		return SnapshotValidation{Valid: false, Reason: err.Error()}
	}
	checks := []struct {
		field string
		want  string
		got   string
	}{
		{"algo_version", expected.AlgorithmVersion, stored.AlgorithmVersion},
		{"param_version", expected.ParamVersion, stored.ParamVersion},
		{"width", fmt.Sprint(expected.Width), fmt.Sprint(stored.Width)},
		{"height", fmt.Sprint(expected.Height), fmt.Sprint(stored.Height)},
		{"tile_width", fmt.Sprint(expected.TileWidth), fmt.Sprint(stored.TileWidth)},
		{"tile_height", fmt.Sprint(expected.TileHeight), fmt.Sprint(stored.TileHeight)},
		{"truth_fingerprint", expected.TruthFingerprint, stored.TruthFingerprint},
	}
	for _, check := range checks {
		if check.want != check.got {
			return SnapshotValidation{Valid: false, Reason: "version_or_meta_mismatch", Violation: &SnapshotViolation{
				Field: check.field, Want: check.want, Got: check.got,
			}}
		}
	}

	records, err := st.ListStages(ctx, expected.ID)
	if err != nil {
		return SnapshotValidation{Valid: false, Reason: err.Error()}
	}
	for _, rec := range records {
		if rec.AlgoVersion != expected.AlgorithmVersion {
			return invalidRecordVersion(rec, "algo_version", expected.AlgorithmVersion, rec.AlgoVersion)
		}
		if rec.ParamVersion != expected.ParamVersion {
			return invalidRecordVersion(rec, "param_version", expected.ParamVersion, rec.ParamVersion)
		}
		if got := store.HashBytes(rec.Blob); got != rec.ContentHash {
			return SnapshotValidation{Valid: false, Reason: "content_hash_mismatch", Violation: &SnapshotViolation{
				ChunkX: rec.ChunkX, ChunkY: rec.ChunkY, Stage: rec.Stage,
				Field: "content_hash", Want: rec.ContentHash, Got: got,
			}}
		}
	}
	return SnapshotValidation{Valid: true, Records: records}
}

func invalidRecordVersion(rec store.StageRecord, field, want, got string) SnapshotValidation {
	return SnapshotValidation{Valid: false, Reason: "stage_version_mismatch", Violation: &SnapshotViolation{
		ChunkX: rec.ChunkX, ChunkY: rec.ChunkY, Stage: rec.Stage,
		Field: field, Want: want, Got: got,
	}}
}
