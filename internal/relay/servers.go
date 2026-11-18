package relay

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"time"

	"parkrelay/internal/protocol"
)

func (r *Relay) udpControlLoop() {
	defer r.wg.Done()
	buf := make([]byte, 64*1024)
	for {
		n, remote, err := r.controlUDP.ReadFrom(buf)
		if err != nil {
			if r.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		payload := append([]byte(nil), buf[:n]...)
		var msg protocol.Message
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		if msg.Type == protocol.TypeProbeReply {
			r.signalProbe(msg.Nonce)
			continue
		}
		if strings.TrimSpace(msg.GatewayID) == "" {
			continue
		}
		go func() {
			resp := r.handleControl(r.ctx, msg, protocol.ProtocolUDP, remote)
			resp.RequestID = msg.RequestID
			_, _ = r.controlUDP.WriteTo(protocol.Encode(resp), remote)
		}()
	}
}

func (r *Relay) tcpControlAcceptLoop() {
	defer r.wg.Done()
	for {
		conn, err := r.controlTCP.Accept()
		if err != nil {
			if r.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		r.controlConnsMu.Lock()
		r.controlConns[conn] = struct{}{}
		r.controlConnsMu.Unlock()
		r.wg.Add(1)
		go r.serveTCPControl(conn)
	}
}

func (r *Relay) serveTCPControl(conn net.Conn) {
	defer r.wg.Done()
	defer func() {
		_ = conn.Close()
		r.controlConnsMu.Lock()
		delete(r.controlConns, conn)
		r.controlConnsMu.Unlock()
	}()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 4096), 1024*1024)
	for sc.Scan() {
		msg, err := decodeMessage(sc.Bytes())
		if err != nil {
			continue
		}
		resp := r.handleControl(r.ctx, msg, protocol.ProtocolTCP, conn.RemoteAddr())
		resp.RequestID = msg.RequestID
		if err := json.NewEncoder(conn).Encode(resp); err != nil {
			return
		}
	}
}

func (r *Relay) udpMappingLoop(m *mapping) {
	defer r.wg.Done()
	buf := make([]byte, 64*1024)
	for {
		n, remote, err := m.udpConn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-r.ctx.Done():
				return
			default:
				continue
			}
		}
		ip, port, err := splitRemote(remote)
		if err != nil {
			continue
		}
		m.mu.Lock()
		boundIP, boundPort := m.BoundIP, m.BoundPort
		active := m.active || m.draining
		m.mu.Unlock()
		if !active {
			continue
		}
		if ip != boundIP || port != boundPort {
			r.noteSourceMismatch(m)
			continue
		}
		m.forwardMu.Lock()
		forward := m.udpForward
		m.forwardMu.Unlock()
		if forward != nil {
			if _, err := forward.Write(buf[:n]); err != nil {
				continue
			}
		}
		m.forwarded.Add(1)
		var msg protocol.Message
		if json.Unmarshal(buf[:n], &msg) != nil {
			msg = protocol.Message{}
		}
		ack := protocol.Response{
			Type: protocol.TypeDataAck, GatewayID: m.GatewayID, Protocol: protocol.ProtocolUDP,
			ExternalPort: m.ExternalPort, BoundIP: boundIP, BoundPort: boundPort,
			OK: true, LeaseRemainingMillis: leaseRemaining(m),
		}
		_, _ = m.udpConn.WriteTo(protocol.Encode(ack), remote)
		_ = msg
	}
}

func leaseRemaining(m *mapping) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.LeaseExpires.After(time.Now()) {
		return 0
	}
	return time.Until(m.LeaseExpires).Milliseconds()
}

func (r *Relay) tcpMappingAcceptLoop(m *mapping) {
	defer r.wg.Done()
	for {
		conn, err := m.tcpLn.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-r.ctx.Done():
				return
			default:
				continue
			}
		}
		r.wg.Add(1)
		go r.serveTCPData(conn, m)
	}
}

func (r *Relay) serveTCPData(conn net.Conn, m *mapping) {
	defer r.wg.Done()
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 4096), 1024*1024)
	if !sc.Scan() {
		r.noteSourceMismatch(m)
		return
	}
	var bind protocol.Message
	if err := json.Unmarshal(sc.Bytes(), &bind); err != nil ||
		bind.Type != protocol.TypeBindSource || bind.GatewayID != m.GatewayID ||
		bind.MappingToken != m.MappingToken {
		r.noteSourceMismatch(m)
		_ = json.NewEncoder(conn).Encode(protocol.Response{
			Type: protocol.TypeBindSourceReply, GatewayID: m.GatewayID, Protocol: protocol.ProtocolTCP,
			OK: false, Error: "invalid mapping token",
		})
		return
	}
	remoteIP, remotePort, err := splitRemote(conn.RemoteAddr())
	if err != nil {
		r.noteSourceMismatch(m)
		return
	}
	m.mu.Lock()
	m.BoundIP, m.BoundPort = remoteIP, remotePort
	m.LastHeartbeat = time.Now()
	m.LeaseExpires = time.Now().Add(r.cfg.LeaseWindow)
	m.active = true
	m.draining = false
	m.mu.Unlock()
	m.tcpDataMu.Lock()
	if m.tcpData != nil && m.tcpData != conn {
		_ = m.tcpData.Close()
	}
	m.tcpData = conn
	m.tcpDataMu.Unlock()
	if err := r.persist(m); err != nil {
		_ = json.NewEncoder(conn).Encode(protocol.Response{
			Type: protocol.TypeBindSourceReply, GatewayID: m.GatewayID, Protocol: protocol.ProtocolTCP,
			OK: false, Error: err.Error(),
		})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if err := json.NewEncoder(conn).Encode(protocol.Response{
		Type: protocol.TypeBindSourceReply, GatewayID: m.GatewayID, Protocol: protocol.ProtocolTCP,
		ExternalPort: m.ExternalPort, BoundIP: remoteIP, BoundPort: remotePort, OK: true,
	}); err != nil {
		return
	}
	for sc.Scan() {
		var data protocol.Message
		if err := json.Unmarshal(sc.Bytes(), &data); err != nil {
			continue
		}
		if data.Type != protocol.TypeData || data.GatewayID != m.GatewayID {
			continue
		}
		m.mu.Lock()
		sourceOK := m.BoundIP == remoteIP && m.BoundPort == remotePort
		m.mu.Unlock()
		if !sourceOK {
			r.noteSourceMismatch(m)
			continue
		}
		if err := r.forwardTCP(m, append(append([]byte(nil), sc.Bytes()...), '\n')); err != nil {
			continue
		}
		m.forwarded.Add(1)
		if err := json.NewEncoder(conn).Encode(protocol.Response{
			Type: protocol.TypeDataAck, GatewayID: m.GatewayID, Protocol: protocol.ProtocolTCP,
			ExternalPort: m.ExternalPort, BoundIP: remoteIP, BoundPort: remotePort,
			LeaseRemainingMillis: leaseRemaining(m), OK: true,
		}); err != nil {
			return
		}
	}
}

func (r *Relay) forwardTCP(m *mapping, payload []byte) error {
	if r.cfg.ForwardTCP == "" {
		return nil
	}
	m.forwardMu.Lock()
	defer m.forwardMu.Unlock()
	if m.tcpForward == nil {
		conn, err := net.DialTimeout("tcp4", r.cfg.ForwardTCP, time.Second)
		if err != nil {
			return err
		}
		m.tcpForward = conn
	}
	_, err := m.tcpForward.Write(payload)
	if err != nil {
		_ = m.tcpForward.Close()
		m.tcpForward = nil
	}
	return err
}
