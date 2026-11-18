package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	TypeHeartbeat       = "heartbeat"
	TypeRequestMapping  = "request_mapping"
	TypeProbe           = "probe"
	TypeProbeReply      = "probe_reply"
	TypeBindSource      = "bind_source"
	TypeBindSourceReply = "bind_source_reply"
	TypeData            = "data"
	TypeDataAck         = "data_ack"
	TypeResponse        = "response"
)

const (
	NATFullCone       = "full_cone"
	NATPortRestricted = "port_restricted"
	ProtocolUDP       = "udp"
	ProtocolTCP       = "tcp"
)

type Message struct {
	Type            string `json:"type"`
	GatewayID       string `json:"gateway_id,omitempty"`
	DeclaredNATType string `json:"declared_nat_type,omitempty"`
	PrivateAddr     string `json:"private_addr,omitempty"`
	Nonce           string `json:"nonce,omitempty"`
	MappingToken    string `json:"mapping_token,omitempty"`
	ForceProbe      bool   `json:"force_probe,omitempty"`
	Payload         string `json:"payload,omitempty"`
	SentUnixNano    int64  `json:"sent_unix_nano,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
}

type ProbeEvidence struct {
	Method                  string `json:"method"`
	ObservedIP              string `json:"observed_ip"`
	ObservedPort            int    `json:"observed_port"`
	PrimaryProbeReplied     bool   `json:"primary_probe_replied"`
	AlternateSourceReplied  bool   `json:"alternate_source_replied"`
	PrimaryProbeLatencyMS   int64  `json:"primary_probe_latency_ms"`
	AlternateProbeLatencyMS int64  `json:"alternate_probe_latency_ms"`
	MeasuredNATType         string `json:"measured_nat_type"`
	DeclaredNATType         string `json:"declared_nat_type"`
	TypeMismatch            bool   `json:"type_mismatch"`
	ProbeError              string `json:"probe_error,omitempty"`
	PreviousObservedIP      string `json:"previous_observed_ip,omitempty"`
	PreviousObservedPort    int    `json:"previous_observed_port,omitempty"`
	ProbedAt                string `json:"probed_at"`
}

type Response struct {
	Type                 string         `json:"type"`
	GatewayID            string         `json:"gateway_id"`
	Protocol             string         `json:"protocol,omitempty"`
	ExternalPort         int            `json:"external_port,omitempty"`
	BoundIP              string         `json:"bound_ip,omitempty"`
	BoundPort            int            `json:"bound_port,omitempty"`
	DeclaredNATType      string         `json:"declared_nat_type,omitempty"`
	MeasuredNATType      string         `json:"measured_nat_type,omitempty"`
	TypeMismatch         bool           `json:"type_mismatch"`
	ProbeEvidence        *ProbeEvidence `json:"probe_evidence,omitempty"`
	Reused               bool           `json:"reused"`
	Recovered            bool           `json:"recovered"`
	LeaseRemainingMillis int64          `json:"lease_remaining_millis"`
	MappingToken         string         `json:"mapping_token,omitempty"`
	OK                   bool           `json:"ok"`
	Error                string         `json:"error,omitempty"`
	RequestID            string         `json:"request_id,omitempty"`
}

func NormalizeNATType(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "full_cone", "full-cone", "fullcone":
		return NATFullCone
	case "port_restricted", "port-restricted", "portrestricted":
		return NATPortRestricted
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

func Validate(m Message) error {
	if strings.TrimSpace(m.GatewayID) == "" {
		return fmt.Errorf("gateway_id is required")
	}
	if m.Type == "" {
		return fmt.Errorf("type is required")
	}
	return nil
}

func Decode(data []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, Validate(m)
}

func Encode(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
