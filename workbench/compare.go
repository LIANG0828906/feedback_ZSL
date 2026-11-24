package workbench

import (
	"fmt"
	"strings"

	"yuvench/errs"
	"yuvench/yuv"
)

// CompareRuns compares final images and every committed intermediate plane.
// It reports the first mismatch with its node, plane, stage, pixel, and values.
func CompareRuns(left, right *RunResult, code string) error {
	if code == "" {
		code = errs.CodeDiff
	}
	if left == nil || right == nil {
		return &errs.Error{Code: code, Message: "one comparison side is missing"}
	}
	maxLen := max(len(left.CommitSequence), len(right.CommitSequence))
	for index := 0; index < maxLen; index++ {
		if index >= len(left.CommitSequence) || index >= len(right.CommitSequence) {
			nodeID := ""
			if index < len(left.CommitSequence) {
				nodeID = left.CommitSequence[index].NodeID
			}
			if index < len(right.CommitSequence) {
				nodeID = right.CommitSequence[index].NodeID
			}
			return &errs.Error{Code: code, Message: "stage commit sequence length differs", Node: nodeID}
		}
		a := left.CommitSequence[index]
		b := right.CommitSequence[index]
		if a.NodeID != b.NodeID || a.Seq != b.Seq {
			return &errs.Error{Code: code, Message: "stage commit order differs", Node: a.NodeID, Expected: b.NodeID, Actual: a.NodeID}
		}
		if a.CacheKey != b.CacheKey || a.Summary != b.Summary {
			if mismatch := compareNodeOutputs(a.NodeID, left.Nodes[a.NodeID], right.Nodes[b.NodeID]); mismatch != nil {
				return mismatchWithCode(mismatch, code)
			}
			return &errs.Error{Code: code, Message: "stage summary differs", Node: a.NodeID, Plane: a.Plane, Stage: a.Stage, Left: a.Summary, Right: b.Summary}
		}
	}
	for _, node := range left.CommitSequence {
		if mismatch := compareNodeOutputs(node.NodeID, left.Nodes[node.NodeID], right.Nodes[node.NodeID]); mismatch != nil {
			return mismatchWithCode(mismatch, code)
		}
	}
	if left.Final != nil || right.Final != nil {
		if mismatch := compareComposite("COMPOSITE:interleave", left.Final, right.Final); mismatch != nil {
			return mismatchWithCode(mismatch, code)
		}
	}
	return nil
}

func mismatchWithCode(mismatch *CompareMismatch, code string) *errs.Error {
	x, y := mismatch.X, mismatch.Y
	left, right := mismatch.Left, mismatch.Right
	stage := mismatch.Stage
	if stage == "" {
		if index := strings.IndexByte(mismatch.NodeID, ':'); index >= 0 {
			stage = mismatch.NodeID[index+1:]
		} else {
			stage = mismatch.NodeID
		}
	}
	return &errs.Error{
		Code:         code,
		Message:      "first pixel mismatch",
		Node:         mismatch.NodeID,
		Plane:        mismatch.Plane,
		Stage:        stage,
		Field:        fmt.Sprintf("pixel[%d,%d]", x, y),
		X:            &x,
		Y:            &y,
		Left:         left,
		Right:        right,
		LeftValue:    &left,
		RightValue:   &right,
		LeftSummary:  mismatch.LeftSummary,
		RightSummary: mismatch.RightSummary,
	}
}

func compareNodeOutputs(nodeID string, left, right *NodeOutput) *CompareMismatch {
	if left == nil || right == nil {
		return &CompareMismatch{Code: errs.CodeDiff, NodeID: nodeID, X: 0, Y: 0}
	}
	if left.Plane != nil || right.Plane != nil {
		if left.Plane == nil || right.Plane == nil {
			return &CompareMismatch{Code: errs.CodeDiff, NodeID: nodeID, Plane: planeName(left, right), X: 0, Y: 0}
		}
		return comparePlanes(nodeID, left.Plane, right.Plane)
	}
	if left.Composite != nil || right.Composite != nil {
		return compareComposite(nodeID, left.Composite, right.Composite)
	}
	return nil
}

func planeName(left, right *NodeOutput) string {
	if left != nil && left.Plane != nil {
		return left.Plane.Name
	}
	if right != nil && right.Plane != nil {
		return right.Plane.Name
	}
	return "YUV"
}

func comparePlanes(nodeID string, left, right *yuv.Plane) *CompareMismatch {
	if left.Width != right.Width || left.Height != right.Height || left.BitDepth != right.BitDepth {
		return &CompareMismatch{Code: errs.CodeDiff, NodeID: nodeID, Plane: left.Name, X: 0, Y: 0}
	}
	for y := 0; y < left.Height; y++ {
		for x := 0; x < left.Width; x++ {
			index := y*left.Width + x
			if left.Data[index] != right.Data[index] {
				return &CompareMismatch{
					Code:         errs.CodeDiff,
					NodeID:       nodeID,
					Plane:        left.Name,
					X:            x,
					Y:            y,
					Left:         left.Data[index],
					Right:        right.Data[index],
					LeftSummary:  left.Summary(),
					RightSummary: right.Summary(),
				}
			}
		}
	}
	return nil
}

func compareComposite(nodeID string, left, right *yuv.Composite) *CompareMismatch {
	if left == nil || right == nil {
		return &CompareMismatch{Code: errs.CodeDiff, NodeID: nodeID, Plane: "YUV", X: 0, Y: 0}
	}
	if left.Width != right.Width || left.Height != right.Height || left.BitDepth != right.BitDepth {
		return &CompareMismatch{Code: errs.CodeDiff, NodeID: nodeID, Plane: "YUV", X: 0, Y: 0}
	}
	for _, plane := range []string{"Y", "U", "V"} {
		a, b := left.Channel(plane), right.Channel(plane)
		for y := 0; y < left.Height; y++ {
			for x := 0; x < left.Width; x++ {
				index := y*left.Width + x
				if a[index] != b[index] {
					return &CompareMismatch{
						Code:         errs.CodeDiff,
						NodeID:       nodeID,
						Plane:        plane,
						X:            x,
						Y:            y,
						Left:         a[index],
						Right:        b[index],
						LeftSummary:  left.Summary(),
						RightSummary: right.Summary(),
					}
				}
			}
		}
	}
	return nil
}
