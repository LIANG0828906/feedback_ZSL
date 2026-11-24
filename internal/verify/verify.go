package verify

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"ocrworkbench/internal/core"
	"ocrworkbench/internal/pipeline"
	"ocrworkbench/internal/store"
)

type Issue struct {
	ImageID string `json:"image_id"`
	Line    int    `json:"line"`
	Field   string `json:"field"`
	Detail  string `json:"detail"`
}

type ImageMetrics struct {
	ImageID           string  `json:"image_id"`
	LineCount         int     `json:"line_count"`
	TruthLines        int     `json:"truth_lines"`
	MinIoU            float64 `json:"min_iou"`
	AverageIoU        float64 `json:"average_iou"`
	MinConfidence     float64 `json:"min_confidence"`
	AverageConfidence float64 `json:"average_confidence"`
	MaxConfidence     float64 `json:"max_confidence"`
	EmptyLines        int     `json:"empty_lines"`
}

func Summarize(run core.RunResult, manifest core.Manifest) []ImageMetrics {
	truthByID := map[string]core.ImageTruth{}
	for _, truth := range manifest.Images {
		truthByID[truth.ID] = truth
	}
	metrics := make([]ImageMetrics, 0, len(run.Images))
	for _, image := range run.Images {
		truth := truthByID[image.ImageID]
		item := ImageMetrics{
			ImageID:    image.ImageID,
			LineCount:  len(image.Lines),
			TruthLines: len(truth.Regions),
			MinIoU:     1,
		}
		iouSum := 0.0
		iouCount := 0
		confidenceSum := 0.0
		item.MinConfidence = 1
		for i, line := range image.Lines {
			if line.IsEmpty {
				item.EmptyLines++
			}
			if i < len(truth.Regions) {
				iou := core.BoxIoU(line.Box, truth.Regions[i].Box)
				item.MinIoU = math.Min(item.MinIoU, iou)
				iouSum += iou
				iouCount++
			}
			item.MinConfidence = math.Min(item.MinConfidence, line.Confidence)
			item.MaxConfidence = math.Max(item.MaxConfidence, line.Confidence)
			confidenceSum += line.Confidence
		}
		if iouCount > 0 {
			item.AverageIoU = iouSum / float64(iouCount)
		} else {
			item.MinIoU = 0
		}
		if len(image.Lines) > 0 {
			item.AverageConfidence = confidenceSum / float64(len(image.Lines))
		} else {
			item.MinConfidence = 0
		}
		metrics = append(metrics, item)
	}
	return metrics
}

func (i Issue) Error() string {
	return fmt.Sprintf("image %s line %d field %s: %s", i.ImageID, i.Line, i.Field, i.Detail)
}

func LoadRun(path string) (core.RunResult, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return core.RunResult{}, err
	}
	var run core.RunResult
	if err := json.Unmarshal(b, &run); err != nil {
		return core.RunResult{}, err
	}
	return run, nil
}

func WriteRun(path string, run core.RunResult) error {
	b, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func VerifyRun(run core.RunResult, manifest core.Manifest) []Issue {
	expectedFingerprint := manifest.TruthFingerprint
	if expectedFingerprint == "" {
		expectedFingerprint, _ = core.TruthFingerprint(manifest)
	}
	var issues []Issue
	if len(run.Images) != len(manifest.Images) {
		issues = append(issues, Issue{Field: "image_count", Detail: fmt.Sprintf("persisted %d images, truth declares %d", len(run.Images), len(manifest.Images))})
	}
	truthByID := map[string]core.ImageTruth{}
	for _, image := range manifest.Images {
		truthByID[image.ID] = image
	}
	for _, image := range run.Images {
		truth, ok := truthByID[image.ImageID]
		if !ok {
			issues = append(issues, Issue{ImageID: image.ImageID, Field: "image_id", Detail: "image not present in truth manifest"})
			continue
		}
		imageIssues := verifyImage(image, truth, expectedFingerprint)
		issues = append(issues, imageIssues...)
	}
	return issues
}

func verifyImage(result core.ImageResult, truth core.ImageTruth, expectedFingerprint string) []Issue {
	var issues []Issue
	add := func(line int, field, detail string) {
		issues = append(issues, Issue{ImageID: result.ImageID, Line: line, Field: field, Detail: detail})
	}
	if result.TruthFingerprint != expectedFingerprint {
		add(-1, "truth_fingerprint", fmt.Sprintf("persisted %s, expected %s", result.TruthFingerprint, expectedFingerprint))
	}
	tolerance := truth.DetectionTolerance
	if tolerance < 0 {
		tolerance = 0
	}
	if absInt(len(result.Lines)-len(truth.Regions)) > tolerance {
		firstDiverging := len(result.Lines)
		if firstDiverging > len(truth.Regions) {
			firstDiverging = len(truth.Regions)
		}
		add(firstDiverging, "line_count", fmt.Sprintf("persisted %d lines, truth declares %d (tolerance %d)", len(result.Lines), len(truth.Regions), tolerance))
	}
	tileCommitted := 0
	for i, tile := range result.Tiles {
		if tile.Index != i {
			add(-1, "tile_order", fmt.Sprintf("tile slot %d has index %d", i, tile.Index))
		}
		if tile.State == "committed" {
			tileCommitted++
			if tile.ContentHash == "" {
				add(-1, "tile_hash", fmt.Sprintf("committed tile %d has no content hash", i))
			}
		}
	}
	for i, line := range result.Lines {
		if line.ReadingIndex != i {
			add(i, "reading_index", fmt.Sprintf("stored %d, expected %d", line.ReadingIndex, i))
		}
		if line.Number != i {
			add(i, "line_number", fmt.Sprintf("stored %d, expected %d", line.Number, i))
		}
		if !core.ValidBox(line.Box) {
			add(i, "box", "non-finite or degenerate persisted box")
		}
		if line.Confidence < 0 || line.Confidence > 1 || math.IsNaN(line.Confidence) {
			add(i, "confidence", fmt.Sprintf("line confidence %.6f outside [0,1]", line.Confidence))
		}
		if len(line.Characters) == 0 {
			if !line.IsEmpty {
				add(i, "empty_line", "zero characters must be explicitly marked empty")
			}
			if line.Text != "" {
				add(i, "text", "empty line must persist empty text")
			}
			if line.Confidence != 0 {
				add(i, "empty_confidence", "empty line confidence must be zero")
			}
		} else {
			recomputedText := ""
			confidenceSum := 0.0
			for charIndex, char := range line.Characters {
				recomputedText += char.Text
				if char.Confidence < 0 || char.Confidence > 1 || math.IsNaN(char.Confidence) {
					add(i, fmt.Sprintf("char[%d].confidence", charIndex), fmt.Sprintf("%.6f outside [0,1]", char.Confidence))
				}
				confidenceSum += char.Confidence
			}
			if recomputedText != line.Text {
				add(i, "text", fmt.Sprintf("line text %q differs from character sequence %q", line.Text, recomputedText))
			}
			recomputedConfidence := confidenceSum / float64(len(line.Characters))
			if math.Abs(recomputedConfidence-line.Confidence) > 1e-12 {
				add(i, "confidence_average", fmt.Sprintf("stored %.17g, recomputed %.17g", line.Confidence, recomputedConfidence))
			}
		}
		if i < len(truth.Regions) {
			region := truth.Regions[i]
			iou := core.BoxIoU(line.Box, region.Box)
			if iou < truth.MinDetectionIoU {
				add(i, "truth_iou", fmt.Sprintf("IoU %.6f below %.6f", iou, truth.MinDetectionIoU))
			}
			if line.Text != region.Text {
				add(i, "truth_text", fmt.Sprintf("recognized %q, truth %q", line.Text, region.Text))
			}
		}
	}
	recomputedStats := recomputeStats(result.Lines, tileCommitted)
	if !sameStats(recomputedStats, result.Stats) {
		add(-1, "confidence_distribution", fmt.Sprintf("stored %+v, recomputed %+v", result.Stats, recomputedStats))
	}
	if result.Progress.CommittedTiles != tileCommitted || result.Progress.CommittedLines != committedLines(result.Lines) {
		add(-1, "progress", fmt.Sprintf("stored progress %+v does not match committed units", result.Progress))
	}
	if !sortedUnique(result.Commits.Tiles) || !sortedUnique(result.Commits.Lines) {
		add(-1, "commit_sequence", "tile or line commit sequence is not strictly increasing")
	}
	return issues
}

func recomputeStats(lines []core.LineResult, committedTiles int) core.Stats {
	stats := core.Stats{LineCount: len(lines), CommittedTiles: committedTiles}
	minConfidence, maxConfidence := math.Inf(1), math.Inf(-1)
	sum := 0.0
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
		sum += line.Confidence
	}
	if len(lines) > 0 {
		stats.AverageConfidence = sum / float64(len(lines))
	}
	if math.IsInf(minConfidence, 1) {
		minConfidence = 0
	}
	if math.IsInf(maxConfidence, -1) {
		maxConfidence = 0
	}
	stats.MinConfidence = minConfidence
	stats.MaxConfidence = maxConfidence
	return stats
}

func sameStats(a, b core.Stats) bool {
	return a.LineCount == b.LineCount &&
		a.EmptyLineCount == b.EmptyLineCount &&
		a.CharacterCount == b.CharacterCount &&
		a.CommittedTiles == b.CommittedTiles &&
		a.CommittedLines == b.CommittedLines &&
		math.Abs(a.AverageConfidence-b.AverageConfidence) <= 1e-12 &&
		math.Abs(a.MinConfidence-b.MinConfidence) <= 1e-12 &&
		math.Abs(a.MaxConfidence-b.MaxConfidence) <= 1e-12
}

func committedLines(lines []core.LineResult) int {
	count := 0
	for _, line := range lines {
		if line.State == "committed" {
			count++
		}
	}
	return count
}

func sortedUnique(values []int) bool {
	if len(values) == 0 {
		return true
	}
	previous := values[0]
	for _, value := range values[1:] {
		if value <= previous {
			return false
		}
		previous = value
	}
	return true
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func SnapshotRun(snapshot *store.Snapshot, truth core.ImageTruth) core.ImageResult {
	result := core.ImageResult{
		ImageID:          snapshot.Key.ImageID,
		TruthFingerprint: snapshot.Key.TruthHash,
		ImageHash:        snapshot.Key.ContentHash,
		TemplateVersion:  snapshot.Key.TemplateVersion,
		ParameterVersion: snapshot.Key.ParameterVersion,
		AlgorithmVersion: snapshot.Key.AlgorithmVersion,
		Tiles:            append([]core.TileResult(nil), snapshot.Tiles...),
		Lines:            append([]core.LineResult(nil), snapshot.Lines...),
	}
	sort.Slice(result.Lines, func(i, j int) bool { return result.Lines[i].Number < result.Lines[j].Number })
	stats, progress := statsForSnapshot(result.Tiles, result.Lines)
	result.Stats = stats
	result.Progress = progress
	result.Commits = core.CommitSequence{}
	for _, tile := range result.Tiles {
		result.Commits.Tiles = append(result.Commits.Tiles, tile.Index)
	}
	for _, line := range result.Lines {
		result.Commits.Lines = append(result.Commits.Lines, line.Number)
	}
	_ = truth
	return result
}

func statsForSnapshot(tiles []core.TileResult, lines []core.LineResult) (core.Stats, core.Progress) {
	return pipeline.BuildStats(tiles, lines)
}

func CompareRuns(left, right core.RunResult) []Issue {
	differences := pipeline.CompareRuns(left, right)
	issues := make([]Issue, 0, len(differences))
	for _, difference := range differences {
		issues = append(issues, Issue{
			ImageID: difference.ImageID,
			Line:    difference.Line,
			Field:   difference.Field,
			Detail:  fmt.Sprintf("%s != %s", difference.Left, difference.Right),
		})
	}
	return issues
}

func FormatIssues(issues []Issue) string {
	if len(issues) == 0 {
		return "ok"
	}
	var parts []string
	for _, issue := range issues {
		parts = append(parts, issue.Error())
	}
	return strings.Join(parts, "\n")
}
