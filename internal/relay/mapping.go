package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"parkrelay/internal/protocol"
	"parkrelay/internal/store"
)

type mapping struct {
	mu sync.Mutex

	GatewayID       string
	Protocol        string
	ExternalPort    int
	BoundIP         string
	BoundPort       int
	DeclaredNATType string
	MeasuredNATType string
	TypeMismatch    bool
	ProbeEvidence   protocol.ProbeEvidence
	MappingToken    string
	CreatedAt       time.Time
	LastHeartbeat   time.Time
	LeaseExpires    time.Time
	Recovered       bool
	active          bool
	draining        bool
	drainDeadline   time.Time
	recycled        bool

	udpConn    net.PacketConn
	tcpLn      net.Listener
	udpForward net.Conn
	tcpForward net.Conn
	forwardMu  sync.Mutex

	tcpDataMu sync.Mutex
	tcpData   net.Conn

	forwarded      atomic.Uint64
	sourceMismatch atomic.Uint64
}

func newToken() string {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

func (m *mapping) snapshot() store.Mapping {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev, _ := json.Marshal(m.ProbeEvidence)
	return store.Mapping{
		GatewayID:       m.GatewayID,
		Protocol:        m.Protocol,
		ExternalPort:    m.ExternalPort,
		BoundIP:         m.BoundIP,
		BoundPort:       m.BoundPort,
		DeclaredNATType: m.DeclaredNATType,
		MeasuredNATType: m.MeasuredNATType,
		TypeMismatch:    m.TypeMismatch,
		ProbeEvidence:   string(ev),
		MappingToken:    m.MappingToken,
		CreatedAt:       m.CreatedAt,
		LastHeartbeat:   m.LastHeartbeat,
		LeaseExpires:    m.LeaseExpires,
		Recovered:       m.Recovered,
	}
}

func mappingFromStore(s store.Mapping) *mapping {
	var evidence protocol.ProbeEvidence
	_ = json.Unmarshal([]byte(s.ProbeEvidence), &evidence)
	return &mapping{
		GatewayID:       s.GatewayID,
		Protocol:        s.Protocol,
		ExternalPort:    s.ExternalPort,
		BoundIP:         s.BoundIP,
		BoundPort:       s.BoundPort,
		DeclaredNATType: s.DeclaredNATType,
		MeasuredNATType: s.MeasuredNATType,
		TypeMismatch:    s.TypeMismatch,
		ProbeEvidence:   evidence,
		MappingToken:    s.MappingToken,
		CreatedAt:       s.CreatedAt,
		LastHeartbeat:   s.LastHeartbeat,
		LeaseExpires:    s.LeaseExpires,
		Recovered:       s.Recovered,
		active:          true,
	}
}

func (m *mapping) status(now time.Time) StatusMapping {
	m.mu.Lock()
	defer m.mu.Unlock()
	var remain int64
	if m.active && now.Before(m.LeaseExpires) {
		remain = int64(time.Until(m.LeaseExpires) / time.Millisecond)
	}
	return StatusMapping{
		GatewayID:               m.GatewayID,
		Protocol:                m.Protocol,
		ExternalPort:            m.ExternalPort,
		BoundIP:                 m.BoundIP,
		BoundPort:               m.BoundPort,
		Active:                  m.active,
		Draining:                m.draining,
		DeclaredNATType:         m.DeclaredNATType,
		MeasuredNATType:         m.MeasuredNATType,
		TypeMismatch:            m.TypeMismatch,
		ProbeEvidence:           m.ProbeEvidence,
		LastHeartbeatUnixMillis: m.LastHeartbeat.UnixMilli(),
		LeaseExpiresUnixMillis:  m.LeaseExpires.UnixMilli(),
		LeaseRemainingMillis:    remain,
		ForwardedPackets:        m.forwarded.Load(),
		SourceMismatchDrops:     m.sourceMismatch.Load(),
		RecoveredFromSQLite:     m.Recovered,
	}
}
