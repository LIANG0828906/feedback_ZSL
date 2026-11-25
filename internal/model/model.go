package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

const (
	CanonicalFormat = "tls-probe-canonical/v1"

	SchemaV1 = "probe.project.v1"
	SchemaV2 = "probe.project.v2"
	SchemaV3 = "probe.project.v3"

	KindHandshake     = "handshake_request"
	KindChainUpdate   = "chain_update"
	StatusCommitted   = "COMMITTED"
	StatusInterrupted = "INTERRUPTED"
)

type Project struct {
	Format       string     `json:"format"`
	ProjectID    string     `json:"project_id"`
	Seed         int64      `json:"seed"`
	TickMs       int64      `json:"-"`
	ThreadBudget int        `json:"thread_budget"`
	Endpoints    []Endpoint `json:"endpoints"`
	Chains       []Chain    `json:"chains"`
	Events       []Event    `json:"events"`
	SourceSchema string     `json:"source_schema,omitempty"`
}

type Endpoint struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	MinProtocol        string   `json:"min_protocol"`
	MaxProtocol        string   `json:"max_protocol"`
	CipherSuites       []string `json:"cipher_suites"`
	PeriodMs           int64    `json:"period_ms"`
	TimeoutMs          int64    `json:"timeout_ms"`
	MaxParallel        int      `json:"max_parallel"`
	BufferCapacity     int      `json:"buffer_capacity"`
	BudgetBP           int64    `json:"budget_bp"`
	ChainIDs           []string `json:"chain_ids"`
	HandshakeWatermark int      `json:"handshake_watermark,omitempty"`
	WatermarkSet       bool     `json:"watermark_set,omitempty"`
	Pinned             bool     `json:"pinned,omitempty"`
	PinDigest          string   `json:"pin_digest,omitempty"`
}

type Chain struct {
	ID           string        `json:"id"`
	Certificates []Certificate `json:"certificates"`
	Pinned       bool          `json:"pinned,omitempty"`
	PinDigest    string        `json:"pin_digest,omitempty"`
}

type Certificate struct {
	Digest           string `json:"digest"`
	Subject          string `json:"subject"`
	Issuer           string `json:"issuer"`
	NotBeforeMs      int64  `json:"not_before_ms"`
	NotAfterMs       int64  `json:"not_after_ms"`
	ValidationCostBP int64  `json:"validation_cost_bp"`
}

type Event struct {
	Sequence int          `json:"sequence"`
	Kind     string       `json:"kind"`
	AtMs     int64        `json:"at_ms"`
	Request  *Request     `json:"request,omitempty"`
	Update   *ChainUpdate `json:"update,omitempty"`
}

type Request struct {
	ID                  string   `json:"id"`
	EndpointID          string   `json:"endpoint_id"`
	OfferedProtocol     string   `json:"offered_protocol"`
	OfferedCiphers      []string `json:"offered_ciphers"`
	DurationMs          int64    `json:"duration_ms"`
	InterruptAfterLevel int      `json:"interrupt_after_level,omitempty"`
}

type ChainUpdate struct {
	ID                    string `json:"id"`
	EndpointID            string `json:"endpoint_id"`
	ChainID               string `json:"chain_id"`
	CommitAtMs            int64  `json:"commit_at_ms"`
	InterruptBeforeCommit bool   `json:"interrupt_before_commit,omitempty"`
}

func (p Project) CanonicalHash() (string, error) {
	cp := p
	cp.Format = CanonicalFormat
	cp.SourceSchema = ""
	b, err := json.Marshal(cp)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (p Project) Endpoint(id string) (Endpoint, bool) {
	for _, ep := range p.Endpoints {
		if ep.ID == id {
			return ep, true
		}
	}
	return Endpoint{}, false
}

func (p Project) Chain(id string) (Chain, bool) {
	for _, ch := range p.Chains {
		if ch.ID == id {
			return ch, true
		}
	}
	return Chain{}, false
}

func SortedEventIndexes(events []Event) []int {
	idx := make([]int, len(events))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		x, y := events[idx[a]], events[idx[b]]
		if x.AtMs != y.AtMs {
			return x.AtMs < y.AtMs
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		return x.Sequence < y.Sequence
	})
	return idx
}

func CloneProject(in Project) Project {
	b, _ := json.Marshal(in)
	var out Project
	_ = json.Unmarshal(b, &out)
	return out
}
