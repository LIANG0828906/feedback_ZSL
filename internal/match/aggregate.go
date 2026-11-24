// Package match 把帧指纹与类别特征音模板比对并聚合为事件区间。
package match

import (
	"math"

	"audiobench/internal/dsp"
	"audiobench/internal/model"
)

// TemplateSet 每个类别的平均相对能量模板。
type TemplateSet map[string][]float64

// BuildTemplates 为全部声明类别构建模板。
func BuildTemplates(categories []model.Category, sampleRate int, p model.Params) TemplateSet {
	set := make(TemplateSet, len(categories))
	for _, cat := range categories {
		set[cat.Label] = dsp.BuildTemplate(cat, sampleRate, p)
	}
	return set
}

// FrameScore 单帧与某类别模板的余弦相似度（基于相对能量分布）。
func FrameScore(fp model.Fingerprint, template []float64) float64 {
	var dot, normA, normB float64
	for b := range template {
		dot += fp.RelativeEnergy[b] * template[b]
		normA += fp.RelativeEnergy[b] * fp.RelativeEnergy[b]
		normB += template[b] * template[b]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

type frameHit struct {
	frame int
	label string
	score float64
}

// Aggregate 对全部帧执行模板匹配并聚合事件。
// 静默帧（总能量接近零）不产生命中；无命中时返回空切片而非错误。
func Aggregate(frames []model.Fingerprint, templates TemplateSet, p model.Params) []model.Event {
	labels := make([]string, 0, len(templates))
	for label := range templates {
		labels = append(labels, label)
	}
	// 固定类别顺序，保证平局选择与输出顺序确定。
	sortLabels(labels)

	hits := make([]frameHit, 0, len(frames))
	for _, fp := range frames {
		if fp.TotalEnergy <= 0 {
			continue
		}
		bestLabel := ""
		bestScore := 0.0
		for _, label := range labels {
			score := FrameScore(fp, templates[label])
			if score > bestScore {
				bestScore = score
				bestLabel = label
			}
		}
		if bestLabel != "" && bestScore >= p.MatchThreshold {
			hits = append(hits, frameHit{frame: fp.Frame, label: bestLabel, score: bestScore})
		}
	}

	// 按类别分组，依据间隙容忍合并同类相邻命中，天然避免跨类别区间重叠。
	events := []model.Event{}
	for _, label := range labels {
		var group []frameHit
		for _, h := range hits {
			if h.label == label {
				group = append(group, h)
			}
		}
		events = append(events, mergeGroup(group, p)...)
	}
	sortEvents(events)
	return events
}

func mergeGroup(group []frameHit, p model.Params) []model.Event {
	out := []model.Event{}
	if len(group) == 0 {
		return out
	}
	runStart := 0
	closeRun := func(endIdx int) {
		run := group[runStart:endIdx]
		if len(run) < p.MinHitFrames {
			return
		}
		first, last := run[0], run[len(run)-1]
		scoreSum := 0.0
		for _, h := range run {
			scoreSum += h.score
		}
		mean := scoreSum / float64(len(run))
		coverage := float64(len(run)) / float64(last.frame-first.frame+1)
		out = append(out, model.Event{
			Label:          first.label,
			StartFrame:     first.frame,
			EndFrame:       last.frame + 1,
			StartSample:    first.frame * p.FrameShift,
			EndSample:      last.frame*p.FrameShift + p.FrameLength,
			HitFrames:      len(run),
			MeanSimilarity: round4(mean),
			Confidence:     round4(mean * (0.5 + 0.5*coverage)),
		})
	}
	for i := 1; i <= len(group); i++ {
		if i == len(group) || group[i].frame-group[i-1].frame > p.GapToleranceFrames+1 {
			closeRun(i)
		}
		if i < len(group) && group[i].frame-group[i-1].frame > p.GapToleranceFrames+1 {
			runStart = i
		}
	}
	return out
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}

func sortLabels(labels []string) {
	for i := 1; i < len(labels); i++ {
		for j := i; j > 0 && labels[j-1] > labels[j]; j-- {
			labels[j-1], labels[j] = labels[j], labels[j-1]
		}
	}
}

func sortEvents(events []model.Event) {
	for i := 1; i < len(events); i++ {
		for j := i; j > 0 && events[j-1].StartFrame > events[j].StartFrame; j-- {
			events[j-1], events[j] = events[j], events[j-1]
		}
	}
}
