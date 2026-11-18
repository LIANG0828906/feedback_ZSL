package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"time"

	"parkrelay/internal/protocol"
)

func probeNonce() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

func (r *Relay) registerProbeWaiter(nonce string) chan struct{} {
	ch := make(chan struct{}, 1)
	r.probeMu.Lock()
	r.probeWaiters[nonce] = ch
	r.probeMu.Unlock()
	return ch
}

func (r *Relay) unregisterProbeWaiter(nonce string) {
	r.probeMu.Lock()
	delete(r.probeWaiters, nonce)
	r.probeMu.Unlock()
}

func (r *Relay) signalProbe(nonce string) {
	r.probeMu.Lock()
	ch := r.probeWaiters[nonce]
	delete(r.probeWaiters, nonce)
	r.probeMu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (r *Relay) probeEndpoint(gatewayID, declared, target string, sourceChanged bool, m *mapping) (protocol.ProbeEvidence, error) {
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return protocol.ProbeEvidence{}, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return protocol.ProbeEvidence{}, err
	}
	primaryNonce, alternateNonce := probeNonce(), probeNonce()
	primaryCh := r.registerProbeWaiter(primaryNonce)
	alternateCh := r.registerProbeWaiter(alternateNonce)
	defer r.unregisterProbeWaiter(primaryNonce)
	defer r.unregisterProbeWaiter(alternateNonce)

	alt, err := net.ListenPacket("udp4", net.JoinHostPort(r.cfg.BindIP, "0"))
	if err != nil {
		return protocol.ProbeEvidence{}, err
	}
	defer alt.Close()
	go func() {
		buf := make([]byte, 2048)
		_ = alt.SetReadDeadline(time.Now().Add(r.cfg.ProbeTimeout))
		for {
			n, _, err := alt.ReadFrom(buf)
			if err != nil {
				return
			}
			var reply protocol.Message
			if json.Unmarshal(buf[:n], &reply) == nil && reply.Type == protocol.TypeProbeReply {
				r.signalProbe(reply.Nonce)
			}
		}
	}()

	start := time.Now()
	primary := protocol.Message{
		Type: protocol.TypeProbe, GatewayID: gatewayID, Nonce: primaryNonce,
		SentUnixNano: start.UnixNano(),
	}
	alternate := protocol.Message{
		Type: protocol.TypeProbe, GatewayID: gatewayID, Nonce: alternateNonce,
		SentUnixNano: start.UnixNano(),
	}
	dest, err := net.ResolveUDPAddr("udp4", target)
	if err != nil {
		return protocol.ProbeEvidence{}, err
	}
	if _, err := r.controlUDP.WriteTo(protocol.Encode(primary), dest); err != nil {
		return protocol.ProbeEvidence{}, fmt.Errorf("primary probe: %w", err)
	}
	if _, err := alt.WriteTo(protocol.Encode(alternate), dest); err != nil {
		return protocol.ProbeEvidence{}, fmt.Errorf("alternate probe: %w", err)
	}

	deadline := start.Add(r.cfg.ProbeTimeout)
	primaryReplied := waitProbe(primaryCh, deadline)
	primaryLatency := time.Since(start).Milliseconds()
	alternateReplied := waitProbe(alternateCh, deadline)
	alternateLatency := time.Since(start).Milliseconds()

	measured := "unknown"
	probeErr := ""
	switch {
	case primaryReplied && alternateReplied:
		measured = protocol.NATFullCone
	case primaryReplied && !alternateReplied:
		measured = protocol.NATPortRestricted
	case !primaryReplied:
		probeErr = "primary probe from control endpoint timed out"
	}
	now := time.Now()
	ev := protocol.ProbeEvidence{
		Method:                  "udp_reflection_two_source",
		ObservedIP:              host,
		ObservedPort:            port,
		PrimaryProbeReplied:     primaryReplied,
		AlternateSourceReplied:  alternateReplied,
		PrimaryProbeLatencyMS:   primaryLatency,
		AlternateProbeLatencyMS: alternateLatency,
		MeasuredNATType:         measured,
		DeclaredNATType:         declared,
		TypeMismatch:            measured != "unknown" && measured != declared,
		ProbeError:              probeErr,
		ProbedAt:                now.UTC().Format(time.RFC3339Nano),
	}
	if sourceChanged && m != nil {
		m.mu.Lock()
		ev.PreviousObservedIP, ev.PreviousObservedPort = m.BoundIP, m.BoundPort
		m.mu.Unlock()
	}
	return ev, nil
}

func waitProbe(ch <-chan struct{}, deadline time.Time) bool {
	wait := time.Until(deadline)
	if wait <= 0 {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

func (r *Relay) probeAndApply(ctx context.Context, m *mapping, target string, sourceChanged bool) error {
	m.mu.Lock()
	declared := m.DeclaredNATType
	m.mu.Unlock()
	ev, err := r.probeEndpoint(m.GatewayID, declared, target, sourceChanged, m)
	if err != nil {
		m.mu.Lock()
		m.ProbeEvidence = protocol.ProbeEvidence{
			Method: "udp_reflection_two_source", ObservedIP: target,
			DeclaredNATType: declared, MeasuredNATType: "unknown",
			ProbeError: err.Error(), ProbedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		m.mu.Unlock()
		return err
	}
	r.applyProbe(ctx, m, ev)
	return nil
}

func (r *Relay) probeRefreshLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.cfg.ProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			seen := make(map[string]bool)
			for _, m := range r.allMappings() {
				if m.Protocol != protocol.ProtocolUDP {
					continue
				}
				m.mu.Lock()
				if !m.active || seen[m.GatewayID] {
					m.mu.Unlock()
					continue
				}
				seen[m.GatewayID] = true
				target := net.JoinHostPort(m.BoundIP, strconv.Itoa(m.BoundPort))
				declared := m.DeclaredNATType
				m.mu.Unlock()
				ev, err := r.probeEndpoint(m.GatewayID, declared, target, false, nil)
				if err != nil || r.ctx.Err() != nil {
					continue
				}
				for _, targetMapping := range r.allMappings() {
					if targetMapping.GatewayID == m.GatewayID {
						r.applyProbe(r.ctx, targetMapping, ev)
						_ = r.persist(targetMapping)
					}
				}
			}
		}
	}
}
