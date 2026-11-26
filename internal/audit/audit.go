// Package audit is the independent review kernel. It must not import rebuild or engine.
package audit

import (
	"fmt"
	"sort"

	"acousticfield/internal/acoustic"
	"acousticfield/internal/checkpoint"
)

type Mismatch struct {
	ChunkID  string `json:"chunk_id,omitempty"`
	BandID   int    `json:"band_id"`
	Field    string `json:"field"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
	Message  string `json:"message,omitempty"`
}

type Result struct {
	OK               bool                        `json:"ok"`
	Dir              string                      `json:"dir"`
	AlgorithmVersion string                      `json:"algorithm_version"`
	ParamVersion     string                      `json:"param_version"`
	ChunkOrder       []string                    `json:"chunk_order"`
	Findings         []Mismatch                  `json:"findings,omitempty"`
	First            *Mismatch                   `json:"first,omitempty"`
	Conservation     acoustic.ConservationReport `json:"conservation"`
	TruthFingerprint string                      `json:"truth_fingerprint"`
}

type digestPath struct {
	Key             string
	ListenerID      string
	Sequence        []string
	Bounces         int
	LengthQ         int64
	SegmentLengthsQ []int64
	BandEnergyQ     []int64
	AbsorbedQ       []int64
}

type truthCore struct {
	Seed     int64
	BandIDs  []int
	Samples  []acoustic.SampleTruth
	Residual []int64
}

// AuditDir independently recomputes ledgers from persisted artifacts.
func AuditDir(dir string) (*Result, error) {
	snapshot, err := checkpoint.LoadSnapshot(dir)
	if err != nil {
		return nil, err
	}
	store, err := checkpoint.Open(dir)
	if err != nil {
		return nil, err
	}
	latest, _, err := store.LatestMatching(snapshot.Params.AlgorithmVersion, snapshot.Params.ParamVersion)
	if err != nil {
		return nil, err
	}
	truth, _ := store.LoadTruth()

	order := auditChunkOrder(&snapshot.Level)
	out := &Result{
		OK:               true,
		Dir:              dir,
		AlgorithmVersion: snapshot.Params.AlgorithmVersion,
		ParamVersion:     snapshot.Params.ParamVersion,
		ChunkOrder:       order,
		TruthFingerprint: snapshot.TruthFingerprint,
	}

	results := make([]acoustic.ChunkResult, 0, len(order))
	for _, id := range order {
		rec, ok := latest[id]
		if !ok {
			out.add(Mismatch{ChunkID: id, BandID: -1, Field: "checkpoint.committed_record", Message: "no version-matching committed chunk"})
			continue
		}
		r := rec.Result
		results = append(results, r)
		auditChunk(out, r)
	}

	inbound := map[string][]int64{}
	bandCount := len(snapshot.Level.BandIDs)
	for _, id := range order {
		inbound[id] = make([]int64, bandCount)
	}
	var lossTotal []int64 = make([]int64, bandCount)
	for _, r := range results {
		for _, t := range r.Transfers {
			for b := 0; b < bandCount; b++ {
				sent := get64(t.BandSentQ, b)
				delivered := get64(t.BandDeliveredQ, b)
				loss := get64(t.BandLossQ, b)
				if sent != delivered+loss {
					out.add(Mismatch{ChunkID: r.ChunkID, BandID: b, Field: "transfer." + t.ID + ".band_loss_q", Expected: sent - delivered, Actual: loss})
				}
				if b < len(t.BandDeliveredQ) && t.ToChunkID != "" {
					inbound[t.ToChunkID][b] += delivered
				}
				lossTotal[b] += loss
			}
		}
	}

	retainedTotal := make([]int64, bandCount)
	emittedTotal := make([]int64, bandCount)
	for _, source := range snapshot.Level.Sources {
		for b := 0; b < bandCount; b++ {
			emittedTotal[b] += get64(source.EmissionQ, b)
		}
	}

	byID := map[string]acoustic.ChunkResult{}
	for _, r := range results {
		byID[r.ChunkID] = r
		for b := 0; b < bandCount; b++ {
			wantInbound := inbound[r.ChunkID][b]
			if got := get64(r.InboundQ, b); got != wantInbound {
				out.add(Mismatch{ChunkID: r.ChunkID, BandID: b, Field: "inbound_q", Expected: wantInbound, Actual: got})
			}
			exterior := int64(0)
			if len(r.Transfers) == 0 {
				exterior = get64(r.OutboundSentQ, b)
			}
			wantRetained := get64(r.ListenerArrivalQ, b) + get64(r.AbsorbedQ, b) + wantInbound + exterior
			if got := get64(r.ArrivalEnergyQ, b); got != wantRetained {
				out.add(Mismatch{ChunkID: r.ChunkID, BandID: b, Field: "arrival_energy_q", Expected: wantRetained, Actual: got})
			}
			if got := get64(r.RetainedQ, b); got != wantRetained {
				out.add(Mismatch{ChunkID: r.ChunkID, BandID: b, Field: "retained_q", Expected: wantRetained, Actual: got})
			}
			retainedTotal[b] += gotOrWant(get64(r.RetainedQ, b), wantRetained)
		}
	}

	residual := make([]int64, bandCount)
	tol := snapshot.Params.ConservationToleranceQ
	violations := []acoustic.ConservationViolation{}
	for b := 0; b < bandCount; b++ {
		residual[b] = emittedTotal[b] - retainedTotal[b] - lossTotal[b]
		if acoustic.Abs64(residual[b]) > tol {
			out.add(Mismatch{ChunkID: "LEVEL", BandID: b, Field: "conservation.residual_q", Expected: 0, Actual: residual[b]})
			violations = append(violations, acoustic.ConservationViolation{BandID: b, SourceChunkID: "LEVEL", ResidualQ: residual[b], ToleranceQ: tol})
		}
	}
	out.Conservation = acoustic.ConservationReport{
		OK: len(violations) == 0, ToleranceQ: tol, BandIDs: snapshot.Level.BandIDs,
		EmittedQ: emittedTotal, RetainedQ: retainedTotal, TransferLossQ: lossTotal,
		ResidualQ: residual, Violations: violations,
	}

	if truth != nil {
		wantFP := acoustic.HashValue("truth:", truthCore{
			Seed: truth.Seed, BandIDs: truth.BandIDs, Samples: truth.Samples,
			Residual: truth.ConservationResidualQ,
		})
		if truth.Fingerprint != wantFP {
			out.add(Mismatch{Field: "truth.fingerprint", Expected: wantFP, Actual: truth.Fingerprint})
		}
		if snapshot.TruthFingerprint != truth.Fingerprint {
			out.add(Mismatch{Field: "snapshot.truth_fingerprint", Expected: truth.Fingerprint, Actual: snapshot.TruthFingerprint})
		}
		for b, want := range truth.ConservationResidualQ {
			if got := get64(out.Conservation.ResidualQ, b); got != want {
				out.add(Mismatch{BandID: b, Field: "truth.conservation_residual_q", Expected: want, Actual: got})
			}
		}
		auditTruth(out, order, byID, truth)
	}

	out.OK = len(out.Findings) == 0
	if len(out.Findings) > 0 {
		first := out.Findings[0]
		out.First = &first
	}
	return out, nil
}

func (r *Result) add(m Mismatch) {
	r.Findings = append(r.Findings, m)
	r.OK = false
	if r.First == nil {
		cp := m
		r.First = &cp
	}
}

func auditChunk(out *Result, r acoustic.ChunkResult) {
	digestInput := make([]digestPath, 0, len(r.Paths))
	listenerSum := map[string][]int64{}
	var aggregateListener []int64
	var aggregateAbsorbed []int64
	for _, p := range r.Paths {
		digestInput = append(digestInput, digestPath{
			Key: p.Key, ListenerID: p.ListenerID, Sequence: p.Sequence, Bounces: p.Bounces,
			LengthQ: p.LengthQ, SegmentLengthsQ: p.SegmentLengthsQ,
			BandEnergyQ: p.BandEnergyQ, AbsorbedQ: p.AbsorbedQ,
		})
		var segmentSum int64
		for _, l := range p.SegmentLengthsQ {
			segmentSum += l
		}
		if segmentSum != p.LengthQ {
			out.add(Mismatch{ChunkID: r.ChunkID, BandID: -1, Field: "paths." + p.Key + ".length_q", Expected: segmentSum, Actual: p.LengthQ})
		}
		if aggregateListener == nil {
			aggregateListener = make([]int64, len(p.BandEnergyQ))
			aggregateAbsorbed = make([]int64, len(p.AbsorbedQ))
		}
		if listenerSum[p.ListenerID] == nil {
			listenerSum[p.ListenerID] = make([]int64, len(p.BandEnergyQ))
		}
		for b := range p.BandEnergyQ {
			aggregateListener[b] += p.BandEnergyQ[b]
			listenerSum[p.ListenerID][b] += p.BandEnergyQ[b]
		}
		for b := range p.AbsorbedQ {
			aggregateAbsorbed[b] += p.AbsorbedQ[b]
		}
	}
	wantDigest := acoustic.HashValue("path-digest:", digestInput)
	if wantDigest != r.PathDigest {
		out.add(Mismatch{ChunkID: r.ChunkID, BandID: -1, Field: "path_digest", Expected: wantDigest, Actual: r.PathDigest})
	}
	for b := range aggregateListener {
		if got := get64(r.ListenerArrivalQ, b); got != aggregateListener[b] {
			out.add(Mismatch{ChunkID: r.ChunkID, BandID: b, Field: "listener_arrival_q", Expected: aggregateListener[b], Actual: got})
		}
		if got := get64(r.AbsorbedQ, b); got != aggregateAbsorbed[b] {
			out.add(Mismatch{ChunkID: r.ChunkID, BandID: b, Field: "absorbed_q", Expected: aggregateAbsorbed[b], Actual: got})
		}
	}
	for _, le := range r.ListenerEnergies {
		want := listenerSum[le.ListenerID]
		for b := range want {
			if got := get64(le.BandArrivalQ, b); got != want[b] {
				out.add(Mismatch{ChunkID: r.ChunkID, BandID: b, Field: "listener_energies." + le.ListenerID + ".band_arrival_q", Expected: want[b], Actual: got})
			}
		}
	}
}

func auditTruth(out *Result, order []string, byID map[string]acoustic.ChunkResult, truth *acoustic.Truth) {
	truthByChunk := map[string]acoustic.SampleTruth{}
	for _, sample := range truth.Samples {
		truthByChunk[sample.ChunkID] = sample
	}
	for _, chunkID := range order {
		sample, ok := truthByChunk[chunkID]
		if !ok {
			out.add(Mismatch{ChunkID: chunkID, BandID: -1, Field: "truth.sample", Message: "missing truth sample"})
			continue
		}
		r := byID[chunkID]
		pathByKey := map[string]acoustic.PathResult{}
		for _, p := range r.Paths {
			pathByKey[p.ListenerID+"\x00"+p.Key] = p
		}
		listenerEnergy := map[string][]int64{}
		for _, le := range r.ListenerEnergies {
			listenerEnergy[le.ListenerID] = le.BandArrivalQ
		}
		for _, wantPath := range sample.Paths {
			p, ok := pathByKey[wantPath.ListenerID+"\x00"+wantPath.PathKey]
			if !ok {
				out.add(Mismatch{ChunkID: chunkID, BandID: -1, Field: "truth.paths." + wantPath.PathKey, Message: "path missing"})
				continue
			}
			if p.LengthQ != wantPath.LengthQ {
				out.add(Mismatch{ChunkID: chunkID, BandID: -1, Field: "paths." + wantPath.PathKey + ".length_q", Expected: wantPath.LengthQ, Actual: p.LengthQ})
			}
			for b, want := range wantPath.BandArrivalQ {
				if got := get64(p.BandEnergyQ, b); got != want {
					out.add(Mismatch{ChunkID: chunkID, BandID: b, Field: "paths." + wantPath.PathKey + ".band_energy_q", Expected: want, Actual: got})
				}
			}
		}
		for b, want := range sample.ExpectedArrivalQ {
			got := int64(0)
			if energies := listenerEnergy[sample.ListenerID]; energies != nil {
				got = get64(energies, b)
			}
			if got != want {
				out.add(Mismatch{ChunkID: chunkID, BandID: b, Field: "expected_arrival_q", Expected: want, Actual: got})
			}
		}
	}
}

func auditChunkOrder(level *acoustic.Level) []string {
	chunks := append([]acoustic.Chunk(nil), level.Chunks...)
	sort.Slice(chunks, func(i, j int) bool {
		if chunks[i].Row != chunks[j].Row {
			return chunks[i].Row < chunks[j].Row
		}
		if chunks[i].Col != chunks[j].Col {
			return chunks[i].Col < chunks[j].Col
		}
		return chunks[i].ID < chunks[j].ID
	})
	ids := make([]string, 0, len(chunks))
	for _, c := range chunks {
		ids = append(ids, c.ID)
	}
	return ids
}

func get64(in []int64, i int) int64 {
	if i < 0 || i >= len(in) {
		return 0
	}
	return in[i]
}

func gotOrWant(got, want int64) int64 {
	// When a field mismatch is recorded, conservation should use persisted actual.
	return got
}

func (m Mismatch) String() string {
	return fmt.Sprintf("chunk=%s band=%d field=%s expected=%v actual=%v %s", m.ChunkID, m.BandID, m.Field, m.Expected, m.Actual, m.Message)
}
