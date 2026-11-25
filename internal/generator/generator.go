package generator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"graphlab/internal/model"
)

const DefaultSeed int64 = 42

type Sample struct {
	Seed      int64
	Core      *model.Project
	Documents map[string][]byte
	Truth     *model.Truth
}

type rng struct {
	state uint64
}

func newRNG(seed int64) *rng {
	return &rng{state: uint64(seed)*6364136223846793005 + 1442695040888963407}
}

func (r *rng) next() uint64 {
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return r.state
}

func (r *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func Generate(seed int64) (*Sample, error) {
	p, err := buildCoreProject(seed)
	if err != nil {
		return nil, err
	}
	truth, err := BuildTruth(p)
	if err != nil {
		return nil, err
	}
	v1, err := renderV1(p)
	if err != nil {
		return nil, err
	}
	v2, err := renderV2(p)
	if err != nil {
		return nil, err
	}
	v3, err := renderV3(p)
	if err != nil {
		return nil, err
	}
	docs := map[string][]byte{
		model.SchemaV1: v1,
		model.SchemaV2: v2,
		model.SchemaV3: v3,
	}
	return &Sample{Seed: seed, Core: p, Documents: docs, Truth: truth}, nil
}

func buildCoreProject(seed int64) (*model.Project, error) {
	r := newRNG(seed)
	shards := []model.Shard{
		{ID: "s0", Low: "v0000", High: "v0006"},
		{ID: "s1", Low: "v0006", High: "v0012"},
		{ID: "s2", Low: "v0012", High: "v0020"},
	}
	labelPatterns := [][]string{
		{"blue", "core"},
		{"green", "core"},
		{"blue"},
		{"amber", "core"},
		{"green"},
		{"blue", "core"},
		{"amber"},
		{"green", "core"},
		{"blue"},
		{"amber", "core"},
		{"green"},
		{"blue", "core"},
		{"amber"},
		{"green", "core"},
		{"blue"},
		{"amber", "core"},
		{"green"},
		{"blue", "core"},
	}
	names := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	vertices := make([]model.Vertex, 0, len(labelPatterns))
	for i, labels := range labelPatterns {
		id := fmt.Sprintf("v%04d", i)
		rank := int64(5 + ((i*7 + r.intn(5)) % 90))
		score := int64(125000 + i*25000 + r.intn(4)*5000)
		vertices = append(vertices, model.Vertex{
			ID: id, DeclOrder: i,
			Labels: append([]string(nil), labels...),
			Attrs: map[string]model.AttrValue{
				"rank":  {Kind: model.AttrInt, Int: rank},
				"score": {Kind: model.AttrFixed, FixedMicros: score},
				"name":  {Kind: model.AttrString, String: names[i%len(names)]},
			},
		})
	}
	levels := []string{"tier-a", "tier-b", "tier-c"}
	var edges []model.Edge
	addEdge := func(from, to string) {
		idx := len(edges)
		edges = append(edges, model.Edge{
			ID: fmt.Sprintf("e%04d", idx), From: from, To: to,
			Directed: true, Weight: int64(1 + (idx+r.intn(3))%5),
			Levels: []string{levels[idx%len(levels)]}, DeclOrder: idx,
		})
	}
	for i := 0; i < len(vertices); i++ {
		addEdge(vertices[i].ID, vertices[(i+1)%len(vertices)].ID)
		if i%3 == 0 {
			addEdge(vertices[i].ID, vertices[(i+5)%len(vertices)].ID)
		}
	}
	p := &model.Project{
		ID: "demo-graph-" + fmt.Sprintf("%d", seed), Version: 1, Seed: seed,
		HierarchyEnabled:  true,
		ClockUnit:         "tick",
		CapacityUnit:      "byte",
		ObservedAtMillis:  180,
		CacheCapacityByte: 4096,
		IndexRules: []model.IndexRule{
			{Attribute: "rank", Kind: model.AttrInt, Bucket: 10},
			{Attribute: "score", Kind: model.AttrFixed, Bucket: 100000},
			{Attribute: "name", Kind: model.AttrString, PrefixLen: 3},
		},
		Shards: shards, Vertices: vertices, Edges: edges,
		Queries: []model.Query{
			{ID: "q01-one-hop-core", Kind: model.QueryOneHop, Start: "v0002", Labels: []string{"core"}, MaxDepth: 1},
			{ID: "q02-two-hop-green", Kind: model.QueryTwoHop, Start: "v0002", Labels: []string{"green"}, MaxDepth: 2},
			{ID: "q03-shortest-long", Kind: model.QueryShortestPath, Start: "v0000", Target: "v0014", MaxDepth: 20},
			{ID: "q04-shortest-unreachable", Kind: model.QueryShortestPath, Start: "v0016", Target: "v0000", MaxDepth: 1},
			{ID: "q05-depth-zero", Kind: model.QueryOneHop, Start: "v0001", MaxDepth: 0},
			{ID: "q06-missing-start", Kind: model.QueryOneHop, Start: "v9999", MaxDepth: 1},
			{ID: "q07-two-hop-amber", Kind: model.QueryTwoHop, Start: "v0007", Labels: []string{"amber"}, MaxDepth: 2},
			{ID: "q08-label-miss", Kind: model.QueryOneHop, Start: "v0005", Labels: []string{"missing"}, MaxDepth: 1},
		},
	}
	if err := model.AssignShards(p); err != nil {
		return nil, err
	}
	model.SortProject(p)
	ops, err := buildOperations(p, r)
	if err != nil {
		return nil, err
	}
	p.Operations = ops
	return p, nil
}

func buildOperations(p *model.Project, r *rng) ([]model.Operation, error) {
	getVertex := func(id string) (model.Vertex, bool) {
		for _, v := range p.Vertices {
			if v.ID == id {
				return model.CloneVertex(v), true
			}
		}
		return model.Vertex{}, false
	}
	findEdge := func(predicate func(model.Edge) bool) (model.Edge, bool) {
		for _, e := range p.Edges {
			if predicate(e) {
				return model.CloneEdge(e), true
			}
		}
		return model.Edge{}, false
	}
	v3, ok := getVertex("v0003")
	if !ok {
		return nil, fmt.Errorf("generator baseline missing v0003")
	}
	v3.ShardID = ""
	v3.Attrs["rank"] = model.AttrValue{Kind: model.AttrInt, Int: 92}
	v3.Attrs["score"] = model.AttrValue{Kind: model.AttrFixed, FixedMicros: 875000}
	v3.Attrs["name"] = model.AttrValue{Kind: model.AttrString, String: "omega"}
	op1 := model.Operation{
		Seq: 1, Kind: model.OpVertexUpdate, VertexID: v3.ID, Vertex: &v3,
		Note: "cross two attribute buckets and change string prefix",
	}
	newVertex := model.Vertex{
		ID: "v0019", DeclOrder: 19, Labels: []string{"amber", "new"},
		Attrs: map[string]model.AttrValue{
			"rank":  {Kind: model.AttrInt, Int: 77},
			"score": {Kind: model.AttrFixed, FixedMicros: 625000},
			"name":  {Kind: model.AttrString, String: "omega"},
		},
	}
	op2 := model.Operation{Seq: 2, Kind: model.OpVertexAdd, VertexID: newVertex.ID, Vertex: &newVertex,
		Note: "append vertex at end of declaration order"}
	maxDecl := -1
	for _, e := range p.Edges {
		if e.DeclOrder > maxDecl {
			maxDecl = e.DeclOrder
		}
	}
	newEdge := model.Edge{
		ID: "e0100", From: "v0019", To: "v0004", Directed: true, Weight: 9,
		Levels: []string{"tier-b"}, DeclOrder: maxDecl + 1,
	}
	op3 := model.Operation{Seq: 3, Kind: model.OpEdgeAdd, EdgeID: newEdge.ID, Edge: &newEdge,
		Note: "attach newly declared vertex to an existing target"}
	updateEdge, ok := findEdge(func(e model.Edge) bool { return e.From == "v0006" })
	if !ok {
		return nil, fmt.Errorf("generator baseline missing edge from v0006")
	}
	updateEdge.ShardID = ""
	updateEdge.Weight = 17
	updateEdge.Levels = []string{"tier-c"}
	op4 := model.Operation{Seq: 4, Kind: model.OpEdgeUpdate, EdgeID: updateEdge.ID, Edge: &updateEdge,
		Note: "weight and hierarchy label change on an outgoing edge"}
	deleteEdge, ok := findEdge(func(e model.Edge) bool {
		return e.From == "v0014" && e.From != "v0011" && e.To != "v0011"
	})
	if !ok {
		return nil, fmt.Errorf("generator baseline missing deletable edge from v0014")
	}
	op5 := model.Operation{Seq: 5, Kind: model.OpEdgeDelete, EdgeID: deleteEdge.ID,
		Note: "delete one edge without touching its vertices"}
	op6 := model.Operation{Seq: 6, Kind: model.OpVertexDelete, VertexID: "v0011",
		Note: "vertex delete cascades incident edge removal"}
	op7 := model.Operation{
		Seq: 7, Kind: model.OpReshard,
		Shards: []model.Shard{
			{ID: "s0", Low: "v0000", High: "v0006"},
			{ID: "s1a", Low: "v0006", High: "v0009"},
			{ID: "s1b", Low: "v0009", High: "v0012"},
			{ID: "s2", Low: "v0012", High: "v0020"},
		},
		Note: "split s1 into s1a/s1b and replace its shard-local index layer",
	}
	_ = r
	return []model.Operation{op1, op2, op3, op4, op5, op6, op7}, nil
}

func (s *Sample) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for version, doc := range s.Documents {
		path := filepath.Join(dir, "project_"+version+".json")
		if err := os.WriteFile(path, doc, 0o644); err != nil {
			return err
		}
	}
	core, err := json.MarshalIndent(s.Core, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "project_core.json"), append(core, '\n'), 0o644); err != nil {
		return err
	}
	truth, err := json.MarshalIndent(s.Truth, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), append(truth, '\n'), 0o644); err != nil {
		return err
	}
	manifest := map[string]any{
		"seed":         s.Seed,
		"project_id":   s.Core.ID,
		"core_digest":  s.Truth.CoreDigest,
		"schemas":      []string{model.SchemaV1, model.SchemaV2, model.SchemaV3},
		"schema_files": []string{"project_v1.json", "project_v2.json", "project_v3.json"},
		"truth_file":   "truth.json",
		"note":         "all files are deterministic and generated locally",
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(b, '\n'), 0o644)
}

func LoadSample(dir string) (map[string][]byte, *model.Truth, error) {
	docs := make(map[string][]byte, 3)
	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		b, err := os.ReadFile(filepath.Join(dir, "project_"+version+".json"))
		if err != nil {
			return nil, nil, err
		}
		docs[version] = b
	}
	b, err := os.ReadFile(filepath.Join(dir, "truth.json"))
	if err != nil {
		return nil, nil, err
	}
	var truth model.Truth
	if err := json.Unmarshal(b, &truth); err != nil {
		return nil, nil, err
	}
	return docs, &truth, nil
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func trimSpace(s string) string { return strings.TrimSpace(s) }
