package fogkernel

import (
	"fmt"
	"math"
)

type Violation struct {
	Code       string  `json:"code"`
	RayID      string  `json:"ray_id,omitempty"`
	BlockIndex int     `json:"block_index,omitempty"`
	Field      string  `json:"field,omitempty"`
	Expected   float64 `json:"expected,omitempty"`
	Actual     float64 `json:"actual,omitempty"`
	Difference float64 `json:"difference,omitempty"`
	Tolerance  float64 `json:"tolerance,omitempty"`
	Message    string  `json:"message"`
}

type PathMatrixEntry struct {
	SchemaVersion  string  `json:"schema_version"`
	Path           Path    `json:"path"`
	RayCount       int     `json:"ray_count"`
	MaxTDifference float64 `json:"max_t_difference_vs_v1"`
	SameStops      bool    `json:"same_stops_vs_v1"`
}

type VerificationReport struct {
	OK                  bool               `json:"ok"`
	SceneVersion        string             `json:"scene_version"`
	RayCount            int                `json:"ray_count"`
	HitCount            int                `json:"hit_count"`
	ExactMaxTError      float64            `json:"exact_max_t_error"`
	ExactMaxTauError    float64            `json:"exact_max_tau_error"`
	ExactMaxEnergyError float64            `json:"exact_max_energy_error"`
	PreviewMaxError     float64            `json:"preview_max_error"`
	PreviewMaxBlockT    float64            `json:"preview_max_block_t_error"`
	PreviewBound        float64            `json:"preview_bound"`
	PreviewWithinBound  bool               `json:"preview_within_bound"`
	DiagnosticReplays   bool               `json:"diagnostic_replays"`
	TerminalMatches     int                `json:"terminal_matches"`
	Violations          []Violation        `json:"violations,omitempty"`
	CrossVersion        *EquivalenceReport `json:"cross_version,omitempty"`
	Matrix              []PathMatrixEntry  `json:"matrix,omitempty"`
}

func VerifyReportAgainstTruth(report *SolveReport, truth *TruthSet, recipe Recipe) []Violation {
	var violations []Violation
	tol := float64(recipe.TransmittanceToleranceQ) / float64(TransmittanceQ)
	tauTol := 10.0 * float64(recipe.TransmittanceToleranceQ) / float64(TauScale)
	energyTol := tol * 10
	if len(report.Rays) != len(truth.Rays) {
		violations = append(violations, Violation{Code: CodeAuditTolerance, Message: fmt.Sprintf("ray count %d != truth %d", len(report.Rays), len(truth.Rays))})
		return violations
	}
	for i := range truth.Rays {
		tr := truth.Rays[i]
		rr := report.Rays[i]
		if tr.RayID != rr.RayID {
			violations = append(violations, Violation{Code: CodeAuditTolerance, RayID: tr.RayID, Field: "ray_id", Message: "ray ordering/id mismatch"})
		}
		if tr.Hit != rr.Hit {
			violations = append(violations, Violation{Code: CodeAuditTolerance, RayID: tr.RayID, Field: "hit", Expected: boolFloat(tr.Hit), Actual: boolFloat(rr.Hit), Message: "hit mismatch"})
			continue
		}
		if !tr.Hit {
			if math.Abs(rr.Transmittance-1) > tol || len(rr.Blocks) != 0 || rr.TerminalBlock != -1 {
				violations = append(violations, Violation{Code: CodeAuditTolerance, RayID: tr.RayID, Field: "empty_result", Expected: 1, Actual: rr.Transmittance, Tolerance: tol, Message: "missed ray must be a deterministic empty hit with T=1"})
			}
			continue
		}
		if !near(rr.Transmittance, tr.Transmittance, tol) {
			violations = append(violations, Violation{Code: CodeAuditTolerance, RayID: tr.RayID, Field: "transmittance", Expected: tr.Transmittance, Actual: rr.Transmittance, Difference: math.Abs(rr.Transmittance - tr.Transmittance), Tolerance: tol, Message: "ray transmittance outside fixed tolerance"})
		}
		if rr.TerminalBlock != tr.TerminalBlock || rr.StopReason != tr.StopReason {
			violations = append(violations, Violation{Code: CodeAuditTolerance, RayID: tr.RayID, Field: "terminal_block", Expected: float64(tr.TerminalBlock), Actual: float64(rr.TerminalBlock), Message: fmt.Sprintf("terminal mismatch: got %d/%s want %d/%s", rr.TerminalBlock, rr.StopReason, tr.TerminalBlock, tr.StopReason)})
		}
		if len(rr.Blocks) != len(tr.Blocks) {
			violations = append(violations, Violation{Code: CodeAuditTolerance, RayID: tr.RayID, Field: "blocks", Expected: float64(len(tr.Blocks)), Actual: float64(len(rr.Blocks)), Message: "block count mismatch"})
			continue
		}
		for bi := range tr.Blocks {
			tb := tr.Blocks[bi]
			rb := rr.Blocks[bi]
			checkFloat := func(field string, want, got, tolerance float64) {
				if !near(want, got, tolerance) {
					violations = append(violations, Violation{
						Code: CodeAuditTolerance, RayID: tr.RayID, BlockIndex: bi, Field: field,
						Expected: want, Actual: got, Difference: math.Abs(want - got), Tolerance: tolerance,
						Message: field + " outside declared fixed tolerance",
					})
				}
			}
			checkFloat("tau", tb.Tau, rb.Tau, tauTol)
			checkFloat("t_in", tb.TIn, rb.TIn, tol)
			checkFloat("t_out", tb.TOut, rb.TOut, tol)
			checkFloat("energy", tb.Energy, rb.Energy, energyTol)
			if rb.ChunkBin != tb.ChunkBin {
				violations = append(violations, Violation{Code: CodeAuditTolerance, RayID: tr.RayID, BlockIndex: bi, Field: "chunk_bin", Expected: float64(tb.ChunkBin), Actual: float64(rb.ChunkBin), Message: "chunk bin mismatch"})
			}
		}
	}
	return violations
}

func boolFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func maxTransDiff(a, b *SolveReport) float64 {
	max := 0.0
	for i := range a.Rays {
		d := math.Abs(a.Rays[i].Transmittance - b.Rays[i].Transmittance)
		if d > max {
			max = d
		}
	}
	return max
}

func maxBlockTDiff(a, b *SolveReport) float64 {
	max := 0.0
	for ri := range a.Rays {
		if ri >= len(b.Rays) {
			continue
		}
		ar, br := a.Rays[ri], b.Rays[ri]
		for bi := 0; bi < len(ar.Blocks) && bi < len(br.Blocks); bi++ {
			if d := math.Abs(ar.Blocks[bi].TOut - br.Blocks[bi].TOut); d > max {
				max = d
			}
		}
	}
	return max
}

func blockConclusionKey(r RayResult) []string {
	out := make([]string, 0, len(r.Blocks)+2)
	out = append(out, fmt.Sprintf("T=%.12f", r.Transmittance), fmt.Sprintf("term=%d:%s", r.TerminalBlock, r.StopReason))
	for _, b := range r.Blocks {
		out = append(out, fmt.Sprintf("%d:%.12f:%.12f", b.BlockIndex, b.TOut, b.Energy))
	}
	return out
}

func sameConclusions(a, b *SolveReport) bool {
	if len(a.Rays) != len(b.Rays) {
		return false
	}
	for i := range a.Rays {
		ka, kb := blockConclusionKey(a.Rays[i]), blockConclusionKey(b.Rays[i])
		if len(ka) != len(kb) {
			return false
		}
		for j := range ka {
			if ka[j] != kb[j] {
				return false
			}
		}
	}
	return true
}

func VerifyGeneratedBundle(bundle *GeneratedBundle) (*VerificationReport, error) {
	scene := bundle.Canonical
	exact, err := SolveScene(scene, SolveOptions{Path: PathExact, Workers: scene.Recipe.DefaultWorkers, Buffer: scene.Recipe.DefaultBuffer})
	if err != nil {
		return nil, err
	}
	violations := VerifyReportAgainstTruth(exact, bundle.Truth, scene.Recipe)
	preview, err := SolveScene(scene, SolveOptions{Path: PathPreview, Workers: 1, Buffer: 32})
	if err != nil {
		return nil, err
	}
	diagnostic, err := SolveScene(scene, SolveOptions{Path: PathDiagnostic, Workers: 1, Buffer: 32})
	if err != nil {
		return nil, err
	}
	rep := &VerificationReport{
		SceneVersion: scene.Version(), RayCount: len(scene.Rays),
		PreviewBound: float64(scene.Recipe.PreviewErrorBoundQ) / float64(TransmittanceQ),
	}
	for _, r := range exact.Rays {
		if r.Hit {
			rep.HitCount++
		}
	}
	for _, v := range violations {
		switch v.Field {
		case "tau":
			if v.Difference > rep.ExactMaxTauError {
				rep.ExactMaxTauError = v.Difference
			}
		case "energy":
			if v.Difference > rep.ExactMaxEnergyError {
				rep.ExactMaxEnergyError = v.Difference
			}
		default:
			if v.Difference > rep.ExactMaxTError {
				rep.ExactMaxTError = v.Difference
			}
		}
	}
	rep.Violations = violations
	rep.OK = len(violations) == 0
	rep.PreviewMaxError = maxTransDiff(exact, preview)
	rep.PreviewMaxBlockT = maxBlockTDiff(exact, preview)
	rep.PreviewWithinBound = rep.PreviewMaxError <= rep.PreviewBound
	if rep.PreviewMaxBlockT > rep.PreviewBound {
		rep.PreviewWithinBound = false
	}
	if !rep.PreviewWithinBound {
		rep.OK = false
		rep.Violations = append(rep.Violations, Violation{Code: CodeAuditTolerance, Field: "preview", Difference: math.Max(rep.PreviewMaxError, rep.PreviewMaxBlockT), Tolerance: rep.PreviewBound, Message: "preview ray or block transmittance error exceeds declared bound"})
	}
	diagDiff := maxTransDiff(exact, diagnostic)
	rep.DiagnosticReplays = diagDiff == 0 && sameConclusions(exact, diagnostic)
	if !rep.DiagnosticReplays {
		rep.OK = false
		rep.Violations = append(rep.Violations, Violation{Code: CodeAuditTolerance, Field: "diagnostic", Difference: diagDiff, Message: "diagnostic path does not replay exact conclusions"})
	}
	for i, tr := range bundle.Truth.Rays {
		if i < len(exact.Rays) && exact.Rays[i].TerminalBlock == tr.TerminalBlock {
			rep.TerminalMatches++
		}
	}
	cross, err := CrossVersionReport(bundle)
	if err != nil {
		return nil, err
	}
	rep.CrossVersion = cross
	if !cross.DeepEqual {
		rep.OK = false
	}

	matrix := []PathMatrixEntry{}
	baseline := map[Path]*SolveReport{}
	for _, schema := range []string{SchemaV1, SchemaV2, SchemaV3} {
		loaded, err := LoadProject(bundle.Docs[schema])
		if err != nil {
			return nil, err
		}
		for _, path := range []Path{PathExact, PathPreview, PathDiagnostic} {
			solved, err := SolveScene(loaded, SolveOptions{Path: path, Workers: 2, Buffer: 32, Order: "reverse", BatchSize: 3})
			if err != nil {
				return nil, err
			}
			base := baseline[path]
			entry := PathMatrixEntry{SchemaVersion: schema, Path: path, RayCount: len(solved.Rays), SameStops: true}
			if base == nil {
				baseline[path] = solved
			} else {
				entry.MaxTDifference = maxTransDiff(base, solved)
				entry.SameStops = sameConclusions(base, solved)
				if !entry.SameStops || entry.MaxTDifference > float64(loaded.Recipe.TransmittanceToleranceQ)/float64(TransmittanceQ) {
					rep.OK = false
					rep.Violations = append(rep.Violations, Violation{Code: CodeAuditTolerance, Field: "schema_path_matrix", Message: fmt.Sprintf("%s/%s changes conclusions", schema, path)})
				}
			}
			matrix = append(matrix, entry)
		}
	}
	rep.Matrix = matrix
	return rep, nil
}
