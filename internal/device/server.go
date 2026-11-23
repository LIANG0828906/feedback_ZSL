package device

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"

	"haptickernel/internal/packetize"
)

// Echo 是设备回显的实际施加采样点。
type Echo struct {
	Seq         int   `json:"seq"`
	Channel     int   `json:"channel"`
	StartOffset int   `json:"start_offset"`
	Samples     []int `json:"samples"`
	TimingUS    int64 `json:"timing_us"`
}

// FaultSpec 声明注入的服务侧异常（按包序号）。
type FaultSpec struct {
	MissSeqs    []int `json:"miss_seqs"`
	DupSeqs     []int `json:"dup_seqs"`
	TimeoutSeqs []int `json:"timeout_seqs"`
}

// ApplyRequest 是内核 -> 设备的提交请求。
type ApplyRequest struct {
	Packet *packetize.Packet `json:"packet"`
	Fault  FaultSpec         `json:"fault"`
}

// ApplyResponse 是设备 -> 内核的回显响应。
type ApplyResponse struct {
	OK      bool   `json:"ok"`
	Timeout bool   `json:"timeout"`
	Echoes  []Echo `json:"echoes"` // 正常 1 条；重发 2 条；缺包 0 条
}

// Server 是仅绑定回环地址的模拟设备服务。
type Server struct {
	mu   sync.Mutex
	seen map[int]int
	srv  *http.Server
	Addr string
}

func NewServer() *Server {
	return &Server{seen: map[int]int{}}
}

// Start 绑定 127.0.0.1 随机端口并启动 HTTP 服务。
func (s *Server) Start() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	s.Addr = "http://" + ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("/apply", s.handleApply)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	s.srv = &http.Server{Handler: mux}
	go func() { _ = s.srv.Serve(ln) }()
	return s.Addr, nil
}

func (s *Server) Close() error {
	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}

func contains(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var req ApplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pkt := req.Packet
	resp := ApplyResponse{OK: true}
	echo := Echo{
		Seq: pkt.Seq, Channel: pkt.Channel, StartOffset: pkt.StartOffset,
		Samples: pkt.Samples, TimingUS: pkt.TimingUS,
	}
	switch {
	case contains(req.Fault.TimeoutSeqs, pkt.Seq):
		// 超时：设备不回显有效数据，用标记位表达，逻辑时间仍可确定复现。
		resp.OK = false
		resp.Timeout = true
	case contains(req.Fault.MissSeqs, pkt.Seq):
		resp.OK = true // 缺包表现为零条回显
	case contains(req.Fault.DupSeqs, pkt.Seq):
		s.mu.Lock()
		s.seen[pkt.Seq]++
		s.mu.Unlock()
		resp.Echoes = []Echo{echo, echo}
	default:
		resp.Echoes = []Echo{echo}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// Client 是回环设备客户端。
type Client struct {
	BaseURL string
	Client  *http.Client
}

func (c *Client) Submit(pkt *packetize.Packet, fault FaultSpec) (*ApplyResponse, error) {
	body, err := json.Marshal(ApplyRequest{Packet: pkt, Fault: fault})
	if err != nil {
		return nil, err
	}
	resp, err := c.Client.Post(c.BaseURL+"/apply", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out ApplyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
