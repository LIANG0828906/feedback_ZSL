package runner

import "edgerefine/internal/model"

func CompareThreePaths(paths []string, reps ...*model.RunReport) *model.ThreePathReport {
	out := &model.ThreePathReport{Paths: paths, AllPixelEqual: true, AccuracyEqual: true, CommitSequencesEqual: true}
	if len(reps) == 0 {
		out.AllPixelEqual = false
		out.CommitSequencesEqual = false
		return out
	}
	base := reps[0]
	out.MaskHashes = make([]string, 0, len(reps))
	out.DeviationHashes = make([]string, 0, len(reps))
	for _, r := range reps {
		if r == nil {
			out.MaskHashes = append(out.MaskHashes, "")
			out.DeviationHashes = append(out.DeviationHashes, "")
			out.AccuracyHashes = append(out.AccuracyHashes, "")
			out.AllPixelEqual = false
			out.AccuracyEqual = false
			out.CommitSequencesEqual = false
			continue
		}
		out.MaskHashes = append(out.MaskHashes, r.MaskHash)
		out.DeviationHashes = append(out.DeviationHashes, r.DeviationHash)
		accHash := "nil"
		if r.Accuracy != nil {
			accHash = model.CanonicalHash(r.Accuracy)
		}
		out.AccuracyHashes = append(out.AccuracyHashes, accHash)
		if r.MaskHash != base.MaskHash || r.DeviationHash != base.DeviationHash {
			out.AllPixelEqual = false
		}
		baseAcc := "nil"
		if base != nil && base.Accuracy != nil {
			baseAcc = model.CanonicalHash(base.Accuracy)
		}
		if accHash != baseAcc {
			out.AccuracyEqual = false
		}
		if !equalStringSlices(r.CommitSequence, base.CommitSequence) {
			out.CommitSequencesEqual = false
		}
	}
	return out
}

func equalStringSlices(a, b []string) bool {
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
