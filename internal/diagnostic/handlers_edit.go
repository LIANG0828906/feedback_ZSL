package diagnostic

import (
	"net/http"

	"ballistic/internal/kernel"
	"ballistic/internal/workbench"
)

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := r.Context()
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "endpoint"
	}

	var plan *workbench.InvalidationPlan
	var err error
	switch kind {
	case "restitution":
		next := int64(6400)
		for _, seg := range s.wb.Scene.Segments {
			if seg.ID == "floor_main" {
				if seg.Restitution == 6400 {
					next = 8200
				}
				break
			}
		}
		var p *workbench.InvalidationPlan
		p, err = s.wb.EditRestitution("floor_main", next)
		plan = p
	default:
		newA := kernel.Vec2{X: 38, Y: 318}
		var p *workbench.InvalidationPlan
		p, err = s.wb.EditEndpointWorld("remote_plate", &newA, nil)
		plan = p
	}
	if err != nil {
		s.writeKernelError(w, err)
		return
	}
	if err := s.wb.RunIncremental(ctx, stringSet(plan.InvalidTiles)); err != nil {
		s.writeKernelError(w, err)
		return
	}
	full, err := s.wb.FreshFullRun(ctx)
	if err != nil {
		s.writeKernelError(w, err)
		return
	}
	comparison := workbench.CompareRuns(full, s.wb)
	_ = full.Store.Close()
	s.lastCompare = &comparison
	s.writeJSON(w, http.StatusOK, s.stateResponseLocked(ctx))
}

func (s *Server) writeKernelError(w http.ResponseWriter, err error) {
	if kernelErr, ok := err.(*kernel.Error); ok {
		s.writeJSON(w, http.StatusUnprocessableEntity, kernelErr.Issue)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func stringSet(items []string) map[string]bool {
	result := map[string]bool{}
	for _, item := range items {
		result[item] = true
	}
	return result
}
