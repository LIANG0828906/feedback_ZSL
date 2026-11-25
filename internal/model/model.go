package model

import "encoding/json"

const (
	SchemaV1 = "frameplan/v1"
	SchemaV2 = "frameplan/v2"
	SchemaV3 = "frameplan/v3"

	EventHeaders      = "HEADERS"
	EventData         = "DATA"
	EventWindowUpdate = "WINDOW_UPDATE"
	EventRSTStream    = "RST_STREAM"
	EventPriority     = "PRIORITY"

	TimeFrameSequence = "frame_sequence"
	TimeMilliseconds  = "milliseconds"

	OrphanReparent = "reparent"
	OrphanInherit  = "inherit"

	StateIdle      = "IDLE"
	StateOpen      = "OPEN"
	StateBlocked   = "BLOCKED"
	StateDrained   = "DRAINED"
	StateReset     = "RESET"
	StateClosed    = "CLOSED"
	StateBackpress = "BACKPRESSURED"
)

// Reason codes are stable and intentionally share E_ prefix with API errors.
const (
	ReasonNone             = ""
	ReasonStreamWindow     = "E_STREAM_WINDOW_ZERO"
	ReasonConnectionWindow = "E_CONNECTION_WINDOW_ZERO"
	ReasonFrameTooLarge    = "E_FRAME_TOO_LARGE"
	ReasonBodyExhausted    = "E_BODY_SEGMENTS_EXHAUSTED"
	ReasonReset            = "E_STREAM_RESET"
	ReasonClosed           = "E_STREAM_CLOSED"
	ReasonAwaitingData     = "AWAITING_DATA"
	ReasonBufferFull       = "E_BUFFER_FULL"
)

type Project struct {
	SchemaVersion     string         `json:"schema_version"`
	SourceVersion     string         `json:"source_version"`
	ProjectID         string         `json:"project_id"`
	Seed              int64          `json:"seed"`
	FixedPointBits    int            `json:"fixed_point_bits"`
	WeightScale       string         `json:"weight_scale"`
	TimeBase          string         `json:"time_base"`
	FrameIntervalMS   int64          `json:"frame_interval_ms"`
	OrphanPolicy      string         `json:"orphan_policy"`
	FrameWatermark    int64          `json:"frame_watermark"`
	WatermarkExplicit bool           `json:"watermark_explicit"`
	Connection        ConnectionSpec `json:"connection"`
	Streams           []StreamSpec   `json:"streams"`
	Events            []Event        `json:"events"`
}

type ConnectionSpec struct {
	ID                 string `json:"id"`
	InitialWindowBytes int64  `json:"initial_window_bytes"`
	MaxWindowBytes     int64  `json:"max_window_bytes"`
}

type StreamSpec struct {
	ID                 int     `json:"id"`
	Name               string  `json:"name,omitempty"`
	ParentID           int     `json:"parent_id"`
	WeightPermyriad    int64   `json:"weight_permyriad"`
	InitialWindowBytes int64   `json:"initial_window_bytes"`
	MaxWindowBytes     int64   `json:"max_window_bytes"`
	MaxFrameBytes      int64   `json:"max_frame_bytes"`
	BodySegments       []int64 `json:"body_segments"`
	OrphanPolicy       string  `json:"orphan_policy,omitempty"`
}

type Event struct {
	Index           int    `json:"index"`
	FrameSeq        int64  `json:"frame_seq"`
	TimeMS          int64  `json:"time_ms"`
	Type            string `json:"type"`
	StreamID        int    `json:"stream_id"`
	LengthBytes     int64  `json:"length_bytes,omitempty"`
	IncrementBytes  int64  `json:"increment_bytes,omitempty"`
	WeightPermyriad int64  `json:"weight_permyriad,omitempty"`
	SegmentIndex    int    `json:"segment_index,omitempty"`
	Fin             bool   `json:"fin,omitempty"`
	Watermark       *int64 `json:"watermark,omitempty"`
}

type StreamSnapshot struct {
	StreamID            int              `json:"stream_id"`
	Name                string           `json:"name,omitempty"`
	ParentID            int              `json:"parent_id"`
	EffectiveParentID   int              `json:"effective_parent_id"`
	WeightPermyriad     int64            `json:"weight_permyriad"`
	WindowRemaining     int64            `json:"window_remaining"`
	SentBytes           int64            `json:"sent_bytes"`
	SendableBytes       int64            `json:"sendable_bytes"`
	PendingFrames       int              `json:"pending_frames"`
	PendingBytes        int64            `json:"pending_bytes"`
	State               string           `json:"state"`
	Reasons             []string         `json:"reasons"`
	BlockedSinceMS      *int64           `json:"blocked_since_ms,omitempty"`
	ReasonSinceMS       map[string]int64 `json:"reason_since_ms,omitempty"`
	SegmentsTotal       int              `json:"segments_total"`
	SegmentsSent        int              `json:"segments_sent"`
	ReleasedCredit      int64            `json:"released_credit"`
	ReleasedBufferBytes int64            `json:"released_buffer_bytes,omitempty"`
}

type SendRecord struct {
	EventIndex       int    `json:"event_index"`
	LogicalMS        int64  `json:"logical_ms"`
	FrameSeq         int64  `json:"frame_seq"`
	StreamID         int    `json:"stream_id"`
	WeightPermyriad  int64  `json:"weight_permyriad"`
	AllocatedWeight  int64  `json:"allocated_weight"`
	BytesSent        int64  `json:"bytes_sent"`
	ConnectionWindow int64  `json:"connection_window_after"`
	StreamWindow     int64  `json:"stream_window_after"`
	SegmentIndex     int    `json:"segment_index"`
	Reason           string `json:"selection_reason,omitempty"`
}

type BlockedEntry struct {
	StreamID int      `json:"stream_id"`
	Reasons  []string `json:"reasons"`
	SinceMS  int64    `json:"since_ms"`
}

type Trace struct {
	EventIndex          int              `json:"event_index"`
	FrameSeq            int64            `json:"frame_seq"`
	LogicalMS           int64            `json:"logical_ms"`
	EventType           string           `json:"event_type"`
	SourceStreamID      int              `json:"source_stream_id,omitempty"`
	ConnectionWindow    int64            `json:"connection_window"`
	ConnectionCap       int64            `json:"connection_cap"`
	Streams             []StreamSnapshot `json:"streams"`
	Blocked             []BlockedEntry   `json:"blocked"`
	Sends               []SendRecord     `json:"sends"`
	RemovedStreamID     int              `json:"removed_stream_id,omitempty"`
	RemovedAtMS         int64            `json:"removed_at_ms,omitempty"`
	ReleasedCredit      int64            `json:"released_credit,omitempty"`
	ReleasedBufferBytes int64            `json:"released_buffer_bytes,omitempty"`
	BackpressureCode    string           `json:"backpressure_code,omitempty"`
	BackpressureContext map[string]any   `json:"backpressure_context,omitempty"`
	Error               *ErrorReport     `json:"error,omitempty"`
	CacheStatus         string           `json:"cache_status,omitempty"`
	Invalidation        []string         `json:"invalidation_sources,omitempty"`
}

type ErrorReport struct {
	Code    string         `json:"code"`
	Path    string         `json:"path,omitempty"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
}

func (e *ErrorReport) JSON() string {
	b, _ := json.Marshal(e)
	return string(b)
}

type RunResult struct {
	ProjectID         string           `json:"project_id"`
	SchemaVersion     string           `json:"schema_version"`
	CheckpointVersion int              `json:"checkpoint_version,omitempty"`
	CommittedThrough  int              `json:"committed_through"`
	Traces            []Trace          `json:"traces"`
	Sends             []SendRecord     `json:"sends"`
	FinalStreams      []StreamSnapshot `json:"final_streams"`
	OverflowEvents    []string         `json:"overflow_events,omitempty"`
}

// Edit describes a deterministic frame-engineering mutation.
type Edit struct {
	Kind        string `json:"kind"`
	StreamID    int    `json:"stream_id,omitempty"`
	EventIndex  int    `json:"event_index,omitempty"`
	Weight      int64  `json:"weight_permyriad,omitempty"`
	ParentID    int    `json:"parent_id,omitempty"`
	WindowBytes int64  `json:"window_bytes,omitempty"`
	Event       *Event `json:"event,omitempty"`
}

const (
	EditWeight      = "change_weight"
	EditParent      = "change_parent"
	EditWindow      = "change_initial_window"
	EditAppendEvent = "append_event"
	EditDeleteEvent = "delete_event"
	EditUpdateEvent = "update_event"
)
