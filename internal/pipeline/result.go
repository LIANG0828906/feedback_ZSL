package pipeline

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"ocrworkbench/internal/core"
)

func BuildStats(tiles []core.TileResult, lines []core.LineResult) (core.Stats, core.Progress) {
	stats := core.Stats{LineCount: len(lines)}
	minConfidence := math.Inf(1)
	maxConfidence := math.Inf(-1)
	confidenceSum := 0.0
	for _, line := range lines {
		if line.State == "committed" {
			stats.CommittedLines++
		}
		if line.IsEmpty {
			stats.EmptyLineCount++
		}
		stats.CharacterCount += len(line.Characters)
		minConfidence = math.Min(minConfidence, line.Confidence)
		maxConfidence = math.Max(maxConfidence, line.Confidence)
		confidenceSum += line.Confidence
	}
	for _, tile := range tiles {
		if tile.State == "committed" {
			stats.CommittedTiles++
		}
	}
	if len(lines) > 0 {
		stats.AverageConfidence = confidenceSum / float64(len(lines))
	}
	if math.IsInf(minConfidence, 1) {
		minConfidence = 0
	}
	if math.IsInf(maxConfidence, -1) {
		maxConfidence = 0
	}
	stats.MinConfidence = minConfidence
	stats.MaxConfidence = maxConfidence
	progress := core.Progress{
		CommittedTiles: stats.CommittedTiles,
		TotalTiles:     len(tiles),
		CommittedLines: stats.CommittedLines,
		TotalLines:     len(lines),
	}
	return stats, progress
}

func CommitSequence(tiles []core.TileResult, lines []core.LineResult) core.CommitSequence {
	tileSeq := make([]int, 0, len(tiles))
	lineSeq := make([]int, 0, len(lines))
	for _, tile := range tiles {
		if tile.State == "committed" {
			tileSeq = append(tileSeq, tile.Index)
		}
	}
	for _, line := range lines {
		if line.State == "committed" {
			lineSeq = append(lineSeq, line.Number)
		}
	}
	sort.Ints(tileSeq)
	sort.Ints(lineSeq)
	return core.CommitSequence{Tiles: tileSeq, Lines: lineSeq}
}

func AttachTruth(result *core.ImageResult, truth core.ImageTruth) {
	for i := range result.Lines {
		if i < len(truth.Regions) {
			result.Lines[i].TruthText = truth.Regions[i].Text
			result.Lines[i].TruthIoU = core.BoxIoU(result.Lines[i].Box, truth.Regions[i].Box)
		}
	}
}

type Difference struct {
	ImageID string
	Line    int
	Field   string
	Left    string
	Right   string
}

func (d Difference) String() string {
	return fmt.Sprintf("image %s line %d field %s: %s != %s", d.ImageID, d.Line, d.Field, d.Left, d.Right)
}

// CompareRuns checks the fields required to be identical across realtime,
// resumed and full-recompute paths. RunKind is intentionally ignored.
func CompareRuns(left, right core.RunResult) []Difference {
	var differences []Difference
	if left.TemplateVersion != right.TemplateVersion {
		differences = append(differences, Difference{Field: "template_version", Left: left.TemplateVersion, Right: right.TemplateVersion})
	}
	if left.ParameterVersion != right.ParameterVersion {
		differences = append(differences, Difference{Field: "parameter_version", Left: left.ParameterVersion, Right: right.ParameterVersion})
	}
	if left.AlgorithmVersion != right.AlgorithmVersion {
		differences = append(differences, Difference{Field: "algorithm_version", Left: left.AlgorithmVersion, Right: right.AlgorithmVersion})
	}
	if len(left.Images) != len(right.Images) {
		differences = append(differences, Difference{Field: "image_count", Left: fmt.Sprint(len(left.Images)), Right: fmt.Sprint(len(right.Images))})
		return differences
	}
	for i := range left.Images {
		a, b := left.Images[i], right.Images[i]
		if a.ImageID != b.ImageID {
			differences = append(differences, Difference{ImageID: a.ImageID, Field: "image_id", Left: a.ImageID, Right: b.ImageID})
			continue
		}
		if len(a.Tiles) != len(b.Tiles) {
			differences = append(differences, Difference{ImageID: a.ImageID, Field: "tile_count", Left: fmt.Sprint(len(a.Tiles)), Right: fmt.Sprint(len(b.Tiles))})
		} else {
			for j := range a.Tiles {
				at, bt := a.Tiles[j], b.Tiles[j]
				if at.Index != bt.Index || at.State != bt.State || at.ContentHash != bt.ContentHash {
					differences = append(differences, Difference{ImageID: a.ImageID, Line: at.Index, Field: "tile_unit", Left: fmt.Sprintf("%d/%s/%s", at.Index, at.State, at.ContentHash), Right: fmt.Sprintf("%d/%s/%s", bt.Index, bt.State, bt.ContentHash)})
				}
			}
		}
		if len(a.Lines) != len(b.Lines) {
			differences = append(differences, Difference{ImageID: a.ImageID, Field: "line_count", Left: fmt.Sprint(len(a.Lines)), Right: fmt.Sprint(len(b.Lines))})
			continue
		}
		for j := range a.Lines {
			al, bl := a.Lines[j], b.Lines[j]
			if al.ReadingIndex != bl.ReadingIndex {
				differences = append(differences, Difference{ImageID: a.ImageID, Line: al.ReadingIndex, Field: "reading_index", Left: fmt.Sprint(al.ReadingIndex), Right: fmt.Sprint(bl.ReadingIndex)})
			}
			if al.State != bl.State {
				differences = append(differences, Difference{ImageID: a.ImageID, Line: al.ReadingIndex, Field: "state", Left: al.State, Right: bl.State})
			}
			if al.Text != bl.Text {
				differences = append(differences, Difference{ImageID: a.ImageID, Line: al.ReadingIndex, Field: "text", Left: al.Text, Right: bl.Text})
			}
			if al.Confidence != bl.Confidence {
				differences = append(differences, Difference{ImageID: a.ImageID, Line: al.ReadingIndex, Field: "confidence", Left: fmt.Sprintf("%.17g", al.Confidence), Right: fmt.Sprintf("%.17g", bl.Confidence)})
			}
			if len(al.Characters) != len(bl.Characters) {
				differences = append(differences, Difference{ImageID: a.ImageID, Line: al.ReadingIndex, Field: "character_count", Left: fmt.Sprint(len(al.Characters)), Right: fmt.Sprint(len(bl.Characters))})
				continue
			}
			for k := range al.Characters {
				ac, bc := al.Characters[k], bl.Characters[k]
				if ac.Text != bc.Text || ac.Confidence != bc.Confidence {
					differences = append(differences, Difference{ImageID: a.ImageID, Line: al.ReadingIndex, Field: fmt.Sprintf("char[%d]", k), Left: fmt.Sprintf("%s/%.17g", ac.Text, ac.Confidence), Right: fmt.Sprintf("%s/%.17g", bc.Text, bc.Confidence)})
				}
			}
			if core.BoxIoU(al.Box, bl.Box) < 1-1e-12 {
				differences = append(differences, Difference{ImageID: a.ImageID, Line: al.ReadingIndex, Field: "box", Left: boxString(al.Box), Right: boxString(bl.Box)})
			}
			if al.ContentHash != bl.ContentHash {
				differences = append(differences, Difference{ImageID: a.ImageID, Line: al.ReadingIndex, Field: "content_hash", Left: al.ContentHash, Right: bl.ContentHash})
			}
		}
		if !sameInts(a.Commits.Tiles, b.Commits.Tiles) {
			differences = append(differences, Difference{ImageID: a.ImageID, Field: "tile_commit_sequence", Left: intsString(a.Commits.Tiles), Right: intsString(b.Commits.Tiles)})
		}
		if !sameInts(a.Commits.Lines, b.Commits.Lines) {
			differences = append(differences, Difference{ImageID: a.ImageID, Field: "line_commit_sequence", Left: intsString(a.Commits.Lines), Right: intsString(b.Commits.Lines)})
		}
		if a.Stats != b.Stats {
			differences = append(differences, Difference{ImageID: a.ImageID, Field: "stats", Left: fmt.Sprintf("%+v", a.Stats), Right: fmt.Sprintf("%+v", b.Stats)})
		}
	}
	return differences
}

func boxString(box core.Box) string {
	var parts []string
	for _, point := range box {
		parts = append(parts, fmt.Sprintf("(%.9f,%.9f)", point.X, point.Y))
	}
	return strings.Join(parts, " ")
}

func intsString(values []int) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = fmt.Sprint(value)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
