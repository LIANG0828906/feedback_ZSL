package core

import "fmt"

func CompareResults(pathA, pathB string, skeleton *Skeleton, a, b []FrameResult) *DiffReport {
	report := &DiffReport{OK: true, PathA: pathA, PathB: pathB, ComparedFrames: len(a)}
	if len(b) < report.ComparedFrames {
		report.ComparedFrames = len(b)
	}
	jointName := func(i int) string {
		if skeleton != nil && i >= 0 && i < len(skeleton.Joints) {
			return skeleton.Joints[i].Name
		}
		return fmt.Sprintf("joint_index_%d", i)
	}
	addMismatch := func(m DiffMismatch) {
		if report.FirstMismatch == nil {
			cp := m
			report.FirstMismatch = &cp
		}
		report.MismatchCount++
		report.OK = false
	}

	if len(a) != len(b) {
		addMismatch(DiffMismatch{Frame: report.ComparedFrames, Field: "frame_count", A: len(a), B: len(b)})
	}
	for f := 0; f < report.ComparedFrames; f++ {
		fa, fb := a[f], b[f]
		if fa.Frame != fb.Frame {
			addMismatch(DiffMismatch{Frame: f, Field: "frame_index", A: fa.Frame, B: fb.Frame})
		}
		for c := 0; c < 3; c++ {
			if fa.Root[c] != fb.Root[c] {
				addMismatch(DiffMismatch{Frame: f, Field: fmt.Sprintf("root.%c", "xyz"[c]), A: fa.Root[c], B: fb.Root[c]})
			}
		}
		if len(fa.Rotations) != len(fb.Rotations) {
			addMismatch(DiffMismatch{Frame: f, Field: "rotations.length", A: len(fa.Rotations), B: len(fb.Rotations)})
		}
		for j := 0; j < minInt(len(fa.Rotations), len(fb.Rotations)); j++ {
			for c := 0; c < 4; c++ {
				if fa.Rotations[j][c] != fb.Rotations[j][c] {
					field := []string{"w", "x", "y", "z"}[c]
					addMismatch(DiffMismatch{Frame: f, Joint: jointName(j), Field: "rotation." + field, A: fa.Rotations[j][c], B: fb.Rotations[j][c]})
				}
			}
		}
		if !equalStringSlice(fa.Contacts.Active, fb.Contacts.Active) {
			addMismatch(DiffMismatch{Frame: f, Field: "contacts.active", A: fa.Contacts.Active, B: fb.Contacts.Active})
		}
		if fa.Contacts.MaxDrift != fb.Contacts.MaxDrift {
			addMismatch(DiffMismatch{Frame: f, Field: "contacts.max_drift", A: fa.Contacts.MaxDrift, B: fb.Contacts.MaxDrift})
		}
		compareStats(f, &fa.Stats, &fb.Stats, addMismatch)
		if len(fa.Diagnostics) != len(fb.Diagnostics) {
			addMismatch(DiffMismatch{Frame: f, Field: "diagnostics.length", A: len(fa.Diagnostics), B: len(fb.Diagnostics)})
		}
		for j := 0; j < minInt(len(fa.Diagnostics), len(fb.Diagnostics)); j++ {
			da, db := fa.Diagnostics[j], fb.Diagnostics[j]
			joint := da.Joint
			if joint == "" {
				joint = jointName(j)
			}
			compareDiagFloat(f, joint, "quant_residual", da.QuantResidual, db.QuantResidual, addMismatch)
			compareDiagFloat(f, joint, "quant_bound", da.QuantBound, db.QuantBound, addMismatch)
			compareDiagFloat(f, joint, "scale_ratio", da.ScaleRatio, db.ScaleRatio, addMismatch)
			compareDiagFloat(f, joint, "scale_residual", da.ScaleResidual, db.ScaleResidual, addMismatch)
			compareDiagFloat(f, joint, "scale_bound", da.ScaleBound, db.ScaleBound, addMismatch)
			compareDiagFloat(f, joint, "fk_observed_error", da.FKObservedError, db.FKObservedError, addMismatch)
			compareDiagFloat(f, joint, "fk_bound", da.FKBound, db.FKBound, addMismatch)
			compareDiagFloat(f, joint, "angle_residual_radians", da.AngleResidual, db.AngleResidual, addMismatch)
			compareDiagFloat(f, joint, "reach_residual", da.ReachResidual, db.ReachResidual, addMismatch)
			compareDiagFloat(f, joint, "projection_residual", da.ProjectionResidual, db.ProjectionResidual, addMismatch)
			compareDiagFloat(f, joint, "projection_bound", da.ProjectionBound, db.ProjectionBound, addMismatch)
			compareDiagFloat(f, joint, "total_bound", da.TotalBound, db.TotalBound, addMismatch)
			if da.Depth != db.Depth {
				addMismatch(DiffMismatch{Frame: f, Joint: joint, Field: "depth", A: da.Depth, B: db.Depth})
			}
			if da.Iterations != db.Iterations {
				addMismatch(DiffMismatch{Frame: f, Joint: joint, Field: "iterations", A: da.Iterations, B: db.Iterations})
			}
		}
		if fa.ContentHash != fb.ContentHash {
			addMismatch(DiffMismatch{Frame: f, Field: "content_hash", A: fa.ContentHash, B: fb.ContentHash})
		}
	}
	return report
}

func compareStats(frame int, a, b *FrameStats, add func(DiffMismatch)) {
	fields := map[string][2]float64{
		"stats.max_angle_violation": {a.MaxAngleViolation, b.MaxAngleViolation},
		"stats.max_reach_violation": {a.MaxReachViolation, b.MaxReachViolation},
		"stats.max_violation":       {a.MaxViolation, b.MaxViolation},
		"stats.max_contact_drift":   {a.MaxContactDrift, b.MaxContactDrift},
	}
	for name, pair := range fields {
		if pair[0] != pair[1] {
			add(DiffMismatch{Frame: frame, Field: name, A: pair[0], B: pair[1]})
		}
	}
	if a.Iterations != b.Iterations {
		add(DiffMismatch{Frame: frame, Field: "stats.iterations", A: a.Iterations, B: b.Iterations})
	}
	if a.ContactViolations != b.ContactViolations {
		add(DiffMismatch{Frame: frame, Field: "stats.contact_violations", A: a.ContactViolations, B: b.ContactViolations})
	}
	if a.Converged != b.Converged {
		add(DiffMismatch{Frame: frame, Field: "stats.converged", A: a.Converged, B: b.Converged})
	}
}

func compareDiagFloat(frame int, joint, field string, a, b float64, add func(DiffMismatch)) {
	if a != b {
		add(DiffMismatch{Frame: frame, Joint: joint, Field: field, A: a, B: b})
	}
}

func equalStringSlice(a, b []string) bool {
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func DiffAsError(r *DiffReport) *Error {
	if r.OK {
		return nil
	}
	details := map[string]any{"mismatch_count": r.MismatchCount, "first": r.FirstMismatch}
	if r.FirstMismatch != nil {
		return &Error{Code: ECodeDiff, Frame: r.FirstMismatch.Frame, Joint: r.FirstMismatch.Joint, Field: r.FirstMismatch.Field, Message: "three-path frame mismatch", Details: details}
	}
	return &Error{Code: ECodeDiff, Message: "result mismatch", Details: details}
}
