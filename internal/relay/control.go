package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"parkrelay/internal/protocol"
	"parkrelay/internal/store"
)

func splitRemote(addr net.Addr) (string, int, error) {
	switch a := addr.(type) {
	case *net.UDPAddr:
		return a.IP.String(), a.Port, nil
	case *net.TCPAddr:
		return a.IP.String(), a.Port, nil
	default:
		host, port, err := net.SplitHostPort(addr.String())
		if err != nil {
			return "", 0, err
		}
		p, err := strconv.Atoi(port)
		return host, p, err
	}
}

func (r *Relay) handleControl(ctx context.Context, msg protocol.Message, proto string, remote net.Addr) protocol.Response {
	declared := protocol.NormalizeNATType(msg.DeclaredNATType)
	ip, port, err := splitRemote(remote)
	if err != nil {
		return protocol.Response{Type: protocol.TypeResponse, GatewayID: msg.GatewayID, Protocol: proto, OK: false, Error: err.Error()}
	}
	switch msg.Type {
	case protocol.TypeRequestMapping:
		return r.requestMapping(ctx, msg, declared, proto, ip, port)
	case protocol.TypeHeartbeat:
		return r.heartbeat(ctx, msg, declared, proto, ip, port)
	default:
		return protocol.Response{Type: protocol.TypeResponse, GatewayID: msg.GatewayID, Protocol: proto,
			OK: false, Error: fmt.Sprintf("unsupported control message %q", msg.Type)}
	}
}

func (r *Relay) requestMapping(ctx context.Context, msg protocol.Message, declared, proto, ip string, port int) protocol.Response {
	if declared != protocol.NATFullCone && declared != protocol.NATPortRestricted {
		return protocol.Response{Type: protocol.TypeResponse, GatewayID: msg.GatewayID, Protocol: proto,
			OK: false, Error: "declared_nat_type must be full_cone or port_restricted"}
	}
	key := mappingKey(msg.GatewayID, proto)
	r.regMu.Lock()
	if existing := r.mappings[key]; existing != nil {
		r.regMu.Unlock()
		r.touchMapping(ctx, existing, declared, proto, ip, port, true, true)
		return r.mappingResponse(existing, true, false, nil)
	}
	if err := ctx.Err(); err != nil {
		r.regMu.Unlock()
		return protocol.Response{Type: protocol.TypeResponse, GatewayID: msg.GatewayID, Protocol: proto, OK: false, Error: err.Error()}
	}
	extPort, pc, ln, err := r.allocator.Allocate(ctx, proto, r.portUsedLocked)
	if err != nil {
		r.regMu.Unlock()
		return protocol.Response{Type: protocol.TypeResponse, GatewayID: msg.GatewayID, Protocol: proto, OK: false, Error: err.Error()}
	}
	now := time.Now()
	m := &mapping{
		GatewayID:       msg.GatewayID,
		Protocol:        proto,
		ExternalPort:    extPort,
		BoundIP:         ip,
		BoundPort:       port,
		DeclaredNATType: declared,
		MeasuredNATType: "unknown",
		MappingToken:    newToken(),
		CreatedAt:       now,
		LastHeartbeat:   now,
		LeaseExpires:    now.Add(r.cfg.LeaseWindow),
		active:          true,
		udpConn:         pc,
		tcpLn:           ln,
	}
	m.ProbeEvidence = protocol.ProbeEvidence{
		Method: "pending", ObservedIP: ip, ObservedPort: port,
		DeclaredNATType: declared, MeasuredNATType: "unknown",
	}
	r.mappings[key] = m
	r.regMu.Unlock()

	r.startMappingReader(m)
	probeEndpoint := net.JoinHostPort(ip, strconv.Itoa(port))
	if proto == protocol.ProtocolTCP {
		if udp := r.lookup(msg.GatewayID, protocol.ProtocolUDP); udp != nil {
			udp.mu.Lock()
			probeEndpoint = net.JoinHostPort(udp.BoundIP, strconv.Itoa(udp.BoundPort))
			udp.mu.Unlock()
		}
	}
	if err := r.probeAndApply(ctx, m, probeEndpoint, true); err != nil {
		// A failed probe is evidence, not grounds to reject the mapping.
	}
	m.mu.Lock()
	m.Recovered = false
	m.mu.Unlock()
	if err := r.persist(m); err != nil {
		r.removeNewMapping(m)
		return protocol.Response{Type: protocol.TypeResponse, GatewayID: msg.GatewayID, Protocol: proto,
			OK: false, Error: "persist mapping: " + err.Error()}
	}
	return r.mappingResponse(m, false, false, nil)
}

func (r *Relay) heartbeat(ctx context.Context, msg protocol.Message, declared, proto, ip string, port int) protocol.Response {
	m := r.lookup(msg.GatewayID, proto)
	if m == nil {
		return protocol.Response{Type: protocol.TypeResponse, GatewayID: msg.GatewayID, Protocol: proto,
			OK: false, Error: "mapping not found; request_mapping is required"}
	}
	probeEvidence, sourceAccepted := r.touchMapping(ctx, m, declared, proto, ip, port, false, msg.ForceProbe)
	if !sourceAccepted {
		return protocol.Response{Type: protocol.TypeResponse, GatewayID: msg.GatewayID, Protocol: proto,
			OK: false, Error: "observed source could not be probed; mapping binding unchanged"}
	}
	return r.mappingResponse(m, true, false, probeEvidence)
}

// touchMapping refreshes a mapping and, for UDP, atomically moves the source
// binding only after a successful probe to the newly observed endpoint.
func (r *Relay) touchMapping(ctx context.Context, m *mapping, declared, proto, ip string, port int, reused, force bool) (*protocol.ProbeEvidence, bool) {
	now := time.Now()
	m.mu.Lock()
	if m.recycled {
		m.mu.Unlock()
		return nil, false
	}
	if m.draining {
		m.draining = false
		m.drainDeadline = time.Time{}
	}
	m.active = true
	m.DeclaredNATType = declared
	sourceChanged := proto == protocol.ProtocolUDP && (m.BoundIP != ip || m.BoundPort != port)
	probeDue := sourceChanged || reused || force
	if !sourceChanged && time.Since(m.ProbeEvidenceTime()) >= r.cfg.ProbeInterval {
		probeDue = true
	}
	m.mu.Unlock()

	var evidence *protocol.ProbeEvidence
	sourceAccepted := true
	if proto == protocol.ProtocolUDP && probeDue {
		target := net.JoinHostPort(ip, strconv.Itoa(port))
		ev, err := r.probeEndpoint(m.GatewayID, declared, target, sourceChanged, m)
		if err != nil {
			_ = err
		}
		evidence = &ev
		if ev.PrimaryProbeReplied {
			m.mu.Lock()
			oldIP, oldPort := m.BoundIP, m.BoundPort
			m.BoundIP, m.BoundPort = ip, port
			m.mu.Unlock()
			evidence.PreviousObservedIP, evidence.PreviousObservedPort = oldIP, oldPort
			r.recordProbe(m.GatewayID, ev)
		} else if sourceChanged {
			sourceAccepted = false
		}
		r.applyProbe(ctx, m, ev)
	} else if proto == protocol.ProtocolTCP && probeDue {
		endpoint := ""
		if udp := r.lookup(m.GatewayID, protocol.ProtocolUDP); udp != nil {
			udp.mu.Lock()
			endpoint = net.JoinHostPort(udp.BoundIP, strconv.Itoa(udp.BoundPort))
			udp.mu.Unlock()
		}
		if endpoint != "" {
			ev, _ := r.probeEndpoint(m.GatewayID, declared, endpoint, false, m)
			evidence = &ev
			r.applyProbe(ctx, m, ev)
		}
	}

	if sourceAccepted {
		m.mu.Lock()
		m.LastHeartbeat = now
		m.LeaseExpires = now.Add(r.cfg.LeaseWindow)
		m.mu.Unlock()
		if err := r.persist(m); err != nil {
			fmt.Printf("persist heartbeat %s/%s: %v\n", m.GatewayID, m.Protocol, err)
		}
	}
	return evidence, sourceAccepted
}

func (m *mapping) ProbeEvidenceTime() time.Time {
	if m.ProbeEvidence.ProbedAt == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, m.ProbeEvidence.ProbedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (r *Relay) mappingResponse(m *mapping, reused, recovered bool, evidence *protocol.ProbeEvidence) protocol.Response {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev := m.ProbeEvidence
	if evidence != nil {
		ev = *evidence
	}
	remain := int64(0)
	if m.LeaseExpires.After(time.Now()) {
		remain = time.Until(m.LeaseExpires).Milliseconds()
	}
	return protocol.Response{
		Type:                 protocol.TypeResponse,
		GatewayID:            m.GatewayID,
		Protocol:             m.Protocol,
		ExternalPort:         m.ExternalPort,
		BoundIP:              m.BoundIP,
		BoundPort:            m.BoundPort,
		DeclaredNATType:      m.DeclaredNATType,
		MeasuredNATType:      m.MeasuredNATType,
		TypeMismatch:         m.TypeMismatch,
		ProbeEvidence:        &ev,
		Reused:               reused,
		Recovered:            recovered || m.Recovered,
		LeaseRemainingMillis: remain,
		MappingToken:         m.MappingToken,
		OK:                   true,
	}
}

func (r *Relay) persist(m *mapping) error {
	snap := m.snapshot()
	return r.store.UpsertMapping(context.Background(), snap)
}

func (r *Relay) removeNewMapping(m *mapping) {
	r.regMu.Lock()
	delete(r.mappings, mappingKey(m.GatewayID, m.Protocol))
	r.regMu.Unlock()
	r.closeMappingTransport(m)
	_ = r.store.DeleteMapping(context.Background(), m.GatewayID, m.Protocol)
}

func (r *Relay) recordProbe(gatewayID string, ev protocol.ProbeEvidence) {
	r.regMu.Lock()
	r.lastProbe[gatewayID] = ev
	r.regMu.Unlock()
}

func (r *Relay) applyProbe(ctx context.Context, m *mapping, ev protocol.ProbeEvidence) {
	m.mu.Lock()
	oldMismatch := m.TypeMismatch
	oldMeasured := m.MeasuredNATType
	oldDeclared := m.DeclaredNATType
	m.ProbeEvidence = ev
	m.MeasuredNATType = ev.MeasuredNATType
	m.TypeMismatch = ev.TypeMismatch
	m.DeclaredNATType = ev.DeclaredNATType
	m.mu.Unlock()
	r.recordProbe(m.GatewayID, ev)
	if ev.TypeMismatch && (!oldMismatch || oldMeasured != ev.MeasuredNATType || oldDeclared != ev.DeclaredNATType) {
		if err := r.store.IncrementNATMismatch(ctx, m.GatewayID); err != nil {
			fmt.Printf("increment nat mismatch %s: %v\n", m.GatewayID, err)
		}
	}
}

func (r *Relay) noteSourceMismatch(m *mapping) {
	m.sourceMismatch.Add(1)
	if err := r.store.IncrementSourceMismatch(context.Background(), m.GatewayID); err != nil {
		fmt.Printf("increment source mismatch %s: %v\n", m.GatewayID, err)
	}
}

func decodeMessage(b []byte) (protocol.Message, error) {
	var m protocol.Message
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	if strings.TrimSpace(m.GatewayID) == "" || strings.TrimSpace(m.Type) == "" {
		return m, fmt.Errorf("gateway_id and type are required")
	}
	return m, nil
}

func (r *Relay) counters(ctx context.Context, gatewayID string) (store.GatewayCounters, error) {
	return r.store.GetCounters(ctx, gatewayID)
}
