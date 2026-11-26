package batching

import (
	"fmt"

	"wardrobekernel/internal/model"
	"wardrobekernel/internal/xerr"
)

// RebuildBatch recomputes one batch from an explicit ordered member list.
// It is used by the dependency-aware incremental scheduler. The member list is
// normally the invalidated batch's previous partition. If the edited part no
// longer fits, the caller falls back to a full deterministic repartition.
func RebuildBatch(project *model.Project, batchNumber int, partIDs []string) (model.Batch, error) {
	partByID := map[string]model.Part{}
	for _, part := range project.Parts {
		partByID[part.ID] = part
	}
	builder := &batchBuilder{batch: model.Batch{
		Number: batchNumber,
		Slots:  []model.SlotAllocation{},
	}}
	decisions := []model.Decision{}
	sequence := 0
	nextSequence := func() int { sequence++; return sequence }

	for _, id := range partIDs {
		part, ok := partByID[id]
		if !ok {
			return model.Batch{}, &xerr.Error{Code: xerr.EState, Message: "rebuild references unknown part " + id}
		}
		if err := validatePartFitsEmpty(project, part); err != nil {
			return model.Batch{}, err
		}
		if len(builder.batch.PartIDs) == 0 {
			builder.batch.SkinKey = part.SkinKey()
		}
		report := simulateFit(builder, part, project)
		if report.arbError != nil {
			return model.Batch{}, report.arbError
		}
		if !report.fits() {
			return model.Batch{}, &xerr.Error{
				Code: xerr.EBudget, Retryable: true,
				Message: fmt.Sprintf("batch %d cannot absorb edited part %s during local rebuild", batchNumber, id),
				Parties: []string{id},
				BasisChain: []string{
					fmt.Sprintf("vertex_ok=%t", report.vertexOK),
					fmt.Sprintf("index_ok=%t", report.indexOK),
					fmt.Sprintf("material_slots_ok=%t", report.slotsOK),
					"caller should run full repartition and reconcile downstream batches",
				},
			}
		}
		applyToBatch(builder, part, project, &decisions, nextSequence, project.ConstraintVersion)
	}
	return builder.batch, nil
}
