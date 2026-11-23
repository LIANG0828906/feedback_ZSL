package terrainbake

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// DiffReport identifies the first pixel or metadata mismatch.
type DiffReport struct {
	Equal      bool                 `json:"equal"`
	BlockID    string               `json:"block_id,omitempty"`
	X          int                  `json:"x,omitempty"`
	Y          int                  `json:"y,omitempty"`
	Field      string               `json:"field,omitempty"`
	Message    string               `json:"message,omitempty"`
	CommitDiff string               `json:"commit_diff,omitempty"`
	SeamStatsA map[string]SeamStats `json:"-"`
	SeamStatsB map[string]SeamStats `json:"-"`
	DiffMask   map[string][]bool    `json:"-"`
}

// CompareSnapshots compares normal pixels, slope pixels, seam statistics and
// the canonical block submission order. The first mismatch is returned as a
// structured E_DIFF error.
func CompareSnapshots(
	a, b map[string]*BlockOutput,
	statsA, statsB map[string]SeamStats,
	seqA, seqB []CommitEvent,
) (*DiffReport, error) {
	rep := &DiffReport{
		SeamStatsA: statsA, SeamStatsB: statsB,
		DiffMask: make(map[string][]bool),
	}
	ids := make([]string, 0, len(a)+len(b))
	seen := make(map[string]bool)
	for id := range a {
		ids = append(ids, id)
		seen[id] = true
	}
	for id := range b {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		oa, okA := a[id]
		ob, okB := b[id]
		if !okA || !okB {
			rep.BlockID = id
			rep.Field = "block"
			rep.Message = "block exists in only one snapshot"
			return rep, diffError(rep)
		}
		if oa.Width != ob.Width || oa.Height != ob.Height {
			rep.BlockID = id
			rep.Field = "dimensions"
			rep.Message = fmt.Sprintf("dimensions differ: %dx%d versus %dx%d", oa.Width, oa.Height, ob.Width, ob.Height)
			return rep, diffError(rep)
		}
		mask := make([]bool, oa.Width*oa.Height)
		for i := 0; i < oa.Width*oa.Height; i++ {
			na, nb := oa.Normals[i], ob.Normals[i]
			if math.Float64bits(na.X) != math.Float64bits(nb.X) ||
				math.Float64bits(na.Y) != math.Float64bits(nb.Y) ||
				math.Float64bits(na.Z) != math.Float64bits(nb.Z) ||
				math.Float64bits(oa.SlopeDeg[i]) != math.Float64bits(ob.SlopeDeg[i]) {
				mask[i] = true
				rep.BlockID = id
				rep.X = i % oa.Width
				rep.Y = i / oa.Width
				rep.Field = "normal_slope"
				rep.Message = fmt.Sprintf("first mismatch: normal (%0.17g,%0.17g,%0.17g)/(%0.17g,%0.17g,%0.17g), slope %0.17g/%0.17g",
					na.X, na.Y, na.Z, nb.X, nb.Y, nb.Z, oa.SlopeDeg[i], ob.SlopeDeg[i])
				rep.DiffMask[id] = mask
				return rep, diffError(rep)
			}
		}
		rep.DiffMask[id] = mask
	}
	for _, id := range ids {
		sa, okA := statsA[id]
		sb, okB := statsB[id]
		if okA != okB || sa.Violations != sb.Violations ||
			math.Float64bits(sa.MaxAngleDeg) != math.Float64bits(sb.MaxAngleDeg) ||
			sa.Constrained != sb.Constrained {
			rep.BlockID = id
			rep.Field = "seam_stats"
			rep.Message = fmt.Sprintf("seam stats differ: %+v versus %+v", sa, sb)
			return rep, diffError(rep)
		}
	}
	if len(seqA) != len(seqB) {
		rep.Field = "commit_sequence"
		rep.Message = fmt.Sprintf("commit sequence length differs: %d versus %d", len(seqA), len(seqB))
		return rep, diffError(rep)
	}
	for i := range seqA {
		if seqA[i].BlockID != seqB[i].BlockID {
			rep.Field = "commit_sequence"
			rep.BlockID = seqA[i].BlockID
			rep.CommitDiff = fmt.Sprintf("index %d: %s versus %s", i, seqA[i].BlockID, seqB[i].BlockID)
			rep.Message = rep.CommitDiff
			return rep, diffError(rep)
		}
	}
	rep.Equal = true
	return rep, nil
}

func diffError(rep *DiffReport) error {
	return &Error{
		Code: CodeDiff, Object: "snapshot", Field: rep.Field,
		BlockID: rep.BlockID, X: rep.X, Y: rep.Y,
		Message: rep.Message,
		Details: map[string]any{
			"commit_diff": rep.CommitDiff,
		},
	}
}

// ThreePathReport records the outcomes and first-difference diagnosis for a
// live-incremental, interrupted-resume, and empty-cache full bake.
type ThreePathReport struct {
	Incremental *BakeResult `json:"incremental"`
	Resume      *BakeResult `json:"resume"`
	Full        *BakeResult `json:"full"`
	Equal       bool        `json:"equal"`
	Diff        *DiffReport `json:"diff,omitempty"`
}

// VerifyThreePaths runs three independent stores with the same project and
// edit sequence. The resume path is forcibly paused after the first committed
// block and restarted through NewEngine+Resume.
func VerifyThreePaths(ctx context.Context, base *Project, edits []Edit, rootDir string, opts EngineOptions) (*ThreePathReport, error) {
	if rootDir == "" {
		return nil, &Error{Code: CodeParam, Object: "verify_three_paths", Field: "root_dir", Message: "root directory is required"}
	}
	if err := ValidateProject(base); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, &Error{Code: CodeIO, Object: "verify_three_paths", Field: "root_dir", Message: err.Error()}
	}
	baseOpts, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	report := &ThreePathReport{}

	makeOpts := func(name string) EngineOptions {
		o := baseOpts
		o.StoreDir = filepath.Join(rootDir, name)
		o.FaultInjector = nil
		o.PauseAfter = 0
		return o
	}
	resetDir := func(name string) error {
		if err := os.RemoveAll(filepath.Join(rootDir, name)); err != nil {
			return &Error{Code: CodeIO, Object: "verify_three_paths", Field: name, Message: err.Error()}
		}
		return nil
	}

	// Live incremental path.
	if err := resetDir("live"); err != nil {
		return nil, err
	}
	liveProject, _ := cloneProject(base)
	live, err := NewEngine(liveProject, makeOpts("live"))
	if err != nil {
		return nil, err
	}
	if _, err := live.FullBake(ctx); err != nil {
		return nil, err
	}
	liveResult, err := live.IncrementalBake(ctx, edits)
	if err != nil {
		return nil, err
	}
	liveOut, err := live.Outputs()
	if err != nil {
		return nil, err
	}
	liveManifest := live.CheckpointManifest()
	liveResult.SeamStats = seamStatsFromOutputs(liveOut)
	liveResult.ContentHash = liveManifest.TargetContentHash
	report.Incremental = liveResult

	// Interrupted resume path. Use the current edited project to reopen.
	if err := resetDir("resume"); err != nil {
		return nil, err
	}
	resumeProject, _ := cloneProject(base)
	resumeOpts := makeOpts("resume")
	resumeEngine, err := NewEngine(resumeProject, resumeOpts)
	if err != nil {
		return nil, err
	}
	if _, err := resumeEngine.FullBake(ctx); err != nil {
		return nil, err
	}
	resumeOpts.PauseAfter = 1
	resumeEngine.opts.PauseAfter = 1
	_, err = resumeEngine.IncrementalBake(ctx, edits)
	if err != nil {
		return nil, err
	}
	editedProject, err := resumeEngine.ProjectSnapshot()
	if err != nil {
		return nil, err
	}
	restartOpts := makeOpts("resume")
	restartOpts.PauseAfter = 0
	restarted, err := NewEngine(editedProject, restartOpts)
	if err != nil {
		return nil, err
	}
	resumeResult, err := restarted.Resume(ctx)
	if err != nil {
		return nil, err
	}
	resumeOut, err := restarted.Outputs()
	if err != nil {
		return nil, err
	}
	resumeManifest := restarted.CheckpointManifest()
	resumeResult.SeamStats = seamStatsFromOutputs(resumeOut)
	resumeResult.ContentHash = resumeManifest.TargetContentHash
	report.Resume = resumeResult

	// Empty-cache full path.
	if err := resetDir("full"); err != nil {
		return nil, err
	}
	fullProject, _ := cloneProject(editedProject)
	full, err := NewEngine(fullProject, makeOpts("full"))
	if err != nil {
		return nil, err
	}
	fullResult, err := full.FullBake(ctx)
	if err != nil {
		return nil, err
	}
	fullOut, err := full.Outputs()
	if err != nil {
		return nil, err
	}
	fullManifest := full.CheckpointManifest()
	fullResult.SeamStats = seamStatsFromOutputs(fullOut)
	fullResult.ContentHash = fullManifest.TargetContentHash
	report.Full = fullResult

	d1, err := CompareSnapshots(liveOut, resumeOut, report.Incremental.SeamStats, report.Resume.SeamStats, report.Incremental.CommitSequence, report.Resume.CommitSequence)
	if err != nil {
		report.Diff = d1
		return report, err
	}
	d2, err := CompareSnapshots(liveOut, fullOut, report.Incremental.SeamStats, report.Full.SeamStats, report.Incremental.CommitSequence, report.Full.CommitSequence)
	if err != nil {
		report.Diff = d2
		return report, err
	}
	report.Equal = true
	return report, nil
}

func seamStatsFromOutputs(outputs map[string]*BlockOutput) map[string]SeamStats {
	out := make(map[string]SeamStats, len(outputs))
	for id, block := range outputs {
		out[id] = block.Seam
	}
	return out
}

// ProjectSnapshot returns a deep copy of the current project state.
func (e *Engine) ProjectSnapshot() (*Project, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneProject(e.project)
}
