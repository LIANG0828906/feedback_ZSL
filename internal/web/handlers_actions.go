package web

import (
	"net/http"

	"edgerefine/internal/errs"
	"edgerefine/internal/model"
	"edgerefine/internal/runner"
)

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sample")
	b, err := s.bundle(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	tx := atoiDefault(r.URL.Query().Get("tx"), 0)
	ty := atoiDefault(r.URL.Query().Get("ty"), 0)
	p := b.Manifest.Params
	if v := r.URL.Query().Get("radius"); v != "" {
		p.Radius = atoiDefault(v, p.Radius)
	}
	if v := r.URL.Query().Get("budget"); v != "" {
		p.IterBudget = atoiDefault(v, p.IterBudget)
	}
	tr, err := runner.RetryTileRefinement(b, s.db, model.TileCoord{TX: tx, TY: ty}, p, sampleFingerprint(b))
	if err != nil {
		code := http.StatusInternalServerError
		if ee, ok := errs.As(err); ok {
			switch ee.Code {
			case errs.EState:
				code = http.StatusConflict
			case errs.EIter:
				code = http.StatusUnprocessableEntity
			case errs.EPara:
				code = http.StatusBadRequest
			}
		}
		writeJSON(w, code, map[string]any{"tile_result": tr, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, tr)
}
