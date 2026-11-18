package relay_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"parkrelay/internal/gateway"
	"parkrelay/internal/natsim"
	"parkrelay/internal/protocol"
	"parkrelay/internal/relay"
)

type fixture struct {
	cfg      relay.Config
	r        *relay.Relay
	full     *natsim.Simulator
	restrict *natsim.Simulator
	gateways []*gateway.Client
}

func findPortRange(t *testing.T, size int) (int, int) {
	t.Helper()
	rng := rand.New(rand.NewSource(12345))
	for attempt := 0; attempt < 100; attempt++ {
		base := 22000 + rng.Intn(30000-size)
		ok := true
		var listeners []io.Closer
		for p := base; p < base+size; p++ {
			u, err := net.ListenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", p))
			if err != nil {
				ok = false
				break
			}
			listeners = append(listeners, u)
			l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", p))
			if err != nil {
				u.Close()
				ok = false
				break
			}
			listeners = append(listeners, l)
		}
		for _, c := range listeners {
			_ = c.Close()
		}
		if ok {
			return base, base + size - 1
		}
	}
	t.Fatal("could not find free mapping port range")
	return 0, 0
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	cfg := relay.DefaultConfig()
	cfg.ControlUDP = "127.0.0.1:0"
	cfg.ControlTCP = "127.0.0.1:0"
	cfg.HTTP = "127.0.0.1:0"
	cfg.PortMin, cfg.PortMax = findPortRange(t, 64)
	cfg.DBPath = t.TempDir() + "/relay.db"
	cfg.LeaseWindow = 2500 * time.Millisecond
	cfg.RecoveryCooldown = 600 * time.Millisecond
	cfg.InFlightDrain = 120 * time.Millisecond
	cfg.ProbeTimeout = 100 * time.Millisecond
	cfg.ProbeInterval = 3 * time.Second
	cfg.SweepInterval = 40 * time.Millisecond
	r, err := relay.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{cfg: cfg, r: r, full: natsim.NewSimulator(protocol.NATFullCone), restrict: natsim.NewSimulator(protocol.NATPortRestricted)}
	t.Cleanup(func() {
		for _, g := range f.gateways {
			g.Stop()
		}
		f.full.Close()
		f.restrict.Close()
		_ = f.r.Stop()
	})
	for i := 1; i <= 12; i++ {
		sim := f.restrict
		declared := protocol.NATPortRestricted
		if i%2 == 1 {
			sim = f.full
			declared = protocol.NATFullCone
		}
		if i == 4 {
			// Deliberately lie: this full-cone gateway declares restricted.
			sim = f.full
			declared = protocol.NATPortRestricted
		}
		ep, err := sim.NewGateway(fmt.Sprintf("gw-%03d", i))
		if err != nil {
			t.Fatal(err)
		}
		g := gateway.New(fmt.Sprintf("gw-%03d", i), declared,
			fmt.Sprintf("10.30.%d.%d:9000", i/200, i%200+1), ep,
			r.ControlUDPAddress(), r.ControlTCPAddress())
		f.gateways = append(f.gateways, g)
	}
	return f
}

func (f *fixture) requestAll(t *testing.T) {
	t.Helper()
	for _, g := range f.gateways {
		if _, err := g.RequestUDP(false); err != nil {
			t.Fatalf("%s udp: %v", g.ID, err)
		}
		if _, err := g.RequestTCP(false); err != nil {
			t.Fatalf("%s tcp: %v", g.ID, err)
		}
		if err := g.BindTCPData(); err != nil {
			t.Fatalf("%s tcp bind: %v", g.ID, err)
		}
	}
}

func (f *fixture) status(t *testing.T, gatewayID string) relay.StatusResponse {
	t.Helper()
	resp, err := http.Get("http://" + f.r.HttpAddress() + "/v1/status?gateway_id=" + gatewayID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out relay.StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMappingLifecycleProbeSourceAndRecovery(t *testing.T) {
	f := newFixture(t)
	f.requestAll(t)

	ports := make(map[int]string)
	for _, g := range f.gateways {
		u, _ := g.Mapping(protocol.ProtocolUDP)
		tcp, _ := g.Mapping(protocol.ProtocolTCP)
		if u.ExternalPort == tcp.ExternalPort {
			t.Fatalf("%s shares a UDP/TCP port %d", g.ID, u.ExternalPort)
		}
		for _, p := range []int{u.ExternalPort, tcp.ExternalPort} {
			if old, ok := ports[p]; ok {
				t.Fatalf("port %d assigned to %s and %s", p, old, g.ID)
			}
			ports[p] = g.ID
		}
	}
	if len(ports) != 24 {
		t.Fatalf("got %d distinct ports, want 24", len(ports))
	}

	// Duplicate requests are idempotent for each protocol.
	for _, g := range f.gateways {
		beforeU, _ := g.Mapping(protocol.ProtocolUDP)
		beforeT, _ := g.Mapping(protocol.ProtocolTCP)
		u, err := g.RequestUDP(false)
		if err != nil || !u.Reused || u.ExternalPort != beforeU.ExternalPort {
			t.Fatalf("%s duplicate UDP: resp=%+v err=%v", g.ID, u, err)
		}
		tr, err := g.RequestTCP(false)
		if err != nil || !tr.Reused || tr.ExternalPort != beforeT.ExternalPort {
			t.Fatalf("%s duplicate TCP: resp=%+v err=%v", g.ID, tr, err)
		}
	}

	// Declared/measured mismatch is retained as evidence but the mapping stays.
	g4 := f.status(t, "gw-004")
	if len(g4.Gateways) != 1 || len(g4.Gateways[0].Mappings) != 2 {
		t.Fatalf("unexpected gw-004 status: %+v", g4)
	}
	if !g4.Gateways[0].Mappings[0].TypeMismatch || g4.Gateways[0].Mappings[0].MeasuredNATType != protocol.NATFullCone {
		t.Fatalf("mismatch evidence missing: %+v", g4.Gateways[0].Mappings[0])
	}

	// Both protocols process data from the bound source at loopback latency.
	var latencies []time.Duration
	for round := 0; round < 5; round++ {
		for _, g := range f.gateways {
			d, err := g.SendUDPData(fmt.Sprintf("udp-%d", round))
			if err != nil {
				t.Fatalf("%s udp data: %v", g.ID, err)
			}
			latencies = append(latencies, d)
			d, err = g.SendTCPData(fmt.Sprintf("tcp-%d", round))
			if err != nil {
				t.Fatalf("%s tcp data: %v", g.ID, err)
			}
			latencies = append(latencies, d)
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := latencies[(len(latencies)*95+99)/100-1]
	if p95 > 20*time.Millisecond {
		t.Fatalf("forwarding P95 %s exceeds 20ms", p95)
	}

	// A source that does not match the binding is counted and dropped.
	spoof, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer spoof.Close()
	for _, g := range f.gateways {
		m, _ := g.Mapping(protocol.ProtocolUDP)
		_, _ = spoof.WriteTo([]byte(`{"type":"data","gateway_id":"spoof"}`),
			&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: m.ExternalPort})
	}
	time.Sleep(100 * time.Millisecond)
	for _, g := range f.gateways {
		st := f.status(t, g.ID)
		if st.Gateways[0].Counters.SourceMismatchDrops < 1 {
			t.Fatalf("%s source mismatch was not counted: %+v", g.ID, st.Gateways[0].Counters)
		}
	}

	// NAT behavior change is re-probed and does not invalidate the port.
	f.full.SetBehavior(protocol.NATPortRestricted)
	g1 := f.gateways[0]
	before, _ := g1.Mapping(protocol.ProtocolUDP)
	resp, err := g1.RequestUDP(true)
	if err != nil {
		t.Fatal(err)
	}
	if resp.MeasuredNATType != protocol.NATPortRestricted || resp.ExternalPort != before.ExternalPort {
		t.Fatalf("NAT behavior did not update: %+v", resp)
	}
	f.full.SetBehavior(protocol.NATFullCone)

	// NAT rebinding updates the source binding only after a successful probe.
	g7 := f.gateways[6]
	oldAddr := g7.NAT.PublicUDPAddr()
	if err := g7.RotateNATEndpoint(); err != nil {
		t.Fatal(err)
	}
	hb, _, err := g7.HeartbeatBoth(true)
	if err != nil || !hb.OK {
		t.Fatalf("heartbeat after NAT rotate: %+v %v", hb, err)
	}
	if hb.BoundIP+":"+fmt.Sprint(hb.BoundPort) == oldAddr || hb.ProbeEvidence == nil || !hb.ProbeEvidence.PrimaryProbeReplied {
		t.Fatalf("binding not updated with evidence: %+v", hb)
	}
	if _, err := g7.SendUDPData("after-rotate"); err != nil {
		t.Fatalf("new binding cannot forward: %v", err)
	}

	// Restart preserves unexpired mappings and gateway identity/ports.
	oldPorts := make(map[string][2]int)
	for _, g := range f.gateways {
		u, _ := g.Mapping(protocol.ProtocolUDP)
		tcp, _ := g.Mapping(protocol.ProtocolTCP)
		oldPorts[g.ID] = [2]int{u.ExternalPort, tcp.ExternalPort}
	}
	restartCfg := f.cfg
	restartCfg.ControlUDP = f.r.ControlUDPAddress()
	restartCfg.ControlTCP = f.r.ControlTCPAddress()
	restartCfg.HTTP = f.r.HttpAddress()
	if err := f.r.Stop(); err != nil {
		t.Fatal(err)
	}
	r2, err := relay.Start(context.Background(), restartCfg)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = restartCfg
	f.r = r2
	for _, g := range f.gateways {
		u, tcp, err := g.HeartbeatBoth(false)
		if err != nil || !u.OK || !tcp.OK {
			t.Fatalf("%s post-restart heartbeat: %+v %+v %v", g.ID, u, tcp, err)
		}
		if !u.Recovered || u.ExternalPort != oldPorts[g.ID][0] || tcp.ExternalPort != oldPorts[g.ID][1] {
			t.Fatalf("%s recovery mismatch: got u=%+v tcp=%+v want=%v", g.ID, u, tcp, oldPorts[g.ID])
		}
		if err := g.BindTCPData(); err != nil {
			t.Fatalf("%s post-restart data bind: %v", g.ID, err)
		}
	}
	all := f.status(t, "")
	active := 0
	for _, gw := range all.Gateways {
		active += gw.ActiveMappings
	}
	if active != 24 {
		t.Fatalf("active mappings after restart = %d, want 24: %+v", active, all)
	}
}

func TestExpiryCooldownAndNewPort(t *testing.T) {
	f := newFixture(t)
	g := f.gateways[0]
	if _, err := g.RequestUDP(false); err != nil {
		t.Fatal(err)
	}
	if _, err := g.RequestTCP(false); err != nil {
		t.Fatal(err)
	}
	if err := g.BindTCPData(); err != nil {
		t.Fatal(err)
	}
	u, _ := g.Mapping(protocol.ProtocolUDP)
	tcp, _ := g.Mapping(protocol.ProtocolTCP)
	old := map[int]bool{u.ExternalPort: true, tcp.ExternalPort: true}

	time.Sleep(f.cfg.LeaseWindow + f.cfg.InFlightDrain + 250*time.Millisecond)
	st := f.status(t, g.ID)
	if len(st.Gateways[0].Mappings) != 0 {
		t.Fatalf("mappings not expired: %+v", st.Gateways[0].Mappings)
	}
	if st.Gateways[0].Counters.RecycleCount != 2 {
		t.Fatalf("recycle count = %d, want 2", st.Gateways[0].Counters.RecycleCount)
	}
	if st.Gateways[0].LastRecycle == nil || st.Gateways[0].LastRecycle.Reason != "keepalive_timeout" {
		t.Fatalf("last recycle missing: %+v", st.Gateways[0].LastRecycle)
	}

	u2, err := g.RequestUDP(false)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := g.RequestTCP(false)
	if err != nil {
		t.Fatal(err)
	}
	if old[u2.ExternalPort] || old[t2.ExternalPort] {
		t.Fatalf("recycled port reused during cooldown: old=%v new=%d/%d", old, u2.ExternalPort, t2.ExternalPort)
	}
	metrics := getMetrics(t, f.r)
	for _, needle := range []string{
		`relay_mapping_port{gateway="gw-001",protocol="udp"}`,
		`relay_mapping_port{gateway="gw-001",protocol="tcp"}`,
		`relay_gateway_source_mismatch_drops_total{gateway="gw-001"}`,
		`relay_gateway_nat_type_mismatch_total{gateway="gw-001"}`,
		`relay_gateway_recycles_total{gateway="gw-001"}`,
	} {
		if !strings.Contains(metrics, needle) {
			t.Fatalf("metrics missing %q\n%s", needle, metrics)
		}
	}
}

func getMetrics(t *testing.T, r *relay.Relay) string {
	t.Helper()
	resp, err := http.Get("http://" + r.HttpAddress() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
