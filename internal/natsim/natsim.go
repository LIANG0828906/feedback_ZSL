// Package natsim implements a loopback NAT behavior test bed. The simulated
// gateway talks to an internal-facing endpoint object while the relay sees only
// the simulator's public UDP/TCP socket. Full-cone endpoints forward inbound
// datagrams from any source; port-restricted endpoints only forward from peers
// previously contacted by the gateway.
package natsim

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"parkrelay/internal/protocol"
)

type Simulator struct {
	Behavior string
	mu       sync.Mutex
	gateways []*GatewayEndpoint
}

func NewSimulator(behavior string) *Simulator {
	return &Simulator{Behavior: protocol.NormalizeNATType(behavior)}
}

func (s *Simulator) NewGateway(gatewayID string) (*GatewayEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := newGatewayEndpoint(gatewayID, s)
	if err != nil {
		return nil, err
	}
	s.gateways = append(s.gateways, e)
	return e, nil
}

func (s *Simulator) SetBehavior(behavior string) {
	s.mu.Lock()
	s.Behavior = protocol.NormalizeNATType(behavior)
	for _, e := range s.gateways {
		e.SetBehavior(s.Behavior)
	}
	s.mu.Unlock()
}

func (s *Simulator) Close() {
	s.mu.Lock()
	endpoints := append([]*GatewayEndpoint(nil), s.gateways...)
	s.gateways = nil
	s.mu.Unlock()
	for _, e := range endpoints {
		e.Close()
	}
}

type GatewayEndpoint struct {
	GatewayID string
	sim       *Simulator

	mu          sync.RWMutex
	behavior    string
	publicUDP   net.PacketConn
	publicTCP   net.Listener
	publicAddr  *net.UDPAddr
	udpReceiver func(payload []byte, source net.Addr)
	tcpReceiver func(conn net.Conn)
	allowed     map[string]bool
	closed      bool

	rotateMu sync.Mutex
	dropped  atomic.Uint64
}

func (s *Simulator) behaviorValue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Behavior
}

func newGatewayEndpoint(id string, sim *Simulator) (*GatewayEndpoint, error) {
	e := &GatewayEndpoint{GatewayID: id, sim: sim, behavior: sim.Behavior, allowed: make(map[string]bool)}
	if err := e.bindPublic(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *GatewayEndpoint) bindPublic() error {
	e.rotateMu.Lock()
	defer e.rotateMu.Unlock()
	for attempt := 0; attempt < 32; attempt++ {
		pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return err
		}
		port := pc.LocalAddr().(*net.UDPAddr).Port
		ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
		if err != nil {
			pc.Close()
			continue
		}
		e.mu.Lock()
		oldUDP, oldTCP := e.publicUDP, e.publicTCP
		e.publicUDP, e.publicTCP = pc, ln
		e.publicAddr = pc.LocalAddr().(*net.UDPAddr)
		e.closed = false
		e.allowed = make(map[string]bool)
		e.mu.Unlock()
		if oldUDP != nil {
			_ = oldUDP.Close()
		}
		if oldTCP != nil {
			_ = oldTCP.Close()
		}
		go e.udpPublicLoop(pc)
		go e.tcpPublicLoop(ln)
		return nil
	}
	return errors.New("could not bind a matching NAT public UDP/TCP port")
}

func (e *GatewayEndpoint) PublicUDPAddr() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.publicAddr == nil {
		return ""
	}
	return e.publicAddr.String()
}

func (e *GatewayEndpoint) PublicTCPAddr() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.publicTCP == nil {
		return ""
	}
	return e.publicTCP.Addr().String()
}

func (e *GatewayEndpoint) SetBehavior(behavior string) {
	e.mu.Lock()
	e.behavior = protocol.NormalizeNATType(behavior)
	e.mu.Unlock()
}

func (e *GatewayEndpoint) Behavior() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.behavior
}

func (e *GatewayEndpoint) SetUDPReceiver(fn func([]byte, net.Addr)) {
	e.mu.Lock()
	e.udpReceiver = fn
	e.mu.Unlock()
}

func (e *GatewayEndpoint) SetTCPReceiver(fn func(net.Conn)) {
	e.mu.Lock()
	e.tcpReceiver = fn
	e.mu.Unlock()
}

func (e *GatewayEndpoint) SendUDP(payload []byte, destination string) error {
	dst, err := net.ResolveUDPAddr("udp4", destination)
	if err != nil {
		return err
	}
	e.mu.Lock()
	pc := e.publicUDP
	behavior := e.behavior
	if behavior == protocol.NATPortRestricted {
		e.allowed[dst.String()] = true
	}
	e.mu.Unlock()
	if pc == nil {
		return net.ErrClosed
	}
	_, err = pc.WriteTo(payload, dst)
	return err
}

func (e *GatewayEndpoint) DialTCP(destination string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp4", destination, timeout)
}

func (e *GatewayEndpoint) udpPublicLoop(pc net.PacketConn) {
	buf := make([]byte, 64*1024)
	for {
		n, source, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		payload := append([]byte(nil), buf[:n]...)
		e.mu.RLock()
		behavior := e.behavior
		allowed := e.allowed[source.String()]
		receiver := e.udpReceiver
		closed := e.closed
		e.mu.RUnlock()
		if closed {
			continue
		}
		if behavior == protocol.NATPortRestricted && !allowed {
			e.dropped.Add(1)
			continue
		}
		if receiver != nil {
			receiver(payload, source)
		}
	}
}

func (e *GatewayEndpoint) tcpPublicLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		e.mu.RLock()
		receiver := e.tcpReceiver
		e.mu.RUnlock()
		if receiver != nil {
			go receiver(conn)
		} else {
			_ = conn.Close()
		}
	}
}

// RotatePublicEndpoint changes the externally visible UDP and TCP port while
// preserving the gateway identity and behavior. This simulates a NAT rebinding.
func (e *GatewayEndpoint) RotatePublicEndpoint() error {
	return e.bindPublic()
}

func (e *GatewayEndpoint) DroppedInbound() uint64 { return e.dropped.Load() }

func (e *GatewayEndpoint) Close() {
	e.rotateMu.Lock()
	defer e.rotateMu.Unlock()
	e.mu.Lock()
	e.closed = true
	udp, tcp := e.publicUDP, e.publicTCP
	e.publicUDP, e.publicTCP = nil, nil
	e.mu.Unlock()
	if udp != nil {
		_ = udp.Close()
	}
	if tcp != nil {
		_ = tcp.Close()
	}
}
