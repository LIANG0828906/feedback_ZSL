package verify

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"

	"iconbench/internal/checkpoint"
	"iconbench/internal/imaging"
	"iconbench/internal/model"
	"iconbench/internal/pipeline"
	"iconbench/internal/sample"
)

type Violation struct {
	BlockX   int    `json:"block_x"`
	BlockY   int    `json:"block_y"`
	PixelX   int    `json:"pixel_x,omitempty"`
	PixelY   int    `json:"pixel_y,omitempty"`
	Layer    string `json:"layer"`
	Field    string `json:"field"`
	Message  string `json:"message"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
}

type Report struct {
	OK                 bool                    `json:"ok"`
	ImageID            string                  `json:"image_id"`
	Database           string                  `json:"database"`
	MaskHash           string                  `json:"mask_hash,omitempty"`
	SequenceCount      int                     `json:"sequence_count"`
	TruthFingerprintOK bool                    `json:"truth_fingerprint_ok"`
	Violation          *Violation              `json:"violation,omitempty"`
	Metrics            model.DivergenceMetrics `json:"metrics,omitempty"`
	RegionReport       *imaging.RegionReport   `json:"region_report,omitempty"`
}

func VerifyAll(dataDir, dbPath string) ([]*Report, error) {
	manifest, err := sample.LoadManifest(filepath.Join(dataDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	if dbPath == "" {
		dbPath = filepath.Join(dataDir, "checkpoints.db")
	}
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	var reports []*Report
	for _, rec := range manifest.Images {
		reports = append(reports, VerifyImage(context.Background(), manifest, store, dataDir, dbPath, rec.ID))
	}
	return reports, nil
}

func VerifyImage(ctx context.Context, manifest *model.Manifest, store *checkpoint.Store, dataDir, dbPath, imageID string) *Report {
	rec, err := findImage(manifest, imageID)
	if err != nil {
		return &Report{OK: false, ImageID: imageID, Database: dbPath, Violation: &Violation{Layer: "manifest", Field: "image_id", Message: err.Error()}}
	}
	report := &Report{ImageID: imageID, Database: dbPath}

	truth, tw, th, err := sample.DecodeReferenceMask(rec)
	if err != nil {
		return fail(report, &Violation{Layer: "manifest", Field: "reference_mask", Message: err.Error()})
	}
	maskSum := sha256.Sum256(truth)
	fp := sha256.New()
	fp.Write(maskSum[:])
	regionJSON, _ := json.Marshal(rec.Regions)
	fp.Write(regionJSON)
	report.TruthFingerprintOK = fmt.Sprintf("%x", fp.Sum(nil)) == rec.TruthFingerprint
	if !report.TruthFingerprintOK {
		return fail(report, &Violation{Layer: "manifest", Field: "truth_fingerprint", Expected: rec.TruthFingerprint, Actual: fmt.Sprintf("%x", fp.Sum(nil)), Message: "truth fingerprint mismatch"})
	}

	records, err := store.CommittedLayers(ctx, imageID)
	if err != nil {
		return fail(report, &Violation{Layer: "sqlite", Field: "committed_layers", Message: err.Error()})
	}
	if len(records) == 0 {
		return fail(report, &Violation{Layer: "checkpoint", Field: "committed_layers", Expected: "non-empty", Actual: 0, Message: "no committed layer products"})
	}

	var params model.Params
	for _, r := range records {
		var art pipeline.Artifact
		if err := json.Unmarshal([]byte(r.ArtifactJSON), &art); err != nil {
			return fail(report, &Violation{BlockX: r.BlockX, BlockY: r.BlockY, Layer: r.Layer, Field: "artifact_json", Message: err.Error()})
		}
		params = art.Params
		break
	}

	if v, err := store.ValidateSnapshot(ctx, imageID, params); err != nil {
		return fail(report, &Violation{Layer: "sqlite", Field: "validate_snapshot", Message: err.Error()})
	} else if v != nil {
		return fail(report, &Violation{BlockX: v.BlockX, BlockY: v.BlockY, Layer: v.Layer, Field: v.Field, Expected: v.Expected, Actual: v.Actual, Message: "checkpoint hash or version mismatch"})
	}

	blocks := pipeline.BuildBlocks(rec.Width, rec.Height, manifest.Budget.TileSize)
	for _, layer := range checkpoint.Layers {
		for _, b := range blocks {
			r, ok := records[checkpoint.Key(b.Index, layer)]
			if !ok || r.State != checkpoint.StateCommitted {
				return fail(report, &Violation{BlockX: b.X, BlockY: b.Y, Layer: layer, Field: "state", Expected: checkpoint.StateCommitted, Actual: r.State, Message: "missing or uncommitted block layer"})
			}
		}
	}

	seq, err := store.Sequence(ctx, imageID)
	if err != nil {
		return fail(report, &Violation{Layer: "sqlite", Field: "commit_sequence", Message: err.Error()})
	}
	report.SequenceCount = len(seq)
	if v := checkSequence(seq, blocks); v != nil {
		return fail(report, v)
	}

	raw := make([]byte, rec.Width*rec.Height)
	for _, b := range blocks {
		r := records[checkpoint.Key(b.Index, checkpoint.LayerFloodMerge)]
		var art pipeline.Artifact
		if err := json.Unmarshal([]byte(r.ArtifactJSON), &art); err != nil {
			return fail(report, &Violation{BlockX: b.X, BlockY: b.Y, Layer: checkpoint.LayerFloodMerge, Field: "artifact_json", Message: err.Error()})
		}
		if len(art.RawMask) != b.Width*b.Height {
			return fail(report, &Violation{BlockX: b.X, BlockY: b.Y, Layer: checkpoint.LayerFloodMerge, Field: "raw_mask_length", Expected: b.Width * b.Height, Actual: len(art.RawMask), Message: "raw mask length mismatch"})
		}
		for y := 0; y < b.Height; y++ {
			for x := 0; x < b.Width; x++ {
				raw[(b.Y0+y)*rec.Width+(b.X0+x)] = art.RawMask[y*b.Width+x]
			}
		}
	}

	recomputed := imaging.OpenClose(raw, rec.Width, rec.Height, params.MorphRadius)
	recomputedHash := pipeline.HashBytes(recomputed)
	_, storedHash, _, err := store.LoadResult(ctx, imageID)
	if err != nil {
		return fail(report, &Violation{Layer: checkpoint.LayerStats, Field: "image_result", Message: err.Error()})
	}
	if storedHash != recomputedHash {
		return fail(report, &Violation{Layer: checkpoint.LayerStats, Field: "mask_hash", Expected: storedHash, Actual: recomputedHash, Message: "recomputed mask hash differs from persisted result"})
	}
	report.MaskHash = recomputedHash

	for _, b := range blocks {
		r := records[checkpoint.Key(b.Index, checkpoint.LayerStats)]
		var art pipeline.Artifact
		if err := json.Unmarshal([]byte(r.ArtifactJSON), &art); err != nil {
			return fail(report, &Violation{BlockX: b.X, BlockY: b.Y, Layer: checkpoint.LayerStats, Field: "artifact_json", Message: err.Error()})
		}
		if art.Stats == nil || len(art.Stats.CleanMask) != b.Width*b.Height {
			return fail(report, &Violation{BlockX: b.X, BlockY: b.Y, Layer: checkpoint.LayerStats, Field: "clean_mask", Message: "stats artifact has no clean mask"})
		}
		for y := 0; y < b.Height; y++ {
			for x := 0; x < b.Width; x++ {
				expected := recomputed[(b.Y0+y)*rec.Width+(b.X0+x)]
				actual := art.Stats.CleanMask[y*b.Width+x]
				if expected != actual {
					return fail(report, &Violation{BlockX: b.X, BlockY: b.Y, PixelX: b.X0 + x, PixelY: b.Y0 + y, Layer: checkpoint.LayerStats, Field: "clean_mask", Expected: expected, Actual: actual, Message: "persisted stats tile differs from mask recomputed from RAG products"})
				}
			}
		}
		if art.Stats.MaskHash != pipeline.HashBytes(art.Stats.CleanMask) {
			return fail(report, &Violation{BlockX: b.X, BlockY: b.Y, Layer: checkpoint.LayerStats, Field: "mask_hash", Expected: art.Stats.MaskHash, Actual: pipeline.HashBytes(art.Stats.CleanMask), Message: "stats tile hash mismatch"})
		}
	}

	predLabels, predStats := imaging.ConnectedComponents(recomputed, rec.Width, rec.Height)
	truthLabels, truthStats := imaging.ConnectedComponents(truth, tw, th)
	regionReport := imaging.MatchRegions(imageID, predStats, truthStats, predLabels, truthLabels, rec.Width, rec.Height, manifest.Tolerances)
	metrics := imaging.MaskMetrics(recomputed, truth)
	metrics.RegionCountActual = len(predStats)
	metrics.RegionCountExpected = len(truthStats)
	metrics.RegionCountDelta = len(predStats) - len(truthStats)
	metrics.MaxCentroidErrorPx = regionReport.MaxCentroidErrorPx
	metrics.MaxAreaRelativeError = regionReport.MaxAreaRelativeError
	report.Metrics = metrics
	report.RegionReport = regionReport

	if regionReport.StateError != nil || !regionReport.WithinTolerance {
		x, y, hasDiff := imaging.FirstMaskDifference(recomputed, truth, rec.Width, rec.Height)
		bx, by := 0, 0
		if hasDiff {
			bx, by = blockOfPixel(x, y, manifest.Budget.TileSize)
		}
		return fail(report, &Violation{
			BlockX: bx, BlockY: by, PixelX: x, PixelY: y,
			Layer: checkpoint.LayerFloodMerge, Field: "foreground_mask",
			Expected: map[string]any{"iou_min": manifest.Tolerances.MaskIoUMin, "centroid_px": manifest.Tolerances.CentroidPx, "area_rel": manifest.Tolerances.AreaRel},
			Actual:   map[string]any{"iou": metrics.IoU, "centroid_px": metrics.MaxCentroidErrorPx, "area_rel": metrics.MaxAreaRelativeError, "region_error": regionReport.StateError},
			Message:  "recomputed mask is outside declared truth tolerance",
		})
	}

	report.OK = true
	return report
}

func findImage(m *model.Manifest, id string) (model.ImageManifest, error) {
	for _, rec := range m.Images {
		if rec.ID == id {
			return rec, nil
		}
	}
	return model.ImageManifest{}, fmt.Errorf("image not found: %s", id)
}

func fail(r *Report, v *Violation) *Report {
	r.OK = false
	r.Violation = v
	return r
}

func checkSequence(seq []checkpoint.CommitRecord, blocks []pipeline.Block) *Violation {
	expected := make([]struct {
		layer string
		block int
	}, 0, len(blocks)*4)
	for _, layer := range checkpoint.Layers {
		for _, b := range blocks {
			expected = append(expected, struct {
				layer string
				block int
			}{layer, b.Index})
		}
	}
	if len(seq) < len(expected) {
		return &Violation{Layer: "commit_sequence", Field: "length", Expected: len(expected), Actual: len(seq), Message: "incomplete commit sequence"}
	}
	for i, want := range expected {
		if i >= len(seq) {
			break
		}
		if seq[i].Layer != want.layer || seq[i].BlockIndex != want.block {
			return &Violation{BlockX: seq[i].BlockX, BlockY: seq[i].BlockY, Layer: seq[i].Layer, Field: "commit_sequence", Expected: fmt.Sprintf("%s block %d", want.layer, want.block), Actual: fmt.Sprintf("%s block %d", seq[i].Layer, seq[i].BlockIndex), Message: "unstable layer/block commit order"}
		}
	}
	return nil
}

func blockOfPixel(x, y, tile int) (int, int) {
	if tile <= 0 {
		tile = 64
	}
	if x < 0 || y < 0 {
		return -1, -1
	}
	return x / tile, y / tile
}
