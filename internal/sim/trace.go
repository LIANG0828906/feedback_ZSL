package sim

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"dnssim/internal/dnswire"
)

const (
	optionTraceRequest uint16 = 65001
	optionTraceAnomaly uint16 = 65002
	optionCacheStatus  uint16 = 65003
	optionNextResolver uint16 = 65004
	optionNextAddress  uint16 = 65005
	traceMetaVersion   byte   = 1
	traceFlagBypass    byte   = 1 << 0
	traceFlagNoFaults  byte   = 1 << 1
)

type traceMeta struct {
	ID       string
	Depth    int
	Attempt  int
	BudgetMS int
	Bypass   bool
	NoFaults bool
}

func encodeTraceMeta(meta traceMeta) []byte {
	data := make([]byte, 1+16+2+2+2+1)
	data[0] = traceMetaVersion
	raw, _ := hex.DecodeString(meta.ID)
	if len(raw) != 16 {
		raw = make([]byte, 16)
	}
	copy(data[1:17], raw)
	binary.BigEndian.PutUint16(data[17:19], uint16(meta.Depth))
	binary.BigEndian.PutUint16(data[19:21], uint16(meta.Attempt))
	budget := meta.BudgetMS
	if budget > 65535 {
		budget = 65535
	}
	if budget < 0 {
		budget = 0
	}
	binary.BigEndian.PutUint16(data[21:23], uint16(budget))
	if meta.Bypass {
		data[23] |= traceFlagBypass
	}
	if meta.NoFaults {
		data[23] |= traceFlagNoFaults
	}
	return data
}

func decodeTraceMeta(msg dnswire.Message) traceMeta {
	data, ok := msg.Option(optionTraceRequest)
	if !ok || len(data) != 24 || data[0] != traceMetaVersion {
		return traceMeta{}
	}
	return traceMeta{
		ID:       hex.EncodeToString(data[1:17]),
		Depth:    int(binary.BigEndian.Uint16(data[17:19])),
		Attempt:  int(binary.BigEndian.Uint16(data[19:21])),
		BudgetMS: int(binary.BigEndian.Uint16(data[21:23])),
		Bypass:   data[23]&traceFlagBypass != 0,
		NoFaults: data[23]&traceFlagNoFaults != 0,
	}
}

type ResponseMeta struct {
	Anomaly      string
	CacheStatus  string
	NextResolver string
	NextAddress  string
}

func addResponseMeta(msg *dnswire.Message, meta ResponseMeta) {
	if meta.Anomaly != "" {
		msg.AddOption(optionTraceAnomaly, []byte(meta.Anomaly))
	}
	if meta.CacheStatus != "" {
		msg.AddOption(optionCacheStatus, []byte(meta.CacheStatus))
	}
	if meta.NextResolver != "" {
		msg.AddOption(optionNextResolver, []byte(meta.NextResolver))
	}
	if meta.NextAddress != "" {
		msg.AddOption(optionNextAddress, []byte(meta.NextAddress))
	}
}

func responseMeta(msg dnswire.Message) ResponseMeta {
	var out ResponseMeta
	if data, ok := msg.Option(optionTraceAnomaly); ok {
		out.Anomaly = string(data)
	}
	if data, ok := msg.Option(optionCacheStatus); ok {
		out.CacheStatus = string(data)
	}
	if data, ok := msg.Option(optionNextResolver); ok {
		out.NextResolver = string(data)
	}
	if data, ok := msg.Option(optionNextAddress); ok {
		out.NextAddress = string(data)
	}
	return out
}

type AnswerRecord struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	TTL   uint32 `json:"ttl"`
	Value string `json:"value"`
}

type Hop struct {
	Sequence    int            `json:"sequence"`
	Depth       int            `json:"depth"`
	Resolver    string         `json:"resolver"`
	Role        string         `json:"role"`
	Server      string         `json:"server"`
	Transport   string         `json:"transport"`
	Attempt     int            `json:"attempt"`
	MessageType string         `json:"message_type"`
	RCode       string         `json:"rcode"`
	TTL         uint32         `json:"ttl"`
	Answers     []AnswerRecord `json:"answers"`
	Authority   []AnswerRecord `json:"authority"`
	Additional  []AnswerRecord `json:"additional"`
	CacheStatus string         `json:"cache_status"`
	Truncated   bool           `json:"truncated"`
	TimedOut    bool           `json:"timed_out"`
	RTTMS       float64        `json:"rtt_ms"`
	Anomaly     string         `json:"anomaly,omitempty"`
	Error       string         `json:"error,omitempty"`
}

type Attribution struct {
	Sequence   int    `json:"sequence"`
	Resolver   string `json:"resolver"`
	Role       string `json:"role"`
	Server     string `json:"server"`
	Anomaly    string `json:"anomaly"`
	Conclusion string `json:"conclusion"`
	Detail     string `json:"detail"`
}

type FinalResolution struct {
	Status  string         `json:"status"`
	RCode   string         `json:"rcode"`
	TTL     uint32         `json:"ttl"`
	Source  string         `json:"source"`
	Values  []string       `json:"values"`
	Answers []AnswerRecord `json:"answers"`
}

type AuthoritativeView struct {
	Available bool           `json:"available"`
	Status    string         `json:"status"`
	RCode     string         `json:"rcode"`
	TTL       uint32         `json:"ttl"`
	Answers   []AnswerRecord `json:"answers"`
	Values    []string       `json:"values"`
}

type TraceResult struct {
	SchemaVersion           int                `json:"schema_version"`
	Site                    string             `json:"site"`
	Domain                  string             `json:"domain"`
	QueryType               string             `json:"query_type"`
	Conclusion              string             `json:"conclusion"`
	ConsistentWithAuthority bool               `json:"consistent_with_authority"`
	MismatchCause           string             `json:"mismatch_cause"`
	Resolution              FinalResolution    `json:"resolution"`
	Attribution             *Attribution       `json:"attribution"`
	Authoritative           *AuthoritativeView `json:"authoritative,omitempty"`
	Hops                    []Hop              `json:"hops"`
}

type traceSession struct {
	mu   sync.Mutex
	hops []*Hop
}

type traceCollector struct {
	mu       sync.Mutex
	sessions map[string]*traceSession
	nextID   atomic.Uint64
}

func newTraceCollector() *traceCollector {
	return &traceCollector{sessions: map[string]*traceSession{}}
}

func (c *traceCollector) open() (string, *traceSession) {
	n := c.nextID.Add(1)
	raw := make([]byte, 16)
	binary.BigEndian.PutUint64(raw[8:], n)
	id := hex.EncodeToString(raw)
	s := &traceSession{}
	c.mu.Lock()
	c.sessions[id] = s
	c.mu.Unlock()
	return id, s
}

func (c *traceCollector) close(id string) {
	c.mu.Lock()
	delete(c.sessions, id)
	c.mu.Unlock()
}

func (c *traceCollector) append(id string, hop Hop) int {
	c.mu.Lock()
	s := c.sessions[id]
	c.mu.Unlock()
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hop.Sequence = len(s.hops) + 1
	if hop.Answers == nil {
		hop.Answers = []AnswerRecord{}
	}
	if hop.Authority == nil {
		hop.Authority = []AnswerRecord{}
	}
	if hop.Additional == nil {
		hop.Additional = []AnswerRecord{}
	}
	stored := hop
	s.hops = append(s.hops, &stored)
	return hop.Sequence
}

func (c *traceCollector) finalize(id string, sequence int, hop Hop) {
	if sequence <= 0 {
		return
	}
	c.mu.Lock()
	s := c.sessions[id]
	c.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sequence < 1 || sequence > len(s.hops) {
		return
	}
	hop.Sequence = sequence
	if hop.Answers == nil {
		hop.Answers = []AnswerRecord{}
	}
	if hop.Authority == nil {
		hop.Authority = []AnswerRecord{}
	}
	if hop.Additional == nil {
		hop.Additional = []AnswerRecord{}
	}
	s.hops[sequence-1] = &hop
}

func (s *traceSession) snapshot() []Hop {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Hop, 0, len(s.hops))
	for _, hop := range s.hops {
		copied := *hop
		copied.Answers = append([]AnswerRecord(nil), hop.Answers...)
		copied.Authority = append([]AnswerRecord(nil), hop.Authority...)
		copied.Additional = append([]AnswerRecord(nil), hop.Additional...)
		out = append(out, copied)
	}
	return out
}

func roundRTT(d time.Duration) float64 {
	v := float64(d.Microseconds()) / 1000
	return math.Round(v*1000) / 1000
}

func rcodeName(rcode uint8) string {
	switch rcode {
	case dnswire.RCodeNoError:
		return "NOERROR"
	case dnswire.RCodeFormErr:
		return "FORMERR"
	case dnswire.RCodeServFail:
		return "SERVFAIL"
	case dnswire.RCodeNXDomain:
		return "NXDOMAIN"
	case dnswire.RCodeNotImp:
		return "NOTIMP"
	case dnswire.RCodeRefused:
		return "REFUSED"
	default:
		return fmt.Sprintf("RCODE%d", rcode)
	}
}
