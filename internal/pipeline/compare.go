package pipeline

import (
	"encoding/json"
	"fmt"

	"meshdoctor/internal/errs"
)

type DiffReport struct {
	Identical   bool           `json:"identical"`
	PathA       string         `json:"path_a"`
	PathB       string         `json:"path_b"`
	FirstStage  string         `json:"first_stage,omitempty"`
	DiffType    string         `json:"diff_type,omitempty"`
	ByteOffset  *int           `json:"byte_offset,omitempty"`
	VertexIndex *int           `json:"vertex_index,omitempty"`
	FaceIndex   *int           `json:"face_index,omitempty"`
	SummaryA    map[string]any `json:"summary_a,omitempty"`
	SummaryB    map[string]any `json:"summary_b,omitempty"`
	Detail      string         `json:"detail,omitempty"`
}

func (r DiffReport) AsError() error {
	if r.Identical {
		return nil
	}
	return errs.WithDetails(errs.New(errs.EDIFF, "first mismatch at %s (%s)", r.FirstStage, r.DiffType), map[string]any{
		"first_stage":  r.FirstStage,
		"diff_type":    r.DiffType,
		"byte_offset":  r.ByteOffset,
		"vertex_index": r.VertexIndex,
		"summary_a":    r.SummaryA,
		"summary_b":    r.SummaryB,
		"report":       r,
	})
}

type ThreeWayReport struct {
	AllIdentical bool       `json:"all_identical"`
	Segmented    DiffReport `json:"segmented_vs_one_shot"`
	Resume       DiffReport `json:"resume_vs_one_shot"`
}

func CompareThree(segmented, resume, oneShot *PathResult) (*ThreeWayReport, error) {
	rep := &ThreeWayReport{
		Segmented: CompareResults(segmented, oneShot),
		Resume:    CompareResults(resume, oneShot),
	}
	rep.AllIdentical = rep.Segmented.Identical && rep.Resume.Identical
	if !rep.AllIdentical {
		bad := rep.Segmented
		if rep.Segmented.Identical {
			bad = rep.Resume
		}
		return rep, errs.New(errs.EDIFF, "path mismatch at %s: %s", bad.FirstStage, bad.DiffType)
	}
	return rep, nil
}

func CompareResults(a, b *PathResult) DiffReport {
	rep := DiffReport{Identical: true}
	if a != nil {
		rep.PathA = a.Path
	}
	if b != nil {
		rep.PathB = b.Path
	}
	if a == nil || b == nil {
		rep.Identical = false
		rep.DiffType = "missing_result"
		return rep
	}

	if diff := compareStrings(a.CommittedSequence, b.CommittedSequence); diff != "" {
		rep.Identical = false
		rep.FirstStage = firstDifferentStage(a.CommittedSequence, b.CommittedSequence)
		rep.DiffType = "stage_sequence"
		rep.Detail = diff
		rep.SummaryA = map[string]any{"sequence": a.CommittedSequence}
		rep.SummaryB = map[string]any{"sequence": b.CommittedSequence}
		return rep
	}

	n := len(a.Stages)
	if len(b.Stages) < n {
		n = len(b.Stages)
	}
	for i := 0; i < n; i++ {
		sa, sb := a.Stages[i], b.Stages[i]
		stage := sa.Stage
		if sa.Stage != sb.Stage {
			rep.Identical = false
			rep.FirstStage = stage
			rep.DiffType = "stage_id"
			rep.Detail = fmt.Sprintf("stage ids %q != %q", sa.Stage, sb.Stage)
			return rep
		}
		ba, bb := sa.CanonicalMeshBytes(), sb.CanonicalMeshBytes()
		if string(ba) != string(bb) {
			d := meshByteDiff(stage, ba, bb, sa, sb)
			return d
		}
		ja, _ := json.Marshal(sa.Stats)
		jb, _ := json.Marshal(sb.Stats)
		if string(ja) != string(jb) {
			offset := firstByteDifference(ja, jb)
			off := offset
			rep.Identical = false
			rep.FirstStage = stage
			rep.DiffType = "stats"
			rep.ByteOffset = &off
			rep.Detail = "topology stats differ"
			rep.SummaryA = map[string]any{"stats": sa.Stats, "hash": sa.MeshHash}
			rep.SummaryB = map[string]any{"stats": sb.Stats, "hash": sb.MeshHash}
			return rep
		}
	}
	if len(a.Stages) != len(b.Stages) {
		rep.Identical = false
		idx := min(len(a.Stages), len(b.Stages))
		stage := StageStats
		if idx < len(Stages) {
			stage = Stages[idx]
		}
		rep.FirstStage = stage
		rep.DiffType = "stage_count"
		rep.Detail = fmt.Sprintf("stage count %d != %d", len(a.Stages), len(b.Stages))
		return rep
	}
	if a.FinalHash != b.FinalHash {
		rep.Identical = false
		rep.FirstStage = StageStats
		rep.DiffType = "final_hash"
		rep.SummaryA = map[string]any{"final_hash": a.FinalHash, "stats": a.FinalStats}
		rep.SummaryB = map[string]any{"final_hash": b.FinalHash, "stats": b.FinalStats}
		return rep
	}
	return rep
}

func meshByteDiff(stage string, a, b []byte, sa, sb StageSnapshot) DiffReport {
	rep := DiffReport{
		Identical: false, PathA: "", PathB: "", FirstStage: stage,
		SummaryA: map[string]any{"bytes": len(a), "hash": sa.MeshHash, "vertices": sa.Stats.Vertices, "faces": sa.Stats.Faces},
		SummaryB: map[string]any{"bytes": len(b), "hash": sb.MeshHash, "vertices": sb.Stats.Vertices, "faces": sb.Stats.Faces},
	}
	offset := firstByteDifference(a, b)
	rep.ByteOffset = &offset

	magicLen := len("MESHDOCTOR-MESH-V1\n")
	vertexBytes := 24
	vertexCount := len(sa.Mesh.Vertices)
	faceCount := len(sa.Mesh.Faces)
	if offset >= magicLen+16 {
		vertexStart := magicLen + 16
		vertexRegionEnd := vertexStart + vertexCount*vertexBytes
		if offset < vertexRegionEnd {
			vi := (offset - vertexStart) / vertexBytes
			rep.VertexIndex = &vi
			rep.DiffType = "vertex_bytes"
		} else {
			faceStart := vertexRegionEnd
			if faceCount > 0 {
				fi := (offset - faceStart) / vertexBytes
				rep.FaceIndex = &fi
				rep.DiffType = "face_bytes"
			} else {
				rep.DiffType = "mesh_trailing_byte"
			}
		}
	} else if len(a) != len(b) {
		rep.DiffType = "mesh_length"
	} else {
		rep.DiffType = "mesh_header_byte"
	}
	if len(a) != len(b) && rep.DiffType == "mesh_header_byte" {
		rep.DiffType = "mesh_length"
	}
	rep.Detail = fmt.Sprintf("canonical mesh differs at byte %d; lengths %d/%d", offset, len(a), len(b))
	return rep
}

func firstByteDifference(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func compareStrings(a, b []string) string {
	if len(a) != len(b) {
		return fmt.Sprintf("sequence length %d != %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			return fmt.Sprintf("stage[%d] %q != %q", i, a[i], b[i])
		}
	}
	return ""
}

func firstDifferentStage(a, b []string) string {
	for i := 0; i < min(len(a), len(b)); i++ {
		if a[i] != b[i] {
			return a[i]
		}
	}
	if len(a) > 0 {
		return a[min(len(a), len(b))-1]
	}
	return StageDiagnostic
}
