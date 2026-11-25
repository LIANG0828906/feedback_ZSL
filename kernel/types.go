package kernel

import (
	"time"

	"example.com/multiscreen-cast/protocol"
)

type Progress struct {
	Committed int `json:"committed"`
	Total     int `json:"total"`
}

func (p Progress) Complete() bool {
	return p.Total > 0 && p.Committed >= p.Total
}

type MappingParams struct {
	AlgorithmVersion string              `json:"algorithm_version"`
	TopologyVersion  int                 `json:"topology_version"`
	Topology         protocol.Topology   `json:"topology"`
	Capability       protocol.Capability `json:"capability"`
}

type ScreenResult struct {
	DisplayID        string        `json:"display_id"`
	Window           int           `json:"window"`
	SourceWindow     int           `json:"source_window"`
	Pixel            protocol.Rect `json:"pixel"`
	World            protocol.Rect `json:"world"`
	ScreenWorld      protocol.Rect `json:"screen_world"`
	Mapping          MappingParams `json:"mapping"`
	SourceHash       string        `json:"source_hash"`
	PixelBufferBytes int64         `json:"pixel_buffer_bytes"`
	BufferChunks     int           `json:"buffer_chunks"`
}

type QueueRecord struct {
	DisplayID string `json:"display_id"`
	Worker    int    `json:"worker"`
	Reason    string `json:"reason"`
}

type AdjudicationItem struct {
	Seq       int64              `json:"seq"`
	Kind      protocol.EventKind `json:"kind"`
	DisplayID string             `json:"display_id"`
	Phase     int                `json:"phase"`
	Status    string             `json:"status"`
}

type Adjudication struct {
	Time  int64              `json:"time"`
	Rule  string             `json:"rule"`
	Items []AdjudicationItem `json:"items"`
}

type EventEffect struct {
	Seq             int64                 `json:"seq"`
	Time            int64                 `json:"time"`
	Kind            protocol.EventKind    `json:"kind"`
	DisplayID       string                `json:"display_id,omitempty"`
	EffectiveTime   int64                 `json:"effective_time"`
	CommittedWindow int                   `json:"committed_window"`
	StateBefore     protocol.SessionState `json:"state_before"`
	StateAfter      protocol.SessionState `json:"state_after"`
	TopologyVersion int                   `json:"topology_version"`
	EndpointsAfter  []string              `json:"endpoints_after"`
	ReplayWindow    int                   `json:"replay_window,omitempty"`
	Replay          *ScreenResult         `json:"replay,omitempty"`
}

type WindowSnapshot struct {
	Window          int                   `json:"window"`
	Time            int64                 `json:"time"`
	State           protocol.SessionState `json:"state"`
	TopologyVersion int                   `json:"topology_version"`
	Endpoints       []string              `json:"endpoints"`
	Screens         []ScreenResult        `json:"screens"`
	QueueRecords    []QueueRecord         `json:"queue_records,omitempty"`
	Adjudications   []Adjudication        `json:"adjudications,omitempty"`
}

type StateTransition struct {
	Seq    int64                 `json:"seq"`
	Time   int64                 `json:"time"`
	Kind   protocol.EventKind    `json:"kind"`
	Before protocol.SessionState `json:"before"`
	After  protocol.SessionState `json:"after"`
}

type Trace struct {
	Windows        []WindowSnapshot      `json:"windows"`
	EventEffects   []EventEffect         `json:"event_effects"`
	Adjudications  []Adjudication        `json:"adjudications"`
	Transitions    []StateTransition     `json:"transitions"`
	FinalState     protocol.SessionState `json:"final_state"`
	FinalEndpoints []string              `json:"final_endpoints"`
}

type ScreenState struct {
	DisplayID    string            `json:"display_id"`
	Endpoint     protocol.Endpoint `json:"endpoint"`
	LastRegion   *protocol.Rect    `json:"last_region,omitempty"`
	LastResult   *ScreenResult     `json:"last_result,omitempty"`
	SourceWindow int               `json:"source_window,omitempty"`
}

type RuntimeSnapshot struct {
	SessionID        string                `json:"session_id"`
	AlgorithmVersion string                `json:"algorithm_version"`
	ContentHash      string                `json:"content_hash"`
	State            protocol.SessionState `json:"state"`
	TopologyVersion  int                   `json:"topology_version"`
	Endpoints        []string              `json:"endpoints"`
	Displays         []protocol.Endpoint   `json:"displays"`
	Screens          []ScreenState         `json:"screens"`
	LastRegion       *protocol.Rect        `json:"last_region,omitempty"`
	CommittedWindows int                   `json:"committed_windows"`
	TotalWindows     int                   `json:"total_windows"`
	NextAction       int                   `json:"next_action"`
	Trace            Trace                 `json:"trace"`
}

type Step struct {
	Action     protocol.Action       `json:"action"`
	Event      *EventEffect          `json:"event,omitempty"`
	Window     *WindowSnapshot       `json:"window,omitempty"`
	StateAfter protocol.SessionState `json:"state_after"`
	Progress   Progress              `json:"progress"`
}

type WindowFailure struct {
	Window    int       `json:"window"`
	DisplayID string    `json:"display_id,omitempty"`
	Message   string    `json:"message"`
	At        time.Time `json:"at"`
}

type CheckpointSink interface {
	SaveCheckpoint(snapshot RuntimeSnapshot) error
}

type Difference struct {
	Window    int    `json:"window"`
	DisplayID string `json:"display_id,omitempty"`
	Field     string `json:"field"`
	Left      string `json:"left"`
	Right     string `json:"right"`
}
