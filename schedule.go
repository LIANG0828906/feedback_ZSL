package lipsync

import (
	"fmt"
	"math/big"
	"sort"
)

type CharSpan struct {
	CharStart  int
	CharEnd    int
	StartFrame int
	EndFrame   int
	Weight     int
}

type SubtitleSchedule struct {
	Frames           []CharSpan
	Spans            []CharSpan
	Compressed       bool
	EffectiveMinimum int
	Priority         string
}

type allocation struct {
	index     int
	weight    int
	remainder *big.Int
}

// allocateWithMinimum distributes total frames by weight while granting each
// item at least minimum frames. It uses integer Hamilton allocation with a
// stable tie-break: larger weight, then lower index.
func allocateWithMinimum(total int, weights []int, minimum int) ([]int, bool, error) {
	n := len(weights)
	if n == 0 || total <= 0 {
		return nil, false, fmt.Errorf("total and weights must be positive")
	}
	compressed := false
	if minimum < 1 {
		minimum = 1
	}
	if n > total {
		return nil, false, fmt.Errorf("fewer frames than items")
	}
	if minimum > total/n {
		minimum = 1
		compressed = true
	}
	out := make([]int, n)
	for i := range out {
		out[i] = minimum
	}
	remaining := total - n*minimum
	if remaining == 0 {
		return out, compressed, nil
	}
	sum := big.NewInt(0)
	for _, w := range weights {
		if w <= 0 {
			return nil, false, fmt.Errorf("weight must be positive")
		}
		sum.Add(sum, big.NewInt(int64(w)))
	}
	parts := make([]allocation, n)
	assigned := 0
	for i, w := range weights {
		numerator := new(big.Int).Mul(big.NewInt(int64(remaining)), big.NewInt(int64(w)))
		q, r := new(big.Int).QuoRem(numerator, sum, new(big.Int))
		qInt := int(q.Int64())
		out[i] += qInt
		assigned += qInt
		parts[i] = allocation{index: i, weight: w, remainder: r}
	}
	left := remaining - assigned
	sort.SliceStable(parts, func(i, j int) bool {
		if parts[i].remainder.Cmp(parts[j].remainder) != 0 {
			return parts[i].remainder.Cmp(parts[j].remainder) > 0
		}
		if parts[i].weight != parts[j].weight {
			return parts[i].weight > parts[j].weight
		}
		return parts[i].index < parts[j].index
	})
	for i := 0; i < left; i++ {
		out[parts[i%n].index]++
	}
	return out, compressed, nil
}

// ScheduleSubtitle assigns one rune interval to every frame. The intervals are
// contiguous, non-overlapping, and cover the full subtitle. If the declared
// minimum cannot fit, every rune keeps one frame and the remaining hold frames
// are compressed by weight priority.
func ScheduleSubtitle(p DialogueProject, frameCount int) (SubtitleSchedule, error) {
	runeCount := p.RuneCount()
	if runeCount == 0 {
		return SubtitleSchedule{}, &DomainError{
			Code:       CodeSchema,
			DialogueID: p.DialogueID,
			Field:      "subtitle.text",
			Message:    "subtitle.text must not be empty",
		}
	}
	if len(p.Subtitle.Weights) != runeCount {
		return SubtitleSchedule{}, &DomainError{
			Code:       CodeSchema,
			DialogueID: p.DialogueID,
			Field:      "subtitle.weights",
			Message:    fmt.Sprintf("subtitle has %d characters but %d weights", runeCount, len(p.Subtitle.Weights)),
		}
	}
	for i, weight := range p.Subtitle.Weights {
		if weight <= 0 {
			return SubtitleSchedule{}, &DomainError{
				Code:       CodeSchema,
				DialogueID: p.DialogueID,
				Field:      fmt.Sprintf("subtitle.weights[%d]", i),
				Message:    "subtitle weight must be positive",
			}
		}
	}
	if frameCount < runeCount {
		return SubtitleSchedule{}, &DomainError{
			Code:       CodeSched,
			DialogueID: p.DialogueID,
			Frame:      intPtr(frameCount),
			Field:      "subtitle",
			Reason:     "fewer frames than subtitle characters",
			Message:    fmt.Sprintf("%d frames cannot cover %d subtitle characters", frameCount, runeCount),
		}
	}
	durations, compressed, err := allocateWithMinimum(frameCount, p.Subtitle.Weights, p.MinReadableFrames)
	if err != nil {
		return SubtitleSchedule{}, &DomainError{
			Code:       CodeSched,
			DialogueID: p.DialogueID,
			Frame:      intPtr(0),
			Field:      "subtitle.weights",
			Reason:     "allocation conflict",
			Message:    "subtitle allocation could not be resolved",
			Cause:      err.Error(),
		}
	}
	effectiveMinimum := p.MinReadableFrames
	priority := "declared_minimum"
	if compressed {
		effectiveMinimum = 1
		priority = "all_characters_then_weight_descending"
	}
	spans := make([]CharSpan, 0, runeCount)
	frames := make([]CharSpan, 0, frameCount)
	frameCursor := 0
	total := 0
	for i, duration := range durations {
		if duration < effectiveMinimum {
			return SubtitleSchedule{}, &DomainError{
				Code:       CodeSched,
				DialogueID: p.DialogueID,
				Frame:      intPtr(frameCursor),
				Field:      "subtitle.weights",
				Reason:     "minimum readable frames violated",
				Message:    "subtitle allocation violated the effective minimum",
			}
		}
		span := CharSpan{
			CharStart:  i,
			CharEnd:    i + 1,
			StartFrame: frameCursor,
			EndFrame:   frameCursor + duration,
			Weight:     p.Subtitle.Weights[i],
		}
		spans = append(spans, span)
		for f := span.StartFrame; f < span.EndFrame; f++ {
			frames = append(frames, span)
		}
		frameCursor += duration
		total += duration
	}
	if len(frames) != frameCount || total != frameCount {
		return SubtitleSchedule{}, &DomainError{
			Code:       CodeSched,
			DialogueID: p.DialogueID,
			Frame:      intPtr(len(frames)),
			Field:      "subtitle",
			Reason:     "frame coverage mismatch",
			Message:    "subtitle schedule does not cover the full dialogue",
		}
	}
	for i := 1; i < len(spans); i++ {
		if spans[i].StartFrame != spans[i-1].EndFrame || spans[i].CharStart != spans[i-1].CharEnd {
			return SubtitleSchedule{}, &DomainError{
				Code:       CodeSched,
				DialogueID: p.DialogueID,
				Frame:      intPtr(spans[i].StartFrame),
				Field:      "subtitle.char_interval",
				Reason:     "non-contiguous character interval",
				Message:    "subtitle character intervals must be contiguous",
			}
		}
	}
	return SubtitleSchedule{
		Frames:           frames,
		Spans:            spans,
		Compressed:       compressed,
		EffectiveMinimum: effectiveMinimum,
		Priority:         priority,
	}, nil
}
