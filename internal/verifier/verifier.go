package verifier

import (
	"fmt"
	"strings"

	"maskpreview/internal/core"
	"maskpreview/internal/engine"
	"maskpreview/internal/sample"
	"maskpreview/internal/segment"
)

type SampleReport struct {
	SampleID         string               `json:"sample_id"`
	SnapshotVersion  string               `json:"snapshot_version"`
	TruthFingerprint string               `json:"truth_fingerprint"`
	Status           string               `json:"status"`
	StageCount       int                  `json:"stage_count"`
	MaskHash         string               `json:"mask_hash"`
	TruthMaskHash    string               `json:"truth_mask_hash"`
	MaskHashMatches  bool                 `json:"mask_hash_matches_truth"`
	RegionCount      int                  `json:"region_count"`
	TruthCount       int                  `json:"truth_region_count"`
	TruthComparison  *segment.MatchReport `json:"truth_comparison,omitempty"`
	Error            string               `json:"error,omitempty"`
}

type Report struct {
	Status  string         `json:"status"`
	Samples []SampleReport `json:"samples"`
}

// Verify reconstructs masks exclusively from persisted stage artifacts. It does
// not decode or segment the source image; the manifest is used only for the
// truth fingerprint, expected geometry, and snapshot identity.
func Verify(manifestDir, dbPath, sampleID string) (*Report, error) {
	manifest, err := sample.LoadManifest(manifestDir)
	if err != nil {
		return nil, err
	}
	store, err := engine.OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	report := &Report{Status: "pass"}
	for _, spec := range manifest.Samples {
		if sampleID != "" && spec.ID != sampleID {
			continue
		}
		sr := verifySample(store, spec)
		if sr.Status != "pass" {
			report.Status = "fail"
		}
		report.Samples = append(report.Samples, sr)
	}
	if len(report.Samples) == 0 {
		return nil, fmt.Errorf("sample %q not found in manifest", sampleID)
	}
	return report, nil
}

func verifySample(store *engine.Store, spec core.SampleSpec) SampleReport {
	out := SampleReport{
		SampleID:         spec.ID,
		TruthFingerprint: spec.TruthFingerprint,
		TruthMaskHash:    spec.MaskHash,
		TruthCount:       spec.RegionCount,
		Status:           "pass",
	}
	fail := func(err error) SampleReport {
		out.Status = "fail"
		out.Error = err.Error()
		return out
	}
	snapshot, err := core.NewSnapshot(spec.ID, spec.ImageSHA256, spec.DefaultParams, spec.Seeds, spec.Quantization)
	if err != nil {
		return fail(err)
	}
	out.SnapshotVersion = snapshot.Version
	checkpoint, err := store.LoadCheckpoint(snapshot, spec.TruthFingerprint)
	if err != nil {
		return fail(err)
	}
	out.StageCount = len(checkpoint.Records)
	cleanMask, err := engine.ReassembleFromArtifacts(checkpoint.Artifacts, checkpoint.Blocks, checkpoint.Info.Width, checkpoint.Info.Height, engine.StageClean)
	if err != nil {
		return fail(err)
	}
	growMask, err := engine.ReassembleFromArtifacts(checkpoint.Artifacts, checkpoint.Blocks, checkpoint.Info.Width, checkpoint.Info.Height, engine.StageGrow)
	if err != nil {
		return fail(err)
	}
	if len(growMask) != len(cleanMask) {
		return fail(fmt.Errorf("grow/clean mask dimensions differ"))
	}
	out.MaskHash = core.HashBytes(cleanMask)
	out.MaskHashMatches = out.MaskHash == spec.MaskHash
	regions, _ := segment.Analyze(cleanMask, checkpoint.Info.Width, checkpoint.Info.Height)
	out.RegionCount = len(regions)
	comparison, err := segment.CompareToTruth(regions, spec.Regions, spec.PreviewTolerance)
	if err != nil {
		return fail(err)
	}
	out.TruthComparison = comparison
	if !comparison.WithinTolerance {
		return fail(fmt.Errorf(
			"truth geometry exceeded tolerance: centroid=%.4f area_rel=%.4f",
			comparison.MaxCentroidDeviation, comparison.MaxAreaRelative,
		))
	}
	if err := verifyStatsArtifacts(checkpoint, cleanMask); err != nil {
		return fail(err)
	}
	return out
}

func verifyStatsArtifacts(cp *engine.Checkpoint, cleanMask []uint8) error {
	for _, artifact := range cp.Artifacts {
		if artifact.Stage != engine.StageStats {
			continue
		}
		local := make([]uint8, artifact.Block.Width*artifact.Block.Height)
		for y := 0; y < artifact.Block.Height; y++ {
			copy(local[y*artifact.Block.Width:(y+1)*artifact.Block.Width],
				cleanMask[(artifact.Block.Y+y)*cp.Info.Width+artifact.Block.X:(artifact.Block.Y+y)*cp.Info.Width+artifact.Block.X+artifact.Block.Width])
		}
		expected, _ := segment.Analyze(local, artifact.Block.Width, artifact.Block.Height)
		if !statsEqual(expected, artifact.Regions) {
			return fmt.Errorf("block %d stats artifact does not match clean output: expected=%s stored=%s", artifact.Block.ID, statsBrief(expected), statsBrief(artifact.Regions))
		}
	}
	return nil
}

func statsEqual(a, b []segment.RegionStat) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Area != b[i].Area || a[i].Centroid != b[i].Centroid || a[i].BBox != b[i].BBox {
			return false
		}
		if len(a[i].Contour) != len(b[i].Contour) {
			return false
		}
		for j := range a[i].Contour {
			if a[i].Contour[j] != b[i].Contour[j] {
				return false
			}
		}
	}
	return true
}

func statsBrief(regions []segment.RegionStat) string {
	parts := make([]string, 0, len(regions))
	for _, region := range regions {
		parts = append(parts, fmt.Sprintf("{id=%d area=%d centroid=%.6f,%.6f bbox=%+v contour=%d}",
			region.ID, region.Area, region.Centroid.X, region.Centroid.Y, region.BBox, len(region.Contour)))
	}
	return "[" + strings.Join(parts, " ") + "]"
}
