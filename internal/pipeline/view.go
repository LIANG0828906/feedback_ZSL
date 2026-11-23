package pipeline

import (
	"fmt"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/store"
)

func BuildRunView(sample *domain.LoadedSample, snapshot store.Snapshot, invalidated []string) domain.RunView {
	view := domain.RunView{
		RunID:            fmt.Sprint(snapshot.Run.ID),
		SampleID:         snapshot.Run.SampleID,
		SampleTitle:      sample.Meta.Title,
		Status:           snapshot.Run.Status,
		ImageWidth:       sample.Width,
		ImageHeight:      sample.Height,
		ImageHash:        sample.ContentHash,
		TemplateVersion:  sample.Manifest.FontVersion,
		ParamVersion:     snapshot.Run.ParamVersion,
		AlgorithmVersion: snapshot.Run.AlgorithmVersion,
		BandWidth:        snapshot.Run.BandWidth,
		AvgStrokeWidth:   snapshot.Run.AvgStrokeWidth,
		Params:           snapshot.Run.Params,
		Tiles:            make([]domain.TileView, 0, len(snapshot.Tiles)),
		Lines:            make([]domain.LineView, 0, len(snapshot.Lines)),
		Commits:          append([]domain.CommitEntry(nil), snapshot.Commits...),
		TruthRegions:     append([]domain.TruthRegion(nil), sample.Truth.Regions...),
		Invalidated:      invalidated,
		Error:            snapshot.Run.Error,
	}
	for _, tile := range snapshot.Tiles {
		view.Tiles = append(view.Tiles, domain.TileView{
			Tile:           tile.Tile,
			State:          tile.State,
			CandidateCount: len(tile.Candidates),
			Candidates:     tile.Candidates,
			Error:          tile.Error,
			CommitSeq:      tile.CommitSeq,
			PayloadHash:    tile.PayloadHash,
		})
	}
	stats := domain.QualityStats{
		TileCount:        len(snapshot.Tiles),
		DetectedLines:    len(snapshot.Lines),
		TruthRegionCount: len(sample.Truth.Regions),
	}
	confidenceSum := 0.0
	confidenceCount := 0
	minConfidence := 1.0
	for _, line := range snapshot.Lines {
		lineView := domain.LineView{
			ReadingIndex: line.Line.ReadingIndex,
			BBox:         line.Line.BBox,
			Quad:         line.Line.Quad,
			State:        line.State,
			Error:        line.Error,
			CommitSeq:    line.CommitSeq,
			PayloadHash:  line.PayloadHash,
			Snapshot:     line.Snapshot,
		}
		if line.State == domain.LineCommitted {
			lineView.Text = line.Recognition.Text
			lineView.Empty = line.Recognition.Empty
			lineView.Confidence = line.Recognition.Confidence
			lineView.Accepted = line.Recognition.Accepted
			lineView.Characters = line.Recognition.Characters
			stats.CommittedLines++
			stats.CharCount += len(line.Recognition.Characters)
			if line.Recognition.Empty {
				stats.EmptyLines++
			}
			confidenceSum += line.Recognition.Confidence
			confidenceCount++
			if line.Recognition.Confidence < minConfidence {
				minConfidence = line.Recognition.Confidence
			}
			if !line.Recognition.Accepted {
				stats.LowConfidenceLines++
			}
		}
		view.Lines = append(view.Lines, lineView)
	}
	for _, tile := range snapshot.Tiles {
		if tile.State == domain.TileCommitted {
			stats.CommittedTiles++
		}
		stats.CandidateCount += len(tile.Candidates)
	}
	if confidenceCount > 0 {
		stats.AvgConfidence = domain.Round4(confidenceSum / float64(confidenceCount))
		stats.MinConfidence = domain.Round4(minConfidence)
	}
	if len(snapshot.Lines) > 0 {
		stats.CountDeviation = len(snapshot.Lines) - len(sample.Truth.Regions)
	}
	iou := 0.0
	matched := 0
	for _, line := range snapshot.Lines {
		if line.State != domain.LineCommitted {
			continue
		}
		if line.Line.ReadingIndex >= 0 && line.Line.ReadingIndex < len(sample.Truth.Regions) {
			truth := sample.Truth.Regions[line.Line.ReadingIndex]
			iou += line.Line.BBox.IoU(truth.BBox)
			if line.Recognition.Text == truth.Text {
				matched++
			}
		}
	}
	if len(snapshot.Lines) > 0 {
		stats.AvgTruthIoU = domain.Round4(iou / float64(len(snapshot.Lines)))
	}
	stats.TextMatchLines = matched
	if len(sample.Truth.Regions) > 0 {
		stats.TextAccuracy = domain.Round4(float64(matched) / float64(len(sample.Truth.Regions)))
	}
	view.Stats = stats
	view.ProgressText = fmt.Sprintf("Tiles %d/%d · Lines %d/%d", stats.CommittedTiles, stats.TileCount, stats.CommittedLines, stats.DetectedLines)
	return view
}

func lineMatches(a, b domain.LineView) (bool, string) {
	if a.ReadingIndex != b.ReadingIndex {
		return false, "reading_index"
	}
	if a.Text != b.Text {
		return false, "text"
	}
	if a.Confidence != b.Confidence {
		return false, "confidence"
	}
	if a.Empty != b.Empty {
		return false, "empty"
	}
	if a.Accepted != b.Accepted {
		return false, "accepted"
	}
	if a.BBox != b.BBox {
		return false, "bbox"
	}
	if len(a.Characters) != len(b.Characters) {
		return false, "characters.length"
	}
	for i := range a.Characters {
		if a.Characters[i] != b.Characters[i] {
			return false, fmt.Sprintf("characters[%d]", i)
		}
	}
	if a.PayloadHash != b.PayloadHash {
		return false, "payload_hash"
	}
	if a.CommitSeq != b.CommitSeq {
		return false, "commit_seq"
	}
	return true, ""
}

func tileMatches(a, b domain.TileView) (bool, string) {
	if a.Index != b.Index || a.X != b.X || a.Y != b.Y || a.W != b.W || a.H != b.H {
		return false, "tile_geometry"
	}
	if a.State != b.State {
		return false, "state"
	}
	if a.CandidateCount != b.CandidateCount {
		return false, "candidate_count"
	}
	if len(a.Candidates) != len(b.Candidates) {
		return false, "candidates.length"
	}
	for i := range a.Candidates {
		if a.Candidates[i] != b.Candidates[i] {
			return false, fmt.Sprintf("candidates[%d]", i)
		}
	}
	if a.PayloadHash != b.PayloadHash {
		return false, "payload_hash"
	}
	if a.CommitSeq != b.CommitSeq {
		return false, "commit_seq"
	}
	return true, ""
}

func commitMatches(a, b domain.CommitEntry) (bool, string) {
	if a.Seq != b.Seq {
		return false, "commit_seq"
	}
	if a.Kind != b.Kind {
		return false, "commit_kind"
	}
	if a.UnitKey != b.UnitKey {
		return false, "unit_key"
	}
	if a.PayloadHash != b.PayloadHash {
		return false, "commit_payload_hash"
	}
	return true, ""
}

func tileCoordinatesForLine(line domain.LineView, tiles []domain.TileView) (int, int, int) {
	for _, tile := range tiles {
		if line.BBox.X >= tile.X && line.BBox.Y >= tile.Y && line.BBox.X < tile.X+tile.W && line.BBox.Y < tile.Y+tile.H {
			return tile.Index, tile.X, tile.Y
		}
	}
	if len(tiles) > 0 {
		return tiles[0].Index, tiles[0].X, tiles[0].Y
	}
	return -1, 0, 0
}
