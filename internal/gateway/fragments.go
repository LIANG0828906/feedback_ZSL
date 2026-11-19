package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"edgegateway/internal/backend"
	"edgegateway/internal/model"
	"edgegateway/internal/store"
)

type fragmentRequest struct {
	Schema         string `json:"schema,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	DeviceID       string `json:"device_id,omitempty"`
	ConnectionID   string `json:"connection_id,omitempty"`
	ResumeToken    string `json:"resume_token,omitempty"`
	Seq            *int64 `json:"seq,omitempty"`
	FragmentSeq    *int64 `json:"fragment_seq,omitempty"`
	Size           int    `json:"size,omitempty"`
	DataBase64     string `json:"data_base64,omitempty"`
	ChecksumSHA256 string `json:"checksum_sha256,omitempty"`
}

type resolvedSession struct {
	sess      *model.Session
	rt        *sessionRuntime
	migrated  bool
	migration *model.Migration
}

func (g *Gateway) handleFragment(w http.ResponseWriter, r *http.Request, ingress string) {
	started := time.Now()
	var req fragmentRequest
	if err := decodeJSONBody(w, r, &req, g.cfg.MaxBodyBytes); err != nil {
		g.writeError(w, http.StatusBadRequest, model.CodeBadRequest, "invalid JSON: "+err.Error(), nil)
		return
	}
	req.SessionID = firstNonEmpty(req.SessionID, r.Header.Get("X-Session-ID"))
	req.DeviceID = firstNonEmpty(req.DeviceID, r.Header.Get("X-Device-ID"))
	req.ConnectionID = firstNonEmpty(req.ConnectionID, r.Header.Get("X-Connection-ID"))
	req.ResumeToken = firstNonEmpty(req.ResumeToken, r.Header.Get("X-Resume-Token"))
	seq := req.Seq
	if seq == nil {
		seq = req.FragmentSeq
	}
	if seq == nil || *seq < 0 {
		g.writeError(w, http.StatusBadRequest, model.CodeBadRequest, "non-negative seq is required", nil)
		return
	}
	data, err := decodeFragmentData(req.DataBase64)
	if err != nil {
		g.writeError(w, http.StatusBadRequest, model.CodeBadRequest, err.Error(), nil)
		return
	}
	if int64(len(data)) > g.cfg.MaxFragmentBytes {
		g.writeError(w, http.StatusRequestEntityTooLarge, model.CodeBadRequest,
			"fragment exceeds max_fragment_bytes", map[string]any{"max_fragment_bytes": g.cfg.MaxFragmentBytes})
		return
	}
	if req.Size > 0 && req.Size != len(data) {
		g.writeError(w, http.StatusBadRequest, model.CodeBadRequest, "size does not match decoded payload", nil)
		return
	}
	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])
	if req.ChecksumSHA256 != "" && !strings.EqualFold(req.ChecksumSHA256, checksum) {
		g.writeError(w, http.StatusBadRequest, model.CodeBadRequest, "checksum_sha256 mismatch", nil)
		return
	}

	resolved, status, code, message := g.resolveFragmentSession(w, r, ingress, &req)
	if resolved == nil {
		g.metrics.Inc("edge_gateway_fragments_total", map[string]string{"ingress": ingress, "result": strings.ToLower(code)}, 1)
		g.writeError(w, status, code, message, nil)
		return
	}
	sess, rt := resolved.sess, resolved.rt
	if model.Contains(sess.AckRanges, *seq) {
		g.respondDuplicateFragment(w, ingress, sess, rt, *seq, resolved, started)
		return
	}

	outcome := g.beginFragment(rt, *seq)
	if outcome.duplicate {
		g.respondDuplicateFragment(w, ingress, sess, rt, *seq, resolved, started)
		return
	}
	if outcome.wait != nil {
		select {
		case <-outcome.wait.done:
			g.writeJSON(w, outcome.wait.result.status, outcome.wait.result.body)
		case <-r.Context().Done():
			return
		}
		return
	}
	if outcome.status != 0 {
		g.metrics.Inc("edge_gateway_fragments_total", map[string]string{"ingress": ingress, "result": strings.ToLower(outcome.code)}, 1)
		extra := g.feedbackPayload(sess, rt.statistics(), nil, false)
		extra["ok"] = false
		extra["code"] = outcome.code
		extra["business_error_code"] = outcome.code
		extra["error"] = outcome.message
		extra["session_id"] = sess.ID
		extra["seq"] = *seq
		extra["rejected_seq"] = nilIfUnblocked(sess)
		extra["reject_reason"] = sess.BlockReason
		extra["accepted_ranges"] = sess.AckRanges
		extra["ack_through"] = sess.AckThrough
		if outcome.retryAfter > 0 {
			extra["retry_after_ms"] = outcome.retryAfter.Milliseconds()
			w.Header().Set("Retry-After", strconv.FormatFloat(outcome.retryAfter.Seconds(), 'f', 3, 64))
		}
		w.Header().Set("X-Edge-Feedback", "limited")
		g.writeJSON(w, outcome.status, extra)
		return
	}

	call := outcome.call
	backendStarted := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), g.cfg.BackendTimeout())
	resp, forwardErr := g.backend.Forward(ctx, backend.ForwardRequest{
		Schema: "edge.fragment.v1", SessionID: sess.ID, DeviceID: sess.DeviceID,
		Ingress: ingress, Seq: *seq, Size: len(data),
		DataBase64: base64.StdEncoding.EncodeToString(data), ChecksumSHA256: checksum,
	})
	cancel()
	rtt := time.Since(backendStarted)
	rt.addRTT(rtt)

	var (
		updatedSess *model.Session
		event       *model.FeedbackEvent
		stats       runtimeStats
		result      fragmentResult
		metricCode  string
	)
	if forwardErr != nil {
		timeout := isTimeout(forwardErr)
		stats = rt.statistics()
		reason := "backend_unavailable"
		status, code := http.StatusBadGateway, model.CodeBackendUnavailable
		if timeout {
			reason, status, code = "backend_timeout", http.StatusGatewayTimeout, model.CodeBackendTimeout
		}
		_ = g.store.RecordTimeout(context.Background(), sess.ID, *seq, int64(len(data)), checksum, reason, store.Stats{
			RTTSamples: stats.RTTSamples, BacklogPeak: stats.BacklogPeak,
			SendWindowPeak: stats.SendWindowPeak,
		})
		event, stats = g.evaluateFeedback(rt, sess, timeout)
		if s, err := g.store.GetSession(context.Background(), sess.ID); err == nil {
			updatedSess = s
		} else {
			updatedSess = sess
		}
		rt.applySession(updatedSess)
		metricCode = code
		body := g.feedbackPayload(updatedSess, stats, event, false)
		body["ok"] = false
		body["code"] = code
		body["business_error_code"] = code
		body["error"] = reason
		body["session_id"] = sess.ID
		body["seq"] = *seq
		body["retryable"] = true
		body["accepted_ranges"] = updatedSess.AckRanges
		body["ack_through"] = updatedSess.AckThrough
		result = fragmentResult{status: status, body: body}
		g.metrics.Inc("edge_gateway_timeouts_total", map[string]string{"ingress": ingress}, boolFloat(timeout))
	} else {
		stats = rt.statistics()
		storeStats := store.Stats{
			RTTSamples: stats.RTTSamples, BacklogPeak: stats.BacklogPeak,
			SendWindowPeak: stats.SendWindowPeak,
		}
		if resp.Rejected {
			reason := resp.Reason
			if reason == "" {
				reason = "backend_rejected_fragment"
			}
			backendCode := resp.Code
			if backendCode == "" {
				backendCode = "FRAGMENT_REJECTED"
			}
			s, err := g.store.RecordRejected(context.Background(), sess.ID, *seq, int64(len(data)), checksum, reason, backendCode, storeStats)
			if err != nil {
				g.logError("record rejection", "session", sess.ID, "seq", *seq, "error", err)
				updatedSess = sess
			} else {
				updatedSess = s
			}
			rt.applySession(updatedSess)
			event, stats = g.evaluateFeedback(rt, updatedSess, false)
			metricCode = model.CodeBackendRejected
			body := g.feedbackPayload(updatedSess, stats, event, false)
			updateMap(body, map[string]any{
				"ok": false, "code": model.CodeBackendRejected,
				"business_error_code": model.CodeBackendRejected,
				"error":               reason, "session_id": sess.ID, "seq": *seq,
				"accepted": false, "duplicate": false,
				"rejected_seq": *seq, "reject_reason": reason,
				"backend_code": backendCode, "retryable": true,
				"accepted_ranges": updatedSess.AckRanges, "ack_through": updatedSess.AckThrough,
				"failure_count": updatedSess.FailureCount, "rejection_count": updatedSess.RejectionCount,
			})
			result = fragmentResult{status: http.StatusUnprocessableEntity, body: body}
		} else {
			s, duplicate, err := g.store.RecordAccepted(context.Background(), sess.ID, *seq, int64(len(data)), checksum, resp.AcceptedRanges, storeStats)
			if err != nil {
				g.logError("record accepted fragment", "session", sess.ID, "seq", *seq, "error", err)
				updatedSess = sess
			} else {
				updatedSess = s
			}
			rt.applySession(updatedSess)
			event, stats = g.evaluateFeedback(rt, updatedSess, false)
			metricCode = "accepted"
			if duplicate || resp.Duplicate {
				metricCode = "duplicate"
			}
			body := g.feedbackPayload(updatedSess, stats, event, false)
			updateMap(body, map[string]any{
				"ok": true, "code": model.CodeOK, "session_id": sess.ID, "seq": *seq,
				"accepted": true, "duplicate": duplicate || resp.Duplicate,
				"accepted_ranges": updatedSess.AckRanges, "ack_through": updatedSess.AckThrough,
				"success_count": updatedSess.SuccessCount, "failure_count": updatedSess.FailureCount,
				"rejected_seq": nil, "reject_reason": "",
			})
			result = fragmentResult{status: http.StatusOK, body: body}
		}
	}
	if event != nil {
		result.body["feedback_event"] = event
	}
	result.body["migrated"] = resolved.migrated
	result.body["migration"] = resolved.migration
	result.body["processing_ms"] = float64(time.Since(started).Microseconds()) / 1000
	g.metrics.Inc("edge_gateway_fragments_total", map[string]string{"ingress": ingress, "result": metricCode}, 1)
	if metricCode == model.CodeBackendRejected {
		g.metrics.Inc("edge_gateway_rejections_total", map[string]string{"ingress": ingress}, 1)
	}
	rt.finish(call, *seq, result)
	g.writeJSON(w, result.status, result.body)
}

func (g *Gateway) respondDuplicateFragment(w http.ResponseWriter, ingress string, sess *model.Session, rt *sessionRuntime, seq int64, resolved *resolvedSession, started time.Time) {
	if latest, err := g.store.GetSession(context.Background(), sess.ID); err == nil {
		sess = latest
		rt.applySession(sess)
	}
	stats := rt.statistics()
	body := g.feedbackPayload(sess, stats, nil, false)
	updateMap(body, map[string]any{
		"ok": true, "code": model.CodeOK, "session_id": sess.ID, "seq": seq,
		"accepted": true, "duplicate": true,
		"accepted_ranges": sess.AckRanges, "ack_through": sess.AckThrough,
		"success_count": sess.SuccessCount, "failure_count": sess.FailureCount,
		"rejected_seq": nil, "reject_reason": "",
		"migrated": resolved.migrated, "migration": resolved.migration,
		"processing_ms": float64(time.Since(started).Microseconds()) / 1000,
	})
	g.metrics.Inc("edge_gateway_fragments_total", map[string]string{"ingress": ingress, "result": "duplicate"}, 1)
	g.writeJSON(w, http.StatusOK, body)
}

func (g *Gateway) resolveFragmentSession(w http.ResponseWriter, r *http.Request, ingress string, req *fragmentRequest) (*resolvedSession, int, string, string) {
	addr, host := g.effectiveAddress(r)
	var (
		sess *model.Session
		rt   *sessionRuntime
		mig  *model.Migration
	)
	if req.SessionID != "" {
		var err error
		sess, err = g.store.GetSession(r.Context(), req.SessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, http.StatusNotFound, model.CodeNotFound, "session not found"
			}
			return nil, http.StatusInternalServerError, "STORE_ERROR", err.Error()
		}
	} else if req.ResumeToken != "" {
		updated, _, m, _, err := g.attachByToken(r, sessionRequest{
			DeviceID: req.DeviceID, ResumeToken: req.ResumeToken,
		}, ingress, addr, host, "fragment_resume")
		if errors.Is(err, errMissingSession) {
			return nil, http.StatusUnauthorized, model.CodeResumeTokenInvalid, "resume token is invalid for this ingress"
		}
		if errors.Is(err, errSessionCompleted) {
			return nil, http.StatusGone, model.CodeSessionCompleted, "session is completed"
		}
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, http.StatusGone, model.CodeSessionCompleted, "session is completed"
			}
			return nil, http.StatusInternalServerError, "STORE_ERROR", err.Error()
		}
		sess = updated
		mig = m
	} else if req.ConnectionID != "" && req.DeviceID != "" {
		var err error
		sess, err = g.store.GetByConnection(r.Context(), req.DeviceID, ingress, req.ConnectionID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, http.StatusInternalServerError, "STORE_ERROR", err.Error()
		}
	}
	if sess == nil && req.DeviceID != "" {
		var err error
		sess, err = g.store.GetActiveByDevice(r.Context(), req.DeviceID, ingress)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, http.StatusInternalServerError, "STORE_ERROR", err.Error()
		}
	}
	reused := sess != nil
	if sess == nil {
		if req.DeviceID == "" {
			return nil, http.StatusBadRequest, model.CodeBadRequest, "device_id or session_id is required"
		}
		var err error
		sess, _, err = g.store.CreateOrGetSession(r.Context(), req.DeviceID, ingress, req.ConnectionID, addr, host, g.cfg.MaxSessions)
		if errors.Is(err, store.ErrSessionLimit) {
			return nil, http.StatusServiceUnavailable, model.CodeSessionLimit, "active session limit reached"
		}
		if err != nil {
			return nil, http.StatusInternalServerError, "STORE_ERROR", err.Error()
		}
		reused = false
	}
	if sess.Ingress != ingress {
		return nil, http.StatusConflict, model.CodeSessionIngressMismatch, "session belongs to the other ingress"
	}
	if req.DeviceID != "" && sess.DeviceID != req.DeviceID {
		return nil, http.StatusConflict, model.CodeConflict, "device_id does not match session"
	}
	if !sess.Active || sess.Completed {
		return nil, http.StatusGone, model.CodeSessionCompleted, "session is completed"
	}
	rt = g.runtime(sess)
	updated, m, _, err := rt.observeAddress(r.Context(), g.store, addr, host, reused, g.cfg.MigrateWindow(), -1, "fragment")
	if err != nil {
		return nil, http.StatusInternalServerError, "STORE_ERROR", err.Error()
	}
	if updated != nil {
		sess = updated
	}
	rt.mergeSession(sess)
	if m != nil {
		mig = m
	}
	return &resolvedSession{sess: sess, rt: rt, migrated: mig != nil, migration: mig}, 0, "", ""
}

func (g *Gateway) feedbackPayload(sess *model.Session, stats runtimeStats, event *model.FeedbackEvent, frozen bool) map[string]any {
	if frozen || time.Now().UnixNano() < sess.MigrationUntilUnix {
		frozen = true
	}
	source := "none"
	if sess.LimitActive {
		source = "backend_backpressure"
	}
	retry := stats.RetryAfter
	if sess.LimitActive && retry <= 0 {
		retry = time.Duration(float64(time.Second) / sess.RateLimitPerSec)
	}
	fb := map[string]any{
		"limited": sess.LimitActive, "source": source, "reason": sess.LimitReason,
		"basis": sess.LimitBasis, "rate_limit_per_sec": sess.RateLimitPerSec,
		"limited_since_unix_nano": sess.LimitSinceUnix,
		"frozen_during_migration": frozen,
		"backend_backlog":         stats.Backlog, "backend_backlog_peak": stats.BacklogPeak,
		"rtt_median_ms":    stats.RTTMedianMS,
		"send_window_used": stats.SendWindow, "send_window_total": g.cfg.MaxSessionInflight,
		"retry_after_ms": retry.Milliseconds(),
	}
	if event != nil {
		fb["event"] = event
	}
	return map[string]any{
		"feedback": fb, "rate_limit_active": sess.LimitActive,
		"rate_limit_per_sec": sess.RateLimitPerSec, "backend_backlog": stats.Backlog,
		"backend_backlog_peak": stats.BacklogPeak,
		"send_window_used":     stats.SendWindow, "send_window_total": g.cfg.MaxSessionInflight,
	}
}

func decodeFragmentData(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("data_base64 is invalid")
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func boolFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

func nilIfUnblocked(sess *model.Session) any {
	if sess.Blocked {
		return sess.BlockedSeq
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// map.update is kept local because it makes the response assembly above much
// easier to audit: every field is added to exactly one response object.
func updateMap(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}
