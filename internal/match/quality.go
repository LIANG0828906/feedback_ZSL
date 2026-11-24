package match

import "audiobench/internal/model"

// ValidateQuality 用清单真值与本次标注结果做一对一匹配并生成质量报告。
// 类别错配或采样点偏差超容差时返回 E_QUAL，报告仍随错误一并返回。
func ValidateQuality(declared []model.EventDecl, detected []model.Event, tolerance int) (*model.QualityReport, error) {
	report := &model.QualityReport{
		TotalDeclared:   len(declared),
		TotalDetected:   len(detected),
		Tolerance:       tolerance,
		ConfidenceHist:  map[string]int{},
		PairDetails:     []model.QualityPair{},
		UnmatchedDecl:   []int{},
		UnmatchedDetect: []int{},
		Mismatches:      []model.QualityMismatch{},
	}

	used := make([]bool, len(detected))
	matchedDetect := map[int]int{} // 声明事件序号 → 检测事件下标
	for _, decl := range declared {
		bestIdx := -1
		bestDist := int(^uint(0) >> 1)
		for j, det := range detected {
			if used[j] {
				continue
			}
			dist := absInt(det.StartSample-decl.StartSample) + absInt(det.EndSample-decl.EndSample)
			if dist < bestDist {
				bestDist = dist
				bestIdx = j
			}
		}
		if bestIdx < 0 {
			report.UnmatchedDecl = append(report.UnmatchedDecl, decl.Index)
			report.Mismatches = append(report.Mismatches, model.QualityMismatch{
				DeclIndex: decl.Index, DeclLabel: decl.Label, DetectLabel: "",
				Reason: "未匹配到任何检测事件",
			})
			continue
		}
		used[bestIdx] = true
		matchedDetect[decl.Index] = bestIdx
		det := detected[bestIdx]
		startBias := det.StartSample - decl.StartSample
		endBias := det.EndSample - decl.EndSample
		labelOK := det.Label == decl.Label
		withinTol := absInt(startBias) <= tolerance && absInt(endBias) <= tolerance
		if labelOK {
			report.LabelCorrect++
		}
		report.Matched++
		report.MaxAbsStartBias = maxInt(report.MaxAbsStartBias, absInt(startBias))
		report.MaxAbsEndBias = maxInt(report.MaxAbsEndBias, absInt(endBias))
		report.PairDetails = append(report.PairDetails, model.QualityPair{
			DeclIndex: decl.Index, DeclLabel: decl.Label, DetectLabel: det.Label,
			StartBias: startBias, EndBias: endBias, Confidence: det.Confidence,
			LabelCorrect: labelOK, WithinTol: withinTol,
		})
		if !labelOK {
			report.Mismatches = append(report.Mismatches, model.QualityMismatch{
				DeclIndex: decl.Index, DeclLabel: decl.Label, DetectLabel: det.Label,
				Reason: "类别错配",
			})
		} else if !withinTol {
			report.Mismatches = append(report.Mismatches, model.QualityMismatch{
				DeclIndex: decl.Index, DeclLabel: decl.Label, DetectLabel: det.Label,
				Reason: "起止采样点偏差超容差", StartBias: startBias, EndBias: endBias,
				Tolerance: tolerance,
			})
		}
	}
	for j, det := range detected {
		if !used[j] {
			report.UnmatchedDetect = append(report.UnmatchedDetect, j)
			report.Mismatches = append(report.Mismatches, model.QualityMismatch{
				DeclIndex: -1, DeclLabel: "", DetectLabel: det.Label,
				Reason: "检测事件无对应清单声明",
			})
		}
	}

	// 汇总统计。
	if report.Matched > 0 {
		startSum, endSum, confSum := 0, 0, 0.0
		report.ConfidenceMin, report.ConfidenceMax = 1.0, 0.0
		for _, pair := range report.PairDetails {
			startSum += pair.StartBias
			endSum += pair.EndBias
			confSum += pair.Confidence
			det := detected[matchedDetect[pair.DeclIndex]]
			report.ConfidenceMin = minFloat(report.ConfidenceMin, det.Confidence)
			report.ConfidenceMax = maxFloat(report.ConfidenceMax, det.Confidence)
			report.ConfidenceHist[histBucket(det.Confidence)]++
		}
		report.LabelAccuracy = round4(float64(report.LabelCorrect) / float64(report.Matched))
		report.MeanStartBias = round4(float64(startSum) / float64(report.Matched))
		report.MeanEndBias = round4(float64(endSum) / float64(report.Matched))
		report.ConfidenceMean = round4(confSum / float64(report.Matched))
	}

	if len(report.Mismatches) > 0 {
		return report, model.NewErr(model.EQuality,
			"标注质量校验发现 %d 项错配（类别错配或偏差超容差 %d 采样点）",
			len(report.Mismatches), tolerance).With("mismatch_count", len(report.Mismatches))
	}
	return report, nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func histBucket(conf float64) string {
	switch {
	case conf >= 0.9:
		return "[0.9,1.0]"
	case conf >= 0.75:
		return "[0.75,0.9)"
	case conf >= 0.5:
		return "[0.5,0.75)"
	default:
		return "[0.0,0.5)"
	}
}
