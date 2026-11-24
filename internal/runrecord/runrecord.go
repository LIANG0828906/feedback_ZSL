package runrecord

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"texlab/internal/apperr"
	"texlab/internal/engine"
	"texlab/internal/model"
)

type StageDigest struct {
	EntryID       string `json:"entry_id"`
	EntryIndex    int    `json:"entry_index"`
	Segment       int    `json:"segment"`
	ContentHash   string `json:"content_hash"`
	InputHash     string `json:"input_hash"`
	RegionHash    string `json:"region_hash"`
	CacheKey      string `json:"cache_key"`
	ParamHash     string `json:"param_hash"`
	CoverageHash  string `json:"coverage_hash"`
	Reused        bool   `json:"reused"`
	CacheHit      bool   `json:"cache_hit"`
	Invalidated   bool   `json:"invalidated"`
	InvalidReason string `json:"invalid_reason,omitempty"`
}

type EntrySummary struct {
	EntryID string        `json:"entry_id"`
	Index   int           `json:"entry_index"`
	Digest  string        `json:"digest"`
	Stages  []StageDigest `json:"stages"`
}

type FinalSegment struct {
	Segment  int    `json:"segment"`
	Depth    int    `json:"depth"`
	Channels int    `json:"channels"`
	Layout   string `json:"layout"`
	Alpha    string `json:"alpha"`
	Hash     string `json:"hash"`
}

type CacheStats struct {
	TotalCells       int `json:"total_cells"`
	ReusedCells      int `json:"reused_cells"`
	CacheHits        int `json:"cache_hits"`
	InvalidatedCells int `json:"invalidated_cells"`
}

type Record struct {
	Schema            string         `json:"schema"`
	PlanFingerprint   string         `json:"plan_fingerprint"`
	SourceFingerprint string         `json:"source_fingerprint"`
	ParamVersion      int            `json:"param_version"`
	AlgorithmVersion  string         `json:"algorithm_version"`
	EntrySummaries    []EntrySummary `json:"entry_summaries"`
	FinalSegments     []FinalSegment `json:"final_segments"`
	FinalDigest       string         `json:"final_digest"`
	CommitSequence    []int          `json:"commit_sequence"`
	Cache             CacheStats     `json:"cache"`
	Invalidations     []StageDigest  `json:"invalidations"`
}

func Build(res *engine.RunResult) Record {
	r := Record{
		Schema: model.RunSchema, PlanFingerprint: res.PlanFingerprint,
		SourceFingerprint: res.SourceFingerprint, ParamVersion: res.Revision,
		AlgorithmVersion: model.AlgorithmVersion, CommitSequence: append([]int(nil), res.CommitSequence...),
	}
	for _, e := range res.Plan.Entries {
		es := EntrySummary{EntryID: e.ID, Index: e.Index}
		if bySeg := res.StageIndex[e.ID]; bySeg != nil {
			var segs []int
			for seg := range bySeg {
				segs = append(segs, seg)
			}
			sort.Ints(segs)
			for _, seg := range segs {
				st := bySeg[seg]
				d := StageDigest{
					EntryID: st.EntryID, EntryIndex: st.EntryIndex, Segment: st.Segment,
					ContentHash: st.ContentHash, InputHash: st.InputHash, RegionHash: st.RegionHash,
					CacheKey: st.CacheKey, ParamHash: st.ParamHash, CoverageHash: st.CoverageHash,
					Reused: st.Reused, CacheHit: st.CacheHit, Invalidated: st.Invalidated,
					InvalidReason: st.InvalidReason,
				}
				es.Stages = append(es.Stages, d)
				if st.Invalidated {
					r.Invalidations = append(r.Invalidations, d)
				}
			}
		}
		es.Digest = EntryDigest(es.Stages)
		r.EntrySummaries = append(r.EntrySummaries, es)
	}
	for _, seg := range res.Segments {
		r.FinalSegments = append(r.FinalSegments, FinalSegment{
			Segment: seg.Segment.Index, Depth: seg.Final.Depth, Channels: seg.Final.Channels,
			Layout: seg.Final.Layout, Alpha: seg.Final.Alpha, Hash: seg.FinalHash,
		})
	}
	r.FinalDigest = FinalDigest(r.FinalSegments)
	r.Cache = CacheStats{
		TotalCells: len(res.Matrix), ReusedCells: res.ReusedCells,
		CacheHits: res.CacheHits, InvalidatedCells: res.InvalidatedCells,
	}
	return r
}

func EntryDigest(stages []StageDigest) string {
	parts := []string{"texlab-entry-summary-v1"}
	for _, s := range stages {
		parts = append(parts, fmt.Sprintf("%s|%d|%s", s.EntryID, s.Segment, s.ContentHash))
	}
	return model.HashParts(parts...)
}

func FinalDigest(segs []FinalSegment) string {
	parts := []string{"texlab-final-summary-v1"}
	for _, s := range segs {
		parts = append(parts, fmt.Sprintf("%d|%d|%d|%s|%s|%s", s.Segment, s.Depth, s.Channels, s.Layout, s.Alpha, s.Hash))
	}
	return model.HashParts(parts...)
}

func Write(path string, r Record) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "create run record directory", Path: path, Details: err.Error()}
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "write run record", Path: path, Details: err.Error()}
	}
	if err := os.Rename(tmp, path); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "replace run record", Path: path, Details: err.Error()}
	}
	return nil
}

func Read(path string) (Record, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Record{}, &apperr.Error{Code: apperr.EIO, Message: "read run record", Path: path, Details: err.Error()}
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return Record{}, &apperr.Error{Code: apperr.EPlan, Message: "decode run record", Field: "record", Details: err.Error()}
	}
	if r.Schema != model.RunSchema {
		return Record{}, apperr.At(apperr.EPlan, 0, "record.schema", "unsupported run record schema")
	}
	return r, nil
}
