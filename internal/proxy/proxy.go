package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"feedback_zsl/internal/config"
	"feedback_zsl/internal/entry"
	"feedback_zsl/internal/metrics"
	"feedback_zsl/internal/upstream"
)

type Proxy struct {
	mu       sync.RWMutex
	rt       *Runtime
	cfgPath  string
	metrics  *metrics.Registry
	logger   *log.Logger
	boundH2  string
	boundH3  string
	boundAdm string
	sessions map[string]sessionState
	sessMu   sync.Mutex
}

type sessionState struct {
	remote     string
	connection string
	migrated   bool
	lastSeen   time.Time
}

func New(cfgPath string, logger *log.Logger) (*Proxy, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	return NewWithConfig(cfg, logger)
}

func NewWithConfig(cfg *config.Config, logger *log.Logger) (*Proxy, error) {
	reg := metrics.New()
	rt, err := newRuntime(cfg, reg)
	if err != nil {
		return nil, err
	}
	return &Proxy{
		rt:       rt,
		cfgPath:  cfg.Path,
		metrics:  reg,
		logger:   logger,
		sessions: make(map[string]sessionState),
	}, nil
}

func (p *Proxy) Start() {
	p.mu.RLock()
	rt := p.rt
	p.mu.RUnlock()
	if rt != nil {
		rt.start()
	}
}

func (p *Proxy) acquireRuntime() *Runtime {
	p.mu.RLock()
	rt := p.rt
	if rt != nil {
		rt.acquire()
	}
	p.mu.RUnlock()
	return rt
}

func (p *Proxy) RequiredHeaders() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.rt == nil {
		return []string{"host"}
	}
	if len(p.rt.cfg.RequiredHdrs) == 0 {
		return []string{"host"}
	}
	return append([]string{"host"}, p.rt.cfg.RequiredHdrs...)
}

func (p *Proxy) SetBoundAddresses(h2, h3, admin string) {
	p.mu.Lock()
	p.boundH2, p.boundH3, p.boundAdm = h2, h3, admin
	p.mu.Unlock()
}

func (p *Proxy) ProtocolError(protocol, streamID, code, message string) {
	p.metrics.ProtocolError(protocol, streamID, code, message)
	p.metrics.EntryError(protocol, code)
}

func (p *Proxy) Reload() error {
	newCfg, err := config.Load(p.cfgPath)
	if err != nil {
		return err
	}
	p.mu.RLock()
	oldCfg := p.rt.cfg
	p.mu.RUnlock()
	if newCfg.ListenH2 != oldCfg.ListenH2 || newCfg.ListenH3 != oldCfg.ListenH3 || newCfg.AdminListen != oldCfg.AdminListen {
		return &config.ValidationError{
			Code:    config.ErrorListenChange,
			Field:   "listen_h2/listen_h3/admin_listen",
			Message: "listener addresses cannot be changed by hot reload; restart the process",
		}
	}
	next, err := newRuntime(newCfg, p.metrics)
	if err != nil {
		return err
	}
	p.mu.Lock()
	old := p.rt
	p.rt = next
	p.mu.Unlock()
	next.start()
	if old != nil {
		old.retire()
	}
	if p.logger != nil {
		p.logger.Printf("configuration atomically switched to revision %s", newCfg.Revision)
	}
	return nil
}

func (p *Proxy) observeMigration(r *http.Request, meta entry.Meta) (string, bool) {
	if meta.Protocol != "h3" {
		return "", false
	}
	session := r.Header.Get("X-Gateway-Session-ID")
	if session == "" {
		return "", false
	}
	p.sessMu.Lock()
	defer p.sessMu.Unlock()
	now := time.Now()
	prev, ok := p.sessions[session]
	resumed := ok && (prev.remote != meta.RemoteAddr || prev.connection != meta.ConnectionID)
	p.sessions[session] = sessionState{
		remote: meta.RemoteAddr, connection: meta.ConnectionID, migrated: prev.migrated || resumed, lastSeen: now,
	}
	for k, v := range p.sessions {
		if now.Sub(v.lastSeen) > 10*time.Minute {
			delete(p.sessions, k)
		}
	}
	if resumed {
		p.metrics.Migration("observed")
	}
	return session, resumed
}

func (p *Proxy) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		rt := p.acquireRuntime()
		if rt == nil {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		queues := rt.QueueDepths()
		rt.release()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(p.metrics.Render(queues)))
	})
	status := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		rt := p.acquireRuntime()
		if rt == nil {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		type statusResponse struct {
			Revision                    string                  `json:"config_revision"`
			ListenH2                    string                  `json:"listen_h2"`
			ListenH3                    string                  `json:"listen_h3"`
			AdminListen                 string                  `json:"admin_listen"`
			Upstreams                   []upstream.Status       `json:"upstreams"`
			Queues                      map[string]int          `json:"priority_queue_depth"`
			Inflight                    int                     `json:"scheduler_inflight"`
			PriorityWait                []metrics.WaitQuantiles `json:"priority_schedule_wait"`
			LastProtocolError           *metrics.ProtocolError  `json:"last_protocol_error,omitempty"`
			MigrationCounts             map[string]uint64       `json:"h3_connection_migrations"`
			TransportMigrationSupported bool                    `json:"transport_migration_supported"`
			H3PriorityUpdateSupported   bool                    `json:"h3_priority_update_supported"`
			H3PriorityHeaderSupported   bool                    `json:"h3_priority_header_supported"`
			MigrationNote               string                  `json:"migration_note"`
			Timestamp                   time.Time               `json:"timestamp"`
		}
		resp := statusResponse{
			Revision:                    rt.cfg.Revision,
			ListenH2:                    p.boundH2,
			ListenH3:                    p.boundH3,
			AdminListen:                 p.boundAdm,
			Upstreams:                   rt.UpstreamStatuses(),
			Queues:                      rt.QueueDepths(),
			Inflight:                    rt.sched.Inflight(),
			PriorityWait:                p.metrics.WaitQuantiles(),
			LastProtocolError:           p.metrics.LastProtocolError(),
			MigrationCounts:             p.metrics.MigrationCounts(),
			TransportMigrationSupported: false,
			H3PriorityUpdateSupported:   false,
			H3PriorityHeaderSupported:   true,
			MigrationNote:               "quic-go v0.45 sends disable_active_migration; h3 address changes are validated at explicit session-resume level",
			Timestamp:                   time.Now().UTC(),
		}
		rt.release()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
	mux.HandleFunc("/__gateway/status", status)
	mux.HandleFunc("/status", status)
	mux.HandleFunc("/__gateway/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := p.Reload(); err != nil {
			code := "CONFIG_RELOAD_FAILED"
			var ve *config.ValidationError
			if asValidation(err, &ve) {
				code = ve.Code
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "reloaded"})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

func asValidation(err error, target **config.ValidationError) bool {
	for err != nil {
		if v, ok := err.(*config.ValidationError); ok {
			*target = v
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return false
}

func (p *Proxy) Shutdown(ctx context.Context) {
	p.mu.Lock()
	rt := p.rt
	p.rt = nil
	p.mu.Unlock()
	if rt != nil {
		rt.retire()
	}
	select {
	case <-ctx.Done():
	case <-time.After(50 * time.Millisecond):
	}
}

func (p *Proxy) String() string {
	return fmt.Sprintf("proxy(config=%s)", p.cfgPath)
}
