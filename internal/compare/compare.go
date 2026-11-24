// Package compare 对实时、断点恢复、全量重算三条路径的产物做逐帧/逐事件比对。
package compare

import (
	"fmt"
	"reflect"

	"audiobench/internal/model"
)

// Diff 定位首个不一致位置与差异字段，并携带两侧取值。
type Diff struct {
	Kind       string `json:"kind"`  // frame | event | quality | meta
	Index      int    `json:"index"` // 帧号或事件编号（-1 表示不适用）
	Field      string `json:"field"` // 差异字段名
	LeftValue  any    `json:"left_value"`
	RightValue any    `json:"right_value"`
}

// Results 三路径产物。
type Results struct {
	Fingerprints []model.Fingerprint
	Events       []model.Event
	Quality      *model.QualityReport
}

// Pair 两条路径的成对产物。
type Pair struct {
	LeftName  string
	RightName string
	Results   Results
	Other     Results
}

// Compare 逐帧、逐事件、逐统计字段比对；首个差异即返回 E_DIV。
func Compare(leftName, rightName string, a, b Results) *model.AppError {
	if diff := compareFingerprints(a.Fingerprints, b.Fingerprints); diff != nil {
		return diffErr(leftName, rightName, diff)
	}
	if diff := compareEvents(a.Events, b.Events); diff != nil {
		return diffErr(leftName, rightName, diff)
	}
	if diff := compareQuality(a.Quality, b.Quality); diff != nil {
		return diffErr(leftName, rightName, diff)
	}
	return nil
}

func diffErr(left, right string, d *Diff) *model.AppError {
	return model.NewErr(model.EDiv,
		"路径 %s 与 %s 在 %s #%d 字段 %s 不一致",
		left, right, d.Kind, d.Index, d.Field).
		With("kind", d.Kind).
		With("index", d.Index).
		With("field", d.Field).
		With("left_"+left, d.LeftValue).
		With("right_"+right, d.RightValue)
}

func compareFingerprints(a, b []model.Fingerprint) *Diff {
	if len(a) != len(b) {
		return &Diff{Kind: "frame", Index: -1, Field: "frame_count",
			LeftValue: len(a), RightValue: len(b)}
	}
	for i := range a {
		if a[i].Frame != b[i].Frame {
			return &Diff{Kind: "frame", Index: i, Field: "frame",
				LeftValue: a[i].Frame, RightValue: b[i].Frame}
		}
		if a[i].OffsetSample != b[i].OffsetSample {
			return &Diff{Kind: "frame", Index: i, Field: "offset_sample",
				LeftValue: a[i].OffsetSample, RightValue: b[i].OffsetSample}
		}
		if a[i].TotalEnergy != b[i].TotalEnergy {
			return &Diff{Kind: "frame", Index: i, Field: "total_energy",
				LeftValue: a[i].TotalEnergy, RightValue: b[i].TotalEnergy}
		}
		if !reflect.DeepEqual(a[i].Quantized, b[i].Quantized) {
			return &Diff{Kind: "frame", Index: i, Field: "quantized",
				LeftValue: a[i].Quantized, RightValue: b[i].Quantized}
		}
		if !reflect.DeepEqual(a[i].RelativeEnergy, b[i].RelativeEnergy) {
			return &Diff{Kind: "frame", Index: i, Field: "relative_energy",
				LeftValue: a[i].RelativeEnergy, RightValue: b[i].RelativeEnergy}
		}
	}
	return nil
}

func compareEvents(a, b []model.Event) *Diff {
	if len(a) != len(b) {
		return &Diff{Kind: "event", Index: -1, Field: "event_count",
			LeftValue: len(a), RightValue: len(b)}
	}
	fields := []struct {
		name string
		get  func(model.Event) any
	}{
		{"label", func(e model.Event) any { return e.Label }},
		{"start_sample", func(e model.Event) any { return e.StartSample }},
		{"end_sample", func(e model.Event) any { return e.EndSample }},
		{"start_frame", func(e model.Event) any { return e.StartFrame }},
		{"end_frame", func(e model.Event) any { return e.EndFrame }},
		{"hit_frames", func(e model.Event) any { return e.HitFrames }},
		{"mean_similarity", func(e model.Event) any { return e.MeanSimilarity }},
		{"confidence", func(e model.Event) any { return e.Confidence }},
	}
	for i := range a {
		for _, f := range fields {
			if !reflect.DeepEqual(f.get(a[i]), f.get(b[i])) {
				return &Diff{Kind: "event", Index: i, Field: f.name,
					LeftValue: f.get(a[i]), RightValue: f.get(b[i])}
			}
		}
	}
	return nil
}

func compareQuality(a, b *model.QualityReport) *Diff {
	if a == nil || b == nil {
		if (a == nil) != (b == nil) {
			return &Diff{Kind: "quality", Index: -1, Field: "report_presence",
				LeftValue: fmt.Sprintf("%v", a != nil), RightValue: fmt.Sprintf("%v", b != nil)}
		}
		return nil
	}
	scalars := []struct {
		name string
		a, b any
	}{
		{"total_declared", a.TotalDeclared, b.TotalDeclared},
		{"total_detected", a.TotalDetected, b.TotalDetected},
		{"matched", a.Matched, b.Matched},
		{"label_correct", a.LabelCorrect, b.LabelCorrect},
		{"label_accuracy", a.LabelAccuracy, b.LabelAccuracy},
		{"mean_start_bias", a.MeanStartBias, b.MeanStartBias},
		{"mean_end_bias", a.MeanEndBias, b.MeanEndBias},
		{"max_abs_start_bias", a.MaxAbsStartBias, b.MaxAbsStartBias},
		{"max_abs_end_bias", a.MaxAbsEndBias, b.MaxAbsEndBias},
		{"confidence_mean", a.ConfidenceMean, b.ConfidenceMean},
		{"confidence_min", a.ConfidenceMin, b.ConfidenceMin},
		{"confidence_max", a.ConfidenceMax, b.ConfidenceMax},
	}
	for _, s := range scalars {
		if !reflect.DeepEqual(s.a, s.b) {
			return &Diff{Kind: "quality", Index: -1, Field: s.name,
				LeftValue: s.a, RightValue: s.b}
		}
	}
	if !reflect.DeepEqual(a.ConfidenceHist, b.ConfidenceHist) {
		return &Diff{Kind: "quality", Index: -1, Field: "confidence_hist",
			LeftValue: a.ConfidenceHist, RightValue: b.ConfidenceHist}
	}
	return nil
}
