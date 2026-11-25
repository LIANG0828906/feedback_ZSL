package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// LoadProject decodes one of the three supported wire schemas and maps it to
// the canonical in-memory model. The returned project has already been
// normalized, so all maps can be traversed in stable order.
func LoadProject(data []byte) (*Project, error) {
	var envelope struct {
		SchemaVersion string `json:"schema_version"`
		Schema        string `json:"schema"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, newError(CodeInvalidJSON, "$", "%v", err)
	}
	versionField := identifyVersion(envelope.SchemaVersion)
	schemaField := identifyVersion(envelope.Schema)
	if versionField != "" && schemaField != "" && versionField != schemaField {
		return nil, newError(CodeUnsupportedSchema, "$.schema", "conflicting schema declarations %q and %q", envelope.SchemaVersion, envelope.Schema)
	}
	version := versionField
	if version == "" {
		version = schemaField
	}
	var (
		project *Project
		err     error
	)
	switch version {
	case "v1":
		project, err = loadV1(data)
	case "v2":
		project, err = loadV2(data)
	case "v3":
		project, err = loadV3(data)
	default:
		return nil, newError(CodeUnsupportedSchema, "$.schema", "unsupported schema version combination %q", strings.TrimSpace(envelope.SchemaVersion+" "+envelope.Schema))
	}
	if err != nil {
		return nil, err
	}
	if err := project.Normalize(); err != nil {
		return nil, err
	}
	return project, nil
}

func identifyVersion(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	value = strings.ReplaceAll(value, "_", ".")
	value = strings.ReplaceAll(value, "/", ".")
	value = strings.ReplaceAll(value, "-", ".")
	value = strings.Trim(value, ".")
	switch value {
	case "v1", "1", "cluster.v1", "cluster.v1.flat", "cluster.v1.ms.absolute":
		return "v1"
	case "v2", "2", "cluster.v2", "cluster.v2.tree", "cluster.v2.tick.relative":
		return "v2"
	case "v3", "3", "cluster.v3", "cluster.v3.canonical", "cluster.v3.flat.relative":
		return "v3"
	}
	if strings.HasPrefix(value, "cluster.v1.") || strings.HasPrefix(value, "cluster.v2.") || strings.HasPrefix(value, "cluster.v3.") {
		return value[len("cluster.") : len("cluster.")+2]
	}
	return ""
}

type v1Clock struct {
	TickMillisNum uint64 `json:"tick_millis_num"`
	TickMillisDen uint64 `json:"tick_millis_den"`
}

type v1Workload struct {
	ID          string `json:"id"`
	BudgetBytes int64  `json:"budget_bytes"`
	UsedBytes   int64  `json:"used_bytes"`
}

type v1Node struct {
	ID               string       `json:"id"`
	NodeID           string       `json:"node_id"`
	Rack             string       `json:"rack"`
	ZoneIDAbs        *int64       `json:"zone_id_abs"`
	ZoneID           *int64       `json:"zone_id"`
	Workloads        []v1Workload `json:"workloads"`
	IsolationTimeout *uint64      `json:"isolation_timeout_ms,omitempty"`
	TimeoutMS        *uint64      `json:"timeout_ms,omitempty"`
}

type v1Edge struct {
	FromNode string `json:"from_node"`
	ToNode   string `json:"to_node"`
}

type v1Event struct {
	AtMS        *uint64 `json:"at_ms"`
	AtTick      *int64  `json:"at_tick"`
	Seq         int     `json:"seq"`
	Kind        string  `json:"kind"`
	NodeID      string  `json:"node_id"`
	DomainID    *int64  `json:"domain_id"`
	WorkloadID  string  `json:"workload_id"`
	AmountBytes int64   `json:"amount_bytes"`
}

type v1Project struct {
	SchemaVersion      string    `json:"schema_version"`
	ProjectID          string    `json:"project_id"`
	SnapshotID         string    `json:"snapshot_id"`
	Revision           string    `json:"revision"`
	Clock              v1Clock   `json:"clock"`
	TimeoutBits        uint8     `json:"timeout_bits"`
	DefaultTimeoutMS   *uint64   `json:"default_timeout_ms"`
	Nodes              []v1Node  `json:"nodes"`
	Edges              []v1Edge  `json:"edges"`
	Events             []v1Event `json:"events"`
	WorkerBudget       int       `json:"worker_budget"`
	PerDomainQueueSize int       `json:"per_domain_pending_limit"`
}

func loadV1(data []byte) (*Project, error) {
	var raw v1Project
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, newError(CodeInvalidJSON, "$", "%v", err)
	}
	if raw.ProjectID == "" {
		return nil, newError(CodeInvalidSchema, "$.project_id", "project id is required")
	}
	project := &Project{
		SchemaVersion:         "v1",
		ProjectID:             raw.ProjectID,
		SnapshotID:            firstNonEmpty(raw.SnapshotID, raw.Revision, raw.ProjectID),
		Revision:              firstNonEmpty(raw.Revision, raw.SnapshotID, raw.ProjectID),
		TickMillis:            Ratio{Num: raw.Clock.TickMillisNum, Den: raw.Clock.TickMillisDen},
		TimeoutBits:           raw.TimeoutBits,
		WorkerBudget:          raw.WorkerBudget,
		PerDomainPendingLimit: raw.PerDomainQueueSize,
	}
	if project.TickMillis.Num == 0 || project.TickMillis.Den == 0 {
		return nil, newError(CodeInvalidSchema, "$.clock", "tick_millis_num and tick_millis_den are required")
	}
	if err := project.TickMillis.normalize(); err != nil {
		return nil, err
	}
	if project.TimeoutBits == 0 {
		project.TimeoutBits = 64
	}
	if raw.DefaultTimeoutMS != nil {
		project.DefaultTimeout = Timeout{Set: true, Wire: *raw.DefaultTimeoutMS, Unit: UnitMS}
	}
	for i, node := range raw.Nodes {
		path := fmt.Sprintf("$.nodes[%d]", i)
		id := firstNonEmpty(node.ID, node.NodeID)
		if id == "" {
			return nil, newError(CodeInvalidSchema, path+".id", "node id is required")
		}
		zone := node.ZoneIDAbs
		if zone == nil {
			zone = node.ZoneID
		}
		if zone == nil {
			return nil, newError(CodeInvalidSchema, path+".zone_id_abs", "absolute zone id is required for v1")
		}
		timeoutRaw := node.IsolationTimeout
		if timeoutRaw == nil {
			timeoutRaw = node.TimeoutMS
		}
		timeout, err := bindTimeout(project, timeoutRaw, UnitMS, path+".isolation_timeout_ms")
		if err != nil {
			return nil, err
		}
		canonicalNode := Node{ID: id, Rack: node.Rack, Zone: *zone, Timeout: timeout}
		for j, workload := range node.Workloads {
			if workload.ID == "" {
				return nil, newError(CodeInvalidSchema, fmt.Sprintf("%s.workloads[%d].id", path, j), "workload id is required")
			}
			canonicalNode.Workloads = append(canonicalNode.Workloads, Workload{
				ID:          workload.ID,
				BudgetBytes: workload.BudgetBytes,
				UsedBytes:   workload.UsedBytes,
			})
		}
		project.Nodes = append(project.Nodes, canonicalNode)
	}
	for i, edge := range raw.Edges {
		if edge.FromNode == "" || edge.ToNode == "" {
			return nil, newError(CodeInvalidSchema, fmt.Sprintf("$.edges[%d]", i), "from_node and to_node are required")
		}
		project.Edges = append(project.Edges, Edge{From: edge.FromNode, To: edge.ToNode})
	}
	for i, event := range raw.Events {
		tick, err := eventTick(event.AtTick, event.AtMS, project.TickMillis, fmt.Sprintf("$.events[%d]", i))
		if err != nil {
			return nil, err
		}
		kind, err := normalizeKind(event.Kind)
		if err != nil {
			return nil, err
		}
		domain := copyInt64(event.DomainID)
		project.Events = append(project.Events, Event{
			Tick:        tick,
			Seq:         event.Seq,
			Kind:        kind,
			NodeID:      event.NodeID,
			DomainID:    domain,
			WorkloadID:  event.WorkloadID,
			AmountBytes: event.AmountBytes,
		})
	}
	return project, nil
}

type v2Meta struct {
	ProjectID     string  `json:"project_id"`
	SnapshotID    string  `json:"snapshot_id"`
	Revision      string  `json:"revision"`
	TickMillisNum uint64  `json:"tick_millis_num"`
	TickMillisDen uint64  `json:"tick_millis_den"`
	TimeoutBits   uint8   `json:"timeout_bits"`
	DefaultTicks  *uint64 `json:"default_timeout_ticks"`
}

type v2Workload struct {
	ID        string `json:"id"`
	BudgetKiB int64  `json:"budget_kib"`
	UsedKiB   int64  `json:"used_kib"`
}

type v2Node struct {
	ID                    string       `json:"id"`
	ZoneOffset            int64        `json:"zone_offset"`
	Workloads             []v2Workload `json:"workloads"`
	IsolationTimeoutTicks *uint64      `json:"isolation_timeout_ticks,omitempty"`
}

type v2Rack struct {
	RackID   string   `json:"rack_id"`
	ZoneBase int64    `json:"zone_base"`
	Nodes    []v2Node `json:"nodes"`
}

type v2Topology struct {
	Racks []v2Rack `json:"racks"`
}

type v2Dependency struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type v2Event struct {
	AtTick      *int64  `json:"at_tick"`
	AtMS        *uint64 `json:"at_ms"`
	Order       int     `json:"order"`
	Type        string  `json:"type"`
	Node        string  `json:"node"`
	Domain      *int64  `json:"domain"`
	WorkloadID  string  `json:"workload_id"`
	AmountBytes int64   `json:"amount_bytes"`
}

type v2Project struct {
	Schema             string         `json:"schema"`
	Meta               v2Meta         `json:"meta"`
	Topology           v2Topology     `json:"topology"`
	Dependencies       []v2Dependency `json:"dependencies"`
	Timeline           []v2Event      `json:"timeline"`
	WorkerBudget       int            `json:"worker_budget"`
	PerDomainQueueSize int            `json:"per_domain_pending_limit"`
}

func loadV2(data []byte) (*Project, error) {
	var raw v2Project
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, newError(CodeInvalidJSON, "$", "%v", err)
	}
	if raw.Meta.ProjectID == "" {
		return nil, newError(CodeInvalidSchema, "$.meta.project_id", "project id is required")
	}
	project := &Project{
		SchemaVersion:         "v2",
		ProjectID:             raw.Meta.ProjectID,
		SnapshotID:            firstNonEmpty(raw.Meta.SnapshotID, raw.Meta.Revision, raw.Meta.ProjectID),
		Revision:              firstNonEmpty(raw.Meta.Revision, raw.Meta.SnapshotID, raw.Meta.ProjectID),
		TickMillis:            Ratio{Num: raw.Meta.TickMillisNum, Den: raw.Meta.TickMillisDen},
		TimeoutBits:           raw.Meta.TimeoutBits,
		WorkerBudget:          raw.WorkerBudget,
		PerDomainPendingLimit: raw.PerDomainQueueSize,
	}
	if project.TickMillis.Num == 0 || project.TickMillis.Den == 0 {
		return nil, newError(CodeInvalidSchema, "$.meta", "tick_millis_num and tick_millis_den are required")
	}
	if err := project.TickMillis.normalize(); err != nil {
		return nil, err
	}
	if project.TimeoutBits == 0 {
		project.TimeoutBits = 64
	}
	if raw.Meta.DefaultTicks != nil {
		project.DefaultTimeout = Timeout{Set: true, Wire: *raw.Meta.DefaultTicks, Unit: UnitTick}
	}
	if len(raw.Topology.Racks) == 0 {
		return nil, newError(CodeInvalidSchema, "$.topology.racks", "rack-grouped topology is required for v2")
	}
	for i, rack := range raw.Topology.Racks {
		path := fmt.Sprintf("$.topology.racks[%d]", i)
		if rack.RackID == "" {
			return nil, newError(CodeInvalidSchema, path+".rack_id", "rack id is required")
		}
		for j, node := range rack.Nodes {
			nodePath := fmt.Sprintf("%s.nodes[%d]", path, j)
			if node.ID == "" {
				return nil, newError(CodeInvalidSchema, nodePath+".id", "node id is required")
			}
			zone, err := addZone(rack.ZoneBase, node.ZoneOffset, nodePath+".zone_offset")
			if err != nil {
				return nil, err
			}
			timeout, err := bindTimeout(project, node.IsolationTimeoutTicks, UnitTick, nodePath+".isolation_timeout_ticks")
			if err != nil {
				return nil, err
			}
			canonicalNode := Node{ID: node.ID, Rack: rack.RackID, Zone: zone, Timeout: timeout}
			for k, workload := range node.Workloads {
				if workload.BudgetKiB < 0 || workload.UsedKiB < 0 {
					return nil, newError(CodeInvalidSchema, fmt.Sprintf("%s.workloads[%d]", nodePath, k), "KiB budget and usage must be non-negative")
				}
				budget, err := toBytes(uint64(workload.BudgetKiB), UnitKiB, fmt.Sprintf("%s.workloads[%d].budget_kib", nodePath, k))
				if err != nil {
					return nil, err
				}
				used, err := toBytes(uint64(workload.UsedKiB), UnitKiB, fmt.Sprintf("%s.workloads[%d].used_kib", nodePath, k))
				if err != nil {
					return nil, err
				}
				if workload.ID == "" {
					return nil, newError(CodeInvalidSchema, fmt.Sprintf("%s.workloads[%d].id", nodePath, k), "workload id is required")
				}
				canonicalNode.Workloads = append(canonicalNode.Workloads, Workload{ID: workload.ID, BudgetBytes: budget, UsedBytes: used})
			}
			project.Nodes = append(project.Nodes, canonicalNode)
		}
	}
	for i, dependency := range raw.Dependencies {
		if dependency.Source == "" || dependency.Target == "" {
			return nil, newError(CodeInvalidSchema, fmt.Sprintf("$.dependencies[%d]", i), "source and target are required")
		}
		project.Edges = append(project.Edges, Edge{From: dependency.Source, To: dependency.Target})
	}
	for i, event := range raw.Timeline {
		tick, err := eventTick(event.AtTick, event.AtMS, project.TickMillis, fmt.Sprintf("$.timeline[%d]", i))
		if err != nil {
			return nil, err
		}
		kind, err := normalizeKind(event.Type)
		if err != nil {
			return nil, err
		}
		project.Events = append(project.Events, Event{
			Tick:        tick,
			Seq:         event.Order,
			Kind:        kind,
			NodeID:      event.Node,
			DomainID:    copyInt64(event.Domain),
			WorkloadID:  event.WorkloadID,
			AmountBytes: event.AmountBytes,
		})
	}
	return project, nil
}

type v3Snapshot struct {
	ID         string `json:"id"`
	RowVersion string `json:"row_version"`
}

type v3Clock struct {
	MillisecondsPerTick NumDen `json:"milliseconds_per_tick"`
}

type NumDen struct {
	Num uint64 `json:"num"`
	Den uint64 `json:"den"`
}

type v3TimeoutConfig struct {
	Bits      uint8   `json:"bits"`
	DefaultMS *uint64 `json:"default_ms"`
}

type v3Zone struct {
	Base   int64 `json:"base"`
	Offset int64 `json:"offset"`
}

type v3Workload struct {
	ID     string `json:"id"`
	Budget int64  `json:"budget"`
	Used   int64  `json:"used"`
}

type v3Node struct {
	ID        string       `json:"id"`
	RackID    string       `json:"rack_id"`
	Zone      v3Zone       `json:"zone"`
	TimeoutMS *uint64      `json:"timeout_ms,omitempty"`
	Workloads []v3Workload `json:"workloads"`
}

type v3Link struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type v3Event struct {
	AtMS        *uint64 `json:"at_ms"`
	AtTick      *int64  `json:"at_tick"`
	Sequence    int     `json:"sequence"`
	Type        string  `json:"type"`
	Node        string  `json:"node"`
	Domain      *int64  `json:"domain"`
	WorkloadID  string  `json:"workload_id"`
	AmountBytes int64   `json:"amount_bytes"`
}

type v3Project struct {
	Schema             string          `json:"schema"`
	Snapshot           v3Snapshot      `json:"snapshot"`
	Clock              v3Clock         `json:"clock"`
	Timeout            v3TimeoutConfig `json:"timeout"`
	Nodes              []v3Node        `json:"nodes"`
	Links              []v3Link        `json:"links"`
	Log                []v3Event       `json:"log"`
	WorkerBudget       int             `json:"worker_budget"`
	PerDomainQueueSize int             `json:"per_domain_pending_limit"`
}

func loadV3(data []byte) (*Project, error) {
	var raw v3Project
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, newError(CodeInvalidJSON, "$", "%v", err)
	}
	snapshotID := firstNonEmpty(raw.Snapshot.ID, raw.Snapshot.RowVersion)
	if snapshotID == "" {
		return nil, newError(CodeInvalidSchema, "$.snapshot.id", "snapshot id is required")
	}
	project := &Project{
		SchemaVersion:         "v3",
		ProjectID:             snapshotID,
		SnapshotID:            snapshotID,
		Revision:              firstNonEmpty(raw.Snapshot.RowVersion, snapshotID),
		TickMillis:            Ratio{Num: raw.Clock.MillisecondsPerTick.Num, Den: raw.Clock.MillisecondsPerTick.Den},
		TimeoutBits:           raw.Timeout.Bits,
		WorkerBudget:          raw.WorkerBudget,
		PerDomainPendingLimit: raw.PerDomainQueueSize,
	}
	if project.TickMillis.Num == 0 || project.TickMillis.Den == 0 {
		return nil, newError(CodeInvalidSchema, "$.clock.milliseconds_per_tick", "tick duration must be positive")
	}
	if err := project.TickMillis.normalize(); err != nil {
		return nil, err
	}
	if project.TimeoutBits == 0 {
		project.TimeoutBits = 64
	}
	if raw.Timeout.DefaultMS != nil {
		project.DefaultTimeout = Timeout{Set: true, Wire: *raw.Timeout.DefaultMS, Unit: UnitMS}
	}
	for i, node := range raw.Nodes {
		path := fmt.Sprintf("$.nodes[%d]", i)
		if node.ID == "" {
			return nil, newError(CodeInvalidSchema, path+".id", "node id is required")
		}
		zone, err := addZone(node.Zone.Base, node.Zone.Offset, path+".zone")
		if err != nil {
			return nil, err
		}
		timeout, err := bindTimeout(project, node.TimeoutMS, UnitMS, path+".timeout_ms")
		if err != nil {
			return nil, err
		}
		canonicalNode := Node{ID: node.ID, Rack: node.RackID, Zone: zone, Timeout: timeout}
		for j, workload := range node.Workloads {
			if workload.ID == "" {
				return nil, newError(CodeInvalidSchema, fmt.Sprintf("%s.workloads[%d].id", path, j), "workload id is required")
			}
			if workload.Budget < 0 || workload.Used < 0 {
				return nil, newError(CodeInvalidSchema, fmt.Sprintf("%s.workloads[%d]", path, j), "KiB budget and usage must be non-negative")
			}
			budget, err := toBytes(uint64(workload.Budget), UnitKiB, fmt.Sprintf("%s.workloads[%d].budget", path, j))
			if err != nil {
				return nil, err
			}
			used, err := toBytes(uint64(workload.Used), UnitKiB, fmt.Sprintf("%s.workloads[%d].used", path, j))
			if err != nil {
				return nil, err
			}
			canonicalNode.Workloads = append(canonicalNode.Workloads, Workload{ID: workload.ID, BudgetBytes: budget, UsedBytes: used})
		}
		project.Nodes = append(project.Nodes, canonicalNode)
	}
	for i, link := range raw.Links {
		if link.From == "" || link.To == "" {
			return nil, newError(CodeInvalidSchema, fmt.Sprintf("$.links[%d]", i), "from and to are required")
		}
		project.Edges = append(project.Edges, Edge{From: link.From, To: link.To})
	}
	for i, event := range raw.Log {
		tick, err := eventTick(event.AtTick, event.AtMS, project.TickMillis, fmt.Sprintf("$.log[%d]", i))
		if err != nil {
			return nil, err
		}
		kind, err := normalizeKind(event.Type)
		if err != nil {
			return nil, err
		}
		project.Events = append(project.Events, Event{
			Tick:        tick,
			Seq:         event.Sequence,
			Kind:        kind,
			NodeID:      event.Node,
			DomainID:    copyInt64(event.Domain),
			WorkloadID:  event.WorkloadID,
			AmountBytes: event.AmountBytes,
		})
	}
	return project, nil
}

func eventTick(tick *int64, atMS *uint64, tickMillis Ratio, path string) (int64, error) {
	if tick != nil && atMS != nil {
		return 0, newError(CodeInvalidSchema, path, "specify either tick or millisecond time, not both")
	}
	if tick != nil {
		if *tick < 0 {
			return 0, newError(CodeInvalidSchema, path, "tick must be non-negative")
		}
		return *tick, nil
	}
	if atMS == nil {
		return 0, newError(CodeInvalidSchema, path, "event time is required")
	}
	ticks, err := millisecondsToTicks(*atMS, tickMillis, path)
	if err != nil {
		return 0, err
	}
	return requireWholeTicks(ticks, path)
}

func normalizeKind(raw string) (EventKind, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	value = strings.ReplaceAll(value, "-", "_")
	switch value {
	case "probe", "fault", "fault_detected", "detected":
		return EventProbe, nil
	case "commit", "isolation_commit", "isolation_committed", "committed":
		return EventCommit, nil
	case "recover", "recovered", "recovery", "recovery_accepted":
		return EventRecover, nil
	case "request", "resource_request", "arrival":
		return EventRequest, nil
	default:
		return "", newError(CodeInvalidEvent, "$.event.kind", "unknown event kind %q", raw)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func copyInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
