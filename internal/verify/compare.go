package verify

import (
	"fmt"
	"reflect"
)

func CompareSnapshots(a, b Snapshot) *Difference {
	if d := compareDecisions(a.Decisions, b.Decisions); d != nil {
		return d
	}
	if d := compareNoises(a.Noises, b.Noises); d != nil {
		return d
	}
	if d := compareIntervals(a.Intervals, b.Intervals); d != nil {
		return d
	}
	if d := compareSubmission(a.SubmissionSequence, b.SubmissionSequence); d != nil {
		return d
	}
	return nil
}

func CompareCommitSequences(a, b []CommitItem) *Difference {
	if len(a) != len(b) {
		return &Difference{Code: "E_DIFF", Field: "commit_sequence.length", Message: fmt.Sprintf("commit sequence length %d != %d", len(a), len(b))}
	}
	for i := range a {
		if a[i].BatchID != b[i].BatchID || a[i].Ordinal != b[i].Ordinal || a[i].Frame != b[i].Frame ||
			a[i].EvidenceType != b[i].EvidenceType || a[i].CacheKey != b[i].CacheKey {
			return &Difference{
				Code: "E_DIFF", Frame: a[i].Frame, EvidenceType: string(a[i].EvidenceType),
				Field:   fmt.Sprintf("commit_sequence[%d]", i),
				Message: fmt.Sprintf("commit item %+v != %+v", a[i], b[i]),
			}
		}
	}
	return nil
}

func compareDecisions(a, b []DecisionEvidence) *Difference {
	if len(a) != len(b) {
		return lengthDiff("decisions", len(a), len(b))
	}
	for i := range a {
		if a[i].Frame != b[i].Frame {
			return frameFieldDiff(a[i].Frame, string(EvDecision), "frame", a[i].Frame, b[i].Frame)
		}
		if d := compareStructFields(a[i], b[i], a[i].Frame, 0, 0, string(EvDecision)); d != nil {
			return d
		}
	}
	return nil
}

func compareNoises(a, b []NoiseEvidence) *Difference {
	if len(a) != len(b) {
		return lengthDiff("noises", len(a), len(b))
	}
	for i := range a {
		if a[i].Frame != b[i].Frame {
			return frameFieldDiff(a[i].Frame, string(EvNoise), "frame", a[i].Frame, b[i].Frame)
		}
		if len(a[i].Blocks) != len(b[i].Blocks) {
			return &Difference{Code: "E_DIFF", Frame: a[i].Frame, EvidenceType: string(EvNoise), Field: "blocks.length",
				Message: fmt.Sprintf("frame %d block count %d != %d", a[i].Frame, len(a[i].Blocks), len(b[i].Blocks))}
		}
		for j := range a[i].Blocks {
			ab, bb := a[i].Blocks[j], b[i].Blocks[j]
			if ab.BlockX != bb.BlockX || ab.BlockY != bb.BlockY {
				return &Difference{Code: "E_DIFF", Frame: a[i].Frame, BlockX: ab.BlockX, BlockY: ab.BlockY,
					EvidenceType: string(EvNoise), Field: "block_coordinate",
					Message: fmt.Sprintf("block coordinate (%d,%d) != (%d,%d)", ab.BlockX, ab.BlockY, bb.BlockX, bb.BlockY)}
			}
			if d := compareStructFields(ab, bb, a[i].Frame, ab.BlockX, ab.BlockY, string(EvNoise)); d != nil {
				return d
			}
		}
		ai, bi := a[i], b[i]
		ai.Blocks = nil
		bi.Blocks = nil
		if d := compareStructFields(ai, bi, a[i].Frame, 0, 0, string(EvNoise)); d != nil {
			return d
		}
	}
	return nil
}

func compareIntervals(a, b []Interval) *Difference {
	if len(a) != len(b) {
		return lengthDiff("intervals", len(a), len(b))
	}
	for i := range a {
		if d := compareStructFields(a[i], b[i], a[i].StartFrame, 0, 0, "interval"); d != nil {
			d.Field = "intervals[" + itoa(i) + "]." + d.Field
			return d
		}
	}
	return nil
}

func compareSubmission(a, b []CommitItem) *Difference {
	if len(a) != len(b) {
		return lengthDiff("submission_sequence", len(a), len(b))
	}
	for i := range a {
		if a[i].Frame != b[i].Frame || a[i].EvidenceType != b[i].EvidenceType || a[i].CacheKey != b[i].CacheKey {
			return &Difference{Code: "E_DIFF", Frame: a[i].Frame, EvidenceType: string(a[i].EvidenceType),
				Field:   fmt.Sprintf("submission_sequence[%d]", i),
				Message: fmt.Sprintf("submission item %+v != %+v", a[i], b[i])}
		}
	}
	return nil
}

func compareStructFields(a, b any, frame, bx, by int, typ string) *Difference {
	va := reflect.ValueOf(a)
	vb := reflect.ValueOf(b)
	ta := va.Type()
	for i := 0; i < ta.NumField(); i++ {
		name := jsonFieldName(ta.Field(i))
		if name == "-" {
			continue
		}
		x := va.Field(i).Interface()
		y := vb.Field(i).Interface()
		if !reflect.DeepEqual(x, y) {
			return &Difference{
				Code: "E_DIFF", Frame: frame, BlockX: bx, BlockY: by, EvidenceType: typ, Field: name,
				Message: fmt.Sprintf("field %s: %#v != %#v", name, x, y),
			}
		}
	}
	return nil
}

func jsonFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name
	}
	name := tag
	for i, c := range tag {
		if c == ',' {
			name = tag[:i]
			break
		}
	}
	if name == "" {
		return f.Name
	}
	return name
}

func lengthDiff(field string, a, b int) *Difference {
	return &Difference{Code: "E_DIFF", Field: field + ".length", Message: fmt.Sprintf("%s length %d != %d", field, a, b)}
}

func frameFieldDiff(frame int, typ, field string, a, b any) *Difference {
	return &Difference{Code: "E_DIFF", Frame: frame, EvidenceType: typ, Field: field,
		Message: fmt.Sprintf("%s: %#v != %#v", field, a, b)}
}
