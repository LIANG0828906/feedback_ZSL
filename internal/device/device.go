// Package device contains a loopback sound-card simulator and its alignment
// client. The service never binds anything except 127.0.0.1.
package device

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"audioforge/internal/errs"
	"audioforge/internal/model"
	"audioforge/internal/packet"
)

type Echo struct {
	Seq       int       `json:"seq"`
	Channel   int       `json:"channel"`
	Samples   []int64   `json:"samples"`
	AppliedAt time.Time `json:"applied_at"`
	Duplicate bool      `json:"duplicate"`
}

type EchoBatch struct {
	Echoes []Echo `json:"echoes"`
}

type FaultPlan struct {
	Drop      []string         `json:"drop"`
	Duplicate []string         `json:"duplicate"`
	DelayMS   map[string]int64 `json:"delay_ms"`
}

type Service struct {
	mu         sync.Mutex
	capability model.Capability
	echoes     []Echo
	faults     FaultPlan
	server     *http.Server
	listener   net.Listener
}

func NewService(capability model.Capability) (*Service, error) {
	if err := capability.Validate(); err != nil {
		return nil, errs.New(errs.EPack, "invalid device capability: %v", err)
	}
	return &Service{
		capability: capability,
		faults: FaultPlan{
			DelayMS: make(map[string]int64),
		},
	}, nil
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/v1/packets", s.packets)
	mux.HandleFunc("/v1/faults", s.setFaults)
	mux.HandleFunc("/v1/echo", s.echo)
	mux.HandleFunc("/v1/reset", s.reset)
	return mux
}

func (s *Service) Start(port int) (string, error) {
	if port < 0 || port > 65535 {
		return "", errs.New(errs.EPack, "invalid loopback port %d", port)
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", errs.Wrap(errs.EPack, err, "bind loopback device service")
	}
	s.mu.Lock()
	s.listener = listener
	s.server = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	server := s.server
	s.mu.Unlock()
	go func() {
		_ = server.Serve(listener)
	}()
	return "http://" + listener.Addr().String(), nil
}

func (s *Service) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *Service) Close() error {
	s.mu.Lock()
	server := s.server
	s.server = nil
	s.mu.Unlock()
	if server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return server.Shutdown(ctx)
}

func (s *Service) Echoes() []Echo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Echo(nil), s.echoes...)
}

func (s *Service) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "bind": "loopback"})
}

func (s *Service) packets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, errs.New(errs.EPack, "read packet batch: %v", err))
		return
	}
	packets, err := packet.DecodeBatch(data)
	if err != nil {
		writeError(w, err)
		return
	}
	s.mu.Lock()
	faults := cloneFaults(s.faults)
	s.mu.Unlock()
	drop := keySet(faults.Drop)
	duplicate := keySet(faults.Duplicate)
	echoes := make([]Echo, 0, len(packets))
	for _, p := range packets {
		if err := packet.Validate(p, s.capability); err != nil {
			writeError(w, err)
			return
		}
		key := packetKey(p.Channel, p.Seq)
		if drop[key] {
			continue
		}
		appliedAt := time.Now().UTC()
		if delay := faults.DelayMS[key]; delay > 0 {
			appliedAt = appliedAt.Add(time.Duration(delay) * time.Millisecond)
		}
		echo := Echo{
			Seq:       p.Seq,
			Channel:   p.Channel,
			Samples:   append([]int64(nil), p.Samples...),
			AppliedAt: appliedAt,
		}
		echoes = append(echoes, echo)
		if duplicate[key] {
			duplicateEcho := echo
			duplicateEcho.Duplicate = true
			duplicateEcho.AppliedAt = echo.AppliedAt.Add(time.Microsecond)
			echoes = append(echoes, duplicateEcho)
		}
	}
	s.mu.Lock()
	s.echoes = append(s.echoes, echoes...)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, EchoBatch{Echoes: echoes})
}

func (s *Service) setFaults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var fault FaultPlan
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&fault); err != nil {
		writeError(w, errs.New(errs.EPack, "decode fault plan: %v", err))
		return
	}
	if fault.DelayMS == nil {
		fault.DelayMS = make(map[string]int64)
	}
	s.mu.Lock()
	s.faults = cloneFaults(fault)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, fault)
}

func (s *Service) echo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, EchoBatch{Echoes: s.Echoes()})
}

func (s *Service) reset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	s.echoes = nil
	s.faults = FaultPlan{DelayMS: make(map[string]int64)}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

func (c *Client) SetFaults(ctx context.Context, faults FaultPlan) error {
	body, _ := json.Marshal(faults)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/faults", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return errs.Wrap(errs.ETimeout, err, "set device faults")
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return decodeHTTPError(response)
	}
	return nil
}

func (c *Client) Reset(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/reset", nil)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return errs.Wrap(errs.ETimeout, err, "reset loopback device")
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return decodeHTTPError(response)
	}
	return nil
}

func (c *Client) Send(ctx context.Context, packets []model.Packet) (Exchange, error) {
	body, err := packet.EncodeBatch(packets)
	if err != nil {
		return Exchange{}, errs.Wrap(errs.EPack, err, "encode packet batch")
	}
	exchange := Exchange{Local: make([]LocalRecord, 0, len(packets))}
	now := time.Now().UTC()
	for _, p := range packets {
		exchange.Local = append(exchange.Local, LocalRecord{Packet: p.Clone(), SentAt: now})
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/packets", bytes.NewReader(body))
	if err != nil {
		return Exchange{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		exchange.CompletedAt = time.Now().UTC()
		return exchange, errs.Wrap(errs.ETimeout, err, "send packet batch to loopback device")
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		exchange.CompletedAt = time.Now().UTC()
		return exchange, decodeHTTPError(response)
	}
	var batch EchoBatch
	if err := json.NewDecoder(response.Body).Decode(&batch); err != nil {
		return exchange, errs.New(errs.EPack, "decode device echo: %v", err)
	}
	exchange.Echoes = batch.Echoes
	exchange.CompletedAt = time.Now().UTC()
	return exchange, nil
}

type LocalRecord struct {
	Packet model.Packet
	SentAt time.Time
}

type Exchange struct {
	Local       []LocalRecord
	Echoes      []Echo
	CompletedAt time.Time
}

type Match struct {
	Seq       int           `json:"seq"`
	Channel   int           `json:"channel"`
	FirstSeen time.Time     `json:"first_seen"`
	Latency   time.Duration `json:"latency"`
}

type Alignment struct {
	Matches []Match       `json:"matches"`
	Issues  []*errs.Error `json:"issues"`
}

func Align(exchange Exchange, timeout time.Duration, now time.Time) Alignment {
	result := Alignment{}
	if now.IsZero() {
		now = time.Now().UTC()
		if !exchange.CompletedAt.IsZero() {
			now = exchange.CompletedAt
		}
	}
	type key struct {
		channel int
		seq     int
	}
	grouped := make(map[key][]Echo)
	for _, echo := range exchange.Echoes {
		k := key{channel: echo.Channel, seq: echo.Seq}
		grouped[k] = append(grouped[k], echo)
	}
	for k := range grouped {
		sort.Slice(grouped[k], func(i, j int) bool {
			return grouped[k][i].AppliedAt.Before(grouped[k][j].AppliedAt)
		})
	}
	for _, local := range exchange.Local {
		k := key{channel: local.Packet.Channel, seq: local.Packet.Seq}
		echoes := grouped[k]
		if len(echoes) == 0 {
			if now.Sub(local.SentAt) >= timeout {
				result.Issues = append(result.Issues, &errs.Error{
					Code:      errs.EMiss,
					Message:   "packet echo missing after timeout",
					PacketSeq: local.Packet.Seq,
					Channel:   local.Packet.Channel,
					FirstSeen: local.SentAt,
					Field:     "echo",
				})
			}
			continue
		}
		first := echoes[0]
		latency := first.AppliedAt.Sub(local.SentAt)
		if latency < 0 {
			latency = 0
		}
		if first.AppliedAt.Sub(local.SentAt) > timeout {
			result.Issues = append(result.Issues, &errs.Error{
				Code:      errs.ETimeout,
				Message:   "packet echo arrived after the deadline",
				PacketSeq: local.Packet.Seq,
				Channel:   local.Packet.Channel,
				FirstSeen: first.AppliedAt,
				Field:     "applied_at",
			})
		}
		if len(echoes) > 1 || first.Duplicate {
			result.Issues = append(result.Issues, &errs.Error{
				Code:      errs.EDup,
				Message:   fmt.Sprintf("packet echoed %d times", len(echoes)),
				PacketSeq: local.Packet.Seq,
				Channel:   local.Packet.Channel,
				FirstSeen: first.AppliedAt,
				Field:     "echo",
			})
		}
		if !equalSamples(first.Samples, local.Packet.Samples) {
			result.Issues = append(result.Issues, &errs.Error{
				Code:      errs.EDiff,
				Message:   "echoed applied samples differ from local packet",
				PacketSeq: local.Packet.Seq,
				Channel:   local.Packet.Channel,
				FirstSeen: first.AppliedAt,
			})
			continue
		}
		result.Matches = append(result.Matches, Match{
			Seq:       local.Packet.Seq,
			Channel:   local.Packet.Channel,
			FirstSeen: first.AppliedAt,
			Latency:   latency,
		})
	}
	sort.Slice(result.Issues, func(i, j int) bool {
		if result.Issues[i].Channel != result.Issues[j].Channel {
			return result.Issues[i].Channel < result.Issues[j].Channel
		}
		if result.Issues[i].PacketSeq != result.Issues[j].PacketSeq {
			return result.Issues[i].PacketSeq < result.Issues[j].PacketSeq
		}
		return result.Issues[i].Code < result.Issues[j].Code
	})
	return result
}

func decodeHTTPError(response *http.Response) error {
	var coded errs.Error
	if err := json.NewDecoder(response.Body).Decode(&coded); err == nil && coded.Code != "" {
		return &coded
	}
	return errs.New(errs.EPack, "device returned HTTP %d", response.StatusCode)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var coded *errs.Error
	if value, ok := err.(*errs.Error); ok {
		coded = value
	} else {
		coded = &errs.Error{Code: errs.EPack, Message: err.Error()}
	}
	writeJSON(w, status, coded)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func cloneFaults(f FaultPlan) FaultPlan {
	out := FaultPlan{
		Drop:      append([]string(nil), f.Drop...),
		Duplicate: append([]string(nil), f.Duplicate...),
		DelayMS:   make(map[string]int64, len(f.DelayMS)),
	}
	for key, value := range f.DelayMS {
		out.DelayMS[key] = value
	}
	return out
}

func keySet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}

func packetKey(channel, seq int) string {
	return fmt.Sprintf("%d/%d", channel, seq)
}

func equalSamples(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
