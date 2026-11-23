package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"textureworkbench/internal/tex"
)

type PixelDiff struct {
	Stage      string `json:"stage"`
	X          int    `json:"x"`
	Y          int    `json:"y"`
	Channel    int    `json:"channel"`
	BlockIndex int    `json:"block_index"`
	Left       any    `json:"left"`
	Right      any    `json:"right"`
	Message    string `json:"message"`
}

type Comparison struct {
	ContentHash       string      `json:"content_hash"`
	DescriptorVersion string      `json:"descriptor_version"`
	AlgorithmVersion  string      `json:"algorithm_version"`
	Match             bool        `json:"match"`
	Paths             []PathStats `json:"paths"`
	FirstDiff         *PixelDiff  `json:"first_diff,omitempty"`
}

func DiffConverted(left, right *tex.Converted, blockWidth, blockHeight int) *PixelDiff {
	if left == nil || right == nil {
		return &PixelDiff{Stage: "result", X: -1, Y: -1, Channel: -1, BlockIndex: -1, Left: left != nil, Right: right != nil, Message: "one conversion result is missing"}
	}
	if left.Width != right.Width || left.Height != right.Height {
		return &PixelDiff{Stage: "dimensions", X: -1, Y: -1, Channel: -1, BlockIndex: -1,
			Left: fmt.Sprintf("%dx%d", left.Width, left.Height), Right: fmt.Sprintf("%dx%d", right.Width, right.Height),
			Message: "output dimensions differ"}
	}
	if left.BitDepth != right.BitDepth {
		return &PixelDiff{Stage: "bit_depth", X: -1, Y: -1, Channel: -1, BlockIndex: -1,
			Left: left.BitDepth, Right: right.BitDepth, Message: "target bit depth differs"}
	}
	if left.Layout != right.Layout {
		return &PixelDiff{Stage: "layout", X: -1, Y: -1, Channel: -1, BlockIndex: -1,
			Left: left.Layout, Right: right.Layout, Message: "target layout differs"}
	}
	if !reflect.DeepEqual(left.Mapping, right.Mapping) {
		return &PixelDiff{Stage: "channel_mapping", X: -1, Y: -1, Channel: -1, BlockIndex: -1,
			Left: left.Mapping, Right: right.Mapping, Message: "channel mapping differs"}
	}
	channels := left.Layout.ChannelCount()
	maxIndex := len(left.Pixels)
	if len(right.Pixels) < maxIndex {
		maxIndex = len(right.Pixels)
	}
	for i := 0; i < maxIndex; i++ {
		if left.Pixels[i] != right.Pixels[i] {
			return pixelDiffAt("pixel_quantization", i, channels, left.Width, blockWidth, blockHeight, left.Pixels[i], right.Pixels[i])
		}
	}
	if len(left.Pixels) != len(right.Pixels) {
		return &PixelDiff{Stage: "pixel_quantization", X: -1, Y: -1, Channel: -1, BlockIndex: -1,
			Left: len(left.Pixels), Right: len(right.Pixels), Message: "pixel buffer lengths differ"}
	}
	maxIndex = len(left.Errors)
	if len(right.Errors) < maxIndex {
		maxIndex = len(right.Errors)
	}
	for i := 0; i < maxIndex; i++ {
		if left.Errors[i] != right.Errors[i] {
			return pixelDiffAt("quantization_error", i, channels, left.Width, blockWidth, blockHeight, left.Errors[i], right.Errors[i])
		}
	}
	if len(left.Errors) != len(right.Errors) {
		return &PixelDiff{Stage: "quantization_error", X: -1, Y: -1, Channel: -1, BlockIndex: -1,
			Left: len(left.Errors), Right: len(right.Errors), Message: "quantization error buffer lengths differ"}
	}
	if !reflect.DeepEqual(left.QuantStats, right.QuantStats) {
		return &PixelDiff{Stage: "quantization_stats", X: -1, Y: -1, Channel: -1, BlockIndex: -1,
			Left: left.QuantStats, Right: right.QuantStats, Message: "quantization statistics differ"}
	}
	if !reflect.DeepEqual(left.Histogram, right.Histogram) {
		channels := len(left.Histogram.Counts)
		if len(right.Histogram.Counts) < channels {
			channels = len(right.Histogram.Counts)
		}
		for c := 0; c < channels; c++ {
			bins := len(left.Histogram.Counts[c])
			if len(right.Histogram.Counts[c]) < bins {
				bins = len(right.Histogram.Counts[c])
			}
			for b := 0; b < bins; b++ {
				if left.Histogram.Counts[c][b] != right.Histogram.Counts[c][b] {
					return &PixelDiff{Stage: "histogram", X: -1, Y: -1, Channel: c, BlockIndex: -1,
						Left: left.Histogram.Counts[c][b], Right: right.Histogram.Counts[c][b],
						Message: fmt.Sprintf("histogram channel %d bin %d differs", c, b)}
				}
			}
		}
		return &PixelDiff{Stage: "histogram", X: -1, Y: -1, Channel: -1, BlockIndex: -1,
			Left: left.Histogram, Right: right.Histogram, Message: "histogram metadata differs"}
	}
	return nil
}

func CompareConvertedOrError(left, right *tex.Converted, blockWidth, blockHeight int) error {
	diff := DiffConverted(left, right, blockWidth, blockHeight)
	if diff == nil {
		return nil
	}
	return &tex.Error{
		Code:    tex.CodeDiff,
		Message: "conversion results differ",
		Details: map[string]any{"diff": diff},
	}
}

func pixelDiffAt(stage string, sampleIndex, channels, width, blockWidth, blockHeight int, left, right any) *PixelDiff {
	if channels <= 0 {
		channels = 1
	}
	pixelIndex := sampleIndex / channels
	x, y := -1, -1
	if width > 0 {
		x = pixelIndex % width
		y = pixelIndex / width
	}
	blockIndex := -1
	if x >= 0 && y >= 0 && blockWidth > 0 && blockHeight > 0 {
		blocksPerRow := (width + blockWidth - 1) / blockWidth
		blockIndex = (y/blockHeight)*blocksPerRow + x/blockWidth
	}
	return &PixelDiff{
		Stage:      stage,
		X:          x,
		Y:          y,
		Channel:    sampleIndex % channels,
		BlockIndex: blockIndex,
		Left:       left,
		Right:      right,
		Message:    fmt.Sprintf("first difference in %s at sample %d", stage, sampleIndex),
	}
}

func CompareThreePaths(image *tex.Image, desc tex.Descriptor, cfg Config) (Comparison, error) {
	cfg = cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return Comparison{}, err
	}
	if err := desc.ValidateForImage(image); err != nil {
		return Comparison{}, err
	}
	tempRoot, err := os.MkdirTemp("", "twb-compare-*")
	if err != nil {
		return Comparison{}, err
	}
	defer os.RemoveAll(tempRoot)

	baseCfg := cfg
	baseCfg.CheckpointRoot = filepath.Join(tempRoot, "realtime")
	baseCfg.SessionKey = "realtime"
	realtime, err := OpenSession(image, desc, baseCfg)
	if err != nil {
		return Comparison{}, err
	}
	start := time.Now()
	if err := realtime.RunSegmented(RunOptions{}); err != nil {
		realtime.Close()
		return Comparison{}, err
	}
	realtimeResult, err := realtime.Result()
	realtimeSnapshot := realtime.Snapshot()
	realtime.Close()
	if err != nil {
		return Comparison{}, err
	}
	realtimeStats := realtimeSnapshot.Stats
	realtimeStats.Path = "realtime_segmented"
	realtimeStats.TotalTimeMS = float64(time.Since(start).Microseconds()) / 1000

	resumeCfg := cfg
	resumeCfg.CheckpointRoot = filepath.Join(tempRoot, "resume")
	resumeCfg.SessionKey = "resume"
	resumeStart := time.Now()
	first, err := OpenSession(image, desc, resumeCfg)
	if err != nil {
		return Comparison{}, err
	}
	firstTotal := first.Snapshot().Progress.Total
	firstLimit := firstTotal / 2
	if firstLimit < 1 && firstTotal > 1 {
		firstLimit = 1
	}
	if err := first.RunSegmented(RunOptions{MaxBlocks: firstLimit}); err != nil {
		first.Close()
		return Comparison{}, err
	}
	firstStats := first.Snapshot().Stats
	first.Close()

	second, err := OpenSession(image, desc, resumeCfg)
	if err != nil {
		return Comparison{}, err
	}
	if err := second.RunSegmented(RunOptions{}); err != nil {
		second.Close()
		return Comparison{}, err
	}
	resumeResult, err := second.Result()
	secondSnapshot := second.Snapshot()
	second.Close()
	if err != nil {
		return Comparison{}, err
	}
	resumeStats := secondSnapshot.Stats
	resumeStats.Path = "checkpoint_resume"
	resumeStats.BlockRecomputes += firstStats.BlockRecomputes
	resumeStats.SuccessfulCommits += firstStats.SuccessfulCommits
	resumeStats.FailedAttempts += firstStats.FailedAttempts
	resumeStats.BufferQueueWaits += firstStats.BufferQueueWaits
	resumeStats.ConversionTimeMS += firstStats.ConversionTimeMS
	resumeStats.CheckpointTimeMS += firstStats.CheckpointTimeMS
	resumeStats.CacheHits = int64(secondSnapshot.Restore.RestoredBlocks)
	resumeStats.TotalTimeMS = float64(time.Since(resumeStart).Microseconds()) / 1000

	fullStart := time.Now()
	fullResult, err := tex.ConvertFull(image, desc, tex.ConvertOptions{})
	if err != nil {
		return Comparison{}, err
	}
	fullDuration := float64(time.Since(fullStart).Microseconds()) / 1000
	fullStats := PathStats{
		Path:              "full_recompute",
		BlockRecomputes:   int64(len(realtimeSnapshot.Blocks)),
		SuccessfulCommits: int64(len(realtimeSnapshot.Blocks)),
		ConversionTimeMS:  fullDuration,
		TotalTimeMS:       fullDuration,
	}
	comparison := Comparison{
		ContentHash:       image.ContentHash,
		DescriptorVersion: desc.Version(),
		AlgorithmVersion:  tex.AlgorithmVersion,
		Paths:             []PathStats{realtimeStats, resumeStats, fullStats},
	}
	diff := DiffConverted(realtimeResult, fullResult, cfg.BlockWidth, cfg.BlockHeight)
	if diff == nil {
		diff = DiffConverted(resumeResult, fullResult, cfg.BlockWidth, cfg.BlockHeight)
	}
	if diff != nil {
		comparison.Match = false
		comparison.FirstDiff = diff
		return comparison, &tex.Error{
			Code:    tex.CodeDiff,
			Message: "the segmented, resumed and full conversion paths produced different results",
			Details: map[string]any{"diff": diff, "comparison": comparison},
		}
	}
	comparison.Match = true
	return comparison, nil
}
