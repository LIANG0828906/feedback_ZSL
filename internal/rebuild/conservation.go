package rebuild

import (
	"fmt"

	"acousticfield/internal/acoustic"
)

func CheckConservation(level *acoustic.Level, results []acoustic.ChunkResult) (acoustic.ConservationReport, error) {
	bands := len(level.BandIDs)
	report := acoustic.ConservationReport{
		OK:         true,
		ToleranceQ: level.Params.ConservationToleranceQ,
		BandIDs:    append([]int(nil), level.BandIDs...),
		EmittedQ:   make([]int64, bands),
		RetainedQ:  sumBands(results, bands, func(r acoustic.ChunkResult) []int64 { return r.RetainedQ }),
	}

	emittedByChunk := map[string][]int64{}
	for _, source := range level.Sources {
		if _, ok := emittedByChunk[source.ChunkID]; !ok {
			emittedByChunk[source.ChunkID] = make([]int64, bands)
		}
		for b := 0; b < bands && b < len(source.EmissionQ); b++ {
			report.EmittedQ[b] += source.EmissionQ[b]
			emittedByChunk[source.ChunkID][b] += source.EmissionQ[b]
		}
	}

	report.TransferLossQ = make([]int64, bands)
	for _, result := range results {
		for b := 0; b < bands; b++ {
			expectedRetained := safeGet(result.ListenerArrivalQ, b) + safeGet(result.AbsorbedQ, b) + safeGet(result.InboundQ, b)
			if len(result.Transfers) == 0 {
				expectedRetained += safeGet(result.OutboundSentQ, b)
			}
			if residual := safeGet(result.RetainedQ, b) - expectedRetained; acoustic.Abs64(residual) > report.ToleranceQ {
				report.OK = false
				report.Violations = append(report.Violations, acoustic.ConservationViolation{
					Code: acoustic.CodeConservation, BandID: b, SourceChunkID: result.ChunkID,
					Kind: "retained_assembly", ResidualQ: residual, ToleranceQ: report.ToleranceQ,
					Message: fmt.Sprintf("chunk %s retained energy assembly residual %d", result.ChunkID, residual),
				})
			}
		}
		for _, transfer := range result.Transfers {
			for b := 0; b < bands; b++ {
				if b < len(transfer.BandLossQ) {
					report.TransferLossQ[b] += transfer.BandLossQ[b]
				}
				var sent, delivered, loss int64
				if b < len(transfer.BandSentQ) {
					sent = transfer.BandSentQ[b]
				}
				if b < len(transfer.BandDeliveredQ) {
					delivered = transfer.BandDeliveredQ[b]
				}
				if b < len(transfer.BandLossQ) {
					loss = transfer.BandLossQ[b]
				}
				if residual := sent - delivered - loss; acoustic.Abs64(residual) > report.ToleranceQ {
					report.OK = false
					report.Violations = append(report.Violations, acoustic.ConservationViolation{
						Code: acoustic.CodeConservation, BandID: b, SourceChunkID: transfer.FromChunkID,
						Kind: "transfer_ledger", ResidualQ: residual, ToleranceQ: report.ToleranceQ,
						Message: fmt.Sprintf("transfer %s sent-delivered-loss residual %d", transfer.ID, residual),
					})
				}
			}
		}
	}

	report.ResidualQ = make([]int64, bands)
	for b := 0; b < bands; b++ {
		report.ResidualQ[b] = report.EmittedQ[b] - report.RetainedQ[b] - report.TransferLossQ[b]
		if acoustic.Abs64(report.ResidualQ[b]) > report.ToleranceQ {
			report.OK = false
			report.Violations = append(report.Violations, acoustic.ConservationViolation{
				Code: acoustic.CodeConservation, BandID: b, SourceChunkID: "LEVEL",
				Kind: "global_energy", ResidualQ: report.ResidualQ[b], ToleranceQ: report.ToleranceQ,
				Message: fmt.Sprintf("band %d emitted-retained-transfer_loss residual %d", b, report.ResidualQ[b]),
			})
		}
	}

	byID := map[string]acoustic.ChunkResult{}
	for _, r := range results {
		byID[r.ChunkID] = r
	}
	for chunkID, emitted := range emittedByChunk {
		r := byID[chunkID]
		for b := 0; b < bands; b++ {
			residual := emitted[b] - safeGet(r.ListenerArrivalQ, b) - safeGet(r.AbsorbedQ, b) - safeGet(r.OutboundSentQ, b)
			if acoustic.Abs64(residual) > report.ToleranceQ {
				report.OK = false
				report.Violations = append(report.Violations, acoustic.ConservationViolation{
					Code: acoustic.CodeConservation, BandID: b, SourceChunkID: chunkID,
					Kind: "source_ledger", ResidualQ: residual, ToleranceQ: report.ToleranceQ,
					Message: fmt.Sprintf("chunk %s source energy ledger residual %d", chunkID, residual),
				})
			}
		}
	}

	if !report.OK {
		err := acoustic.NewError(acoustic.CodeConservation, "level", "energy", "energy conservation failed with %d violations", len(report.Violations))
		err.Details = map[string]any{"violations": report.Violations}
		return report, err
	}
	return report, nil
}

func safeGet(values []int64, index int) int64 {
	if index < 0 || index >= len(values) {
		return 0
	}
	return values[index]
}
