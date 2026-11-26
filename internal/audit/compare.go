package audit

import (
	"fmt"

	"acousticfield/internal/acoustic"
	"acousticfield/internal/checkpoint"
)

type ReportComparison struct {
	OK       bool       `json:"ok"`
	Dirs     []string   `json:"dirs"`
	First    *Mismatch  `json:"first,omitempty"`
	Findings []Mismatch `json:"findings,omitempty"`
}

// CompareReportDirs compares persisted report.json files without invoking recompute.
func CompareReportDirs(dirs ...string) (*ReportComparison, error) {
	if len(dirs) < 2 {
		return nil, fmt.Errorf("compare requires at least two directories")
	}
	reports := make([]*acoustic.Report, 0, len(dirs))
	for _, dir := range dirs {
		r, err := checkpoint.LoadReport(dir)
		if err != nil {
			return nil, err
		}
		reports = append(reports, r)
	}
	return CompareReports(reports, dirs), nil
}

func CompareReports(reports []*acoustic.Report, labels []string) *ReportComparison {
	cmp := &ReportComparison{OK: true, Dirs: labels}
	add := func(m Mismatch) {
		cmp.OK = false
		cmp.Findings = append(cmp.Findings, m)
		if cmp.First == nil {
			cp := m
			cmp.First = &cp
		}
	}
	base := reports[0]
	for ri := 1; ri < len(reports); ri++ {
		other := reports[ri]
		label := fmt.Sprintf("%s_vs_%s", labels[0], labels[ri])
		if stringsJoin(base.ChunkOrder) != stringsJoin(other.ChunkOrder) {
			add(Mismatch{Field: "chunk_order", Expected: base.ChunkOrder, Actual: other.ChunkOrder, Message: label})
			return cmp
		}
		baseSeq := commitIDs(base)
		otherSeq := commitIDs(other)
		if stringsJoin(baseSeq) != stringsJoin(otherSeq) {
			add(Mismatch{Field: "commit_sequence", Expected: baseSeq, Actual: otherSeq, Message: label})
		}
		baseChunks := map[string]acoustic.ChunkResult{}
		otherChunks := map[string]acoustic.ChunkResult{}
		for _, c := range base.Chunks {
			baseChunks[c.ChunkID] = c
		}
		for _, c := range other.Chunks {
			otherChunks[c.ChunkID] = c
		}
		for _, id := range base.ChunkOrder {
			bc := baseChunks[id]
			oc, ok := otherChunks[id]
			if !ok {
				add(Mismatch{ChunkID: id, BandID: -1, Field: "chunks", Message: "chunk missing in " + label})
				continue
			}
			compareBandArray(add, id, "arrival_energy_q", bc.ArrivalEnergyQ, oc.ArrivalEnergyQ, label)
			compareBandArray(add, id, "listener_arrival_q", bc.ListenerArrivalQ, oc.ListenerArrivalQ, label)
			compareBandArray(add, id, "inbound_q", bc.InboundQ, oc.InboundQ, label)
			compareBandArray(add, id, "retained_q", bc.RetainedQ, oc.RetainedQ, label)
			compareBandArray(add, id, "absorbed_q", bc.AbsorbedQ, oc.AbsorbedQ, label)
			if bc.PathDigest != oc.PathDigest {
				add(Mismatch{ChunkID: id, BandID: -1, Field: "path_digest", Expected: bc.PathDigest, Actual: oc.PathDigest, Message: label})
			}
			if bc.ContentHash != oc.ContentHash {
				add(Mismatch{ChunkID: id, BandID: -1, Field: "content_hash", Expected: bc.ContentHash, Actual: oc.ContentHash, Message: label})
			}
			if bc.CacheKey != oc.CacheKey {
				add(Mismatch{ChunkID: id, BandID: -1, Field: "cache_key", Expected: bc.CacheKey, Actual: oc.CacheKey, Message: label})
			}
			comparePaths(add, id, bc.Paths, oc.Paths, label)
		}
		for b := range base.Conservation.ResidualQ {
			var otherResidual int64
			if b < len(other.Conservation.ResidualQ) {
				otherResidual = other.Conservation.ResidualQ[b]
			}
			if base.Conservation.ResidualQ[b] != otherResidual {
				add(Mismatch{ChunkID: "LEVEL", BandID: b, Field: "conservation.residual_q", Expected: base.Conservation.ResidualQ[b], Actual: otherResidual, Message: label})
			}
		}
	}
	return cmp
}

func compareBandArray(add func(Mismatch), chunkID, field string, expected, actual []int64, label string) {
	n := len(expected)
	if len(actual) > n {
		n = len(actual)
	}
	for b := 0; b < n; b++ {
		var e, a int64
		if b < len(expected) {
			e = expected[b]
		}
		if b < len(actual) {
			a = actual[b]
		}
		if e != a {
			add(Mismatch{ChunkID: chunkID, BandID: b, Field: field, Expected: e, Actual: a, Message: label})
			return
		}
	}
}

func comparePaths(add func(Mismatch), chunkID string, expected, actual []acoustic.PathResult, label string) {
	if len(expected) != len(actual) {
		add(Mismatch{ChunkID: chunkID, BandID: -1, Field: "paths.length", Expected: len(expected), Actual: len(actual), Message: label})
		return
	}
	actualByKey := map[string]acoustic.PathResult{}
	for _, p := range actual {
		actualByKey[p.ListenerID+"\x00"+p.Key] = p
	}
	for _, ep := range expected {
		ap, ok := actualByKey[ep.ListenerID+"\x00"+ep.Key]
		if !ok {
			add(Mismatch{ChunkID: chunkID, BandID: -1, Field: "paths." + ep.Key, Message: "path missing " + label})
			continue
		}
		if ep.LengthQ != ap.LengthQ {
			add(Mismatch{ChunkID: chunkID, BandID: -1, Field: "paths." + ep.Key + ".length_q", Expected: ep.LengthQ, Actual: ap.LengthQ, Message: label})
		}
		compareBandArray(add, chunkID, "paths."+ep.Key+".band_energy_q", ep.BandEnergyQ, ap.BandEnergyQ, label)
	}
}

func commitIDs(r *acoustic.Report) []string {
	out := make([]string, 0, len(r.CommitSequence))
	for _, c := range r.CommitSequence {
		out = append(out, c.ChunkID)
	}
	return out
}

func stringsJoin(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += "|"
		}
		out += s
	}
	return out
}
