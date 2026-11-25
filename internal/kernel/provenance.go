package kernel

type ProvenanceStatus string

const (
	ProvenanceRecomputed ProvenanceStatus = "recomputed"
	ProvenanceReused     ProvenanceStatus = "reused"
)

type ResultProvenance struct {
	Status              ProvenanceStatus `json:"status"`
	ReasonCode          string           `json:"reason_code,omitempty"`
	InvalidationSources []string         `json:"invalidation_sources,omitempty"`
	PropagationClosure  []string         `json:"propagation_closure,omitempty"`
	CacheKey            string           `json:"cache_key,omitempty"`
}
