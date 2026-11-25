package runner

import "edgerefine/internal/model"

func CompareReference(ref, refined []byte, declaredHash string) *model.ReferenceReport {
	if len(ref) != len(refined) || len(ref) == 0 {
		return nil
	}
	var intersection, union, mismatch, fp, fn int
	var absSum int64
	for i := range ref {
		rb := ref[i] >= 128
		mb := refined[i] >= 128
		if rb && mb {
			intersection++
		}
		if rb || mb {
			union++
		}
		if rb != mb {
			mismatch++
			if mb {
				fp++
			} else {
				fn++
			}
		}
		d := int(ref[i]) - int(refined[i])
		if d < 0 {
			d = -d
		}
		absSum += int64(d)
	}
	n := float64(len(ref))
	return &model.ReferenceReport{
		ReferenceMaskHash: declaredHash,
		IoU:               float64(intersection) / float64(maxInt(1, union)),
		MismatchRatio:     float64(mismatch) / n,
		MeanAbsoluteError: float64(absSum) / n / 255.0,
		FalsePositive:     fp,
		FalseNegative:     fn,
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
