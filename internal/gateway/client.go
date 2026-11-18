package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"parkrelay/internal/natsim"
	"parkrelay/internal/protocol"
)

type Client struct {
	ID              string
	DeclaredNATType string
	PrivateAddr     string
	NAT             *natsim.GatewayEndpoint
	ControlUDP      string
	ControlTCP      string

	mu         sync.Mutex
	udpMapping *protocol.Response
	tcpMapping *protocol.Response
	tcpData    net.Conn
	tcpDataMu  sync.Mutex
	seq        uint64
	udpRPC     sync.Mutex
	stopCh     chan struct{}
	stopOnce   sync.Once
	udpRespCh  chan protocol.Response
	udpDataAck chan protocol.Response
	tcpReq     sync.Mutex
}

func New(id, declared, privateAddr string, nat *natsim.GatewayEndpoint, controlUDP, controlTCP string) *Client {
	g := &Client{
		ID: id, DeclaredNATType: protocol.NormalizeNATType(declared), PrivateAddr: privateAddr,
		NAT: nat, ControlUDP: controlUDP, ControlTCP: controlTCP,
		stopCh: make(chan struct{}), udpRespCh: make(chan protocol.Response, 64),
		udpDataAck: make(chan protocol.Response, 64),
	}
	nat.SetUDPReceiver(g.onUDP)
	nat.SetTCPReceiver(g.onPublicTCP)
	return g
}

func (g *Client) StartHeartbeat(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-g.stopCh:
				return
			case <-ticker.C:
				_, _, _ = g.HeartbeatBoth(false)
			}
		}
	}()
}

func (g *Client) Stop() {
	g.stopOnce.Do(func() {
		close(g.stopCh)
		g.tcpDataMu.Lock()
		if g.tcpData != nil {
			_ = g.tcpData.Close()
			g.tcpData = nil
		}
		g.tcpDataMu.Unlock()
	})
}

func (g *Client) nextRequestID() string {
	g.seq++
	return fmt.Sprintf("%s-%d", g.ID, g.seq)
}

func (g *Client) onUDP(payload []byte, source net.Addr) {
	var msg protocol.Message
	if json.Unmarshal(payload, &msg) != nil {
		return
	}
	switch msg.Type {
	case protocol.TypeProbe:
		reply := protocol.Message{
			Type: protocol.TypeProbeReply, GatewayID: g.ID, Nonce: msg.Nonce,
		}
		_ = g.NAT.SendUDP(protocol.Encode(reply), source.String())
	case protocol.TypeResponse, protocol.TypeBindSourceReply:
		var resp protocol.Response
		if json.Unmarshal(payload, &resp) == nil {
			select {
			case g.udpRespCh <- resp:
			default:
			}
		}
	case protocol.TypeDataAck:
		var resp protocol.Response
		if json.Unmarshal(payload, &resp) == nil {
			select {
			case g.udpDataAck <- resp:
			default:
			}
		}
	}
}

func (g *Client) onPublicTCP(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	buf := make([]byte, 512)
	_, _ = conn.Read(buf)
}

func (g *Client) udpRequest(msg protocol.Message) (protocol.Response, error) {
	g.udpRPC.Lock()
	defer g.udpRPC.Unlock()
	msg.GatewayID = g.ID
	msg.DeclaredNATType = g.DeclaredNATType
	msg.PrivateAddr = g.PrivateAddr
	msg.RequestID = g.nextRequestID()
	if err := g.NAT.SendUDP(protocol.Encode(msg), g.ControlUDP); err != nil {
		return protocol.Response{}, err
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case resp := <-g.udpRespCh:
			if resp.RequestID == msg.RequestID {
				return resp, nil
			}
		case <-deadline.C:
			return protocol.Response{}, fmt.Errorf("udp request %s timed out", msg.Type)
		case <-g.stopCh:
			return protocol.Response{}, net.ErrClosed
		}
	}
}

func (g *Client) tcpRequest(msg protocol.Message) (protocol.Response, error) {
	g.tcpReq.Lock()
	defer g.tcpReq.Unlock()
	msg.GatewayID = g.ID
	msg.DeclaredNATType = g.DeclaredNATType
	msg.PrivateAddr = g.PrivateAddr
	msg.RequestID = g.nextRequestID()
	conn, err := g.NAT.DialTCP(g.ControlTCP, time.Second)
	if err != nil {
		return protocol.Response{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		return protocol.Response{}, err
	}
	var resp protocol.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return protocol.Response{}, err
	}
	return resp, nil
}

func (g *Client) RequestUDP(force bool) (protocol.Response, error) {
	resp, err := g.udpRequest(protocol.Message{Type: protocol.TypeRequestMapping, ForceProbe: force})
	if err == nil && resp.OK {
		g.mu.Lock()
		r := resp
		g.udpMapping = &r
		g.mu.Unlock()
	}
	return resp, err
}

func (g *Client) RequestTCP(force bool) (protocol.Response, error) {
	resp, err := g.tcpRequest(protocol.Message{Type: protocol.TypeRequestMapping, ForceProbe: force})
	if err == nil && resp.OK {
		g.mu.Lock()
		r := resp
		g.tcpMapping = &r
		g.mu.Unlock()
	}
	return resp, err
}

func (g *Client) HeartbeatBoth(force bool) (protocol.Response, protocol.Response, error) {
	u, uerr := g.udpRequest(protocol.Message{Type: protocol.TypeHeartbeat, ForceProbe: force})
	t, terr := g.tcpRequest(protocol.Message{Type: protocol.TypeHeartbeat, ForceProbe: force})
	if uerr != nil {
		return u, t, uerr
	}
	if terr != nil {
		return u, t, terr
	}
	return u, t, nil
}

func (g *Client) Mapping(proto string) (protocol.Response, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var p *protocol.Response
	if proto == protocol.ProtocolUDP {
		p = g.udpMapping
	} else {
		p = g.tcpMapping
	}
	if p == nil {
		return protocol.Response{}, false
	}
	return *p, true
}

func (g *Client) SetMappingFromResponse(resp protocol.Response) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := resp
	if resp.Protocol == protocol.ProtocolUDP {
		g.udpMapping = &r
	} else {
		g.tcpMapping = &r
	}
}

func (g *Client) SendUDPData(payload string) (time.Duration, error) {
	m, ok := g.Mapping(protocol.ProtocolUDP)
	if !ok {
		return 0, fmt.Errorf("%s has no udp mapping", g.ID)
	}
	start := time.Now()
	msg := protocol.Message{
		Type: protocol.TypeData, GatewayID: g.ID, Payload: payload,
		SentUnixNano: start.UnixNano(),
	}
	if err := g.NAT.SendUDP(protocol.Encode(msg), net.JoinHostPort("127.0.0.1", fmt.Sprint(m.ExternalPort))); err != nil {
		return 0, err
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case ack := <-g.udpDataAck:
			if ack.OK && ack.Protocol == protocol.ProtocolUDP && ack.GatewayID == g.ID {
				return time.Since(start), nil
			}
		case <-timer.C:
			return 0, fmt.Errorf("udp data ack timed out")
		case <-g.stopCh:
			return 0, net.ErrClosed
		}
	}
}

func (g *Client) BindTCPData() error {
	m, ok := g.Mapping(protocol.ProtocolTCP)
	if !ok {
		return fmt.Errorf("%s has no tcp mapping", g.ID)
	}
	conn, err := g.NAT.DialTCP(net.JoinHostPort("127.0.0.1", fmt.Sprint(m.ExternalPort)), time.Second)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	bind := protocol.Message{
		Type: protocol.TypeBindSource, GatewayID: g.ID, MappingToken: m.MappingToken,
	}
	if err := json.NewEncoder(conn).Encode(bind); err != nil {
		conn.Close()
		return err
	}
	var resp protocol.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		conn.Close()
		return err
	}
	if !resp.OK {
		conn.Close()
		return fmt.Errorf("tcp source bind rejected: %s", resp.Error)
	}
	_ = conn.SetDeadline(time.Time{})
	g.tcpDataMu.Lock()
	if g.tcpData != nil {
		_ = g.tcpData.Close()
	}
	g.tcpData = conn
	g.tcpDataMu.Unlock()
	return nil
}

func (g *Client) SendTCPData(payload string) (time.Duration, error) {
	g.tcpDataMu.Lock()
	conn := g.tcpData
	g.tcpDataMu.Unlock()
	if conn == nil {
		return 0, fmt.Errorf("tcp data channel is not bound")
	}
	start := time.Now()
	msg := protocol.Message{Type: protocol.TypeData, GatewayID: g.ID, Payload: payload, SentUnixNano: start.UnixNano()}
	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 4096), 1024*1024)
	if !sc.Scan() {
		return 0, fmt.Errorf("tcp data ack read failed")
	}
	var resp protocol.Response
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return 0, err
	}
	if resp.Type != protocol.TypeDataAck || !resp.OK {
		return 0, fmt.Errorf("unexpected tcp data response: %s", resp.Error)
	}
	return time.Since(start), nil
}

func (g *Client) RotateNATEndpoint() error {
	return g.NAT.RotatePublicEndpoint()
}
