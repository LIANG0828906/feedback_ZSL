package forward

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
)

const (
	SchemaV1 = "v1"
	SchemaV2 = "v2"
	SchemaV3 = "v3"

	StateIdle         = "unestablished"
	StateHandshaking  = "handshaking"
	StateEstablished  = "established"
	StateDisconnected = "disconnected"

	ActionTunnel = "tunnel"
	ActionDirect = "direct"
	ActionDrop   = "drop"
	ActionReject = "reject"
)

type Project struct {
	ID                string        `json:"id"`
	Name              string        `json:"name"`
	SourceSchema      string        `json:"source_schema,omitempty"`
	ContentHash       string        `json:"content_hash,omitempty"`
	IntegerBits       int           `json:"integer_bits"`
	MaxFragments      int           `json:"max_fragments"`
	ToleranceBytes    int64         `json:"tolerance_bytes"`
	Peers             []Peer        `json:"peers"`
	RouteTargets      []RouteTarget `json:"route_targets"`
	Tunnels           []Tunnel      `json:"tunnels"`
	Rules             []Rule        `json:"rules"`
	Fallback          Action        `json:"fallback"`
	WatermarkDegraded bool          `json:"watermark_degraded,omitempty"`
	DeclaredSeed      int64         `json:"declared_seed,omitempty"`
}

type Peer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint,omitempty"`
	DeclOrder int    `json:"decl_order"`
}

type RouteTarget struct {
	ID          string `json:"id"`
	PeerID      string `json:"peer_id,omitempty"`
	Destination string `json:"destination_cidr"`
	NextHop     string `json:"next_hop"`
	Egress      string `json:"egress"`
	DeclOrder   int    `json:"decl_order"`
}

type Endpoint struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

type Tunnel struct {
	ID                      string   `json:"id"`
	DeclOrder               int      `json:"decl_order"`
	PeerID                  string   `json:"peer_id"`
	Local                   Endpoint `json:"local"`
	Remote                  Endpoint `json:"remote"`
	OuterProtocol           string   `json:"outer_protocol"`
	CipherSuite             string   `json:"cipher_suite"`
	MTUBytes                int64    `json:"mtu_bytes"`
	PathMTUBytes            int64    `json:"path_mtu_bytes"`
	KeepaliveMS             int64    `json:"keepalive_ms"`
	HandshakeMS             int64    `json:"handshake_ms"`
	RateLimitBps            int64    `json:"rate_limit_bps"`
	KeepaliveWatermarkBytes *int64   `json:"keepalive_watermark_bytes,omitempty"`
	WatermarkEnabled        bool     `json:"watermark_enabled"`
}

type PortRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type Action struct {
	Type     string `json:"type"`
	TunnelID string `json:"tunnel_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type Rule struct {
	ID             string      `json:"id"`
	DeclOrder      int         `json:"decl_order"`
	Priority       int         `json:"priority"`
	GroupPeerID    string      `json:"group_peer_id,omitempty"`
	CIDRs          []string    `json:"cidrs,omitempty"`
	DomainSuffixes []string    `json:"domain_suffixes,omitempty"`
	PortRanges     []PortRange `json:"port_ranges,omitempty"`
	Action         Action      `json:"action"`
}

type PacketSpec struct {
	ID           string `json:"id"`
	AtMs         int64  `json:"at_ms"`
	SrcIP        string `json:"src_ip"`
	DstIP        string `json:"dst_ip,omitempty"`
	Domain       string `json:"domain,omitempty"`
	Protocol     string `json:"protocol"`
	SrcPort      int    `json:"src_port"`
	DstPort      int    `json:"dst_port"`
	PayloadBytes int64  `json:"payload_bytes"`
}

type Event struct {
	ID             string      `json:"id"`
	Seq            int         `json:"seq"`
	Kind           string      `json:"kind"`
	AtMs           int64       `json:"at_ms"`
	TunnelID       string      `json:"tunnel_id,omitempty"`
	ConfigRevision string      `json:"config_revision,omitempty"`
	Packet         *PacketSpec `json:"packet,omitempty"`
}

type Engineering struct {
	Schema  string          `json:"schema"`
	Project *Project        `json:"project"`
	Events  []Event         `json:"events"`
	Raw     json.RawMessage `json:"-"`
}

func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	buf, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	var out Project
	if err := json.Unmarshal(buf, &out); err != nil {
		panic(err)
	}
	return &out
}

// CanonicalHash hashes the normalized engineering intent, independent of schema field names.
func (p *Project) CanonicalHash() string {
	cp := p.Clone()
	cp.SourceSchema = ""
	cp.ContentHash = ""
	buf, _ := json.Marshal(cp)
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:16])
}

func (p *Project) Tunnel(id string) (*Tunnel, int, bool) {
	for i := range p.Tunnels {
		if p.Tunnels[i].ID == id {
			return &p.Tunnels[i], i, true
		}
	}
	return nil, -1, false
}

func (p *Project) Peer(id string) (*Peer, bool) {
	for i := range p.Peers {
		if p.Peers[i].ID == id {
			return &p.Peers[i], true
		}
	}
	return nil, false
}

func (p *Project) Rule(id string) (*Rule, int, bool) {
	for i := range p.Rules {
		if p.Rules[i].ID == id {
			return &p.Rules[i], i, true
		}
	}
	return nil, -1, false
}

func ParseCIDR(s string) (*net.IPNet, error) {
	_, network, err := net.ParseCIDR(s)
	if err != nil {
		return nil, err
	}
	return network, nil
}

func MustCIDR(s string) *net.IPNet {
	n, err := ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// CompareHash returns a stable short label useful in reports and checkpoints.
func ShortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
