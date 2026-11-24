package review

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"sort"

	_ "modernc.org/sqlite"

	"screenverify/internal/model"
)

type Violation struct {
	Code         string `json:"code"`
	Frame        int    `json:"frame,omitempty"`
	EvidenceType string `json:"evidence_type,omitempty"`
	Item         string `json:"item"`
	Message      string `json:"message"`
}

type Interval struct {
	StartFrame int `json:"start_frame"`
	EndFrame   int `json:"end_frame"`
}

type Result struct {
	OK                 bool       `json:"ok"`
	DBPath             string     `json:"db_path"`
	TruthPath          string     `json:"truth_path"`
	TruthFingerprint   string     `json:"truth_fingerprint"`
	RecordedTruthFP    string     `json:"recorded_truth_fingerprint,omitempty"`
	RelocatedIntervals []Interval `json:"relocated_intervals"`
	TamperedFrames     []int      `json:"tampered_frames"`
	CheckedEvidence    int        `json:"checked_evidence"`
	CheckedBlocks      int        `json:"checked_blocks"`
	CheckedCheckpoints int        `json:"checked_checkpoints"`
	Violation          *Violation `json:"violation,omitempty"`
}

type evidenceRow struct {
	Frame      int
	Type       string
	PayloadRaw string
	PayloadSHA string
	Payload    map[string]any
}

func Run(dbPath, truthPath string) (*Result, error) {
	res := &Result{OK: true, DBPath: dbPath, TruthPath: truthPath, RelocatedIntervals: []Interval{}, TamperedFrames: []int{}}
	truthRaw, err := os.ReadFile(truthPath)
	if err != nil {
		return nil, err
	}
	truth := &model.Truth{}
	if err := json.Unmarshal(truthRaw, truth); err != nil {
		return nil, err
	}
	fp, err := model.FingerprintTruth(truth)
	if err != nil {
		return nil, err
	}
	res.TruthFingerprint = fp

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	recorded, _, _ := queryMeta(db, "truth_fingerprint")
	res.RecordedTruthFP = recorded
	if recorded != "" && recorded != fp {
		return fail(res, &Violation{Code: "E_DIFF", EvidenceType: "run_meta", Item: "truth_fingerprint",
			Message: "truth fingerprint does not match versioned checkpoint metadata"}), nil
	}

	checkpoints, err := db.Query(`SELECT COUNT(*) FROM checkpoints`)
	if err == nil {
		if checkpoints.Next() {
			_ = checkpoints.Scan(&res.CheckedCheckpoints)
		}
		checkpoints.Close()
	}

	rows, err := db.Query(`SELECT frame_no, evidence_type, payload_json, payload_sha256 FROM evidence WHERE status='ok' ORDER BY frame_no, evidence_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	evidence := []evidenceRow{}
	for rows.Next() {
		var row evidenceRow
		if err := rows.Scan(&row.Frame, &row.Type, &row.PayloadRaw, &row.PayloadSHA); err != nil {
			return nil, err
		}
		sum := sha256.Sum256([]byte(row.PayloadRaw))
		if hex.EncodeToString(sum[:]) != row.PayloadSHA {
			return fail(res, &Violation{Code: "E_DIFF", Frame: row.Frame, EvidenceType: row.Type, Item: "payload_sha256",
				Message: "persisted payload digest does not match"}), nil
		}
		if err := json.Unmarshal([]byte(row.PayloadRaw), &row.Payload); err != nil {
			return fail(res, &Violation{Code: "E_DIFF", Frame: row.Frame, EvidenceType: row.Type, Item: "payload_json",
				Message: "persisted payload is not valid JSON"}), nil
		}
		evidence = append(evidence, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.CheckedEvidence = len(evidence)

	for _, row := range evidence {
		switch row.Type {
		case "decision":
			if v := checkDecision(row); v != nil {
				return fail(res, v), nil
			}
			if truthy(row.Payload["tampered"]) {
				res.TamperedFrames = append(res.TamperedFrames, row.Frame)
			}
		case "noise_variance":
			blocks, v := checkNoise(row, truth)
			res.CheckedBlocks += blocks
			if v != nil {
				return fail(res, v), nil
			}
		}
	}
	sort.Ints(res.TamperedFrames)
	res.RelocatedIntervals = contiguousIntervals(res.TamperedFrames)
	if v := checkTruthInterval(truth, res.RelocatedIntervals, res.TamperedFrames); v != nil {
		return fail(res, v), nil
	}
	return res, nil
}

func fail(res *Result, v *Violation) *Result {
	res.OK = false
	res.Violation = v
	return res
}

func queryMeta(db *sql.DB, key string) (string, bool, error) {
	var v string
	err := db.QueryRow(`SELECT value FROM run_meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return v, err == nil, err
}

func checkDecision(row evidenceRow) *Violation {
	p := row.Payload
	score := number(p["score"])
	conf := number(p["confidence"])
	threshold := number(p["threshold"])
	if !unit(score) || !unit(conf) || !unit(threshold) {
		return &Violation{Code: "E_DIFF", Frame: row.Frame, EvidenceType: row.Type, Item: "confidence",
			Message: "score/confidence/threshold is outside [0,1]"}
	}
	tampered := truthy(p["tampered"])
	want := 0.0
	if tampered {
		denom := math.Max(1e-9, 1-threshold)
		want = clamp01((score - threshold) / denom)
		want = round6(want)
	}
	if math.Abs(want-conf) > 1e-6 {
		return &Violation{Code: "E_DIFF", Frame: row.Frame, EvidenceType: row.Type, Item: "confidence",
			Message: "confidence does not match independent threshold formula"}
	}
	if tampered {
		reasons, _ := p["reasons"].([]any)
		if len(reasons) == 0 {
			return &Violation{Code: "E_DIFF", Frame: row.Frame, EvidenceType: row.Type, Item: "reasons",
				Message: "tampered decision has no evidence reason"}
		}
	}
	return nil
}

func checkNoise(row evidenceRow, truth *model.Truth) (int, *Violation) {
	p := row.Payload
	blockSize := int(number(p["block_size"]))
	if blockSize <= 0 || truth.Width%blockSize != 0 || truth.Height%blockSize != 0 {
		return 0, &Violation{Code: "E_DIFF", Frame: row.Frame, EvidenceType: row.Type, Item: "block_size",
			Message: "block_size is invalid for frame dimensions"}
	}
	rawBlocks, _ := p["blocks"].([]any)
	vs := []float64{}
	for _, rb := range rawBlocks {
		b, ok := rb.(map[string]any)
		if !ok {
			return len(rawBlocks), &Violation{Code: "E_DIFF", Frame: row.Frame, EvidenceType: row.Type, Item: "blocks", Message: "block is not object"}
		}
		bx := int(number(b["block_x"]))
		by := int(number(b["block_y"]))
		x := int(number(b["x"]))
		y := int(number(b["y"]))
		if bx < 0 || by < 0 || x != bx*blockSize || y != by*blockSize || bx*blockSize+blockSize > truth.Width || by*blockSize+blockSize > truth.Height {
			return len(rawBlocks), &Violation{Code: "E_DIFF", Frame: row.Frame, EvidenceType: row.Type, Item: "block_coordinate",
				Message: "block coordinate is outside deterministic grid"}
		}
		variance := number(b["variance"])
		rawVariance := number(b["raw_variance"])
		z := number(b["z_score"])
		if variance < 0 || rawVariance < 0 || math.IsNaN(z) || math.IsInf(z, 0) {
			return len(rawBlocks), &Violation{Code: "E_DIFF", Frame: row.Frame, EvidenceType: row.Type, Item: "variance",
				Message: "block variance or z_score is invalid"}
		}
		vs = append(vs, variance)
	}
	if len(vs) != (truth.Width/blockSize)*(truth.Height/blockSize) {
		return len(rawBlocks), &Violation{Code: "E_DIFF", Frame: row.Frame, EvidenceType: row.Type, Item: "blocks.length",
			Message: "block count does not match deterministic grid"}
	}
	med := round6(median(vs))
	if math.Abs(med-number(p["median_variance"])) > 2e-6 {
		return len(rawBlocks), &Violation{Code: "E_DIFF", Frame: row.Frame, EvidenceType: row.Type, Item: "median_variance",
			Message: "median block variance mismatch"}
	}
	devs := make([]float64, len(vs))
	for i, v := range vs {
		devs[i] = math.Abs(v - number(p["median_variance"]))
	}
	mad := round6(median(devs))
	if math.Abs(mad-number(p["mad"])) > 2e-6 {
		return len(rawBlocks), &Violation{Code: "E_DIFF", Frame: row.Frame, EvidenceType: row.Type, Item: "mad",
			Message: "block variance MAD mismatch"}
	}
	mean, std := meanStd(vs)
	disp := round6(std / (mean + 1e-6))
	if math.Abs(disp-number(p["dispersion"])) > 2e-6 {
		return len(rawBlocks), &Violation{Code: "E_DIFF", Frame: row.Frame, EvidenceType: row.Type, Item: "dispersion",
			Message: "block variance dispersion mismatch"}
	}
	if !unit(number(p["score"])) {
		return len(rawBlocks), &Violation{Code: "E_DIFF", Frame: row.Frame, EvidenceType: row.Type, Item: "score",
			Message: "noise score outside [0,1]"}
	}
	return len(rawBlocks), nil
}

func checkTruthInterval(truth *model.Truth, intervals []Interval, frames []int) *Violation {
	r := truth.Replacement
	if r.StartFrame < 0 {
		if len(frames) != 0 {
			f := frames[0]
			return &Violation{Code: "E_DIFF", Frame: f, EvidenceType: "decision", Item: "replacement_interval",
				Message: "clean truth has replacement mask but persisted decisions contain tampered frame"}
		}
		return nil
	}
	if len(intervals) == 0 {
		return &Violation{Code: "E_DIFF", Frame: r.StartFrame, EvidenceType: "decision", Item: "replacement_interval",
			Message: "no replacement interval relocated from persisted evidence"}
	}
	best := intervals[0]
	for _, iv := range intervals[1:] {
		if iv.EndFrame-iv.StartFrame > best.EndFrame-best.StartFrame {
			best = iv
		}
	}
	tol := truth.FrameTolerance
	if abs(best.StartFrame-r.StartFrame) > tol || abs(best.EndFrame-r.EndFrame) > tol {
		return &Violation{Code: "E_DIFF", Frame: best.StartFrame, EvidenceType: "decision", Item: "replacement_interval",
			Message: "relocated interval is outside declared frame tolerance"}
	}
	return nil
}

func contiguousIntervals(frames []int) []Interval {
	if len(frames) == 0 {
		return []Interval{}
	}
	out := []Interval{}
	start, end := frames[0], frames[0]
	for _, f := range frames[1:] {
		if f == end+1 {
			end = f
		} else {
			out = append(out, Interval{start, end})
			start, end = f, f
		}
	}
	out = append(out, Interval{start, end})
	return out
}

func number(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	default:
		return 0
	}
}

func truthy(v any) bool { b, _ := v.(bool); return b }

func unit(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1 }

func clamp01(v float64) float64 {
	if v < 0 || math.IsNaN(v) {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func median(xs []float64) float64 {
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	n := len(cp)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}

func meanStd(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	ss := 0.0
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(ss / float64(len(xs)))
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
