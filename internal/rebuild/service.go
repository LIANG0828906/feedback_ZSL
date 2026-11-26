// Package rebuild orchestrates deterministic full, realtime/incremental and recovery runs.
package rebuild

import (
	"context"
	"sort"

	"acousticfield/internal/acoustic"
	"acousticfield/internal/checkpoint"
	"acousticfield/internal/deps"
	"acousticfield/internal/engine"
	"acousticfield/internal/schedx"
	"acousticfield/internal/schema"
	"acousticfield/internal/statefx"
)

type Service struct {
	Level           *acoustic.Level
	Truth           *acoustic.Truth
	Store           *checkpoint.Store
	CommittedRounds int64
}

type RunOptions struct {
	RoundID           string
	Mode              string
	Full              bool
	Recover           bool
	Invalidations     []acoustic.Invalidation
	TransferOverrides map[string]int64
	AfterCommit       func(chunkID string, txnSeq int64) error
}

func NewService(level *acoustic.Level, truth *acoustic.Truth, store *checkpoint.Store) *Service {
	return &Service{Level: level, Truth: truth, Store: store}
}

func (s *Service) Run(ctx context.Context, opts RunOptions) (*acoustic.Report, error) {
	if err := schema.ValidateLevel(s.Level); err != nil {
		return nil, err
	}
	if opts.RoundID == "" {
		opts.RoundID = "round-1"
	}
	if opts.Mode == "" {
		opts.Mode = "full"
	}
	p := s.Level.Params
	schema.NormalizeParams(&p)
	s.Level.Params = p

	if s.Store != nil {
		if err := s.Store.SaveParams(p); err != nil {
			return nil, err
		}
		if s.Truth != nil {
			if err := s.Store.SaveTruth(s.Truth); err != nil {
				return nil, err
			}
		}
	}
	truthFingerprint := ""
	if s.Truth != nil {
		truthFingerprint = s.Truth.Fingerprint
	}
	if s.Store != nil {
		if err := s.Store.SaveSnapshot(s.Level, truthFingerprint); err != nil {
			return nil, err
		}
	}

	orderedChunks := ChunkOrder(s.Level)
	orderIDs := make([]string, 0, len(orderedChunks))
	chunkByID := map[string]acoustic.Chunk{}
	for _, c := range orderedChunks {
		orderIDs = append(orderIDs, c.ID)
		chunkByID[c.ID] = c
	}

	report := &acoustic.Report{
		SchemaVersion:    "rebuild-report-v1",
		Seed:             s.Level.Seed,
		RoundID:          opts.RoundID,
		Mode:             opts.Mode,
		LevelHash:        acoustic.HashValue("level:", s.Level),
		ParamVersion:     p.ParamVersion,
		AlgorithmVersion: p.AlgorithmVersion,
		ChunkOrder:       append([]string(nil), orderIDs...),
		CommitSequence:   []acoustic.CommitInfo{},
		Chunks:           []acoustic.ChunkResult{},
		Invalidations:    append([]acoustic.Invalidation(nil), opts.Invalidations...),
		Queue:            []acoustic.QueueRecord{},
	}
	report.TruthFingerprint = truthFingerprint

	round := statefx.NewRound(opts.RoundID)
	if err := round.Transition(statefx.RoundRunning); err != nil {
		return report, err
	}
	tracker := statefx.NewProgress(len(orderIDs))

	invalid := map[string]bool{}
	for _, inv := range opts.Invalidations {
		invalid[inv.ChunkID] = true
	}
	if opts.Full {
		invalid = map[string]bool{}
		for _, id := range orderIDs {
			invalid[id] = true
		}
	}

	resultByID := map[string]acoustic.ChunkResult{}
	sourceByID := map[string]string{}
	txnByID := map[string]int64{}
	existingMaxTxn := int64(0)
	if s.Store != nil {
		latest, records, err := s.Store.LatestMatching(p.AlgorithmVersion, p.ParamVersion)
		if err != nil {
			return report, err
		}
		for _, rec := range records {
			if rec.TxnSeq > existingMaxTxn {
				existingMaxTxn = rec.TxnSeq
			}
		}
		for id, rec := range latest {
			if rec.TxnSeq > existingMaxTxn {
				existingMaxTxn = rec.TxnSeq
			}
			if opts.Recover && rec.RoundID == opts.RoundID {
				resultByID[id] = rec.Result
				sourceByID[id] = "recovered"
				txnByID[id] = rec.TxnSeq
				tracker.ChunkCommitted()
				continue
			}
			if !opts.Full && !invalid[id] {
				currentHash := engine.ContentHash(chunkByID[id], s.Level.Sources, s.Level.Listeners)
				if rec.Result.ContentHash == currentHash && rec.Result.ParamVersion == p.ParamVersion {
					resultByID[id] = rec.Result
					sourceByID[id] = "reused"
					txnByID[id] = rec.TxnSeq
					tracker.ChunkCommitted()
				}
			}
		}
	}

	missingIDs := make([]string, 0)
	for _, id := range orderIDs {
		if _, ok := resultByID[id]; !ok {
			missingIDs = append(missingIDs, id)
		}
	}

	machines := map[string]*statefx.ChunkMachine{}
	screenResults := make([]*acoustic.ChunkResult, len(missingIDs))
	jobs := make([]schedx.Job, 0, len(missingIDs))
	for i, id := range missingIDs {
		i, id := i, id
		machine := statefx.NewChunk(id)
		machines[id] = machine
		jobs = append(jobs, schedx.Job{
			ChunkID: id,
			Run: func(workerID, bufferCap int) error {
				if err := machine.Transition(statefx.ChunkScreening); err != nil {
					return err
				}
				result, err := engine.ScreenChunk(chunkByID[id], s.Level.Sources, s.Level.Listeners, p, bufferCap)
				if err != nil {
					_ = machine.Transition(statefx.ChunkFailed)
					return err
				}
				screenResults[i] = result
				return nil
			},
		})
	}

	pool := schedx.DefaultPool(p.Budget)
	jobErrs, queue := pool.Run(ctx, jobs)
	report.Queue = queue

	var firstErr error
	for i, id := range missingIDs {
		if jobErrs[i] != nil {
			if firstErr == nil {
				firstErr = jobErrs[i]
			}
			code := "E_ENGINE"
			if ae, ok := acoustic.AsError(jobErrs[i]); ok {
				code = string(ae.Code)
			}
			report.Failures = append(report.Failures, acoustic.ChunkFailure{ChunkID: id, Code: code, Message: jobErrs[i].Error()})
		} else if screenResults[i] != nil {
			if err := machines[id].Transition(statefx.ChunkTransfer); err != nil {
				return report, err
			}
			resultByID[id] = *screenResults[i]
			sourceByID[id] = "computed"
		}
	}
	if firstErr != nil {
		_ = round.Transition(statefx.RoundFailed)
		report.Progress = tracker.Snapshot()
		report.Progress.CommittedRounds = s.CommittedRounds
		report.Conservation, _ = CheckConservation(s.Level, orderedResults(orderIDs, resultByID))
		_ = s.persistReport(report)
		return report, firstErr
	}
	if ctx.Err() != nil {
		_ = round.Transition(statefx.RoundFailed)
		report.Progress = tracker.Snapshot()
		report.Progress.CommittedRounds = s.CommittedRounds
		return report, ctx.Err()
	}

	boundariesByID := map[string][]Boundary{}
	for _, c := range orderedChunks {
		boundariesByID[c.ID] = Boundaries(c, s.Level.Chunks)
	}

	for _, id := range orderIDs {
		result, ok := resultByID[id]
		if !ok || result.CacheKey != "" {
			continue
		}
		buildOutboundTransfers(id, &result, boundariesByID[id], p, opts.TransferOverrides)
		resultByID[id] = result
	}
	applyTransferOverrides(resultByID, opts.TransferOverrides, p)

	inboundQ := map[string][]int64{}
	inboundIDs := map[string][]string{}
	inboundTransfers := map[string][]acoustic.Transfer{}
	upstream := map[string][]cacheUpstream{}
	for _, id := range orderIDs {
		inboundQ[id] = make([]int64, len(s.Level.BandIDs))
	}
	for _, fromID := range orderIDs {
		fromResult := resultByID[fromID]
		for _, transfer := range fromResult.Transfers {
			to := transfer.ToChunkID
			for b, v := range transfer.BandDeliveredQ {
				if b < len(inboundQ[to]) {
					inboundQ[to][b] += v
				}
			}
			inboundIDs[to] = append(inboundIDs[to], transfer.ID)
			inboundTransfers[to] = append(inboundTransfers[to], transfer)
			upstream[to] = append(upstream[to], cacheUpstream{
				FromChunkID:             fromID,
				UpstreamCacheKey:        fromResult.LocalCacheKey,
				BoundaryTransferID:      transfer.ID,
				BoundaryTransferVersion: transfer.TransferVersion,
			})
		}
	}

	for _, id := range orderIDs {
		result := resultByID[id]
		if result.CacheKey != "" {
			continue
		}
		result.InboundQ = inboundQ[id]
		sort.Strings(inboundIDs[id])
		result.InboundTransferIDs = inboundIDs[id]
		result.InboundTransfers = sortTransfers(inboundTransfers[id])
		noNeighborExterior := int64(0)
		if len(boundariesByID[id]) == 0 {
			for _, v := range result.OutboundSentQ {
				noNeighborExterior += v
			}
			result.Quantization = append(result.Quantization, acoustic.QuantRecord{
				Stage: "exterior_field_sink", Location: id, TruncatedQ: noNeighborExterior,
				ResidualQ: 0,
			})
		}
		for b := range result.RetainedQ {
			exterior := int64(0)
			if len(boundariesByID[id]) == 0 && b < len(result.OutboundSentQ) {
				exterior = result.OutboundSentQ[b]
			}
			result.RetainedQ[b] = safeGet(result.ListenerArrivalQ, b) + safeGet(result.AbsorbedQ, b) + safeGet(result.InboundQ, b) + exterior
		}
		result.ArrivalEnergyQ = acoustic.CopyInt64s(result.RetainedQ)
		sort.Slice(upstream[id], func(i, j int) bool {
			return upstream[id][i].BoundaryTransferID < upstream[id][j].BoundaryTransferID
		})
		result.CacheKey = acoustic.HashValue("chunk-cache:", struct {
			ChunkID          string
			ContentHash      string
			LocalCacheKey    string
			ParamVersion     string
			AlgorithmVersion string
			Upstream         []cacheUpstream
		}{id, result.ContentHash, result.LocalCacheKey, p.ParamVersion, p.AlgorithmVersion, upstream[id]})
		resultByID[id] = result
	}

	ordered := orderedResults(orderIDs, resultByID)
	conservation, consErr := CheckConservation(s.Level, ordered)
	report.Conservation = conservation
	report.Chunks = ordered
	if consErr != nil {
		_ = round.Transition(statefx.RoundFailed)
		report.Progress = tracker.Snapshot()
		_ = s.persistReport(report)
		return report, consErr
	}

	nextTxn := existingMaxTxn + 1
	for i, id := range orderIDs {
		if ctx.Err() != nil {
			_ = round.Transition(statefx.RoundFailed)
			report.Progress = tracker.Snapshot()
			report.Progress.CommittedRounds = s.CommittedRounds
			_ = s.persistReport(report)
			return report, ctx.Err()
		}
		result := resultByID[id]
		info := acoustic.CommitInfo{ChunkID: id, Seq: i + 1, CacheKey: result.CacheKey, Source: sourceByID[id]}
		if info.Source == "computed" {
			if s.Store != nil {
				rec, err := s.Store.CommitChunk(opts.RoundID, nextTxn, i+1, result)
				if err != nil {
					_ = machines[id].Transition(statefx.ChunkFailed)
					_ = round.Transition(statefx.RoundFailed)
					return report, err
				}
				info.TxnSeq = rec.TxnSeq
				nextTxn++
			}
			if err := machines[id].Transition(statefx.ChunkCommitted); err != nil {
				return report, err
			}
			tracker.ChunkCommitted()
			if opts.AfterCommit != nil {
				if err := opts.AfterCommit(id, info.TxnSeq); err != nil {
					report.CommitSequence = append(report.CommitSequence, info)
					report.Progress = tracker.Snapshot()
					_ = s.persistReport(report)
					return report, err
				}
			}
		} else {
			info.TxnSeq = txnByID[id]
		}
		report.CommitSequence = append(report.CommitSequence, info)
	}

	if err := round.Transition(statefx.RoundCommitted); err != nil {
		return report, err
	}
	s.CommittedRounds++
	tracker.RoundCommitted()
	report.Progress = tracker.Snapshot()
	report.Progress.CommittedRounds = s.CommittedRounds
	if err := s.persistReport(report); err != nil {
		return report, err
	}
	return report, nil
}

func (s *Service) persistReport(report *acoustic.Report) error {
	if s.Store == nil {
		return nil
	}
	return s.Store.SaveReport(report)
}

type cacheUpstream struct {
	FromChunkID             string
	UpstreamCacheKey        string
	BoundaryTransferID      string
	BoundaryTransferVersion string
}

func orderedResults(order []string, byID map[string]acoustic.ChunkResult) []acoustic.ChunkResult {
	out := make([]acoustic.ChunkResult, 0, len(order))
	for _, id := range order {
		if r, ok := byID[id]; ok {
			out = append(out, r)
		}
	}
	return out
}

func buildOutboundTransfers(chunkID string, result *acoustic.ChunkResult, boundaries []Boundary, p acoustic.Params, overrides map[string]int64) {
	bands := len(result.OutboundSentQ)
	if len(boundaries) == 0 {
		return
	}
	k := int64(len(boundaries))
	sentByBoundary := make([][]int64, len(boundaries))
	for j := range boundaries {
		sentByBoundary[j] = make([]int64, bands)
	}
	for b := 0; b < bands; b++ {
		total := result.OutboundSentQ[b]
		base := total / k
		rem := total % k
		for j := range boundaries {
			v := base
			if int64(j) < rem {
				v++
			}
			sentByBoundary[j][b] = v
		}
	}

	for j, boundary := range boundaries {
		transferID := transferID(chunkID, boundary.NeighborID, boundary.Side)
		permille := p.BoundaryTransmissionPermille
		if overrides != nil {
			if v, ok := overrides[transferID]; ok && v >= 0 && v <= 1000 {
				permille = v
			}
		}
		transfer := acoustic.Transfer{
			ID:                   transferID,
			FromChunkID:          chunkID,
			ToChunkID:            boundary.NeighborID,
			Boundary:             boundary.Side,
			Position:             boundary.Position,
			BandSentQ:            sentByBoundary[j],
			BandDeliveredQ:       make([]int64, bands),
			BandLossQ:            make([]int64, bands),
			TransmissionPermille: permille,
		}
		for b := 0; b < bands; b++ {
			numerator := transfer.BandSentQ[b] * permille
			delivered := numerator / 1000
			transfer.BandDeliveredQ[b] = delivered
			transfer.BandLossQ[b] = transfer.BandSentQ[b] - delivered
			result.Quantization = append(result.Quantization, acoustic.QuantRecord{
				BandID:       b,
				Stage:        "transfer_div",
				Location:     transferID,
				NumeratorQ:   numerator,
				DenominatorQ: 1000,
				TruncatedQ:   delivered,
				ResidualQ:    numerator % 1000,
			})
		}
		transfer.TransferVersion = acoustic.ShortHash(struct {
			ID        string
			Permille  int64
			Sent      []int64
			Delivered []int64
			Loss      []int64
		}{transfer.ID, permille, transfer.BandSentQ, transfer.BandDeliveredQ, transfer.BandLossQ}, 16)
		result.Transfers = append(result.Transfers, transfer)
		key := "boundary_transfer:" + transfer.FromChunkID + "->" + transfer.ToChunkID
		result.Dependencies = appendUniqueString(result.Dependencies, key)
		result.DependencyRecords = append(result.DependencyRecords, acoustic.Dependency{
			Key: key, Kind: "boundary_transfer", ChunkID: transfer.FromChunkID,
			TargetChunkID: transfer.ToChunkID, Field: transfer.ID,
		})
		result.Dependencies = appendUniqueString(result.Dependencies, "downstream:"+transfer.ToChunkID+":energy")
	}
	result.Transfers = sortTransfers(result.Transfers)
	sort.Strings(result.Dependencies)
}

func applyTransferOverrides(byID map[string]acoustic.ChunkResult, overrides map[string]int64, p acoustic.Params) {
	if len(overrides) == 0 {
		return
	}
	for id, result := range byID {
		changed := false
		for i := range result.Transfers {
			t := &result.Transfers[i]
			permille, ok := overrides[t.ID]
			if !ok || permille < 0 || permille > 1000 {
				continue
			}
			t.TransmissionPermille = permille
			for b := range t.BandSentQ {
				num := t.BandSentQ[b] * permille
				t.BandDeliveredQ[b] = num / 1000
				t.BandLossQ[b] = t.BandSentQ[b] - t.BandDeliveredQ[b]
			}
			t.TransferVersion = acoustic.ShortHash(struct {
				ID        string
				Permille  int64
				Sent      []int64
				Delivered []int64
				Loss      []int64
			}{t.ID, permille, t.BandSentQ, t.BandDeliveredQ, t.BandLossQ}, 16)
			changed = true
		}
		if changed {
			byID[id] = result
		}
	}
}

func transferID(from, to, side string) string {
	return "TR:" + from + ":" + to + ":" + side
}

func sortTransfers(in []acoustic.Transfer) []acoustic.Transfer {
	sort.SliceStable(in, func(i, j int) bool { return in[i].ID < in[j].ID })
	return in
}

func appendUniqueString(in []string, v string) []string {
	for _, x := range in {
		if x == v {
			return in
		}
	}
	return append(in, v)
}

// Graph is re-exported for callers that build invalidation plans.
func DependencyGraph(reports ...*acoustic.Report) *deps.Graph {
	g := deps.NewGraph()
	for _, report := range reports {
		if report == nil {
			continue
		}
		for _, chunk := range report.Chunks {
			for _, key := range chunk.Dependencies {
				g.AddDependency(key, chunk.ChunkID)
			}
			for _, t := range chunk.Transfers {
				g.AddDownstream(t.FromChunkID, t.ToChunkID, "upstream_boundary_energy_changed")
			}
		}
	}
	return g
}
