package fog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type MediumSummary struct {
	ID           string  `json:"id"`
	Bounds       AABB    `json:"bounds"`
	Resolution   [3]int  `json:"resolution"`
	DensityMin   int32   `json:"density_min"`
	DensityMax   int32   `json:"density_max"`
	AbsorptionFP int64   `json:"absorption_fp"`
	ScatteringFP int64   `json:"scattering_fp"`
	Anisotropy   float64 `json:"anisotropy"`
	ContentHash  string  `json:"content_hash"`
}

type SceneSummary struct {
	Name            string          `json:"name"`
	Seed            int64           `json:"seed"`
	Width           int             `json:"width"`
	Height          int             `json:"height"`
	Media           []MediumSummary `json:"media"`
	CameraSamples   int             `json:"camera_samples"`
	TruthRays       int             `json:"truth_rays"`
	SnapshotVersion string          `json:"snapshot_version"`
	CameraVersion   string          `json:"camera_version"`
	RecipeVersion   string          `json:"recipe_version"`
	Algorithm       string          `json:"algorithm"`
	Recipe          Recipe          `json:"recipe"`
}

func SummarizeScene(s *Scene) SceneSummary {
	sum := SceneSummary{
		Name: s.Name, Seed: s.Seed, Width: s.Width, Height: s.Height,
		CameraSamples: len(s.Cameras), TruthRays: len(s.Truth),
		SnapshotVersion: s.ContentHash(s.Recipe.CoefficientScale),
		CameraVersion:   s.CameraHash(), RecipeVersion: s.RecipeHash(),
		Algorithm: s.Recipe.AlgorithmVersion, Recipe: s.Recipe,
	}
	for i := range s.Media {
		m := &s.Media[i]
		ms := MediumSummary{
			ID: m.ID, Bounds: m.Bounds, Resolution: m.DensityRes,
			AbsorptionFP: m.AbsorptionFP, ScatteringFP: m.ScatteringFP,
			Anisotropy: m.Anisotropy, ContentHash: m.ContentHash(s.Recipe.CoefficientScale),
		}
		if len(m.Density) > 0 {
			ms.DensityMin, ms.DensityMax = m.Density[0], m.Density[0]
			for _, d := range m.Density[1:] {
				if d < ms.DensityMin {
					ms.DensityMin = d
				}
				if d > ms.DensityMax {
					ms.DensityMax = d
				}
			}
		}
		sum.Media = append(sum.Media, ms)
	}
	return sum
}

type DiagnosticsReport struct {
	GeneratedAt     string                `json:"generated_at"`
	WorkDir         string                `json:"work_dir"`
	Scene           SceneSummary          `json:"scene"`
	Preview         *Frame                `json:"preview,omitempty"`
	Exact           *Frame                `json:"exact,omitempty"`
	Comparison      *FrameComparison      `json:"comparison,omitempty"`
	ComparisonFault *Fault                `json:"comparison_fault,omitempty"`
	CacheNoEditHits int                   `json:"cache_no_edit_hits"`
	PostEdit        *Frame                `json:"post_edit,omitempty"`
	PostEditHits    int                   `json:"post_edit_cache_hits"`
	EditReport      *InvalidationReport   `json:"edit_invalidation,omitempty"`
	EditMedia       string                `json:"edit_media,omitempty"`
	EditVoxel       int                   `json:"edit_voxel"`
	EditFrom        int32                 `json:"edit_from"`
	EditTo          int32                 `json:"edit_to"`
	Injections      []*InjectionReport    `json:"injections"`
	ThreePath       *ThreePathReport      `json:"three_path,omitempty"`
	ThreePathFault  *Fault                `json:"three_path_fault,omitempty"`
	Checkpoint      CheckpointDiagnostics `json:"checkpoint"`
	Notes           []string              `json:"notes,omitempty"`
}

func RunDiagnostics(scene *Scene, baseDir string) (*DiagnosticsReport, error) {
	scene = scene.Clone()
	if f := scene.Validate(); f != nil {
		return nil, f
	}
	if baseDir == "" {
		baseDir = os.TempDir()
	}
	root, err := os.MkdirTemp(baseDir, "vfog-diagnostics-")
	if err != nil {
		return nil, err
	}
	rep := &DiagnosticsReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		WorkDir:     root,
		Scene:       SummarizeScene(scene),
	}
	preview := renderForDiagnostics(scene, nil, RenderOptions{Mode: RenderPreview}, rep)
	rep.Preview = preview
	checkpointPath := filepath.Join(root, "checkpoint.vfogj")
	store, err := OpenCheckpointStore(checkpointPath)
	if err != nil {
		return nil, err
	}
	engine := NewEngine(scene, store)
	exact, err := engine.Render(context.Background(), RenderOptions{Mode: RenderExact, MaxParallelTiles: scene.Recipe.MaxParallelTiles})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	rep.Exact = exact
	if preview != nil {
		comp, fault := CompareFrames(preview, exact, scene.Recipe.Tolerance)
		rep.Comparison = comp
		rep.ComparisonFault = fault
	}
	noEdit, err := engine.Render(context.Background(), RenderOptions{Mode: RenderExact, MaxParallelTiles: scene.Recipe.MaxParallelTiles})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	rep.CacheNoEditHits = noEdit.Stats.CacheHits
	graph := engine.DependencyGraph()
	node, inv := chooseInvalidationCandidate(graph, len(exact.Tiles))
	if node != nil {
		mediaID := SourceMediaID(node.ID)
		voxel := sourceVoxel(node.ID)
		_, m := scene.MediaByID(mediaID)
		if m != nil && voxel >= 0 && voxel < len(m.Density) {
			from := m.Density[voxel]
			to := from + 1
			if to > DensityMax {
				to = from - 1
			}
			report, editErr := engine.EditDensity(mediaID, voxel, to)
			if editErr != nil {
				return nil, editErr
			}
			rep.EditReport = report
			rep.EditMedia, rep.EditVoxel, rep.EditFrom, rep.EditTo = mediaID, voxel, from, to
			post, renderErr := engine.Render(context.Background(), RenderOptions{Mode: RenderExact, MaxParallelTiles: scene.Recipe.MaxParallelTiles})
			if renderErr != nil {
				return nil, renderErr
			}
			rep.PostEdit = post
			rep.PostEditHits = post.Stats.CacheHits
		}
	} else {
		rep.Notes = append(rep.Notes, "no dependency source had downstream tiles; edit invalidation was not exercised")
		_ = inv
	}
	rep.Checkpoint = store.Diagnostics()
	if err := store.Close(); err != nil {
		return nil, err
	}

	failTile := "tile-r00-c00"
	if exact != nil && len(exact.Tiles) > 0 {
		failTile = exact.Tiles[0].ID
	}
	failMedia := "unknown"
	if len(scene.Media) > 0 {
		failMedia = scene.Media[0].ID
	}
	for _, kind := range []InjectionKind{InjectDecode, InjectOrder, InjectDensity, InjectBlock} {
		rep.Injections = append(rep.Injections, RunInjection(scene, kind, failTile, failMedia))
	}
	tp, tpErr := RunThreePath(scene, root, ThreePathOptions{
		ResumeFraction: 0.5, ShadowBudget: scene.Recipe.ShadowBudget,
		MaxParallel: scene.Recipe.MaxParallelTiles,
	})
	rep.ThreePath = tp
	if tpErr != nil {
		if f, ok := FaultAs(tpErr); ok {
			rep.ThreePathFault = f
		} else {
			rep.ThreePathFault = &Fault{Code: EDiverge, Path: "three_path", Message: tpErr.Error()}
		}
	}
	if preview != nil && !preview.Decision.LatencyMet {
		rep.Notes = append(rep.Notes, fmt.Sprintf("preview latency %.3fms exceeded %dms on this host", preview.Decision.ElapsedMS, preview.Decision.LatencyLimitMS))
	}
	return rep, nil
}

func renderForDiagnostics(scene *Scene, store *CheckpointStore, opts RenderOptions, _ *DiagnosticsReport) *Frame {
	frame, err := NewEngine(scene, store).Render(context.Background(), opts)
	if err != nil {
		if frame != nil {
			return frame
		}
	}
	return frame
}

func chooseInvalidationCandidate(g *DependencyGraph, totalTiles int) (*DepNode, *InvalidationReport) {
	if g == nil {
		return nil, nil
	}
	nodes := g.Nodes()
	var fallback *DepNode
	for i := range nodes {
		n := nodes[i]
		if n.Kind != "density" {
			continue
		}
		rep := g.Invalidate(n.ID)
		if len(rep.Tiles) > 0 && len(rep.Tiles) < totalTiles {
			node := n
			return &node, rep
		}
		if fallback == nil && len(rep.Tiles) > 0 {
			node := n
			fallback = &node
		}
	}
	if fallback != nil {
		return fallback, g.Invalidate(fallback.ID)
	}
	return nil, nil
}

func sourceVoxel(source string) int {
	for i := len(source) - 1; i >= 0; i-- {
		if source[i] == ':' {
			n := 0
			for j := i + 1; j < len(source); j++ {
				if source[j] < '0' || source[j] > '9' {
					return -1
				}
				n = n*10 + int(source[j]-'0')
			}
			return n
		}
	}
	return -1
}
