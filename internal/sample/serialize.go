package sample

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/model"
)

func (b *Bundle) render() error {
	var err error
	if b.V1, err = json.MarshalIndent(toV1(b.Canonical), "", "  "); err != nil {
		return err
	}
	if b.V2, err = json.MarshalIndent(toV2(b.Canonical), "", "  "); err != nil {
		return err
	}
	if b.V3, err = json.MarshalIndent(toV3(b.Canonical), "", "  "); err != nil {
		return err
	}
	return nil
}

// Write emits all local deterministic inputs and truth under dir.
func (b *Bundle) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return bizerr.Wrap(bizerr.Internal, dir, err, "create sample directory")
	}
	files := map[string][]byte{
		"project-v1.json": b.V1,
		"project-v2.json": b.V2,
		"project-v3.json": b.V3,
	}
	canonical, err := json.MarshalIndent(b.Canonical, "", "  ")
	if err != nil {
		return err
	}
	files["canonical-project.json"] = canonical
	truth, err := json.MarshalIndent(b.Truth, "", "  ")
	if err != nil {
		return err
	}
	files["truth.json"] = truth
	cases, err := json.MarshalIndent(b.NumericalCases, "", "  ")
	if err != nil {
		return err
	}
	files["fixed-point-cases.json"] = cases
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			return bizerr.Wrap(bizerr.Internal, path, err, "write sample file")
		}
	}
	return nil
}

func toV1(p *model.Project) map[string]any {
	priorities := make([]map[string]any, 0, len(p.Streams))
	streams := make([]map[string]any, 0, len(p.Streams))
	for _, s := range p.Streams {
		priorities = append(priorities, map[string]any{
			"stream_id":        s.ID,
			"parent_stream_id": s.ParentID,
			"weight_permille":  s.WeightPermyriad / 10,
			"orphan_policy":    s.OrphanPolicy,
		})
		streams = append(streams, map[string]any{
			"stream_id":                      s.ID,
			"name":                           s.Name,
			"initial_window_remaining_bytes": s.InitialWindowBytes,
			"max_window_bytes":               s.MaxWindowBytes,
			"max_frame_size_bytes":           s.MaxFrameBytes,
			"body_segment_bytes":             s.BodySegments,
		})
	}
	frames := make([]map[string]any, 0, len(p.Events))
	for _, e := range p.Events {
		frames = append(frames, map[string]any{
			"frame_no":        e.FrameSeq,
			"kind":            strings.ToLower(e.Type),
			"stream_id":       e.StreamID,
			"payload_bytes":   e.LengthBytes,
			"increment_bytes": e.IncrementBytes,
			"segment_index":   e.SegmentIndex,
			"fin":             e.Fin,
		})
	}
	return map[string]any{
		"schema":            "multiplex.frameplan/v1+seq-flat-permille-abs",
		"project_id":        p.ProjectID,
		"seed":              p.Seed,
		"frame_interval_ms": p.FrameIntervalMS,
		"frame_watermark":   p.FrameWatermark,
		"orphan_policy":     p.OrphanPolicy,
		"connection": map[string]any{
			"connection_id":        p.Connection.ID,
			"initial_window_bytes": p.Connection.InitialWindowBytes,
			"max_window_bytes":     p.Connection.MaxWindowBytes,
		},
		"streams":    streams,
		"priorities": priorities,
		"frames":     frames,
	}
}

func toV2(p *model.Project) map[string]any {
	streams := make([]map[string]any, 0, len(p.Streams))
	nodes := map[int]map[string]any{}
	for _, s := range p.Streams {
		initialKiB := s.InitialWindowBytes / 1024
		sentKiB := int64(1)
		streams = append(streams, map[string]any{
			"streamId":         s.ID,
			"name":             s.Name,
			"windowBudgetKiB":  initialKiB + sentKiB,
			"alreadySentKiB":   sentKiB,
			"windowCeilingKiB": s.MaxWindowBytes / 1024,
			"maxFrameKiB":      s.MaxFrameBytes / 1024,
			"bodySegmentsKiB":  divideSegments(s.BodySegments, 1024),
		})
		nodes[s.ID] = map[string]any{
			"streamId":          s.ID,
			"weightBasisPoints": s.WeightPermyriad,
			"orphanPolicy":      s.OrphanPolicy,
			"children":          []map[string]any{},
		}
	}
	var rootChildren []map[string]any
	for _, s := range p.Streams {
		node := nodes[s.ID]
		if s.ParentID == 0 {
			rootChildren = append(rootChildren, node)
		} else {
			parent := nodes[s.ParentID]
			children := parent["children"].([]map[string]any)
			parent["children"] = append(children, node)
		}
	}
	frames := make([]map[string]any, 0, len(p.Events))
	for _, e := range p.Events {
		frames = append(frames, map[string]any{
			"atMs":         e.TimeMS,
			"frameType":    e.Type,
			"streamId":     e.StreamID,
			"payloadKiB":   e.LengthBytes / 1024,
			"updateKiB":    e.IncrementBytes / 1024,
			"segmentIndex": e.SegmentIndex,
			"endStream":    e.Fin,
		})
	}
	return map[string]any{
		"schema":       "gateway.frameplan/v2+ms-tree-bps-sent-kib",
		"projectId":    p.ProjectID,
		"seed":         p.Seed,
		"tickMs":       p.FrameIntervalMS,
		"waterMarkMs":  p.FrameWatermark * p.FrameIntervalMS,
		"orphanPolicy": p.OrphanPolicy,
		"connection": map[string]any{
			"connectionId":       p.Connection.ID,
			"remainingWindowKiB": p.Connection.InitialWindowBytes / 1024,
			"windowCeilingKiB":   p.Connection.MaxWindowBytes / 1024,
		},
		"streams":      streams,
		"priorityTree": map[string]any{"streamId": 0, "weightBasisPoints": 0, "children": rootChildren},
		"frames":       frames,
	}
}

func toV3(p *model.Project) map[string]any {
	flows := make([]map[string]any, 0, len(p.Streams))
	for _, s := range p.Streams {
		flows = append(flows, map[string]any{
			"fid":             s.ID,
			"name":            s.Name,
			"upstreamFid":     s.ParentID,
			"shareMyriad":     s.WeightPermyriad,
			"creditMode":      "absolute_remaining",
			"creditBytes":     s.InitialWindowBytes,
			"ceilingBytes":    s.MaxWindowBytes,
			"maxPayloadBytes": s.MaxFrameBytes,
			"bodyPlan":        s.BodySegments,
			"orphanRule":      s.OrphanPolicy,
		})
	}
	timeline := make([]map[string]any, 0, len(p.Events))
	for _, e := range p.Events {
		timeline = append(timeline, map[string]any{
			"tick":         e.FrameSeq,
			"op":           e.Type,
			"fid":          e.StreamID,
			"payloadBytes": e.LengthBytes,
			"updateBytes":  e.IncrementBytes,
			"shareMyriad":  e.WeightPermyriad,
			"segment":      e.SegmentIndex,
			"finish":       e.Fin,
		})
	}
	return map[string]any{
		"apiVersion":    "multiplex.gateway/v3.0+2024-11",
		"engineeringId": p.ProjectID,
		"randomSeed":    p.Seed,
		"clock":         map[string]any{"basis": "frame_number", "tickMs": p.FrameIntervalMS},
		"orphanRule":    p.OrphanPolicy,
		"connection": map[string]any{
			"cid":          p.Connection.ID,
			"creditMode":   "absolute_remaining",
			"creditBytes":  p.Connection.InitialWindowBytes,
			"ceilingBytes": p.Connection.MaxWindowBytes,
		},
		"flows":    flows,
		"timeline": timeline,
	}
}

func divideSegments(in []int64, div int64) []int64 {
	out := make([]int64, len(in))
	for i, v := range in {
		if v%div != 0 {
			panic(fmt.Sprintf("sample segment %d is not divisible by %d", v, div))
		}
		out[i] = v / div
	}
	return out
}
