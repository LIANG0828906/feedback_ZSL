// Package batching contains the deterministic batch partition kernel and
// explainable pressure arbitration.
package batching

import (
	"fmt"
	"sort"

	"wardrobekernel/internal/assembly"
	"wardrobekernel/internal/model"
	"wardrobekernel/internal/xerr"
)

// Plan computes assembly conflicts, legal subset and deterministic batches.
func Plan(project *model.Project) (*model.Plan, error) {
	assemblyResult, err := assembly.Evaluate(project, project.CandidateAssembly)
	if err != nil {
		return nil, err
	}
	return PlanLegalAssembly(project, assemblyResult)
}

// PlanLegalAssembly batches an already evaluated assembly. It is used by the
// realtime, recovery and full-recompute paths so all three share one kernel.
func PlanLegalAssembly(project *model.Project, assemblyResult *model.AssemblyResult) (*model.Plan, error) {
	parts := make(map[string]model.Part, len(project.Parts))
	for _, part := range project.Parts {
		parts[part.ID] = part
	}
	ordered := sortCandidates(assemblyResult.LegalAssembly, parts)
	constraints := project.Constraints

	batches := make([]*batchBuilder, 0)
	deferred := make([]model.Part, 0)
	decisions := make([]model.Decision, 0)
	sequence := 0
	nextSequence := func() int { sequence++; return sequence }

	for _, id := range ordered {
		part := parts[id]
		if err := validatePartFitsEmpty(project, part); err != nil {
			return nil, err
		}

		reports := simulateAll(batches, part, project)
		chosen := firstFit(reports)
		if chosen != nil {
			applyToBatch(chosen, part, project, &decisions, nextSequence, project.ConstraintVersion)
			continue
		}
		if arbErr := firstArbitrationError(reports); arbErr != nil {
			return nil, arbErr
		}

		target := pressureTarget(reports)
		if target == nil {
			decisions = append(decisions, model.Decision{
				Sequence: nextSequence(), BatchNumber: len(batches) + 1, CandidateID: part.ID,
				Kind: "SKIN_ISOLATION", Result: model.PressureSplit,
				Basis: []string{
					fmt.Sprintf("allow_cross_skin_batching=%t", constraints.AllowCrossSkinBatch),
					fmt.Sprintf("candidate_skin_key=%s", part.SkinKey()),
					"no compatible existing batch; opening deterministic skin lane",
				},
			})
			batches = appendNewBatch(batches, part, project, &decisions, nextSequence)
			continue
		}

		pressure, err := arbitrateTarget(target, part, project)
		if err != nil {
			return nil, err
		}
		if pressure.Result == model.PressureSplit {
			pressure.Basis = append(pressure.Basis, fmt.Sprintf("new_batch_number=%d", len(batches)+1))
		}
		decisions = append(decisions, model.Decision{
			Sequence: nextSequence(), BatchNumber: target.batch.batch.Number, CandidateID: part.ID,
			IncumbentID: pressure.WinnerID, Kind: target.pressureKind(),
			Result: pressure.Result, Basis: pressure.Basis,
		})

		if pressure.Result == model.PressureDefer {
			deferred = append(deferred, part)
			continue
		}
		batches = appendNewBatch(batches, part, project, &decisions, nextSequence)
	}

	// Deferred parts get a second deterministic pass after the primary lane.
	for _, part := range deferred {
		reports := simulateAll(batches, part, project)
		chosen := firstFit(reports)
		if chosen != nil {
			decisions = append(decisions, model.Decision{
				Sequence: nextSequence(), BatchNumber: chosen.batch.Number, CandidateID: part.ID,
				Kind: "DEFERRED_RETRY", Result: "PLACE",
				Basis: []string{"deferred candidate retried in deterministic priority order", "a previously opened batch had sufficient contiguous capacity"},
			})
			applyToBatch(chosen, part, project, &decisions, nextSequence, project.ConstraintVersion)
			continue
		}
		if arbErr := firstArbitrationError(reports); arbErr != nil {
			return nil, arbErr
		}
		decisions = append(decisions, model.Decision{
			Sequence: nextSequence(), BatchNumber: len(batches) + 1, CandidateID: part.ID,
			Kind: "DEFERRED_RETRY", Result: "NEW_BATCH",
			Basis: []string{"deferred candidate remained unable to fit existing batches", "opening fresh batch preserves all budgets and offsets"},
		})
		batches = appendNewBatch(batches, part, project, &decisions, nextSequence)
	}

	out := make([]model.Batch, 0, len(batches))
	commitSequence := make([]int, 0, len(batches))
	for i, builder := range batches {
		builder.batch.Number = i + 1
		if len(builder.batch.Slots) > 1 {
			// Slot order is append order; normalize shared lists only.
			for j := range builder.batch.Slots {
				sort.Strings(builder.batch.Slots[j].SharedWith)
			}
		}
		out = append(out, builder.batch)
		commitSequence = append(commitSequence, i+1)
	}

	return &model.Plan{
		AlgorithmVersion: project.AlgorithmVersion,
		Assembly:         *assemblyResult,
		Batches:          out,
		Decisions:        decisions,
		CommitSequence:   commitSequence,
	}, nil
}

func sortCandidates(ids []string, parts map[string]model.Part) []string {
	out := append([]string(nil), ids...)
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := parts[out[i]], parts[out[j]]
		if pi.ArbitrationPriority != pj.ArbitrationPriority {
			return pi.ArbitrationPriority > pj.ArbitrationPriority
		}
		return pi.ID < pj.ID
	})
	return out
}

func validatePartFitsEmpty(project *model.Project, part model.Part) error {
	c := project.Constraints
	if part.Vertices > c.MaxVertexBudget {
		return &xerr.Error{
			Code: xerr.EBudget, Retryable: false,
			Message: fmt.Sprintf("part %s vertex size %d exceeds empty batch budget %d", part.ID, part.Vertices, c.MaxVertexBudget),
			Parties: []string{part.ID},
			BasisChain: []string{
				fmt.Sprintf("vertices=%d", part.Vertices),
				fmt.Sprintf("max_vertex_budget=%d", c.MaxVertexBudget),
			},
		}
	}
	indexCount := submeshIndexCount(part)
	if indexCount > c.MaxIndexBudget {
		return &xerr.Error{
			Code: xerr.EBudget, Retryable: false,
			Message: fmt.Sprintf("part %s index size %d exceeds empty batch budget %d", part.ID, indexCount, c.MaxIndexBudget),
			Parties: []string{part.ID},
			BasisChain: []string{
				fmt.Sprintf("submesh_indices=%d", indexCount),
				fmt.Sprintf("max_index_budget=%d", c.MaxIndexBudget),
			},
		}
	}
	if len(part.MaterialSlots) > c.MaxMaterialSlots {
		return &xerr.Error{
			Code: xerr.EBudget, Retryable: false,
			Message: fmt.Sprintf("part %s needs %d material slots, batch maximum is %d", part.ID, len(part.MaterialSlots), c.MaxMaterialSlots),
			Parties: []string{part.ID},
			BasisChain: []string{
				fmt.Sprintf("material_slots=%d", len(part.MaterialSlots)),
				fmt.Sprintf("max_material_slots=%d", c.MaxMaterialSlots),
			},
		}
	}
	return nil
}

func submeshIndexCount(part model.Part) int {
	total := 0
	for _, sm := range part.Submeshes {
		total += sm.IndexCount
	}
	return total
}

type batchBuilder struct {
	batch model.Batch
}

func newBatch(part model.Part) *batchBuilder {
	return &batchBuilder{batch: model.Batch{
		Number:  0,
		SkinKey: part.SkinKey(),
		PartIDs: []string{part.ID},
		Slots:   []model.SlotAllocation{},
	}}
}

func appendNewBatch(batches []*batchBuilder, part model.Part, project *model.Project,
	decisions *[]model.Decision, nextSequence func() int,
) []*batchBuilder {
	builder := &batchBuilder{batch: model.Batch{
		Number:  len(batches) + 1,
		SkinKey: part.SkinKey(),
		Slots:   []model.SlotAllocation{},
	}}
	applyToBatch(builder, part, project, decisions, nextSequence, project.ConstraintVersion)
	builder.batch.Number = len(batches) + 1
	return append(batches, builder)
}

type slotContention struct {
	logicalSlotID string
	slot          model.MaterialSlot
	owner         model.Part
}

type fitReport struct {
	batch          *batchBuilder
	skinOK         bool
	vertexOK       bool
	indexOK        bool
	slotsOK        bool
	newSlotCount   int
	contentions    []slotContention
	arbError       *xerr.Error
	vertexRequired int
	indexRequired  int
}

func (r fitReport) fits() bool {
	return r.skinOK && r.vertexOK && r.indexOK && r.slotsOK && r.arbError == nil
}

func (r fitReport) pressureKind() string {
	if len(r.contentions) > 0 && !r.slotsOK {
		return "MATERIAL_SLOT_CONTENTION"
	}
	if !r.vertexOK || !r.indexOK {
		return "BUDGET_SQUEEZE"
	}
	return "MATERIAL_SLOT_CAPACITY"
}

func simulateAll(batches []*batchBuilder, part model.Part, project *model.Project) []fitReport {
	reports := make([]fitReport, 0, len(batches))
	for _, batch := range batches {
		reports = append(reports, simulateFit(batch, part, project))
	}
	return reports
}

func simulateFit(batch *batchBuilder, part model.Part, project *model.Project) fitReport {
	c := project.Constraints
	report := fitReport{
		batch:          batch,
		skinOK:         c.AllowCrossSkinBatch || batch.batch.SkinKey == part.SkinKey(),
		vertexOK:       batch.batch.VertexUsed+part.Vertices <= c.MaxVertexBudget,
		indexRequired:  submeshIndexCount(part),
		vertexRequired: part.Vertices,
	}
	report.indexOK = batch.batch.IndexUsed+report.indexRequired <= c.MaxIndexBudget

	slots := sortedMaterialSlots(part)
	for _, slot := range slots {
		exact := false
		var strongest *model.Part
		for _, alloc := range batch.batch.Slots {
			if alloc.LogicalSlotID != slot.SlotID {
				continue
			}
			if alloc.MaterialID == slot.MaterialID && alloc.MaterialVersion == slot.MaterialVersion {
				exact = true
				continue
			}
			owner, ok := partByID(project, alloc.OwnerPartID)
			if !ok {
				continue
			}
			if strongest == nil || owner.ArbitrationPriority > strongest.ArbitrationPriority ||
				(owner.ArbitrationPriority == strongest.ArbitrationPriority && owner.ID < strongest.ID) {
				ownerCopy := owner
				strongest = &ownerCopy
			}
		}
		if exact {
			continue
		}
		report.newSlotCount++
		if strongest != nil {
			report.contentions = append(report.contentions, slotContention{
				logicalSlotID: slot.SlotID, slot: slot, owner: *strongest,
			})
			if c.Arbitration.TiePolicy == model.TieStrict &&
				strongest.ArbitrationPriority == part.ArbitrationPriority {
				_, err := ArbitratePressure(PressureRequest{
					BatchNumber: batch.batch.Number, Kind: "MATERIAL_SLOT_CONTENTION",
					Candidate: part, Incumbent: strongest, TiePolicy: model.TieStrict,
					ConstraintVersion: project.ConstraintVersion,
				})
				if coded, ok := xerr.AsError(err); ok {
					report.arbError = coded
				}
			}
		}
	}
	report.slotsOK = len(batch.batch.Slots)+report.newSlotCount <= c.MaxMaterialSlots
	return report
}

func partByID(project *model.Project, id string) (model.Part, bool) {
	for _, part := range project.Parts {
		if part.ID == id {
			return part, true
		}
	}
	return model.Part{}, false
}

func firstFit(reports []fitReport) *batchBuilder {
	for i := range reports {
		if reports[i].fits() {
			return reports[i].batch
		}
	}
	return nil
}

func firstArbitrationError(reports []fitReport) error {
	for i := range reports {
		if reports[i].arbError != nil {
			return reports[i].arbError
		}
	}
	return nil
}

func pressureTarget(reports []fitReport) *fitReport {
	best := -1
	bestScore := 0
	for i := range reports {
		r := &reports[i]
		if !r.skinOK {
			continue
		}
		if r.vertexOK && r.indexOK && r.slotsOK {
			continue
		}
		score := pressureScore(*r)
		if best == -1 || score < bestScore || (score == bestScore && r.batch.batch.Number < reports[best].batch.batch.Number) {
			best = i
			bestScore = score
		}
	}
	if best == -1 {
		return nil
	}
	return &reports[best]
}

func pressureScore(r fitReport) int {
	vertexOverage := 0
	if !r.vertexOK {
		vertexOverage = 1
	}
	indexOverage := 0
	if !r.indexOK {
		indexOverage = 1
	}
	slotOverage := 0
	if !r.slotsOK {
		slotOverage = 1
	}
	// Fit reports already carry booleans; exact amounts are added by the
	// planner's arbitration basis. A small deterministic rank is enough to
	// choose the most feasible pressure target in v1.
	return vertexOverage*4 + indexOverage*2 + slotOverage
}

func arbitrateTarget(target *fitReport, part model.Part, project *model.Project) (PressureResult, error) {
	c := project.Constraints
	kind := target.pressureKind()
	var incumbent *model.Part
	used := target.batch.batch.VertexUsed
	budget := c.MaxVertexBudget
	required := target.vertexRequired
	if kind == "MATERIAL_SLOT_CONTENTION" && len(target.contentions) > 0 {
		owner := target.contentions[0].owner
		incumbent = &owner
	} else if kind == "BUDGET_SQUEEZE" {
		strongest := strongestPart(target.batch, project)
		incumbent = strongest
		if !target.vertexOK {
			used, budget, required = target.batch.batch.VertexUsed, c.MaxVertexBudget, target.vertexRequired
		} else {
			used, budget, required = target.batch.batch.IndexUsed, c.MaxIndexBudget, target.indexRequired
		}
	} else {
		incumbent = strongestPart(target.batch, project)
	}

	result, err := ArbitratePressure(PressureRequest{
		BatchNumber: target.batch.batch.Number,
		Kind:        kind, Candidate: part, Incumbent: incumbent,
		TiePolicy: c.Arbitration.TiePolicy, CandidateRequired: required,
		BatchUsed: used, Budget: budget,
		AvailableSlots:    c.MaxMaterialSlots - len(target.batch.batch.Slots),
		NeededSlots:       target.newSlotCount,
		ConstraintVersion: project.ConstraintVersion,
	})
	if err != nil {
		return PressureResult{}, err
	}
	if kind == "BUDGET_SQUEEZE" && target.vertexOK == false && target.indexOK == false {
		result.Basis = append(result.Basis, fmt.Sprintf("index pressure also present used=%d required=%d budget=%d", target.batch.batch.IndexUsed, target.indexRequired, c.MaxIndexBudget))
	}
	return result, nil
}

func strongestPart(batch *batchBuilder, project *model.Project) *model.Part {
	var strongest *model.Part
	for _, id := range batch.batch.PartIDs {
		part, ok := partByID(project, id)
		if !ok {
			continue
		}
		if strongest == nil || part.ArbitrationPriority > strongest.ArbitrationPriority ||
			(part.ArbitrationPriority == strongest.ArbitrationPriority && part.ID < strongest.ID) {
			copy := part
			strongest = &copy
		}
	}
	return strongest
}

func applyToBatch(batch *batchBuilder, part model.Part, project *model.Project,
	decisions *[]model.Decision, nextSequence func() int, constraintVersion string,
) {
	if batch.batch.SkinKey != part.SkinKey() {
		batch.batch.SkinKey = "mixed"
	}
	batch.batch.PartIDs = append(batch.batch.PartIDs, part.ID)

	slotIndexByDeclared := make(map[string]int, len(part.MaterialSlots))
	for _, slot := range sortedMaterialSlots(part) {
		exactIndex := -1
		contentionIndex := -1
		var contentionAlloc model.SlotAllocation
		for i, alloc := range batch.batch.Slots {
			if alloc.LogicalSlotID != slot.SlotID {
				continue
			}
			if alloc.MaterialID == slot.MaterialID && alloc.MaterialVersion == slot.MaterialVersion {
				exactIndex = i
				break
			}
			if contentionIndex == -1 {
				contentionIndex = i
				contentionAlloc = alloc
			}
		}

		if exactIndex >= 0 {
			alloc := &batch.batch.Slots[exactIndex]
			alloc.SharedWith = append(alloc.SharedWith, part.ID)
			slotIndexByDeclared[slot.SlotID] = exactIndex
			*decisions = append(*decisions, model.Decision{
				Sequence: nextSequence(), BatchNumber: batch.batch.Number, CandidateID: part.ID,
				IncumbentID: alloc.OwnerPartID, Kind: "MATERIAL_SLOT_SHARE", Result: "SHARE",
				Basis: []string{
					fmt.Sprintf("logical_slot=%s", slot.SlotID),
					fmt.Sprintf("material=%s@%s", slot.MaterialID, slot.MaterialVersion),
					"matching material version reuses batch slot",
				},
			})
			continue
		}

		newIndex := len(batch.batch.Slots)
		if contentionIndex >= 0 {
			owner, _ := partByID(project, contentionAlloc.OwnerPartID)
			basis := []string{
				fmt.Sprintf("logical_slot=%s", slot.SlotID),
				fmt.Sprintf("incumbent_material=%s@%s", contentionAlloc.MaterialID, contentionAlloc.MaterialVersion),
				fmt.Sprintf("candidate_material=%s@%s", slot.MaterialID, slot.MaterialVersion),
				fmt.Sprintf("incumbent=%s priority=%d", owner.ID, owner.ArbitrationPriority),
				fmt.Sprintf("candidate=%s priority=%d", part.ID, part.ArbitrationPriority),
			}
			if owner.ArbitrationPriority == part.ArbitrationPriority {
				basis = append(basis, fmt.Sprintf("tie_break=lexicographic_id smaller id owns requested slot; owner=%s", owner.ID))
			} else if owner.ArbitrationPriority > part.ArbitrationPriority {
				basis = append(basis, "higher priority incumbent owns requested slot")
			}
			basis = append(basis, "candidate material is remapped to a fresh batch material slot")
			*decisions = append(*decisions, model.Decision{
				Sequence: nextSequence(), BatchNumber: batch.batch.Number, CandidateID: part.ID,
				IncumbentID: owner.ID, Kind: "MATERIAL_SLOT_CONTENTION", Result: "REMAP_CANDIDATE", Basis: basis,
			})
			batch.batch.Slots = append(batch.batch.Slots, model.SlotAllocation{
				SlotID: slot.SlotID + "#remap-" + part.ID, LogicalSlotID: slot.SlotID,
				BatchSlotIndex: newIndex, MaterialID: slot.MaterialID, MaterialVersion: slot.MaterialVersion,
				OwnerPartID: part.ID, RemappedFor: part.ID,
			})
		} else {
			batch.batch.Slots = append(batch.batch.Slots, model.SlotAllocation{
				SlotID: slot.SlotID, LogicalSlotID: slot.SlotID, BatchSlotIndex: newIndex,
				MaterialID: slot.MaterialID, MaterialVersion: slot.MaterialVersion, OwnerPartID: part.ID,
			})
		}
		slotIndexByDeclared[slot.SlotID] = newIndex
	}

	placement := model.PartPlacement{PartID: part.ID, VertexBase: batch.batch.VertexUsed, IndexBase: batch.batch.IndexUsed}
	indexCursor := batch.batch.IndexUsed
	for _, sm := range sortedSubmeshes(part) {
		batchSlotIndex := slotIndexByDeclared[sm.MaterialSlotID]
		start := placement.VertexBase + sm.VertexStart
		indexStart := indexCursor
		indexCursor += sm.IndexCount
		placement.Submeshes = append(placement.Submeshes, model.SubmeshPlacement{
			SubmeshID: sm.ID, PartID: part.ID, MaterialSlotID: sm.MaterialSlotID,
			BatchSlotIndex: batchSlotIndex, VertexBase: placement.VertexBase,
			SourceVertexStart: sm.VertexStart, VertexStart: start, VertexEnd: start + sm.VertexCount,
			SourceIndexStart: sm.IndexStart, IndexStart: indexStart, IndexEnd: indexStart + sm.IndexCount,
			IndexCount: sm.IndexCount,
		})
	}
	batch.batch.Placements = append(batch.batch.Placements, placement)
	batch.batch.VertexUsed += part.Vertices
	batch.batch.IndexUsed = indexCursor
	_ = constraintVersion
}

func sortedMaterialSlots(part model.Part) []model.MaterialSlot {
	out := append([]model.MaterialSlot(nil), part.MaterialSlots...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].SlotID < out[j].SlotID })
	return out
}

func sortedSubmeshes(part model.Part) []model.Submesh {
	out := append([]model.Submesh(nil), part.Submeshes...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].VertexStart != out[j].VertexStart {
			return out[i].VertexStart < out[j].VertexStart
		}
		return out[i].ID < out[j].ID
	})
	return out
}
