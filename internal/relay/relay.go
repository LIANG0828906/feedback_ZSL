package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"parkrelay/internal/protocol"
	"parkrelay/internal/store"
)

type Relay struct {
	cfg   Config
	store *store.Store

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	controlUDP net.PacketConn
	controlTCP net.Listener
	httpLn     net.Listener
	httpSrv    *http.Server
	allocator  *portAllocator

	regMu     sync.RWMutex
	mappings  map[string]*mapping
	lastProbe map[string]protocol.ProbeEvidence

	probeMu      sync.Mutex
	probeWaiters map[string]chan struct{}

	controlConnsMu sync.Mutex
	controlConns   map[net.Conn]struct{}

	stopOnce sync.Once
}

func Start(ctx context.Context, cfg Config) (*Relay, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	r := &Relay{
		cfg:          cfg,
		store:        st,
		ctx:          runCtx,
		cancel:       cancel,
		mappings:     make(map[string]*mapping),
		lastProbe:    make(map[string]protocol.ProbeEvidence),
		probeWaiters: make(map[string]chan struct{}),
		controlConns: make(map[net.Conn]struct{}),
	}
	if err := st.Ping(runCtx); err != nil {
		cancel()
		st.Close()
		return nil, fmt.Errorf("sqlite ping: %w", err)
	}
	cooldowns, err := st.Cooldowns(runCtx, time.Now())
	if err != nil {
		r.closeAfterStartFailure()
		return nil, err
	}
	r.allocator = newPortAllocator(cfg.BindIP, cfg.PortMin, cfg.PortMax, cooldowns)
	if err := r.restoreMappings(); err != nil {
		r.closeAfterStartFailure()
		return nil, err
	}
	if err := r.startControlServers(); err != nil {
		r.closeAfterStartFailure()
		return nil, err
	}
	if err := r.startHTTP(); err != nil {
		r.closeAfterStartFailure()
		return nil, err
	}
	r.wg.Add(2)
	go r.sweepLoop()
	go r.probeRefreshLoop()
	return r, nil
}

func (r *Relay) closeAfterStartFailure() {
	r.cancel()
	r.closeAllMappings()
	if r.controlUDP != nil {
		r.controlUDP.Close()
	}
	if r.controlTCP != nil {
		r.controlTCP.Close()
	}
	if r.httpLn != nil {
		r.httpLn.Close()
	}
	r.store.Close()
}

func (r *Relay) Stop() error {
	var stopErr error
	r.stopOnce.Do(func() {
		r.cancel()
		if r.controlUDP != nil {
			_ = r.controlUDP.Close()
		}
		if r.controlTCP != nil {
			_ = r.controlTCP.Close()
		}
		r.controlConnsMu.Lock()
		for conn := range r.controlConns {
			_ = conn.Close()
		}
		r.controlConnsMu.Unlock()
		if r.httpSrv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = r.httpSrv.Shutdown(ctx)
			cancel()
		}
		r.closeAllMappings()
		r.wg.Wait()
		stopErr = r.store.Close()
	})
	return stopErr
}

func (r *Relay) restoreMappings() error {
	loaded, err := r.store.LoadMappings(r.ctx)
	if err != nil {
		return fmt.Errorf("load mappings: %w", err)
	}
	now := time.Now()
	used := make(map[int]bool)
	for _, sm := range loaded {
		if !sm.LeaseExpires.After(now) {
			until := now.Add(r.cfg.RecoveryCooldown)
			if err := r.store.AddRecycle(r.ctx, store.Recycle{
				GatewayID: sm.GatewayID, Protocol: sm.Protocol, ExternalPort: sm.ExternalPort,
				Reason: "lease_expired_during_restore", RecycledAt: now, CooldownUntil: until,
			}); err != nil {
				return err
			}
			r.allocator.setCooldown(sm.ExternalPort, until)
			if err := r.store.DeleteMapping(r.ctx, sm.GatewayID, sm.Protocol); err != nil {
				return err
			}
			continue
		}
		if used[sm.ExternalPort] {
			return fmt.Errorf("sqlite contains duplicate active external port %d", sm.ExternalPort)
		}
		m := mappingFromStore(sm)
		switch sm.Protocol {
		case protocol.ProtocolUDP:
			pc, err := listenRestoredUDP(r.cfg.BindIP, sm.ExternalPort)
			if err != nil {
				return fmt.Errorf("restore udp mapping %s:%d: %w", sm.GatewayID, sm.ExternalPort, err)
			}
			m.udpConn = pc
		case protocol.ProtocolTCP:
			ln, err := listenRestoredTCP(r.cfg.BindIP, sm.ExternalPort)
			if err != nil {
				return fmt.Errorf("restore tcp mapping %s:%d: %w", sm.GatewayID, sm.ExternalPort, err)
			}
			m.tcpLn = ln
		default:
			return fmt.Errorf("unknown protocol %q in sqlite", sm.Protocol)
		}
		used[sm.ExternalPort] = true
		m.Recovered = true
		r.mappings[mappingKey(sm.GatewayID, sm.Protocol)] = m
		if sm.ProbeEvidence != "" && sm.ProbeEvidence != "{}" {
			r.lastProbe[sm.GatewayID] = m.ProbeEvidence
		}
	}
	for _, m := range r.mappings {
		if err := r.persist(m); err != nil {
			return fmt.Errorf("mark recovered mapping %s/%s: %w", m.GatewayID, m.Protocol, err)
		}
		r.startMappingReader(m)
	}
	return nil
}

func mappingKey(gatewayID, proto string) string { return gatewayID + "\x00" + proto }

func (r *Relay) portUsedLocked(port int) bool {
	for _, m := range r.mappings {
		if m.ExternalPort == port {
			return true
		}
	}
	return false
}

func (r *Relay) lookup(gatewayID, proto string) *mapping {
	r.regMu.RLock()
	defer r.regMu.RUnlock()
	return r.mappings[mappingKey(gatewayID, proto)]
}

func (r *Relay) allMappings() []*mapping {
	r.regMu.RLock()
	defer r.regMu.RUnlock()
	out := make([]*mapping, 0, len(r.mappings))
	for _, m := range r.mappings {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GatewayID != out[j].GatewayID {
			return out[i].GatewayID < out[j].GatewayID
		}
		return protocolRank(out[i].Protocol) < protocolRank(out[j].Protocol)
	})
	return out
}

func protocolRank(proto string) int {
	if proto == protocol.ProtocolUDP {
		return 0
	}
	return 1
}

func (r *Relay) closeAllMappings() {
	r.regMu.Lock()
	all := make([]*mapping, 0, len(r.mappings))
	for _, m := range r.mappings {
		all = append(all, m)
	}
	r.regMu.Unlock()
	for _, m := range all {
		r.closeMappingTransport(m)
	}
}

func (r *Relay) closeMappingTransport(m *mapping) {
	if m.udpConn != nil {
		_ = m.udpConn.Close()
	}
	if m.tcpLn != nil {
		_ = m.tcpLn.Close()
	}
	m.tcpDataMu.Lock()
	if m.tcpData != nil {
		_ = m.tcpData.Close()
		m.tcpData = nil
	}
	m.tcpDataMu.Unlock()
	m.forwardMu.Lock()
	if m.udpForward != nil {
		_ = m.udpForward.Close()
		m.udpForward = nil
	}
	if m.tcpForward != nil {
		_ = m.tcpForward.Close()
		m.tcpForward = nil
	}
	m.forwardMu.Unlock()
}

func (r *Relay) startMappingReader(m *mapping) {
	if m.udpConn != nil {
		if r.cfg.ForwardUDP != "" {
			conn, err := net.DialTimeout("udp4", r.cfg.ForwardUDP, time.Second)
			if err == nil {
				m.forwardMu.Lock()
				m.udpForward = conn
				m.forwardMu.Unlock()
			}
		}
		r.wg.Add(1)
		go r.udpMappingLoop(m)
	}
	if m.tcpLn != nil {
		r.wg.Add(1)
		go r.tcpMappingAcceptLoop(m)
	}
}

func (r *Relay) startControlServers() error {
	pc, err := net.ListenPacket("udp4", r.cfg.ControlUDP)
	if err != nil {
		return fmt.Errorf("listen udp control: %w", err)
	}
	ln, err := net.Listen("tcp4", r.cfg.ControlTCP)
	if err != nil {
		pc.Close()
		return fmt.Errorf("listen tcp control: %w", err)
	}
	r.controlUDP, r.controlTCP = pc, ln
	r.wg.Add(2)
	go r.udpControlLoop()
	go r.tcpControlAcceptLoop()
	return nil
}

func (r *Relay) startHTTP() error {
	ln, err := net.Listen("tcp4", r.cfg.HTTP)
	if err != nil {
		return fmt.Errorf("listen http: %w", err)
	}
	r.httpLn = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", r.handleHealth)
	mux.HandleFunc("/metrics", r.handleMetrics)
	mux.HandleFunc("/v1/status", r.handleStatus)
	mux.HandleFunc("/v1/recycles", r.handleRecycles)
	r.httpSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		err := r.httpSrv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && r.ctx.Err() == nil {
			// The health endpoint reflects the listener. A production process
			// manager should restart on this error.
			fmt.Printf("http server stopped: %v\n", err)
		}
	}()
	return nil
}

func (r *Relay) HttpAddress() string {
	if r.httpLn == nil {
		return r.cfg.HTTP
	}
	return r.httpLn.Addr().String()
}

func (r *Relay) ControlUDPAddress() string {
	if r.controlUDP == nil {
		return r.cfg.ControlUDP
	}
	return r.controlUDP.LocalAddr().String()
}

func (r *Relay) ControlTCPAddress() string {
	if r.controlTCP == nil {
		return r.cfg.ControlTCP
	}
	return r.controlTCP.Addr().String()
}
