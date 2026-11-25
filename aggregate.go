package audiofp

import (
	"fmt"
	"math"
	"sort"
)

type CategoryScore struct {
	Category string  `json:"category"`
	Score    float64 `json:"score"`
}

type FrameScore struct {
	FrameIndex   int             `json:"frame_index"`
	StartSample  int             `json:"start_sample"`
	Energy       byte            `json:"energy"`
	BestCategory string          `json:"best_category,omitempty"`
	BestScore    float64         `json:"best_score"`
	Hit          bool            `json:"hit"`
	Scores       []CategoryScore `json:"scores"`
}

type Interval struct {
	ID             string  `json:"id"`
	Category       string  `json:"category"`
	StartSample    int     `json:"start_sample"`
	EndSample      int     `json:"end_sample"`
	FirstFrame     int     `json:"first_frame"`
	LastFrame      int     `json:"last_frame"`
	HitFrames      int     `json:"hit_frames"`
	SpanFrames     int     `json:"span_frames"`
	GapFrames      int     `json:"gap_frames"`
	MeanSimilarity float64 `json:"mean_similarity"`
	Confidence     float64 `json:"confidence"`
}

func ScoreFrames(frames []Fingerprint, templates []Template, params Params) []FrameScore {
	out := make([]FrameScore, 0, len(frames))
	for _, frame := range frames {
		score := FrameScore{
			FrameIndex:  frame.FrameIndex,
			StartSample: frame.StartSample,
			Energy:      frame.Energy,
			Scores:      make([]CategoryScore, 0, len(templates)),
		}
		best := -1.0
		for _, template := range templates {
			value := FingerprintSimilarity(frame, template.Fingerprint)
			score.Scores = append(score.Scores, CategoryScore{Category: template.Category, Score: value})
			if value > best {
				best = value
				score.BestCategory = template.Category
				score.BestScore = value
			}
		}
		score.Hit = best >= params.MatchThreshold && frame.Energy >= params.MinEnergy
		if !score.Hit {
			score.BestCategory = ""
		}
		out = append(out, score)
	}
	return out
}

func Aggregate(frames []Fingerprint, templates []Template, params Params) ([]Interval, []FrameScore, error) {
	params = params.Normalize()
	if err := params.Validate(); err != nil {
		return nil, nil, err
	}
	if len(frames) == 0 {
		return []Interval{}, []FrameScore{}, nil
	}
	for i, frame := range frames {
		if frame.FrameIndex != i {
			return nil, nil, newError(CodeState, fmt.Sprintf("frames[%d].frame_index", i),
				"aggregate requires dense ordered frames: expected index %d got %d", i, frame.FrameIndex)
		}
	}

	scores := ScoreFrames(frames, templates, params)
	hitsByCategory := make(map[string][]int)
	for _, score := range scores {
		if score.Hit {
			hitsByCategory[score.BestCategory] = append(hitsByCategory[score.BestCategory], score.FrameIndex)
		}
	}

	categories := make([]string, 0, len(hitsByCategory))
	for category := range hitsByCategory {
		categories = append(categories, category)
	}
	sort.Strings(categories)

	intervals := make([]Interval, 0)
	gapFrames := params.GapFrames()
	for _, category := range categories {
		hits := hitsByCategory[category]
		groups := groupHits(hits, gapFrames)
		categoryIntervals := make([]Interval, 0, len(groups))
		for _, group := range groups {
			if len(group) < params.MinHits {
				continue
			}
			first := group[0]
			last := group[len(group)-1]
			var similaritySum float64
			for _, index := range group {
				for _, cs := range scores[index].Scores {
					if cs.Category == category {
						similaritySum += cs.Score
						break
					}
				}
			}
			meanSimilarity := similaritySum / float64(len(group))
			spanFrames := last - first + 1
			missingFrames := spanFrames - len(group)
			density := float64(len(group)) / float64(spanFrames)
			lengthFactor := math.Min(1.0, float64(len(group))/float64(params.MinHits+2))
			confidence := meanSimilarity * (0.80 + 0.20*density) * (0.85 + 0.15*lengthFactor)
			confidence = math.Round(math.Max(0, math.Min(1, confidence))*1e6) / 1e6

			start := frames[first].StartSample
			end := frames[last].StartSample + frames[last].FrameLength
			interval := Interval{
				Category:       category,
				StartSample:    start,
				EndSample:      end,
				FirstFrame:     first,
				LastFrame:      last,
				HitFrames:      len(group),
				SpanFrames:     spanFrames,
				GapFrames:      missingFrames,
				MeanSimilarity: math.Round(meanSimilarity*1e6) / 1e6,
				Confidence:     confidence,
			}
			categoryIntervals = append(categoryIntervals, interval)
		}

		// Long analysis frames can physically overlap the next hit group. Clamp
		// the previous endpoint to the next hop-grid start to guarantee that
		// same-category output intervals do not overlap.
		for i := 0; i+1 < len(categoryIntervals); i++ {
			nextStart := categoryIntervals[i+1].StartSample
			if categoryIntervals[i].EndSample > nextStart {
				categoryIntervals[i].EndSample = nextStart
			}
		}
		intervals = append(intervals, categoryIntervals...)
	}

	sort.SliceStable(intervals, func(i, j int) bool {
		if intervals[i].StartSample == intervals[j].StartSample {
			return intervals[i].Category < intervals[j].Category
		}
		return intervals[i].StartSample < intervals[j].StartSample
	})
	for i := range intervals {
		intervals[i].ID = fmt.Sprintf("ann-%03d", i+1)
	}

	if err := validateIntervalGrid(intervals, params); err != nil {
		return nil, scores, err
	}
	return intervals, scores, nil
}

func groupHits(hits []int, gapFrames int) [][]int {
	if len(hits) == 0 {
		return nil
	}
	sort.Ints(hits)
	groups := make([][]int, 0)
	current := []int{hits[0]}
	for i := 1; i < len(hits); i++ {
		previous := hits[i-1]
		if hits[i]-previous-1 <= gapFrames {
			current = append(current, hits[i])
		} else {
			groups = append(groups, current)
			current = []int{hits[i]}
		}
	}
	groups = append(groups, current)
	return groups
}

func validateIntervalGrid(intervals []Interval, params Params) error {
	quantum := params.QuantumStep()
	if quantum <= 0 {
		return paraError("quantum", "invalid quantization step")
	}
	for _, interval := range intervals {
		if interval.StartSample%quantum != 0 || interval.EndSample%quantum != 0 {
			return newError(CodeState, "interval",
				"interval %s boundary (%d,%d) is not on quantum %d",
				interval.ID, interval.StartSample, interval.EndSample, quantum)
		}
		if interval.StartSample >= interval.EndSample {
			return newError(CodeState, "interval", "interval %s has invalid range [%d,%d)",
				interval.ID, interval.StartSample, interval.EndSample)
		}
	}
	return nil
}
