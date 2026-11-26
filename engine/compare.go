package engine

import (
	"fmt"

	"maskqc/model"
)

type RunSummary struct {
	Mode              Mode            `json:"mode"`
	ProjectID         string          `json:"projectId"`
	AccumulatedHash   string          `json:"accumulatedHash"`
	CommitSequence    []int           `json:"commitSequence"`
	ReusedBlocks      []int           `json:"reusedBlocks"`
	InvalidatedBlocks []int           `json:"invalidatedBlocks"`
	DomainCount       int             `json:"domainCount"`
	Domains           []DomainSummary `json:"domains"`
	Issues            model.Errors    `json:"issues"`
	Budget            BudgetReport    `json:"budget"`
	ElapsedMillis     int64           `json:"elapsedMillis"`
	CheckpointInvalid bool            `json:"checkpointInvalid"`
	CheckpointReason  string          `json:"checkpointReason,omitempty"`
	StrokeFailures    []string        `json:"strokeFailures,omitempty"`
}

type DomainSummary struct {
	ID        string          `json:"id"`
	Area      int             `json:"area"`
	CentroidX float64         `json:"centroidX"`
	CentroidY float64         `json:"centroidY"`
	BBox      model.Rect      `json:"bbox"`
	Holes     int             `json:"holes"`
	RingWidth int             `json:"ringWidth"`
	Direction model.Direction `json:"direction"`
}

func Summarize(result *RunResult) RunSummary {
	if result == nil {
		return RunSummary{}
	}
	summary := RunSummary{
		Mode: result.Mode, ProjectID: result.ProjectID, AccumulatedHash: result.AccumulatedHash,
		CommitSequence:    append([]int(nil), result.CommitSequence...),
		ReusedBlocks:      append([]int(nil), result.ReusedBlocks...),
		InvalidatedBlocks: append([]int(nil), result.InvalidatedBlocks...),
		Issues:            result.Issues, Budget: result.Budget, ElapsedMillis: result.ElapsedMillis,
		CheckpointInvalid: result.CheckpointInvalid, CheckpointReason: result.CheckpointReason,
		StrokeFailures: append([]string(nil), result.StrokeFailures...),
	}
	if result.Analysis != nil {
		summary.DomainCount = len(result.Analysis.Domains)
		for _, domain := range result.Analysis.Domains {
			summary.Domains = append(summary.Domains, DomainSummary{
				ID: domain.ID, Area: domain.Area, CentroidX: domain.CentroidX, CentroidY: domain.CentroidY,
				BBox: domain.BBox, Holes: domain.Holes, RingWidth: domain.RingWidth, Direction: domain.Direction,
			})
		}
	}
	return summary
}

func CompareRuns(left, right *RunResult) *Mismatch {
	if left == nil || right == nil {
		return &Mismatch{BlockIndex: -1, Reason: "one run result is nil"}
	}
	if left.ProjectID != right.ProjectID {
		return &Mismatch{BlockIndex: -1, Reason: "project id differs"}
	}
	if len(left.CommitSequence) != len(right.CommitSequence) {
		index := min(len(left.CommitSequence), len(right.CommitSequence))
		blockIndex := -1
		if index < len(left.CommitSequence) {
			blockIndex = left.CommitSequence[index]
		} else if index < len(right.CommitSequence) {
			blockIndex = right.CommitSequence[index]
		}
		return &Mismatch{BlockIndex: blockIndex, Reason: "commit sequence length differs"}
	}
	for index := range left.CommitSequence {
		if left.CommitSequence[index] != right.CommitSequence[index] {
			return &Mismatch{
				BlockIndex: left.CommitSequence[index],
				Reason:     fmt.Sprintf("commit sequence differs at position %d: %d != %d", index, left.CommitSequence[index], right.CommitSequence[index]),
			}
		}
	}
	leftBlocks := map[int]BlockResult{}
	for _, block := range left.Blocks {
		leftBlocks[block.Index] = block
	}
	rightBlocks := map[int]BlockResult{}
	for _, block := range right.Blocks {
		rightBlocks[block.Index] = block
	}
	if len(leftBlocks) != len(rightBlocks) {
		return &Mismatch{BlockIndex: -1, Reason: "block count differs"}
	}
	for index := 0; index < len(leftBlocks); index++ {
		leftBlock, leftOK := leftBlocks[index]
		rightBlock, rightOK := rightBlocks[index]
		if !leftOK || !rightOK {
			return &Mismatch{BlockIndex: index, Reason: "block is missing from one run"}
		}
		if leftBlock.CacheKey != rightBlock.CacheKey {
			return &Mismatch{BlockIndex: index, Reason: "block cache key differs"}
		}
		if len(leftBlock.Mask) != len(rightBlock.Mask) {
			return &Mismatch{BlockIndex: index, Reason: "block mask length differs"}
		}
		for offset := range leftBlock.Mask {
			if leftBlock.Mask[offset] != rightBlock.Mask[offset] {
				return &Mismatch{
					BlockIndex: index, X: leftBlock.Rect.MinX + offset%leftBlock.Width,
					Y:      leftBlock.Rect.MinY + offset/leftBlock.Width,
					Reason: fmt.Sprintf("pixel differs: %d != %d", leftBlock.Mask[offset], rightBlock.Mask[offset]),
				}
			}
		}
	}
	if len(left.Accumulated) != len(right.Accumulated) {
		return &Mismatch{BlockIndex: -1, Reason: "accumulated mask length differs"}
	}
	for index := range left.Accumulated {
		if left.Accumulated[index] != right.Accumulated[index] {
			blockIndex := -1
			width := left.Width
			for _, block := range left.Blocks {
				x, y := index%width, index/width
				if x >= block.Rect.MinX && x <= block.Rect.MaxX && y >= block.Rect.MinY && y <= block.Rect.MaxY {
					blockIndex = block.Index
					return &Mismatch{BlockIndex: blockIndex, X: x, Y: y, Reason: "accumulated pixel differs"}
				}
			}
			return &Mismatch{BlockIndex: blockIndex, X: index % width, Y: index / width, Reason: "accumulated pixel differs"}
		}
	}
	if left.OrderVersion != right.OrderVersion {
		return &Mismatch{BlockIndex: -1, Reason: "composite order version differs"}
	}
	leftDomains := summarizeDomains(left)
	rightDomains := summarizeDomains(right)
	if leftDomains != rightDomains {
		return &Mismatch{BlockIndex: -1, Reason: "connected topology or direction summary differs"}
	}
	return nil
}

func summarizeDomains(result *RunResult) string {
	if result == nil || result.Analysis == nil {
		return "<nil>"
	}
	return model.HashJSON(result.Analysis.Domains)
}
