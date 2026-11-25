package forward

import (
	"math"
)

const (
	MinInnerOverhead = 20
	FragmentAlign    = 8
)

func ProtocolOverhead(protocol string) int64 {
	switch protocol {
	case "udp", "wireguard_udp":
		return 28 // 20 IPv4 + 8 UDP
	case "tcp":
		return 40 // 20 IPv4 + 20 TCP
	case "gre":
		return 24 // 20 IPv4 + 4 GRE
	default:
		return -1
	}
}

func CipherOverhead(suite string) (int64, bool) {
	switch suite {
	case "none":
		return 0, true
	case "aes-128-gcm", "aes-256-gcm", "chacha20-poly1305":
		return 16, true
	default:
		return 0, false
	}
}

type WidthCalc struct {
	Bits int
	Max  uint64
}

func NewWidthCalc(bits int) (*WidthCalc, *BizError) {
	switch bits {
	case 16:
		return &WidthCalc{Bits: bits, Max: math.MaxUint16}, nil
	case 32:
		return &WidthCalc{Bits: bits, Max: math.MaxUint32}, nil
	case 64:
		return &WidthCalc{Bits: bits, Max: math.MaxUint64}, nil
	default:
		return nil, NewBizError(CodeFragmentWidth, "project.integer_bits", "unsupported integer width %d", bits)
	}
}

func (w *WidthCalc) Add(a, b uint64) (uint64, *BizError) {
	if b > w.Max-a || a > w.Max {
		return 0, NewBizError(CodeFragmentOverflow, "", "%d-bit addition overflow: %d + %d", w.Bits, a, b).
			WithContext("bits", w.Bits).WithContext("left", a).WithContext("right", b)
	}
	return a + b, nil
}

func (w *WidthCalc) Mul(a, b uint64) (uint64, *BizError) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > w.Max/b {
		return 0, NewBizError(CodeFragmentOverflow, "", "%d-bit multiplication overflow: %d * %d", w.Bits, a, b).
			WithContext("bits", w.Bits).WithContext("left", a).WithContext("right", b)
	}
	return a * b, nil
}

func (w *WidthCalc) Fit(v int64) (uint64, *BizError) {
	if v < 0 {
		return 0, NewBizError(CodeFragmentOverflow, "", "negative byte value cannot fit unsigned width: %d", v)
	}
	u := uint64(v)
	if u > w.Max {
		return 0, NewBizError(CodeFragmentOverflow, "", "value %d exceeds %d-bit max %d", v, w.Bits, w.Max).
			WithContext("bits", w.Bits).WithContext("value", v).WithContext("max", w.Max)
	}
	return u, nil
}

// SaturatingAdd never wraps. It returns Max and saturated=true on overflow.
func (w *WidthCalc) SaturatingAdd(a, b uint64) (value uint64, saturated bool) {
	if a > w.Max || b > w.Max || b > w.Max-a {
		return w.Max, true
	}
	return a + b, false
}

type FragmentPiece struct {
	Index        int   `json:"index"`
	PayloadBytes int64 `json:"payload_bytes"`
	OffsetBytes  int64 `json:"offset_bytes"`
	OffsetUnits  int64 `json:"offset_8byte_units"`
	InnerBytes   int64 `json:"inner_bytes"`
	OuterBytes   int64 `json:"outer_bytes"`
}

type FragmentRequest struct {
	PacketID      string
	TunnelID      string
	PeerID        string
	PayloadBytes  int64
	MTUBytes      int64
	PathMTUBytes  int64
	OuterProtocol string
	CipherSuite   string
	IntegerBits   int
	MaxFragments  int
}

type FragmentPlan struct {
	PacketID           string          `json:"packet_id"`
	TunnelID           string          `json:"tunnel_id"`
	PeerID             string          `json:"peer_id,omitempty"`
	Needed             bool            `json:"needed"`
	PieceCount         int             `json:"piece_count"`
	Pieces             []FragmentPiece `json:"pieces"`
	PayloadBytes       int64           `json:"payload_bytes"`
	InnerHeaderBytes   int64           `json:"inner_header_bytes"`
	ProtocolOverhead   int64           `json:"protocol_overhead_bytes"`
	CipherOverhead     int64           `json:"cipher_overhead_bytes"`
	OuterOverheadBytes int64           `json:"outer_overhead_bytes"`
	BudgetMTUBytes     int64           `json:"budget_mtu_bytes"`
	InnerCapBytes      int64           `json:"inner_cap_bytes"`
	PayloadCapBytes    int64           `json:"payload_cap_bytes"`
	AlignedCapBytes    int64           `json:"aligned_cap_bytes"`
	TotalOuterBytes    int64           `json:"total_outer_bytes"`
	ToleranceBytes     int64           `json:"tolerance_bytes"`
	IntegerBits        int             `json:"integer_bits"`
	CounterValue       uint64          `json:"counter_value"`
	CounterSaturated   bool            `json:"counter_saturated"`
	RejectCode         string          `json:"reject_code,omitempty"`
	RejectReason       string          `json:"reject_reason,omitempty"`
}

func PlanFragmentation(req FragmentRequest) FragmentPlan {
	plan := FragmentPlan{
		PacketID:         req.PacketID,
		TunnelID:         req.TunnelID,
		PeerID:           req.PeerID,
		PayloadBytes:     req.PayloadBytes,
		InnerHeaderBytes: MinInnerOverhead,
		IntegerBits:      req.IntegerBits,
	}
	width, err := NewWidthCalc(req.IntegerBits)
	if err != nil {
		plan.RejectCode = err.Code
		plan.RejectReason = err.Message
		return plan
	}
	if req.MaxFragments <= 0 {
		biz := NewBizError(CodeFieldOutOfRange, "project.max_fragments", "max_fragments must be positive")
		plan.RejectCode = biz.Code
		plan.RejectReason = biz.Message
		return plan
	}
	protoOH := ProtocolOverhead(req.OuterProtocol)
	if protoOH < 0 {
		biz := NewBizError(CodeFieldConflict, "tunnel.outer_protocol", "unsupported protocol %q", req.OuterProtocol)
		plan.RejectCode = biz.Code
		plan.RejectReason = biz.Message
		return plan
	}
	cipherOH, ok := CipherOverhead(req.CipherSuite)
	if !ok {
		biz := NewBizError(CodeFieldConflict, "tunnel.cipher_suite", "unsupported cipher %q", req.CipherSuite)
		plan.RejectCode = biz.Code
		plan.RejectReason = biz.Message
		return plan
	}
	plan.ProtocolOverhead = protoOH
	plan.CipherOverhead = cipherOH
	outerOH := protoOH + cipherOH
	plan.OuterOverheadBytes = outerOH

	if req.MTUBytes < outerOH {
		biz := NewBizError(CodeMTUTooSmall, "tunnel.mtu_bytes", "mtu %d < outer overhead %d", req.MTUBytes, outerOH)
		plan.RejectCode = biz.Code
		plan.RejectReason = biz.Message
		return plan
	}
	if req.PayloadBytes < 0 {
		biz := NewBizError(CodeFieldOutOfRange, "packet.payload_bytes", "payload must be non-negative")
		plan.RejectCode = biz.Code
		plan.RejectReason = biz.Message
		return plan
	}

	budget := req.MTUBytes
	if req.PathMTUBytes > 0 && req.PathMTUBytes < budget {
		budget = req.PathMTUBytes
	}
	plan.BudgetMTUBytes = budget
	if budget < outerOH+MinInnerOverhead {
		biz := NewBizError(CodePathMTUTooSmall, "tunnel.path_mtu_bytes",
			"effective budget %d < outer overhead %d + inner header %d", budget, outerOH, MinInnerOverhead)
		plan.RejectCode = CodeMTUTooSmall
		plan.RejectReason = biz.Message
		return plan
	}
	innerCap := budget - outerOH
	payloadCap := innerCap - MinInnerOverhead
	alignedCap := payloadCap - payloadCap%FragmentAlign
	plan.InnerCapBytes = innerCap
	plan.PayloadCapBytes = payloadCap
	plan.AlignedCapBytes = alignedCap

	_, biz := width.Fit(req.PayloadBytes)
	if biz != nil {
		plan.RejectCode = biz.Code
		plan.RejectReason = biz.Message
		return plan
	}

	count := int64(1)
	needed := req.PayloadBytes > payloadCap
	if needed {
		if alignedCap <= 0 {
			biz := NewBizError(CodeMTUTooSmall, "tunnel.mtu_bytes", "aligned fragment payload capacity is zero")
			plan.RejectCode = biz.Code
			plan.RejectReason = biz.Message
			return plan
		}
		// If payload exceeds one full cap, remaining chunks use aligned non-last capacity.
		count = (req.PayloadBytes-payloadCap+alignedCap-1)/alignedCap + 1
	}
	if count > int64(req.MaxFragments) {
		biz := NewBizError(CodeFragmentTooMany, "packet.payload_bytes",
			"fragment count %d exceeds max %d", count, req.MaxFragments).
			WithContext("packet_id", req.PacketID).
			WithContext("fragment_count", count).
			WithContext("max_fragments", req.MaxFragments)
		plan.RejectCode = biz.Code
		plan.RejectReason = biz.Message
		return plan
	}

	pieces := make([]FragmentPiece, 0, count)
	var total uint64
	var offset int64
	remaining := req.PayloadBytes
	for i := int64(0); i < count; i++ {
		chunk := remaining
		if needed && i < count-1 {
			chunk = alignedCap
		}
		innerBytes := chunk + MinInnerOverhead
		outerBytes := innerBytes + outerOH
		chunkU, err := width.Fit(chunk)
		if err != nil {
			plan.RejectCode = err.Code
			plan.RejectReason = err.Message
			return plan
		}
		innerU, err := width.Add(chunkU, uint64(MinInnerOverhead))
		if err != nil {
			plan.RejectCode = err.Code
			plan.RejectReason = err.Message
			return plan
		}
		outerU, err := width.Add(innerU, uint64(outerOH))
		if err != nil {
			plan.RejectCode = err.Code
			plan.RejectReason = err.Message
			return plan
		}
		total, err = width.Add(total, outerU)
		if err != nil {
			plan.RejectCode = err.Code
			plan.RejectReason = err.Message
			return plan
		}
		pieces = append(pieces, FragmentPiece{
			Index:        int(i),
			PayloadBytes: chunk,
			OffsetBytes:  offset,
			OffsetUnits:  offset / FragmentAlign,
			InnerBytes:   innerBytes,
			OuterBytes:   outerBytes,
		})
		remaining -= chunk
		offset += chunk
	}

	counter, saturated := width.SaturatingAdd(0, total)
	plan.CounterValue = counter
	plan.CounterSaturated = saturated
	plan.Needed = needed
	plan.PieceCount = int(count)
	plan.Pieces = pieces
	plan.TotalOuterBytes = int64(total)
	return plan
}
