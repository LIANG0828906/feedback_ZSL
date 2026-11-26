package pipeline

import (
	"encoding/json"

	"msworkbench/internal/checkpoint"
	"msworkbench/internal/domain"
	"msworkbench/internal/graph"
	"msworkbench/internal/quant"
	"msworkbench/internal/recon"
)

// RunIncremental recomputes only graph-dirty blocks. Clean block outputs and
// clean block-band results are loaded from the persistent checkpoint cache.
func RunIncremental(opts Options) (*Report, error) {
	opts.normalize()
	if opts.Graph == nil {
		opts.Graph = graph.Build(opts.Project, opts.Sample, opts.Profile, opts.Thresholds)
	}
	g := opts.Graph
	g.RefreshDirtyKeys()
	p, set, store := g.Project, opts.Sample, opts.Store
	coords := p.Coords()
	report := &Report{RunID: opts.RunID, Profile: g.Profile, Parallelism: opts.Parallelism, PixelBufferPerWorker: opts.PixelBufferPerWorker, QueueCapacity: opts.QueueCapacity}

	dirtyCoords := []domain.BlockCoord{}
	outputs := map[string]*BlockOutput{}
	for _, c := range coords {
		if g.BlockDirty(c.String()) {
			dirtyCoords = append(dirtyCoords, c)
		}
	}
	computed, waits, err := computeBlocks(p, set, g, g.Profile, opts.Thresholds, dirtyCoords, opts.Parallelism, opts.PixelBufferPerWorker, opts.QueueCapacity)
	if err != nil {
		return nil, err
	}
	report.WaitEvents = waits

	for _, c := range dirtyCoords {
		block := c.String()
		cb := computed[block]
		qr := cb.qr
		bandSources := map[string]string{}
		for _, band := range p.Bands {
			id := "quant:" + block + ":" + band.ID
			node := g.Nodes[id]
			if node != nil && !node.Dirty {
				tx, ok := store.FindBandCache(node.CacheKey)
				if ok {
					old := &quant.Result{}
					if err := json.Unmarshal(tx.Payload, old); err == nil && old.Bands[band.ID] != nil {
						qr.Bands[band.ID] = old.Bands[band.ID]
						bandSources[band.ID] = "persistent-band-cache-hit"
						report.CacheHits++
						continue
					}
				}
			}
			bandSources[band.ID] = "recomputed"
			report.CacheMisses++
		}
		rr, err := recon.ReconstructBlock(p, qr)
		if err != nil {
			return nil, err
		}
		outputs[block] = &BlockOutput{Block: c, Quant: qr, Recon: rr, Source: "incremental_dirty_block", BandSources: bandSources}
	}

	for _, c := range coords {
		block := c.String()
		if _, ok := outputs[block]; ok {
			continue
		}
		aggKey := graph.QuantAggregateKey(g, block)
		reconKey := graph.ReconCacheKey(g, block)
		qPayload, qOK := store.FindCache(aggKey)
		rPayload, rOK := store.FindCache(reconKey)
		if !qOK || !rOK {
			one, waits, err := computeBlocks(p, set, g, g.Profile, opts.Thresholds, []domain.BlockCoord{c}, 1, opts.PixelBufferPerWorker, 1)
			if err != nil {
				return nil, err
			}
			report.WaitEvents = append(report.WaitEvents, waits...)
			cb := one[block]
			outputs[block] = &BlockOutput{Block: c, Quant: cb.qr, Recon: cb.rr, Source: "incremental_cache_miss_fallback"}
			report.CacheMisses += 2
			continue
		}
		qr := &quant.Result{}
		rr := &recon.BlockResult{}
		if err := json.Unmarshal(qPayload, qr); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(rPayload, rr); err != nil {
			return nil, err
		}
		outputs[block] = &BlockOutput{Block: c, Quant: qr, Recon: rr, Source: "persistent-block-cache-hit", QuantKey: aggKey, ReconKey: reconKey}
		report.CacheHits += 2
	}

	committed := 0
	for _, c := range coords {
		block := c.String()
		out := outputs[block]
		qr, rr := out.Quant, out.Recon
		quantKey := graph.QuantAggregateKey(g, block)
		reconKey := graph.ReconCacheKey(g, block)
		maxQ := int64(0)
		for _, br := range qr.Bands {
			if br.ErrorBoundQ > maxQ {
				maxQ = br.ErrorBoundQ
			}
		}
		qRec := checkpoint.StageRecord{RunID: opts.RunID, Block: block, Stage: checkpoint.StageQuant, OutputDigest: qr.QuantHash, SelectedMethod: string(qr.Decision.Method), ErrorBoundBP: maxQ, CacheKey: quantKey, BandCacheKeys: graph.BandCacheKeyMap(g, block), ContentHash: qr.InputHash}
		if _, err := store.CommitStage(qRec, qr); err != nil {
			return nil, err
		}
		maxC := int64(0)
		for _, b := range rr.ErrorBoundBP {
			if b > maxC {
				maxC = b
			}
		}
		rRec := checkpoint.StageRecord{RunID: opts.RunID, Block: block, Stage: checkpoint.StageRecon, OutputDigest: rr.ReconHash, SelectedMethod: string(rr.Method), ErrorBoundBP: maxC, CacheKey: reconKey, ContentHash: qr.InputHash}
		if _, err := store.CommitStage(rRec, rr); err != nil {
			return nil, err
		}
		committed += 2
		if opts.CrashAfterStages > 0 && committed >= opts.CrashAfterStages {
			return nil, &CrashError{AfterStages: opts.CrashAfterStages}
		}
		out.QuantKey, out.ReconKey = quantKey, reconKey
		report.Blocks = append(report.Blocks, out)
		report.LogicalSequence = append(report.LogicalSequence, block+"/quant", block+"/recon")
	}
	g.ClearDirty()
	txs := store.Transactions()
	for _, tx := range txs {
		if tx.Record.RunID == opts.RunID {
			report.Records = append(report.Records, tx.Record)
		}
	}
	return report, nil
}
