package scenario

import (
	"bufio"
	"net"
	"sync"
	"sync/atomic"
)

type forwardingSinks struct {
	udp      net.PacketConn
	tcp      net.Listener
	udpCount atomic.Int64
	tcpCount atomic.Int64
	close    sync.Once
}

func newForwardingSinks() (*forwardingSinks, error) {
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_ = udp.Close()
		return nil, err
	}
	s := &forwardingSinks{udp: udp, tcp: tcp}
	go s.readUDP()
	go s.acceptTCP()
	return s, nil
}

func (s *forwardingSinks) UDPAddr() string { return s.udp.LocalAddr().String() }
func (s *forwardingSinks) TCPAddr() string { return s.tcp.Addr().String() }

func (s *forwardingSinks) readUDP() {
	buf := make([]byte, 64*1024)
	for {
		if _, _, err := s.udp.ReadFrom(buf); err != nil {
			return
		}
		s.udpCount.Add(1)
	}
}

func (s *forwardingSinks) acceptTCP() {
	for {
		conn, err := s.tcp.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			sc := bufio.NewScanner(conn)
			sc.Buffer(make([]byte, 4096), 1024*1024)
			for sc.Scan() {
				s.tcpCount.Add(1)
			}
		}()
	}
}

func (s *forwardingSinks) Snapshot() ForwardedToSink {
	return ForwardedToSink{UDPPackets: s.udpCount.Load(), TCPPackets: s.tcpCount.Load()}
}

func (s *forwardingSinks) Close() {
	s.close.Do(func() {
		_ = s.udp.Close()
		_ = s.tcp.Close()
	})
}
