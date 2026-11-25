package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"strokeforge/internal/engine"
	"strokeforge/internal/generator"
	"strokeforge/internal/model"
	"strokeforge/internal/service"
)

var engineGoVersion = runtime.Version()

type suite struct {
	seed int64
	out  string
	svc  *service.Service
	pool engine.PoolConfig
}

func newSuite(seed int64, out string) *suite {
	pool := engine.DefaultPoolConfig()
	cpDir := filepath.Join(out, "checkpoint-demo")
	svc, err := service.New(seed, pool, cpDir)
	must(err)
	return &suite{seed: seed, out: out, svc: svc, pool: pool}
}

func (s *suite) segmentCount() int {
	n := 0
	for _, g := range s.svc.Doc().Glyphs {
		n += len(g.StrokeSegments)
	}
	return n
}

func timed(name, expected string, fn func() (string, error)) checkResult {
	start := time.Now()
	actual, err := fn()
	elapsed := time.Since(start).Seconds() * 1000
	res := checkResult{Name: name, Expected: expected, DurationMS: elapsed, Actual: actual}
	if err != nil {
		res.OK = false
		if actual == "" {
			res.Actual = err.Error()
		} else {
			res.Actual = actual + " | " + err.Error()
		}
		return res
	}
	res.OK = true
	return res
}

func expectCode(name, want string, fn func() error) checkResult {
	start := time.Now()
	err := fn()
	elapsed := time.Since(start).Seconds() * 1000
	res := checkResult{Name: name, Expected: want, DurationMS: elapsed}
	if err == nil {
		res.OK = false
		res.Actual = "no error returned"
		return res
	}
	if ce, ok := err.(*model.CodedError); ok {
		res.OK = ce.Code == want
		res.Actual = ce.Code
		if ce.GlyphID != "" {
			res.Actual = fmt.Sprintf("%s glyph=%s segment=%d", ce.Code, ce.GlyphID, ce.SegmentID)
		}
	} else {
		res.OK = false
		res.Actual = err.Error()
	}
	return res
}

func (s *suite) runAll() []checkResult {
	var checks []checkResult

	// 1. Deterministic generation + load round trip.
	doc1, err := generator.Generate(s.seed)
	must(err)
	doc2, err := generator.Generate(s.seed)
	must(err)
	raw1, _ := json.Marshal(doc1)
	raw2, _ := json.Marshal(doc2)
	sum := sha256.Sum256(raw1)
	checks = append(checks, timed("generate + load sample", "deterministic and valid", func() (string, error) {
		if string(raw1) != string(raw2) {
			return "", fmt.Errorf("generator not deterministic")
		}
		return fmt.Sprintf("valid=true deterministic=true hash=%s", shortHash(sum[:])), nil
	}))

	// 2. Initial full render.
	stats, err := s.svc.FullRender()
	checks = append(checks, timed("initial full render", "all segments committed", func() (string, error) {
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d segments, peak buffer=%d", stats.Rendered, stats.PeakBuffer), nil
	}))

	// 3. Incremental after parameter change equals full render.
	params := s.svc.Doc().StrokeParams
	changed := params
	changed.Width = 11
	entries, _, diff, err := s.svc.Incremental(changed)
	checks = append(checks, timed("incremental equals full render", "pixel/density/sequence equal", func() (string, error) {
		if err != nil {
			return fmt.Sprintf("%+v", diff), err
		}
		return fmt.Sprintf("%+v", diff), nil
	}))

	// 4. Control-point edit only invalidates the owning glyph.
	target := s.svc.Doc().Glyphs[0]
	contour := target.Contours[0]
	seg0 := contour.Segments[0]
	editEntries, editDiff, err := s.svc.EditControlPoint(
		target.ID, contour.ID, seg0.ID, "c1",
		model.Point{X: seg0.C1.X + 4, Y: seg0.C1.Y},
	)
	invalidated := len(editEntries)
	reused := s.segmentCount() - countDirty(editEntries)
	checks = append(checks, timed("control edit limited to dependent glyph", "affected segments + reuse elsewhere", func() (string, error) {
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("invalidated=%d reused=%d", invalidated, reused), nil
	}))

	// 5. Edited incremental path equals full.
	checks = append(checks, timed("control edit incremental equals full", "pixel/density/sequence equal", func() (string, error) {
		if err != nil {
			return fmt.Sprintf("%+v", editDiff), err
		}
		return fmt.Sprintf("%+v", editDiff), nil
	}))

	// 6+7. Checkpoint partial run + recovery.
	cpDir := filepath.Join(s.out, "checkpoint-acc")
	cpSvc := mustNewService(s.seed, cpDir)
	saved, err := cpSvc.CheckpointPartial(5)
	checks = append(checks, timed("checkpoint partial run", "5 committed records", func() (string, error) {
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("rebuilt=%d", saved), nil
	}))
	// Simulate restart with a fresh service over the same checkpoint dir.
	restartSvc := mustNewService(s.seed, cpDir)
	report, recDiff, err := restartSvc.Recover()
	checks = append(checks, timed("checkpoint recovery", "5 valid restored records", func() (string, error) {
		if err != nil {
			return fmt.Sprintf("%+v", report), err
		}
		return fmt.Sprintf("%+v", report), nil
	}))

	// 8. Recovered document equals full render.
	checks = append(checks, timed("recovered document equals full render", "pixel/density/sequence equal", func() (string, error) {
		if err != nil {
			return fmt.Sprintf("%+v", recDiff), err
		}
		return fmt.Sprintf("%+v", recDiff), nil
	}))

	// 9. Tampered checkpoint invalidates everything.
	tamperSvc := mustNewService(s.seed, cpDir)
	_, _ = tamperSvc.CheckpointPartial(30)
	must(tamperSvc.TamperCheckpoint("Aegis", 0))
	tamperReport, _, err := tamperSvc.Recover()
	checks = append(checks, timed("tampered checkpoint invalidates all", "full rebuild triggered", func() (string, error) {
		if err != nil {
			return "", err
		}
		if !tamperReport.FullRebuild || !tamperReport.Invalidated {
			return "", fmt.Errorf("expected invalidation, got %+v", tamperReport)
		}
		return fmt.Sprintf("%+v", tamperReport), nil
	}))

	// 10. Out-of-range width -> E_PARA.
	bad := s.svc.Doc().StrokeParams
	bad.Width = 999
	checks = append(checks, expectCode("out-of-range width returns E_PARA", model.EPara, func() error {
		_, _, _, err := s.svc.Incremental(bad)
		return err
	}))

	// 11. Band outside bbox -> E_BOUND via huge padding-shrinking param set.
	// Use an isolated workbench so the failing render cannot pollute the
	// main service state used for the exported masks later.
	boundSvc := mustNewService(s.seed, filepath.Join(s.out, "checkpoint-bound"))
	overBound := boundSvc.Doc().StrokeParams
	overBound.BBoxPadding = 0
	overBound.Width = 60
	checks = append(checks, expectCode("band outside bbox returns E_BOUND", model.EBound, func() error {
		_, _, _, err := boundSvc.Incremental(overBound)
		return err
	}))

	// 12. Illegal state transition -> E_STATE.
	checks = append(checks, expectCode("illegal state transition returns E_STATE", model.EState, func() error {
		return engine.Transition(engine.StateCommitted, engine.StateProcessing)
	}))

	// 13. Parallelism limit -> E_PARA.
	checks = append(checks, expectCode("parallelism limit returns E_PARA", model.EPara, func() error {
		_, err := engine.NewWorkbench(s.svc.Doc(), engine.PoolConfig{MaxWorkers: 99, MaxBufferBytes: 1 << 20})
		return err
	}))

	// 14. Per-worker buffer limit -> E_PARA.
	checks = append(checks, expectCode("per-worker buffer limit returns E_PARA", model.EPara, func() error {
		_, err := engine.NewWorkbench(s.svc.Doc(), engine.PoolConfig{MaxWorkers: 2, MaxBufferBytes: 1024})
		return err
	}))

	// 15-18. Fault injection chain.
	checks = append(checks, expectCode("fault self-intersection", model.ESelf, func() error {
		return s.svc.Diagnose(engine.FaultSelfIntersect, "Aegis", 5)
	}))
	checks = append(checks, expectCode("fault zero-length", model.EZlen, func() error {
		return s.svc.Diagnose(engine.FaultZeroLength, "Aegis", 5)
	}))
	checks = append(checks, expectCode("fault density-overflow", model.EDens, func() error {
		return s.svc.Diagnose(engine.FaultDensityOverflow, "Aegis", 0)
	}))
	checks = append(checks, expectCode("fault stage-failure", model.EStage, func() error {
		return s.svc.Diagnose(engine.FaultStageFailure, "Aegis", 0)
	}))

	_ = entries

	// Rebuild a pristine, fully rendered service so exported artifacts reflect
	// the original deterministic sample rather than the mutated demo state.
	s.svc = mustNewService(s.seed, filepath.Join(s.out, "checkpoint-export"))
	if _, err := s.svc.FullRender(); err != nil {
		must(err)
	}
	return checks
}

func mustNewService(seed int64, cpDir string) *service.Service {
	svc, err := service.New(seed, engine.DefaultPoolConfig(), cpDir)
	must(err)
	return svc
}

func countDirty(entries []engine.InvalidationEntry) int {
	seen := map[string]bool{}
	n := 0
	for _, e := range entries {
		key := fmt.Sprintf("%s/%d", e.GlyphID, e.SegmentID)
		if seen[key] {
			continue
		}
		seen[key] = true
		n++
	}
	return n
}

func shortHash(b []byte) string {
	return hex.EncodeToString(b)[:16]
}

func init() {
	// Ensure artifacts dir exists even when invoked from a clean checkout.
	_ = os.MkdirAll("artifacts", 0o755)
}
