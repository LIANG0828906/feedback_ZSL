package repair

import (
	"meshforge/internal/errs"
	"meshforge/internal/mesh"
)

type OrientReport struct {
	Components          int       `json:"components"`
	FlippedFaces        int       `json:"flipped_faces"`
	ConflictEdgesBefore int       `json:"conflict_edges_before"`
	ConflictEdgesAfter  int       `json:"conflict_edges_after"`
	SignedVolumesBefore []float64 `json:"signed_volumes_before"`
	SignedVolumesAfter  []float64 `json:"signed_volumes_after"`
}

func UnifyOrientation(in mesh.Mesh) (mesh.Mesh, OrientReport, *errs.CodedError) {
	reportBefore := mesh.AnalyzeOrientation(in)
	report := OrientReport{
		Components:          reportBefore.Components,
		FlippedFaces:        reportBefore.FlippedFaces,
		ConflictEdgesBefore: reportBefore.ConflictEdges,
		SignedVolumesBefore: reportBefore.ComponentSignedVolumes,
	}
	if len(reportBefore.ConflictComponents) > 0 {
		e := errs.New(errs.CodeOrient,
			"orientation constraints conflict in %d component(s)",
			len(reportBefore.ConflictComponents))
		e.ConflictComponents = append([]int(nil), reportBefore.ConflictComponents...)
		return mesh.Mesh{}, report, e
	}
	out := mesh.ApplyOrientation(in, reportBefore)
	after := mesh.AnalyzeOrientation(out)
	report.ConflictEdgesAfter = after.ConflictEdges
	report.SignedVolumesAfter = after.ComponentSignedVolumes
	if len(after.ConflictComponents) > 0 || after.ConflictEdges > 0 {
		e := errs.New(errs.CodeOrient,
			"orientation conflict remains in %d component(s) after propagation",
			len(after.ConflictComponents))
		e.ConflictComponents = append([]int(nil), after.ConflictComponents...)
		return mesh.Mesh{}, report, e
	}
	return out, report, nil
}
