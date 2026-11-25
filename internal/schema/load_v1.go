package schema

import (
	"encoding/json"
	"sort"
	"strings"

	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/fixedpoint"
	"multiplexgw/internal/model"
)

type rawV1 struct {
	Schema          string `json:"schema"`
	ProjectID       string `json:"project_id"`
	Seed            int64  `json:"seed"`
	FrameIntervalMS int64  `json:"frame_interval_ms"`
	FrameWatermark  *int64 `json:"frame_watermark"`
	OrphanPolicy    string `json:"orphan_policy"`
	Connection      struct {
		ID                 string `json:"connection_id"`
		InitialWindowBytes int64  `json:"initial_window_bytes"`
		MaxWindowBytes     int64  `json:"max_window_bytes"`
	} `json:"connection"`
	Streams []struct {
		StreamID               int     `json:"stream_id"`
		Name                   string  `json:"name"`
		InitialWindowRemaining int64   `json:"initial_window_remaining_bytes"`
		MaxWindowBytes         int64   `json:"max_window_bytes"`
		MaxFrameSizeBytes      int64   `json:"max_frame_size_bytes"`
		BodySegmentBytes       []int64 `json:"body_segment_bytes"`
	} `json:"streams"`
	Priorities []struct {
		StreamID       int    `json:"stream_id"`
		ParentStreamID int    `json:"parent_stream_id"`
		WeightPermille int64  `json:"weight_permille"`
		OrphanPolicy   string `json:"orphan_policy"`
	} `json:"priorities"`
	PriorityTree json.RawMessage `json:"priority_tree"`
	Frames       []struct {
		FrameNo        int64  `json:"frame_no"`
		Kind           string `json:"kind"`
		StreamID       int    `json:"stream_id"`
		PayloadBytes   int64  `json:"payload_bytes"`
		IncrementBytes int64  `json:"increment_bytes"`
		SegmentIndex   int    `json:"segment_index"`
		Fin            bool   `json:"fin"`
		Watermark      *int64 `json:"watermark"`
	} `json:"frames"`
}

func loadV1(path string, data []byte) (*model.Project, error) {
	var raw rawV1
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, bizerr.Wrap(bizerr.SchemaParse, path, err, "v1 JSON decode failed")
	}
	if len(raw.PriorityTree) > 0 && string(raw.PriorityTree) != "null" {
		return nil, bizerr.New(bizerr.SchemaConflict, path+".priorities",
			"v1 document contains both flat priorities and priority_tree")
	}
	if len(raw.Priorities) == 0 {
		return nil, bizerr.New(bizerr.FieldMapping, path+".priorities", "v1 requires flat priority table")
	}
	prio := map[int]rawV1Priority{}
	for _, p := range raw.Priorities {
		if _, dup := prio[p.StreamID]; dup {
			return nil, bizerr.New(bizerr.SchemaConflict, path+".priorities", "duplicate priority for stream %d", p.StreamID)
		}
		w, err := fixedpoint.PermilleToPermyriad(p.WeightPermille)
		if err != nil {
			return nil, err
		}
		prio[p.StreamID] = rawV1Priority{Parent: p.ParentStreamID, Weight: w, Orphan: p.OrphanPolicy}
	}
	interval := raw.FrameIntervalMS
	if interval <= 0 {
		interval = 10
	}
	p := &model.Project{
		SchemaVersion:     model.SchemaV1,
		SourceVersion:     raw.Schema,
		ProjectID:         raw.ProjectID,
		Seed:              raw.Seed,
		FixedPointBits:    fixedpoint.FractionBits,
		WeightScale:       "permille->permyriad-q16",
		TimeBase:          model.TimeFrameSequence,
		FrameIntervalMS:   interval,
		OrphanPolicy:      defaultString(raw.OrphanPolicy, model.OrphanReparent),
		WatermarkExplicit: raw.FrameWatermark != nil,
		Connection: model.ConnectionSpec{
			ID:                 raw.Connection.ID,
			InitialWindowBytes: raw.Connection.InitialWindowBytes,
			MaxWindowBytes:     raw.Connection.MaxWindowBytes,
		},
	}
	if raw.FrameWatermark != nil {
		p.FrameWatermark = *raw.FrameWatermark
	}
	for _, s := range raw.Streams {
		pr, ok := prio[s.StreamID]
		if !ok {
			return nil, bizerr.New(bizerr.FieldMapping, path+".priorities", "stream %d missing priority row", s.StreamID)
		}
		p.Streams = append(p.Streams, model.StreamSpec{
			ID:                 s.StreamID,
			Name:               s.Name,
			ParentID:           pr.Parent,
			WeightPermyriad:    pr.Weight,
			InitialWindowBytes: s.InitialWindowRemaining,
			MaxWindowBytes:     s.MaxWindowBytes,
			MaxFrameBytes:      s.MaxFrameSizeBytes,
			BodySegments:       append([]int64(nil), s.BodySegmentBytes...),
			OrphanPolicy:       defaultString(pr.Orphan, p.OrphanPolicy),
		})
	}
	sort.Slice(p.Streams, func(i, j int) bool { return p.Streams[i].ID < p.Streams[j].ID })

	for _, f := range raw.Frames {
		ev := model.Event{
			FrameSeq:       f.FrameNo,
			TimeMS:         f.FrameNo * interval,
			Type:           normalizeKind(f.Kind),
			StreamID:       f.StreamID,
			LengthBytes:    f.PayloadBytes,
			IncrementBytes: f.IncrementBytes,
			SegmentIndex:   f.SegmentIndex,
			Fin:            f.Fin,
			Watermark:      f.Watermark,
		}
		p.Events = append(p.Events, ev)
	}
	sort.SliceStable(p.Events, func(i, j int) bool {
		if p.Events[i].FrameSeq != p.Events[j].FrameSeq {
			return p.Events[i].FrameSeq < p.Events[j].FrameSeq
		}
		return p.Events[i].TimeMS < p.Events[j].TimeMS
	})
	for i := range p.Events {
		p.Events[i].Index = i
	}
	if !p.WatermarkExplicit && len(p.Events) > 0 {
		p.FrameWatermark = p.Events[len(p.Events)-1].FrameSeq
	}
	if err := Validate(p); err != nil {
		return nil, err
	}
	return p, nil
}

type rawV1Priority struct {
	Parent int
	Weight int64
	Orphan string
}

func normalizeKind(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "HEADERS", "HEADER":
		return model.EventHeaders
	case "DATA":
		return model.EventData
	case "WINDOW_UPDATE", "WINDOW-UPDATE", "WINDOWUPDATE":
		return model.EventWindowUpdate
	case "RST_STREAM", "RST-STREAM", "RSTSTREAM", "RESET":
		return model.EventRSTStream
	case "PRIORITY":
		return model.EventPriority
	default:
		return strings.ToUpper(s)
	}
}

func defaultString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
