package schema

import (
	"encoding/json"
	"sort"
	"strings"

	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/model"
)

type rawV3 struct {
	APIVersion    string `json:"apiVersion"`
	EngineeringID string `json:"engineeringId"`
	RandomSeed    int64  `json:"randomSeed"`
	Clock         struct {
		Basis  string `json:"basis"`
		TickMS int64  `json:"tickMs"`
	} `json:"clock"`
	WaterLevel *int64 `json:"waterLevel"`
	OrphanRule string `json:"orphanRule"`
	Connection struct {
		CID          string `json:"cid"`
		CreditMode   string `json:"creditMode"`
		CreditBytes  *int64 `json:"creditBytes"`
		BudgetBytes  *int64 `json:"budgetBytes"`
		SentBytes    *int64 `json:"sentBytes"`
		CeilingBytes int64  `json:"ceilingBytes"`
	} `json:"connection"`
	Flows []struct {
		FID          int     `json:"fid"`
		Name         string  `json:"name"`
		UpstreamFID  int     `json:"upstreamFid"`
		ShareMyriad  int64   `json:"shareMyriad"`
		CreditMode   string  `json:"creditMode"`
		CreditBytes  *int64  `json:"creditBytes"`
		BudgetBytes  *int64  `json:"budgetBytes"`
		SentBytes    *int64  `json:"sentBytes"`
		CeilingBytes int64   `json:"ceilingBytes"`
		MaxPayload   int64   `json:"maxPayloadBytes"`
		BodyPlan     []int64 `json:"bodyPlan"`
		OrphanRule   string  `json:"orphanRule"`
	} `json:"flows"`
	Timeline []struct {
		Tick         int64  `json:"tick"`
		Op           string `json:"op"`
		FID          int    `json:"fid"`
		PayloadBytes int64  `json:"payloadBytes"`
		UpdateBytes  int64  `json:"updateBytes"`
		ShareMyriad  int64  `json:"shareMyriad"`
		Segment      int    `json:"segment"`
		Finish       bool   `json:"finish"`
	} `json:"timeline"`
}

func loadV3(path string, data []byte) (*model.Project, error) {
	var raw rawV3
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, bizerr.Wrap(bizerr.SchemaParse, path, err, "v3 JSON decode failed")
	}
	interval := raw.Clock.TickMS
	if interval <= 0 {
		interval = 10
	}
	basis := strings.ToLower(raw.Clock.Basis)
	if basis == "" {
		basis = model.TimeFrameSequence
	}
	if basis != model.TimeFrameSequence && basis != model.TimeMilliseconds && basis != "frame_number" && basis != "millisecond" {
		return nil, bizerr.New(bizerr.FieldMapping, path+".clock.basis", "unsupported clock basis %q", raw.Clock.Basis)
	}
	connWindow, err := resolveV3Window(path+".connection", raw.Connection.CreditMode, raw.Connection.CreditBytes, raw.Connection.BudgetBytes, raw.Connection.SentBytes)
	if err != nil {
		return nil, err
	}
	p := &model.Project{
		SchemaVersion:     model.SchemaV3,
		SourceVersion:     raw.APIVersion,
		ProjectID:         raw.EngineeringID,
		Seed:              raw.RandomSeed,
		FixedPointBits:    16,
		WeightScale:       "myriad-q16",
		TimeBase:          model.TimeFrameSequence,
		FrameIntervalMS:   interval,
		OrphanPolicy:      mapOrphanRule(raw.OrphanRule),
		WatermarkExplicit: raw.WaterLevel != nil,
		Connection: model.ConnectionSpec{
			ID:                 raw.Connection.CID,
			InitialWindowBytes: connWindow,
			MaxWindowBytes:     raw.Connection.CeilingBytes,
		},
	}
	if raw.WaterLevel != nil {
		p.FrameWatermark = *raw.WaterLevel
	}
	for _, f := range raw.Flows {
		w, err := resolveV3Window(path+".flows", f.CreditMode, f.CreditBytes, f.BudgetBytes, f.SentBytes)
		if err != nil {
			return nil, err
		}
		p.Streams = append(p.Streams, model.StreamSpec{
			ID:                 f.FID,
			Name:               f.Name,
			ParentID:           f.UpstreamFID,
			WeightPermyriad:    f.ShareMyriad,
			InitialWindowBytes: w,
			MaxWindowBytes:     f.CeilingBytes,
			MaxFrameBytes:      f.MaxPayload,
			BodySegments:       append([]int64(nil), f.BodyPlan...),
			OrphanPolicy:       mapOrphanRule(defaultString(f.OrphanRule, raw.OrphanRule)),
		})
	}
	sort.Slice(p.Streams, func(i, j int) bool { return p.Streams[i].ID < p.Streams[j].ID })

	for _, item := range raw.Timeline {
		ev := model.Event{
			Type:            normalizeKind(item.Op),
			StreamID:        item.FID,
			LengthBytes:     item.PayloadBytes,
			IncrementBytes:  item.UpdateBytes,
			WeightPermyriad: item.ShareMyriad,
			SegmentIndex:    item.Segment,
			Fin:             item.Finish,
		}
		if basis == model.TimeMilliseconds || basis == "millisecond" {
			ev.TimeMS = item.Tick
			if item.Tick%interval != 0 {
				return nil, bizerr.New(bizerr.SchemaConflict, path+".timeline.tick", "ms tick %d not aligned", item.Tick)
			}
			ev.FrameSeq = item.Tick / interval
		} else {
			ev.FrameSeq = item.Tick
			ev.TimeMS = item.Tick * interval
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

func resolveV3Window(path, mode string, credit, budget, sent *int64) (int64, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "", "absolute_remaining", "remaining":
		if credit == nil {
			return 0, bizerr.New(bizerr.FieldMapping, path+".creditBytes", "missing absolute remaining credit")
		}
		if budget != nil || sent != nil {
			return 0, bizerr.New(bizerr.SchemaConflict, path, "absolute credit conflicts with budget/sent fields")
		}
		if *credit < 0 {
			return 0, bizerr.New(bizerr.WindowNegative, path+".creditBytes", "negative credit")
		}
		return *credit, nil
	case "cumulative_sent", "sent":
		if credit != nil {
			return 0, bizerr.New(bizerr.SchemaConflict, path, "cumulative_sent mode conflicts with creditBytes")
		}
		if budget == nil || sent == nil {
			return 0, bizerr.New(bizerr.FieldMapping, path, "cumulative_sent requires budgetBytes and sentBytes")
		}
		if *budget < 0 || *sent < 0 || *sent > *budget {
			return 0, bizerr.New(bizerr.WindowNegative, path, "invalid budget/sent %d/%d", *budget, *sent)
		}
		return *budget - *sent, nil
	default:
		return 0, bizerr.New(bizerr.FieldMapping, path+".creditMode", "unknown window expression %q", mode)
	}
}

func mapOrphanRule(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "inherit", "inherit_parent_weight", "heir":
		return model.OrphanInherit
	default:
		return model.OrphanReparent
	}
}
