package fog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

type ThreePathReport struct {
	OK                   bool    `json:"ok"`
	Blocks               int     `json:"blocks"`
	Pixels               int     `json:"pixels"`
	ExpectedInvalidation []int   `json:"expected_invalidation"`
	IncrementalComputed  []int   `json:"incremental_computed"`
	IncrementalCacheHits []int   `json:"incremental_cache_hits"`
	ResumeRecovered      []int   `json:"resume_recovered"`
	FullSequence         []int   `json:"full_sequence"`
	IncrementalSequence  []int   `json:"incremental_sequence"`
	ResumeSequence       []int   `json:"resume_sequence"`
	FirstBlockMismatch   *Error  `json:"first_block_mismatch,omitempty"`
	FirstPixelMismatch   *Error  `json:"first_pixel_mismatch,omitempty"`
	ElapsedMS            float64 `json:"elapsed_ms"`
}

// VerifyThreePaths runs one-shot full recompute, precise incremental rendering,
// and crash/recovery rendering against separate SQLite files in dir.
func VerifyThreePaths(ctx context.Context, dir string) (*ThreePathReport, error) {
	start := time.Now()
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, &Error{Code: EStore, Field: "verify.dir", Message: err.Error()}
	}
	s, err := Generate(DefaultSeed)
	if err != nil {
		return nil, err
	}
	vp := NewViewport(s.Width, s.Height, s.Recipe.TileSize)
	report := &ThreePathReport{Blocks: len(vp.Tiles), Pixels: s.Width * s.Height}
	report.ExpectedInvalidation = append([]int(nil), s.Truth.DensityInvalidation.ExpectedBlocks...)

	graph, err := BuildDependencyGraph(s, "c0")
	if err != nil {
		return nil, err
	}
	manifest := graph.InvalidateVoxel(s.DeclaredEdit)
	if !reflect.DeepEqual(manifest.Blocks, report.ExpectedInvalidation) {
		return report, &Error{Code: EDiverge, Field: "invalidation", Message: fmt.Sprintf("graph %v != truth %v", manifest.Blocks, report.ExpectedInvalidation)}
	}

	incStore, err := OpenCheckpointStore(filepath.Join(dir, "verify-incremental.db"))
	if err != nil {
		return report, err
	}
	defer incStore.Close()
	if _, err := (&Pipeline{Scenario: s, Store: incStore, CameraID: "c0"}).RunCheckpointed(ctx, nil); err != nil {
		return report, err
	}
	resumePath := filepath.Join(dir, "verify-resume.db")
	resumeStore, err := OpenCheckpointStore(resumePath)
	if err != nil {
		return report, err
	}
	if _, err := (&Pipeline{Scenario: s, Store: resumeStore, CameraID: "c0"}).RunCheckpointed(ctx, nil); err != nil {
		_ = resumeStore.Close()
		return report, err
	}

	if _, _, err := ApplyDensityEdit(s, s.DeclaredEdit); err != nil {
		_ = resumeStore.Close()
		return report, err
	}

	fullStore, err := OpenCheckpointStore(filepath.Join(dir, "verify-full.db"))
	if err != nil {
		_ = resumeStore.Close()
		return report, err
	}
	defer fullStore.Close()
	fullRes, err := (&Pipeline{Scenario: s, Store: fullStore, CameraID: "c0"}).RunCheckpointed(ctx, &manifest)
	if err != nil {
		_ = resumeStore.Close()
		return report, err
	}
	incRes, err := (&Pipeline{Scenario: s, Store: incStore, CameraID: "c0"}).RunCheckpointed(ctx, &manifest)
	if err != nil {
		_ = resumeStore.Close()
		return report, err
	}
	report.IncrementalComputed = append([]int(nil), incRes.Computed...)
	report.IncrementalCacheHits = append([]int(nil), incRes.CacheHits...)

	crashPipeline := &Pipeline{Scenario: s, Store: resumeStore, CameraID: "c0", CrashAfterCommits: 7}
	crashRes, crashErr := crashPipeline.RunCheckpointed(ctx, &manifest)
	if crashErr != nil && crashErr != ErrSimulatedCrash {
		_ = resumeStore.Close()
		return report, crashErr
	}
	if crashRes == nil {
		_ = resumeStore.Close()
		return report, fmt.Errorf("crash pipeline returned no result")
	}
	_ = resumeStore.Close()
	reopened, err := OpenCheckpointStore(resumePath)
	if err != nil {
		return report, err
	}
	defer reopened.Close()
	resumeRes, err := (&Pipeline{Scenario: s, Store: reopened, CameraID: "c0"}).RunCheckpointed(ctx, &manifest)
	if err != nil {
		return report, err
	}
	report.ResumeRecovered = append([]int(nil), resumeRes.Recovered...)

	report.FullSequence = append([]int(nil), fullRes.CommitSequence...)
	report.IncrementalSequence = append([]int(nil), incRes.CommitSequence...)
	report.ResumeSequence = append([]int(nil), resumeRes.CommitSequence...)
	wantSeq := canonicalIDs(len(vp.Tiles))
	for name, seq := range map[string][]int{"full": report.FullSequence, "incremental": report.IncrementalSequence, "resume": report.ResumeSequence} {
		if !reflect.DeepEqual(seq, wantSeq) {
			return report, &Error{Code: EDiverge, Field: "commit_sequence", Message: fmt.Sprintf("%s sequence %v", name, seq)}
		}
	}
	for name, other := range map[string]*PipelineResult{"incremental": incRes, "resume": resumeRes} {
		if blockErr := CompareBlockResults(fullRes.Frame, other.Frame); blockErr != nil {
			report.FirstBlockMismatch = errorAsFog(blockErr)
			return report, blockErr
		}
		if pixelErr := CompareFrames(fullRes.Frame, other.Frame, 0, 0); pixelErr != nil {
			report.FirstPixelMismatch = errorAsFog(pixelErr)
			return report, pixelErr
		}
		if !reflect.DeepEqual(other.Manifest.Blocks, report.ExpectedInvalidation) {
			return report, &Error{Code: EDiverge, Field: name + ".invalidation", Message: "invalidation list differs"}
		}
	}
	if !reflect.DeepEqual(report.IncrementalComputed, report.ExpectedInvalidation) {
		return report, &Error{Code: EDiverge, Field: "incremental.computed", Message: fmt.Sprintf("%v != %v", report.IncrementalComputed, report.ExpectedInvalidation)}
	}
	report.OK = true
	report.ElapsedMS = float64(time.Since(start).Microseconds()) / 1000
	return report, nil
}

func canonicalIDs(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func errorAsFog(err error) *Error {
	var fe *Error
	if asError(err, &fe) {
		return fe
	}
	return &Error{Code: EDiverge, Message: err.Error()}
}

// CompareBlockResults locates the first differing tile and scalar field.
func CompareBlockResults(a, b *Frame) error {
	if len(a.Blocks) != len(b.Blocks) {
		return &Error{Code: EDiverge, Field: "blocks", Message: "block count differs"}
	}
	for blockID := 0; blockID < len(a.Blocks); blockID++ {
		ba := a.Blocks[blockID]
		bb := b.Blocks[blockID]
		if ba == nil || bb == nil {
			if ba != bb {
				return &Error{Code: EDiverge, BlockID: blockIDPtr(blockID), Field: "block.presence", Message: "one path is missing block"}
			}
			continue
		}
		if ba.Status == "failed" || bb.Status == "failed" {
			if ba.Status != bb.Status {
				return &Error{Code: EDiverge, BlockID: blockIDPtr(blockID), Field: "block.status", Message: "block status differs"}
			}
			continue
		}
		if len(ba.Transmittance) != len(bb.Transmittance) {
			return &Error{Code: EDiverge, BlockID: blockIDPtr(blockID), Field: "block.transmittance", Message: "length differs"}
		}
		for i := range ba.Transmittance {
			if ba.Transmittance[i] != bb.Transmittance[i] {
				x := ba.X0 + i%ba.Width
				y := ba.Y0 + i/ba.Width
				return &Error{
					Code: EDiverge, BlockID: blockIDPtr(blockID), X: x, Y: y,
					Field:  fmt.Sprintf("block[%d].transmittance[%d]", blockID, i),
					Actual: ba.Transmittance[i], Expected: bb.Transmittance[i],
					Message: "first block field mismatch: transmittance",
				}
			}
		}
		for i := range ba.Radiance {
			if ba.Radiance[i] != bb.Radiance[i] {
				x := ba.X0 + i%ba.Width
				y := ba.Y0 + i/ba.Width
				return &Error{
					Code: EDiverge, BlockID: blockIDPtr(blockID), X: x, Y: y,
					Field:  fmt.Sprintf("block[%d].radiance[%d]", blockID, i),
					Actual: ba.Radiance[i], Expected: bb.Radiance[i],
					Message: "first block field mismatch: radiance",
				}
			}
		}
	}
	return nil
}

type TruthVerificationReport struct {
	OK               bool        `json:"ok"`
	PixelTruths      int         `json:"pixel_truths"`
	DensityTruths    int         `json:"density_truths"`
	MaxTransError    float64     `json:"max_trans_error"`
	MaxRadianceError float64     `json:"max_radiance_error"`
	MaxDensityError  float64     `json:"max_density_error"`
	FailedPixel      *PixelTruth `json:"failed_pixel,omitempty"`
	MonotonicBudgets []int       `json:"monotonic_budgets"`
}

func VerifyTruth(s *Scenario) (*TruthVerificationReport, error) {
	rep := &TruthVerificationReport{PixelTruths: len(s.Truth.Pixels), DensityTruths: len(s.Truth.DensityIntegrals), MonotonicBudgets: []int{1, 2, 4, 8, 16, 32, 64}}
	integ := NewIntegrator(s, ModeFull)
	for i := range s.Truth.Pixels {
		pt := s.Truth.Pixels[i]
		cam, ok := s.Camera(pt.CameraID)
		if !ok {
			return rep, &Error{Code: ESchema, Field: "truth.camera", Message: "missing camera"}
		}
		ray := cam.RayForPixel(pt.PixelX, pt.PixelY, s.Width, s.Height, pt.BlockID)
		tr := TraceRay(ray, s.Media, s.FixedScale)
		got, err := integ.Integrate(tr)
		if err != nil {
			return rep, err
		}
		dT := absFloat(got.Transmittance - pt.ExpectedTransmittance)
		dL := absFloat(got.Radiance - pt.ExpectedRadiance)
		rep.MaxTransError = maxFloat(rep.MaxTransError, dT)
		rep.MaxRadianceError = maxFloat(rep.MaxRadianceError, dL)
		if dT > pt.ToleranceT || dL > pt.ToleranceL {
			failed := pt
			rep.FailedPixel = &failed
			return rep, &Error{Code: EDiverge, X: pt.PixelX, Y: pt.PixelY, Actual: dT, Tolerance: pt.ToleranceT, Message: "truth tolerance exceeded"}
		}

		var lastT, lastL float64 = 1e100, 1e100
		for _, steps := range rep.MonotonicBudgets {
			stepInteg := NewIntegrator(s, ModeFull)
			stepInteg.Steps = steps
			stepGot, err := stepInteg.Integrate(tr)
			if err != nil {
				return rep, err
			}
			eT := absFloat(stepGot.Transmittance - pt.ExpectedTransmittance)
			eL := absFloat(stepGot.Radiance - pt.ExpectedRadiance)
			if eT > lastT+1e-12 || eL > lastL+1e-12 {
				return rep, &Error{Code: EDiverge, X: pt.PixelX, Y: pt.PixelY, Field: "step_convergence", Message: fmt.Sprintf("non-monotonic at steps=%d", steps)}
			}
			lastT, lastL = eT, eL
		}
	}

	for _, dt := range s.Truth.DensityIntegrals {
		m, ok := s.Medium(dt.MediumID)
		if !ok {
			return rep, &Error{Code: ESchema, MediaID: dt.MediumID, Field: "truth.density", Message: "missing medium"}
		}
		cam := s.ActiveCamera()
		ray := cam.RayForPixel(dt.PixelX, dt.PixelY, s.Width, s.Height, dt.BlockID)
		value, _ := IntegrateDensity(m, ray, FromFixed(dt.TMinQ, s.FixedScale), FromFixed(dt.TMaxQ, s.FixedScale))
		errD := absFloat(value - dt.ExpectedValue)
		rep.MaxDensityError = maxFloat(rep.MaxDensityError, errD)
		if errD > 1e-8 {
			return rep, &Error{Code: EDiverge, MediaID: dt.MediumID, RayID: dt.RayID, Field: "truth.density_integral", Actual: value, Expected: dt.ExpectedValue}
		}
	}
	rep.OK = true
	return rep, nil
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
