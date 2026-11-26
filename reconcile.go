package bonerig

import (
	"fmt"
	"sort"
)

// CompareFrames locates the first frame/joint/field mismatch between paths.
func CompareFrames(expected, actual []FrameResult) *Error {
	em := map[int]FrameResult{}
	am := map[int]FrameResult{}
	for _, f := range expected {
		em[f.Index] = f
	}
	for _, f := range actual {
		am[f.Index] = f
	}
	indices := make([]int, 0, len(em))
	for i := range em {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	if len(actual) != len(expected) {
		d := DiffDetail{Joint: "$", Frame: -1, Field: "frame_count", Expected: len(expected), Actual: len(actual)}
		return DiffError(d)
	}
	for _, idx := range indices {
		ef := em[idx]
		af, ok := am[idx]
		if !ok {
			return DiffError(DiffDetail{Frame: idx, Joint: "$frame", Field: "missing_frame", Expected: true, Actual: false})
		}
		joints := []string{}
		seen := map[string]bool{}
		for j := range ef.Outputs {
			joints = append(joints, j)
			seen[j] = true
		}
		for j := range af.Outputs {
			if !seen[j] {
				joints = append(joints, j)
			}
		}
		sort.Strings(joints)
		for _, j := range joints {
			eo, eok := ef.Outputs[j]
			ao, aok := af.Outputs[j]
			if eok != aok {
				return DiffError(DiffDetail{Frame: idx, Joint: j, Field: "joint_present", Expected: eok, Actual: aok})
			}
			if d := compareQuat(idx, j, "rotation", eo.Rotation, ao.Rotation); d != nil {
				return DiffError(*d)
			}
			if d := compareVec(idx, j, "translation", eo.Translation, ao.Translation); d != nil {
				return DiffError(*d)
			}
		}
		if d := comparePrecision(idx, ef.Precision, af.Precision); d != nil {
			return DiffError(*d)
		}
	}
	return nil
}

func compareQuat(frame int, joint, field string, e, a Quat) *DiffDetail {
	pairs := []struct {
		name string
		e, a int64
	}{
		{"w", e.W, a.W}, {"x", e.X, a.X}, {"y", e.Y, a.Y}, {"z", e.Z, a.Z}, {"scale", e.Scale, a.Scale},
	}
	for _, p := range pairs {
		if p.e != p.a {
			return &DiffDetail{Frame: frame, Joint: joint, Field: field + "." + p.name, Expected: p.e, Actual: p.a}
		}
	}
	return nil
}

func compareVec(frame int, joint, field string, e, a Vec3) *DiffDetail {
	pairs := []struct {
		name string
		e, a int64
	}{
		{"x", e.X, a.X}, {"y", e.Y, a.Y}, {"z", e.Z, a.Z}, {"scale", e.Scale, a.Scale},
	}
	for _, p := range pairs {
		if p.e != p.a {
			return &DiffDetail{Frame: frame, Joint: joint, Field: field + "." + p.name, Expected: p.e, Actual: p.a}
		}
	}
	return nil
}

func comparePrecision(frame int, e, a []PrecisionRecord) *DiffDetail {
	em := map[string]PrecisionRecord{}
	am := map[string]PrecisionRecord{}
	for _, r := range e {
		em[r.Joint] = r
	}
	for _, r := range a {
		am[r.Joint] = r
	}
	for joint, er := range em {
		ar, ok := am[joint]
		if !ok {
			return &DiffDetail{Frame: frame, Joint: joint, Field: "precision", Expected: true, Actual: false}
		}
		fields := []struct {
			name string
			e, a int64
		}{
			{"norm_residual", er.NormResidual, ar.NormResidual},
			{"norm_upper", er.NormUpper, ar.NormUpper},
			{"chain_residual", er.ChainResidual, ar.ChainResidual},
			{"chain_upper", er.ChainUpper, ar.ChainUpper},
			{"scale_residual", er.ScaleResidual, ar.ScaleResidual},
			{"scale_upper", er.ScaleUpper, ar.ScaleUpper},
			{"translation_scale_residual", er.TranslationScaleResidual, ar.TranslationScaleResidual},
			{"map_residual", er.MapResidual, ar.MapResidual},
			{"map_upper", er.MapUpper, ar.MapUpper},
			{"total_upper", er.TotalUpper, ar.TotalUpper},
			{"measured_deviation", er.MeasuredDeviation, ar.MeasuredDeviation},
			{"chain_length", er.ChainLength, ar.ChainLength},
		}
		for _, f := range fields {
			if f.e != f.a {
				return &DiffDetail{Frame: frame, Joint: joint, Field: "precision." + f.name, Expected: f.e, Actual: f.a}
			}
		}
		if er.WithinBound != ar.WithinBound {
			return &DiffDetail{Frame: frame, Joint: joint, Field: "precision.within_bound", Expected: er.WithinBound, Actual: ar.WithinBound}
		}
	}
	return nil
}

// RequireEqualSequence returns E_DIFF when commit sequences differ.
func RequireEqualSequence(expected, actual []string) *Error {
	if len(expected) != len(actual) {
		e := DiffError(DiffDetail{Joint: "$interval", Frame: -1, Field: "commit_sequence_length", Expected: len(expected), Actual: len(actual)})
		e.Expected = expected
		e.Actual = actual
		return e
	}
	for i := range expected {
		if expected[i] != actual[i] {
			e := DiffError(DiffDetail{Joint: "$interval", Frame: -1, Field: fmt.Sprintf("commit_sequence[%d]", i), Expected: expected[i], Actual: actual[i]})
			return e
		}
	}
	return nil
}
