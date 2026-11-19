package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"feedback_zsl/internal/config"
	"feedback_zsl/internal/metrics"
	"feedback_zsl/internal/priority"
	"feedback_zsl/internal/scheduler"
	"feedback_zsl/internal/upstream"
)

type route struct {
	cfg       config.RouteConfig
	upstreams []*upstream.Upstream
	methods   map[string]bool
}

func (r *route) matches(req *http.Request) bool {
	if !strings.HasPrefix(req.URL.Path, r.cfg.PathPrefix) {
		return false
	}
	if len(r.methods) == 0 {
		return true
	}
	return r.methods[strings.ToUpper(req.Method)]
}

type Runtime struct {
	cfg       *config.Config
	mapper    *priority.Mapper
	routes    []*route
	upstreams map[string]*upstream.Upstream
	sched     *scheduler.Scheduler
	metrics   *metrics.Registry
	idem      *idempotencyCache

	healthCtx    context.Context
	healthCancel context.CancelFunc
	startOnce    sync.Once

	mu        sync.Mutex
	refs      int
	retired   bool
	closeOnce sync.Once
	closed    bool
}

func newRuntime(cfg *config.Config, reg *metrics.Registry) (*Runtime, error) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := &Runtime{
		cfg:          cfg,
		mapper:       priority.New(cfg),
		upstreams:    make(map[string]*upstream.Upstream, len(cfg.Upstreams)),
		metrics:      reg,
		idem:         newIdempotencyCache(),
		healthCtx:    ctx,
		healthCancel: cancel,
	}
	for _, uc := range cfg.Upstreams {
		up, err := upstream.New(uc, cfg.Timeouts, cfg.Scheduler, reg)
		if err != nil {
			rt.close()
			return nil, err
		}
		rt.upstreams[uc.ID] = up
	}
	for _, rc := range cfg.Routes {
		r := &route{cfg: rc, methods: make(map[string]bool)}
		for _, id := range rc.Upstreams {
			up := rt.upstreams[id]
			if up == nil {
				rt.close()
				return nil, fmt.Errorf("route %s references unknown upstream %s", rc.ID, id)
			}
			r.upstreams = append(r.upstreams, up)
		}
		for _, m := range rc.Methods {
			r.methods[m] = true
		}
		rt.routes = append(rt.routes, r)
	}
	sort.SliceStable(rt.routes, func(i, j int) bool {
		return len(rt.routes[i].cfg.PathPrefix) > len(rt.routes[j].cfg.PathPrefix)
	})
	rt.sched = scheduler.New(scheduler.Config{
		MaxInflight: cfg.Scheduler.MaxInflight,
		MaxWait:     cfg.Scheduler.MaxWait.Duration(),
		AgingPeriod: cfg.Scheduler.AgingInterval.Duration(),
	})
	return rt, nil
}

func (rt *Runtime) start() {
	rt.startOnce.Do(func() {
		for _, up := range rt.upstreams {
			up.StartHealth(rt.healthCtx)
		}
	})
}

func (rt *Runtime) acquire() {
	rt.mu.Lock()
	rt.refs++
	rt.mu.Unlock()
}

func (rt *Runtime) release() {
	rt.mu.Lock()
	rt.refs--
	shouldClose := rt.retired && rt.refs == 0
	rt.mu.Unlock()
	if shouldClose {
		rt.close()
	}
}

func (rt *Runtime) retire() {
	rt.mu.Lock()
	rt.retired = true
	shouldClose := rt.refs == 0
	rt.mu.Unlock()
	if shouldClose {
		rt.close()
	}
}

func (rt *Runtime) close() {
	rt.closeOnce.Do(func() {
		rt.mu.Lock()
		rt.closed = true
		rt.mu.Unlock()
		if rt.healthCancel != nil {
			rt.healthCancel()
		}
		if rt.sched != nil {
			rt.sched.Close()
		}
		for _, up := range rt.upstreams {
			up.Close()
		}
	})
}

func (rt *Runtime) match(req *http.Request) *route {
	for _, r := range rt.routes {
		if r.matches(req) {
			return r
		}
	}
	return nil
}

func (rt *Runtime) pick(r *route, excluded map[string]bool) *upstream.Upstream {
	for _, up := range r.upstreams {
		if excluded[up.ID()] {
			continue
		}
		if up.Healthy() {
			return up
		}
	}
	return nil
}

func (rt *Runtime) healthyCount(r *route, excluded map[string]bool) int {
	n := 0
	for _, up := range r.upstreams {
		if !excluded[up.ID()] && up.Healthy() {
			n++
		}
	}
	return n
}

func (rt *Runtime) QueueDepths() map[string]int {
	out := make(map[string]int, len(rt.cfg.Priorities))
	for _, p := range rt.cfg.Priorities {
		out[p.Name] = 0
	}
	for _, q := range rt.sched.Queues() {
		out[q.Priority] = q.Depth
	}
	return out
}

func (rt *Runtime) UpstreamStatuses() []upstream.Status {
	out := make([]upstream.Status, 0, len(rt.upstreams))
	for _, up := range rt.upstreams {
		out = append(out, up.Status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
