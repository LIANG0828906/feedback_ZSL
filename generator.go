package layercache

import (
	"encoding/json"
	"fmt"
	"sort"
)

const DefaultSeed int64 = 20261127

type GeneratedFixture struct {
	Project *Project
	V1      []byte
	V2      []byte
	V3      []byte
	Truth   []byte
}

type ExpectedBatch struct {
	TimeMS   int64    `json:"timeMs"`
	BatchID  string   `json:"batchId"`
	Promote  []string `json:"promote"`
	Demote   []string `json:"demote"`
	Deferred []string `json:"deferred,omitempty"`
}

type HeatExpectation struct {
	TimeMS  int64     `json:"timeMs"`
	Object  string    `json:"object"`
	Level   HeatLevel `json:"level"`
	LayerID string    `json:"layerId,omitempty"`
}

type FixtureTruth struct {
	Seed        int64             `json:"seed"`
	Batches     []ExpectedBatch   `json:"batches"`
	CommitTimes []int64           `json:"commitTimes"`
	AbortTimes  []int64           `json:"abortTimes"`
	HeatChecks  []HeatExpectation `json:"heatChecks"`
	FinalLayers map[string]string `json:"finalLayers"`
}

// GenerateFixture deterministically constructs one semantic project and three
// schema renderings of it. Only the local seed and fixed tables are used.
func GenerateFixture(seed int64) (*GeneratedFixture, error) {
	project, err := buildCanonicalProject(seed)
	if err != nil {
		return nil, err
	}
	attachIndependentWatermarks(project)
	if err := project.Validate(); err != nil {
		return nil, err
	}
	v1, err := renderV1(project)
	if err != nil {
		return nil, err
	}
	v2, err := renderV2(project)
	if err != nil {
		return nil, err
	}
	v3, err := renderV3(project)
	if err != nil {
		return nil, err
	}
	truth := buildTruth(project)
	truthJSON, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, err
	}
	return &GeneratedFixture{Project: project, V1: append(v1, '\n'), V2: append(v2, '\n'), V3: append(v3, '\n'), Truth: append(truthJSON, '\n')}, nil
}

func buildCanonicalProject(seed int64) (*Project, error) {
	if seed == 0 {
		seed = DefaultSeed
	}
	cfg := Config{
		TimeUnit: "ms", CapacityUnit: "bytes", HeatScale: 1000, MigrationScale: 1000,
		WatermarkMode: WatermarkProvided, TickMS: 10, FixedBits: 32,
		HeatWindowMS: 200, HeatDecayMS: 500, HeatMaxCount: 40,
		HotThreshold: 7000, ColdThreshold: 2000, MigrateHotLimit: 7000, MigrateColdLimit: 2000,
		BatchMaxObjects: 6, BatchMaxBytes: 6144, Workers: 2, PerLayerBuffer: 16,
	}
	p := &Project{SchemaVersion: "canonical", Seed: seed, Config: cfg}
	p.Layers = []Layer{
		{ID: "hot-a", Kind: LayerHot, CapacityBytes: 5120, AllowedObjects: allObjectIDs(), DeclOrder: 0},
		{ID: "hot-b", Kind: LayerHot, CapacityBytes: 4096, AllowedObjects: allObjectIDs(), DeclOrder: 1},
		{ID: "cold-a", Kind: LayerCold, CapacityBytes: 8192, AllowedObjects: allObjectIDs(), DeclOrder: 2},
		{ID: "cold-b", Kind: LayerCold, CapacityBytes: 6144, AllowedObjects: allObjectIDs(), DeclOrder: 3},
	}
	p.Objects = []Object{
		{ID: "o1", SizeBytes: 1024, InitialLayer: "hot-a", Priority: 3, DeclOrder: 0},
		{ID: "o2", SizeBytes: 1024, InitialLayer: "hot-a", Priority: 2, DeclOrder: 1},
		{ID: "o3", SizeBytes: 2048, InitialLayer: "hot-a", Priority: 5, DeclOrder: 2},
		{ID: "o4", SizeBytes: 1024, InitialLayer: "cold-a", Priority: 3, DeclOrder: 3},
		{ID: "o5", SizeBytes: 1024, InitialLayer: "cold-a", Priority: 2, DeclOrder: 4},
		{ID: "o6", SizeBytes: 1024, InitialLayer: "cold-b", Priority: 4, DeclOrder: 5},
		{ID: "o7", SizeBytes: 1024, InitialLayer: "hot-b", Priority: 1, DeclOrder: 6},
		{ID: "o8", SizeBytes: 1024, InitialLayer: "cold-b", Priority: 3, DeclOrder: 7},
	}
	counts := map[string]func(int64) int64{
		"o1": func(int64) int64 { return 0 },
		"o2": func(t int64) int64 {
			if t < 120 {
				return 1
			}
			return 0
		},
		"o3": func(int64) int64 { return 4 },
		"o4": func(t int64) int64 {
			if t <= 300 {
				return 4
			}
			return 0
		},
		"o5": func(t int64) int64 {
			if t <= 290 {
				return 4
			}
			return 0
		},
		"o6": func(t int64) int64 {
			if t <= 280 {
				return 4
			}
			return 0
		},
		"o7": func(int64) int64 { return 0 },
		"o8": func(t int64) int64 {
			if t >= 450 && t <= 700 {
				return 8
			}
			return 0
		},
	}
	for i := range p.Objects {
		for t := int64(0); t <= 900; t += 10 {
			p.Objects[i].Samples = append(p.Objects[i].Samples, AccessSample{AtMS: t, Count: counts[p.Objects[i].ID](t), SourceTick: t / 10, SourceIndex: int(t / 10)})
		}
	}
	events := []Event{
		{Order: 1, AtMS: 290, Kind: EventAccess, ObjectID: "o4", Count: 1},
		{Order: 2, AtMS: 290, Kind: EventAccess, ObjectID: "o5", Count: 1},
		{Order: 3, AtMS: 290, Kind: EventAccess, ObjectID: "o6", Count: 1},
		{Order: 4, AtMS: 300, Kind: EventRebalance, BatchID: "b1"},
		{Order: 5, AtMS: 310, Kind: EventRead, ObjectID: "o4"},
		{Order: 6, AtMS: 310, Kind: EventWrite, ObjectID: "o4"},
		{Order: 7, AtMS: 320, Kind: EventRead, ObjectID: "o5"},
		{Order: 8, AtMS: 320, Kind: EventWrite, ObjectID: "o5"},
		{Order: 9, AtMS: 400, Kind: EventCommit, BatchID: "b1"},
		{Order: 10, AtMS: 440, Kind: EventAccess, ObjectID: "o8", Count: 2},
		{Order: 11, AtMS: 500, Kind: EventRebalance, BatchID: "b2"},
		{Order: 12, AtMS: 510, Kind: EventRead, ObjectID: "o4"},
		{Order: 13, AtMS: 510, Kind: EventWrite, ObjectID: "o4"},
		{Order: 14, AtMS: 520, Kind: EventWrite, ObjectID: "o8"},
		{Order: 15, AtMS: 600, Kind: EventAbort, BatchID: "b2"},
		{Order: 16, AtMS: 650, Kind: EventRebalance, BatchID: "b3"},
		{Order: 17, AtMS: 660, Kind: EventRead, ObjectID: "o4"},
		{Order: 18, AtMS: 660, Kind: EventWrite, ObjectID: "o4"},
		{Order: 19, AtMS: 750, Kind: EventCommit, BatchID: "b3"},
		{Order: 20, AtMS: 800, Kind: EventRead, ObjectID: "o4"},
	}
	p.Events = events
	return p, nil
}

func allObjectIDs() []string {
	return []string{"o1", "o2", "o3", "o4", "o5", "o6", "o7", "o8"}
}

// attachIndependentWatermarks uses a deliberately separate reference
// calculation, not the production scorer, so fixture truth cannot be
// back-derived from component output.
func attachIndependentWatermarks(p *Project) {
	type rec struct {
		at    int64
		count int64
	}
	records := map[string][]rec{}
	for _, object := range p.Objects {
		for _, sample := range object.Samples {
			if sample.Count > 0 {
				records[object.ID] = append(records[object.ID], rec{sample.AtMS, sample.Count})
			}
		}
	}
	for _, event := range p.Events {
		if event.Kind == EventAccess && event.Count > 0 {
			records[event.ObjectID] = append(records[event.ObjectID], rec{event.AtMS, event.Count})
		}
	}
	for _, object := range p.Objects {
		sort.Slice(records[object.ID], func(i, j int) bool { return records[object.ID][i].at < records[object.ID][j].at })
	}
	for i := range p.Objects {
		object := &p.Objects[i]
		for j := range object.Samples {
			now := object.Samples[j].AtMS
			window := int64(0)
			last := int64(-1)
			for _, r := range records[object.ID] {
				if r.at <= now && r.count > 0 {
					last = r.at
					if r.at > now-p.Config.HeatWindowMS {
						window += r.count
					}
				}
			}
			score := referenceHeat(window, last, now, p.Config)
			wm := score
			object.Samples[j].Watermark = &wm
		}
	}
}

func referenceHeat(window, last, now int64, cfg Config) int64 {
	if window <= 0 || last < 0 {
		return 0
	}
	raw := canonicalHeatScale
	if window < cfg.HeatMaxCount {
		raw = window * canonicalHeatScale / cfg.HeatMaxCount
	}
	elapsed := now - last
	if elapsed >= cfg.HeatDecayMS {
		return 0
	}
	remaining := cfg.HeatDecayMS - elapsed
	factor := remaining * canonicalHeatScale / cfg.HeatDecayMS
	score := raw * factor / canonicalHeatScale
	score = (score / 10) * 10
	if score > 10000 {
		score = 10000
	}
	return score
}

func renderV1(p *Project) ([]byte, error) {
	layers := []any{}
	for _, layer := range p.Layers {
		layers = append(layers, map[string]any{
			"id": layer.ID, "tier": string(layer.Kind), "capacity": layer.CapacityBytes,
			"allowed": layer.AllowedObjects, "order": layer.DeclOrder,
		})
	}
	objects := []any{}
	for _, object := range p.Objects {
		accesses := []any{}
		for _, sample := range object.Samples {
			watermark := int64(0)
			if sample.Watermark != nil {
				watermark = *sample.Watermark / 10
			}
			accesses = append(accesses, map[string]any{"tick": sample.AtMS / p.Config.TickMS, "count": sample.Count, "watermark": watermark})
		}
		objects = append(objects, map[string]any{
			"id": object.ID, "bytes": object.SizeBytes, "initialLayer": object.InitialLayer,
			"priority": object.Priority, "order": object.DeclOrder, "accesses": accesses,
		})
	}
	events := []any{}
	for _, event := range p.Events {
		item := map[string]any{"order": event.Order, "tick": event.AtMS / p.Config.TickMS, "kind": string(event.Kind)}
		if event.ObjectID != "" {
			item["object"] = event.ObjectID
		}
		if event.Count != 0 {
			item["count"] = event.Count
		}
		if event.BatchID != "" {
			item["batch"] = event.BatchID
		}
		if event.SourceLayer != "" {
			item["source"] = event.SourceLayer
		}
		if event.TargetLayer != "" {
			item["target"] = event.TargetLayer
		}
		events = append(events, item)
	}
	doc := map[string]any{
		"schemaVersion": "v1", "seed": p.Seed,
		"config": map[string]any{
			"timeUnit": "tick", "tickMillis": p.Config.TickMS, "capacityUnit": "bytes",
			"heatScale": 1000, "migrationScale": 1000, "fixedBits": p.Config.FixedBits,
			"heatWindowTicks": p.Config.HeatWindowMS / p.Config.TickMS,
			"heatDecayTicks":  p.Config.HeatDecayMS / p.Config.TickMS,
			"heatMaxCount":    p.Config.HeatMaxCount,
			"hotThreshold":    p.Config.HotThreshold / 10, "coldThreshold": p.Config.ColdThreshold / 10,
			"migrationHotThreshold": p.Config.MigrateHotLimit / 10, "migrationColdThreshold": p.Config.MigrateColdLimit / 10,
			"batchMaxObjects": p.Config.BatchMaxObjects, "batchMaxBytes": p.Config.BatchMaxBytes,
			"workers": p.Config.Workers, "perLayerBuffer": p.Config.PerLayerBuffer,
		},
		"layers": layers, "objects": objects, "events": events,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func renderV2(p *Project) ([]byte, error) {
	nested := map[string]any{"hot": []any{}, "cold": []any{}}
	for _, layer := range p.Layers {
		objects := []any{}
		for _, object := range p.Objects {
			if object.InitialLayer != layer.ID {
				continue
			}
			accesses := []any{}
			for _, sample := range object.Samples {
				accesses = append(accesses, map[string]any{"at_ms": sample.AtMS, "count": sample.Count})
			}
			objects = append(objects, map[string]any{
				"id": object.ID, "size_kib": object.SizeBytes / 1024, "priority": object.Priority,
				"decl_order": object.DeclOrder, "accesses": accesses,
			})
		}
		nested[string(layer.Kind)] = append(nested[string(layer.Kind)].([]any), map[string]any{
			"layer_id": layer.ID, "capacity_kib": layer.CapacityBytes / 1024,
			"allows": layer.AllowedObjects, "order": layer.DeclOrder, "objects": objects,
		})
	}
	events := []any{}
	for _, event := range p.Events {
		item := map[string]any{"order": event.Order, "at_ms": event.AtMS, "kind": string(event.Kind)}
		if event.ObjectID != "" {
			item["object_id"] = event.ObjectID
		}
		if event.Count != 0 {
			item["count"] = event.Count
		}
		if event.BatchID != "" {
			item["batch_id"] = event.BatchID
		}
		if event.SourceLayer != "" {
			item["source_layer"] = event.SourceLayer
		}
		if event.TargetLayer != "" {
			item["target_layer"] = event.TargetLayer
		}
		events = append(events, item)
	}
	doc := map[string]any{
		"schema_version": "v2", "seed": p.Seed,
		"settings": map[string]any{
			"fixed_bits": p.Config.FixedBits,
			"time":       map[string]any{"unit": "ms", "tick_ms": p.Config.TickMS},
			"capacity":   map[string]any{"unit": "kib"},
			"heat": map[string]any{
				"scale": 10000, "window_ms": p.Config.HeatWindowMS, "decay_ms": p.Config.HeatDecayMS,
				"max_count": p.Config.HeatMaxCount, "hot_threshold": p.Config.HotThreshold, "cold_threshold": p.Config.ColdThreshold,
			},
			"migration": map[string]any{
				"scale": 10000, "hot_threshold": p.Config.MigrateHotLimit, "cold_threshold": p.Config.MigrateColdLimit,
				"max_objects": p.Config.BatchMaxObjects, "max_bytes": p.Config.BatchMaxBytes,
			},
			"execution": map[string]any{"workers": p.Config.Workers, "per_layer_buffer": p.Config.PerLayerBuffer},
			"watermark": map[string]any{"mode": "derive"},
		},
		"layers": nested, "events": events,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func renderV3(p *Project) ([]byte, error) {
	layers := []any{}
	for _, layer := range p.Layers {
		layers = append(layers, map[string]any{
			"id": layer.ID, "kind": string(layer.Kind), "capacity_bytes": layer.CapacityBytes,
			"allows": layer.AllowedObjects, "order": layer.DeclOrder,
		})
	}
	objects := []any{}
	for _, object := range p.Objects {
		samples := []any{}
		for _, sample := range object.Samples {
			watermark := int64(0)
			if sample.Watermark != nil {
				watermark = *sample.Watermark / 10
			}
			samples = append(samples, map[string]any{"at_ms": sample.AtMS, "count": sample.Count, "heat_watermark": watermark})
		}
		objects = append(objects, map[string]any{
			"id": object.ID, "size_bytes": object.SizeBytes, "initial_layer": object.InitialLayer,
			"priority": object.Priority, "decl_order": object.DeclOrder, "samples": samples,
		})
	}
	events := []any{}
	for _, event := range p.Events {
		item := map[string]any{"order": event.Order, "at_ms": event.AtMS, "kind": string(event.Kind)}
		if event.ObjectID != "" {
			item["object_id"] = event.ObjectID
		}
		if event.Count != 0 {
			item["count"] = event.Count
		}
		if event.BatchID != "" {
			item["batch_id"] = event.BatchID
		}
		if event.SourceLayer != "" {
			item["source_layer"] = event.SourceLayer
		}
		if event.TargetLayer != "" {
			item["target_layer"] = event.TargetLayer
		}
		events = append(events, item)
	}
	doc := map[string]any{
		"schema": "v3", "seed": p.Seed,
		"policy": map[string]any{
			"capacity_unit": "bytes", "time_unit": "ms", "heat_scale": 1000, "migration_scale": 10000,
			"tick_ms": p.Config.TickMS, "fixed_bits": p.Config.FixedBits,
			"heat_window_ms": p.Config.HeatWindowMS, "heat_decay_ms": p.Config.HeatDecayMS,
			"heat_max_count": p.Config.HeatMaxCount,
			"hot_threshold":  p.Config.HotThreshold / 10, "cold_threshold": p.Config.ColdThreshold / 10,
			"migration_hot_threshold": p.Config.MigrateHotLimit, "migration_cold_threshold": p.Config.MigrateColdLimit,
			"batch_max_objects": p.Config.BatchMaxObjects, "batch_max_bytes": p.Config.BatchMaxBytes,
			"workers": p.Config.Workers, "per_layer_buffer": p.Config.PerLayerBuffer,
			"watermark_policy": "provided",
		},
		"layers": layers, "objects": objects, "events": events,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func buildTruth(p *Project) FixtureTruth {
	return FixtureTruth{
		Seed: p.Seed,
		Batches: []ExpectedBatch{
			{TimeMS: 300, BatchID: "b1", Promote: []string{"o6", "o4", "o5"}, Demote: []string{"o1", "o2", "o7"}},
			{TimeMS: 500, BatchID: "b2", Promote: []string{"o8"}, Demote: []string{"o6", "o4", "o5"}},
			{TimeMS: 650, BatchID: "b3", Promote: []string{"o8"}, Demote: []string{"o6", "o4", "o5"}},
		},
		CommitTimes: []int64{400, 750},
		AbortTimes:  []int64{600},
		HeatChecks: []HeatExpectation{
			{TimeMS: 300, Object: "o4", Level: HeatHot},
			{TimeMS: 300, Object: "o1", Level: HeatCold},
			{TimeMS: 500, Object: "o8", Level: HeatHot},
			{TimeMS: 500, Object: "o4", Level: HeatCold},
		},
		FinalLayers: map[string]string{
			"o1": "cold-a", "o2": "cold-a", "o3": "hot-a", "o4": "cold-a",
			"o5": "cold-a", "o6": "cold-a", "o7": "cold-a", "o8": "hot-a",
		},
	}
}

func (f *GeneratedFixture) WriteSamples(dir string) error {
	files := []struct {
		name string
		data []byte
	}{
		{"v1.json", f.V1},
		{"v2.json", f.V2},
		{"v3.json", f.V3},
		{"truth.json", f.Truth},
	}
	for _, file := range files {
		if err := writeFileAtomic(dir+"/"+file.name, file.data); err != nil {
			return err
		}
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	return writeFile(path, data)
}

func fixtureSummary(f *GeneratedFixture) string {
	return fmt.Sprintf("seed=%d objects=%d layers=%d events=%d", f.Project.Seed, len(f.Project.Objects), len(f.Project.Layers), len(f.Project.Events))
}
