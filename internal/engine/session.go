package engine

import (
	"context"
	"sync"

	"maskpreview/internal/core"
	"maskpreview/internal/segment"
)

type FinalizeResult struct {
	Run        *RunResult                 `json:"run"`
	Comparison *segment.PreviewComparison `json:"comparison"`
}

// Session remembers the final preview snapshot. Finalize refuses to silently
// combine a new parameter snapshot with preview statistics from an older one.
type Session struct {
	mu          sync.Mutex
	lastPreview *segment.PreviewResult
	lastSnap    core.Snapshot
	tolerance   core.PreviewTolerance
}

func NewSession(tolerance ...core.PreviewTolerance) *Session {
	tol := core.PreviewTolerance{CentroidPx: 3.0, AreaRelative: 0.22}
	if len(tolerance) > 0 {
		tol = tolerance[0]
	}
	return &Session{tolerance: tol}
}

func (s *Session) Preview(img *core.Image, snap core.Snapshot, budgetMS int) (*segment.PreviewResult, error) {
	preview, err := segment.Preview(img, snap, budgetMS)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.lastPreview = preview
	s.lastSnap = snap.Clone()
	s.mu.Unlock()
	return preview, nil
}

func (s *Session) Finalize(ctx context.Context, img *core.Image, snap core.Snapshot, truthFingerprint string, store *Store, cfg RunConfig) (*FinalizeResult, error) {
	s.mu.Lock()
	preview := s.lastPreview
	lastSnap := s.lastSnap
	s.mu.Unlock()
	if preview == nil {
		return nil, &core.Error{
			Code:    core.CodeState,
			Op:      "session.finalize",
			Field:   "preview",
			Got:     "missing",
			Want:    "a completed preview for the same snapshot",
			Message: "finalize requires a preceding preview",
		}
	}
	if preview.SnapshotVersion != snap.Version || !sameSnapshotParameters(lastSnap, snap) {
		return nil, core.VersionError("session.finalize", preview.SnapshotVersion, snap.Version)
	}
	if preview.Quantization != snap.Quantization {
		return nil, core.VersionError("session.finalize.quantization", preview.SnapshotVersion, snap.Version)
	}
	run, err := Execute(ctx, img, snap, truthFingerprint, ModeIncremental, store, cfg)
	if err != nil {
		return nil, err
	}
	comparison, err := segment.ComparePreview(run.Exact, preview, s.tolerance)
	if err != nil {
		return &FinalizeResult{Run: run, Comparison: comparison}, err
	}
	return &FinalizeResult{Run: run, Comparison: comparison}, nil
}

func sameSnapshotParameters(a, b core.Snapshot) bool {
	if a.Version != b.Version || a.ParamVersion != b.ParamVersion || a.ImageID != b.ImageID ||
		a.ImageHash != b.ImageHash || a.Params != b.Params || a.Quantization != b.Quantization ||
		a.AlgorithmVersion != b.AlgorithmVersion || len(a.Seeds) != len(b.Seeds) {
		return false
	}
	for i := range a.Seeds {
		if a.Seeds[i] != b.Seeds[i] {
			return false
		}
	}
	return true
}
