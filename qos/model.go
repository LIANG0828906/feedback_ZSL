package qos

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	RateScalePermille    int64 = 1000
	RateScalePermyriad   int64 = 10000
	defaultWorkerThreads       = 1
	defaultQueueDepth          = 1024
)

type Policy string

const (
	PolicyDrop  Policy = "drop"
	PolicyDefer Policy = "defer"
)

// RateDecl is used internally while parsing source representations.
type RateDecl struct {
	Value int64 `json:"value"`
	Scale int64 `json:"scale"`
}

type Channel struct {
	ID               string `json:"id"`
	Order            int    `json:"order"`
	RatePermyriad    int64  `json:"rate_permyriad"`
	CapacityBytes    int64  `json:"capacity_bytes"`
	InitialTokens    int64  `json:"initial_tokens_bytes"`
	Policy           Policy `json:"policy"`
	Priority         int    `json:"priority"`
	WeightPermyriad  int64  `json:"weight_permyriad"`
	QueueAgeMS       int64  `json:"queue_age_ms"`
	MaxDelayMS       int64  `json:"max_delay_ms,omitempty"`
	PriorityBoost    int    `json:"priority_boost,omitempty"`
	DSCPBoostClasses []int  `json:"dscp_boost_classes,omitempty"`
}

// Packet is immutable input. Seq is the declaration order and is used as the
// final deterministic tie-breaker.
type Packet struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	ArrivalMS int64  `json:"arrival_ms"`
	SizeBytes int64  `json:"size_bytes"`
	Priority  int    `json:"priority"`
	DSCP      *int   `json:"dscp,omitempty"`
	Seq       int    `json:"seq"`
}

type Project struct {
	Name               string    `json:"name"`
	Version            string    `json:"version"`
	TickMS             int64     `json:"tick_ms"`
	NominalPacketBytes int64     `json:"nominal_packet_bytes"`
	EgressBudgetBytes  int64     `json:"egress_budget_bytes"`
	Width              int       `json:"integer_width"`
	WorkerThreads      int       `json:"worker_threads"`
	QueueDepth         int       `json:"queue_depth"`
	HorizonMS          int64     `json:"horizon_ms"`
	Channels           []Channel `json:"channels"`
	Packets            []Packet  `json:"packets"`
	SchemaVersion      int       `json:"schema_version"`
}

func (p *Project) Channel(id string) (*Channel, int) {
	for i := range p.Channels {
		if p.Channels[i].ID == id {
			return &p.Channels[i], i
		}
	}
	return nil, -1
}

func (p *Project) ChannelOrder() []string {
	out := make([]string, len(p.Channels))
	for i := range p.Channels {
		out[i] = p.Channels[i].ID
	}
	return out
}

func CloneProject(p *Project) *Project {
	if p == nil {
		return nil
	}
	out := *p
	out.Channels = make([]Channel, len(p.Channels))
	for i, c := range p.Channels {
		out.Channels[i] = c
		if c.DSCPBoostClasses != nil {
			out.Channels[i].DSCPBoostClasses = append([]int(nil), c.DSCPBoostClasses...)
		}
	}
	out.Packets = make([]Packet, len(p.Packets))
	for i, pkt := range p.Packets {
		out.Packets[i] = pkt
		if pkt.DSCP != nil {
			d := *pkt.DSCP
			out.Packets[i].DSCP = &d
		}
	}
	return &out
}

// CanonicalDigest hashes only normalized semantics. Declared field names,
// schema version and source units deliberately do not affect the digest.
func (p *Project) CanonicalDigest() (string, error) {
	type canonicalChannel struct {
		ID              string
		Order           int
		RatePermyriad   int64
		CapacityBytes   int64
		InitialTokens   int64
		Policy          Policy
		Priority        int
		WeightPermyriad int64
		QueueAgeMS      int64
		MaxDelayMS      int64
		PriorityBoost   int
		DSCP            []int
	}
	type canonicalProject struct {
		Name               string
		Version            string
		TickMS             int64
		NominalPacketBytes int64
		EgressBudgetBytes  int64
		Width              int
		WorkerThreads      int
		QueueDepth         int
		HorizonMS          int64
		Channels           []canonicalChannel
		Packets            []Packet
	}
	c := canonicalProject{
		Name: p.Name, Version: p.Version, TickMS: p.TickMS,
		NominalPacketBytes: p.NominalPacketBytes,
		EgressBudgetBytes:  p.EgressBudgetBytes,
		Width:              p.Width, WorkerThreads: p.WorkerThreads,
		QueueDepth: p.QueueDepth, HorizonMS: p.HorizonMS,
	}
	c.Channels = make([]canonicalChannel, len(p.Channels))
	for i, ch := range p.Channels {
		c.Channels[i] = canonicalChannel{
			ID: ch.ID, Order: ch.Order, RatePermyriad: ch.RatePermyriad,
			CapacityBytes: ch.CapacityBytes, InitialTokens: ch.InitialTokens,
			Policy: ch.Policy, Priority: ch.Priority,
			WeightPermyriad: ch.WeightPermyriad, QueueAgeMS: ch.QueueAgeMS,
			MaxDelayMS: ch.MaxDelayMS, PriorityBoost: ch.PriorityBoost,
			DSCP: append([]int(nil), ch.DSCPBoostClasses...),
		}
	}
	c.Packets = make([]Packet, len(p.Packets))
	for i, pkt := range p.Packets {
		c.Packets[i] = pkt
		if pkt.DSCP != nil {
			d := *pkt.DSCP
			c.Packets[i].DSCP = &d
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", wrapError(EInternal, "canonical_digest", err, nil)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func ValidateProject(p *Project) error {
	if p == nil {
		return NewError(EInternal, "project", "project is nil", nil)
	}
	if p.TickMS <= 0 {
		return NewError(ETimeAlignment, "project.tick_ms", "tick must be positive", map[string]any{"tick_ms": p.TickMS})
	}
	if p.NominalPacketBytes <= 0 {
		return NewError(EOverflow, "project.nominal_packet_bytes", "nominal packet size must be positive", map[string]any{"value": p.NominalPacketBytes})
	}
	if p.Width != 32 && p.Width != 64 {
		return NewError(EOverflow, "project.integer_width", "integer width must be 32 or 64", map[string]any{"width": p.Width})
	}
	if p.WorkerThreads < 1 || p.WorkerThreads > 64 {
		return NewError(EThreadRange, "project.worker_threads", "worker threads must be in [1,64]", map[string]any{"worker_threads": p.WorkerThreads})
	}
	if p.QueueDepth < 1 || p.QueueDepth > 4096 {
		return NewError(EQueueBudget, "project.queue_depth", "queue depth must be in [1,4096]", map[string]any{"queue_depth": p.QueueDepth})
	}
	if p.EgressBudgetBytes <= 0 || p.EgressBudgetBytes > 1<<30 {
		return NewError(EQueueBudget, "project.egress_budget_bytes", "egress budget must be in (0,2^30]", map[string]any{"egress_budget_bytes": p.EgressBudgetBytes})
	}
	if p.HorizonMS <= 0 {
		return NewError(EHorizon, "project.horizon_ms", "horizon must be positive", map[string]any{"horizon_ms": p.HorizonMS})
	}
	if p.HorizonMS%p.TickMS != 0 {
		return NewError(ETimeAlignment, "project.horizon_ms", "horizon must align to a logical tick",
			map[string]any{"horizon_ms": p.HorizonMS, "tick_ms": p.TickMS})
	}
	if len(p.Channels) == 0 {
		return NewError(EFieldMissing, "project.channels", "at least one channel is required", nil)
	}
	maxInt := int64(1<<31 - 1)
	if p.Width == 64 {
		maxInt = 1<<63 - 1
	}
	chIDs := make(map[string]bool, len(p.Channels))
	orders := make(map[int]string, len(p.Channels))
	for i := range p.Channels {
		ch := &p.Channels[i]
		path := fmt.Sprintf("project.channels[%d]", i)
		if ch.ID == "" {
			return NewError(EFieldMissing, path+".id", "channel id is required", nil)
		}
		if chIDs[ch.ID] {
			return NewError(EFieldConflict, path+".id", "duplicate channel id", map[string]any{"channel": ch.ID})
		}
		chIDs[ch.ID] = true
		if other, ok := orders[ch.Order]; ok {
			return NewError(EFieldConflict, path+".order", "duplicate channel order", map[string]any{"order": ch.Order, "channel": ch.ID, "other": other})
		}
		orders[ch.Order] = ch.ID
		if ch.RatePermyriad < 0 {
			return NewError(ERateNegative, path+".rate", "rate must not be negative", map[string]any{"channel": ch.ID, "rate_permyriad": ch.RatePermyriad})
		}
		if ch.RatePermyriad == 0 && ch.Policy == PolicyDefer {
			return NewError(ERateZero, path+".rate", "defer policy requires a positive rate", map[string]any{"channel": ch.ID})
		}
		if ch.RatePermyriad > maxInt {
			return NewError(ERateOverflow, path+".rate", "rate exceeds declared integer width", map[string]any{"channel": ch.ID, "rate_permyriad": ch.RatePermyriad, "width": p.Width})
		}
		if ch.CapacityBytes <= 0 {
			return NewError(ECapacityNegative, path+".capacity", "bucket capacity must be positive", map[string]any{"channel": ch.ID, "capacity_bytes": ch.CapacityBytes})
		}
		if ch.CapacityBytes > maxInt {
			return NewError(EOverflow, path+".capacity", "capacity exceeds declared integer width", map[string]any{"channel": ch.ID, "capacity_bytes": ch.CapacityBytes, "width": p.Width})
		}
		if ch.InitialTokens < 0 || ch.InitialTokens > ch.CapacityBytes {
			return NewError(EWatermarkRange, path+".watermark", "initial watermark must be in [0,capacity]", map[string]any{"channel": ch.ID, "tokens": ch.InitialTokens, "capacity": ch.CapacityBytes})
		}
		if ch.Policy != PolicyDrop && ch.Policy != PolicyDefer {
			return NewError(EPolicy, path+".policy", "policy must be drop or defer", map[string]any{"channel": ch.ID, "policy": ch.Policy})
		}
		if ch.WeightPermyriad <= 0 || ch.WeightPermyriad > 1<<40 {
			return NewError(EWeightRange, path+".weight", "weight must be in (0,2^40]", map[string]any{"channel": ch.ID, "weight_permyriad": ch.WeightPermyriad})
		}
		if ch.QueueAgeMS <= 0 {
			return NewError(EDelayQueue, path+".queue_age", "queue age must be positive", map[string]any{"channel": ch.ID, "queue_age_ms": ch.QueueAgeMS})
		}
		if ch.MaxDelayMS < 0 {
			return NewError(EDelayQueue, path+".max_delay", "max delay must not be negative", map[string]any{"channel": ch.ID, "max_delay_ms": ch.MaxDelayMS})
		}
		if ch.MaxDelayMS > 0 && ch.MaxDelayMS < ch.QueueAgeMS {
			return NewError(EDelayQueue, path+".max_delay", "max delay must be zero (unbounded) or at least one queue age", map[string]any{"channel": ch.ID, "max_delay_ms": ch.MaxDelayMS, "queue_age_ms": ch.QueueAgeMS})
		}
		if ch.PriorityBoost < 0 {
			return NewError(EPriorityRange, path+".priority_boost", "priority boost must not be negative", map[string]any{"channel": ch.ID, "priority_boost": ch.PriorityBoost})
		}
		for _, d := range ch.DSCPBoostClasses {
			if d < 0 || d > 63 {
				return NewError(EDSCPRange, path+".dscp_boost_classes", "DSCP class must be in [0,63]", map[string]any{"channel": ch.ID, "dscp": d})
			}
		}
	}
	packetIDs := make(map[string]bool, len(p.Packets))
	var maxPacket int64
	for i := range p.Packets {
		pkt := &p.Packets[i]
		path := fmt.Sprintf("project.packets[%d]", i)
		if pkt.ID == "" {
			return NewError(EFieldMissing, path+".id", "packet id is required", nil)
		}
		if packetIDs[pkt.ID] {
			return NewError(EDuplicatePacket, path+".id", "duplicate packet id", map[string]any{"packet": pkt.ID})
		}
		packetIDs[pkt.ID] = true
		if !chIDs[pkt.ChannelID] {
			return NewError(EUnknownChannel, path+".channel", "packet references unknown channel", map[string]any{"packet": pkt.ID, "channel": pkt.ChannelID})
		}
		if pkt.ArrivalMS < 0 {
			return NewError(ETimeNegative, path+".arrival", "arrival time must not be negative", map[string]any{"packet": pkt.ID, "arrival_ms": pkt.ArrivalMS})
		}
		if pkt.ArrivalMS%p.TickMS != 0 {
			return NewError(ETimeAlignment, path+".arrival", "arrival must align to a logical tick", map[string]any{"packet": pkt.ID, "arrival_ms": pkt.ArrivalMS, "tick_ms": p.TickMS})
		}
		if pkt.SizeBytes <= 0 {
			return NewError(EOverflow, path+".size_bytes", "packet size must be positive", map[string]any{"packet": pkt.ID})
		}
		if pkt.SizeBytes > maxInt {
			return NewError(EOverflow, path+".size_bytes", "packet size exceeds declared integer width", map[string]any{"packet": pkt.ID, "width": p.Width})
		}
		if pkt.SizeBytes > maxPacket {
			maxPacket = pkt.SizeBytes
		}
		if pkt.DSCP != nil && (*pkt.DSCP < 0 || *pkt.DSCP > 63) {
			return NewError(EDSCPRange, path+".dscp", "DSCP class must be in [0,63]", map[string]any{"packet": pkt.ID, "dscp": *pkt.DSCP})
		}
		if ch, _ := p.Channel(pkt.ChannelID); ch != nil && ch.MaxDelayMS > 0 {
			if pkt.ArrivalMS > (1<<63-1)-ch.MaxDelayMS {
				return NewError(EOverflow, path+".arrival", "arrival plus max delay overflows", map[string]any{"packet": pkt.ID})
			}
		}
	}
	for i := range p.Channels {
		ch := &p.Channels[i]
		for j := range p.Packets {
			pkt := &p.Packets[j]
			if pkt.ChannelID == ch.ID && pkt.SizeBytes > ch.CapacityBytes {
				return NewError(EBucketTooSmall, fmt.Sprintf("project.channels[%d].capacity", i),
					"packet is larger than bucket capacity",
					map[string]any{"channel": ch.ID, "packet": pkt.ID, "packet_bytes": pkt.SizeBytes, "capacity_bytes": ch.CapacityBytes})
			}
		}
	}
	if maxPacket > p.EgressBudgetBytes {
		return NewError(EQueueBudget, "project.egress_budget_bytes",
			"egress budget is smaller than a packet and can never drain the queue",
			map[string]any{"egress_budget_bytes": p.EgressBudgetBytes, "largest_packet_bytes": maxPacket})
	}
	return nil
}

func SortedChannels(p *Project) []Channel {
	out := append([]Channel(nil), p.Channels...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}
