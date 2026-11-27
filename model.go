package layercache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type LayerKind string

const (
	LayerHot  LayerKind = "hot"
	LayerCold LayerKind = "cold"
)

type HeatLevel string

const (
	HeatCold HeatLevel = "COLD"
	HeatWarm HeatLevel = "WARM"
	HeatHot  HeatLevel = "HOT"
)

type EventKind string

const (
	EventAccess    EventKind = "access"
	EventRebalance EventKind = "rebalance"
	EventRead      EventKind = "read"
	EventWrite     EventKind = "write"
	EventCommit    EventKind = "commit"
	EventAbort     EventKind = "abort"
	EventMigrate   EventKind = "migrate"
)

type WatermarkMode string

const (
	WatermarkProvided WatermarkMode = "provided"
	WatermarkDerived  WatermarkMode = "derive"
)

// Config is the normalized, unit-independent policy shared by all schema
// versions. Heat values use a canonical basis-point scale (0..10000).
type Config struct {
	TimeUnit         string        `json:"timeUnit"`
	CapacityUnit     string        `json:"capacityUnit"`
	HeatScale        int           `json:"heatScale"`
	MigrationScale   int           `json:"migrationScale"`
	WatermarkMode    WatermarkMode `json:"watermarkMode"`
	TickMS           int64         `json:"tickMs"`
	FixedBits        int           `json:"fixedBits"`
	HeatWindowMS     int64         `json:"heatWindowMs"`
	HeatDecayMS      int64         `json:"heatDecayMs"`
	HeatMaxCount     int64         `json:"heatMaxCount"`
	HotThreshold     int64         `json:"hotThreshold"`
	ColdThreshold    int64         `json:"coldThreshold"`
	MigrateHotLimit  int64         `json:"migrationHotLimit"`
	MigrateColdLimit int64         `json:"migrationColdLimit"`
	BatchMaxObjects  int           `json:"batchMaxObjects"`
	BatchMaxBytes    int64         `json:"batchMaxBytes"`
	Workers          int           `json:"workers"`
	PerLayerBuffer   int           `json:"perLayerBuffer"`
}

type Layer struct {
	ID             string    `json:"id"`
	Kind           LayerKind `json:"kind"`
	CapacityBytes  int64     `json:"capacityBytes"`
	AllowedObjects []string  `json:"allowedObjects"`
	DeclOrder      int       `json:"declOrder"`
}

type AccessSample struct {
	AtMS        int64  `json:"atMs"`
	Count       int64  `json:"count"`
	Watermark   *int64 `json:"watermark,omitempty"`
	SourceTick  int64  `json:"sourceTick,omitempty"`
	SourceIndex int    `json:"sourceIndex"`
}

type Object struct {
	ID           string         `json:"id"`
	SizeBytes    int64          `json:"sizeBytes"`
	InitialLayer string         `json:"initialLayer"`
	Priority     int            `json:"priority"`
	DeclOrder    int            `json:"declOrder"`
	Samples      []AccessSample `json:"samples"`
}

type Event struct {
	Order       int       `json:"order"`
	AtMS        int64     `json:"atMs"`
	Kind        EventKind `json:"kind"`
	ObjectID    string    `json:"objectId,omitempty"`
	Count       int64     `json:"count,omitempty"`
	BatchID     string    `json:"batchId,omitempty"`
	SourceLayer string    `json:"sourceLayer,omitempty"`
	TargetLayer string    `json:"targetLayer,omitempty"`
}

type Project struct {
	SchemaVersion string   `json:"schemaVersion"`
	Seed          int64    `json:"seed"`
	Config        Config   `json:"config"`
	Layers        []Layer  `json:"layers"`
	Objects       []Object `json:"objects"`
	Events        []Event  `json:"events"`
	SourceHash    string   `json:"-"`
}

func (p *Project) DeepCopy() *Project {
	raw, _ := json.Marshal(p)
	var out Project
	_ = json.Unmarshal(raw, &out)
	return &out
}

// SemanticKey deliberately excludes source schema spelling, units and
// watermark representation. Equal keys mean equal normalized semantics.
func (p *Project) SemanticKey() string {
	cp := p.DeepCopy()
	cp.SchemaVersion = ""
	cp.Config.TimeUnit = ""
	cp.Config.CapacityUnit = ""
	cp.Config.HeatScale = 0
	cp.Config.MigrationScale = 0
	cp.Config.WatermarkMode = ""
	cp.SourceHash = ""
	for i := range cp.Objects {
		cp.Objects[i].Samples = append([]AccessSample(nil), cp.Objects[i].Samples...)
		for j := range cp.Objects[i].Samples {
			cp.Objects[i].Samples[j].SourceTick = 0
			cp.Objects[i].Samples[j].SourceIndex = 0
			cp.Objects[i].Samples[j].Watermark = nil
		}
	}
	raw, _ := json.Marshal(cp)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (p *Project) Validate() error {
	if p.Seed < 0 {
		return bizError(CodeFieldType, p.SchemaVersion, "seed", "seed must be non-negative")
	}
	if p.Config.FixedBits < 16 || p.Config.FixedBits > 62 {
		return bizError(CodeInvalidScale, p.SchemaVersion, "config.fixedBits", "fixedBits must be in [16,62]")
	}
	maxFixed := int64(1)<<p.Config.FixedBits - 1
	thresholds := []struct {
		name  string
		value int64
	}{
		{"config.hotThreshold", p.Config.HotThreshold},
		{"config.coldThreshold", p.Config.ColdThreshold},
		{"config.migrationHotLimit", p.Config.MigrateHotLimit},
		{"config.migrationColdLimit", p.Config.MigrateColdLimit},
	}
	for _, threshold := range thresholds {
		if threshold.value < 0 || threshold.value > 10000 || threshold.value > maxFixed {
			return bizError(CodeInvalidScale, p.SchemaVersion, threshold.name, "heat threshold must be within [0,10000] and fixed width")
		}
	}
	if p.Config.HotThreshold < p.Config.ColdThreshold {
		return bizError(CodeInvalidScale, p.SchemaVersion, "config", "hot threshold must be >= cold threshold")
	}
	if p.Config.HeatWindowMS <= 0 || p.Config.HeatDecayMS <= 0 || p.Config.HeatMaxCount <= 0 {
		return bizError(CodeInvalidScale, p.SchemaVersion, "config", "heat window, decay and max count must be positive")
	}
	if p.Config.BatchMaxObjects <= 0 {
		return bizError(CodeZeroBatchLimit, p.SchemaVersion, "config.batchMaxObjects", "batch object limit must be positive")
	}
	if p.Config.BatchMaxBytes <= 0 {
		return bizError(CodeZeroBatchLimit, p.SchemaVersion, "config.batchMaxBytes", "batch byte limit must be positive")
	}
	if p.Config.Workers <= 0 || p.Config.Workers > 8 {
		return bizError(CodeThreadBudget, p.SchemaVersion, "config.workers", "workers must be in [1,8]")
	}
	if p.Config.PerLayerBuffer <= 0 || p.Config.PerLayerBuffer > 4096 {
		return bizError(CodeBufferBudget, p.SchemaVersion, "config.perLayerBuffer", "per-layer buffer must be in [1,4096]")
	}
	layerIDs := map[string]*Layer{}
	for i := range p.Layers {
		l := &p.Layers[i]
		if _, ok := layerIDs[l.ID]; ok {
			return bizError(CodeDuplicateLayer, p.SchemaVersion, fmt.Sprintf("layers[%d].id", i), "duplicate layer id %q", l.ID)
		}
		layerIDs[l.ID] = l
		if l.CapacityBytes < 0 {
			return bizError(CodeNegativeCapacity, p.SchemaVersion, fmt.Sprintf("layers[%d].capacity", i), "capacity must not be negative")
		}
	}
	objectIDs := map[string]*Object{}
	for i := range p.Objects {
		o := &p.Objects[i]
		if _, ok := objectIDs[o.ID]; ok {
			return bizError(CodeDuplicateObject, p.SchemaVersion, fmt.Sprintf("objects[%d].id", i), "duplicate object id %q", o.ID)
		}
		objectIDs[o.ID] = o
		if o.SizeBytes <= 0 {
			return bizError(CodeFieldType, p.SchemaVersion, fmt.Sprintf("objects[%d].size", i), "object size must be positive")
		}
		if _, ok := layerIDs[o.InitialLayer]; !ok {
			return bizError(CodeMissingLayer, p.SchemaVersion, fmt.Sprintf("objects[%d].initialLayer", i), "initial layer %q does not exist", o.InitialLayer)
		}
		if !layerAllows(layerIDs[o.InitialLayer], o.ID) {
			return bizError(CodeObjectNotAllowed, p.SchemaVersion, fmt.Sprintf("objects[%d].initialLayer", i), "layer %q does not allow object %q", o.InitialLayer, o.ID)
		}
		for j := range o.Samples {
			if o.Samples[j].Count < 0 {
				return bizError(CodeInvalidCount, p.SchemaVersion, fmt.Sprintf("objects[%d].samples[%d].count", i, j), "access count must not be negative")
			}
			if j > 0 && o.Samples[j].AtMS <= o.Samples[j-1].AtMS {
				return bizError(CodeDuplicateSample, p.SchemaVersion, fmt.Sprintf("objects[%d].samples[%d].at", i, j), "sample times must be strictly increasing")
			}
			if w := o.Samples[j].Watermark; w != nil && (*w < 0 || *w > 10000 || *w > maxFixed) {
				return bizError(CodeInvalidScale, p.SchemaVersion, fmt.Sprintf("objects[%d].samples[%d].watermark", i, j), "watermark must be within fixed scale")
			}
		}
	}
	orderIDs := map[int]bool{}
	lastTime := int64(-1 << 62)
	for i := range p.Events {
		ev := &p.Events[i]
		if orderIDs[ev.Order] {
			return bizError(CodeDuplicateEventOrder, p.SchemaVersion, fmt.Sprintf("events[%d].order", i), "duplicate event order %d", ev.Order)
		}
		orderIDs[ev.Order] = true
		if ev.AtMS < lastTime {
			return bizError(CodeEventOrder, p.SchemaVersion, fmt.Sprintf("events[%d].at", i), "events must be time ordered")
		}
		lastTime = ev.AtMS
		if ev.ObjectID != "" {
			if _, ok := objectIDs[ev.ObjectID]; !ok {
				return bizError(CodeMissingObject, p.SchemaVersion, fmt.Sprintf("events[%d].object", i), "object %q does not exist", ev.ObjectID)
			}
		}
		if ev.SourceLayer != "" {
			if _, ok := layerIDs[ev.SourceLayer]; !ok {
				return bizError(CodeMissingLayer, p.SchemaVersion, fmt.Sprintf("events[%d].sourceLayer", i), "layer %q does not exist", ev.SourceLayer)
			}
		}
		if ev.TargetLayer != "" {
			if _, ok := layerIDs[ev.TargetLayer]; !ok {
				return bizError(CodeMissingLayer, p.SchemaVersion, fmt.Sprintf("events[%d].targetLayer", i), "layer %q does not exist", ev.TargetLayer)
			}
		}
		switch ev.Kind {
		case EventAccess:
			if ev.ObjectID == "" || ev.Count < 0 {
				return bizError(CodeInvalidEvent, p.SchemaVersion, fmt.Sprintf("events[%d]", i), "access needs object and non-negative count")
			}
		case EventRead, EventWrite:
			if ev.ObjectID == "" {
				return bizError(CodeInvalidEvent, p.SchemaVersion, fmt.Sprintf("events[%d]", i), "read/write needs object")
			}
		case EventMigrate:
			if ev.ObjectID == "" || ev.SourceLayer == "" || ev.TargetLayer == "" {
				return bizError(CodeInvalidEvent, p.SchemaVersion, fmt.Sprintf("events[%d]", i), "migrate needs object, source and target")
			}
			if ev.SourceLayer == ev.TargetLayer {
				return bizError(CodeInvalidEvent, p.SchemaVersion, fmt.Sprintf("events[%d]", i), "migrate source and target must differ")
			}
		case EventCommit, EventAbort:
			// Empty BatchID means the currently active batch.
		case EventRebalance:
			// Optional source/target restrict the candidate direction.
		default:
			return bizError(CodeInvalidEvent, p.SchemaVersion, fmt.Sprintf("events[%d].kind", i), "unknown event kind %q", ev.Kind)
		}
	}
	occupancy := map[string]int64{}
	for _, o := range p.Objects {
		occupancy[o.InitialLayer] += o.SizeBytes
		if occupancy[o.InitialLayer] > layerIDs[o.InitialLayer].CapacityBytes {
			return bizError(CodeCapacityExceeded, p.SchemaVersion, "objects", "initial occupancy exceeds layer %q capacity", o.InitialLayer)
		}
	}
	return nil
}

func layerAllows(l *Layer, objectID string) bool {
	if len(l.AllowedObjects) == 0 {
		return true
	}
	for _, id := range l.AllowedObjects {
		if id == objectID {
			return true
		}
	}
	return false
}

func (p *Project) SortedLayers() []Layer {
	out := append([]Layer(nil), p.Layers...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].DeclOrder < out[j].DeclOrder })
	return out
}

func (p *Project) SortedObjects() []Object {
	out := append([]Object(nil), p.Objects...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].DeclOrder < out[j].DeclOrder })
	return out
}

func (p *Project) SortedEvents() []Event {
	out := append([]Event(nil), p.Events...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].AtMS != out[j].AtMS {
			return out[i].AtMS < out[j].AtMS
		}
		return out[i].Order < out[j].Order
	})
	return out
}
