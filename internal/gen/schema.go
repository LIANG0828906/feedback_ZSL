package gen

// This file defines the three raw schema representations. They intentionally
// differ in field naming, list shape, units, weight scale, time unit and
// optional soft-timeout presence, exercising every loader mapping branch.

// ---------- Schema v1: snake_case, flat tables, cores, permille, ticks ------

type V1Project struct {
	SchemaVersion     string      `json:"schema_version"`
	ProjectID         string      `json:"project_id"`
	ReferenceCPU      V1Resources `json:"reference_cpu"`
	TopologySemantics []V1TopoSem `json:"topology_semantics"`
	Nodes             []V1Node    `json:"nodes"`
	Pods              []V1Pod     `json:"pods"`
}

type V1TopoSem struct {
	Key      string `json:"topology_key"`
	Semantic string `json:"semantic"`
}

type V1Resources struct {
	CPUCores int64 `json:"cpu_cores"`
	MemBytes int64 `json:"mem_bytes"`
}

type V1Node struct {
	NodeID      string            `json:"node_id"`
	Allocatable V1Resources       `json:"allocatable"`
	Labels      map[string]string `json:"labels"`
	Topology    map[string]string `json:"topology"`
	Taints      []V1Taint         `json:"taints"`
}

type V1Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

type V1Pod struct {
	PodID           string            `json:"pod_id"`
	Request         V1Resources       `json:"request"`
	NodeSelector    map[string]string `json:"node_selector"`
	Tolerations     []V1Toleration    `json:"tolerations"`
	MustRules       []V1Rule          `json:"must_rules"`
	PreferRules     []V1Rule          `json:"prefer_rules"`
	SoftTimeoutTick int64             `json:"soft_timeout_tick"`
}

type V1Toleration struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
	Effect   string `json:"effect"`
}

type V1Rule struct {
	RuleID         string            `json:"rule_id"`
	Mode           string            `json:"mode"`
	MatchLabels    map[string]string `json:"match_labels"`
	TopologyKey    string            `json:"topology_key"`
	WeightPermille int64             `json:"weight_permille"`
	MaxColocated   int64             `json:"max_colocated"`
}

// ---------- Schema v2: camelCase, nested topology tree, strings, bps, ms -----

type V2Project struct {
	SchemaVersion     string      `json:"schemaVersion"`
	ProjectID         string      `json:"projectId"`
	ReferenceCPU      V2Resources `json:"referenceCpu"`
	TopologySemantics []V2TopoSem `json:"topologySemantics"`
	ZoneTree          []V2Zone    `json:"zoneTree"`
	Pods              []V2Pod     `json:"pods"`
}

type V2TopoSem struct {
	TopologyKey string `json:"topologyKey"`
	Semantic    string `json:"semantic"`
}

type V2Resources struct {
	CPU string `json:"cpu"`
	Mem string `json:"mem"`
}

type V2Zone struct {
	ZoneName string   `json:"zoneName"`
	Racks    []V2Rack `json:"racks"`
}

type V2Rack struct {
	RackName string   `json:"rackName"`
	Nodes    []V2Node `json:"nodes"`
}

type V2Node struct {
	NodeID      string            `json:"nodeId"`
	Allocatable V2Resources       `json:"allocatable"`
	Labels      map[string]string `json:"labels"`
	Topology    map[string]string `json:"topology"`
	Taints      []V2Taint         `json:"taints"`
}

type V2Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

type V2Pod struct {
	PodID         string            `json:"podId"`
	Request       V2Resources       `json:"request"`
	NodeSelector  map[string]string `json:"nodeSelector"`
	Tolerations   []V2Toleration    `json:"tolerations"`
	MustRules     []V2Rule          `json:"mustRules"`
	PreferRules   []V2Rule          `json:"preferRules"`
	SoftTimeoutMs int64             `json:"softTimeoutMs"`
}

type V2Toleration struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
	Effect   string `json:"effect"`
}

type V2Rule struct {
	RuleID       string            `json:"ruleId"`
	Mode         string            `json:"mode"`
	MatchLabels  map[string]string `json:"matchLabels"`
	TopologyKey  string            `json:"topologyKey"`
	WeightBps    int64             `json:"weightBps"`
	MaxColocated int64             `json:"maxColocated"`
}

// ---------- Schema v3: kebab-case, flat, milli tokens / ratio, bps, ms -------

type V3Project struct {
	SchemaVersion     string      `json:"schema-version"`
	ProjectID         string      `json:"project-id"`
	ReferenceCPU      V3Resources `json:"reference-cpu"`
	TopologySemantics []V3TopoSem `json:"topology-semantics"`
	Nodes             []V3Node    `json:"nodes"`
	Pods              []V3Pod     `json:"pods"`
}

type V3TopoSem struct {
	TopologyKey string `json:"topology-key"`
	Semantic    string `json:"semantic"`
}

type V3Resources struct {
	CPU string `json:"cpu"`
	Mem string `json:"mem"`
}

type V3Node struct {
	NodeID      string            `json:"node-id"`
	Allocatable V3Resources       `json:"allocatable"`
	Labels      map[string]string `json:"labels"`
	Topology    map[string]string `json:"topology"`
	Taints      []V3Taint         `json:"taints"`
}

type V3Taint struct {
	Key    string `json:"key"`
	Effect string `json:"effect"`
	Value  string `json:"value"`
}

type V3Pod struct {
	PodID        string            `json:"pod-id"`
	Request      V3Resources       `json:"request"`
	NodeSelector map[string]string `json:"node-selector"`
	Tolerations  []V3Toleration    `json:"tolerations"`
	MustRules    []V3Rule          `json:"must-rules"`
	PreferRules  []V3Rule          `json:"prefer-rules"`
	// Soft timeout intentionally omitted in v3.
}

type V3Toleration struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
	Effect   string `json:"effect"`
}

type V3Rule struct {
	RuleID       string            `json:"rule-id"`
	Mode         string            `json:"mode"`
	MatchLabels  map[string]string `json:"match-labels"`
	TopologyKey  string            `json:"topology-key"`
	WeightBps    int64             `json:"weight-bps"`
	MaxColocated int64             `json:"max-co-located"`
}
