package fogkernel

import (
	"math"
	"reflect"
	"strconv"
)

type EquivalenceReport struct {
	Versions     []string          `json:"versions"`
	SceneVersion string            `json:"scene_version"`
	DeepEqual    bool              `json:"deep_equal"`
	MaxTauDiffQ  int64             `json:"max_tau_diff_q"`
	MaxTDiffQ    int64             `json:"max_t_diff_q"`
	MediaCount   int               `json:"media_count"`
	RayCount     int               `json:"ray_count"`
	Problems     []ConversionAudit `json:"problems"`
}

type BlockTauAudit struct {
	SchemaVersion string   `json:"schema_version"`
	RayIndex      int      `json:"ray_index"`
	RayID         string   `json:"ray_id"`
	BlockIndex    int      `json:"block_index"`
	VolumeIDs     []string `json:"volume_ids"`
	Tau           float64  `json:"tau"`
	TauQ          int64    `json:"tau_q"`
	DifferenceQ   int64    `json:"difference_q_vs_v1"`
	ToleranceQ    int64    `json:"tolerance_q"`
	WithinTol     bool     `json:"within_tolerance"`
	StatusCode    string   `json:"status_code"`
}

type BlockAuditReport struct {
	ToleranceQ   int64           `json:"tolerance_q"`
	ItemCount    int             `json:"item_count"`
	FailureCount int             `json:"failure_count"`
	Items        []BlockTauAudit `json:"items"`
}

func CriticalConversionAudits() []ConversionAudit {
	tol := DefaultTolQ
	audits := []ConversionAudit{}

	addCoeff := func(id, version string, raw int64, scale ScaleKind) {
		q, err := CoefficientToQ(raw, scale, "$.critical."+id)
		a := ConversionAudit{
			ID: id, Kind: "coefficient", SourceVersion: version,
			RawValue: intToString(raw), CanonicalQ: q,
			Intermediate:   "raw * " + intToString(coeffMultiplier(scale)),
			CanonicalFloat: QToFloat(q, CoeffScale), ToleranceQ: tol,
			StatusCode: "OK", WithinTolerance: true,
		}
		if err != nil {
			biz, _ := AsBizError(err)
			a.StatusCode = biz.Code
			a.Message = biz.Message
			a.Overflow = biz.Code == CodeFixedOverflow
			a.WithinTolerance = false
		}
		back := QToPermyriad(q)
		if scale == ScalePermille {
			back = QToPermyriad(q)
			a.DifferenceQ = int64(math.Abs(float64(back - raw*10)))
		} else {
			_ = back
		}
		audits = append(audits, a)
	}
	addCoeff("coeff-min-permille", SchemaV1, 1, ScalePermille)
	addCoeff("coeff-min-permyriad", SchemaV2, 10, ScalePermyriad)
	addCoeff("coeff-max-safe-permille", SchemaV1, math.MaxInt64/(CoeffScale/1000), ScalePermille)
	addCoeff("coeff-max-safe-permyriad", SchemaV2, math.MaxInt64/(CoeffScale/100), ScalePermyriad)

	addDist := func(id, version, raw, intermediate string, micro int64, err error) {
		a := ConversionAudit{
			ID: id, Kind: "distance", SourceVersion: version, RawValue: raw,
			Intermediate: intermediate, CanonicalQ: micro,
			CanonicalFloat: float64(micro) / float64(DistanceScale),
			ToleranceQ:     tol, StatusCode: "OK", WithinTolerance: true,
		}
		if err != nil {
			biz, _ := AsBizError(err)
			a.StatusCode = biz.Code
			a.Message = biz.Message
			a.Overflow = biz.Code == CodeFixedOverflow
			a.WithinTolerance = false
		}
		audits = append(audits, a)
	}
	micro, err := MMToMicro(1, "$.critical.mm1")
	addDist("distance-one-mm", SchemaV1, "1 mm", "1*1000", micro, err)
	micro, err = DecimalMetersToMicro("0.001", "$.critical.m001")
	addDist("distance-equivalent-meter", SchemaV2, "0.001 m", "decimal*1000000", micro, err)
	safeMM := math.MaxInt64 / MicroPerMM
	micro, err = MMToMicro(safeMM, "$.critical.maxmm")
	addDist("distance-max-safe-mm", SchemaV1, intToString(safeMM)+" mm", "checked_mul(mm,1000)", micro, err)

	tau := 123456.0 / float64(TauScale)
	tauQ := QuantizeTau(tau)
	roundTrip := float64(tauQ) / float64(TauScale)
	diffQ := FixedDifferenceQ(tau, roundTrip, TauScale)
	audits = append(audits, ConversionAudit{
		ID: "tau-quantization-edge", Kind: "optical_thickness", SourceVersion: "canonical",
		RawValue: "0.000123456", Intermediate: "round(tau*1e9)", TauQ: tauQ, Tau: tau,
		CanonicalQ: tauQ, CanonicalFloat: roundTrip, DifferenceQ: diffQ,
		ToleranceQ: 1, WithinTolerance: diffQ <= 1, StatusCode: "OK",
	})
	return audits
}

func coeffMultiplier(scale ScaleKind) int64 {
	if scale == ScalePermille {
		return CoeffScale / 1000
	}
	return CoeffScale / 10000
}

func intToString(v int64) string {
	return strconv.FormatInt(v, 10)
}

func ScenesEquivalent(a, b *Scene) bool {
	if a == nil || b == nil {
		return a == b
	}
	return reflect.DeepEqual(normalizeSceneForCompare(a), normalizeSceneForCompare(b))
}

func normalizeSceneForCompare(s *Scene) *Scene {
	cp := s.Clone()
	for _, m := range cp.Media {
		m.SourceVersions = nil
	}
	return cp
}

func CrossVersionReport(bundle *GeneratedBundle) (*EquivalenceReport, error) {
	report := &EquivalenceReport{Versions: []string{SchemaV1, SchemaV2, SchemaV3}}
	loaded := make([]*Scene, 0, 3)
	for _, v := range report.Versions {
		scene, err := LoadProject(bundle.Docs[v])
		if err != nil {
			return nil, err
		}
		loaded = append(loaded, scene)
	}
	report.SceneVersion = loaded[0].Version()
	report.MediaCount = len(loaded[0].Media)
	report.RayCount = len(loaded[0].Rays)
	report.DeepEqual = true
	for i := 1; i < len(loaded); i++ {
		if !ScenesEquivalent(loaded[0], loaded[i]) {
			report.DeepEqual = false
			report.Problems = append(report.Problems, ConversionAudit{
				ID: "scene-deep-equal", SourceVersion: report.Versions[i],
				StatusCode: CodeAuditTolerance, WithinTolerance: false,
				Message: "loaded scene differs from " + report.Versions[0],
			})
		}
	}
	base, err := SolveScene(loaded[0], SolveOptions{Path: PathExact, Workers: 1, Buffer: 16})
	if err != nil {
		return nil, err
	}
	for i := 1; i < len(loaded); i++ {
		other, err := SolveScene(loaded[i], SolveOptions{Path: PathExact, Workers: 1, Buffer: 16})
		if err != nil {
			return nil, err
		}
		maxTau, maxT := compareReportDiff(base, other)
		if maxTau > report.MaxTauDiffQ {
			report.MaxTauDiffQ = maxTau
		}
		if maxT > report.MaxTDiffQ {
			report.MaxTDiffQ = maxT
		}
	}
	return report, nil
}

func AuditBlockTauAcrossVersions(bundle *GeneratedBundle) (*BlockAuditReport, error) {
	tolQ := int64(1000)
	report := &BlockAuditReport{ToleranceQ: tolQ, Items: []BlockTauAudit{}}
	var base *SolveReport
	for _, schema := range []string{SchemaV1, SchemaV2, SchemaV3} {
		scene, err := LoadProject(bundle.Docs[schema])
		if err != nil {
			return nil, err
		}
		solved, err := SolveScene(scene, SolveOptions{Path: PathExact, Workers: 1, Buffer: 32})
		if err != nil {
			return nil, err
		}
		if base == nil {
			base = solved
		}
		for ri, rr := range solved.Rays {
			for bi, br := range rr.Blocks {
				diff := int64(0)
				if ri < len(base.Rays) && bi < len(base.Rays[ri].Blocks) {
					diff = FixedDifferenceQ(br.Tau, base.Rays[ri].Blocks[bi].Tau, TauScale)
				}
				ok := diff <= tolQ
				item := BlockTauAudit{
					SchemaVersion: schema, RayIndex: ri, RayID: rr.RayID, BlockIndex: bi,
					VolumeIDs: br.VolumeIDs, Tau: br.Tau, TauQ: QuantizeTau(br.Tau),
					DifferenceQ: diff, ToleranceQ: tolQ, WithinTol: ok, StatusCode: "OK",
				}
				if !ok {
					item.StatusCode = CodeAuditTolerance
					report.FailureCount++
				}
				report.Items = append(report.Items, item)
			}
		}
	}
	report.ItemCount = len(report.Items)
	return report, nil
}

func compareReportDiff(a, b *SolveReport) (maxTauQ, maxTQ int64) {
	for ri := range a.Rays {
		ar, br := a.Rays[ri], b.Rays[ri]
		for bi := range ar.Blocks {
			if bi >= len(br.Blocks) {
				continue
			}
			if d := FixedDifferenceQ(ar.Blocks[bi].Tau, br.Blocks[bi].Tau, TauScale); d > maxTauQ {
				maxTauQ = d
			}
			if d := FixedDifferenceQ(ar.Blocks[bi].TOut, br.Blocks[bi].TOut, TransmittanceQ); d > maxTQ {
				maxTQ = d
			}
		}
	}
	return maxTauQ, maxTQ
}
