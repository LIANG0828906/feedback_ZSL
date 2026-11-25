// Package service exposes the workbench use-cases used by the CLI, the
// browser workbench and the Wails desktop binding.
package service

import (
	"encoding/json"
	"os"
	"path/filepath"

	"strokeforge/internal/checkpoint"
	"strokeforge/internal/checkpoint/pb"
	"strokeforge/internal/engine"
	"strokeforge/internal/generator"
	"strokeforge/internal/loader"
	"strokeforge/internal/model"
	"strokeforge/internal/raster"
)

// Service is the application facade.
type Service struct {
	doc       *model.Document
	workbench *engine.Workbench
	cpDir     string
}

// New generates the deterministic sample and prepares the engine.
func New(seed int64, pool engine.PoolConfig, cpDir string) (*Service, error) {
	doc, err := generator.Generate(seed)
	if err != nil {
		return nil, err
	}
	wb, err := engine.NewWorkbench(doc, pool)
	if err != nil {
		return nil, err
	}
	return &Service{doc: doc, workbench: wb, cpDir: cpDir}, nil
}

// FromDocument builds a service from an already parsed document.
func FromDocument(doc *model.Document, pool engine.PoolConfig, cpDir string) (*Service, error) {
	if err := loader.Validate(doc); err != nil {
		return nil, err
	}
	wb, err := engine.NewWorkbench(doc, pool)
	if err != nil {
		return nil, err
	}
	return &Service{doc: doc, workbench: wb, cpDir: cpDir}, nil
}

// Doc returns the underlying document.
func (s *Service) Doc() *model.Document { return s.doc }

// Workbench exposes the engine for lower-level tests.
func (s *Service) Workbench() *engine.Workbench { return s.workbench }

// WriteSample writes the deterministic sample JSON to outDir/sample.json.
func (s *Service) WriteSample(outDir string) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(outDir, "sample.json")
	raw, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, raw, 0o644)
}

// FullRender runs the from-scratch pipeline.
func (s *Service) FullRender() (*engine.RenderStats, error) {
	return s.workbench.FullRender()
}

// Incremental applies parameter edits and renders only dirty segments.
func (s *Service) Incremental(next model.StrokeParams) ([]engine.InvalidationEntry, *engine.RenderStats, *engine.DiffResult, error) {
	entries, _, err := s.workbench.UpdateParams(next)
	if err != nil {
		return nil, nil, nil, err
	}
	dirty := engine.DirtySet(entries)
	stats, err := s.workbench.RenderPending(dirty)
	if err != nil {
		return entries, nil, nil, err
	}
	diff, err := s.workbench.Compare(dirty)
	if err != nil {
		return entries, stats, diff, err
	}
	return entries, stats, diff, nil
}

// EditControlPoint edits one point and re-renders the affected glyph only.
func (s *Service) EditControlPoint(glyphID, contourID, segmentID, field string, value model.Point) ([]engine.InvalidationEntry, *engine.DiffResult, error) {
	entries, err := s.workbench.EditControlPoint(glyphID, contourID, segmentID, field, value)
	if err != nil {
		return nil, nil, err
	}
	dirty := engine.DirtySet(entries)
	if _, err := s.workbench.RenderPending(dirty); err != nil {
		return entries, nil, err
	}
	diff, err := s.workbench.Compare(dirty)
	return entries, diff, err
}

// CheckpointPartial renders up to maxSegments commits and persists each one.
func (s *Service) CheckpointPartial(maxSegments int) (int, error) {
	store, err := checkpoint.Open(s.cpDir)
	if err != nil {
		return 0, err
	}
	if err := store.Reset(); err != nil {
		return 0, err
	}
	if _, err := s.workbench.FullRender(); err != nil {
		return 0, err
	}
	manifest := s.buildManifest()
	if err := store.WriteManifest(manifest); err != nil {
		return 0, err
	}
	saved := 0
	for _, g := range s.doc.Glyphs {
		for _, seg := range g.StrokeSegments {
			rec := s.workbench.Record(g.ID, seg.ID)
			if rec == nil {
				return saved, model.NewError(model.EStage, "missing committed record")
			}
			if err := store.SaveRecord(rec.Snapshot()); err != nil {
				return saved, err
			}
			saved++
			if saved >= maxSegments {
				return saved, nil
			}
		}
	}
	return saved, nil
}

// Recover loads checkpoints, restores valid records and resumes rendering.
func (s *Service) Recover() (*checkpoint.RecoveryReport, *engine.DiffResult, error) {
	store, err := checkpoint.Open(s.cpDir)
	if err != nil {
		return nil, nil, err
	}
	manifest := s.buildManifest()
	frames := s.workbench.Frames()
	records, report, err := store.Load(manifest, frames)
	if err != nil {
		return report, nil, err
	}
	if report.FullRebuild {
		if _, err := s.workbench.FullRender(); err != nil {
			return report, nil, err
		}
		diff, err := s.workbench.Compare(nil)
		return report, diff, err
	}
	if err := s.workbench.RestoreCheckpoint(records); err != nil {
		return report, nil, err
	}
	// Resume everything that was not restored.
	stats, err := s.workbench.RenderPending(nil)
	if err != nil {
		return report, nil, err
	}
	_ = stats
	diff, err := s.workbench.Compare(nil)
	return report, diff, err
}

// TamperCheckpoint corrupts one persisted record for recovery tests.
func (s *Service) TamperCheckpoint(glyph string, seg int) error {
	store, err := checkpoint.Open(s.cpDir)
	if err != nil {
		return err
	}
	return store.Tamper(glyph, seg)
}

// InjectFault forwards to the engine.
func (s *Service) InjectFault(glyph string, seg int, kind string) error {
	return s.workbench.InjectFault(glyph, seg, kind)
}

// ClearFaults removes injected faults.
func (s *Service) ClearFaults() { s.workbench.ClearFaults() }

// MaskPNG exports a glyph mask as grayscale PNG bytes.
func (s *Service) MaskPNG(glyphID string) ([]byte, error) {
	return s.workbench.MaskPNG(glyphID)
}

// Snapshot builds the JSON view consumed by the diagnostics UI.
func (s *Service) Snapshot() (*SnapshotView, error) {
	return BuildSnapshot(s.doc, s.workbench)
}

// Diagnose runs one injected-fault scenario and returns its coded result.
func (s *Service) Diagnose(kind string, glyphID string, segID int) error {
	if glyphID == "" {
		glyphID = s.doc.Glyphs[0].ID
	}
	return s.workbench.DiagnoseSegment(glyphID, segID, kind)
}

// LoadSampleFromDisk re-validates a generated file (used by tests/CLI).
func LoadSampleFromDisk(path string) (*model.Document, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return loader.Parse(raw)
}

var _ = raster.NewFrame

func (s *Service) buildManifest() pb.Manifest {
	hashes := map[string]string{}
	for _, g := range s.doc.Glyphs {
		hashes[g.ID] = s.workbench.GlyphContentHash(g.ID)
	}
	return pb.Manifest{
		Algorithm:    model.AlgorithmVersion,
		GlyphHashes:  hashes,
		ParamVersion: s.workbench.ParamVersion(),
		Seed:         s.doc.Seed,
	}
}
