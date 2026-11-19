package sim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"dnssim/internal/dnswire"
)

type zoneRuntime struct {
	cfg       Zone
	authority string
	records   map[string][]dnswire.RR
}

type resolverRuntime struct {
	cfg    Resolver
	addr   string
	faults []Fault
	cache  *dnsCache
	server *dnsServer
}

type Engine struct {
	cfg       *Config
	resolvers map[string]*resolverRuntime
	order     []*resolverRuntime
	zones     []*zoneRuntime
	addrIndex map[string]*resolverRuntime
	traces    *traceCollector

	ctx    context.Context
	cancel context.CancelFunc

	querySlots chan struct{}
	traceSlots chan struct{}

	activeQueries atomic.Int64
	peakQueries   atomic.Int64
	udpQueries    atomic.Uint64
	tcpQueries    atomic.Uint64

	closeOnce sync.Once
	wg        sync.WaitGroup
}

func NewEngine(cfg *Config) (*Engine, error) {
	if err := PrepareConfig(cfg); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		cfg:        cfg,
		resolvers:  make(map[string]*resolverRuntime, len(cfg.Resolvers)),
		addrIndex:  make(map[string]*resolverRuntime, len(cfg.Resolvers)),
		traces:     newTraceCollector(),
		ctx:        ctx,
		cancel:     cancel,
		querySlots: make(chan struct{}, cfg.Limits.MaxConcurrentQueries),
		traceSlots: make(chan struct{}, cfg.Limits.MaxConcurrentTraces),
	}
	for i := range cfg.Resolvers {
		r := cfg.Resolvers[i]
		rt := &resolverRuntime{cfg: r, cache: newDNSCache(), faults: append([]Fault(nil), r.Faults...)}
		e.resolvers[r.ID] = rt
		e.order = append(e.order, rt)
	}
	for i := range cfg.Faults {
		f := cfg.Faults[i]
		if rt := e.resolvers[f.Resolver]; rt != nil {
			rt.faults = append(rt.faults, f)
		}
	}
	for i := range cfg.Zones {
		z := cfg.Zones[i]
		zr := &zoneRuntime{cfg: z, authority: z.Authority, records: map[string][]dnswire.RR{}}
		for _, rec := range z.Records {
			rr, err := recordToRR(rec)
			if err != nil {
				cancel()
				return nil, bizError(CodeConfigInvalidValue, "zones."+z.Name+".records", "%v", err)
			}
			zr.records[rr.Name] = append(zr.records[rr.Name], rr)
		}
		e.zones = append(e.zones, zr)
	}
	sort.SliceStable(e.zones, func(i, j int) bool { return e.zones[i].cfg.Name < e.zones[j].cfg.Name })
	return e, nil
}

func StartEngine(cfg *Config) (*Engine, error) {
	e, err := NewEngine(cfg)
	if err != nil {
		return nil, err
	}
	if err := e.startServers(); err != nil {
		e.Close()
		return nil, err
	}
	return e, nil
}

func recordToRR(rec Record) (dnswire.RR, error) {
	t, err := dnswire.ParseType(rec.Type)
	if err != nil {
		return dnswire.RR{}, err
	}
	rr := dnswire.RR{Name: rec.Name, Type: t, Class: dnswire.ClassIN, TTL: rec.TTL}
	switch t {
	case dnswire.TypeA, dnswire.TypeAAAA:
		rr.A = rec.Value
	case dnswire.TypeCNAME, dnswire.TypeNS:
		rr.Target = rec.Value
	case dnswire.TypeTXT:
		rr.Text = []string{rec.Value}
	default:
		return dnswire.RR{}, fmt.Errorf("unsupported record type %q", rec.Type)
	}
	return rr, nil
}

func (e *Engine) startServers() error {
	for _, rt := range e.order {
		if err := bindResolver(e, rt); err != nil {
			return err
		}
		e.addrIndex[rt.addr] = rt
	}
	for _, rt := range e.order {
		if rt.cfg.Cache != nil {
			rt.cache.loadPreload(rt.cfg.Cache.Preload)
		}
		rt.server.start()
	}
	return nil
}

func bindResolver(e *Engine, rt *resolverRuntime) error {
	_, port, err := net.SplitHostPort(rt.cfg.Address)
	if err != nil {
		return bizError(CodeBindFailed, "resolvers."+rt.cfg.ID+".address", "resolve address: %v", err)
	}
	attempts := 1
	if port == "0" {
		attempts = 32
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		udpAddr, err := net.ResolveUDPAddr("udp", rt.cfg.Address)
		if err != nil {
			return bizError(CodeBindFailed, "resolvers."+rt.cfg.ID+".address", "resolve address: %v", err)
		}
		udp, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			lastErr = err
			if port == "0" {
				continue
			}
			break
		}
		actual := udp.LocalAddr().String()
		tcp, err := net.Listen("tcp", actual)
		if err != nil {
			_ = udp.Close()
			lastErr = err
			if port == "0" {
				continue
			}
			break
		}
		rt.addr = actual
		rt.server = &dnsServer{engine: e, resolver: rt, udp: udp, tcp: tcp}
		return nil
	}
	return bizError(CodeBindFailed, "resolvers."+rt.cfg.ID+".address", "bind UDP/TCP %s: %v", rt.cfg.Address, lastErr)
}

func (e *Engine) Close() {
	e.closeOnce.Do(func() {
		e.cancel()
		for _, rt := range e.order {
			if rt.server != nil {
				rt.server.close()
			}
		}
		e.wg.Wait()
	})
}

func (e *Engine) Context() context.Context { return e.ctx }

func (e *Engine) Config() *Config { return e.cfg }

func (e *Engine) Addresses() map[string]string {
	out := make(map[string]string, len(e.order))
	for _, rt := range e.order {
		out[rt.cfg.ID] = rt.addr
	}
	return out
}

func (e *Engine) Stats() map[string]any {
	return map[string]any{
		"active_queries":     e.activeQueries.Load(),
		"peak_queries":       e.peakQueries.Load(),
		"query_limit":        e.cfg.Limits.MaxConcurrentQueries,
		"udp_queries":        e.udpQueries.Load(),
		"tcp_queries":        e.tcpQueries.Load(),
		"trace_limit":        e.cfg.Limits.MaxConcurrentTraces,
		"resolver_listeners": len(e.order),
	}
}

func (e *Engine) site(id string) (*Site, bool) {
	for i := range e.cfg.Sites {
		if e.cfg.Sites[i].ID == id {
			return &e.cfg.Sites[i], true
		}
	}
	return nil, false
}

func (e *Engine) authorityFor(name string) (*resolverRuntime, *zoneRuntime, bool) {
	var best *zoneRuntime
	for _, z := range e.zones {
		if dnswire.IsSubdomain(name, z.cfg.Name) && (best == nil || len(z.cfg.Name) > len(best.cfg.Name)) {
			best = z
		}
	}
	if best == nil {
		return nil, nil, false
	}
	rt := e.resolvers[best.authority]
	if rt == nil {
		return nil, nil, false
	}
	return rt, best, true
}

func (e *Engine) resolverByAddress(addr string) *resolverRuntime {
	return e.addrIndex[addr]
}

type slotLease struct {
	engine *Engine
	mu     sync.Mutex
	held   bool
}

func (e *Engine) acquireSlot(ctx context.Context) (*slotLease, error) {
	timeout := time.Duration(e.cfg.Limits.DefaultTimeoutMS) * time.Millisecond
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case e.querySlots <- struct{}{}:
		e.enterQuery()
		return &slotLease{engine: e, held: true}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("query concurrency limit reached")
	}
}

func (e *Engine) enterQuery() {
	n := e.activeQueries.Add(1)
	for {
		peak := e.peakQueries.Load()
		if n <= peak || e.peakQueries.CompareAndSwap(peak, n) {
			break
		}
	}
}

func (l *slotLease) Suspend() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.held {
		return
	}
	<-l.engine.querySlots
	l.held = false
	l.engine.activeQueries.Add(-1)
}

func (l *slotLease) Resume(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		return nil
	}
	timeout := time.Duration(l.engine.cfg.Limits.DefaultTimeoutMS) * time.Millisecond
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case l.engine.querySlots <- struct{}{}:
		l.engine.enterQuery()
		l.held = true
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("query concurrency limit reached")
	}
}

func (l *slotLease) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.held {
		return
	}
	<-l.engine.querySlots
	l.held = false
	l.engine.activeQueries.Add(-1)
}
