package gateway

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"edgegateway/internal/model"
	"edgegateway/internal/store"
)

var errSessionCompleted = errors.New("session is completed")

type sessionRequest struct {
	DeviceID     string `json:"device_id"`
	Ingress      string `json:"ingress,omitempty"`
	ConnectionID string `json:"connection_id,omitempty"`
	ResumeToken  string `json:"resume_token,omitempty"`
}

func (g *Gateway) handleSessionCreate(w http.ResponseWriter, r *http.Request, ingress string) {
	var req sessionRequest
	if err := decodeJSONBody(w, r, &req, g.cfg.MaxBodyBytes); err != nil {
		g.writeError(w, http.StatusBadRequest, model.CodeBadRequest, "invalid JSON: "+err.Error(), nil)
		return
	}
	req.DeviceID = strings.TrimSpace(req.DeviceID)
	if req.DeviceID == "" || len(req.DeviceID) > 128 {
		g.writeError(w, http.StatusBadRequest, model.CodeBadRequest, "device_id is required (max 128 bytes)", nil)
		return
	}
	if req.Ingress != "" && req.Ingress != ingress {
		g.writeError(w, http.StatusConflict, model.CodeSessionIngressMismatch,
			"request ingress does not match listener", map[string]any{"listener_ingress": ingress})
		return
	}
	if len(req.ConnectionID) > 256 {
		g.writeError(w, http.StatusBadRequest, model.CodeBadRequest, "connection_id is too long", nil)
		return
	}
	addr, host := g.effectiveAddress(r)
	var (
		sess     *model.Session
		reused   bool
		mig      *model.Migration
		migrated bool
		err      error
	)
	if req.ResumeToken != "" {
		sess, reused, mig, migrated, err = g.attachByToken(r, req, ingress, addr, host, "session_establish")
	} else {
		sess, reused, err = g.store.CreateOrGetSession(r.Context(), req.DeviceID, ingress, req.ConnectionID, addr, host, g.cfg.MaxSessions)
		if err == nil && reused {
			rt := g.runtime(sess)
			var updated *model.Session
			updated, mig, migrated, err = rt.observeAddress(r.Context(), g.store, addr, host, true, g.cfg.MigrateWindow(), -1, "session_establish")
			if updated != nil {
				sess = updated
			}
		}
	}
	if errors.Is(err, store.ErrSessionLimit) {
		g.writeError(w, http.StatusServiceUnavailable, model.CodeSessionLimit,
			"active session limit reached", map[string]any{"max_sessions": g.cfg.MaxSessions})
		return
	}
	if err != nil {
		if errors.Is(err, errMissingSession) {
			g.writeError(w, http.StatusUnauthorized, model.CodeResumeTokenInvalid, "resume token is invalid for this ingress", nil)
			return
		}
		if errors.Is(err, errSessionCompleted) {
			g.writeError(w, http.StatusGone, model.CodeSessionCompleted, "session is completed", nil)
			return
		}
		g.sessionStoreError(w, err)
		return
	}
	body := map[string]any{
		"ok": true, "session": sess, "session_id": sess.ID, "resume_token": g.resumeToken(sess.ID),
		"reused": reused, "migrated": migrated, "migration": mig,
	}
	g.writeJSON(w, http.StatusOK, body)
}

func (g *Gateway) handleSessionAttach(w http.ResponseWriter, r *http.Request, ingress string) {
	var req sessionRequest
	if err := decodeJSONBody(w, r, &req, g.cfg.MaxBodyBytes); err != nil {
		g.writeError(w, http.StatusBadRequest, model.CodeBadRequest, "invalid JSON: "+err.Error(), nil)
		return
	}
	req.DeviceID = strings.TrimSpace(req.DeviceID)
	if req.ResumeToken == "" {
		g.writeError(w, http.StatusBadRequest, model.CodeBadRequest, "resume_token is required", nil)
		return
	}
	addr, host := g.effectiveAddress(r)
	sess, _, mig, migrated, err := g.attachByToken(r, req, ingress, addr, host, "session_attach")
	if err != nil {
		if errors.Is(err, errMissingSession) {
			g.writeError(w, http.StatusUnauthorized, model.CodeResumeTokenInvalid, err.Error(), nil)
			return
		}
		if errors.Is(err, errSessionCompleted) {
			g.writeError(w, http.StatusGone, model.CodeSessionCompleted, "session is completed", nil)
			return
		}
		g.sessionStoreError(w, err)
		return
	}
	g.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "session": sess, "session_id": sess.ID, "resume_token": g.resumeToken(sess.ID),
		"reused": true, "migrated": migrated, "migration": mig,
	})
}

func (g *Gateway) attachByToken(r *http.Request, req sessionRequest, ingress, addr, host, trigger string) (*model.Session, bool, *model.Migration, bool, error) {
	id, err := g.verifyResumeToken(req.ResumeToken)
	if err != nil {
		return nil, false, nil, false, errMissingSession
	}
	sess, err := g.store.GetSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil, false, errMissingSession
		}
		return nil, false, nil, false, err
	}
	if req.DeviceID != "" && sess.DeviceID != req.DeviceID {
		return nil, false, nil, false, errMissingSession
	}
	if sess.Ingress != ingress {
		return nil, false, nil, false, errMissingSession
	}
	if !sess.Active || sess.Completed {
		return nil, false, nil, false, errSessionCompleted
	}
	rt := g.runtime(sess)
	updated, mig, migrated, err := rt.observeAddress(r.Context(), g.store, addr, host, true, g.cfg.MigrateWindow(), -1, trigger)
	if err != nil {
		return nil, false, nil, false, err
	}
	if updated != nil {
		sess = updated
	}
	return sess, true, mig, migrated, nil
}
