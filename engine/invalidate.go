package engine

import (
	"context"
	"fmt"
	"sort"

	"maskqc/model"
	"maskqc/raster"
)

func (e *Engine) ApplyStrokeEdit(ctx context.Context, updated model.Stroke) (*RunResult, error) {
	<-e.mu
	oldIndex := -1
	for index, stroke := range e.Project.Strokes {
		if stroke.ID == updated.ID {
			oldIndex = index
			break
		}
	}
	if oldIndex < 0 {
		e.mu <- struct{}{}
		return nil, &model.Error{
			Code: model.CodeSchema, Path: "stroke.id", ObjectID: updated.ID,
			Field: "id", Message: "stroke does not exist in the project",
		}
	}
	if err := raster.ValidateStroke(updated); err != nil {
		e.mu <- struct{}{}
		return nil, err
	}
	oldStroke := e.Project.Strokes[oldIndex]
	newStrokes := append([]model.Stroke(nil), e.Project.Strokes...)
	updated.SourceVersion = oldStroke.SourceVersion
	newStrokes[oldIndex] = updated
	invalidatedSet := map[int]bool{}
	reasons := make([]string, 0)
	affectedStrokes := []string{updated.ID}
	oldBlocks := e.blocksForRect(oldStroke.Bounds())
	newBlocks := e.blocksForRect(updated.Bounds())
	if !sameIntSet(oldBlocks, newBlocks) || oldStroke.Hardness != updated.Hardness || oldStroke.Weight != updated.Weight ||
		oldStroke.Radius != updated.Radius ||
		oldStroke.X0 != updated.X0 || oldStroke.Y0 != updated.Y0 || oldStroke.X1 != updated.X1 || oldStroke.Y1 != updated.Y1 {
		for _, index := range append(append([]int(nil), oldBlocks...), newBlocks...) {
			invalidatedSet[index] = true
		}
		reasons = append(reasons, "stroke geometry, hardness, or weight changed")
	}
	if oldStroke.Priority != updated.Priority || oldStroke.CompositeOrder != updated.CompositeOrder || oldStroke.Seq != updated.Seq {
		orderAffected, affected := orderChangedBlocks(e.Project.Strokes, newStrokes, e.config.BlockSize, e.Project.Width, e.Project.Height)
		for index := range orderAffected {
			invalidatedSet[index] = true
		}
		affectedStrokes = append(affectedStrokes, affected...)
		reasons = append(reasons, "composite order changed; only blocks touched by inverted overlapping pairs are invalidated")
	}
	invalidated := make([]int, 0, len(invalidatedSet))
	for index := range invalidatedSet {
		invalidated = append(invalidated, index)
	}
	sort.Ints(invalidated)
	sort.Strings(affectedStrokes)
	affectedStrokes = uniqueStrings(affectedStrokes)
	report := &InvalidationReport{
		StrokeID: updated.ID, OldSignature: oldStroke.Signature(), NewSignature: updated.Signature(),
		AffectedStrokes: affectedStrokes, InvalidatedBlocks: invalidated,
		Downstream: []string{"verify:topology", "verify:direction"},
		Reasons:    reasons,
	}
	if e.store != nil && len(invalidated) > 0 {
		if err := e.store.InvalidateBlocks(e.Project.ID, invalidated, "stroke edit "+updated.ID); err != nil {
			e.mu <- struct{}{}
			return nil, err
		}
	}
	for _, index := range invalidated {
		delete(e.cache, index)
	}
	e.Project.Strokes = newStrokes
	e.Project.Normalize()
	e.pendingInvalidation = report
	e.mu <- struct{}{}
	result, err := e.Run(ctx, ModeIncremental)
	if result != nil && result.Invalidation == nil {
		result.Invalidation = report
		result.InvalidatedBlocks = invalidated
	}
	return result, err
}

func (e *Engine) blocksForRect(rect model.Rect) []int {
	rect = rect.Clip(e.Project.Width, e.Project.Height)
	if rect.Empty() {
		return nil
	}
	size := e.config.BlockSize
	columns := (e.Project.Width + size - 1) / size
	minBlockX := rect.MinX / size
	maxBlockX := rect.MaxX / size
	minBlockY := rect.MinY / size
	maxBlockY := rect.MaxY / size
	indices := make([]int, 0, (maxBlockX-minBlockX+1)*(maxBlockY-minBlockY+1))
	for blockY := minBlockY; blockY <= maxBlockY; blockY++ {
		for blockX := minBlockX; blockX <= maxBlockX; blockX++ {
			indices = append(indices, blockY*columns+blockX)
		}
	}
	return indices
}

func orderChangedBlocks(oldStrokes, newStrokes []model.Stroke, blockSize, width, height int) (map[int]bool, []string) {
	oldOrder := model.OrderedStrokes(oldStrokes)
	newOrder := model.OrderedStrokes(newStrokes)
	oldPosition := map[string]int{}
	newPosition := map[string]int{}
	for index, stroke := range oldOrder {
		oldPosition[stroke.ID] = index
	}
	for index, stroke := range newOrder {
		newPosition[stroke.ID] = index
	}
	byID := map[string]model.Stroke{}
	for _, stroke := range newStrokes {
		byID[stroke.ID] = stroke
	}
	affected := map[string]bool{}
	columns := (width + blockSize - 1) / blockSize
	blocks := map[int]bool{}
	for i := 0; i < len(newOrder); i++ {
		for j := i + 1; j < len(newOrder); j++ {
			left, right := newOrder[i], newOrder[j]
			oldLeft, oldRight := oldPosition[left.ID], oldPosition[right.ID]
			if (oldLeft < oldRight) == (newPosition[left.ID] < newPosition[right.ID]) {
				continue
			}
			overlap := left.Bounds().Intersect(right.Bounds())
			if overlap.Empty() {
				continue
			}
			affected[left.ID] = true
			affected[right.ID] = true
			rect := overlap.Clip(width, height)
			if rect.Empty() {
				continue
			}
			for blockY := rect.MinY / blockSize; blockY <= rect.MaxY/blockSize; blockY++ {
				for blockX := rect.MinX / blockSize; blockX <= rect.MaxX/blockSize; blockX++ {
					blocks[blockY*columns+blockX] = true
				}
			}
		}
	}
	ids := make([]string, 0, len(affected))
	for id := range affected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return blocks, ids
}

func sameIntSet(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]int(nil), left...)
	rightCopy := append([]int(nil), right...)
	sort.Ints(leftCopy)
	sort.Ints(rightCopy)
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	output := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		output = append(output, value)
	}
	sort.Strings(output)
	return output
}

func DescribeInvalidation(report *InvalidationReport) string {
	if report == nil {
		return "none"
	}
	return fmt.Sprintf("stroke=%s blocks=%v downstream=%v", report.StrokeID, report.InvalidatedBlocks, report.Downstream)
}
