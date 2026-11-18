package scenario

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"parkrelay/internal/gateway"
	"parkrelay/internal/natsim"
	"parkrelay/internal/protocol"
	"parkrelay/internal/relay"
	"parkrelay/internal/store"
)

type SampleFile struct {
	Seed     int64    `json:"seed"`
	Gateways []Sample `json:"gateways"`
}

type Sample struct {
	GatewayID        string `json:"gateway_id"`
	DeviceIdentifier string `json:"device_identifier"`
	PrivateAddress   string `json:"private_address"`
	NATSimulator     string `json:"nat_simulator"`
	DeclaredNATType  string `json:"declared_nat_type"`
	ExpectedMismatch bool   `json:"expected_mismatch"`
}

type Environment struct {
	GOMAXPROCS       int    `json:"gomaxprocs"`
	MemoryLimitBytes int64  `json:"memory_limit_bytes"`
	TempDir          string `json:"temp_dir"`
	ControlUDP       string `json:"control_udp"`
	ControlTCP       string `json:"control_tcp"`
	HTTP             string `json:"http"`
	MappingRange     string `json:"mapping_port_range"`
	ForwardUDP       string `json:"forward_udp_sink"`
	ForwardTCP       string `json:"forward_tcp_sink"`
}

type Check struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Details string `json:"details"`
}

type MappingEvent struct {
	Round        int    `json:"round"`
	Event        string `json:"event"`
	Protocol     string `json:"protocol"`
	ExternalPort int    `json:"external_port"`
	PreviousPort int    `json:"previous_port,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type ProbeConclusion struct {
	Round         int                    `json:"round"`
	Phase         string                 `json:"phase"`
	Protocol      string                 `json:"protocol,omitempty"`
	Declared      string                 `json:"declared_nat_type"`
	Measured      string                 `json:"measured_nat_type"`
	TypeMismatch  bool                   `json:"type_mismatch"`
	PortUnchanged bool                   `json:"port_unchanged"`
	Evidence      protocol.ProbeEvidence `json:"evidence"`
}

type RecycleEvent struct {
	ID             int64  `json:"id"`
	Protocol       string `json:"protocol"`
	ExternalPort   int    `json:"external_port"`
	Reason         string `json:"reason"`
	RecycledAtUnix int64  `json:"recycled_at_unix_millis"`
	CooldownUntil  int64  `json:"cooldown_until_unix_millis"`
}

type ForwardingLatency struct {
	Protocol string  `json:"protocol"`
	Count    int     `json:"count"`
	Samples  []int64 `json:"samples_micros"`
	P95MS    float64 `json:"p95_millis"`
}

type RestartRecovery struct {
	OldUDPPort           int  `json:"old_udp_port"`
	OldTCPPort           int  `json:"old_tcp_port"`
	NewUDPPort           int  `json:"new_udp_port"`
	NewTCPPort           int  `json:"new_tcp_port"`
	UDPRecovered         bool `json:"udp_recovered"`
	TCPRecovered         bool `json:"tcp_recovered"`
	PortsPreserved       bool `json:"ports_preserved"`
	HeartbeatAccepted    bool `json:"heartbeat_accepted"`
	DuplicateMappingSeen bool `json:"duplicate_mapping_seen"`
}

type GatewayReport struct {
	GatewayID               string            `json:"gateway_id"`
	DeviceIdentifier        string            `json:"device_identifier"`
	DeclaredNATType         string            `json:"declared_nat_type"`
	ActualNATType           string            `json:"actual_nat_type"`
	MappingAllocationEvents []MappingEvent    `json:"mapping_allocation_sequence"`
	RecycleSequence         []RecycleEvent    `json:"recycle_sequence"`
	ProbeConclusions        []ProbeConclusion `json:"probe_conclusions"`
	SourceMismatchDrops     int64             `json:"source_mismatch_drops"`
	NATTypeMismatchCount    int64             `json:"nat_type_mismatch_count"`
	RecycleCount            int64             `json:"recycle_count"`
	UDPForwarding           ForwardingLatency `json:"udp_forwarding"`
	TCPForwarding           ForwardingLatency `json:"tcp_forwarding"`
	RestartRecovery         *RestartRecovery  `json:"restart_recovery,omitempty"`
	FinalMapping            map[string]int    `json:"final_mapping_ports"`
	FinalBound              map[string]string `json:"final_bound_endpoints"`
}

type RoundRequest struct {
	GatewayID string `json:"gateway_id"`
	UDPPort   int    `json:"udp_port"`
	TCPPort   int    `json:"tcp_port"`
	UDPStatus string `json:"udp_status"`
	TCPStatus string `json:"tcp_status"`
}

type RoundReport struct {
	Round      int            `json:"round"`
	Phase      string         `json:"phase"`
	StartedUTC string         `json:"started_utc"`
	EndedUTC   string         `json:"ended_utc"`
	GatewayIDs []string       `json:"gateway_ids"`
	Requests   []RoundRequest `json:"gateway_requests"`
	Notes      []string       `json:"notes"`
}

type ExpiredRestoreReport struct {
	SeededGatewayID string `json:"seeded_gateway_id"`
	RestoredActive  int    `json:"restored_active_mappings"`
	RecycleCount    int64  `json:"recycle_count"`
	Reason          string `json:"reason"`
	Passed          bool   `json:"passed"`
}

type ForwardedToSink struct {
	UDPPackets int64 `json:"udp_packets"`
	TCPPackets int64 `json:"tcp_packets"`
}

type Summary struct {
	Checks               []Check `json:"checks"`
	AllPassed            bool    `json:"all_passed"`
	P95ForwardingMS      float64 `json:"p95_forwarding_millis"`
	MappingCount         int     `json:"active_mapping_count"`
	DistinctMappingPorts int     `json:"distinct_mapping_ports"`
}

type Report struct {
	SchemaVersion   string               `json:"schema_version"`
	Seed            int64                `json:"seed"`
	GeneratedAtUTC  string               `json:"generated_at_utc"`
	Environment     Environment          `json:"environment"`
	Samples         []Sample             `json:"gateway_samples"`
	Rounds          []RoundReport        `json:"rounds"`
	Gateways        []GatewayReport      `json:"gateways"`
	ExpiredRestore  ExpiredRestoreReport `json:"expired_restore"`
	ForwardedToSink ForwardedToSink      `json:"forwarded_to_sink"`
	Prometheus      string               `json:"prometheus_metrics"`
	Summary         Summary              `json:"summary"`
}

type runner struct {
	report   *Report
	cfg      relay.Config
	r        *relay.Relay
	samples  []Sample
	full     *natsim.Simulator
	restrict *natsim.Simulator
	clients  map[string]*gateway.Client
	reports  map[string]*GatewayReport
	sinks    *forwardingSinks
}

func Run(ctx context.Context, samplesPath, outPath, workDir string, maxProcs int, memLimit int64) (*Report, error) {
	raw, err := os.ReadFile(samplesPath)
	if err != nil {
		return nil, err
	}
	var sf SampleFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return nil, err
	}
	if len(sf.Gateways) != 12 {
		return nil, fmt.Errorf("sample file contains %d gateways, want 12", len(sf.Gateways))
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(workDir + "/relay.db" + suffix)
	}
	cfg, err := scenarioConfig(workDir + "/relay.db")
	if err != nil {
		return nil, err
	}
	sinks, err := newForwardingSinks()
	if err != nil {
		return nil, err
	}
	cfg.ForwardUDP, cfg.ForwardTCP = sinks.UDPAddr(), sinks.TCPAddr()
	rep := &Report{
		SchemaVersion:  "park-relay-validation/v1",
		Seed:           sf.Seed,
		GeneratedAtUTC: time.Now().UTC().Format(time.RFC3339Nano),
		Environment: Environment{
			GOMAXPROCS: maxProcs, MemoryLimitBytes: memLimit, TempDir: workDir,
			ControlUDP: cfg.ControlUDP, ControlTCP: cfg.ControlTCP, HTTP: cfg.HTTP,
			MappingRange: fmt.Sprintf("%d-%d", cfg.PortMin, cfg.PortMax),
			ForwardUDP:   sinks.UDPAddr(), ForwardTCP: sinks.TCPAddr(),
		},
		Samples: sf.Gateways,
	}
	run := &runner{report: rep, cfg: cfg, samples: sf.Gateways, clients: make(map[string]*gateway.Client), reports: make(map[string]*GatewayReport), sinks: sinks}
	err = run.execute(ctx)
	rep.ForwardedToSink = sinks.Snapshot()
	sinks.Close()
	rep.GeneratedAtUTC = time.Now().UTC().Format(time.RFC3339Nano)
	run.finalize()
	if writeErr := writeReport(outPath, rep); writeErr != nil && err == nil {
		err = writeErr
	}
	return rep, err
}

func scenarioConfig(dbPath string) (relay.Config, error) {
	base, err := randomPortBase(24000, 60000, 128)
	if err != nil {
		return relay.Config{}, err
	}
	udpAddr, err := freeAddr("udp")
	if err != nil {
		return relay.Config{}, err
	}
	tcpAddr, err := freeAddr("tcp")
	if err != nil {
		return relay.Config{}, err
	}
	httpAddr, err := freeAddr("tcp")
	if err != nil {
		return relay.Config{}, err
	}
	cfg := relay.DefaultConfig()
	cfg.ControlUDP, cfg.ControlTCP, cfg.HTTP = udpAddr, tcpAddr, httpAddr
	cfg.DBPath = dbPath
	cfg.PortMin, cfg.PortMax = base, base+127
	cfg.LeaseWindow = 3500 * time.Millisecond
	cfg.RecoveryCooldown = 1500 * time.Millisecond
	cfg.InFlightDrain = 180 * time.Millisecond
	cfg.ProbeTimeout = 90 * time.Millisecond
	cfg.ProbeInterval = 30 * time.Second
	cfg.SweepInterval = 50 * time.Millisecond
	return cfg, nil
}

func randomPortBase(min, max, size int) (int, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	base := min + int(binary.LittleEndian.Uint64(b[:])%uint64(max-min-size))
	for attempt := 0; attempt < 100; attempt++ {
		if portRangeFree(base, size) {
			return base, nil
		}
		base += size
		if base+size >= max {
			base = min
		}
	}
	return 0, fmt.Errorf("no free %d-port range", size)
}

func portRangeFree(base, size int) bool {
	var closers []io.Closer
	ok := true
	for p := base; p < base+size; p++ {
		u, err := net.ListenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			ok = false
			break
		}
		closers = append(closers, u)
		l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			ok = false
			break
		}
		closers = append(closers, l)
	}
	for _, c := range closers {
		_ = c.Close()
	}
	return ok
}

func freeAddr(proto string) (string, error) {
	if proto == "udp" {
		c, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		addr := c.LocalAddr().String()
		c.Close()
		return addr, nil
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := l.Addr().String()
	l.Close()
	return addr, nil
}

func (r *runner) execute(ctx context.Context) error {
	r.full = natsim.NewSimulator(protocol.NATFullCone)
	r.restrict = natsim.NewSimulator(protocol.NATPortRestricted)
	defer r.full.Close()
	defer r.restrict.Close()
	defer r.sinks.Close()
	r.initReports()
	for _, sample := range r.samples {
		sim := r.restrict
		if protocol.NormalizeNATType(sample.NATSimulator) == protocol.NATFullCone {
			sim = r.full
		}
		ep, err := sim.NewGateway(sample.GatewayID)
		if err != nil {
			return err
		}
		ep.SetBehavior(sample.NATSimulator)
		r.clients[sample.GatewayID] = gateway.New(sample.GatewayID, sample.DeclaredNATType,
			sample.PrivateAddress, ep, r.cfg.ControlUDP, r.cfg.ControlTCP)
	}
	defer func() {
		for _, g := range r.clients {
			g.Stop()
		}
	}()
	relayInstance, err := relay.Start(ctx, r.cfg)
	if err != nil {
		return err
	}
	r.r = relayInstance
	defer func() {
		if r.r != nil {
			_ = r.r.Stop()
		}
	}()

	if err := r.roundInitial(ctx); err != nil {
		return err
	}
	if err := r.roundFaults(ctx); err != nil {
		return err
	}
	if err := r.roundDisconnect(ctx); err != nil {
		return err
	}
	if err := r.roundRestart(ctx); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	forwarded := r.sinks.Snapshot()
	r.report.ForwardedToSink = forwarded
	r.addCheck("valid_source_payloads_reach_forwarding_sink", forwarded.UDPPackets >= 48 && forwarded.TCPPackets >= 48,
		fmt.Sprintf("udp_packets=%d tcp_packets=%d", forwarded.UDPPackets, forwarded.TCPPackets))
	r.report.Prometheus = r.metrics(ctx)
	return r.checkExpiredRestore(ctx)
}

func (r *runner) initReports() {
	for _, s := range r.samples {
		r.reports[s.GatewayID] = &GatewayReport{
			GatewayID: s.GatewayID, DeviceIdentifier: s.DeviceIdentifier,
			DeclaredNATType: s.DeclaredNATType, ActualNATType: s.NATSimulator,
			MappingAllocationEvents: []MappingEvent{}, RecycleSequence: []RecycleEvent{},
			ProbeConclusions: []ProbeConclusion{},
			UDPForwarding:    ForwardingLatency{Protocol: "udp", Samples: []int64{}},
			TCPForwarding:    ForwardingLatency{Protocol: "tcp", Samples: []int64{}},
		}
	}
}

func (r *runner) newRound(number int, phase string) (*RoundReport, func()) {
	rr := &RoundReport{Round: number, Phase: phase, StartedUTC: time.Now().UTC().Format(time.RFC3339Nano), GatewayIDs: []string{}, Requests: []RoundRequest{}, Notes: []string{}}
	for _, s := range r.samples {
		rr.GatewayIDs = append(rr.GatewayIDs, s.GatewayID)
	}
	r.report.Rounds = append(r.report.Rounds, *rr)
	return &r.report.Rounds[len(r.report.Rounds)-1], func() {
		r.report.Rounds[len(r.report.Rounds)-1].EndedUTC = time.Now().UTC().Format(time.RFC3339Nano)
	}
}

func (r *runner) roundInitial(ctx context.Context) error {
	round, done := r.newRound(1, "initial_mapping_probe_and_forwarding")
	defer func() {
		r.populateRoundRequests(round)
		done()
	}()
	var ports = make(map[int]string)
	for _, sample := range r.samples {
		g := r.clients[sample.GatewayID]
		u, err := g.RequestUDP(false)
		if err != nil || !u.OK {
			return fmt.Errorf("%s UDP request: %+v %w", g.ID, u, err)
		}
		tcp, err := g.RequestTCP(false)
		if err != nil || !tcp.OK {
			return fmt.Errorf("%s TCP request: %+v %w", g.ID, tcp, err)
		}
		if err := g.BindTCPData(); err != nil {
			return fmt.Errorf("%s TCP data bind: %w", g.ID, err)
		}
		for _, item := range []protocol.Response{u, tcp} {
			if old, exists := ports[item.ExternalPort]; exists {
				return fmt.Errorf("port %d assigned to both %s and %s", item.ExternalPort, old, g.ID)
			}
			ports[item.ExternalPort] = g.ID
			r.addMappingEvent(g.ID, MappingEvent{Round: 1, Event: "allocated", Protocol: item.Protocol, ExternalPort: item.ExternalPort})
			r.addProbe(g.ID, 1, "initial", item.Protocol, item)
		}
		round.Requests = append(round.Requests, RoundRequest{
			GatewayID: g.ID, UDPPort: u.ExternalPort, TCPPort: tcp.ExternalPort, UDPStatus: "reused=false", TCPStatus: "reused=false",
		})
	}
	r.addCheck("all_12_gateways_have_udp_and_tcp_mappings", len(ports) == 24, fmt.Sprintf("%d active protocol mappings", len(ports)))

	// Duplicate requests must return the existing mapping on both transports.
	idempotent := true
	for _, sample := range r.samples {
		g := r.clients[sample.GatewayID]
		u0, _ := g.Mapping(protocol.ProtocolUDP)
		t0, _ := g.Mapping(protocol.ProtocolTCP)
		u, err := g.RequestUDP(false)
		tcp, err2 := g.RequestTCP(false)
		if err != nil || err2 != nil || !u.Reused || !tcp.Reused || u.ExternalPort != u0.ExternalPort || tcp.ExternalPort != t0.ExternalPort {
			idempotent = false
		}
	}
	r.addCheck("duplicate_requests_are_idempotent", idempotent, "same gateway+protocol returns the existing port")

	for roundIndex := 0; roundIndex < 3; roundIndex++ {
		for _, sample := range r.samples {
			g := r.clients[sample.GatewayID]
			du, err := g.SendUDPData(fmt.Sprintf("initial-udp-%d", roundIndex))
			if err != nil {
				return err
			}
			dt, err := g.SendTCPData(fmt.Sprintf("initial-tcp-%d", roundIndex))
			if err != nil {
				return err
			}
			r.addLatency(g.ID, "udp", du)
			r.addLatency(g.ID, "tcp", dt)
		}
	}
	active, queryable := r.queryAll(ctx)
	r.addCheck("all_mappings_queryable", active == 24 && queryable == 12, fmt.Sprintf("active=%d queryable_gateways=%d", active, queryable))
	st3, err := r.status(ctx, "gw-003")
	if err != nil {
		return err
	}
	initialMismatch := len(st3.Gateways) == 1
	if initialMismatch {
		for _, m := range st3.Gateways[0].Mappings {
			if !m.TypeMismatch || len(m.ProbeEvidence.ProbeError) > 0 {
				initialMismatch = false
			}
		}
	}
	r.addCheck("declared_type_mismatch_is_annotated_with_evidence", initialMismatch,
		"gw-003 declares port_restricted while the two-source probe measures full_cone")
	return nil
}

func (r *runner) roundFaults(ctx context.Context) error {
	round, done := r.newRound(2, "nat_type_change_rebinding_and_source_forgery")
	defer func() {
		r.populateRoundRequests(round)
		round.Notes = append(round.Notes, "gw-001 full_cone->port_restricted", "gw-008 port_restricted->full_cone", "gw-007 NAT endpoint rebound", "forged UDP/TCP sources injected")
		done()
	}()

	g1 := r.clients["gw-001"]
	oldU1, _ := g1.Mapping(protocol.ProtocolUDP)
	g1.NAT.SetBehavior(protocol.NATPortRestricted)
	u1, err := g1.RequestUDP(true)
	if err != nil {
		return err
	}
	r.addProbe("gw-001", 2, "injected_full_to_port_restricted", "udp", u1)
	r.addCheck("type_change_is_measured_not_rejected", u1.OK && u1.MeasuredNATType == protocol.NATPortRestricted &&
		u1.TypeMismatch && u1.ExternalPort == oldU1.ExternalPort, fmt.Sprintf("measured=%s mismatch=%v port=%d", u1.MeasuredNATType, u1.TypeMismatch, u1.ExternalPort))

	g8 := r.clients["gw-008"]
	oldU8, _ := g8.Mapping(protocol.ProtocolUDP)
	g8.NAT.SetBehavior(protocol.NATFullCone)
	u8, err := g8.RequestUDP(true)
	if err != nil {
		return err
	}
	r.addProbe("gw-008", 2, "injected_port_restricted_to_full", "udp", u8)
	r.addCheck("reverse_type_change_is_measured", u8.OK && u8.MeasuredNATType == protocol.NATFullCone &&
		u8.TypeMismatch && u8.ExternalPort == oldU8.ExternalPort, fmt.Sprintf("measured=%s mismatch=%v port=%d", u8.MeasuredNATType, u8.TypeMismatch, u8.ExternalPort))

	g7 := r.clients["gw-007"]
	oldPublic := g7.NAT.PublicUDPAddr()
	oldU7, _ := g7.Mapping(protocol.ProtocolUDP)
	if err := g7.RotateNATEndpoint(); err != nil {
		return err
	}
	hu, ht, err := g7.HeartbeatBoth(true)
	if err != nil {
		return err
	}
	if err := g7.BindTCPData(); err != nil {
		return err
	}
	newPublic := g7.NAT.PublicUDPAddr()
	r.addProbe("gw-007", 2, "nat_endpoint_rebinding", "udp", hu)
	r.addCheck("nat_rebinding_updates_binding", hu.OK && hu.BoundIP+":"+strconv.Itoa(hu.BoundPort) == newPublic &&
		newPublic != oldPublic && hu.ExternalPort == oldU7.ExternalPort && ht.OK,
		fmt.Sprintf("old_public=%s new_public=%s mapping_port=%d", oldPublic, newPublic, hu.ExternalPort))
	if _, err := g7.SendUDPData("after-rebind"); err != nil {
		return err
	}
	beforeOldSource, err := r.status(ctx, g7.ID)
	if err != nil {
		return err
	}
	oldSource, err := net.ListenPacket("udp4", oldPublic)
	if err != nil {
		return fmt.Errorf("bind old NAT source %s for stale-source injection: %w", oldPublic, err)
	}
	_, _ = oldSource.WriteTo([]byte(`{"type":"data","gateway_id":"gw-007"}`),
		&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: oldU7.ExternalPort})
	_ = oldSource.Close()
	time.Sleep(100 * time.Millisecond)
	afterOldSource, err := r.status(ctx, g7.ID)
	if err != nil {
		return err
	}
	if len(beforeOldSource.Gateways) != 1 || len(afterOldSource.Gateways) != 1 {
		return fmt.Errorf("gw-007 status unavailable during stale-source check")
	}
	oldDropped := len(beforeOldSource.Gateways) == 1 && len(afterOldSource.Gateways) == 1 &&
		afterOldSource.Gateways[0].Counters.SourceMismatchDrops > beforeOldSource.Gateways[0].Counters.SourceMismatchDrops
	r.addCheck("old_source_rejected_after_nat_rebinding", oldDropped,
		fmt.Sprintf("stale_source=%s drops_before=%d drops_after=%d", oldPublic,
			beforeOldSource.Gateways[0].Counters.SourceMismatchDrops, afterOldSource.Gateways[0].Counters.SourceMismatchDrops))

	spoof, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer spoof.Close()
	for _, sample := range r.samples {
		g := r.clients[sample.GatewayID]
		m, _ := g.Mapping(protocol.ProtocolUDP)
		_, _ = spoof.WriteTo([]byte(`{"type":"data","gateway_id":"forged"}`),
			&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: m.ExternalPort})
		t, _ := g.Mapping(protocol.ProtocolTCP)
		conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(t.ExternalPort)), 200*time.Millisecond)
		if err == nil {
			_, _ = conn.Write([]byte(`{"type":"data","gateway_id":"forged"}` + "\n"))
			_ = conn.Close()
		}
	}
	time.Sleep(150 * time.Millisecond)
	allDropped := true
	for _, sample := range r.samples {
		st, err := r.status(ctx, sample.GatewayID)
		if err != nil || len(st.Gateways) != 1 || st.Gateways[0].Counters.SourceMismatchDrops < 1 {
			allDropped = false
		}
	}
	r.addCheck("forged_sources_are_dropped_and_counted", allDropped, "each gateway has at least one source-mismatch increment")
	active, queryable := r.queryAll(ctx)
	r.addCheck("post_fault_all_mappings_remain_queryable", active == 24 && queryable == 12, fmt.Sprintf("active=%d queryable=%d", active, queryable))
	return nil
}

func (r *runner) roundDisconnect(ctx context.Context) error {
	round, done := r.newRound(3, "disconnect_recycle_cooldown_and_reallocation")
	defer func() {
		r.populateRoundRequests(round)
		round.Notes = append(round.Notes, "gw-004 and gw-011 stopped heartbeats", "both mappings per gateway recycled after lease", "reconnected during cooldown and received new ports")
		done()
	}()
	disconnected := map[string]bool{"gw-004": true, "gw-011": true}
	oldPorts := make(map[string][2]int)
	for id := range disconnected {
		u, _ := r.clients[id].Mapping(protocol.ProtocolUDP)
		t, _ := r.clients[id].Mapping(protocol.ProtocolTCP)
		oldPorts[id] = [2]int{u.ExternalPort, t.ExternalPort}
	}
	waitUntil := time.Now().Add(r.cfg.LeaseWindow + r.cfg.InFlightDrain + 500*time.Millisecond)
	for time.Now().Before(waitUntil) {
		for _, sample := range r.samples {
			if disconnected[sample.GatewayID] {
				continue
			}
			_, _, _ = r.clients[sample.GatewayID].HeartbeatBoth(false)
		}
		time.Sleep(300 * time.Millisecond)
	}
	recycled := true
	for id := range disconnected {
		st, err := r.status(ctx, id)
		if err != nil || len(st.Gateways) != 1 || len(st.Gateways[0].Mappings) != 0 ||
			st.Gateways[0].Counters.RecycleCount < 2 || st.Gateways[0].LastRecycle == nil ||
			st.Gateways[0].LastRecycle.Reason != "keepalive_timeout" {
			recycled = false
		}
	}
	r.addCheck("disconnected_gateways_recycle_both_mappings", recycled, "UDP and TCP reclaimed after keepalive timeout")

	cooldownHeld := true
	for id := range disconnected {
		g := r.clients[id]
		u, err := g.RequestUDP(false)
		if err != nil {
			return err
		}
		t, err := g.RequestTCP(false)
		if err != nil {
			return err
		}
		if err := g.BindTCPData(); err != nil {
			return err
		}
		if u.ExternalPort == oldPorts[id][0] || t.ExternalPort == oldPorts[id][1] {
			cooldownHeld = false
		}
		r.addMappingEvent(id, MappingEvent{
			Round: 3, Event: "reallocated_after_recycle", Protocol: "udp", ExternalPort: u.ExternalPort,
			PreviousPort: oldPorts[id][0], Reason: "keepalive_timeout",
		})
		r.addMappingEvent(id, MappingEvent{
			Round: 3, Event: "reallocated_after_recycle", Protocol: "tcp", ExternalPort: t.ExternalPort,
			PreviousPort: oldPorts[id][1], Reason: "keepalive_timeout",
		})
		r.addProbe(id, 3, "after_reallocation", "udp", u)
	}
	r.addCheck("released_ports_not_reused_during_cooldown", cooldownHeld, "reallocated ports differ from the released ports")
	for _, sample := range r.samples {
		recs, err := r.recycles(ctx, sample.GatewayID)
		if err == nil {
			r.reports[sample.GatewayID].RecycleSequence = recs
		}
	}
	active, queryable := r.queryAll(ctx)
	r.addCheck("all_gateways_active_after_reconnect", active == 24 && queryable == 12, fmt.Sprintf("active=%d queryable=%d", active, queryable))
	return nil
}

func (r *runner) roundRestart(ctx context.Context) error {
	round, done := r.newRound(4, "relay_restart_recovery")
	defer func() {
		r.populateRoundRequests(round)
		round.Notes = append(round.Notes, "relay process stopped and restarted on the same control/HTTP ports", "all 12 gateways resumed with original UDP/TCP mappings")
		done()
	}()
	oldPorts := make(map[string][2]int)
	for _, sample := range r.samples {
		_, _, _ = r.clients[sample.GatewayID].HeartbeatBoth(false)
		u, _ := r.clients[sample.GatewayID].Mapping(protocol.ProtocolUDP)
		t, _ := r.clients[sample.GatewayID].Mapping(protocol.ProtocolTCP)
		oldPorts[sample.GatewayID] = [2]int{u.ExternalPort, t.ExternalPort}
	}
	if err := r.r.Stop(); err != nil {
		return err
	}
	time.Sleep(120 * time.Millisecond)
	restarted, err := relay.Start(ctx, r.cfg)
	if err != nil {
		return err
	}
	r.r = restarted

	allPreserved := true
	for _, sample := range r.samples {
		g := r.clients[sample.GatewayID]
		u, t, err := g.HeartbeatBoth(true)
		heartbeatOK := err == nil && u.OK && t.OK
		preserved := heartbeatOK && u.ExternalPort == oldPorts[sample.GatewayID][0] &&
			t.ExternalPort == oldPorts[sample.GatewayID][1]
		if err := g.BindTCPData(); err != nil {
			return err
		}
		rr := &RestartRecovery{
			OldUDPPort: oldPorts[sample.GatewayID][0], OldTCPPort: oldPorts[sample.GatewayID][1],
			NewUDPPort: u.ExternalPort, NewTCPPort: t.ExternalPort, UDPRecovered: u.Recovered,
			TCPRecovered: t.Recovered, PortsPreserved: preserved, HeartbeatAccepted: heartbeatOK,
			DuplicateMappingSeen: false,
		}
		r.reports[sample.GatewayID].RestartRecovery = rr
		if !preserved || !u.Recovered || !t.Recovered {
			allPreserved = false
		}
		r.addProbe(sample.GatewayID, 4, "after_relay_restart", "udp", u)
		r.addMappingEvent(sample.GatewayID, MappingEvent{Round: 4, Event: "recovered", Protocol: "udp", ExternalPort: u.ExternalPort})
		r.addMappingEvent(sample.GatewayID, MappingEvent{Round: 4, Event: "recovered", Protocol: "tcp", ExternalPort: t.ExternalPort})
	}
	active, queryable := r.queryAll(ctx)
	r.addCheck("restart_restores_original_ports_without_duplicates", allPreserved && active == 24 && queryable == 12,
		fmt.Sprintf("ports_preserved=%v active=%d queryable=%d", allPreserved, active, queryable))
	for _, sample := range r.samples {
		g := r.clients[sample.GatewayID]
		du, err := g.SendUDPData("post-restart")
		if err != nil {
			return err
		}
		dt, err := g.SendTCPData("post-restart")
		if err != nil {
			return err
		}
		r.addLatency(g.ID, "udp", du)
		r.addLatency(g.ID, "tcp", dt)
	}
	return nil
}

func (r *runner) checkExpiredRestore(ctx context.Context) error {
	// Stop the main relay first so the validation ports are free for this
	// isolated recovery check.
	if r.r != nil {
		_ = r.r.Stop()
		r.r = nil
	}
	expiredDB := filepath.Join(r.report.Environment.TempDir, "expired.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(expiredDB + suffix)
	}
	st, err := store.Open(expiredDB)
	if err != nil {
		return err
	}
	now := time.Now()
	old := now.Add(-5 * time.Second)
	for _, proto := range []string{protocol.ProtocolUDP, protocol.ProtocolTCP} {
		port := r.cfg.PortMin
		if proto == protocol.ProtocolTCP {
			port++
		}
		if err := st.UpsertMapping(ctx, store.Mapping{
			GatewayID: "gw-expired", Protocol: proto, ExternalPort: port,
			BoundIP: "127.0.0.1", BoundPort: 40000, DeclaredNATType: protocol.NATFullCone,
			MeasuredNATType: protocol.NATFullCone, ProbeEvidence: `{"method":"seeded-expired"}`,
			MappingToken: "expired-token", CreatedAt: old, LastHeartbeat: old, LeaseExpires: old,
		}); err != nil {
			st.Close()
			return err
		}
	}
	_ = st.Close()
	cfg := r.cfg
	cfg.DBPath = expiredDB
	expiredRelay, err := relay.Start(ctx, cfg)
	if err != nil {
		return err
	}
	defer expiredRelay.Stop()
	status, err := r.statusAt(ctx, expiredRelay.HttpAddress(), "gw-expired")
	if err != nil {
		return err
	}
	recs, err := r.recyclesAt(ctx, expiredRelay.HttpAddress(), "gw-expired")
	if err != nil {
		return err
	}
	count := int64(0)
	reason := ""
	if len(status.Gateways) == 1 {
		count = status.Gateways[0].Counters.RecycleCount
		if status.Gateways[0].LastRecycle != nil {
			reason = status.Gateways[0].LastRecycle.Reason
		}
	}
	restored := 0
	if len(status.Gateways) == 1 {
		restored = len(status.Gateways[0].Mappings)
	}
	passed := restored == 0 && count == 2 && reason == "lease_expired_during_restore" && len(recs) == 2
	r.report.ExpiredRestore = ExpiredRestoreReport{
		SeededGatewayID: "gw-expired", RestoredActive: restored, RecycleCount: count, Reason: reason, Passed: passed,
	}
	r.addCheck("expired_mappings_are_not_restored", passed, fmt.Sprintf("active=%d recycles=%d reason=%s", restored, count, reason))
	return nil
}

func (r *runner) status(ctx context.Context, id string) (relay.StatusResponse, error) {
	return r.statusAt(ctx, r.r.HttpAddress(), id)
}

func (r *runner) statusAt(ctx context.Context, addr, id string) (relay.StatusResponse, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/v1/status?gateway_id="+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return relay.StatusResponse{}, err
	}
	defer resp.Body.Close()
	var out relay.StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

func (r *runner) recycles(ctx context.Context, id string) ([]RecycleEvent, error) {
	return r.recyclesAt(ctx, r.r.HttpAddress(), id)
}

func (r *runner) recyclesAt(ctx context.Context, addr, id string) ([]RecycleEvent, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/v1/recycles?gateway_id="+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload struct {
		Recycles []RecycleEvent `json:"recycles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Recycles, nil
}

func (r *runner) metrics(ctx context.Context) string {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+r.r.HttpAddress()+"/metrics", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	text := string(b)
	ok := true
	for _, s := range r.samples {
		for _, needle := range []string{
			fmt.Sprintf(`relay_gateway_active_mappings{gateway=%q}`, s.GatewayID),
			fmt.Sprintf(`relay_mapping_port{gateway=%q,protocol="udp"}`, s.GatewayID),
			fmt.Sprintf(`relay_mapping_port{gateway=%q,protocol="tcp"}`, s.GatewayID),
			fmt.Sprintf(`relay_mapping_keepalive_remaining_seconds{gateway=%q,protocol="udp"}`, s.GatewayID),
			fmt.Sprintf(`relay_gateway_source_mismatch_drops_total{gateway=%q}`, s.GatewayID),
			fmt.Sprintf(`relay_gateway_nat_type_mismatch_total{gateway=%q}`, s.GatewayID),
			fmt.Sprintf(`relay_gateway_recycles_total{gateway=%q}`, s.GatewayID),
		} {
			if !strings.Contains(text, needle) {
				ok = false
			}
		}
	}
	r.addCheck("prometheus_labels_and_metrics_stable", ok, "all 12 gateways expose required labels")
	return text
}

func (r *runner) queryAll(ctx context.Context) (int, int) {
	active, queryable := 0, 0
	for _, sample := range r.samples {
		st, err := r.status(ctx, sample.GatewayID)
		if err != nil || len(st.Gateways) != 1 {
			continue
		}
		queryable++
		active += st.Gateways[0].ActiveMappings
		gr := r.reports[sample.GatewayID]
		gr.SourceMismatchDrops = st.Gateways[0].Counters.SourceMismatchDrops
		gr.NATTypeMismatchCount = st.Gateways[0].Counters.NATTypeMismatchCount
		gr.RecycleCount = st.Gateways[0].Counters.RecycleCount
		if gr.FinalMapping == nil {
			gr.FinalMapping = make(map[string]int)
			gr.FinalBound = make(map[string]string)
		}
		for _, m := range st.Gateways[0].Mappings {
			gr.FinalMapping[m.Protocol] = m.ExternalPort
			gr.FinalBound[m.Protocol] = net.JoinHostPort(m.BoundIP, strconv.Itoa(m.BoundPort))
		}
	}
	return active, queryable
}

func (r *runner) addMappingEvent(id string, ev MappingEvent) {
	gr := r.reports[id]
	gr.MappingAllocationEvents = append(gr.MappingAllocationEvents, ev)
}

func (r *runner) addProbe(id string, round int, phase, proto string, resp protocol.Response) {
	if resp.ProbeEvidence == nil {
		return
	}
	gr := r.reports[id]
	gr.ProbeConclusions = append(gr.ProbeConclusions, ProbeConclusion{
		Round: round, Phase: phase, Protocol: proto, Declared: resp.DeclaredNATType,
		Measured: resp.MeasuredNATType, TypeMismatch: resp.TypeMismatch,
		PortUnchanged: true, Evidence: *resp.ProbeEvidence,
	})
}

func (r *runner) addLatency(id, proto string, d time.Duration) {
	gr := r.reports[id]
	value := d.Microseconds()
	if proto == "udp" {
		gr.UDPForwarding.Samples = append(gr.UDPForwarding.Samples, value)
	} else {
		gr.TCPForwarding.Samples = append(gr.TCPForwarding.Samples, value)
	}
}

func (r *runner) addCheck(name string, passed bool, details string) {
	r.report.Summary.Checks = append(r.report.Summary.Checks, Check{Name: name, Passed: passed, Details: details})
}

func (r *runner) populateRoundRequests(round *RoundReport) {
	round.Requests = round.Requests[:0]
	for _, sample := range r.samples {
		g := r.clients[sample.GatewayID]
		u, uok := g.Mapping(protocol.ProtocolUDP)
		t, tok := g.Mapping(protocol.ProtocolTCP)
		req := RoundRequest{GatewayID: sample.GatewayID, UDPStatus: "active", TCPStatus: "active"}
		if uok {
			req.UDPPort = u.ExternalPort
		} else {
			req.UDPStatus = "absent"
		}
		if tok {
			req.TCPPort = t.ExternalPort
		} else {
			req.TCPStatus = "absent"
		}
		round.Requests = append(round.Requests, req)
	}
}

func (r *runner) finalize() {
	ids := make([]string, 0, len(r.reports))
	for id := range r.reports {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var allLatencies []int64
	for _, id := range ids {
		gr := r.reports[id]
		gr.UDPForwarding.Count = len(gr.UDPForwarding.Samples)
		gr.UDPForwarding.P95MS = percentile95(gr.UDPForwarding.Samples)
		gr.TCPForwarding.Count = len(gr.TCPForwarding.Samples)
		gr.TCPForwarding.P95MS = percentile95(gr.TCPForwarding.Samples)
		allLatencies = append(allLatencies, gr.UDPForwarding.Samples...)
		allLatencies = append(allLatencies, gr.TCPForwarding.Samples...)
		r.report.Gateways = append(r.report.Gateways, *gr)
	}
	r.report.Summary.P95ForwardingMS = percentile95(allLatencies)
	r.report.Summary.MappingCount = len(allLatencies) // overwritten below from final mapping
	count, distinct := 0, make(map[int]bool)
	for _, gr := range r.report.Gateways {
		for _, p := range gr.FinalMapping {
			count++
			distinct[p] = true
		}
	}
	r.report.Summary.MappingCount = count
	r.report.Summary.DistinctMappingPorts = len(distinct)
	r.addCheck("forwarding_p95_within_20ms", r.report.Summary.P95ForwardingMS <= 20.0,
		fmt.Sprintf("P95 %.3f ms across %d samples", r.report.Summary.P95ForwardingMS, len(allLatencies)))
	allPassed := len(r.report.Summary.Checks) > 0
	for _, c := range r.report.Summary.Checks {
		if !c.Passed {
			allPassed = false
		}
	}
	r.report.Summary.AllPassed = allPassed
}

func percentile95(values []int64) float64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]int64(nil), values...)
	sort.Slice(copyValues, func(i, j int) bool { return copyValues[i] < copyValues[j] })
	idx := (len(copyValues)*95 + 99) / 100
	if idx < 1 {
		idx = 1
	}
	return float64(copyValues[idx-1]) / 1000.0
}

func writeReport(path string, report *Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}
