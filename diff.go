package maskqc

import (
	"fmt"
	"reflect"
)

// EquivalenceReport summarizes the realtime/recovery/full comparison.
type EquivalenceReport struct {
	Consistent bool     `json:"consistent"`
	Paths      []string `json:"paths"`
	Diff       *Error   `json:"diff,omitempty"`
}

// VerifyPipelineEquivalence compares chunks, pixels, topology and commit order.
func VerifyPipelineEquivalence(results ...*PipelineResult) *EquivalenceReport {
	report := &EquivalenceReport{Consistent: true, Paths: []string{}}
	for _, r := range results {
		if r == nil {
			report.Consistent = false
			report.Diff = &Error{Code: CodeDiff, Field: "result", Message: "nil pipeline result"}
			return report
		}
		report.Paths = append(report.Paths, r.Path)
	}
	if len(results) < 2 {
		return report
	}
	base := results[0]
	for i := 1; i < len(results); i++ {
		if d := compareTwo(base, results[i]); d != nil {
			report.Consistent = false
			report.Diff = d
			return report
		}
	}
	return report
}

func compareTwo(a, b *PipelineResult) *Error {
	if len(a.Chunks) != len(b.Chunks) {
		return diffError(ChunkCoord{}, "chunks.length", map[string]any{"a": len(a.Chunks), "b": len(b.Chunks)})
	}
	for i := range a.Chunks {
		ca, cb := a.Chunks[i], b.Chunks[i]
		if ca.Coord != cb.Coord {
			return diffError(ca.Coord, "coord", map[string]any{"index": i, "a": ca.Coord, "b": cb.Coord})
		}
		coord := ca.Coord
		checks := []struct {
			field string
			av    any
			bv    any
		}{
			{"bounds", ca.Bounds, cb.Bounds},
			{"input_hash", ca.InputHash, cb.InputHash},
			{"order_key", ca.OrderKey, cb.OrderKey},
			{"params_key", ca.ParamsKey, cb.ParamsKey},
			{"cache_key", ca.CacheKey, cb.CacheKey},
			{"output_hash", ca.OutputHash, cb.OutputHash},
		}
		for _, check := range checks {
			if !reflect.DeepEqual(check.av, check.bv) {
				return diffError(coord, check.field, map[string]any{"a": check.av, "b": check.bv})
			}
		}
		if string(ca.Alpha) != string(cb.Alpha) {
			pixel := firstDifferentPixel(ca.Alpha, cb.Alpha)
			return diffError(coord, "alpha", map[string]any{
				"first_pixel_index": pixel,
				"a_value":           alphaAt(ca.Alpha, pixel),
				"b_value":           alphaAt(cb.Alpha, pixel),
			})
		}
	}
	if !reflect.DeepEqual(a.Topology, b.Topology) {
		domain := firstTopologyDifference(a.Topology, b.Topology)
		return diffError(ChunkCoord{}, "topology", map[string]any{"domain": domain, "a": a.Topology, "b": b.Topology})
	}
	if d := compareCommits(a.StrokeCommits, b.StrokeCommits); d != nil {
		return d
	}
	return nil
}

func compareCommits(a, b []ComponentRecord) *Error {
	if len(a) != len(b) {
		return diffError(ChunkCoord{}, "stroke_commits.length", map[string]any{"a": len(a), "b": len(b)})
	}
	for i := range a {
		ca, cb := a[i], b[i]
		field := ""
		switch {
		case ca.OrderIndex != cb.OrderIndex:
			field = "stroke_commits.order_index"
		case ca.StrokeSeq != cb.StrokeSeq:
			field = "stroke_commits.stroke_seq"
		case ca.Chunk != cb.Chunk:
			field = "stroke_commits.chunk"
		case ca.ContentHash != cb.ContentHash:
			field = "stroke_commits.content_hash"
		case ca.CacheKey != cb.CacheKey:
			field = "stroke_commits.cache_key"
		case ca.OrderVersion != cb.OrderVersion:
			field = "stroke_commits.order_version"
		case ca.AlgorithmVersion != cb.AlgorithmVersion:
			field = "stroke_commits.algorithm_version"
		}
		if field != "" {
			return diffError(ca.Chunk, field, map[string]any{"index": i, "a": ca, "b": cb})
		}
	}
	return nil
}

func firstDifferentPixel(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func alphaAt(a []byte, idx int) any {
	if idx < 0 || idx >= len(a) {
		return nil
	}
	return a[idx]
}

func firstTopologyDifference(a, b []DomainMetric) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if !reflect.DeepEqual(a[i], b[i]) {
			return i + 1
		}
	}
	return n + 1
}

func diffError(coord ChunkCoord, field string, details map[string]any) *Error {
	if details == nil {
		details = map[string]any{}
	}
	details["chunk_x"] = coord.X
	details["chunk_y"] = coord.Y
	details["field_name"] = field
	return &Error{
		Code:    CodeDiff,
		Message: fmt.Sprintf("path outputs differ at chunk %s field %s", coord, field),
		Field:   field,
		Path:    fmt.Sprintf("/chunks/%d/%s", coord.Y*100+coord.X, field),
		Details: details,
	}
}
