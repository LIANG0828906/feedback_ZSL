package schema

import (
	"encoding/json"
	"sort"

	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/model"
)

type rawV2Node struct {
	StreamID          int          `json:"streamId"`
	WeightBasisPoints int64        `json:"weightBasisPoints"`
	OrphanPolicy      string       `json:"orphanPolicy"`
	Children          []*rawV2Node `json:"children"`
}

type rawV2 struct {
	Schema       string `json:"schema"`
	ProjectID    string `json:"projectId"`
	Seed         int64  `json:"seed"`
	TickMS       int64  `json:"tickMs"`
	WaterMarkMS  *int64 `json:"waterMarkMs"`
	OrphanPolicy string `json:"orphanPolicy"`
	Connection   struct {
		ID                 string `json:"connectionId"`
		RemainingWindowKiB *int64 `json:"remainingWindowKiB"`
		WindowBudgetKiB    *int64 `json:"windowBudgetKiB"`
		AlreadySentKiB     *int64 `json:"alreadySentKiB"`
		WindowCeilingKiB   int64  `json:"windowCeilingKiB"`
	} `json:"connection"`
	Streams []struct {
		StreamID           int     `json:"streamId"`
		Name               string  `json:"name"`
		RemainingWindowKiB *int64  `json:"remainingWindowKiB"`
		WindowBudgetKiB    *int64  `json:"windowBudgetKiB"`
		AlreadySentKiB     *int64  `json:"alreadySentKiB"`
		WindowCeilingKiB   int64   `json:"windowCeilingKiB"`
		MaxFrameKiB        int64   `json:"maxFrameKiB"`
		BodySegmentsKiB    []int64 `json:"bodySegmentsKiB"`
	} `json:"streams"`
	PriorityTree *rawV2Node      `json:"priorityTree"`
	Priorities   json.RawMessage `json:"priorities"`
	Frames       []struct {
		AtMS         int64  `json:"atMs"`
		FrameType    string `json:"frameType"`
		StreamID     int    `json:"streamId"`
		PayloadKiB   int64  `json:"payloadKiB"`
		UpdateKiB    int64  `json:"updateKiB"`
		SegmentIndex int    `json:"segmentIndex"`
		EndStream    bool   `json:"endStream"`
	} `json:"frames"`
}

func loadV2(path string, data []byte) (*model.Project, error) {
	var raw rawV2
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, bizerr.Wrap(bizerr.SchemaParse, path, err, "v2 JSON decode failed")
	}
	if len(raw.Priorities) > 0 && string(raw.Priorities) != "null" {
		return nil, bizerr.New(bizerr.SchemaConflict, path+".priorityTree",
			"v2 document contains nested tree and flat priorities")
	}
	if raw.PriorityTree == nil {
		return nil, bizerr.New(bizerr.FieldMapping, path+".priorityTree", "v2 requires nested priority tree")
	}
	interval := raw.TickMS
	if interval <= 0 {
		interval = 10
	}
	nodes := map[int]rawV2Node{}
	parents := map[int]int{}
	if raw.PriorityTree.StreamID == 0 {
		for _, child := range raw.PriorityTree.Children {
			if err := flattenV2Tree(child, 0, nodes, parents); err != nil {
				return nil, err
			}
		}
	} else {
		if err := flattenV2Tree(raw.PriorityTree, 0, nodes, parents); err != nil {
			return nil, err
		}
	}

	connWindow, err := resolveV2Window(
		path+".connection",
		raw.Connection.RemainingWindowKiB,
		raw.Connection.WindowBudgetKiB,
		raw.Connection.AlreadySentKiB,
	)
	if err != nil {
		return nil, err
	}
	p := &model.Project{
		SchemaVersion:     model.SchemaV2,
		SourceVersion:     raw.Schema,
		ProjectID:         raw.ProjectID,
		Seed:              raw.Seed,
		FixedPointBits:    16,
		WeightScale:       "permyriad-direct-q16",
		TimeBase:          model.TimeMilliseconds,
		FrameIntervalMS:   interval,
		OrphanPolicy:      defaultString(raw.OrphanPolicy, model.OrphanReparent),
		WatermarkExplicit: raw.WaterMarkMS != nil,
		Connection: model.ConnectionSpec{
			ID:                 raw.Connection.ID,
			InitialWindowBytes: connWindow * KiB,
			MaxWindowBytes:     raw.Connection.WindowCeilingKiB * KiB,
		},
	}
	if raw.WaterMarkMS != nil {
		if *raw.WaterMarkMS%interval != 0 {
			return nil, bizerr.New(bizerr.SchemaConflict, path+".waterMarkMs",
				"watermark %d is not aligned to tick %d", *raw.WaterMarkMS, interval)
		}
		p.FrameWatermark = *raw.WaterMarkMS / interval
	}

	for _, s := range raw.Streams {
		node, ok := nodes[s.StreamID]
		if !ok {
			return nil, bizerr.New(bizerr.FieldMapping, path+".priorityTree", "stream %d missing tree node", s.StreamID)
		}
		window, err := resolveV2Window(path+".streams", s.RemainingWindowKiB, s.WindowBudgetKiB, s.AlreadySentKiB)
		if err != nil {
			return nil, err
		}
		segments := make([]int64, 0, len(s.BodySegmentsKiB))
		for _, kib := range s.BodySegmentsKiB {
			segments = append(segments, kib*KiB)
		}
		p.Streams = append(p.Streams, model.StreamSpec{
			ID:                 s.StreamID,
			Name:               s.Name,
			ParentID:           parents[s.StreamID],
			WeightPermyriad:    node.WeightBasisPoints,
			InitialWindowBytes: window * KiB,
			MaxWindowBytes:     s.WindowCeilingKiB * KiB,
			MaxFrameBytes:      s.MaxFrameKiB * KiB,
			BodySegments:       segments,
			OrphanPolicy:       defaultString(node.OrphanPolicy, p.OrphanPolicy),
		})
	}
	sort.Stable(sortById(p.Streams))
	for _, f := range raw.Frames {
		if f.AtMS <= 0 || f.AtMS%interval != 0 {
			return nil, bizerr.New(bizerr.SchemaConflict, path+".frames.atMs",
				"frame timestamp %d is not a positive multiple of %d", f.AtMS, interval)
		}
		p.Events = append(p.Events, model.Event{
			FrameSeq:       f.AtMS / interval,
			TimeMS:         f.AtMS,
			Type:           normalizeKind(f.FrameType),
			StreamID:       f.StreamID,
			LengthBytes:    f.PayloadKiB * KiB,
			IncrementBytes: f.UpdateKiB * KiB,
			SegmentIndex:   f.SegmentIndex,
			Fin:            f.EndStream,
		})
	}
	sort.SliceStable(p.Events, func(i, j int) bool { return p.Events[i].TimeMS < p.Events[j].TimeMS })
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

func flattenV2Tree(n *rawV2Node, parent int, out map[int]rawV2Node, parents map[int]int) error {
	if n == nil {
		return nil
	}
	if n.StreamID <= 0 {
		return bizerr.New(bizerr.FieldMapping, "priorityTree.streamId", "tree node has no positive stream id")
	}
	if _, exists := out[n.StreamID]; exists {
		return bizerr.New(bizerr.SchemaConflict, "priorityTree", "stream %d appears more than once", n.StreamID)
	}
	clone := *n
	out[n.StreamID] = clone
	parents[n.StreamID] = parent
	for _, child := range n.Children {
		if err := flattenV2Tree(child, n.StreamID, out, parents); err != nil {
			return err
		}
	}
	return nil
}

func resolveV2Window(path string, remaining, budget, sent *int64) (int64, error) {
	if remaining != nil && (budget != nil || sent != nil) {
		return 0, bizerr.New(bizerr.SchemaConflict, path,
			"absolute remaining window conflicts with budget/sent cumulative expression")
	}
	if remaining != nil {
		if *remaining < 0 {
			return 0, bizerr.New(bizerr.WindowNegative, path+".remainingWindowKiB", "negative remaining window")
		}
		return *remaining, nil
	}
	if budget == nil || sent == nil {
		return 0, bizerr.New(bizerr.FieldMapping, path,
			"v2 window requires either remainingWindowKiB or windowBudgetKiB+alreadySentKiB")
	}
	if *budget < 0 || *sent < 0 || *sent > *budget {
		return 0, bizerr.New(bizerr.WindowNegative, path,
			"invalid cumulative window budget=%d sent=%d", *budget, *sent)
	}
	return *budget - *sent, nil
}

type idSorter []model.StreamSpec

func sortById(s []model.StreamSpec) idSorter { return idSorter(s) }
func (x idSorter) Len() int                  { return len(x) }
func (x idSorter) Less(i, j int) bool        { return x[i].ID < x[j].ID }
func (x idSorter) Swap(i, j int)             { x[i], x[j] = x[j], x[i] }
