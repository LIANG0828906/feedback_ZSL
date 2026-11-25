package acousticfield

import (
	"context"
	"fmt"
	"sort"
)

type VerifyReport struct {
	OK                 bool               `json:"ok"`
	SnapshotID         string             `json:"snapshot_id"`
	TruthFingerprint   string             `json:"truth_fingerprint"`
	FieldFingerprint   string             `json:"field_fingerprint"`
	Chunks             int                `json:"chunks"`
	CommitSequence     []int              `json:"commit_sequence"`
	CommitSequenceHash string             `json:"commit_sequence_hash"`
	Conservation       ConservationReport `json:"conservation"`
}

// VerifySnapshot is the independent verifier. It reads only persisted chunk
// results, their boundary-transfer records, and the supplied analytic truth.
// It never calls SolveChunk, the wave-propagation planner, or another solver helper.
func VerifySnapshot(ctx context.Context, store *Store, snapshotID string, truth Truth, options Options) (*VerifyReport, error) {
	options = options.Normalized()
	meta, err := store.SnapshotMeta(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	if meta.TruthFingerprint != truth.Fingerprint {
		return nil, diffError(&Diff{ChunkID: -1, Field: "truth_fingerprint", Expected: truth.Fingerprint, Actual: meta.TruthFingerprint})
	}
	if !meta.Complete {
		return nil, diffError(&Diff{ChunkID: -1, Field: "snapshot_complete", Expected: true, Actual: false})
	}
	results, err := store.LoadResults(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	report := &VerifyReport{SnapshotID: snapshotID, TruthFingerprint: truth.Fingerprint, Chunks: len(results)}
	byChunk := make(map[int]ChunkResult, len(results))
	for _, res := range results {
		if _, dup := byChunk[res.ChunkID]; dup {
			return report, diffError(&Diff{ChunkID: res.ChunkID, Field: "chunk_result", Reason: "duplicate persisted chunk"})
		}
		if res.State != ChunkCommitted {
			return report, diffError(&Diff{ChunkID: res.ChunkID, Field: "state", Expected: string(ChunkCommitted), Actual: string(res.State)})
		}
		byChunk[res.ChunkID] = res
	}
	truthChunks := make(map[int]TruthChunk, len(truth.Chunks))
	for _, c := range truth.Chunks {
		truthChunks[c.ChunkID] = c
	}
	if len(byChunk) != len(truthChunks) {
		for _, id := range sortedKeys(truthChunks) {
			if _, ok := byChunk[id]; !ok {
				return report, diffError(&Diff{ChunkID: id, Field: "chunk_result", Reason: "missing committed chunk"})
			}
		}
		return report, diffError(&Diff{ChunkID: -1, Field: "chunk_count", Expected: len(truthChunks), Actual: len(byChunk)})
	}

	truthPaths := make(map[string]TruthPath, len(truth.Paths))
	truthPathCount := make(map[int]int, len(truth.Chunks))
	for _, p := range truth.Paths {
		truthPaths[p.PathID] = p
		truthPathCount[p.ChunkID]++
	}
	truthListeners := make(map[int]TruthListener, len(truth.Listeners))
	for _, l := range truth.Listeners {
		truthListeners[l.ChunkID] = l
	}
	field := make(map[int][]BandEnergy)
	transferOut := map[string]map[string]int64{}
	transferIn := map[string]map[string]int64{}
	escapes := map[string]map[string]int64{}
	allOutgoing := map[string]int{}
	for _, res := range results {
		if got := PathDigest(res.Paths); got != res.PathDigest {
			return report, diffError(&Diff{ChunkID: res.ChunkID, Field: "path_digest", Expected: res.PathDigest, Actual: got})
		}
		if len(res.Paths) != truthPathCount[res.ChunkID] {
			return report, diffError(&Diff{ChunkID: res.ChunkID, Field: "path_count", Expected: truthPathCount[res.ChunkID], Actual: len(res.Paths)})
		}
		for _, t := range res.Outgoing {
			allOutgoing[canonicalHash(t)]++
			if transferOut[t.SourceID] == nil {
				transferOut[t.SourceID] = map[string]int64{}
			}
			for _, e := range t.Energy {
				transferOut[t.SourceID][e.BandID] += e.Energy
			}
		}
		for _, t := range res.Incoming {
			allOutgoing[canonicalHash(t)]--
			if transferIn[t.SourceID] == nil {
				transferIn[t.SourceID] = map[string]int64{}
			}
			for _, e := range t.Energy {
				transferIn[t.SourceID][e.BandID] += e.Energy
			}
		}
		for _, e := range res.Escapes {
			if escapes[e.SourceID] == nil {
				escapes[e.SourceID] = map[string]int64{}
			}
			escapes[e.SourceID][e.BandID] += e.Energy
		}
		var recomputed []BandEnergy
		for _, se := range res.SourceEnergy {
			for _, e := range se.Energy {
				recomputed = addBandEnergy(recomputed, e.BandID, e.Energy)
			}
		}
		sortBandEnergy(recomputed)
		field[res.ChunkID] = recomputed
		for _, band := range sortedBandIDs(truth.Bands) {
			wantStored := bandValue(res.ArrivalEnergy, band)
			gotRecomputed := bandValue(recomputed, band)
			if wantStored != gotRecomputed {
				return report, diffError(&Diff{ChunkID: res.ChunkID, BandID: band, Field: "arrival_energy_recomputed", Expected: wantStored, Actual: gotRecomputed})
			}
			tc := truthChunks[res.ChunkID]
			wantTruth := bandValue(tc.Energy, band)
			if wantStored != wantTruth {
				return report, diffError(&Diff{ChunkID: res.ChunkID, BandID: band, Field: "arrival_energy_truth", Expected: wantTruth, Actual: wantStored})
			}
			for _, se := range res.SourceEnergy {
				wantSE := int64(0)
				for _, tse := range tc.SourceEnergy {
					if tse.SourceID == se.SourceID {
						wantSE = bandValue(tse.Energy, band)
					}
				}
				if got := bandValue(se.Energy, band); got != wantSE {
					return report, diffError(&Diff{ChunkID: res.ChunkID, BandID: band, SourceID: se.SourceID, Field: "source_energy_truth", Expected: wantSE, Actual: got})
				}
			}
		}
		for _, p := range res.Paths {
			tp, ok := truthPaths[p.ID]
			if !ok {
				return report, diffError(&Diff{ChunkID: res.ChunkID, BandID: "", Field: "path_presence", Actual: p.ID, Reason: "path not present in truth"})
			}
			delta := tp.LengthMicros - p.LengthMicros
			if delta < 0 {
				delta = -delta
			}
			if delta > options.PathToleranceMicros {
				return report, diffError(&Diff{ChunkID: res.ChunkID, Field: "path_length", PathID: p.ID, Expected: tp.LengthMicros, Actual: p.LengthMicros})
			}
		}
		if tl, ok := truthListeners[res.ChunkID]; ok {
			if got := sumBandEnergy(res.ArrivalEnergy); got != tl.TotalEnergy {
				return report, diffError(&Diff{ChunkID: res.ChunkID, Field: "listener_total_energy", Expected: tl.TotalEnergy, Actual: got})
			}
		}
	}
	for digest, n := range allOutgoing {
		if n != 0 {
			return report, diffError(&Diff{ChunkID: -1, Field: "boundary_transfer_pairing", Reason: fmt.Sprintf("unpaired transfer digest %s count=%d", digest, n)})
		}
	}

	truthEmissions := map[string][]BandEnergy{}
	truthEscapes := map[string][]BandEnergy{}
	for _, s := range truth.Sources {
		truthEmissions[s.SourceID] = s.Emission
		truthEscapes[s.SourceID] = s.Escape
	}
	cons := ConservationReport{OK: true}
	sourceIDs := make([]string, 0, len(truthEmissions))
	for id := range truthEmissions {
		sourceIDs = append(sourceIDs, id)
	}
	sort.Strings(sourceIDs)
	for _, sourceID := range sourceIDs {
		for _, band := range sortedBandIDs(truth.Bands) {
			emission := bandValue(truthEmissions[sourceID], band)
			var captured int64
			for _, res := range results {
				for _, se := range res.SourceEnergy {
					if se.SourceID == sourceID {
						captured += bandValue(se.Energy, band)
					}
				}
			}
			net := transferOut[sourceID][band] - transferIn[sourceID][band]
			esc := escapes[sourceID][band]
			wantEsc := bandValue(truthEscapes[sourceID], band)
			if esc != wantEsc {
				return report, diffError(&Diff{ChunkID: -1, BandID: band, SourceID: sourceID, Field: "escape_energy", Expected: wantEsc, Actual: esc})
			}
			residual := emission - (captured + net + esc)
			absResidual := residual
			if absResidual < 0 {
				absResidual = -absResidual
			}
			if absResidual > cons.MaxResidual {
				cons.MaxResidual = absResidual
			}
			if absResidual > options.ConservationResidue {
				cons.OK = false
				if cons.Error == "" {
					cons.Error = fmt.Sprintf("source=%s band=%s residual=%d", sourceID, band, residual)
				}
			}
			cons.Bands = append(cons.Bands, ConservationBand{
				SourceID: sourceID, BandID: band, Emission: emission, ChunkEnergy: captured,
				BoundaryNet: net, Escape: esc, Residual: residual,
			})
		}
	}
	report.Conservation = cons
	if !cons.OK {
		return report, diffError(&Diff{ChunkID: -1, Field: "conservation", Expected: 0, Actual: cons.MaxResidual, Reason: cons.Error})
	}

	seq, seqHash, err := verifyCommitSequence(ctx, store, snapshotID, meta.LastRound)
	if err != nil {
		return report, err
	}
	if len(seq) != len(results) {
		return report, diffError(&Diff{ChunkID: -1, Field: "commit_sequence_length", Expected: len(results), Actual: len(seq)})
	}
	seqSet := make(map[int]bool, len(seq))
	for _, id := range seq {
		if seqSet[id] {
			return report, diffError(&Diff{ChunkID: id, Field: "commit_sequence_duplicate"})
		}
		seqSet[id] = true
	}
	for _, res := range results {
		if !seqSet[res.ChunkID] {
			return report, diffError(&Diff{ChunkID: res.ChunkID, Field: "commit_sequence_missing"})
		}
	}
	report.CommitSequence = seq
	report.CommitSequenceHash = seqHash
	report.FieldFingerprint = fieldFingerprint(results)
	report.OK = true
	return report, nil
}

func verifyCommitSequence(ctx context.Context, store *Store, snapshotID string, roundID int64) ([]int, string, error) {
	log, err := store.LoadCommitLog(ctx, snapshotID, roundID)
	if err != nil {
		return nil, "", err
	}
	if len(log) == 0 {
		return nil, "", diffError(&Diff{ChunkID: -1, Field: "commit_sequence", Reason: "commit log is empty"})
	}
	seq := make([]int, 0, len(log))
	for _, e := range log {
		if e.Sequence != len(seq) {
			return nil, "", diffError(&Diff{ChunkID: e.ChunkID, Field: "commit_sequence", Sequence: e.Sequence, Expected: len(seq), Actual: e.Sequence})
		}
		seq = append(seq, e.ChunkID)
	}
	return seq, canonicalHash(seq), nil
}

func fieldFingerprint(results []ChunkResult) string {
	type fieldResult struct {
		ChunkID int          `json:"chunk_id"`
		Energy  []BandEnergy `json:"energy"`
		Paths   []Path       `json:"paths"`
	}
	list := make([]fieldResult, 0, len(results))
	for _, res := range results {
		list = append(list, fieldResult{ChunkID: res.ChunkID, Energy: res.ArrivalEnergy, Paths: res.Paths})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ChunkID < list[j].ChunkID })
	return canonicalHash(list)
}

func sortedKeys(v map[int]TruthChunk) []int {
	out := make([]int, 0, len(v))
	for id := range v {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

func sortedBandIDs(v []string) []string {
	out := append([]string(nil), v...)
	sort.Strings(out)
	return out
}

// CompareRunReports locates the first differing chunk/band/field between two
// reconstruction paths. It is used by the three-path acceptance check.
func CompareRunReports(runA, runB *RunReport) error {
	if runA == nil || runB == nil {
		return diffError(&Diff{ChunkID: -1, Field: "run", Reason: "nil run report"})
	}
	a := make(map[int]ChunkResult, len(runA.Results))
	b := make(map[int]ChunkResult, len(runB.Results))
	for _, r := range runA.Results {
		a[r.ChunkID] = r
	}
	for _, r := range runB.Results {
		b[r.ChunkID] = r
	}
	ids := make([]int, 0, len(a)+len(b))
	seen := map[int]bool{}
	for id := range a {
		ids = append(ids, id)
		seen[id] = true
	}
	for id := range b {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	for _, id := range ids {
		ra, oka := a[id]
		rb, okb := b[id]
		if !oka || !okb {
			return diffError(&Diff{ChunkID: id, Field: "chunk_result", Reason: "chunk exists in only one run"})
		}
		if err := compareChunkResults(ra, rb); err != nil {
			return err
		}
	}
	if len(runA.Submissions) != len(runB.Submissions) {
		return diffError(&Diff{ChunkID: -1, Field: "submission_count", Expected: len(runA.Submissions), Actual: len(runB.Submissions)})
	}
	for i := range runA.Submissions {
		sa, sb := runA.Submissions[i], runB.Submissions[i]
		if sa.Sequence != sb.Sequence || sa.ChunkID != sb.ChunkID {
			return diffError(&Diff{ChunkID: sb.ChunkID, Field: "commit_sequence", Sequence: i, Expected: sa.ChunkID, Actual: sb.ChunkID})
		}
	}
	if !sameConservation(runA.Conservation, runB.Conservation) {
		return diffError(&Diff{ChunkID: -1, Field: "conservation_residual", Expected: runA.Conservation.MaxResidual, Actual: runB.Conservation.MaxResidual})
	}
	return nil
}

func compareChunkResults(a, b ChunkResult) error {
	bands := map[string]bool{}
	for _, e := range a.ArrivalEnergy {
		bands[e.BandID] = true
	}
	for _, e := range b.ArrivalEnergy {
		bands[e.BandID] = true
	}
	bandIDs := make([]string, 0, len(bands))
	for id := range bands {
		bandIDs = append(bandIDs, id)
	}
	sort.Strings(bandIDs)
	for _, band := range bandIDs {
		if x, y := bandValue(a.ArrivalEnergy, band), bandValue(b.ArrivalEnergy, band); x != y {
			return diffError(&Diff{ChunkID: a.ChunkID, BandID: band, Field: "arrival_energy", Expected: x, Actual: y})
		}
	}
	if canonicalHash(a.SourceEnergy) != canonicalHash(b.SourceEnergy) {
		return diffError(&Diff{ChunkID: a.ChunkID, Field: "source_energy", Expected: sourceEnergyDigest(a.SourceEnergy), Actual: sourceEnergyDigest(b.SourceEnergy)})
	}
	if len(a.Paths) != len(b.Paths) {
		return diffError(&Diff{ChunkID: a.ChunkID, Field: "path_count", Expected: len(a.Paths), Actual: len(b.Paths)})
	}
	for i := range a.Paths {
		if a.Paths[i].ID != b.Paths[i].ID {
			return diffError(&Diff{ChunkID: a.ChunkID, Field: "path_id", PathID: a.Paths[i].ID, Expected: a.Paths[i].ID, Actual: b.Paths[i].ID})
		}
		if a.Paths[i].LengthMicros != b.Paths[i].LengthMicros {
			return diffError(&Diff{ChunkID: a.ChunkID, Field: "path_length", PathID: a.Paths[i].ID, Expected: a.Paths[i].LengthMicros, Actual: b.Paths[i].LengthMicros})
		}
		if canonicalHash(a.Paths[i].Energy) != canonicalHash(b.Paths[i].Energy) {
			return diffError(&Diff{ChunkID: a.ChunkID, Field: "path_energy", PathID: a.Paths[i].ID, Expected: canonicalHash(a.Paths[i].Energy), Actual: canonicalHash(b.Paths[i].Energy)})
		}
	}
	if canonicalHash(a.Incoming) != canonicalHash(b.Incoming) {
		return diffError(&Diff{ChunkID: a.ChunkID, Field: "incoming_transfer"})
	}
	if canonicalHash(a.Outgoing) != canonicalHash(b.Outgoing) {
		return diffError(&Diff{ChunkID: a.ChunkID, Field: "outgoing_transfer"})
	}
	if canonicalHash(a.Escapes) != canonicalHash(b.Escapes) {
		return diffError(&Diff{ChunkID: a.ChunkID, Field: "escape_energy"})
	}
	return nil
}

func sameConservation(a, b ConservationReport) bool {
	if len(a.Bands) != len(b.Bands) {
		return false
	}
	for i := range a.Bands {
		if a.Bands[i] != b.Bands[i] {
			return false
		}
	}
	return a.MaxResidual == b.MaxResidual
}
