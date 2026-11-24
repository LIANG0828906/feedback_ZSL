package verify

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"

	"audiocheck/internal/model"

	_ "modernc.org/sqlite"
)

type Mismatch struct {
	Frame      *int              `json:"frame,omitempty"`
	BlockIndex int               `json:"block_index"`
	Block      *model.BlockCoord `json:"block,omitempty"`
	Field      string            `json:"field"`
	Expected   any               `json:"expected,omitempty"`
	Actual     any               `json:"actual,omitempty"`
	Message    string            `json:"message"`
}

type Result struct {
	OK              bool       `json:"ok"`
	DBPath          string     `json:"db_path"`
	ExpectedTruthFP string     `json:"expected_truth_fingerprint"`
	StoredTruthFP   string     `json:"stored_truth_fingerprint"`
	CheckedBlocks   int        `json:"checked_blocks"`
	CheckedFrames   int        `json:"checked_frames"`
	CommitOrder     []int      `json:"commit_order"`
	Mismatches      []Mismatch `json:"mismatches,omitempty"`
}

func VerifyDatabase(dbPath, truthFingerprint string) (*Result, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	res := &Result{OK: true, DBPath: dbPath, ExpectedTruthFP: truthFingerprint}
	row := db.QueryRow(`SELECT value FROM snapshot_meta WHERE key='truth_fingerprint'`)
	if err := row.Scan(&res.StoredTruthFP); err != nil {
		return nil, fmt.Errorf("read truth_fingerprint: %w", err)
	}
	if truthFingerprint != "" && res.StoredTruthFP != truthFingerprint {
		res.OK = false
		res.Mismatches = append(res.Mismatches, Mismatch{Field: "truth_fingerprint", Expected: truthFingerprint, Actual: res.StoredTruthFP, Message: "truth fingerprint mismatch"})
		return res, nil
	}

	rows, err := db.Query(`SELECT block_index,payload_json FROM blocks ORDER BY block_index`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var blocks []model.BlockResult
	for rows.Next() {
		var idx int
		var payload string
		if err := rows.Scan(&idx, &payload); err != nil {
			return nil, err
		}
		var block model.BlockResult
		if err := json.Unmarshal([]byte(payload), &block); err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	orderRows, err := db.Query(`SELECT block_index FROM blocks ORDER BY commit_seq`)
	if err == nil {
		defer orderRows.Close()
		for orderRows.Next() {
			var idx int
			if err := orderRows.Scan(&idx); err != nil {
				return nil, err
			}
			res.CommitOrder = append(res.CommitOrder, idx)
		}
	}

	for _, block := range blocks {
		res.CheckedBlocks++
		mismatches := verifyBlock(block)
		if len(mismatches) > 0 {
			res.OK = false
			res.Mismatches = append(res.Mismatches, mismatches...)
			return res, nil
		}
		res.CheckedFrames += len(block.Frames)
	}
	return res, nil
}

func verifyBlock(block model.BlockResult) []Mismatch {
	m := block.Manifest
	p := block.EvalParams.WithDefaults(m)
	origin := block.Block.BufferOriginSample
	bufLen := block.Block.BufferEndSample - origin
	if len(block.Original) != bufLen*m.Channels || len(block.Reconstructed) != bufLen*m.Channels {
		return []Mismatch{blockMismatch(block, "persisted_samples", fmt.Sprintf("want %d interleaved samples", bufLen*m.Channels), fmt.Sprintf("orig=%d recon=%d", len(block.Original), len(block.Reconstructed)))}
	}
	orig := planar(block.Original, m.Channels, bufLen)
	recon := planar(block.Reconstructed, m.Channels, bufLen)

	summary := summarize(block.Reconstructed)
	if summary.SHA256 != block.ReconSummary.SHA256 {
		return []Mismatch{blockMismatch(block, "recon_summary.sha256", block.ReconSummary.SHA256, summary.SHA256)}
	}

	starts := gridStarts(p.WindowSizeSamples, p.StepSamples, m.SamplesPerChannel())
	for _, stored := range block.Frames {
		frame := stored.Frame
		got, err := recomputeFrame(m, p, orig, recon, origin, starts, frame, stored.Quant.QuantStep)
		if err != nil {
			mm := frameMismatch(block, frame, "sample_buffer", "recomputation error", err.Error())
			return []Mismatch{mm}
		}
		if mm := compareFrame(block, stored, got); mm != nil {
			return []Mismatch{*mm}
		}
	}
	return nil
}

func compareFrame(block model.BlockResult, want, got model.FrameReport) *Mismatch {
	checks := []struct {
		field string
		w, g  any
	}{
		{"psnr_db_fixed", want.PSNRDBFixed, got.PSNRDBFixed},
		{"psnr_infinite", want.PSNRInfinite, got.PSNRInfinite},
		{"snr_db_fixed", want.SNRDBFixed, got.SNRDBFixed},
		{"snr_infinite", want.SNRInfinite, got.SNRInfinite},
		{"ssim_fixed", want.SSIMFixed, got.SSIMFixed},
		{"noise_floor_std_fixed", want.NoiseFloorStdFixed, got.NoiseFloorStdFixed},
		{"frame_residual_std_fixed", want.FrameResidualStdFixed, got.FrameResidualStdFixed},
		{"pulse_observed_sample", want.PulseObservedSample, got.PulseObservedSample},
		{"bound.rms_error_upper_fixed", want.Bound.RMSErrorUpperFixed, got.Bound.RMSErrorUpperFixed},
		{"bound.snr_upper_db_fixed", want.Bound.SNRUpperDBFixed, got.Bound.SNRUpperDBFixed},
		{"bound.snr_minimum_db_fixed", want.Bound.SNRMinimumDBFixed, got.Bound.SNRMinimumDBFixed},
	}
	for _, c := range checks {
		if c.w != c.g {
			mm := frameMismatch(block, want.Frame, c.field, c.w, c.g)
			return &mm
		}
	}
	if len(want.Heatmap) == len(got.Heatmap) {
		for i := range want.Heatmap {
			if want.Heatmap[i] != got.Heatmap[i] {
				mm := frameMismatch(block, want.Frame, fmt.Sprintf("heatmap[%d]", i), want.Heatmap[i], got.Heatmap[i])
				return &mm
			}
		}
	}
	return nil
}

func CompareReports(a, b *model.Report) *Mismatch {
	if a == nil || b == nil {
		return &Mismatch{Field: "report", Message: "nil report"}
	}
	af := map[int]model.FrameReport{}
	bf := map[int]model.FrameReport{}
	blockByFrame := map[int]int{}
	for _, blk := range a.Blocks {
		for _, f := range blk.Frames {
			af[f.Frame] = f
			blockByFrame[f.Frame] = blk.Block.Index
		}
	}
	for _, blk := range b.Blocks {
		for _, f := range blk.Frames {
			bf[f.Frame] = f
			if _, ok := blockByFrame[f.Frame]; !ok {
				blockByFrame[f.Frame] = blk.Block.Index
			}
		}
	}
	n := maxInt(len(af), len(bf))
	for frame := 0; frame < n; frame++ {
		x, okx := af[frame]
		y, oky := bf[frame]
		if !okx || !oky {
			f := frame
			return &Mismatch{Frame: &f, BlockIndex: blockByFrame[frame], Field: "frame_present", Expected: okx, Actual: oky}
		}
		coord := findCoord(a, blockByFrame[frame])
		if mm := compareFrame(model.BlockResult{Block: coord}, x, y); mm != nil {
			mm.BlockIndex = blockByFrame[frame]
			c := coord
			mm.Block = &c
			return mm
		}
	}
	if len(a.CommitOrder) != len(b.CommitOrder) {
		return &Mismatch{Field: "commit_order", Expected: a.CommitOrder, Actual: b.CommitOrder, Message: "commit order length differs"}
	}
	for i := range a.CommitOrder {
		if a.CommitOrder[i] != b.CommitOrder[i] {
			return &Mismatch{BlockIndex: i, Field: fmt.Sprintf("commit_order[%d]", i), Expected: a.CommitOrder[i], Actual: b.CommitOrder[i]}
		}
	}
	return nil
}

func findCoord(r *model.Report, idx int) model.BlockCoord {
	for _, b := range r.Blocks {
		if b.Block.Index == idx {
			return b.Block
		}
	}
	return model.BlockCoord{Index: idx}
}

func planar(interleaved []int32, channels, length int) [][]int32 {
	out := make([][]int32, channels)
	for ch := range out {
		out[ch] = make([]int32, length)
	}
	for t := 0; t < length; t++ {
		for ch := 0; ch < channels; ch++ {
			out[ch][t] = interleaved[t*channels+ch]
		}
	}
	return out
}

func recomputeFrame(m model.Manifest, p model.EvalParams, orig, recon [][]int32, origin int, starts []int, frame int, q int64) (model.FrameReport, error) {
	a := frame * m.HopSize
	var signalEnergy, errorEnergy int64
	frameRes := residual{}
	heatSums := make([]int64, 32)
	heatCounts := make([]int64, 32)
	for ch := 0; ch < m.Channels; ch++ {
		for n := 0; n < m.FrameSize; n++ {
			li := a + n - origin
			x := int64(orig[ch][li])
			y := int64(recon[ch][li])
			e := y - x
			signalEnergy += x * x
			errorEnergy += e * e
			frameRes.add(e)
			bin := n * 32 / m.FrameSize
			if bin >= 32 {
				bin = 31
			}
			heatSums[bin] += abs64(e)
			heatCounts[bin]++
		}
	}
	peak := m.PeakAmplitude()
	psnr, psnrInf := psnrFixed(peak, m.FrameSize*m.Channels, errorEnergy)
	snr, snrInf := snrFixed(signalEnergy, errorEnergy)
	idxs := selectWindows(m, p.WindowSizeSamples, starts, frame)
	var ssimSum int64
	var ssimCount int64
	windowRes := residual{}
	w := p.WindowSizeSamples
	for _, idx := range idxs {
		start := starts[idx]
		for ch := 0; ch < m.Channels; ch++ {
			st := pair{}
			for n := 0; n < w; n++ {
				li := start + n - origin
				x := int64(orig[ch][li])
				y := int64(recon[ch][li])
				st.add(x, y)
				windowRes.add(y - x)
			}
			ssimSum += fixedSSIM(st, peak)
			ssimCount++
		}
	}
	heat := make([]int, 32)
	for i := range heat {
		heat[i] = int(math.Round(float64(heatSums[i]) / float64(heatCounts[i])))
	}
	got := model.FrameReport{
		Frame: frame, PSNRDBFixed: psnr, PSNRInfinite: psnrInf,
		SNRDBFixed: snr, SNRInfinite: snrInf,
		SSIMFixed:          average(ssimSum, ssimCount),
		NoiseFloorStdFixed: stdFixed(windowRes), FrameResidualStdFixed: stdFixed(frameRes),
		PulseObservedSample: detectPulse(m, orig, origin, a), Heatmap: heat,
	}
	got.Bound = buildBound(frame, q, m.FrameSize, p.WindowSizeSamples, peak)
	return got, nil
}

type pair struct{ n, sx, sy, sxx, syy, sxy int64 }

func (p *pair) add(x, y int64) {
	p.n++
	p.sx += x
	p.sy += y
	p.sxx += x * x
	p.syy += y * y
	p.sxy += x * y
}

type residual struct{ n, sum, sq int64 }

func (r *residual) add(e int64) { r.n++; r.sum += e; r.sq += e * e }

func stdFixed(r residual) int64 {
	v := float64(r.n*r.sq-r.sum*r.sum) / (float64(r.n) * float64(r.n))
	if v < 0 {
		v = 0
	}
	return int64(math.Round(math.Sqrt(v) * float64(model.MetricScale)))
}

func fixedSSIM(s pair, peak int64) int64 {
	n := float64(s.n)
	mx := float64(s.sx) / n
	my := float64(s.sy) / n
	vx := float64(s.n*s.sxx-s.sx*s.sx) / (n * n)
	vy := float64(s.n*s.syy-s.sy*s.sy) / (n * n)
	cov := float64(s.n*s.sxy-s.sx*s.sy) / (n * n)
	if vx < 0 {
		vx = 0
	}
	if vy < 0 {
		vy = 0
	}
	l := float64(peak)
	c1 := 1e-4 * l * l
	c2 := 9e-4 * l * l
	v := ((2*mx*my + c1) * (2*cov + c2)) / ((mx*mx + my*my + c1) * (vx + vy + c2))
	if v > 1 {
		v = 1
	}
	if v < -1 {
		v = -1
	}
	return int64(math.Round(v * float64(model.MetricScale)))
}

func psnrFixed(peak int64, count int, sse int64) (int64, bool) {
	if sse == 0 {
		return 0, true
	}
	v := 10 * math.Log10(float64(peak*peak)/(float64(sse)/float64(count)))
	return roundFixed(v), false
}

func snrFixed(energy, sse int64) (int64, bool) {
	if sse == 0 {
		return 0, true
	}
	return roundFixed(10 * math.Log10(float64(energy)/float64(sse))), false
}

func roundFixed(v float64) int64 {
	if math.IsInf(v, 1) || v > 180 {
		return 180 * model.MetricScale
	}
	if math.IsInf(v, -1) || v < -180 {
		return -180 * model.MetricScale
	}
	return int64(math.Round(v * float64(model.MetricScale)))
}

func average(sum, count int64) int64 {
	if count == 0 {
		return 0
	}
	return int64(math.Round(float64(sum) / float64(count)))
}

func gridStarts(w, step, total int) []int {
	var out []int
	for s := 0; s+w <= total; s += step {
		out = append(out, s)
	}
	if len(out) == 0 && w <= total {
		out = []int{0}
	}
	return out
}

func selectWindows(m model.Manifest, w int, starts []int, frame int) []int {
	a := frame * m.HopSize
	b := a + m.FrameSize
	var out []int
	for i, s := range starts {
		c := s + w/2
		if c >= a && c < b {
			out = append(out, i)
		}
	}
	if len(out) > 0 {
		return out
	}
	target := a + m.FrameSize/2
	best, bestDist := -1, math.MaxInt
	for i, s := range starts {
		d := absInt(s + w/2 - target)
		if d < bestDist {
			bestDist, best = d, i
		}
	}
	if best >= 0 {
		return []int{best}
	}
	return nil
}

func detectPulse(m model.Manifest, orig [][]int32, origin, start int) int {
	bestSample, bestScore := start, int64(-1)
	for n := 0; n < m.HopSize; n++ {
		li1 := start + n - origin
		var score int64
		if n > 0 {
			li0 := start + n - 1 - origin
			score = abs64(int64(orig[0][li1]) - int64(orig[0][li0]))
		} else {
			score = abs64(int64(orig[0][li1]))
		}
		if score > bestScore {
			bestScore, bestSample = score, start+n
		}
	}
	return bestSample
}

func buildBound(frame int, q int64, fSize, wSize int, peak int64) model.ErrorBound {
	bits := model.CoefficientFracBits
	coeffMax := float64(q)/2 + 0.5/float64(int64(1)<<bits)
	sampleUpper := math.Sqrt(float64(fSize))*coeffMax + 0.5 + 0.5/float64(model.MetricScale)
	rmsUpper := int64(math.Ceil((sampleUpper + 0.5/float64(model.MetricScale)) * float64(model.MetricScale)))
	snrMin := int64(math.Floor(20 * math.Log10(float64(peak)/sampleUpper) * float64(model.MetricScale)))
	snrMax := int64(math.Ceil(10*math.Log10(float64(peak*peak)*float64(wSize))*float64(model.MetricScale) + 0.5))
	return model.ErrorBound{Frame: frame, QuantStep: q, FrameSize: fSize, WindowSize: wSize, RMSErrorUpperFixed: rmsUpper, SNRMinimumDBFixed: snrMin, SNRUpperDBFixed: snrMax}
}

func summarize(samples []int32) model.SampleSummary {
	s := model.SampleSummary{Count: len(samples)}
	h := sha256Sum(samples)
	s.SHA256 = h
	if len(samples) > 0 {
		s.Min, s.Max = samples[0], samples[0]
	}
	for _, v := range samples {
		s.Sum += int64(v)
		s.SumSq += int64(v) * int64(v)
		if v < s.Min {
			s.Min = v
		}
		if v > s.Max {
			s.Max = v
		}
	}
	return s
}

func sha256Sum(samples []int32) string {
	// Canonical JSON is used by the main pipeline's summary helper; this local
	// hash intentionally repeats that deterministic encoding.
	b, _ := json.Marshal(samples)
	return model.HashBytes(b)
}

func blockMismatch(b model.BlockResult, field string, expected, actual any) Mismatch {
	coord := b.Block
	return Mismatch{BlockIndex: b.Block.Index, Block: &coord, Field: field, Expected: expected, Actual: actual}
}

func frameMismatch(b model.BlockResult, frame int, field string, expected, actual any) Mismatch {
	f := frame
	coord := b.Block
	return Mismatch{Frame: &f, BlockIndex: b.Block.Index, Block: &coord, Field: field, Expected: expected, Actual: actual}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
