package verify

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"

	"maskpipeline/internal/apperr"
	"maskpipeline/internal/checkpoint"
	"maskpipeline/internal/model"
	"maskpipeline/internal/sample"
)

type Report struct {
	SessionID          string              `json:"session_id"`
	Status             string              `json:"status"`
	TruthFingerprint   string              `json:"truth_fingerprint"`
	TruthFingerprintOK bool                `json:"truth_fingerprint_ok"`
	CommitCount        int                 `json:"commit_count"`
	StageSequence      []string            `json:"stage_sequence"`
	GrowthMaskHash     string              `json:"growth_mask_hash"`
	CleanupMaskHash    string              `json:"cleanup_mask_hash"`
	StatsHash          string              `json:"stats_hash"`
	RegionCount        int                 `json:"region_count"`
	Regions            []model.RegionStats `json:"regions"`
}

func Run(ctx context.Context, store *checkpoint.Store, sampleDir, sessionID string) (*Report, error) {
	if store == nil {
		return nil, apperr.New(apperr.CodeIO, "verify", "checkpoint store is required")
	}
	manifest, err := sample.LoadManifest(sampleDir)
	if err != nil {
		return nil, err
	}
	truthFP := manifest.Fingerprint()
	if truthFP != manifest.Truth.Fingerprint {
		return nil, apperr.New(apperr.CodeMeta, "verify", "manifest truth fingerprint is self-inconsistent").
			WithField("truth.fingerprint").
			WithDetail("declared", manifest.Truth.Fingerprint).
			WithDetail("actual", truthFP)
	}
	summary, found, err := store.Summary(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, apperr.New(apperr.CodeState, "verify", "checkpoint session does not exist").
			WithDetail("session_id", sessionID)
	}
	if summary.Meta.TruthFingerprint != truthFP {
		return nil, apperr.New(apperr.CodeMeta, "verify", "session truth fingerprint differs from manifest").
			WithField("truth.fingerprint").
			WithDetail("session", summary.Meta.TruthFingerprint).
			WithDetail("manifest", truthFP)
	}
	if summary.Meta.Width != manifest.Width || summary.Meta.Height != manifest.Height ||
		summary.Meta.TileSize != manifest.TileSize {
		return nil, apperr.New(apperr.CodeMeta, "verify", "session dimensions/tile size differ from manifest").
			WithField("tile_size").
			WithDetail("session_width", summary.Meta.Width).
			WithDetail("manifest_width", manifest.Width).
			WithDetail("session_tile", summary.Meta.TileSize).
			WithDetail("manifest_tile", manifest.TileSize)
	}
	tiles := model.TileLayout(manifest.Width, manifest.Height, manifest.TileSize)
	if summary.Meta.TilesX*summary.Meta.TilesY != len(tiles) {
		return nil, apperr.New(apperr.CodeMeta, "verify", "session tile grid differs from manifest layout").
			WithField("tiles_x")
	}
	commits, err := store.Commits(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	wantSeq := checkpoint.StageSequence(len(tiles))
	gotSeq := checkpoint.ParseStageSequence(commits)
	if len(commits) != len(wantSeq) {
		x, y, tile, stage := firstMissingLocation(tiles, wantSeq, gotSeq)
		return nil, inconsistency("commit sequence is incomplete", x, y, tile, stage,
			map[string]any{"want": len(wantSeq), "got": len(commits)})
	}
	for i := range wantSeq {
		if gotSeq[i] != wantSeq[i] {
			tile, stage := parseSeq(wantSeq[i])
			x, y := tileOrigin(tiles, tile)
			return nil, inconsistency("commit sequence differs from canonical order", x, y, tile, stage,
				map[string]any{"want": wantSeq[i], "got": gotSeq[i], "seq": i})
		}
		if commits[i].Seq != i {
			tile, stage := commits[i].TileIndex, commits[i].Stage
			x, y := tileOrigin(tiles, tile)
			return nil, inconsistency("commit sequence numbers are not contiguous", x, y, tile, stage,
				map[string]any{"want_seq": i, "got_seq": commits[i].Seq})
		}
		if commits[i].ParamVersion != summary.Meta.ParamsVersion || commits[i].AlgorithmVersion != summary.Meta.AlgorithmVersion {
			tile, stage := commits[i].TileIndex, commits[i].Stage
			x, y := tileOrigin(tiles, tile)
			return nil, apperr.New(apperr.CodeVersion, "verify", "commit parameter or algorithm version mismatch").
				WithDetail("x", x).WithDetail("y", y).
				WithDetail("tile", tile).WithDetail("stage", stage).
				WithDetail("left_version", summary.Meta.ParamsVersion).
				WithDetail("right_version", commits[i].ParamVersion).
				WithDetail("left_algorithm", summary.Meta.AlgorithmVersion).
				WithDetail("right_algorithm", commits[i].AlgorithmVersion)
		}
	}

	stages, err := store.AllStages(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	stageMap := make(map[[2]int]checkpoint.StageView, len(stages))
	for _, st := range stages {
		stageMap[[2]int{st.TileIndex, int(stageIndex(st.Stage))}] = st
	}
	for _, tile := range tiles {
		for _, stage := range model.Stages {
			st, ok := stageMap[[2]int{tile.Index, int(stageIndex(stage))}]
			if !ok || st.State != model.StateCommitted {
				return nil, inconsistency("stage is missing or not committed", tile.X, tile.Y, tile.Index, stage, nil)
			}
			if st.ParamVersion != summary.Meta.ParamsVersion || st.AlgorithmVersion != summary.Meta.AlgorithmVersion {
				return nil, apperr.New(apperr.CodeVersion, "verify", "stage version differs from session").
					WithDetail("x", tile.X).WithDetail("y", tile.Y).
					WithDetail("tile", tile.Index).WithDetail("stage", stage).
					WithDetail("left_version", summary.Meta.ParamsVersion).
					WithDetail("right_version", st.ParamVersion)
			}
			if st.ContentHash == "" || model.HashBytes(st.Output) != st.ContentHash {
				return nil, inconsistency("stage content hash mismatch", tile.X, tile.Y, tile.Index, stage, nil)
			}
			if len(st.Output) != tile.Width*tile.Height && stage != model.StageStats {
				return nil, inconsistency("stage binary output length mismatch", tile.X, tile.Y, tile.Index, stage,
					map[string]any{"want": tile.Width * tile.Height, "got": len(st.Output)})
			}
		}
	}

	growth := make([]byte, manifest.Width*manifest.Height)
	cleanup := make([]byte, manifest.Width*manifest.Height)
	partials := make([]model.TileStats, 0, len(tiles))
	for _, tile := range tiles {
		growthTile := stageMap[[2]int{tile.Index, int(stageIndex(model.StageGrowth))}].Output
		cleanupTile := stageMap[[2]int{tile.Index, int(stageIndex(model.StageCleanup))}].Output
		if err := model.PasteLabels(growth, manifest.Width, manifest.Height, tile, growthTile); err != nil {
			return nil, inconsistency("cannot reassemble growth tile", tile.X, tile.Y, tile.Index, model.StageGrowth, nil)
		}
		if err := model.PasteLabels(cleanup, manifest.Width, manifest.Height, tile, cleanupTile); err != nil {
			return nil, inconsistency("cannot reassemble cleanup tile", tile.X, tile.Y, tile.Index, model.StageCleanup, nil)
		}
		statsView := stageMap[[2]int{tile.Index, int(stageIndex(model.StageStats))}]
		var partial model.TileStats
		if err := json.Unmarshal(statsView.Output, &partial); err != nil {
			return nil, inconsistency("stats stage payload is invalid JSON", tile.X, tile.Y, tile.Index, model.StageStats, nil)
		}
		if partial.TileIndex != tile.Index || partial.Width != tile.Width || partial.Height != tile.Height {
			return nil, inconsistency("stats stage geometry mismatch", tile.X, tile.Y, tile.Index, model.StageStats,
				map[string]any{"tile_index": partial.TileIndex, "width": partial.Width, "height": partial.Height})
		}
		expected := model.TilePartialStats(tile.Index, cleanupTile, tile.Width, tile.Height, tile.X, tile.Y)
		if !reflect.DeepEqual(partial, expected) {
			return nil, inconsistency("stats stage payload differs from cleanup tile", tile.X, tile.Y, tile.Index, model.StageStats, nil)
		}
		partials = append(partials, partial)
	}

	stats := model.StatsFromLabels(cleanup, manifest.Width, manifest.Height)
	aggregated := model.AggregateTileStats(partials)
	if !statsEqual(stats, aggregated) {
		pt := model.Point{X: 0, Y: 0}
		if len(stats.Regions) > 0 {
			pt = stats.Regions[0].BBoxPoint()
		}
		tile, stage := locatePixel(tiles, pt.X, pt.Y)
		return nil, inconsistency("aggregated stats differ from reassembled cleanup mask", pt.X, pt.Y, tile, stage, nil)
	}
	if stats.RegionCount != manifest.Truth.RegionCount {
		pt := model.Point{X: 0, Y: 0}
		if len(stats.Regions) > 0 {
			pt = stats.Regions[0].BBoxPoint()
		}
		tile, stage := locatePixel(tiles, pt.X, pt.Y)
		return nil, inconsistency("foreground region count differs from manifest truth count", pt.X, pt.Y, tile, stage,
			map[string]any{"manifest": manifest.Truth.RegionCount, "checkpoint": stats.RegionCount})
	}
	cleanupHash := model.HashBytes(cleanup)
	statsHash := model.StatsHash(stats)
	if summary.FinalMaskHash != "" && summary.FinalMaskHash != cleanupHash {
		tile, _ := locatePixel(tiles, 0, 0)
		return nil, inconsistency("final mask hash differs from reassembled cleanup mask", 0, 0, tile, model.StageCleanup,
			map[string]any{"session": summary.FinalMaskHash, "recomputed": cleanupHash})
	}
	if summary.FinalStatsHash != "" && summary.FinalStatsHash != statsHash {
		pt := model.Point{X: 0, Y: 0}
		if len(stats.Regions) > 0 {
			pt = stats.Regions[0].BBoxPoint()
		}
		tile, stage := locatePixel(tiles, pt.X, pt.Y)
		return nil, inconsistency("final stats hash differs from recomputed statistics", pt.X, pt.Y, tile, stage,
			map[string]any{"session": summary.FinalStatsHash, "recomputed": statsHash})
	}
	return &Report{
		SessionID:          sessionID,
		Status:             summary.Status,
		TruthFingerprint:   truthFP,
		TruthFingerprintOK: true,
		CommitCount:        len(commits),
		StageSequence:      gotSeq,
		GrowthMaskHash:     model.HashBytes(growth),
		CleanupMaskHash:    cleanupHash,
		StatsHash:          statsHash,
		RegionCount:        stats.RegionCount,
		Regions:            stats.Regions,
	}, nil
}

func inconsistency(message string, x, y, tile int, stage model.Stage, details map[string]any) error {
	e := apperr.New(apperr.CodeBlock, "verify", message).
		WithDetail("x", x).
		WithDetail("y", y).
		WithDetail("tile", tile).
		WithDetail("stage", stage)
	return e.WithDetails(details)
}

func stageIndex(stage model.Stage) int {
	for i, s := range model.Stages {
		if s == stage {
			return i
		}
	}
	return -1
}

func parseSeq(s string) (int, model.Stage) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, model.StageGrowth
	}
	tile, _ := strconv.Atoi(parts[0])
	return tile, model.Stage(parts[1])
}

func tileOrigin(tiles []model.Tile, index int) (int, int) {
	if index >= 0 && index < len(tiles) {
		return tiles[index].X, tiles[index].Y
	}
	return 0, 0
}

func firstMissingLocation(tiles []model.Tile, want, got []string) (int, int, int, model.Stage) {
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		if want[i] != got[i] {
			tile, stage := parseSeq(want[i])
			x, y := tileOrigin(tiles, tile)
			return x, y, tile, stage
		}
	}
	if len(got) > len(want) {
		tile, stage := parseSeq(got[len(want)])
		x, y := tileOrigin(tiles, tile)
		return x, y, tile, stage
	}
	tile, stage := parseSeq(want[n])
	x, y := tileOrigin(tiles, tile)
	return x, y, tile, stage
}

func locatePixel(tiles []model.Tile, x, y int) (int, model.Stage) {
	for _, tile := range tiles {
		if x >= tile.X && x < tile.X+tile.Width && y >= tile.Y && y < tile.Y+tile.Height {
			return tile.Index, model.StageCleanup
		}
	}
	return 0, model.StageCleanup
}

func statsEqual(a, b model.Stats) bool {
	if a.RegionCount != b.RegionCount || len(a.Regions) != len(b.Regions) {
		return false
	}
	for i := range a.Regions {
		ar, br := a.Regions[i], b.Regions[i]
		if ar.ID != br.ID || ar.Area != br.Area || ar.BBox != br.BBox ||
			math.Abs(ar.Centroid.X-br.Centroid.X) > 1e-12 ||
			math.Abs(ar.Centroid.Y-br.Centroid.Y) > 1e-12 {
			return false
		}
	}
	return true
}
