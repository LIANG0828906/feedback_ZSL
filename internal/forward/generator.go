package forward

import (
	"math/rand"
)

const DefaultSeed int64 = 42042

type Sample struct {
	Seed      int64             `json:"seed"`
	Project   *Project          `json:"project"`
	Events    []Event           `json:"events"`
	Documents map[string][]byte `json:"documents"`
	Truth     *Truth            `json:"truth"`
}

type Truth struct {
	ExpectedFinalStates map[string]string       `json:"expected_final_states"`
	Transitions         []Transition            `json:"transitions"`
	FragmentPlans       map[string]FragmentPlan `json:"fragment_plans"`
	EventCount          int                     `json:"event_count"`
	PacketCount         int                     `json:"packet_count"`
}

func GenerateSample(seed int64) *Sample {
	rng := rand.New(rand.NewSource(seed))

	wm1 := int64(64 * 1024)
	wm2 := int64(32 * 1024)
	p := &Project{
		ID:             "prj-tunnel-bench-42",
		Name:           "Deterministic Tunnel and VPN Forwarding Sample",
		IntegerBits:    16,
		MaxFragments:   8,
		ToleranceBytes: 8,
		DeclaredSeed:   seed,
		Fallback:       Action{Type: ActionTunnel, TunnelID: "T1", Reason: "declared-default"},
		Peers: []Peer{
			{ID: "P1", Name: "west-gateway", Endpoint: "203.0.113.10:51820", DeclOrder: 0},
			{ID: "P2", Name: "east-gateway", Endpoint: "198.19.0.10:51821", DeclOrder: 1},
			{ID: "P3", Name: "edge-gateway", Endpoint: "192.0.2.30:51822", DeclOrder: 2},
		},
		RouteTargets: []RouteTarget{
			{ID: "RT-DEFAULT", Destination: "0.0.0.0/0", NextHop: "198.51.100.254", Egress: "eth0", DeclOrder: 0},
			{ID: "RT-WEST", PeerID: "P1", Destination: "203.0.113.0/24", NextHop: "198.51.100.253", Egress: "eth0", DeclOrder: 1},
			{ID: "RT-EAST", PeerID: "P2", Destination: "198.18.0.0/15", NextHop: "10.100.0.1", Egress: "eth1", DeclOrder: 2},
		},
		Tunnels: []Tunnel{
			{
				ID: "T1", DeclOrder: 0, PeerID: "P1",
				Local: Endpoint{IP: "198.51.100.1", Port: 50001}, Remote: Endpoint{IP: "203.0.113.10", Port: 51820},
				OuterProtocol: "udp", CipherSuite: "aes-256-gcm",
				MTUBytes: 1500, PathMTUBytes: 1450, KeepaliveMS: 25_000, HandshakeMS: 40,
				RateLimitBps: 1280 * 1024, KeepaliveWatermarkBytes: &wm1, WatermarkEnabled: true,
			},
			{
				ID: "T2", DeclOrder: 1, PeerID: "P2",
				Local: Endpoint{IP: "198.51.100.2", Port: 50002}, Remote: Endpoint{IP: "198.19.0.10", Port: 51821},
				OuterProtocol: "udp", CipherSuite: "chacha20-poly1305",
				MTUBytes: 1480, PathMTUBytes: 1500, KeepaliveMS: 10_000, HandshakeMS: 70,
				RateLimitBps: 512 * 1024, KeepaliveWatermarkBytes: &wm2, WatermarkEnabled: true,
			},
			{
				ID: "T3", DeclOrder: 2, PeerID: "P3",
				Local: Endpoint{IP: "198.51.100.3", Port: 50003}, Remote: Endpoint{IP: "192.0.2.30", Port: 51822},
				OuterProtocol: "udp", CipherSuite: "aes-128-gcm",
				MTUBytes: 1400, PathMTUBytes: 1400, KeepaliveMS: 5_000, HandshakeMS: 40,
				RateLimitBps: 256 * 1024, WatermarkEnabled: false,
			},
		},
		Rules: []Rule{
			{
				ID: "R-DOMAIN-INTERNAL", DeclOrder: 0, Priority: 10, GroupPeerID: "P1",
				DomainSuffixes: []string{"internal.example"},
				Action:         Action{Type: ActionTunnel, TunnelID: "T1"},
			},
			{
				ID: "R-NET-BRANCH-16", DeclOrder: 1, Priority: 20, GroupPeerID: "P1",
				CIDRs:  []string{"10.20.0.0/16"},
				Action: Action{Type: ActionTunnel, TunnelID: "T1"},
			},
			{
				ID: "R-NET-BRANCH-24-APP", DeclOrder: 2, Priority: 20, GroupPeerID: "P2",
				CIDRs:      []string{"10.20.8.0/24"},
				PortRanges: []PortRange{{Start: 8000, End: 8999}},
				Action:     Action{Type: ActionTunnel, TunnelID: "T2"},
			},
			{
				ID: "R-NET-BRANCH-24-OPS", DeclOrder: 3, Priority: 20, GroupPeerID: "P2",
				CIDRs:      []string{"10.20.8.0/24"},
				PortRanges: []PortRange{{Start: 8443, End: 8443}},
				Action:     Action{Type: ActionTunnel, TunnelID: "T3"},
			},
			{
				ID: "R-VIDEO-SUFFIX", DeclOrder: 4, Priority: 30, GroupPeerID: "P3",
				DomainSuffixes: []string{"video.example"}, PortRanges: []PortRange{{Start: 8000, End: 8999}},
				Action: Action{Type: ActionTunnel, TunnelID: "T3"},
			},
			{
				ID: "R-SSH-EAST", DeclOrder: 5, Priority: 40, GroupPeerID: "P2",
				PortRanges: []PortRange{{Start: 22, End: 22}},
				Action:     Action{Type: ActionTunnel, TunnelID: "T2"},
			},
		},
	}

	// Seed-controlled last octet keeps addresses varying while CIDR membership stays fixed.
	branchHost := 20 + rng.Intn(200)
	events := []Event{
		{ID: "E01", Kind: "config_apply", AtMs: 0, ConfigRevision: "cfg-seed-1"},
		{ID: "E02", Kind: "connect", AtMs: 100, TunnelID: "T1"},
		{ID: "E03", Kind: "handshake_complete", AtMs: 140, TunnelID: "T1"},
		{ID: "E04", Kind: "connect", AtMs: 200, TunnelID: "T2"},
		{ID: "E05", Kind: "handshake_complete", AtMs: 270, TunnelID: "T2"},
		{ID: "E06", Kind: "connect", AtMs: 300, TunnelID: "T3"},
		{ID: "E07", Kind: "handshake_complete", AtMs: 340, TunnelID: "T3"},
		packetEvent("E08", 400, PacketSpec{ID: "P-APP-SMALL", SrcIP: "10.0.0.10", DstIP: "10.20.8.8", Protocol: "tcp", SrcPort: 49000, DstPort: 8443, PayloadBytes: 1400}),
		packetEvent("E09", 450, PacketSpec{ID: "P-APP-LARGE", SrcIP: "10.0.0.11", DstIP: "10.20.8.9", Protocol: "tcp", SrcPort: 49001, DstPort: 8443, PayloadBytes: 3000}),
		packetEvent("E10", 500, PacketSpec{ID: "P-BRANCH-WEB", SrcIP: "10.0.0.12", DstIP: "10.20.20." + itoa(branchHost), Protocol: "tcp", SrcPort: 49002, DstPort: 443, PayloadBytes: 2800}),
		packetEvent("E11", 550, PacketSpec{ID: "P-VIDEO", SrcIP: "10.0.0.13", Domain: "stream.video.example", DstIP: "203.0.113.77", Protocol: "udp", SrcPort: 49003, DstPort: 8200, PayloadBytes: 900}),
		packetEvent("E12", 600, PacketSpec{ID: "P-INTERNAL-DNS", SrcIP: "10.0.0.14", Domain: "ops.internal.example", DstIP: "10.20.1.53", Protocol: "tcp", SrcPort: 49004, DstPort: 443, PayloadBytes: 500}),
		packetEvent("E13", 650, PacketSpec{ID: "P-FALLBACK", SrcIP: "10.0.0.15", DstIP: "8.8.8.8", Protocol: "udp", SrcPort: 49005, DstPort: 53, PayloadBytes: 1200}),
		packetEvent("E14", 700, PacketSpec{ID: "P-TOO-LARGE", SrcIP: "10.0.0.16", DstIP: "10.20.8.10", Protocol: "tcp", SrcPort: 49006, DstPort: 8443, PayloadBytes: 20_000}),
		packetEvent("E15", 750, PacketSpec{ID: "P-SSH", SrcIP: "10.0.0.17", DstIP: "203.0.113.9", Protocol: "tcp", SrcPort: 49007, DstPort: 22, PayloadBytes: 800}),
		{ID: "E16", Kind: "disconnect", AtMs: 900, TunnelID: "T3"},
		{ID: "E17", Kind: "keepalive_timeout", AtMs: 1200, TunnelID: "T2"},
		{ID: "E18", Kind: "config_apply", AtMs: 1500, ConfigRevision: "cfg-seed-2"},
	}
	for i := range events {
		events[i].Seq = i + 1
	}

	docs := map[string][]byte{}
	for _, schema := range []string{SchemaV1, SchemaV2, SchemaV3} {
		raw, err := RenderDocument(schema, p, events)
		if err != nil {
			panic(err)
		}
		docs[schema] = raw
	}

	sample := &Sample{Seed: seed, Project: p, Events: events, Documents: docs}
	sample.Truth = buildTruth(sample)
	return sample
}

func packetEvent(id string, at int64, pkt PacketSpec) Event {
	pkt.AtMs = at
	return Event{ID: id, Seq: 0, Kind: "packet", AtMs: at, Packet: &pkt}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func buildTruth(s *Sample) *Truth {
	truth := &Truth{
		ExpectedFinalStates: map[string]string{"T1": StateEstablished, "T2": StateDisconnected, "T3": StateDisconnected},
		Transitions: []Transition{
			{EventID: "E02", TunnelID: "T1", FromState: StateIdle, ToState: StateHandshaking, EffectiveAtMs: 100, SourceEventID: "E02", Changed: true},
			{EventID: "E03", TunnelID: "T1", FromState: StateHandshaking, ToState: StateEstablished, EffectiveAtMs: 140, SourceEventID: "E03", Changed: true},
			{EventID: "E04", TunnelID: "T2", FromState: StateIdle, ToState: StateHandshaking, EffectiveAtMs: 200, SourceEventID: "E04", Changed: true},
			{EventID: "E05", TunnelID: "T2", FromState: StateHandshaking, ToState: StateEstablished, EffectiveAtMs: 270, SourceEventID: "E05", Changed: true},
			{EventID: "E06", TunnelID: "T3", FromState: StateIdle, ToState: StateHandshaking, EffectiveAtMs: 300, SourceEventID: "E06", Changed: true},
			{EventID: "E07", TunnelID: "T3", FromState: StateHandshaking, ToState: StateEstablished, EffectiveAtMs: 340, SourceEventID: "E07", Changed: true},
			{EventID: "E16", TunnelID: "T3", FromState: StateEstablished, ToState: StateDisconnected, EffectiveAtMs: 900, SourceEventID: "E16", Changed: true},
			{EventID: "E17", TunnelID: "T2", FromState: StateEstablished, ToState: StateDisconnected, EffectiveAtMs: 1200, SourceEventID: "E17", Changed: true},
		},
		FragmentPlans: map[string]FragmentPlan{},
		EventCount:    len(s.Events),
	}
	packets := 0
	for _, ev := range s.Events {
		if ev.Packet == nil {
			continue
		}
		packets++
		verdict := EvaluateVerdict(s.Project, *ev.Packet)
		tid := verdict.Action.TunnelID
		t, _, ok := s.Project.Tunnel(tid)
		req := FragmentRequest{PacketID: ev.Packet.ID, PayloadBytes: ev.Packet.PayloadBytes, IntegerBits: s.Project.IntegerBits, MaxFragments: s.Project.MaxFragments}
		if ok {
			req.TunnelID = t.ID
			req.MTUBytes = t.MTUBytes
			req.PathMTUBytes = t.PathMTUBytes
			req.OuterProtocol = t.OuterProtocol
			req.CipherSuite = t.CipherSuite
		}
		truth.FragmentPlans[ev.Packet.ID] = PlanFragmentation(req)
	}
	truth.PacketCount = packets
	return truth
}
