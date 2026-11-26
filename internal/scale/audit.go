package scale

import (
	"strconv"

	"schedlab/internal/gen"
)

// CaseResult is the audit verdict for one critical conversion.
type CaseResult struct {
	Case      gen.ScaleCase
	Got       int64
	Diff      int64
	Within    bool
	Rejected  bool // true when the case is expected to be rejected (non-integral)
	Traces    []Trace
	ErrorCode string
}

// AuditCases runs every generator critical case through the fixed-point
// converters and marks whether the absolute difference stays within tolerance.
func AuditCases(cases []gen.ScaleCase) []CaseResult {
	out := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		res := CaseResult{Case: c}
		var got int64
		var tr Trace
		var err error
		switch c.Kind {
		case "ratio_to_milli":
			got, tr, err = RatioToMilli(c.Input, c.RefMilli)
		case "cores_to_milli":
			got, tr, err = CoresToMilli(c.Input)
		case "permille_to_bps":
			v, _ := strconv.ParseInt(c.Input, 10, 64)
			got, tr, err = PermilleToBPS(v)
		case "bps_to_permille":
			v, _ := strconv.ParseInt(c.Input, 10, 64)
			got, tr, err = BPSToPermille(v)
			if err != nil || c.Expect == -1 {
				res.Rejected = true
			}
		case "memory_to_bytes":
			got, tr, err = MemoryToBytes(c.Input)
		case "tick_to_ms":
			v, _ := strconv.ParseInt(c.Input, 10, 64)
			got, tr, err = TickToMs(v)
		}
		res.Traces = []Trace{tr}
		if err != nil {
			res.ErrorCode = "E_OVERFLOW"
			if err == ErrSyntax {
				res.ErrorCode = "E_RESOURCE_PARSE"
			}
		}
		if res.Rejected {
			// Expected rejection counts as within tolerance by definition.
			res.Within = true
			out = append(out, res)
			continue
		}
		res.Got = got
		diff := got - c.Expect
		if diff < 0 {
			diff = -diff
		}
		res.Diff = diff
		res.Within = diff <= c.Tolerance
		out = append(out, res)
	}
	return out
}

// RankingPair is two node identifiers and their relative order.
type RankingPair struct {
	Higher string
	Lower  string
}

// RankingUnchanged reports whether the ordering by score (descending, stable on
// ties) is identical between two score maps keyed by node id.
func RankingUnchanged(a, b map[string]int64, order []string) bool {
	rankOf := func(scores map[string]int64) []string {
		ranked := append([]string(nil), order...)
		// Stable insertion sort by descending score, preserving input order.
		for i := 1; i < len(ranked); i++ {
			for j := i; j > 0 && scores[ranked[j]] > scores[ranked[j-1]]; j-- {
				ranked[j], ranked[j-1] = ranked[j-1], ranked[j]
			}
		}
		return ranked
	}
	ra := rankOf(a)
	rb := rankOf(b)
	if len(ra) != len(rb) {
		return false
	}
	for i := range ra {
		if ra[i] != rb[i] {
			return false
		}
	}
	return true
}
