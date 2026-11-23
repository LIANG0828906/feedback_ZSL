package forensic

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
)

type PixelDivergence struct {
	X          int    `json:"x"`
	Y          int    `json:"y"`
	BlockIndex int    `json:"blockIndex"`
	Stage      string `json:"stage"`
	Want       uint8  `json:"want"`
	Got        uint8  `json:"got"`
}

type Comparison struct {
	Equal             bool             `json:"equal"`
	FirstPixel        *PixelDivergence `json:"firstPixel,omitempty"`
	BlockIndex        int              `json:"blockIndex"`
	Stage             string           `json:"stage"`
	Field             string           `json:"field,omitempty"`
	IncrementalHash   string           `json:"incrementalHash"`
	ReferenceHash     string           `json:"referenceHash"`
	SerialHash        string           `json:"serialHash"`
	SummaryEqual      bool             `json:"summaryEqual"`
	SequenceEqual     bool             `json:"sequenceEqual"`
	HeatmapEqual      bool             `json:"heatmapEqual"`
	IncrementalCommit []int            `json:"incrementalCommit"`
	ReferenceCommit   []int            `json:"referenceCommit"`
	SerialCommit      []int            `json:"serialCommit"`
	Message           string           `json:"message,omitempty"`
}

type RunStep struct {
	Params Params `json:"params"`
	Scope  []int  `json:"scope,omitempty"`
}

type ThreeWayReport struct {
	Equal       bool       `json:"equal"`
	Comparison  Comparison `json:"comparison"`
	Incremental RunResult  `json:"incremental"`
	Full        RunResult  `json:"full"`
	Serial      RunResult  `json:"serial"`
}

// VerifyThreePaths runs the incremental edit history, an empty-cache full run,
// and a single-worker empty-cache run. The returned report compares final
// pixels, per-block summaries and canonical final block commit order.
func VerifyThreePaths(ctx context.Context, img *Image, workDir string, steps []RunStep) (*ThreeWayReport, error) {
	if len(steps) == 0 {
		return nil, ParamError("steps", "at least one run step is required")
	}
	if workDir == "" {
		return nil, ParamError("workDir", "verification workspace is required")
	}
	for i, step := range steps {
		if err := step.Params.Validate(img); err != nil {
			if ferr, ok := AsError(err); ok {
				ferr.Field = fmt.Sprintf("steps[%d].%s", i, ferr.Field)
			}
			return nil, err
		}
	}

	incStore, err := OpenStore(filepath.Join(workDir, "incremental.sqlite"))
	if err != nil {
		return nil, err
	}
	defer incStore.Close()
	incEngine := NewEngine(img, incStore)
	var incResult RunResult
	for _, step := range steps {
		incResult, err = incEngine.Run(ctx, step.Params, RunOptions{ScopeBlocks: step.Scope}, NewControl())
		if err != nil {
			return nil, err
		}
	}

	final := steps[len(steps)-1].Params
	fullStore, err := OpenStore(filepath.Join(workDir, "full.sqlite"))
	if err != nil {
		return nil, err
	}
	defer fullStore.Close()
	fullResult, err := NewEngine(img, fullStore).Run(ctx, final, RunOptions{}, NewControl())
	if err != nil {
		return nil, err
	}

	serialStore, err := OpenStore(filepath.Join(workDir, "serial.sqlite"))
	if err != nil {
		return nil, err
	}
	defer serialStore.Close()
	serialEngine := NewEngine(img, serialStore)
	var serialResult RunResult
	for _, step := range steps {
		p := step.Params
		p.WorkerCount = 1
		p.QueueCapacity = 1
		serialResult, err = serialEngine.Run(ctx, p, RunOptions{ScopeBlocks: step.Scope}, NewControl())
		if err != nil {
			return nil, err
		}
	}

	report := &ThreeWayReport{
		Equal: true, Incremental: incResult, Full: fullResult, Serial: serialResult,
	}
	report.Comparison, err = CompareRuns(incResult, fullResult, serialResult)
	report.Equal = report.Comparison.Equal
	if err != nil {
		return report, err
	}
	return report, nil
}

func CompareRuns(incremental, full, serial RunResult) (Comparison, error) {
	cmp := Comparison{
		IncrementalHash:   incremental.HeatmapSHA256,
		ReferenceHash:     full.HeatmapSHA256,
		SerialHash:        serial.HeatmapSHA256,
		IncrementalCommit: finalCommitSequence(incremental),
		ReferenceCommit:   finalCommitSequence(full),
		SerialCommit:      finalCommitSequence(serial),
		BlockIndex:        -1,
	}
	cmp.SequenceEqual = equalInts(cmp.IncrementalCommit, cmp.ReferenceCommit) &&
		equalInts(cmp.ReferenceCommit, cmp.SerialCommit)
	cmp.SummaryEqual = equalBlockSummaries(incremental.States, full.States) &&
		equalBlockSummaries(full.States, serial.States)
	cmp.HeatmapEqual = equalBytes(incremental.HeatmapRGBA, full.HeatmapRGBA) &&
		equalBytes(full.HeatmapRGBA, serial.HeatmapRGBA)
	if !cmp.HeatmapEqual {
		div := firstPixelDiff(incremental, full, incremental.HeatmapRGBA, full.HeatmapRGBA)
		if div == nil {
			div = firstPixelDiff(full, serial, full.HeatmapRGBA, serial.HeatmapRGBA)
		}
		if div != nil {
			cmp.FirstPixel = div
			cmp.BlockIndex = div.BlockIndex
			cmp.Stage = StageHeatmap
			cmp.Message = fmt.Sprintf("first divergent pixel at (%d,%d) in block %d", div.X, div.Y, div.BlockIndex)
		} else {
			cmp.Stage = StageHeatmap
			cmp.Message = "heatmap dimensions differ"
		}
		return cmp, &Error{Code: CodeDiverge, Block: cmp.BlockIndex, Stage: cmp.Stage, X: pixelX(div), Y: pixelY(div), Message: cmp.Message}
	}
	if !cmp.SummaryEqual {
		idx, stage, field, msg := firstSummaryDiff(incremental.States, full.States)
		if idx < 0 {
			idx, stage, field, msg = firstSummaryDiff(full.States, serial.States)
		}
		cmp.BlockIndex, cmp.Stage, cmp.Field, cmp.Message = idx, stage, field, msg
		return cmp, &Error{Code: CodeDiverge, Block: idx, Stage: stage, Message: msg}
	}
	if !cmp.SequenceEqual {
		cmp.Stage = StageSummary
		cmp.Message = "final block commit sequences differ"
		return cmp, &Error{Code: CodeDiverge, Block: firstIntDiff(cmp.IncrementalCommit, cmp.ReferenceCommit), Stage: StageSummary, Message: cmp.Message}
	}
	cmp.Equal = true
	return cmp, nil
}

func finalCommitSequence(r RunResult) []int {
	out := make([]int, 0, len(r.States))
	for _, s := range r.States {
		out = append(out, s.BlockIndex)
	}
	sort.Ints(out)
	return out
}

func equalBlockSummaries(a, b []BlockState) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].BlockIndex != b[i].BlockIndex ||
			a[i].ContentDigest != b[i].ContentDigest ||
			a[i].ContentHash != b[i].ContentHash ||
			a[i].Fingerprint != b[i].Fingerprint ||
			a[i].Stats != b[i].Stats ||
			a[i].Tampered != b[i].Tampered ||
			a[i].SummaryKey != b[i].SummaryKey {
			return false
		}
	}
	return true
}

func firstSummaryDiff(a, b []BlockState) (int, string, string, string) {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i].BlockIndex != b[i].BlockIndex {
			return a[i].BlockIndex, StageSummary, "blockIndex", "block summary ordering differs"
		}
		if a[i].ContentHash != b[i].ContentHash {
			return a[i].BlockIndex, StageHash, "contentHash", "content hash differs"
		}
		if a[i].Fingerprint != b[i].Fingerprint {
			return a[i].BlockIndex, StageHash, "fingerprint", "fingerprint differs"
		}
		if a[i].Stats != b[i].Stats {
			return a[i].BlockIndex, StageAnalysis, "stats", "per-block statistics differ"
		}
		if a[i].Tampered != b[i].Tampered {
			return a[i].BlockIndex, StageAnalysis, "tampered", "tamper verdict differs"
		}
		if a[i].SummaryKey != b[i].SummaryKey {
			return a[i].BlockIndex, StageSummary, "summaryKey", "summary cache key differs"
		}
	}
	if len(a) != len(b) {
		return -1, StageSummary, "blockCount", "summary block count differs"
	}
	return -1, "", "", ""
}

func firstPixelDiff(runA, runB RunResult, a, b []uint8) *PixelDivergence {
	if runA.Params.WorkerCount < 1 || len(a) == 0 || len(b) == 0 {
		return nil
	}
	width := 0
	for _, chunk := range runA.Chunks {
		if chunk.X1 > width {
			width = chunk.X1
		}
	}
	if width == 0 {
		return nil
	}
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	for i := 0; i < limit; i += 4 {
		if a[i] == b[i] && a[i+1] == b[i+1] && a[i+2] == b[i+2] && a[i+3] == b[i+3] {
			continue
		}
		pixel := i / 4
		x, y := pixel%width, pixel/width
		return &PixelDivergence{X: x, Y: y, BlockIndex: blockAt(runA.Chunks, x, y), Stage: StageHeatmap, Want: b[i], Got: a[i]}
	}
	return nil
}

func blockAt(chunks []Chunk, x, y int) int {
	for _, c := range chunks {
		if x >= c.X0 && x < c.X1 && y >= c.Y0 && y < c.Y1 {
			return c.Index
		}
	}
	return -1
}

func equalBytes(a, b []uint8) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func firstIntDiff(a, b []int) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return a[i]
		}
	}
	if len(a) > 0 {
		return a[len(a)-1]
	}
	return -1
}

func pixelX(d *PixelDivergence) int {
	if d == nil {
		return 0
	}
	return d.X
}

func pixelY(d *PixelDivergence) int {
	if d == nil {
		return 0
	}
	return d.Y
}
