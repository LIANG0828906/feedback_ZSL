package verifier

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"qkernel/internal/attribution"
	"qkernel/internal/chain"
	"qkernel/internal/errs"
	"qkernel/internal/imagex"
	"qkernel/internal/metrics"
	"qkernel/internal/session"
	"qkernel/internal/synth"
)

type Violation struct {
	BlockX    int    `json:"block_x"`
	BlockY    int    `json:"block_y"`
	StepIndex int    `json:"step_index"`
	Field     string `json:"field"`
	Expected  string `json:"expected,omitempty"`
	Actual    string `json:"actual,omitempty"`
	Message   string `json:"message"`
}

type Report struct {
	OK                  bool                   `json:"ok"`
	Root                string                 `json:"root"`
	RunID               string                 `json:"run_id"`
	FrameID             string                 `json:"frame_id"`
	FinalHash           string                 `json:"final_hash"`
	MetricsFileHash     string                 `json:"metrics_file_hash"`
	AttributionFileHash string                 `json:"attribution_file_hash"`
	CommitSequenceHash  string                 `json:"commit_sequence_hash"`
	CommitCount         int                    `json:"commit_count"`
	Violations          []Violation            `json:"violations"`
	Commits             []session.CommitRecord `json:"-"`
}

func VerifyRoot(root, runID string) (*Report, error) {
	store, err := session.OpenStore(root)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	var run *session.RunRecord
	if runID == "" {
		run, err = store.LatestRun()
	} else {
		run, err = store.GetRun(runID)
	}
	if err != nil {
		return nil, err
	}
	rep := &Report{OK: true, Root: root, RunID: run.RunID, FrameID: run.FrameID}

	truth, err := synth.LoadTruth(filepath.Join(root, "truth", run.FrameID+".json"))
	if err != nil {
		return nil, err
	}
	if truth.Fingerprint == "" {
		return nil, errs.New(errs.ESchema, "truth fingerprint missing")
	}

	base, err := readBaseFrame(root, truth)
	if err != nil {
		return nil, err
	}
	verifySamples(rep, base, truth, run.MetricBlockSize)

	commits, err := store.ListCommits(run.RunID)
	if err != nil {
		return nil, err
	}
	rep.Commits = commits
	rep.CommitCount = len(commits)

	segCols := (truth.Frame.Width + run.SegmentSize - 1) / run.SegmentSize
	segRows := (truth.Frame.Height + run.SegmentSize - 1) / run.SegmentSize
	segCount := segCols * segRows
	byStep := map[int]map[int]session.CommitRecord{}
	maxStep := 0
	for _, c := range commits {
		if c.StepIndex > maxStep {
			maxStep = c.StepIndex
		}
		if byStep[c.StepIndex] == nil {
			byStep[c.StepIndex] = map[int]session.CommitRecord{}
		}
		byStep[c.StepIndex][c.SegmentIndex] = c
		expectedOrder := (c.StepIndex-1)*segCount + c.SegmentIndex
		if c.CommitOrder != expectedOrder {
			rep.add(Violation{BlockX: c.SegmentIndex % segCols, BlockY: c.SegmentIndex / segCols, StepIndex: c.StepIndex, Field: "commit_order", Expected: itoa(expectedOrder), Actual: itoa(c.CommitOrder), Message: "commit order is not stable"})
		}
	}

	stepFrames := map[int]*imagex.Frame{0: base}
	for stepNo := 1; stepNo <= maxStep; stepNo++ {
		stepCommits := byStep[stepNo]
		if len(stepCommits) != segCount {
			rep.add(Violation{StepIndex: stepNo, Field: "commit_count", Expected: itoa(segCount), Actual: itoa(len(stepCommits)), Message: "missing persisted segment commits"})
		}
		frame := imagex.NewFrame(truth.Frame.Width, truth.Frame.Height, truth.Frame.Channels)
		for segIndex := 0; segIndex < segCount; segIndex++ {
			c, ok := stepCommits[segIndex]
			if !ok {
				continue
			}
			sr := segmentRect(segIndex, segCols, run.SegmentSize, truth.Frame.Width, truth.Frame.Height)
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(c.OutputPath)))
			if err != nil {
				rep.add(Violation{BlockX: segIndex % segCols, BlockY: segIndex / segCols, StepIndex: stepNo, Field: "output_blob", Message: err.Error()})
				continue
			}
			if got := imagex.HashBytes(b); got != c.OutputHash {
				rep.add(Violation{BlockX: segIndex % segCols, BlockY: segIndex / segCols, StepIndex: stepNo, Field: "output_hash", Expected: c.OutputHash, Actual: got, Message: "segment blob hash mismatch"})
			}
			wantLen := sr.Dx() * sr.Dy() * truth.Frame.Channels
			if len(b) != wantLen {
				rep.add(Violation{BlockX: segIndex % segCols, BlockY: segIndex / segCols, StepIndex: stepNo, Field: "blob_length", Expected: itoa(wantLen), Actual: itoa(len(b)), Message: "segment blob length mismatch"})
				continue
			}
			frame.CopyRectFrom(sr, b)
		}
		stepFrames[stepNo] = frame
	}

	final := stepFrames[maxStep]
	if final == nil {
		return nil, errs.New(errs.EState, "no final step frame could be reconstructed")
	}
	rep.FinalHash = final.Hash()
	verifyFinalRAW(rep, root, run.FrameID, final, run.MetricBlockSize, maxStep)

	metricStates := verifyMetrics(rep, root, run.FrameID, base, final, stepFrames, maxStep, run.MetricBlockSize)
	verifyAttribution(rep, root, store, run, metricStates, truth)

	if b, err := os.ReadFile(filepath.Join(root, "metrics", run.FrameID+".json")); err == nil {
		rep.MetricsFileHash = imagex.HashBytes(b)
	}
	if b, err := os.ReadFile(filepath.Join(root, "attribution", run.FrameID+".json")); err == nil {
		rep.AttributionFileHash = imagex.HashBytes(b)
	}
	seqBytes, _ := json.Marshal(normalizeCommits(commits))
	rep.CommitSequenceHash = imagex.HashBytes(seqBytes)
	rep.OK = len(rep.Violations) == 0
	return rep, nil
}

func (r *Report) add(v Violation) {
	if v.BlockX == 0 && v.BlockY == 0 && v.Message != "" {
		// Keep zero coordinates valid; no special state needed.
	}
	r.Violations = append(r.Violations, v)
	r.OK = false
}

func readBaseFrame(root string, truth *synth.GroundTruth) (*imagex.Frame, error) {
	rawPath := filepath.Join(root, "base", truth.Frame.ID+".rgb")
	b, err := os.ReadFile(rawPath)
	if err != nil {
		png, pngErr := imagex.LoadPNG(filepath.Join(root, "base", truth.Frame.ID+".png"))
		if pngErr != nil {
			return nil, errs.Wrap(errs.EIO, err, "read persisted raw base; png fallback also failed: %v", pngErr)
		}
		return png, nil
	}
	want := truth.Frame.Width * truth.Frame.Height * truth.Frame.Channels
	if len(b) != want {
		return nil, errs.New(errs.EMeta, "base raw length %d conflicts with truth frame size %d", len(b), want)
	}
	return &imagex.Frame{Width: truth.Frame.Width, Height: truth.Frame.Height, Channels: truth.Frame.Channels, Pix: b}, nil
}

func verifySamples(rep *Report, base *imagex.Frame, truth *synth.GroundTruth, metricBlock int) {
	for _, s := range truth.Samples {
		p := base.Pixel(s.X, s.Y)
		for c := 0; c < 3; c++ {
			if int(p[c]) != s.Expected[c] {
				bx, by := blockOf(s.X, s.Y, metricBlock)
				rep.add(Violation{BlockX: bx, BlockY: by, StepIndex: 0, Field: "truth_samples.expected", Expected: itoa(s.Expected[c]), Actual: itoa(int(p[c])), Message: "persisted base pixel conflicts with recorded truth sample"})
				return
			}
		}
	}
}

func verifyFinalRAW(rep *Report, root, frameID string, final *imagex.Frame, metricBlock, maxStep int) {
	raw, err := os.ReadFile(filepath.Join(root, "images", frameID, "final.rgb"))
	if err != nil {
		rep.add(Violation{StepIndex: maxStep, Field: "final_raw", Message: err.Error()})
		return
	}
	if len(raw) != len(final.Pix) {
		rep.add(Violation{StepIndex: maxStep, Field: "final_raw_length", Expected: itoa(len(final.Pix)), Actual: itoa(len(raw)), Message: "final raw length mismatch"})
		return
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] == final.Pix[i] {
			continue
		}
		pixel := i / final.Channels
		x := pixel % final.Width
		y := pixel / final.Width
		bx, by := blockOf(x, y, metricBlock)
		channel := i % final.Channels
		rep.add(Violation{BlockX: bx, BlockY: by, StepIndex: maxStep, Field: "final_pixels.channel_" + itoa(channel), Expected: itoa(int(final.Pix[i])), Actual: itoa(int(raw[i])), Message: "persisted final image differs from segment reconstruction"})
		return
	}
}

func verifyMetrics(rep *Report, root, frameID string, base, final *imagex.Frame, steps map[int]*imagex.Frame, maxStep, blockSize int) []*metrics.Report {
	baseM, err := metrics.Compute(base, blockSize)
	if err != nil {
		rep.add(Violation{StepIndex: 0, Field: "metric_block_size", Message: err.Error()})
		return nil
	}
	finalM, err := metrics.Compute(final, blockSize)
	if err != nil {
		rep.add(Violation{StepIndex: maxStep, Field: "metric_block_size", Message: err.Error()})
		return nil
	}
	compared, err := metrics.Compare(baseM, finalM)
	if err != nil {
		rep.add(Violation{StepIndex: maxStep, Field: "metrics", Message: err.Error()})
		return nil
	}
	states := []*metrics.Report{baseM}
	for i := 1; i <= maxStep; i++ {
		m, err := metrics.Compute(steps[i], blockSize)
		if err != nil {
			rep.add(Violation{StepIndex: i, Field: "metrics", Message: err.Error()})
			continue
		}
		states = append(states, m)
	}

	persistedBytes, err := os.ReadFile(filepath.Join(root, "metrics", frameID+".json"))
	if err != nil {
		rep.add(Violation{StepIndex: maxStep, Field: "metrics_file", Message: err.Error()})
		return states
	}
	var persisted metrics.Report
	if err := json.Unmarshal(persistedBytes, &persisted); err != nil {
		rep.add(Violation{StepIndex: maxStep, Field: "metrics_file", Message: err.Error()})
		return states
	}
	for i, want := range compared.Blocks {
		if i >= len(persisted.Blocks) {
			rep.add(Violation{BlockX: want.X, BlockY: want.Y, StepIndex: maxStep, Field: "metrics.block_count", Message: "missing persisted metric block"})
			break
		}
		got := persisted.Blocks[i]
		checkFloat(rep, want.X, want.Y, maxStep, "metrics.noise_variance", want.NoiseVariance, got.NoiseVariance)
		checkFloat(rep, want.X, want.Y, maxStep, "metrics.edge_clarity", want.EdgeClarity, got.EdgeClarity)
		checkFloat(rep, want.X, want.Y, maxStep, "metrics.chroma_cb_mean", want.ChromaCbMean, got.ChromaCbMean)
		checkFloat(rep, want.X, want.Y, maxStep, "metrics.chroma_cr_mean", want.ChromaCrMean, got.ChromaCrMean)
	}
	return states
}

func verifyAttribution(rep *Report, root string, store *session.Store, run *session.RunRecord, states []*metrics.Report, truth *synth.GroundTruth) {
	if len(states) < 2 {
		return
	}
	chainJSON, _, err := store.ChainForVersion(run.ParamVersion)
	if err != nil {
		rep.add(Violation{Field: "param_version", StepIndex: 0, Message: err.Error()})
		return
	}
	var c chain.Chain
	if err := json.Unmarshal([]byte(chainJSON), &c); err != nil {
		rep.add(Violation{Field: "chain_json", StepIndex: 0, Message: err.Error()})
		return
	}
	types := make([]string, len(c.Steps))
	for i, s := range c.Steps {
		types[i] = string(s.Type)
	}
	recomputed, recomputeErr := attribution.BuildReport(states, types, attribution.DefaultDeclaredShare)
	persistedBytes, err := os.ReadFile(filepath.Join(root, "attribution", run.FrameID+".json"))
	if err != nil {
		rep.add(Violation{Field: "attribution_file", StepIndex: len(c.Steps), Message: err.Error()})
		return
	}
	var persisted attribution.Report
	if err := json.Unmarshal(persistedBytes, &persisted); err != nil {
		rep.add(Violation{Field: "attribution_file", StepIndex: len(c.Steps), Message: err.Error()})
		return
	}
	for i, want := range recomputed.Blocks {
		if i >= len(persisted.Blocks) {
			rep.add(Violation{BlockX: want.BlockX, BlockY: want.BlockY, Field: "attribution.block_count", StepIndex: len(c.Steps), Message: "missing persisted attribution block"})
			break
		}
		got := persisted.Blocks[i]
		if got.Status != want.Status || got.StepIndex != want.StepIndex {
			rep.add(Violation{BlockX: want.BlockX, BlockY: want.BlockY, Field: "attribution.step_index", StepIndex: want.StepIndex, Expected: want.Status + ":" + itoa(want.StepIndex), Actual: got.Status + ":" + itoa(got.StepIndex), Message: "persisted attribution differs from independent recomputation"})
		}
	}
	if err := attribution.CheckTruth(recomputed, truth); err != nil {
		var ke *errs.Error
		if errors.As(err, &ke) {
			rep.add(Violation{BlockX: ke.BlockX, BlockY: ke.BlockY, Field: "attribution.truth_implant", StepIndex: firstCandidateOr(ke.Candidates), Expected: "truth implant", Actual: itoa(ke.StepIndex), Message: ke.Error()})
		} else {
			rep.add(Violation{Field: "attribution.truth_implant", Message: err.Error()})
		}
	}
	if recomputeErr != nil {
		var ke *errs.Error
		if errors.As(recomputeErr, &ke) {
			rep.add(Violation{BlockX: ke.BlockX, BlockY: ke.BlockY, Field: "attribution.share", StepIndex: firstCandidateOr(ke.Candidates), Actual: "ambiguous", Message: ke.Error()})
		}
	}
}

type CompareReport struct {
	OK             bool       `json:"ok"`
	RunID          string     `json:"run_id"`
	Reports        []*Report  `json:"reports"`
	FirstViolation *Violation `json:"first_violation,omitempty"`
}

func CompareRoots(runID string, roots ...string) (*CompareReport, error) {
	if len(roots) < 2 {
		return nil, errs.New(errs.EPara, "at least two roots are required")
	}
	cr := &CompareReport{OK: true, RunID: runID}
	var base *Report
	for _, root := range roots {
		vr, err := VerifyRoot(root, runID)
		if err != nil {
			return nil, err
		}
		cr.Reports = append(cr.Reports, vr)
		if base == nil {
			base = vr
			if cr.RunID == "" {
				cr.RunID = vr.RunID
			}
			continue
		}
		if v := compareReport(base, vr); v != nil {
			cr.OK = false
			cr.FirstViolation = v
			return cr, nil
		}
	}
	for _, vr := range cr.Reports {
		if !vr.OK {
			cr.OK = false
			for _, v := range vr.Violations {
				vv := v
				cr.FirstViolation = &vv
				return cr, nil
			}
		}
	}
	return cr, nil
}

func compareReport(a, b *Report) *Violation {
	if a.FinalHash != b.FinalHash {
		return &Violation{Field: "final_hash", Expected: a.FinalHash, Actual: b.FinalHash, Message: "final pixel hash differs across paths"}
	}
	if a.MetricsFileHash != b.MetricsFileHash {
		return &Violation{Field: "metrics_file_hash", Expected: a.MetricsFileHash, Actual: b.MetricsFileHash, Message: "metric report differs across paths"}
	}
	if a.AttributionFileHash != b.AttributionFileHash {
		return &Violation{Field: "attribution_file_hash", Expected: a.AttributionFileHash, Actual: b.AttributionFileHash, Message: "attribution report differs across paths"}
	}
	ca := normalizeCommits(a.Commits)
	cb := normalizeCommits(b.Commits)
	if len(ca) != len(cb) {
		return &Violation{Field: "commit_count", Expected: itoa(len(ca)), Actual: itoa(len(cb)), Message: "commit sequence length differs"}
	}
	for i := range ca {
		if v := compareCommit(ca[i], cb[i]); v != nil {
			return v
		}
	}
	return nil
}

type normalizedCommit struct {
	CommitOrder      int             `json:"commit_order"`
	FrameID          string          `json:"frame_id"`
	SegmentIndex     int             `json:"segment_index"`
	StepIndex        int             `json:"step_index"`
	CacheKey         string          `json:"cache_key"`
	InputHash        string          `json:"input_hash"`
	OutputHash       string          `json:"output_hash"`
	OutputPath       string          `json:"output_path"`
	Region           imagex.Rect     `json:"region"`
	UpstreamKeys     []string        `json:"upstream_keys"`
	Summary          json.RawMessage `json:"summary"`
	ChainFingerprint string          `json:"chain_fingerprint"`
	ParamVersion     int             `json:"param_version"`
	AlgoVersion      string          `json:"algo_version"`
}

func normalizeCommits(in []session.CommitRecord) []normalizedCommit {
	out := make([]normalizedCommit, 0, len(in))
	for _, c := range in {
		out = append(out, normalizedCommit{
			CommitOrder: c.CommitOrder, FrameID: c.FrameID, SegmentIndex: c.SegmentIndex, StepIndex: c.StepIndex,
			CacheKey: c.CacheKey, InputHash: c.InputHash, OutputHash: c.OutputHash, OutputPath: c.OutputPath,
			Region: c.Region, UpstreamKeys: append([]string(nil), c.UpstreamKeys...), Summary: c.Summary,
			ChainFingerprint: c.ChainFingerprint, ParamVersion: c.ParamVersion, AlgoVersion: c.AlgoVersion,
		})
	}
	return out
}

func compareCommit(a, b normalizedCommit) *Violation {
	segCols := 1
	if a.CommitOrder >= 0 {
		// Segment coordinates are recovered by callers when needed; order is enough here.
	}
	checks := []struct {
		field string
		eq    bool
		av    any
		bv    any
	}{
		{"commit_order", a.CommitOrder == b.CommitOrder, a.CommitOrder, b.CommitOrder},
		{"frame_id", a.FrameID == b.FrameID, a.FrameID, b.FrameID},
		{"segment_index", a.SegmentIndex == b.SegmentIndex, a.SegmentIndex, b.SegmentIndex},
		{"step_index", a.StepIndex == b.StepIndex, a.StepIndex, b.StepIndex},
		{"cache_key", a.CacheKey == b.CacheKey, a.CacheKey, b.CacheKey},
		{"input_hash", a.InputHash == b.InputHash, a.InputHash, b.InputHash},
		{"output_hash", a.OutputHash == b.OutputHash, a.OutputHash, b.OutputHash},
		{"summary", string(a.Summary) == string(b.Summary), string(a.Summary), string(b.Summary)},
		{"region", a.Region == b.Region, a.Region, b.Region},
		{"upstream_keys", strings.Join(a.UpstreamKeys, "|") == strings.Join(b.UpstreamKeys, "|"), a.UpstreamKeys, b.UpstreamKeys},
		{"chain_fingerprint", a.ChainFingerprint == b.ChainFingerprint, a.ChainFingerprint, b.ChainFingerprint},
		{"param_version", a.ParamVersion == b.ParamVersion, a.ParamVersion, b.ParamVersion},
		{"algo_version", a.AlgoVersion == b.AlgoVersion, a.AlgoVersion, b.AlgoVersion},
	}
	for _, c := range checks {
		if !c.eq {
			return &Violation{StepIndex: a.StepIndex, Field: "commits." + c.field, Expected: toString(c.av), Actual: toString(c.bv), Message: "persisted commit sequence differs"}
		}
	}
	_ = segCols
	return nil
}

func segmentRect(index, cols, size, w, h int) imagex.Rect {
	x := (index % cols) * size
	y := (index / cols) * size
	x2 := minInt(x+size, w)
	y2 := minInt(y+size, h)
	return imagex.R(x, y, x2, y2)
}

func blockOf(x, y, block int) (int, int) {
	if block <= 0 {
		block = 1
	}
	return x / block, y / block
}

func checkFloat(rep *Report, bx, by int, step int, field string, want, got float64) {
	diff := want - got
	if diff < 0 {
		diff = -diff
	}
	scale := 1.0
	if want < 0 {
		scale = -want
	} else if want > 1 {
		scale = want
	}
	if diff > 1e-8*scale+1e-10 {
		rep.add(Violation{BlockX: bx, BlockY: by, StepIndex: step, Field: field, Expected: formatFloat(want), Actual: formatFloat(got), Message: "metric value differs from independent recomputation"})
	}
}

func firstCandidateOr(c []int) int {
	if len(c) > 0 {
		return c[0]
	}
	return -1
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}

func formatFloat(v float64) string {
	return jsonNumber(v).String()
}

func jsonNumber(v float64) json.Number {
	b, _ := json.Marshal(v)
	return json.Number(b)
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}
