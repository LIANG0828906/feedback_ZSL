package review

import (
	"database/sql"
	"encoding/json"
	"math"
	"os"

	_ "modernc.org/sqlite"

	"integritykernel/internal/core"
)

type Result struct {
	OK                  bool            `json:"ok"`
	Database            string          `json:"database"`
	Fingerprint         string          `json:"fingerprint"`
	CheckedFrames       int             `json:"checked_frames"`
	RecomputedIntervals []core.Interval `json:"recomputed_intervals"`
	FirstViolation      *core.Error     `json:"first_violation,omitempty"`
}

type row struct {
	frame        int
	evidenceType string
	payload      []byte
	digest       string
}

func Run(databasePath, fingerprintPath string) (*Result, error) {
	fingerprintRaw, err := os.ReadFile(fingerprintPath)
	if err != nil {
		return nil, core.WithField(core.NewError(core.CodeDecode, "read truth fingerprint: %v", err), "truth.fingerprint.json")
	}
	var fingerprint core.TruthFingerprint
	if err := json.Unmarshal(fingerprintRaw, &fingerprint); err != nil {
		return nil, core.WithField(core.NewError(core.CodeDecode, "decode truth fingerprint: %v", err), "truth.fingerprint.json")
	}

	db, err := sql.Open("sqlite", databasePath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var paramsJSON string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='params_json'`).Scan(&paramsJSON); err != nil {
		return nil, core.NewError(core.CodeDecode, "persisted parameter JSON unavailable: %v", err)
	}
	var params core.Params
	if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
		return nil, core.WithField(core.NewError(core.CodeDecode, "decode persisted params: %v", err), "params_json")
	}

	rows, err := db.Query(`
SELECT e.frame_index, e.evidence_type, e.payload_json, e.digest
FROM current_evidence c JOIN evidence e ON e.cache_key = c.cache_key
ORDER BY c.frame_index, c.evidence_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byFrame := map[int]map[string]row{}
	for rows.Next() {
		var r row
		var payload string
		if err := rows.Scan(&r.frame, &r.evidenceType, &payload, &r.digest); err != nil {
			return nil, err
		}
		r.payload = []byte(payload)
		if byFrame[r.frame] == nil {
			byFrame[r.frame] = map[string]row{}
		}
		byFrame[r.frame][r.evidenceType] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := &Result{OK: true, Database: databasePath, Fingerprint: fingerprintPath, CheckedFrames: fingerprint.FrameCount}
	recomputedVerdicts := make([]core.VerdictEvidence, 0, fingerprint.FrameCount)
	for frame := 0; frame < fingerprint.FrameCount; frame++ {
		items := byFrame[frame]
		for _, evidenceType := range []string{core.EvidenceWatermark, core.EvidenceResidual, core.EvidenceNoise, core.EvidenceVerdict} {
			r, ok := items[evidenceType]
			if !ok {
				return violation(result, frame, evidenceType, "presence", "persisted evidence is missing")
			}
			if core.SHA256Hex(r.payload) != r.digest {
				return violation(result, frame, evidenceType, "digest", "persisted evidence digest does not match payload")
			}
		}

		var wm core.WatermarkEvidence
		var residual core.ResidualEvidence
		var noise core.NoiseEvidence
		var persisted core.VerdictEvidence
		if err := json.Unmarshal(items[core.EvidenceWatermark].payload, &wm); err != nil {
			return violation(result, frame, core.EvidenceWatermark, "payload", err.Error())
		}
		if err := json.Unmarshal(items[core.EvidenceResidual].payload, &residual); err != nil {
			return violation(result, frame, core.EvidenceResidual, "payload", err.Error())
		}
		if err := json.Unmarshal(items[core.EvidenceNoise].payload, &noise); err != nil {
			return violation(result, frame, core.EvidenceNoise, "payload", err.Error())
		}
		if err := json.Unmarshal(items[core.EvidenceVerdict].payload, &persisted); err != nil {
			return violation(result, frame, core.EvidenceVerdict, "payload", err.Error())
		}
		recomputed := recomputeVerdict(frame, &wm, residual, noise, params)
		if recomputed.Tampered != persisted.Tampered {
			return violation(result, frame, core.EvidenceVerdict, "tampered", "recomputed tampered flag differs")
		}
		if math.Abs(recomputed.Score-persisted.Score) > 1e-12 {
			return violation(result, frame, core.EvidenceVerdict, "score", "recomputed score differs")
		}
		recomputedVerdicts = append(recomputedVerdicts, recomputed)
	}

	result.RecomputedIntervals = locateIntervals(recomputedVerdicts)
	if len(result.RecomputedIntervals) == 0 {
		frame := fingerprint.ReplacementStart
		return violation(result, frame, "interval", "intervals", "independent review localized no interval")
	}
	best := result.RecomputedIntervals[0]
	for _, interval := range result.RecomputedIntervals[1:] {
		if distance(interval, fingerprint) < distance(best, fingerprint) {
			best = interval
		}
	}
	tolerance := params.FrameTolerance
	if abs(best.StartFrame-fingerprint.ReplacementStart) > tolerance {
		return violation(result, fingerprint.ReplacementStart, "interval", "start_frame", "independent start frame is outside truth tolerance")
	}
	if abs(best.EndFrame-fingerprint.ReplacementEnd) > tolerance {
		return violation(result, fingerprint.ReplacementEnd, "interval", "end_frame", "independent end frame is outside truth tolerance")
	}
	return result, nil
}

func recomputeVerdict(frame int, wm *core.WatermarkEvidence, residual core.ResidualEvidence, noise core.NoiseEvidence, params core.Params) core.VerdictEvidence {
	v := core.VerdictEvidence{Frame: frame}
	switch {
	case wm.Missing:
		v.WatermarkScore = 0.92
	case wm.Misaligned:
		v.WatermarkScore = 0.86
	case !wm.MatchesExpected:
		v.WatermarkScore = 1.0
	default:
		v.WatermarkScore = clamp01((1-wm.OverallConfidence)*0.35 + 0.015)
	}
	v.ResidualScore = residual.AnomalyScore
	v.NoiseScore = noise.AnomalyScore
	sum := params.WatermarkWeight + params.ResidualWeight + params.NoiseWeight
	v.Score = (params.WatermarkWeight*v.WatermarkScore + params.ResidualWeight*v.ResidualScore + params.NoiseWeight*v.NoiseScore) / sum
	corroborating := 0
	if v.WatermarkScore >= 0.35 {
		corroborating++
	}
	if v.ResidualScore >= 0.35 {
		corroborating++
	}
	if v.NoiseScore >= 0.35 {
		corroborating++
	}
	v.Tampered = v.Score >= params.VerdictThreshold && corroborating >= 2
	if v.Tampered {
		v.Confidence = clamp01(0.55 + 0.45*v.Score)
	} else {
		v.Confidence = clamp01(0.55 + 0.45*(1-v.Score))
	}
	return v
}

func locateIntervals(verdicts []core.VerdictEvidence) []core.Interval {
	var out []core.Interval
	for i := 0; i < len(verdicts); {
		if !verdicts[i].Tampered {
			i++
			continue
		}
		start := i
		confidence := 0.0
		for i < len(verdicts) && verdicts[i].Tampered {
			confidence += verdicts[i].Confidence
			i++
		}
		end := i - 1
		out = append(out, core.Interval{StartFrame: start, EndFrame: end, FrameCount: end - start + 1, Confidence: confidence / float64(end-start+1)})
	}
	return out
}

func violation(result *Result, frame int, evidenceType, field, message string) (*Result, error) {
	f := frame
	result.OK = false
	result.FirstViolation = &core.Error{
		Code: core.CodeDiff, Message: message, Frame: &f, Field: field, EvidenceType: evidenceType,
	}
	return result, nil
}

func distance(interval core.Interval, fp core.TruthFingerprint) int {
	return abs(interval.StartFrame-fp.ReplacementStart) + abs(interval.EndFrame-fp.ReplacementEnd)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
