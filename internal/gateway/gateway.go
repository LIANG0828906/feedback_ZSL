package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"edgegateway/internal/backend"
	"edgegateway/internal/config"
	"edgegateway/internal/metrics"
	"edgegateway/internal/model"
	"edgegateway/internal/store"
)

type remoteAddrProvider interface {
	RemoteAddr() net.Addr
}

type connContextKey struct{}

// WithQUICConnection lets the HTTP/3 server expose the underlying QUIC
// connection for dynamic RemoteAddr observation.
func WithQUICConnection(ctx context.Context, conn remoteAddrProvider) context.Context {
	return context.WithValue(ctx, connContextKey{}, conn)
}

type ingressContextKey struct{}

type Gateway struct {
	cfg     config.Config
	store   *store.Store
	backend *backend.Client
	metrics *metrics.Registry
	secret  []byte
	log     *slog.Logger

	mu sync.Mutex
	rt map[string]*sessionRuntime
}

func New(cfg config.Config, st *store.Store, bc *backend.Client, mr *metrics.Registry, secret []byte, log *slog.Logger) *Gateway {
	return &Gateway{
		cfg: cfg, store: st, backend: bc, metrics: mr, secret: secret, log: log,
		rt: make(map[string]*sessionRuntime),
	}
}

func (g *Gateway) Handler(ingress string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := context.WithValue(r.Context(), ingressContextKey{}, ingress)
		rr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		g.route(rr, r.WithContext(ctx), ingress)
		g.metrics.ObserveDuration(ingress, time.Since(start).Seconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (g *Gateway) route(w http.ResponseWriter, r *http.Request, ingress string) {
	switch {
	case r.URL.Path == "/healthz":
		g.handleHealth(w, r)
	case r.URL.Path == "/metrics":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		g.metrics.Render(r.Context(), w)
	case r.URL.Path == "/v1/config":
		g.writeJSON(w, http.StatusOK, map[string]any{
			"udp_listen": g.cfg.UDPListen, "tcp_listen": g.cfg.TCPListen,
			"metrics_listen": g.cfg.MetricsListen,
			"max_sessions":   g.cfg.MaxSessions, "max_session_inflight": g.cfg.MaxSessionInflight,
			"migration_window_ms": g.cfg.MigrationWindowMS, "rtt_threshold_ms": g.cfg.RTTThresholdMS,
			"backlog_threshold": g.cfg.BacklogThreshold, "rate_limit_per_sec": g.cfg.RateLimitPerSec,
			"allow_peer_override": g.cfg.AllowPeerOverride,
		})
	case r.URL.Path == "/v1/sessions" && r.Method == http.MethodPost:
		g.handleSessionCreate(w, r, ingress)
	case r.URL.Path == "/v1/sessions" && r.Method == http.MethodGet:
		g.handleSessionList(w, r)
	case r.URL.Path == "/v1/sessions/attach" && r.Method == http.MethodPost:
		g.handleSessionAttach(w, r, ingress)
	case r.URL.Path == "/v1/fragments" && r.Method == http.MethodPost:
		g.handleFragment(w, r, ingress)
	case strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
		g.handleSessionSubroute(w, r, ingress)
	default:
		g.writeError(w, http.StatusNotFound, model.CodeNotFound, "route not found", nil)
	}
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	backendOK := g.backend.Health(ctx) == nil
	code := http.StatusOK
	if !backendOK {
		code = http.StatusServiceUnavailable
	}
	g.writeJSON(w, code, map[string]any{"ok": backendOK, "backend": backendOK, "time_unix_nano": time.Now().UnixNano()})
}

func (g *Gateway) handleSessionSubroute(w http.ResponseWriter, r *http.Request, ingress string) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		g.writeError(w, http.StatusNotFound, model.CodeNotFound, "session id required", nil)
		return
	}
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		g.handleSessionGet(w, r, id)
		return
	}
	if len(parts) == 2 {
		switch {
		case parts[1] == "complete" && r.Method == http.MethodPost:
			g.handleSessionComplete(w, r, id)
		case parts[1] == "resume" && r.Method == http.MethodPost:
			g.handleSessionResume(w, r, id)
		case parts[1] == "feedback" && r.Method == http.MethodGet:
			g.handleSessionFeedback(w, r, id)
		case parts[1] == "rejections" && r.Method == http.MethodGet:
			g.handleSessionRejections(w, r, id)
		case parts[1] == "migrations" && r.Method == http.MethodGet:
			g.handleSessionMigrations(w, r, id)
		default:
			g.writeError(w, http.StatusNotFound, model.CodeNotFound, "route not found", nil)
		}
		return
	}
	g.writeError(w, http.StatusNotFound, model.CodeNotFound, "route not found", nil)
}

func (g *Gateway) handleSessionList(w http.ResponseWriter, r *http.Request) {
	activeOnly := r.URL.Query().Get("active") != "false"
	sessions, err := g.store.ListSessions(r.Context(), activeOnly, 200)
	if err != nil {
		g.writeError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	g.writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions, "count": len(sessions)})
}

func (g *Gateway) handleSessionGet(w http.ResponseWriter, r *http.Request, id string) {
	sess, err := g.store.GetSession(r.Context(), id)
	if err != nil {
		g.sessionStoreError(w, err)
		return
	}
	migrations, _ := g.store.Migrations(r.Context(), id)
	feedback, _ := g.store.Feedback(r.Context(), id, 20)
	rejections, _ := g.store.Rejections(r.Context(), id, 20)
	rt := g.runtime(sess)
	live := rt.statistics()
	g.writeJSON(w, http.StatusOK, map[string]any{
		"session": sess, "migrations": migrations, "feedback": feedback,
		"rejections": rejections, "resume_token": g.resumeToken(id),
		"runtime": map[string]any{
			"rtt_median_ms": live.RTTMedianMS, "rtt_samples_ms": live.RTTSamples,
			"backend_backlog": live.Backlog, "backend_backlog_peak": live.BacklogPeak,
			"send_window_used": live.SendWindow, "send_window_peak": live.SendWindowPeak,
			"send_window_total": g.cfg.MaxSessionInflight,
			"rate_limit_active": live.RateActive, "rate_limit_per_sec": live.Rate,
		},
	})
}

func (g *Gateway) handleSessionMigrations(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := g.store.GetSession(r.Context(), id); err != nil {
		g.sessionStoreError(w, err)
		return
	}
	out, err := g.store.Migrations(r.Context(), id)
	if err != nil {
		g.writeError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	g.writeJSON(w, http.StatusOK, map[string]any{"migrations": out})
}

func (g *Gateway) handleSessionFeedback(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := g.store.GetSession(r.Context(), id); err != nil {
		g.sessionStoreError(w, err)
		return
	}
	out, err := g.store.Feedback(r.Context(), id, 200)
	if err != nil {
		g.writeError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	g.writeJSON(w, http.StatusOK, map[string]any{"feedback": out})
}

func (g *Gateway) handleSessionRejections(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := g.store.GetSession(r.Context(), id); err != nil {
		g.sessionStoreError(w, err)
		return
	}
	out, err := g.store.Rejections(r.Context(), id, 200)
	if err != nil {
		g.writeError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	g.writeJSON(w, http.StatusOK, map[string]any{"rejections": out})
}

func (g *Gateway) handleSessionComplete(w http.ResponseWriter, r *http.Request, id string) {
	if err := g.store.CompleteSession(r.Context(), id); err != nil {
		g.sessionStoreError(w, err)
		return
	}
	g.forgetRuntime(id)
	sess, err := g.store.GetSession(r.Context(), id)
	if err != nil {
		g.sessionStoreError(w, err)
		return
	}
	g.writeJSON(w, http.StatusOK, map[string]any{"session": sess, "completed": true})
}

func (g *Gateway) handleSessionResume(w http.ResponseWriter, r *http.Request, id string) {
	sess, err := g.store.ClearBlock(r.Context(), id)
	if err != nil {
		g.sessionStoreError(w, err)
		return
	}
	g.writeJSON(w, http.StatusOK, map[string]any{"session": sess, "blocked": false})
}

func (g *Gateway) sessionStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		g.writeError(w, http.StatusNotFound, model.CodeNotFound, "session not found", nil)
		return
	}
	g.writeError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
}

func (g *Gateway) resumeToken(sessionID string) string {
	mac := hmac.New(sha256.New, g.secret)
	_, _ = mac.Write([]byte(sessionID))
	id := base64.RawURLEncoding.EncodeToString([]byte(sessionID))
	return "v1." + id + "." + hex.EncodeToString(mac.Sum(nil))
}

func (g *Gateway) verifyResumeToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return "", errors.New("invalid token format")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("invalid token payload")
	}
	id := string(raw)
	sig, err := hex.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("invalid token signature")
	}
	mac := hmac.New(sha256.New, g.secret)
	_, _ = mac.Write([]byte(id))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", errors.New("invalid token signature")
	}
	return id, nil
}

func (g *Gateway) effectiveAddress(r *http.Request) (string, string) {
	if g.cfg.AllowPeerOverride {
		if override := strings.TrimSpace(r.Header.Get("X-Edge-Peer-Address")); override != "" {
			return override, hostPart(override)
		}
	}
	addr := r.RemoteAddr
	if provider, ok := r.Context().Value(connContextKey{}).(remoteAddrProvider); ok {
		if a := provider.RemoteAddr(); a != nil {
			addr = a.String()
		}
	}
	return addr, hostPart(addr)
}

func hostPart(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		return strings.ToLower(host)
	}
	return strings.ToLower(addr)
}

func (g *Gateway) writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
}

func (g *Gateway) writeError(w http.ResponseWriter, status int, code, message string, extra map[string]any) {
	body := map[string]any{"ok": false, "code": code, "error": message, "business_error_code": code}
	for k, v := range extra {
		body[k] = v
	}
	g.writeJSON(w, status, body)
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	body := http.MaxBytesReader(w, r.Body, limit)
	defer body.Close()
	dec := json.NewDecoder(body)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func (g *Gateway) logError(msg string, args ...any) {
	if g.log != nil {
		g.log.Error(msg, args...)
	}
}

func (g *Gateway) String() string {
	return fmt.Sprintf("edgegateway sessions_limit=%d", g.cfg.MaxSessions)
}
