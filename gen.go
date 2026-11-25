package orchestrator

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
)

const (
	DefaultSeed      int64 = 424242
	DefaultWorkers         = 4
	DefaultDomainCap       = 3
)

type TickTruth struct {
	Faulty        []string            `json:"faulty"`
	Frozen        []string            `json:"frozen"`
	Domains       map[int64][]string  `json:"domains"`
	Waiting       []string            `json:"waiting"`
	Unfrozen      []string            `json:"unfrozen"`
	ForcedDomains []int64             `json:"forced_domains"`
	Recovered     []string            `json:"recovered"`
	Blocked       map[string][]string `json:"blocked_chains"`
}

type Truth struct {
	Ticks map[int]TickTruth `json:"ticks"`
}

type Scenario struct {
	Seed      int64    `json:"seed"`
	Canonical *Project `json:"canonical"`
	V1        []byte   `json:"-"`
	V2        []byte   `json:"-"`
	V3        []byte   `json:"-"`
	Truth     Truth    `json:"truth"`
}

type genNode struct {
	ID           string
	Rack         string
	Zone         int64
	TimeoutTicks *uint64
	BudgetBytes  int64
	UsedBytes    int64
}

type genEdge struct {
	From string
	To   string
}

type genEvent struct {
	Tick        int64
	Seq         int
	Kind        EventKind
	NodeID      string
	DomainID    *int64
	WorkloadID  string
	AmountBytes int64
}

// GenerateScenario builds one deterministic cluster content and serializes it
// into all three wire schemas. The seed permutes wire order only; the logical
// content and truth remain stable, proving canonical traversal is independent
// of source ordering.
func GenerateScenario(seed int64) (*Scenario, error) {
	nodes := []genNode{
		{ID: "a", Rack: "rack-a", Zone: 100, TimeoutTicks: uint64Ptr(12), BudgetBytes: 4096, UsedBytes: 1024},
		{ID: "b", Rack: "rack-a", Zone: 100, TimeoutTicks: uint64Ptr(12), BudgetBytes: 4096, UsedBytes: 1024},
		{ID: "c", Rack: "rack-a", Zone: 100, TimeoutTicks: uint64Ptr(12), BudgetBytes: 4096, UsedBytes: 2048},
		{ID: "d", Rack: "rack-b", Zone: 200, TimeoutTicks: uint64Ptr(8), BudgetBytes: 4096, UsedBytes: 0},
		{ID: "e", Rack: "rack-b", Zone: 200, TimeoutTicks: uint64Ptr(8), BudgetBytes: 4096, UsedBytes: 1024},
		{ID: "f", Rack: "rack-c", Zone: 300, BudgetBytes: 4096, UsedBytes: 0},
		{ID: "g", Rack: "rack-c", Zone: 300, BudgetBytes: 4096, UsedBytes: 0},
		{ID: "h", Rack: "rack-d", Zone: 400, BudgetBytes: 4096, UsedBytes: 0},
		{ID: "i", Rack: "rack-d", Zone: 400, BudgetBytes: 4096, UsedBytes: 0},
	}
	edges := []genEdge{
		{From: "b", To: "a"},
		{From: "c", To: "b"},
		{From: "d", To: "c"},
		{From: "e", To: "d"},
		{From: "g", To: "f"},
		{From: "i", To: "g"},
	}
	events := buildTimeline()
	snapshotID := fmt.Sprintf("snapshot-%d", seed)
	projectTemplate := &Project{
		ProjectID:             snapshotID,
		SnapshotID:            snapshotID,
		Revision:              "rev-1",
		TickMillis:            Ratio{Num: 5, Den: 2},
		TimeoutBits:           32,
		DefaultTimeout:        Timeout{Set: true, Wire: 50, Unit: UnitMS},
		WorkerBudget:          DefaultWorkers,
		PerDomainPendingLimit: DefaultDomainCap,
	}

	v1Doc, err := marshalV1(projectTemplate, nodes, edges, events, seed)
	if err != nil {
		return nil, err
	}
	v2Doc, err := marshalV2(projectTemplate, nodes, edges, events, seed)
	if err != nil {
		return nil, err
	}
	v3Doc, err := marshalV3(projectTemplate, nodes, edges, events, seed)
	if err != nil {
		return nil, err
	}
	v1Project, err := LoadProject(v1Doc)
	if err != nil {
		return nil, fmt.Errorf("load generated v1: %w", err)
	}
	v2Project, err := LoadProject(v2Doc)
	if err != nil {
		return nil, fmt.Errorf("load generated v2: %w", err)
	}
	v3Project, err := LoadProject(v3Doc)
	if err != nil {
		return nil, fmt.Errorf("load generated v3: %w", err)
	}
	if !EquivalentProjects(v1Project, v2Project) || !EquivalentProjects(v1Project, v3Project) {
		return nil, fmt.Errorf("generated schemas do not normalize to equivalent projects")
	}
	return &Scenario{
		Seed:      seed,
		Canonical: v1Project,
		V1:        v1Doc,
		V2:        v2Doc,
		V3:        v3Doc,
		Truth:     sampleTruth(),
	}, nil
}

func buildTimeline() []genEvent {
	seq := make(map[int64]int)
	events := make([]genEvent, 0, 28)
	add := func(tick int64, kind EventKind, node string, domain *int64, workload string, amount int64) {
		seq[tick]++
		events = append(events, genEvent{
			Tick:        tick,
			Seq:         seq[tick],
			Kind:        kind,
			NodeID:      node,
			DomainID:    domain,
			WorkloadID:  workload,
			AmountBytes: amount,
		})
	}
	add(0, EventRequest, "", nil, "wa", 1024)
	add(0, EventRequest, "", nil, "wb", 1024)

	add(2, EventProbe, "a", nil, "", 0)
	add(2, EventProbe, "b", nil, "", 0)
	add(2, EventProbe, "c", nil, "", 0)
	add(2, EventRequest, "", nil, "wb", 2048)

	add(4, EventCommit, "", int64Ptr(100), "", 0)
	add(4, EventRecover, "c", nil, "", 0)

	add(6, EventRecover, "a", nil, "", 0)

	add(8, EventRecover, "b", nil, "", 0)
	add(8, EventRequest, "", nil, "wb", 4096)

	add(12, EventProbe, "d", nil, "", 0)
	add(12, EventRequest, "", nil, "we", 2048)

	add(14, EventRequest, "", nil, "wd", 2048)

	add(22, EventCommit, "", int64Ptr(200), "", 0)
	add(22, EventRecover, "d", nil, "", 0)

	add(26, EventRequest, "", nil, "wd", 3072)

	add(32, EventProbe, "f", nil, "", 0)
	add(34, EventRequest, "", nil, "wg", 1024)

	add(36, EventCommit, "", int64Ptr(300), "", 0)
	add(36, EventRecover, "f", nil, "", 0)

	add(50, EventProbe, "h", nil, "", 0)

	add(52, EventCommit, "", int64Ptr(400), "", 0)

	add(54, EventRecover, "h", nil, "", 0)
	return events
}

func marshalV1(template *Project, nodes []genNode, edges []genEdge, events []genEvent, seed int64) ([]byte, error) {
	raw := v1Project{
		SchemaVersion:      "cluster.v1",
		ProjectID:          template.ProjectID,
		SnapshotID:         template.SnapshotID,
		Revision:           template.Revision,
		Clock:              v1Clock{TickMillisNum: template.TickMillis.Num, TickMillisDen: template.TickMillis.Den},
		TimeoutBits:        template.TimeoutBits,
		DefaultTimeoutMS:   uint64Ptr(50),
		WorkerBudget:       template.WorkerBudget,
		PerDomainQueueSize: template.PerDomainPendingLimit,
	}
	for _, node := range nodes {
		v1Node := v1Node{
			ID:        node.ID,
			Rack:      node.Rack,
			ZoneIDAbs: int64Ptr(node.Zone),
			Workloads: []v1Workload{{
				ID:          workloadID(node.ID),
				BudgetBytes: node.BudgetBytes,
				UsedBytes:   node.UsedBytes,
			}},
		}
		if node.TimeoutTicks != nil {
			v1Node.IsolationTimeout = uint64Ptr(ticksToMilliseconds(*node.TimeoutTicks, template.TickMillis))
		}
		raw.Nodes = append(raw.Nodes, v1Node)
	}
	for _, edge := range edges {
		raw.Edges = append(raw.Edges, v1Edge{FromNode: edge.From, ToNode: edge.To})
	}
	for _, event := range events {
		v1Event := v1Event{
			AtMS:        uint64Ptr(ticksToMilliseconds(uint64(event.Tick), template.TickMillis)),
			Seq:         event.Seq,
			Kind:        string(event.Kind),
			NodeID:      event.NodeID,
			DomainID:    copyInt64(event.DomainID),
			WorkloadID:  event.WorkloadID,
			AmountBytes: event.AmountBytes,
		}
		raw.Events = append(raw.Events, v1Event)
	}
	rng := rand.New(rand.NewSource(seed ^ 0x11))
	rng.Shuffle(len(raw.Nodes), func(i, j int) { raw.Nodes[i], raw.Nodes[j] = raw.Nodes[j], raw.Nodes[i] })
	rng.Shuffle(len(raw.Edges), func(i, j int) { raw.Edges[i], raw.Edges[j] = raw.Edges[j], raw.Edges[i] })
	rng.Shuffle(len(raw.Events), func(i, j int) { raw.Events[i], raw.Events[j] = raw.Events[j], raw.Events[i] })
	return json.MarshalIndent(raw, "", "  ")
}

func marshalV2(template *Project, nodes []genNode, edges []genEdge, events []genEvent, seed int64) ([]byte, error) {
	raw := v2Project{
		Schema: "cluster/v2",
		Meta: v2Meta{
			ProjectID:     template.ProjectID,
			SnapshotID:    template.SnapshotID,
			Revision:      template.Revision,
			TickMillisNum: template.TickMillis.Num,
			TickMillisDen: template.TickMillis.Den,
			TimeoutBits:   template.TimeoutBits,
			DefaultTicks:  uint64Ptr(20),
		},
		WorkerBudget:       template.WorkerBudget,
		PerDomainQueueSize: template.PerDomainPendingLimit,
	}
	rackIndex := make(map[string]int)
	for _, node := range nodes {
		index, ok := rackIndex[node.Rack]
		if !ok {
			index = len(raw.Topology.Racks)
			rackIndex[node.Rack] = index
			raw.Topology.Racks = append(raw.Topology.Racks, v2Rack{RackID: node.Rack, ZoneBase: relativeZoneBase(node.Zone)})
		}
		v2Node := v2Node{
			ID:         node.ID,
			ZoneOffset: node.Zone - raw.Topology.Racks[index].ZoneBase,
			Workloads: []v2Workload{{
				ID:        workloadID(node.ID),
				BudgetKiB: node.BudgetBytes / 1024,
				UsedKiB:   node.UsedBytes / 1024,
			}},
		}
		if node.TimeoutTicks != nil {
			v2Node.IsolationTimeoutTicks = uint64Ptr(*node.TimeoutTicks)
		}
		raw.Topology.Racks[index].Nodes = append(raw.Topology.Racks[index].Nodes, v2Node)
	}
	for _, edge := range edges {
		raw.Dependencies = append(raw.Dependencies, v2Dependency{Source: edge.From, Target: edge.To})
	}
	for _, event := range events {
		raw.Timeline = append(raw.Timeline, v2Event{
			AtTick:      int64Ptr(event.Tick),
			Order:       event.Seq,
			Type:        wireKind(event.Kind, true),
			Node:        event.NodeID,
			Domain:      copyInt64(event.DomainID),
			WorkloadID:  event.WorkloadID,
			AmountBytes: event.AmountBytes,
		})
	}
	rng := rand.New(rand.NewSource(seed ^ 0x22))
	rng.Shuffle(len(raw.Topology.Racks), func(i, j int) {
		raw.Topology.Racks[i], raw.Topology.Racks[j] = raw.Topology.Racks[j], raw.Topology.Racks[i]
	})
	for index := range raw.Topology.Racks {
		rack := &raw.Topology.Racks[index]
		rng.Shuffle(len(rack.Nodes), func(i, j int) { rack.Nodes[i], rack.Nodes[j] = rack.Nodes[j], rack.Nodes[i] })
	}
	rng.Shuffle(len(raw.Dependencies), func(i, j int) {
		raw.Dependencies[i], raw.Dependencies[j] = raw.Dependencies[j], raw.Dependencies[i]
	})
	rng.Shuffle(len(raw.Timeline), func(i, j int) { raw.Timeline[i], raw.Timeline[j] = raw.Timeline[j], raw.Timeline[i] })
	return json.MarshalIndent(raw, "", "  ")
}

func marshalV3(template *Project, nodes []genNode, edges []genEdge, events []genEvent, seed int64) ([]byte, error) {
	raw := v3Project{
		Schema:   "cluster.v3",
		Snapshot: v3Snapshot{ID: template.SnapshotID, RowVersion: template.Revision},
		Clock: v3Clock{MillisecondsPerTick: NumDen{
			Num: template.TickMillis.Num,
			Den: template.TickMillis.Den,
		}},
		Timeout:            v3TimeoutConfig{Bits: template.TimeoutBits, DefaultMS: uint64Ptr(50)},
		WorkerBudget:       template.WorkerBudget,
		PerDomainQueueSize: template.PerDomainPendingLimit,
	}
	for _, node := range nodes {
		v3Node := v3Node{
			ID:     node.ID,
			RackID: node.Rack,
			Zone:   v3Zone{Base: relativeZoneBase(node.Zone), Offset: node.Zone - relativeZoneBase(node.Zone)},
			Workloads: []v3Workload{{
				ID:     workloadID(node.ID),
				Budget: node.BudgetBytes / 1024,
				Used:   node.UsedBytes / 1024,
			}},
		}
		if node.TimeoutTicks != nil {
			v3Node.TimeoutMS = uint64Ptr(ticksToMilliseconds(*node.TimeoutTicks, template.TickMillis))
		}
		raw.Nodes = append(raw.Nodes, v3Node)
	}
	for _, edge := range edges {
		raw.Links = append(raw.Links, v3Link{From: edge.From, To: edge.To})
	}
	for _, event := range events {
		raw.Log = append(raw.Log, v3Event{
			AtMS:        uint64Ptr(ticksToMilliseconds(uint64(event.Tick), template.TickMillis)),
			Sequence:    event.Seq,
			Type:        wireKind(event.Kind, false),
			Node:        event.NodeID,
			Domain:      copyInt64(event.DomainID),
			WorkloadID:  event.WorkloadID,
			AmountBytes: event.AmountBytes,
		})
	}
	rng := rand.New(rand.NewSource(seed ^ 0x33))
	rng.Shuffle(len(raw.Nodes), func(i, j int) { raw.Nodes[i], raw.Nodes[j] = raw.Nodes[j], raw.Nodes[i] })
	rng.Shuffle(len(raw.Links), func(i, j int) { raw.Links[i], raw.Links[j] = raw.Links[j], raw.Links[i] })
	rng.Shuffle(len(raw.Log), func(i, j int) { raw.Log[i], raw.Log[j] = raw.Log[j], raw.Log[i] })
	return json.MarshalIndent(raw, "", "  ")
}

func sampleTruth() Truth {
	return Truth{Ticks: map[int]TickTruth{
		2: {
			Faulty:  []string{"a", "b", "c"},
			Frozen:  []string{"wa", "wb", "wc", "wd", "we"},
			Domains: map[int64][]string{100: {"a", "b", "c"}},
			Waiting: []string{"wb"},
		},
		4: {
			Faulty:   []string{"a", "b", "c"},
			Domains:  map[int64][]string{100: {"a", "b", "c"}},
			Unfrozen: []string{"wa", "wb", "wc", "wd", "we"},
			Blocked:  map[string][]string{"c": {"c", "b"}},
		},
		6: {
			Faulty:    []string{"b", "c"},
			Recovered: []string{"a"},
			Blocked:   map[string][]string{"c": {"c", "b"}},
		},
		8: {
			Recovered: []string{"b", "c"},
		},
		12: {
			Faulty:  []string{"d"},
			Frozen:  []string{"wd", "we"},
			Domains: map[int64][]string{200: {"d", "e"}},
			Waiting: []string{"we"},
		},
		14: {
			Faulty:  []string{"d"},
			Frozen:  []string{"wd", "we"},
			Domains: map[int64][]string{200: {"d", "e"}},
			Waiting: []string{"we", "wd"},
		},
		20: {
			Faulty:        []string{"d"},
			Frozen:        []string{"wd", "we"},
			Domains:       map[int64][]string{200: {"d", "e"}},
			ForcedDomains: []int64{200},
			Waiting:       []string{"we", "wd"},
		},
		22: {
			Domains:   map[int64][]string{200: {"d", "e"}},
			Unfrozen:  []string{"wd", "we"},
			Recovered: []string{"d"},
		},
		32: {
			Faulty:  []string{"f"},
			Frozen:  []string{"wf", "wg", "wi"},
			Domains: map[int64][]string{300: {"f", "g"}},
		},
		34: {
			Faulty:  []string{"f"},
			Frozen:  []string{"wf", "wg", "wi"},
			Domains: map[int64][]string{300: {"f", "g"}},
			Waiting: []string{"wg"},
		},
		36: {
			Domains:   map[int64][]string{300: {"f", "g"}},
			Unfrozen:  []string{"wf", "wg", "wi"},
			Recovered: []string{"f"},
		},
		50: {
			Faulty:  []string{"h"},
			Frozen:  []string{"wh", "wi"},
			Domains: map[int64][]string{400: {"h", "i"}},
		},
		52: {
			Domains:  map[int64][]string{400: {"h", "i"}},
			Unfrozen: []string{"wh", "wi"},
			Faulty:   []string{"h"},
		},
		54: {
			Recovered: []string{"h"},
		},
	}}
}

func EquivalentProjects(a, b *Project) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ProjectID == b.ProjectID &&
		a.SnapshotID == b.SnapshotID &&
		a.Revision == b.Revision &&
		a.TickMillis == b.TickMillis &&
		a.TimeoutBits == b.TimeoutBits &&
		a.WorkerBudget == b.WorkerBudget &&
		a.PerDomainPendingLimit == b.PerDomainPendingLimit &&
		reflect.DeepEqual(a.Nodes, b.Nodes) &&
		reflect.DeepEqual(a.Edges, b.Edges) &&
		reflect.DeepEqual(a.Events, b.Events)
}

func workloadID(nodeID string) string { return "w" + nodeID }

func relativeZoneBase(zone int64) int64 {
	if zone >= 200 {
		return zone - 10
	}
	return zone
}

func ticksToMilliseconds(ticks uint64, tickMillis Ratio) uint64 {
	// Generated ticks are even, so the exact 5/2 conversion is integral.
	return ticks * tickMillis.Num / tickMillis.Den
}

func wireKind(kind EventKind, v2 bool) string {
	switch kind {
	case EventProbe:
		return "fault_detected"
	case EventCommit:
		return "isolation_committed"
	case EventRecover:
		if v2 {
			return "recovery"
		}
		return "recovered"
	case EventRequest:
		return "resource_request"
	default:
		return string(kind)
	}
}

func uint64Ptr(value uint64) *uint64 { return &value }
func int64Ptr(value int64) *int64    { return &value }
