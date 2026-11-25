package vpcompose

import (
	"fmt"
	"sort"
)

type ComparableRun struct {
	Name      string                `json:"name"`
	Committed []int                 `json:"committed"`
	Results   map[int]*WindowResult `json:"results"`
	States    map[int]WindowState   `json:"states"`
}

func (e *Engine) Comparable(name string) (*ComparableRun, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	run := &ComparableRun{
		Name: name, Committed: committedIndexes(e.windows),
		Results: make(map[int]*WindowResult), States: make(map[int]WindowState),
	}
	for _, window := range e.windows {
		run.States[window.Index] = window.State
		if window.State == WindowCommitted {
			run.Results[window.Index] = cloneWindowResult(window.Result)
		}
	}
	return run, nil
}

func cloneWindowResult(result *WindowResult) *WindowResult {
	if result == nil {
		return nil
	}
	clone := &WindowResult{
		WindowIndex:  result.WindowIndex,
		FrameDigests: append([]string(nil), result.FrameDigests...),
		SampleStats:  append([]SampleStats(nil), result.SampleStats...),
		Digest:       result.Digest,
	}
	for _, frame := range result.Frames {
		if frame == nil {
			clone.Frames = append(clone.Frames, nil)
			continue
		}
		frameClone := *frame
		frameClone.Pixels = append([]uint16(nil), frame.Pixels...)
		clone.Frames = append(clone.Frames, &frameClone)
	}
	return clone
}

func CompareRuns(runs ...*ComparableRun) error {
	if len(runs) < 2 {
		return nil
	}
	base := runs[0]
	maxWindow := 0
	for _, run := range runs {
		if run == nil {
			return schemaError("run", "nil comparable run")
		}
		seen := make(map[int]bool)
		for _, index := range run.Committed {
			seen[index] = true
			if index > maxWindow {
				maxWindow = index
			}
		}
		for index := range run.States {
			if index > maxWindow {
				maxWindow = index
			}
		}
	}
	for index := 0; index <= maxWindow; index++ {
		baseState, baseOK := base.States[index]
		for _, other := range runs[1:] {
			otherState, otherOK := other.States[index]
			if baseOK != otherOK || baseState != otherState {
				return diffError(index, "windowState", base.Name, baseState, other.Name, otherState)
			}
			baseResult, baseResultOK := base.Results[index]
			otherResult, otherResultOK := other.Results[index]
			if baseResultOK != otherResultOK {
				return diffError(index, "resultPresence", base.Name, baseResultOK, other.Name, otherResultOK)
			}
			if !baseResultOK {
				continue
			}
			if err := compareWindowResults(index, base.Name, baseResult, other.Name, otherResult); err != nil {
				return err
			}
		}
	}
	for _, other := range runs[1:] {
		if len(base.Committed) != len(other.Committed) {
			return diffError(0, "commitSequence", base.Name, base.Committed, other.Name, other.Committed)
		}
		for i := range base.Committed {
			if base.Committed[i] != other.Committed[i] {
				return diffError(base.Committed[i], "commitSequence", base.Name, base.Committed, other.Name, other.Committed)
			}
		}
	}
	return nil
}

func compareWindowResults(index int, baseName string, base *WindowResult, otherName string, other *WindowResult) error {
	if len(base.Frames) != len(other.Frames) {
		return diffError(index, "frames.length", baseName, len(base.Frames), otherName, len(other.Frames))
	}
	for frameIndex := range base.Frames {
		a, b := base.Frames[frameIndex], other.Frames[frameIndex]
		if a == nil || b == nil {
			if a != b {
				return diffError(index, fmt.Sprintf("frames[%d]", frameIndex), baseName, a == nil, otherName, b == nil)
			}
			continue
		}
		if a.Width != b.Width {
			return diffError(index, fmt.Sprintf("frames[%d].width", frameIndex), baseName, a.Width, otherName, b.Width)
		}
		if a.Height != b.Height {
			return diffError(index, fmt.Sprintf("frames[%d].height", frameIndex), baseName, a.Height, otherName, b.Height)
		}
		if a.Channels != b.Channels {
			return diffError(index, fmt.Sprintf("frames[%d].channels", frameIndex), baseName, a.Channels, otherName, b.Channels)
		}
		if a.TimeTicks != b.TimeTicks {
			return diffError(index, fmt.Sprintf("frames[%d].timeTick", frameIndex), baseName, a.TimeTicks, otherName, b.TimeTicks)
		}
		if a.Timebase != b.Timebase {
			return diffError(index, fmt.Sprintf("frames[%d].timebase", frameIndex), baseName, a.Timebase, otherName, b.Timebase)
		}
		if a.AlphaMode != b.AlphaMode {
			return diffError(index, fmt.Sprintf("frames[%d].alphaMode", frameIndex), baseName, a.AlphaMode, otherName, b.AlphaMode)
		}
		for offset := 0; offset < minInt(len(a.Pixels), len(b.Pixels)); offset++ {
			if a.Pixels[offset] != b.Pixels[offset] {
				channel := offset % a.Channels
				pixel := offset / a.Channels
				return diffError(index,
					fmt.Sprintf("frames[%d].pixels[%d].channel%d", frameIndex, pixel, channel),
					baseName, a.Pixels[offset], otherName, b.Pixels[offset])
			}
		}
		if len(a.Pixels) != len(b.Pixels) {
			return diffError(index, fmt.Sprintf("frames[%d].pixels.length", frameIndex), baseName, len(a.Pixels), otherName, len(b.Pixels))
		}
	}
	if len(base.SampleStats) != len(other.SampleStats) {
		return diffError(index, "sampleStats.length", baseName, len(base.SampleStats), otherName, len(other.SampleStats))
	}
	for i := range base.SampleStats {
		if base.SampleStats[i] != other.SampleStats[i] {
			if base.SampleStats[i].Sum != other.SampleStats[i].Sum {
				return diffError(index, fmt.Sprintf("sampleStats[%d].sum", i), baseName, base.SampleStats[i].Sum, otherName, other.SampleStats[i].Sum)
			}
			if base.SampleStats[i].Digest != other.SampleStats[i].Digest {
				return diffError(index, fmt.Sprintf("sampleStats[%d].digest", i), baseName, base.SampleStats[i].Digest, otherName, other.SampleStats[i].Digest)
			}
			return diffError(index, fmt.Sprintf("sampleStats[%d]", i), baseName, base.SampleStats[i], otherName, other.SampleStats[i])
		}
	}
	if base.Digest != other.Digest {
		return diffError(index, "window.digest", baseName, base.Digest, otherName, other.Digest)
	}
	return nil
}

func diffError(window int, field, baseName string, baseValue any, otherName string, otherValue any) error {
	return &EngineError{
		Code: CodeDiff, Window: window, Field: field,
		Message:  fmt.Sprintf("%s and %s differ", baseName, otherName),
		Expected: baseValue, Actual: otherValue,
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func FirstUncommittedWindow(committed []int, total int) int {
	seen := make(map[int]bool, len(committed))
	for _, index := range committed {
		seen[index] = true
	}
	for index := 0; index < total; index++ {
		if !seen[index] {
			return index
		}
	}
	return total
}

func SortedWindowIDs(results map[int]*WindowResult) []int {
	ids := make([]int, 0, len(results))
	for id := range results {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}
