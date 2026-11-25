package web

import (
	"net/http"

	"edgerefine/internal/diagnostic"
	"edgerefine/internal/errs"
	"edgerefine/internal/model"
	"edgerefine/internal/runner"
	"edgerefine/internal/sample"
	"edgerefine/internal/verify"
)

type modelThresholdSnapshot = model.ThresholdSnapshot

func sampleFingerprint(b *sample.Bundle) string {
	return sample.Fingerprint(b.Manifest)
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sample")
	b, err := s.bundle(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p := b.Manifest.Params
	if v := r.URL.Query().Get("radius"); v != "" {
		p.Radius = atoiDefault(v, p.Radius)
	}
	manual := manualThreshold(r)
	pr, err := runner.RunPreview(b, p, manual, 4, nil, r.Context())
	if err != nil && pr == nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.mu.Lock()
	s.previews[id] = pr
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"preview": pr.Report.Preview, "accuracy": pr.Report.Accuracy,
		"converged_tile_ratio": pr.Report.ConvergedTileRatio,
		"mask_hash":            pr.Report.MaskHash,
	})
}

func (s *Server) handleFinalize(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sample")
	b, err := s.bundle(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p := b.Manifest.Params
	if v := r.URL.Query().Get("radius"); v != "" {
		p.Radius = atoiDefault(v, p.Radius)
	}
	manual := manualThreshold(r)
	fp := sampleFingerprint(b)

	s.mu.Lock()
	prev := s.previews[id]
	s.mu.Unlock()
	var snaps map[[2]int]modelThresholdSnapshot
	if prev != nil {
		snaps = prev.Snapshots
	} else {
		built, err := runner.BuildSnapshots(b, p, manual)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		snaps = built
	}
	rep, err := runner.Run(b, runner.Options{
		Workers: 4, Params: p, Store: s.db, TruthFingerprint: fp, Persist: true,
		Resume: true, StrictAccuracy: false, Snapshots: snaps, ManualThreshold: manual,
	})
	if rep != nil && prev != nil && prev.Report != nil {
		div, derr := runner.ComparePreviewExact(prev.Report, rep, p)
		rep.Divergence = div
		if derr != nil {
			err = derr
		}
	}
	s.mu.Lock()
	s.reports[id] = rep
	s.mu.Unlock()
	code := http.StatusOK
	if err != nil {
		if ee, ok := errs.As(err); ok && ee.Code == errs.EDiverge {
			code = http.StatusUnprocessableEntity
		}
	}
	writeJSON(w, code, runSummary(id, rep, err))
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sample")
	b, err := s.bundle(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rep, err := verify.Verify(s.db, b.Manifest, sampleFingerprint(b))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	code := http.StatusOK
	if !rep.OK {
		code = http.StatusConflict
	}
	writeJSON(w, code, rep)
}

func (s *Server) handleThreePath(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sample")
	res, err := diagnostic.RunThreePaths(s.samplesDir, id, "checkpoints")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRadiusSweep(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sample")
	b, err := s.bundle(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	points, monotonic, err := runner.RadiusSweep(b, b.Manifest.Params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"points": points, "monotonic_non_increasing_signed_bias": monotonic,
		"direction": "larger radius moves signed normal bias in one measurable direction",
	})
}
