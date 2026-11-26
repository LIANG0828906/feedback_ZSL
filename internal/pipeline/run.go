// Package pipeline wires generation metadata to quant, recon, graph and checkpoint.
package pipeline

import (
	"encoding/json"

	"msworkbench/internal/checkpoint"
	"msworkbench/internal/domain"
	"msworkbench/internal/graph"
	"msworkbench/internal/quant"
	"msworkbench/internal/recon"
	"msworkbench/internal/sample"
)

type CrashError struct{ AfterStages int }

func (e *CrashError) Error() string {
	return "simulated forced termination after checkpoint stage"
}

type BlockOutput struct {
	Block       domain.BlockCoord  `json:"block"`
	QuantKey    string             `json:"quant_key"`
	ReconKey    string             `json:"recon_key"`
	Source      string             `json:"source"`
	BandSources map[string]string  `json:"band_sources,omitempty"`
	Quant       *quant.Result      `json:"quant"`
	Recon       *recon.BlockResult `json:"recon"`
}

type Report struct {
	RunID                string                   `json:"run_id"`
	Profile              domain.SampleScale       `json:"profile"`
	Parallelism          int                      `json:"parallelism"`
	PixelBufferPerWorker int                      `json:"pixel_buffer_per_worker"`
	QueueCapacity        int                      `json:"queue_capacity"`
	Blocks               []*BlockOutput           `json:"blocks"`
	LogicalSequence      []string                 `json:"logical_sequence"`
	Records              []checkpoint.StageRecord `json:"records"`
	WaitEvents           []WaitEvent              `json:"wait_events,omitempty"`
	CacheHits            int                      `json:"cache_hits"`
	CacheMisses          int                      `json:"cache_misses"`
}

type Options struct {
	Project              *domain.Project
	Sample               *sample.SampleSet
	Graph                *graph.Graph
	Store                *checkpoint.FileStore
	RunID                string
	Profile              domain.SampleScale
	Thresholds           quant.Thresholds
	Parallelism          int
	PixelBufferPerWorker int
	QueueCapacity        int
	Resume               bool
	CrashAfterStages     int
}

func (o *Options) normalize() {
	o.Thresholds = o.Thresholds.Normalize()
	o.Parallelism, o.PixelBufferPerWorker, o.QueueCapacity = normalizeBudget(o.Parallelism, o.PixelBufferPerWorker, o.QueueCapacity)
	if o.Profile == "" {
		o.Profile = domain.ScaleDense
	}
}

func stageKey(block, stage string) string { return block + "/" + stage }

// Execute runs all blocks or resumes a run from verified checkpoint records.
func Execute(opts Options) (*Report, error) {
	opts.normalize()
	p, set, store := opts.Project, opts.Sample, opts.Store
	g := opts.Graph
	if g == nil {
		g = graph.Build(p, set, opts.Profile, opts.Thresholds)
	}
	coords := p.Coords()
	report := &Report{RunID: opts.RunID, Profile: opts.Profile, Parallelism: opts.Parallelism, PixelBufferPerWorker: opts.PixelBufferPerWorker, QueueCapacity: opts.QueueCapacity}

	existing := map[string]checkpoint.Transaction{}
	if opts.Resume {
		txs, err := store.RecoverRun(opts.RunID, set.BlockHashes)
		if err != nil {
			return nil, err
		}
		for _, tx := range txs {
			existing[stageKey(tx.Record.Block, tx.Record.Stage)] = tx
		}
	}

	type prebuilt struct {
		out *BlockOutput
		qr  *quant.Result
		rr  *recon.BlockResult
	}
	ready := map[string]prebuilt{}
	need := []domain.BlockCoord{}
	for _, c := range coords {
		block := c.String()
		qTx, qOK := existing[stageKey(block, checkpoint.StageQuant)]
		rTx, rOK := existing[stageKey(block, checkpoint.StageRecon)]
		switch {
		case qOK && rOK:
			qr := &quant.Result{}
			rr := &recon.BlockResult{}
			if err := json.Unmarshal(qTx.Payload, qr); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(rTx.Payload, rr); err != nil {
				return nil, err
			}
			ready[block] = prebuilt{qr: qr, rr: rr, out: &BlockOutput{Block: c, Quant: qr, Recon: rr, Source: "recovered_both_stages", QuantKey: qTx.Record.CacheKey, ReconKey: rTx.Record.CacheKey}}
			report.CacheHits += 2
		case qOK:
			qr := &quant.Result{}
			if err := json.Unmarshal(qTx.Payload, qr); err != nil {
				return nil, err
			}
			rr, err := recon.ReconstructBlock(p, qr)
			if err != nil {
				return nil, err
			}
			ready[block] = prebuilt{qr: qr, rr: rr, out: &BlockOutput{Block: c, Quant: qr, Recon: rr, Source: "recovered_quant_recomputed_recon", QuantKey: qTx.Record.CacheKey}}
			report.CacheHits++
		default:
			need = append(need, c)
			report.CacheMisses++
		}
	}

	computed, waits, err := computeBlocks(p, set, g, opts.Profile, opts.Thresholds, need, opts.Parallelism, opts.PixelBufferPerWorker, opts.QueueCapacity)
	if err != nil {
		return nil, err
	}
	report.WaitEvents = append(report.WaitEvents, waits...)

	committedInProcess := 0
	for _, c := range coords {
		block := c.String()
		var qr *quant.Result
		var rr *recon.BlockResult
		source := "computed"
		if pb, ok := ready[block]; ok {
			qr, rr = pb.qr, pb.rr
			source = pb.out.Source
		} else {
			cb := computed[block]
			qr, rr = cb.qr, cb.rr
		}
		quantKey := graph.QuantAggregateKey(g, block)
		reconKey := graph.ReconCacheKey(g, block)
		if pb, ok := ready[block]; ok && pb.out.QuantKey != "" {
			quantKey = pb.out.QuantKey
			if pb.out.ReconKey != "" {
				reconKey = pb.out.ReconKey
			}
		}

		if _, qExists := existing[stageKey(block, checkpoint.StageQuant)]; !qExists {
			maxBound := int64(0)
			for _, br := range qr.Bands {
				if br.ErrorBoundQ > maxBound {
					maxBound = br.ErrorBoundQ
				}
			}
			rec := checkpoint.StageRecord{
				RunID: opts.RunID, Block: block, Stage: checkpoint.StageQuant,
				OutputDigest: qr.QuantHash, SelectedMethod: string(qr.Decision.Method),
				ErrorBoundBP: maxBound, CacheKey: quantKey,
				BandCacheKeys: graph.BandCacheKeyMap(g, block), ContentHash: qr.InputHash,
			}
			committed, err := store.CommitStage(rec, qr)
			if err != nil {
				return nil, err
			}
			committedInProcess++
			if opts.CrashAfterStages > 0 && committedInProcess >= opts.CrashAfterStages {
				return nil, &CrashError{AfterStages: committedInProcess}
			}
			_ = committed
		}
		if _, rExists := existing[stageKey(block, checkpoint.StageRecon)]; !rExists {
			maxBound := int64(0)
			for _, b := range rr.ErrorBoundBP {
				if b > maxBound {
					maxBound = b
				}
			}
			rec := checkpoint.StageRecord{
				RunID: opts.RunID, Block: block, Stage: checkpoint.StageRecon,
				OutputDigest: rr.ReconHash, SelectedMethod: string(rr.Method),
				ErrorBoundBP: maxBound, CacheKey: reconKey, ContentHash: qr.InputHash,
			}
			if _, err := store.CommitStage(rec, rr); err != nil {
				return nil, err
			}
			committedInProcess++
			if opts.CrashAfterStages > 0 && committedInProcess >= opts.CrashAfterStages {
				return nil, &CrashError{AfterStages: committedInProcess}
			}
		}

		report.Blocks = append(report.Blocks, &BlockOutput{Block: c, Quant: qr, Recon: rr, Source: source, QuantKey: quantKey, ReconKey: reconKey})
		report.LogicalSequence = append(report.LogicalSequence, block+"/quant", block+"/recon")
	}

	txs := store.Transactions()
	for _, tx := range txs {
		if tx.Record.RunID == opts.RunID {
			report.Records = append(report.Records, tx.Record)
		}
	}
	return report, nil
}
