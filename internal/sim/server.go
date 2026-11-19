package sim

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"dnssim/internal/dnswire"
)

type dnsServer struct {
	engine   *Engine
	resolver *resolverRuntime
	udp      *net.UDPConn
	tcp      net.Listener

	closed  atomic.Bool
	wg      sync.WaitGroup
	conns   sync.Map
	closeMu sync.Mutex
}

func (s *dnsServer) start() {
	s.wg.Add(2)
	go s.serveUDP()
	go s.serveTCP()
}

func (s *dnsServer) close() {
	s.closeMu.Lock()
	if s.closed.Swap(true) {
		s.closeMu.Unlock()
		return
	}
	if s.udp != nil {
		_ = s.udp.Close()
	}
	if s.tcp != nil {
		_ = s.tcp.Close()
	}
	s.conns.Range(func(key, _ any) bool {
		_ = key.(net.Conn).Close()
		return true
	})
	s.closeMu.Unlock()
	s.wg.Wait()
}

func (s *dnsServer) serveUDP() {
	defer s.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, addr, err := s.udp.ReadFromUDP(buf)
		if err != nil {
			if s.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		packet := append([]byte(nil), buf[:n]...)
		s.engine.udpQueries.Add(1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handlePacket(packet, addr, "udp")
		}()
	}
}

func (s *dnsServer) serveTCP() {
	defer s.wg.Done()
	for {
		conn, err := s.tcp.Accept()
		if err != nil {
			if s.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s.conns.Store(conn, struct{}{})
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.conns.Delete(conn)
			defer conn.Close()
			s.handleTCP(conn)
		}()
	}
}

func (s *dnsServer) handleTCP(conn net.Conn) {
	for {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var length [2]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(length[:]))
		if n == 0 {
			return
		}
		packet := make([]byte, n)
		if _, err := io.ReadFull(conn, packet); err != nil {
			return
		}
		s.engine.tcpQueries.Add(1)
		response, drop := s.process(packet, conn.RemoteAddr(), "tcp")
		if drop || response == nil {
			return
		}
		frame := make([]byte, 2+len(response))
		binary.BigEndian.PutUint16(frame[:2], uint16(len(response)))
		copy(frame[2:], response)
		_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write(frame); err != nil {
			return
		}
	}
}

func (s *dnsServer) handlePacket(packet []byte, addr *net.UDPAddr, transport string) {
	response, drop := s.process(packet, addr, transport)
	if drop || response == nil {
		return
	}
	_ = s.udp.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = s.udp.WriteToUDP(response, addr)
}

func (s *dnsServer) process(packet []byte, peer net.Addr, transport string) ([]byte, bool) {
	req, err := dnswire.Parse(packet)
	if err != nil {
		if len(packet) >= 2 {
			bad := dnswire.Message{
				ID:             binary.BigEndian.Uint16(packet[:2]),
				Response:       true,
				RCode:          dnswire.RCodeFormErr,
				UDPPayloadSize: 1232,
				Questions:      []dnswire.Question{},
				Answers:        []dnswire.RR{},
				Authorities:    []dnswire.RR{},
				Additionals:    []dnswire.RR{},
			}
			raw, _ := dnswire.Pack(bad)
			return raw, false
		}
		return nil, true
	}
	if req.Response || req.Opcode != 0 || len(req.Questions) != 1 {
		bad := dnswire.Message{
			ID:             req.ID,
			Response:       true,
			Opcode:         req.Opcode,
			RCode:          dnswire.RCodeFormErr,
			Questions:      req.Questions,
			UDPPayloadSize: 1232,
			Answers:        []dnswire.RR{},
			Authorities:    []dnswire.RR{},
			Additionals:    []dnswire.RR{},
		}
		raw, _ := dnswire.Pack(bad)
		return raw, false
	}
	lease, err := s.engine.acquireSlot(s.engine.ctx)
	if err != nil {
		overloaded := dnswire.Message{
			ID:                 req.ID,
			Response:           true,
			RecursionDesired:   req.RecursionDesired,
			RecursionAvailable: true,
			RCode:              dnswire.RCodeServFail,
			Questions:          req.Questions,
			UDPPayloadSize:     1232,
			Answers:            []dnswire.RR{},
			Authorities:        []dnswire.RR{},
			Additionals:        []dnswire.RR{},
		}
		addResponseMeta(&overloaded, ResponseMeta{Anomaly: "overloaded", CacheStatus: "none"})
		raw, _ := dnswire.Pack(overloaded)
		return raw, false
	}
	defer lease.Release()

	reply, drop := s.engine.answer(s.resolver, req, peer, transport, lease)
	if drop {
		return nil, true
	}
	reply.msg.ID = req.ID
	reply.msg.Response = true
	reply.msg.Opcode = req.Opcode
	reply.msg.RecursionDesired = req.RecursionDesired
	if reply.msg.RCode == 0 && reply.msg.RecursionAvailable {
		reply.msg.RecursionAvailable = true
	}
	if s.resolver.cfg.Role == RoleAuthority {
		reply.msg.Authoritative = true
	}
	if s.resolver.cfg.Role == RoleRecursive || s.resolver.cfg.Role == RoleForwarder {
		reply.msg.RecursionAvailable = true
	}
	if len(req.Options) > 0 || req.UDPPayloadSize > 0 {
		reply.msg.UDPPayloadSize = 1232
	}
	addResponseMeta(&reply.msg, ResponseMeta{
		Anomaly:      reply.anomaly,
		CacheStatus:  reply.cacheStatus,
		NextResolver: reply.nextResolver,
		NextAddress:  reply.nextAddress,
	})
	if reply.msg.Answers == nil {
		reply.msg.Answers = []dnswire.RR{}
	}
	if reply.msg.Authorities == nil {
		reply.msg.Authorities = []dnswire.RR{}
	}
	if reply.msg.Additionals == nil {
		reply.msg.Additionals = []dnswire.RR{}
	}
	raw, err := dnswire.Pack(reply.msg)
	if err != nil {
		return nil, true
	}
	if transport == "udp" {
		limit := 512
		if req.UDPPayloadSize >= 512 {
			limit = int(req.UDPPayloadSize)
		}
		if s.resolver.cfg.UDPMaxBytes > 0 && s.resolver.cfg.UDPMaxBytes < limit {
			limit = s.resolver.cfg.UDPMaxBytes
		}
		if limit > 4096 {
			limit = 4096
		}
		if s.resolver.cfg.TruncateUDP || len(raw) > limit {
			truncated := dnswire.Truncate(reply.msg)
			raw, err = dnswire.Pack(truncated)
			if err != nil {
				return nil, true
			}
		}
	}
	return raw, false
}

func waitContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
