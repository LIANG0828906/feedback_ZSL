package relay

import "parkrelay/internal/protocol"

type StatusMapping struct {
	GatewayID               string                 `json:"gateway_id"`
	Protocol                string                 `json:"protocol"`
	ExternalPort            int                    `json:"external_port"`
	BoundIP                 string                 `json:"bound_ip"`
	BoundPort               int                    `json:"bound_port"`
	Active                  bool                   `json:"active"`
	Draining                bool                   `json:"draining"`
	DeclaredNATType         string                 `json:"declared_nat_type"`
	MeasuredNATType         string                 `json:"measured_nat_type"`
	TypeMismatch            bool                   `json:"type_mismatch"`
	ProbeEvidence           protocol.ProbeEvidence `json:"probe_evidence"`
	LastHeartbeatUnixMillis int64                  `json:"last_heartbeat_unix_millis"`
	LeaseExpiresUnixMillis  int64                  `json:"lease_expires_unix_millis"`
	LeaseRemainingMillis    int64                  `json:"lease_remaining_millis"`
	ForwardedPackets        uint64                 `json:"forwarded_packets"`
	SourceMismatchDrops     uint64                 `json:"source_mismatch_drops"`
	RecoveredFromSQLite     bool                   `json:"recovered_from_sqlite"`
}

type RecycleStatus struct {
	ID                   int64  `json:"id"`
	Protocol             string `json:"protocol"`
	ExternalPort         int    `json:"external_port"`
	Reason               string `json:"reason"`
	RecycledAtUnixMillis int64  `json:"recycled_at_unix_millis"`
	CooldownUntilMillis  int64  `json:"cooldown_until_unix_millis"`
}

type GatewayCounters struct {
	SourceMismatchDrops  int64 `json:"source_mismatch_drops"`
	NATTypeMismatchCount int64 `json:"nat_type_mismatch_count"`
	RecycleCount         int64 `json:"recycle_count"`
}

type GatewayStatus struct {
	GatewayID      string                  `json:"gateway_id"`
	ActiveMappings int                     `json:"active_mappings"`
	Mappings       []StatusMapping         `json:"mappings"`
	LastProbe      *protocol.ProbeEvidence `json:"last_probe,omitempty"`
	LastRecycle    *RecycleStatus          `json:"last_recycle,omitempty"`
	Counters       GatewayCounters         `json:"counters"`
}

type StatusResponse struct {
	NowUnixMillis int64           `json:"now_unix_millis"`
	Gateways      []GatewayStatus `json:"gateways"`
}
