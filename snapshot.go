package layercache

type ObjectSnapshot struct {
	ID               string    `json:"id"`
	LayerID          string    `json:"layerId"`
	AnswerLayer      string    `json:"answerLayer"`
	TargetLayer      string    `json:"targetLayer,omitempty"`
	Pending          bool      `json:"pending"`
	PendingWrites    int64     `json:"pendingWrites"`
	SizeBytes        int64     `json:"sizeBytes"`
	Heat             int64     `json:"heat"`
	HeatLevel        HeatLevel `json:"heatLevel"`
	LastAccessMS     int64     `json:"lastAccessMs"`
	CountFromMS      int64     `json:"countFromMs"`
	CountToMS        int64     `json:"countToMs"`
	CountInWindow    int64     `json:"countInWindow"`
	Watermark        *int64    `json:"watermark,omitempty"`
	WatermarkValid   bool      `json:"watermarkValid"`
	OverflowDetected bool      `json:"overflowDetected"`
	Saturated        bool      `json:"saturated"`
}

type LayerSnapshot struct {
	ID            string    `json:"id"`
	Kind          LayerKind `json:"kind"`
	CapacityBytes int64     `json:"capacityBytes"`
	Occupancy     int64     `json:"occupancyBytes"`
	ObjectCount   int       `json:"objectCount"`
	Objects       []string  `json:"objects"`
	PendingBytes  int64     `json:"pendingBytes"`
}

type Migration struct {
	ObjectID    string `json:"objectId"`
	SourceLayer string `json:"sourceLayer"`
	TargetLayer string `json:"targetLayer"`
	SizeBytes   int64  `json:"sizeBytes"`
}

type DeferredDecision struct {
	ObjectID   string `json:"objectId"`
	Direction  string `json:"direction"`
	Position   int    `json:"position"`
	Constraint string `json:"constraint"`
	Detail     string `json:"detail"`
}

type BatchResult struct {
	BatchID       string             `json:"batchId"`
	EventOrder    int                `json:"eventOrder"`
	TimeMS        int64              `json:"timeMs"`
	Promote       []Migration        `json:"promote"`
	Demote        []Migration        `json:"demote"`
	Deferred      []DeferredDecision `json:"deferred"`
	PlannedBefore []LayerOccupancy   `json:"plannedBefore"`
	PlannedAfter  []LayerOccupancy   `json:"plannedAfter"`
	PromotedBytes int64              `json:"promotedBytes"`
	DemotedBytes  int64              `json:"demotedBytes"`
}

type LayerOccupancy struct {
	LayerID   string `json:"layerId"`
	Occupancy int64  `json:"occupancyBytes"`
}

type GateObservation struct {
	EventOrder    int    `json:"eventOrder"`
	ObjectID      string `json:"objectId"`
	Operation     string `json:"operation"`
	AnswerLayer   string `json:"answerLayer"`
	SourceLayer   string `json:"sourceLayer,omitempty"`
	TargetLayer   string `json:"targetLayer,omitempty"`
	Pending       bool   `json:"pending"`
	Rejected      bool   `json:"rejected"`
	Queued        bool   `json:"queued"`
	TargetVisible bool   `json:"targetVisible"`
	Reason        string `json:"reason,omitempty"`
}

type CommitResult struct {
	BatchID        string           `json:"batchId"`
	TimeMS         int64            `json:"timeMs"`
	Migrations     []Migration      `json:"migrations"`
	OccupancyAfter []LayerOccupancy `json:"occupancyAfter"`
}

type AbortResult struct {
	BatchID    string      `json:"batchId"`
	TimeMS     int64       `json:"timeMs"`
	Migrations []Migration `json:"migrations"`
}

type TickResult struct {
	Index    int               `json:"index"`
	TimeMS   int64             `json:"timeMs"`
	Objects  []ObjectSnapshot  `json:"objects"`
	Layers   []LayerSnapshot   `json:"layers"`
	Batches  []BatchResult     `json:"batches"`
	Gates    []GateObservation `json:"gates"`
	Commits  []CommitResult    `json:"commits"`
	Aborts   []AbortResult     `json:"aborts"`
	Revision string            `json:"revision"`
}

type RunMetrics struct {
	Layer []LayerTiming `json:"layer"`
}

type LayerTiming struct {
	LayerID        string `json:"layerId"`
	HeatNanos      int64  `json:"heatNanos"`
	CandidateNanos int64  `json:"candidateNanos"`
	LastWorker     int    `json:"lastWorker"`
	Backpressure   bool   `json:"backpressure"`
}

type RunResult struct {
	ProjectKey         string       `json:"projectKey"`
	Ticks              []TickResult `json:"ticks"`
	Metrics            RunMetrics   `json:"metrics"`
	Interrupted        bool         `json:"interrupted"`
	LastCommittedTick  int64        `json:"lastCommittedTick"`
	LastCommittedBatch string       `json:"lastCommittedBatch"`
}

type InvalidationReport struct {
	Source          string             `json:"source"`
	StartTimeMS     int64              `json:"startTimeMs"`
	AffectedObjects []string           `json:"affectedObjects"`
	AffectedLayers  []string           `json:"affectedLayers"`
	Closure         []DependencyEdge   `json:"closure"`
	Ticks           []TickInvalidation `json:"ticks"`
}

type DependencyEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type TickInvalidation struct {
	TimeMS         int64    `json:"timeMs"`
	Recomputed     bool     `json:"recomputed"`
	Reused         bool     `json:"reused"`
	Reasons        []string `json:"reasons,omitempty"`
	ChangedObjects []string `json:"changedObjects,omitempty"`
	ChangedLayers  []string `json:"changedLayers,omitempty"`
}

type IncrementalResult struct {
	Run    RunResult          `json:"run"`
	Report InvalidationReport `json:"report"`
}
