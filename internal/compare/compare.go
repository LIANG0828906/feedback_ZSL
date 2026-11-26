// Package compare locates the first item-level difference between paths.
package compare

import (
	"fmt"
	"reflect"

	"wardrobekernel/internal/model"
	"wardrobekernel/internal/xerr"
)

// Plans returns an E_DIFF error with the first batch/part/field mismatch.
func Plans(expected, actual *model.Plan) error {
	if mm := assembly(&expected.Assembly, &actual.Assembly); mm != nil {
		return diffError(mm)
	}
	if len(expected.CommitSequence) != len(actual.CommitSequence) {
		return diffError(&xerr.Mismatch{
			BatchNumber: -1, FieldName: "commit_sequence.length",
			Expected: len(expected.CommitSequence), Actual: len(actual.CommitSequence),
		})
	}
	for i := range expected.CommitSequence {
		if expected.CommitSequence[i] != actual.CommitSequence[i] {
			return diffError(&xerr.Mismatch{
				BatchNumber: i + 1, FieldName: "commit_sequence",
				Expected: expected.CommitSequence, Actual: actual.CommitSequence,
			})
		}
	}
	if len(expected.Batches) != len(actual.Batches) {
		return diffError(&xerr.Mismatch{
			BatchNumber: min(len(expected.Batches), len(actual.Batches)) + 1,
			FieldName:   "batches.length", Expected: len(expected.Batches), Actual: len(actual.Batches),
		})
	}
	for i := range expected.Batches {
		eb := expected.Batches[i]
		ab := actual.Batches[i]
		batchNumber := eb.Number
		if eb.Number != ab.Number {
			return diffError(&xerr.Mismatch{BatchNumber: i + 1, FieldName: "batch_number", Expected: eb.Number, Actual: ab.Number})
		}
		if mm := scalarBatch(batchNumber, "skin_key", eb.SkinKey, ab.SkinKey); mm != nil {
			return diffError(mm)
		}
		if mm := scalarBatch(batchNumber, "vertex_used", eb.VertexUsed, ab.VertexUsed); mm != nil {
			return diffError(mm)
		}
		if mm := scalarBatch(batchNumber, "index_used", eb.IndexUsed, ab.IndexUsed); mm != nil {
			return diffError(mm)
		}
		if eb.CacheKey != ab.CacheKey {
			return diffError(&xerr.Mismatch{BatchNumber: batchNumber, FieldName: "cache_key", Expected: eb.CacheKey, Actual: ab.CacheKey})
		}
		if !reflect.DeepEqual(eb.Dependencies, ab.Dependencies) {
			return diffError(&xerr.Mismatch{BatchNumber: batchNumber, FieldName: "dependencies", Expected: eb.Dependencies, Actual: ab.Dependencies})
		}
		if !reflect.DeepEqual(eb.PartIDs, ab.PartIDs) {
			for j := 0; j < max(len(eb.PartIDs), len(ab.PartIDs)); j++ {
				var ev, av string
				if j < len(eb.PartIDs) {
					ev = eb.PartIDs[j]
				}
				if j < len(ab.PartIDs) {
					av = ab.PartIDs[j]
				}
				if ev != av {
					return diffError(&xerr.Mismatch{
						BatchNumber: batchNumber, FieldName: "part_ids", Path: fmt.Sprintf("part_ids[%d]", j),
						Expected: ev, Actual: av,
					})
				}
			}
		}
		if mm := slots(batchNumber, eb.Slots, ab.Slots); mm != nil {
			return diffError(mm)
		}
		if mm := placements(batchNumber, eb.Placements, ab.Placements); mm != nil {
			return diffError(mm)
		}
	}
	return nil
}

func assembly(ea, aa *model.AssemblyResult) *xerr.Mismatch {
	if !reflect.DeepEqual(ea.LegalAssembly, aa.LegalAssembly) {
		return &xerr.Mismatch{BatchNumber: 0, FieldName: "assembly.legal_assembly", Expected: ea.LegalAssembly, Actual: aa.LegalAssembly}
	}
	if len(ea.Conflicts) != len(aa.Conflicts) {
		return &xerr.Mismatch{BatchNumber: 0, FieldName: "assembly.conflicts.length", Expected: len(ea.Conflicts), Actual: len(aa.Conflicts)}
	}
	for i := range ea.Conflicts {
		ec, ac := ea.Conflicts[i], aa.Conflicts[i]
		partID := ec.LoserID
		if ec.Category != ac.Category {
			return &xerr.Mismatch{BatchNumber: 0, PartID: partID, FieldName: "assembly.conflicts.category", Path: fmt.Sprintf("conflicts[%d]", i), Expected: ec.Category, Actual: ac.Category}
		}
		if ec.WinnerID != ac.WinnerID || ec.LoserID != ac.LoserID || ec.RuleID != ac.RuleID || ec.AttachPointID != ac.AttachPointID {
			return &xerr.Mismatch{BatchNumber: 0, PartID: firstNonEmpty(ec.LoserID, ac.LoserID), FieldName: "assembly.conflicts.parties", Path: fmt.Sprintf("conflicts[%d]", i), Expected: ec, Actual: ac}
		}
	}
	return nil
}

func scalarBatch(batch int, field string, expected, actual any) *xerr.Mismatch {
	if reflect.DeepEqual(expected, actual) {
		return nil
	}
	return &xerr.Mismatch{BatchNumber: batch, FieldName: field, Expected: expected, Actual: actual}
}

func slots(batch int, es, as []model.SlotAllocation) *xerr.Mismatch {
	if len(es) != len(as) {
		return &xerr.Mismatch{BatchNumber: batch, FieldName: "slots.length", Expected: len(es), Actual: len(as)}
	}
	for i := range es {
		if es[i].BatchSlotIndex != as[i].BatchSlotIndex || es[i].LogicalSlotID != as[i].LogicalSlotID ||
			es[i].MaterialID != as[i].MaterialID || es[i].MaterialVersion != as[i].MaterialVersion ||
			es[i].OwnerPartID != as[i].OwnerPartID {
			return &xerr.Mismatch{
				BatchNumber: batch, PartID: as[i].OwnerPartID, FieldName: "slots.material",
				Path: fmt.Sprintf("slots[%d]", i), Expected: es[i], Actual: as[i],
			}
		}
	}
	return nil
}

func placements(batch int, eps, aps []model.PartPlacement) *xerr.Mismatch {
	expectedByPart := map[string]model.PartPlacement{}
	for _, p := range eps {
		expectedByPart[p.PartID] = p
	}
	for _, ap := range aps {
		ep, ok := expectedByPart[ap.PartID]
		if !ok {
			return &xerr.Mismatch{BatchNumber: batch, PartID: ap.PartID, FieldName: "placements.part_id", Actual: ap.PartID}
		}
		if ep.VertexBase != ap.VertexBase {
			return &xerr.Mismatch{BatchNumber: batch, PartID: ap.PartID, FieldName: "placements.vertex_base", Expected: ep.VertexBase, Actual: ap.VertexBase}
		}
		if ep.IndexBase != ap.IndexBase {
			return &xerr.Mismatch{BatchNumber: batch, PartID: ap.PartID, FieldName: "placements.index_base", Expected: ep.IndexBase, Actual: ap.IndexBase}
		}
		expectedSubs := map[string]model.SubmeshPlacement{}
		for _, s := range ep.Submeshes {
			expectedSubs[s.SubmeshID] = s
		}
		for _, as := range ap.Submeshes {
			es, ok := expectedSubs[as.SubmeshID]
			if !ok {
				return &xerr.Mismatch{BatchNumber: batch, PartID: ap.PartID, FieldName: "submeshes.submesh_id", Actual: as.SubmeshID}
			}
			fields := []struct {
				name string
				e, a any
			}{
				{"vertex_start", es.VertexStart, as.VertexStart},
				{"vertex_end", es.VertexEnd, as.VertexEnd},
				{"index_start", es.IndexStart, as.IndexStart},
				{"index_end", es.IndexEnd, as.IndexEnd},
				{"batch_slot_index", es.BatchSlotIndex, as.BatchSlotIndex},
			}
			for _, f := range fields {
				if !reflect.DeepEqual(f.e, f.a) {
					return &xerr.Mismatch{
						BatchNumber: batch, PartID: ap.PartID, FieldName: "submesh." + f.name,
						Path: "submeshes." + as.SubmeshID, Expected: f.e, Actual: f.a,
					}
				}
			}
		}
	}
	return nil
}

func diffError(mm *xerr.Mismatch) error {
	return &xerr.Error{
		Code: xerr.EDiff, Message: "incremental and full recompute differ", FirstMismatch: mm,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
