// Package schema loads the three historical/input frame engineering formats
// into one canonical model.Project.
package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/fixedpoint"
	"multiplexgw/internal/model"
)

const KiB = int64(1024)

// LoadFile reads and loads a frame engineering document.
func LoadFile(path string) (*model.Project, error) {
	b, err := readFile(path)
	if err != nil {
		return nil, bizerr.Wrap(bizerr.NotFound, path, err, "read project file")
	}
	return LoadBytes(path, b)
}

// LoadBytes dispatches by the version marker without trusting file names.
func LoadBytes(path string, data []byte) (*model.Project, error) {
	var head struct {
		Schema     string `json:"schema"`
		APIVersion string `json:"apiVersion"`
		Spec       struct {
			APIVersion string `json:"apiVersion"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, bizerr.Wrap(bizerr.SchemaParse, path, err, "invalid JSON document")
	}
	marker := strings.ToLower(strings.TrimSpace(firstNonEmpty(head.Schema, head.APIVersion, head.Spec.APIVersion)))
	switch {
	case strings.Contains(marker, "v1") || strings.Contains(marker, "seq-flat"):
		return loadV1(path, data)
	case strings.Contains(marker, "v2") || strings.Contains(marker, "ms-tree"):
		return loadV2(path, data)
	case strings.Contains(marker, "v3") || strings.Contains(marker, "2024-11"):
		return loadV3(path, data)
	default:
		return nil, bizerr.New(bizerr.SchemaUnsupported, path, "unsupported schema marker %q", marker)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func decodeStrict(path string, data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		// Strict decoding is intentionally not used for the evolving v3 shape;
		// callers that use it get a stable parse/mapping error.
		return bizerr.Wrap(bizerr.SchemaParse, path, err, "decode frame engineering")
	}
	return nil
}

// Validate checks canonical cross-field constraints.
func Validate(p *model.Project) error {
	if p == nil {
		return bizerr.New(bizerr.FieldMapping, "$", "nil project")
	}
	if p.Seed == 0 {
		// Seed zero is still deterministic, but generated samples use non-zero.
	}
	if p.Connection.InitialWindowBytes < 0 {
		return bizerr.New(bizerr.WindowNegative, "connection.initial_window_bytes",
			"connection window %d is negative", p.Connection.InitialWindowBytes)
	}
	if p.Connection.MaxWindowBytes <= 0 {
		return bizerr.New(bizerr.FieldMapping, "connection.max_window_bytes", "missing positive window cap")
	}
	if p.Connection.InitialWindowBytes > p.Connection.MaxWindowBytes {
		return bizerr.New(bizerr.WindowOverCap, "connection.initial_window_bytes",
			"initial window %d exceeds cap %d", p.Connection.InitialWindowBytes, p.Connection.MaxWindowBytes)
	}
	ids := map[int]bool{0: true}
	for i := range p.Streams {
		s := &p.Streams[i]
		path := fmt.Sprintf("streams[%d](id=%d)", i, s.ID)
		if s.ID <= 0 {
			return bizerr.New(bizerr.FieldMapping, path+".id", "stream id must be positive")
		}
		if ids[s.ID] {
			return bizerr.New(bizerr.SchemaConflict, path+".id", "duplicate stream id %d", s.ID)
		}
		ids[s.ID] = true
		if s.WeightPermyriad < 0 || s.WeightPermyriad > fixedpoint.MaxPermyriad {
			return bizerr.New(bizerr.WeightRange, path+".weight",
				"weight %d outside [1,%d] permyriad", s.WeightPermyriad, fixedpoint.MaxPermyriad).
				WithContext("stream_id", s.ID)
		}
		if s.WeightPermyriad == 0 {
			return bizerr.New(bizerr.WeightZero, path+".weight", "stream %d has zero scheduling weight", s.ID).
				WithContext("stream_id", s.ID)
		}
		if s.InitialWindowBytes < 0 {
			return bizerr.New(bizerr.WindowNegative, path+".initial_window_bytes",
				"stream %d window %d is negative", s.ID, s.InitialWindowBytes).
				WithContext("stream_id", s.ID)
		}
		if s.MaxWindowBytes <= 0 {
			s.MaxWindowBytes = s.InitialWindowBytes
		}
		if s.InitialWindowBytes > s.MaxWindowBytes {
			return bizerr.New(bizerr.WindowOverCap, path+".max_window_bytes",
				"stream %d initial window %d exceeds cap %d", s.ID, s.InitialWindowBytes, s.MaxWindowBytes).
				WithContext("stream_id", s.ID)
		}
		if s.MaxFrameBytes <= 0 {
			return bizerr.New(bizerr.FieldMapping, path+".max_frame_bytes", "max frame must be positive").
				WithContext("stream_id", s.ID)
		}
	}
	for i := range p.Streams {
		if err := checkParent(p, p.Streams[i].ID, p.Streams[i].ParentID, map[int]bool{}); err != nil {
			return err
		}
	}
	seenEventIndex := map[int]bool{}
	for i := range p.Events {
		e := &p.Events[i]
		if seenEventIndex[e.Index] {
			return bizerr.New(bizerr.SchemaConflict, fmt.Sprintf("events[%d].index", i), "duplicate event index %d", e.Index)
		}
		seenEventIndex[e.Index] = true
		switch e.Type {
		case model.EventHeaders, model.EventData, model.EventRSTStream, model.EventPriority:
			if e.StreamID <= 0 || !ids[e.StreamID] {
				return bizerr.New(bizerr.FieldMapping, fmt.Sprintf("events[%d].stream_id", i),
					"event %s references unknown stream %d", e.Type, e.StreamID)
			}
		case model.EventWindowUpdate:
			if e.StreamID < 0 || (e.StreamID != 0 && !ids[e.StreamID]) {
				return bizerr.New(bizerr.FieldMapping, fmt.Sprintf("events[%d].stream_id", i),
					"WINDOW_UPDATE references unknown stream %d", e.StreamID)
			}
			if e.IncrementBytes < 0 {
				return bizerr.New(bizerr.WindowNegative, fmt.Sprintf("events[%d].increment_bytes", i),
					"WINDOW_UPDATE increment %d is negative", e.IncrementBytes).
					WithContext("event_index", e.Index).WithContext("stream_id", e.StreamID)
			}
		default:
			return bizerr.New(bizerr.FieldMapping, fmt.Sprintf("events[%d].type", i), "unknown event type %q", e.Type)
		}
		if e.Type == model.EventData && e.LengthBytes < 0 {
			return bizerr.New(bizerr.FieldMapping, fmt.Sprintf("events[%d].length_bytes", i), "negative DATA length")
		}
	}
	return nil
}

func checkParent(p *model.Project, start, parent int, path map[int]bool) error {
	if parent == 0 {
		return nil
	}
	if parent == start || path[parent] {
		cycle := []int{start}
		for id := range path {
			cycle = append(cycle, id)
		}
		sort.Ints(cycle)
		return bizerr.New(bizerr.DependencyCycle, "streams.dependencies",
			"dependency cycle detected near stream %d", start).
			WithContext("stream_id", start).WithContext("cycle_hint", cycle)
	}
	found := false
	var next int
	for _, s := range p.Streams {
		if s.ID == parent {
			found = true
			next = s.ParentID
			break
		}
	}
	if !found {
		return bizerr.New(bizerr.FieldMapping, "streams.parent_id", "stream %d references missing parent %d", start, parent).
			WithContext("stream_id", start).WithContext("parent_id", parent)
	}
	path[parent] = true
	return checkParent(p, start, next, path)
}

// CompareCanonical returns human-readable differences relevant to acceptance.
func CompareCanonical(a, b *model.Project) []string {
	var diffs []string
	if a.ProjectID != b.ProjectID {
		diffs = append(diffs, fmt.Sprintf("project_id %q != %q", a.ProjectID, b.ProjectID))
	}
	if len(a.Streams) != len(b.Streams) {
		diffs = append(diffs, fmt.Sprintf("stream count %d != %d", len(a.Streams), len(b.Streams)))
	}
	am := map[int]model.StreamSpec{}
	bm := map[int]model.StreamSpec{}
	for _, s := range a.Streams {
		am[s.ID] = s
	}
	for _, s := range b.Streams {
		bm[s.ID] = s
	}
	ids := map[int]bool{}
	for id := range am {
		ids[id] = true
	}
	for id := range bm {
		ids[id] = true
	}
	var sortedIDs []int
	for id := range ids {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Ints(sortedIDs)
	for _, id := range sortedIDs {
		x, ok1 := am[id]
		y, ok2 := bm[id]
		if !ok1 || !ok2 {
			diffs = append(diffs, fmt.Sprintf("stream %d presence %v/%v", id, ok1, ok2))
			continue
		}
		if x.ParentID != y.ParentID {
			diffs = append(diffs, fmt.Sprintf("stream %d parent %d != %d", id, x.ParentID, y.ParentID))
		}
		if x.WeightPermyriad != y.WeightPermyriad {
			diffs = append(diffs, fmt.Sprintf("stream %d weight %d != %d", id, x.WeightPermyriad, y.WeightPermyriad))
		}
		if x.InitialWindowBytes != y.InitialWindowBytes {
			diffs = append(diffs, fmt.Sprintf("stream %d initial window %d != %d", id, x.InitialWindowBytes, y.InitialWindowBytes))
		}
		if x.MaxFrameBytes != y.MaxFrameBytes {
			diffs = append(diffs, fmt.Sprintf("stream %d max frame %d != %d", id, x.MaxFrameBytes, y.MaxFrameBytes))
		}
		if len(x.BodySegments) != len(y.BodySegments) {
			diffs = append(diffs, fmt.Sprintf("stream %d segments len %d != %d", id, len(x.BodySegments), len(y.BodySegments)))
		} else {
			for j := range x.BodySegments {
				if x.BodySegments[j] != y.BodySegments[j] {
					diffs = append(diffs, fmt.Sprintf("stream %d segment %d %d != %d", id, j, x.BodySegments[j], y.BodySegments[j]))
				}
			}
		}
	}
	if len(a.Events) != len(b.Events) {
		diffs = append(diffs, fmt.Sprintf("event count %d != %d", len(a.Events), len(b.Events)))
		return diffs
	}
	for i := range a.Events {
		x, y := a.Events[i], b.Events[i]
		if x.Type != y.Type || x.StreamID != y.StreamID || x.LengthBytes != y.LengthBytes || x.IncrementBytes != y.IncrementBytes {
			diffs = append(diffs, fmt.Sprintf("event[%d] (%s/%d/%d/%d) != (%s/%d/%d/%d)",
				i, x.Type, x.StreamID, x.LengthBytes, x.IncrementBytes,
				y.Type, y.StreamID, y.LengthBytes, y.IncrementBytes))
		}
		if x.FrameSeq != y.FrameSeq || x.TimeMS != y.TimeMS {
			diffs = append(diffs, fmt.Sprintf("event[%d] time (%d/%d) != (%d/%d)", i, x.FrameSeq, x.TimeMS, y.FrameSeq, y.TimeMS))
		}
	}
	if a.Connection.InitialWindowBytes != b.Connection.InitialWindowBytes {
		diffs = append(diffs, fmt.Sprintf("connection window %d != %d", a.Connection.InitialWindowBytes, b.Connection.InitialWindowBytes))
	}
	return diffs
}

func readFile(path string) ([]byte, error) {
	// isolated as a seam for tests
	return osReadFile(path)
}
