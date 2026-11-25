package channelpipe

import (
	"fmt"
	"reflect"
)

type StageEdge struct {
	From int    `json:"from"`
	To   int    `json:"to"`
	Type string `json:"type"`
}

type RegionDependency struct {
	StageIndex int  `json:"stage_index"`
	ChunkIndex int  `json:"chunk_index"`
	Region     Rect `json:"region"`
}

type DependencyGraph struct {
	PlanID             string             `json:"plan_id"`
	StageCount         int                `json:"stage_count"`
	StageEdges         []StageEdge        `json:"stage_edges"`
	RegionDependencies []RegionDependency `json:"region_dependencies"`
}

type InvalidationItem struct {
	PlanID     string   `json:"plan_id"`
	StageIndex int      `json:"stage_index"`
	StageID    string   `json:"stage_id"`
	ChunkIndex int      `json:"chunk_index"`
	Region     Rect     `json:"region"`
	Reasons    []string `json:"reasons"`
}

type InvalidationReport struct {
	PlanID            string             `json:"plan_id"`
	ChangedStageCount int                `json:"changed_stage_count"`
	Items             []InvalidationItem `json:"items"`
	Graph             DependencyGraph    `json:"graph"`
}

func BuildDependencyGraph(plan Plan, chunks []Chunk) DependencyGraph {
	graph := DependencyGraph{PlanID: plan.ID, StageCount: len(plan.Stages)}
	for i := 1; i < len(plan.Stages); i++ {
		graph.StageEdges = append(graph.StageEdges, StageEdge{From: i - 1, To: i, Type: "sequential-output"})
	}
	for stageIndex := range plan.Stages {
		for _, chunk := range chunks {
			graph.RegionDependencies = append(graph.RegionDependencies, RegionDependency{
				StageIndex: stageIndex,
				ChunkIndex: chunk.Index,
				Region:     chunk.Rect,
			})
		}
	}
	return graph
}

func InvalidatePlanEdit(oldPlan, newPlan Plan, chunks []Chunk) (*InvalidationReport, error) {
	if oldPlan.ID != newPlan.ID {
		return nil, FieldError(CodeInvalid, "id", "cannot compare different plans %q and %q", oldPlan.ID, newPlan.ID)
	}
	if err := ValidatePlan(&newPlan); err != nil {
		return nil, err
	}
	report := &InvalidationReport{PlanID: newPlan.ID, Graph: BuildDependencyGraph(newPlan, chunks)}
	reasonsByIndex := map[int][]string{}

	oldIDs := stageIDs(oldPlan)
	newIDs := stageIDs(newPlan)
	orderChanged := !reflect.DeepEqual(oldIDs, newIDs)
	firstOrder := -1
	if orderChanged {
		limit := len(oldIDs)
		if len(newIDs) < limit {
			limit = len(newIDs)
		}
		for i := 0; i < limit; i++ {
			if oldIDs[i] != newIDs[i] {
				firstOrder = i
				break
			}
		}
		if firstOrder < 0 {
			firstOrder = limit
		}
		for i := firstOrder; i < len(newPlan.Stages); i++ {
			reasonsByIndex[i] = append(reasonsByIndex[i], "stage-order-or-dependency")
		}
	}

	oldByID := map[string]Stage{}
	for _, stage := range oldPlan.Stages {
		oldByID[stage.ID] = stage
	}
	for newIndex, newStage := range newPlan.Stages {
		oldStage, ok := oldByID[newStage.ID]
		if !ok {
			reasonsByIndex[newIndex] = append(reasonsByIndex[newIndex], "stage-added")
			continue
		}
		if reason := stageEditReason(oldStage, newStage); reason != "" {
			reasonsByIndex[newIndex] = append(reasonsByIndex[newIndex], reason)
		}
	}

	earliest := -1
	for index := range reasonsByIndex {
		if earliest == -1 || index < earliest {
			earliest = index
		}
	}
	if earliest == -1 {
		return report, nil
	}

	for stageIndex := earliest; stageIndex < len(newPlan.Stages); stageIndex++ {
		reasons := append([]string(nil), reasonsByIndex[stageIndex]...)
		if len(reasons) == 0 {
			reasons = []string{fmt.Sprintf("upstream-stage:%s", newPlan.Stages[earliest].ID)}
		}
		report.ChangedStageCount++
		for _, chunk := range chunks {
			report.Items = append(report.Items, InvalidationItem{
				PlanID:     newPlan.ID,
				StageIndex: stageIndex,
				StageID:    newPlan.Stages[stageIndex].ID,
				ChunkIndex: chunk.Index,
				Region:     chunk.Rect,
				Reasons:    reasons,
			})
		}
	}
	return report, nil
}

func InvalidateRegionEdit(plan Plan, chunks []Chunk, edited Rect) *InvalidationReport {
	report := &InvalidationReport{PlanID: plan.ID, Graph: BuildDependencyGraph(plan, chunks)}
	for stageIndex := range plan.Stages {
		for _, chunk := range chunks {
			if !chunk.Rect.Intersects(edited) {
				continue
			}
			report.Items = append(report.Items, InvalidationItem{
				PlanID:     plan.ID,
				StageIndex: stageIndex,
				StageID:    plan.Stages[stageIndex].ID,
				ChunkIndex: chunk.Index,
				Region:     chunk.Rect,
				Reasons:    []string{"source-region-content"},
			})
		}
	}
	return report
}

func stageIDs(plan Plan) []string {
	ids := make([]string, len(plan.Stages))
	for i, stage := range plan.Stages {
		ids[i] = stage.ID
	}
	return ids
}

func stageEditReason(oldStage, newStage Stage) string {
	if oldStage.TargetDepth != newStage.TargetDepth {
		return "parameter:target_depth"
	}
	if oldStage.Rounding != newStage.Rounding {
		return "parameter:rounding"
	}
	if !reflect.DeepEqual(oldStage.TargetChannels, newStage.TargetChannels) || !reflect.DeepEqual(oldStage.Swizzle, newStage.Swizzle) {
		return "parameter:channel_swizzle"
	}
	if !reflect.DeepEqual(normalizedFills(oldStage), normalizedFills(newStage)) {
		return "parameter:fill_policy"
	}
	if oldStage.DropPolicy != newStage.DropPolicy {
		return "parameter:drop_policy"
	}
	if oldStage.AlphaMode != newStage.AlphaMode {
		return "parameter:alpha_mode"
	}
	return ""
}

func normalizedFills(stage Stage) []FillSpec {
	result := make([]FillSpec, len(stage.TargetChannels))
	for i := range stage.TargetChannels {
		if i < len(stage.Fills) {
			fill := stage.Fills[i]
			if fill.Kind == FillConstant {
				fill.Value = normalizedFillValue(fill, stage.TargetDepth)
			} else if fill.Kind == FillZero || fill.Kind == FillOpaque {
				fill.Value = normalizedFillValue(fill, stage.TargetDepth)
				fill.Kind = FillConstant
			}
			result[i] = fill
		}
	}
	return result
}
