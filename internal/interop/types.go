package interop

import (
	"fmt"
	"sort"

	"saveinterop/internal/core"
)

type Trace struct {
	Direction    string `json:"direction"`
	SourceBranch string `json:"sourceBranch"`
	TargetBranch string `json:"targetBranch"`
	SourcePath   string `json:"sourcePath"`
	TargetPath   string `json:"targetPath"`
	SemanticRole string `json:"semanticRole"`
	RuleVersion  string `json:"ruleVersion"`
	Status       string `json:"status"`
}

type Ruling struct {
	Item         string `json:"item"`
	Action       string `json:"action"`
	Policy       string `json:"policy"`
	Priority     int    `json:"priority"`
	SourceBranch string `json:"sourceBranch"`
	TargetBranch string `json:"targetBranch"`
	ConditionID  string `json:"conditionId"`
	Original     int64  `json:"original"`
	Result       int64  `json:"result"`
	Reason       string `json:"reason"`
}

type Evaluation struct {
	ConditionID string `json:"conditionId"`
	Value       bool   `json:"value"`
}

type LoadResult struct {
	Model        core.SemanticModel
	SourceBranch string
	RuleVersion  string
	ArchiveHash  string
	Traces       []Trace
	Rulings      []Ruling
	Evaluations  []Evaluation
}

func (r *LoadResult) Normalize() {
	r.Model.Normalize()
	sort.SliceStable(r.Traces, func(i, j int) bool {
		if r.Traces[i].Direction != r.Traces[j].Direction {
			return r.Traces[i].Direction < r.Traces[j].Direction
		}
		if r.Traces[i].SourcePath != r.Traces[j].SourcePath {
			return r.Traces[i].SourcePath < r.Traces[j].SourcePath
		}
		return r.Traces[i].TargetPath < r.Traces[j].TargetPath
	})
	sort.SliceStable(r.Rulings, func(i, j int) bool {
		if r.Rulings[i].Item != r.Rulings[j].Item {
			return r.Rulings[i].Item < r.Rulings[j].Item
		}
		return r.Rulings[i].Action < r.Rulings[j].Action
	})
	sort.SliceStable(r.Evaluations, func(i, j int) bool {
		return r.Evaluations[i].ConditionID < r.Evaluations[j].ConditionID
	})
}

type LoadOptions struct {
	ExpectedBranch string
	RuleVersion    string
}

type ExportOptions struct {
	SourceBranch string
	TargetBranch string
	RuleVersion  string
	// PreserveConditionals asks the exporter to try to keep conditional
	// decomposition. If the target branch cannot express the same condition,
	// the declared conflict policy decides the fallback. Totals remain the
	// authority in all cases.
	PreserveConditionals bool
}

type ExportResult struct {
	Archive      []byte
	ArchiveHash  string
	TargetBranch string
	RuleVersion  string
	Traces       []Trace
	Rulings      []Ruling
}

func (r *ExportResult) Normalize() {
	sort.SliceStable(r.Traces, func(i, j int) bool {
		if r.Traces[i].SourcePath != r.Traces[j].SourcePath {
			return r.Traces[i].SourcePath < r.Traces[j].SourcePath
		}
		return r.Traces[i].TargetPath < r.Traces[j].TargetPath
	})
	sort.SliceStable(r.Rulings, func(i, j int) bool {
		if r.Rulings[i].Item != r.Rulings[j].Item {
			return r.Rulings[i].Item < r.Rulings[j].Item
		}
		return r.Rulings[i].Action < r.Rulings[j].Action
	})
}

// CompareThreePaths compares semantic entries, trace sequence, and ruling
// sequence. It returns E_DIFF with the first differing internal semantic name
// and the checkpoint window containing that item.
func CompareThreePaths(left, right LoadResult, windowSize int) error {
	if windowSize <= 0 {
		windowSize = 1
	}
	leftEntries := core.SemanticEntries(left.Model)
	rightEntries := core.SemanticEntries(right.Model)
	limit := len(leftEntries)
	if len(rightEntries) < limit {
		limit = len(rightEntries)
	}
	for i := 0; i < limit; i++ {
		if leftEntries[i] != rightEntries[i] {
			return diffError(leftEntries[i].Name, leftEntries[i].Value, rightEntries[i].Value, i, windowSize)
		}
	}
	if len(leftEntries) != len(rightEntries) {
		if len(leftEntries) > limit {
			return diffError(leftEntries[limit].Name, leftEntries[limit].Value, "<absent>", limit, windowSize)
		}
		return diffError(rightEntries[limit].Name, "<absent>", rightEntries[limit].Value, limit, windowSize)
	}
	limit = len(left.Traces)
	if len(right.Traces) < limit {
		limit = len(right.Traces)
	}
	for i := 0; i < limit; i++ {
		if left.Traces[i] != right.Traces[i] {
			return diffError(fmt.Sprintf("mappingTrace[%04d]", i), traceString(left.Traces[i]), traceString(right.Traces[i]), i, windowSize)
		}
	}
	if len(left.Traces) != len(right.Traces) {
		i := limit
		leftValue, rightValue := any("<absent>"), any("<absent>")
		if i < len(left.Traces) {
			leftValue = traceString(left.Traces[i])
		}
		if i < len(right.Traces) {
			rightValue = traceString(right.Traces[i])
		}
		return diffError(fmt.Sprintf("mappingTrace[%04d]", i), leftValue, rightValue, i, windowSize)
	}
	limit = len(left.Rulings)
	if len(right.Rulings) < limit {
		limit = len(right.Rulings)
	}
	for i := 0; i < limit; i++ {
		if left.Rulings[i] != right.Rulings[i] {
			return diffError(fmt.Sprintf("ruling[%04d]", i), left.Rulings[i], right.Rulings[i], i, windowSize)
		}
	}
	if len(left.Rulings) != len(right.Rulings) {
		i := limit
		leftValue, rightValue := any("<absent>"), any("<absent>")
		if i < len(left.Rulings) {
			leftValue = left.Rulings[i]
		}
		if i < len(right.Rulings) {
			rightValue = right.Rulings[i]
		}
		return diffError(fmt.Sprintf("ruling[%04d]", i), leftValue, rightValue, i, windowSize)
	}
	return nil
}

func diffError(name string, left, right any, index, windowSize int) error {
	return &core.Error{
		Code:     core.CodeDiff,
		Op:       "three-path-compare",
		Message:  "first consistency difference",
		Semantic: name,
		Left:     left,
		Right:    right,
		Window:   index / windowSize,
	}
}

func traceString(trace Trace) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s", trace.Direction, trace.SourceBranch,
		trace.TargetBranch, trace.SourcePath, trace.TargetPath, trace.SemanticRole, trace.Status)
}
