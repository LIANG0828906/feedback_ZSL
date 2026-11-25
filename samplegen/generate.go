package samplegen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"soundlab/audiofp"
)

const schemaVersion = 1

type markerState struct {
	id     string
	asset  string
	sample int
	desc   string
}

func Generate(outDir string, cfg Config) (*Index, error) {
	cfg = cfg.withDefaults()
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	index := &Index{SchemaVersion: schemaVersion, Seed: cfg.Seed}
	for batch := 0; batch < cfg.Batches; batch++ {
		summary, err := generateBatch(outDir, cfg, batch)
		if err != nil {
			return nil, err
		}
		index.Batches = append(index.Batches, *summary)
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "index.json"), append(data, '\n'), 0o644); err != nil {
		return nil, err
	}
	return index, nil
}

func batchRecipe(batch int) audiofp.Recipe {
	r := audiofp.Recipe{
		FrameLen: 256, FrameShift: 128, Bands: 8, QuantBits: 4,
		SimilarityThreshold: 0.55, CandidateLimit: 12,
	}
	switch batch % 4 {
	case 1:
		r.FrameLen, r.FrameShift, r.Bands = 128, 64, 8
		r.SimilarityThreshold = 0.52
	case 2:
		r.FrameLen, r.FrameShift, r.Bands = 512, 256, 12
		r.SimilarityThreshold = 0.58
	}
	return r
}

func generateBatch(root string, cfg Config, batch int) (*BatchSummary, error) {
	batchID := fmt.Sprintf("batch-%03d", batch)
	base := filepath.Join(root, batchID)
	if err := os.RemoveAll(base); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(base, "assets"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(base, "segments"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(base, "queries"), 0o755); err != nil {
		return nil, err
	}
	initialRecipe := batchRecipe(batch)
	sampleRate := 8000
	baseSeed := cfg.Seed + int64(batch+1)*100003

	var finalAssets []audiofp.PCMAsset
	var manifest audiofp.Manifest
	var initialSpecs []audiofp.AssetSpec
	var pcmBytes int64

	for attempt := 0; attempt < 40; attempt++ {
		attemptSeed := baseSeed + int64(attempt)*7919
		initial := make([]audiofp.PCMAsset, 0, cfg.AssetsPerBatch)
		for a := 0; a < cfg.AssetsPerBatch; a++ {
			total := 12000 + batch*320 + a*480
			markerBlocks := map[int]bool{5 + a: true, 19 + a: true}
			silentTail := 0
			if a == cfg.AssetsPerBatch-1 {
				silentTail = initialRecipe.FrameLen + 2*initialRecipe.FrameShift
			}
			assetRNG := rand.New(rand.NewSource(attemptSeed + int64(a+1)*104729))
			asset := synthAsset(fmt.Sprintf("b%03d-a%d", batch, a), sampleRate, total,
				initialRecipe, assetRNG, markerBlocks, silentTail)
			initial = append(initial, asset)
		}
		replacements := make(map[int]audiofp.PCMAsset)
		replSpecs := make(map[int]audiofp.AssetSpec)
		edits, err := buildEdits(batch, initial, initialRecipe, attemptSeed, sampleRate, replacements, replSpecs)
		if err != nil {
			return nil, err
		}
		assetMap := make(map[string]audiofp.PCMAsset, len(initial))
		for _, a := range initial {
			assetMap[a.ID] = a.Clone()
		}
		recipe := initialRecipe
		for _, op := range edits {
			if _, err := audiofp.ApplyEdit(assetMap, replacements, &recipe, op); err != nil {
				return nil, err
			}
		}
		final := make([]audiofp.PCMAsset, 0, len(assetMap))
		for _, a := range initial {
			final = append(final, assetMap[a.ID])
		}
		truthAssets := make([]audiofp.AssetTruth, 0, len(final))
		minMargin := 1.0
		for _, a := range final {
			ref := referenceFrames(a, recipe)
			if ref.minMargin < minMargin {
				minMargin = ref.minMargin
			}
			truthAssets = append(truthAssets, audiofp.AssetTruth{
				AssetID: a.ID, ContentHash: a.ContentHash(),
				TotalSamples: len(a.Samples), Frames: ref.frames,
			})
		}
		if minMargin < 2e-6 {
			continue
		}

		finalMarkers := transformMarkers(markersFor(initial, initialRecipe), edits)
		queries, err := buildQueries(base, batch, final, truthAssets, recipe, attemptSeed)
		if err != nil {
			return nil, err
		}
		if len(queries) == 0 {
			return nil, fmt.Errorf("batch %s: no valid query could be constructed", batchID)
		}
		finalAssets = final
		initialSpecs = make([]audiofp.AssetSpec, 0, len(initial))
		for _, a := range initial {
			data := audiofp.EncodePCM(a.Samples)
			rel := filepath.ToSlash(filepath.Join("assets", a.ID+".pcm"))
			if err := os.WriteFile(filepath.Join(base, rel), data, 0o644); err != nil {
				return nil, err
			}
			sum := sha256.Sum256(data)
			initialSpecs = append(initialSpecs, audiofp.AssetSpec{
				ID: a.ID, File: rel, SampleRate: a.SampleRate, TotalSamples: len(a.Samples),
				Channels: 1, SampleFormat: "s16le", SHA256: hex.EncodeToString(sum[:]),
			})
			pcmBytes += int64(len(data))
		}
		for seq, repl := range replacements {
			spec := replSpecs[seq]
			data := audiofp.EncodePCM(repl.Samples)
			rel := filepath.ToSlash(filepath.Join("segments", spec.File))
			if err := os.WriteFile(filepath.Join(base, rel), data, 0o644); err != nil {
				return nil, err
			}
			pcmBytes += int64(len(data))
		}
		manifest = audiofp.Manifest{
			SchemaVersion: schemaVersion, Seed: cfg.Seed, BatchID: batchID,
			Assets: initialSpecs, Recipe: initialRecipe, Edits: edits,
			Truth: audiofp.Truth{FinalRecipe: recipe, Assets: truthAssets, Markers: markersToTruth(finalMarkers, recipe), Queries: queries},
		}
		break
	}
	if finalAssets == nil {
		return nil, fmt.Errorf("batch %s: could not synthesize quantization-stable fixtures", batchID)
	}
	projectPath := filepath.Join(base, "project.json")
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(projectPath, append(data, '\n'), 0o644); err != nil {
		return nil, err
	}
	frameCount := 0
	for _, a := range manifest.Truth.Assets {
		frameCount += len(a.Frames)
	}
	return &BatchSummary{
		BatchID: batchID, Manifest: filepath.ToSlash(filepath.Join(batchID, "project.json")),
		AssetCount: len(manifest.Assets), EditCount: len(manifest.Edits),
		QueryCount: len(manifest.Truth.Queries), FrameCount: frameCount,
		PCMBytes: pcmBytes, FinalRecipe: manifest.Truth.FinalRecipe,
	}, nil
}

func buildEdits(batch int, initial []audiofp.PCMAsset, r audiofp.Recipe, attemptSeed int64,
	sampleRate int, replacements map[int]audiofp.PCMAsset, specs map[int]audiofp.AssetSpec) ([]audiofp.EditOp, error) {
	rng := rand.New(rand.NewSource(attemptSeed + 17))
	var edits []audiofp.EditOp
	logical := int64(100)
	seq := 0
	addTrim := func(assetID string, start, end int) {
		edits = append(edits, audiofp.EditOp{
			Seq: seq, LogicalTime: logical, Kind: "trim", AssetID: assetID,
			StartSample: start, EndSample: end,
		})
		seq++
		logical += 100
	}
	addTrim(initial[0].ID, 1000, 1400)
	if len(initial) > 1 {
		start, end := 2000, 2400
		repl := synthSegment(end-start, sampleRate, r, rng)
		replSpec := audiofp.AssetSpec{
			ID: initial[1].ID, File: fmt.Sprintf("b%03d-seq%d.pcm", batch, seq),
			SampleRate: sampleRate, TotalSamples: len(repl), Channels: 1,
			SampleFormat: "s16le",
		}
		payload := audiofp.EncodePCM(repl)
		sum := sha256.Sum256(payload)
		replSpec.SHA256 = hex.EncodeToString(sum[:])
		replacements[seq] = audiofp.PCMAsset{ID: initial[1].ID, SampleRate: sampleRate, Samples: repl}
		specs[seq] = replSpec
		edits = append(edits, audiofp.EditOp{
			Seq: seq, LogicalTime: logical, Kind: "replace", AssetID: initial[1].ID,
			StartSample: start, EndSample: end, ReplacementFile: "segments/" + replSpec.File,
			ReplacementSHA256: replSpec.SHA256, ReplacementTotalSamples: len(repl),
		})
		seq++
		logical += 100
	}
	if batch%2 == 1 {
		value := float64(10)
		edits = append(edits, audiofp.EditOp{
			Seq: seq, LogicalTime: logical, Kind: "set_param", Param: "bands", Value: &value,
		})
		seq++
		logical += 100
	}
	if batch%3 == 2 {
		value := float64(64)
		edits = append(edits, audiofp.EditOp{
			Seq: seq, LogicalTime: logical, Kind: "set_param", Param: "frame_shift", Value: &value,
		})
		seq++
		logical += 100
	}
	if len(initial) > 2 && batch%2 == 1 {
		addTrim(initial[2].ID, 600, 900)
	}
	return edits, nil
}

func markersFor(initial []audiofp.PCMAsset, r audiofp.Recipe) []markerState {
	var out []markerState
	for i, asset := range initial {
		if i >= 2 {
			break
		}
		for _, block := range []int{5 + i, 19 + i} {
			sample := block * r.FrameShift
			if sample >= len(asset.Samples) {
				continue
			}
			out = append(out, markerState{
				id: fmt.Sprintf("marker-%d-%d", i, block), asset: asset.ID, sample: sample,
				desc: "high-band transient marker",
			})
		}
	}
	return out
}

func transformMarkers(markers []markerState, edits []audiofp.EditOp) []markerState {
	out := append([]markerState(nil), markers...)
	for _, op := range edits {
		switch op.Kind {
		case "trim":
			next := out[:0]
			for _, m := range out {
				if m.asset != op.AssetID {
					next = append(next, m)
					continue
				}
				if m.sample >= op.EndSample {
					m.sample -= op.EndSample - op.StartSample
					next = append(next, m)
				} else if m.sample < op.StartSample {
					next = append(next, m)
				}
			}
			out = next
		case "replace":
			next := out[:0]
			for _, m := range out {
				if m.asset == op.AssetID && m.sample >= op.StartSample && m.sample < op.EndSample {
					continue
				}
				next = append(next, m)
			}
			out = next
		}
	}
	return out
}

func markersToTruth(markers []markerState, r audiofp.Recipe) []audiofp.MarkerTruth {
	out := make([]audiofp.MarkerTruth, 0, len(markers))
	for _, m := range markers {
		out = append(out, audiofp.MarkerTruth{
			MarkerID: m.id, AssetID: m.asset, Sample: m.sample,
			FrameNo: m.sample / r.FrameShift, Description: m.desc,
		})
	}
	return out
}

func buildQueries(base string, batch int, final []audiofp.PCMAsset, truthAssets []audiofp.AssetTruth,
	r audiofp.Recipe, attemptSeed int64) ([]audiofp.QueryTruth, error) {
	byID := make(map[string]audiofp.AssetTruth, len(truthAssets))
	for _, t := range truthAssets {
		byID[t.AssetID] = t
	}
	var queries []audiofp.QueryTruth
	for assetIndex := 0; assetIndex < len(final); assetIndex++ {
		asset := final[assetIndex]
		at := byID[asset.ID]
		frameCountQuery := 5
		if len(at.Frames) < frameCountQuery+6 {
			continue
		}
		bestStart := -1
		var best []audiofp.RankedTruth
		bestGap := -2.0
		for frameStart := 2; frameStart+frameCountQuery+2 <= len(at.Frames); frameStart += 2 {
			qFrames := at.Frames[frameStart : frameStart+frameCountQuery]
			ranked := rankTruth(qFrames, truthAssets, r, r.SimilarityThreshold, r.CandidateLimit)
			if len(ranked) == 0 || ranked[0].AssetID != asset.ID ||
				ranked[0].StartSample != qFrames[0].StartSample {
				continue
			}
			gap := 1.0
			if len(ranked) > 1 {
				gap = ranked[0].Similarity - ranked[1].Similarity
			}
			if gap > bestGap {
				bestGap = gap
				bestStart = frameStart
				best = ranked
			}
		}
		if bestStart < 0 {
			continue
		}
		qFrames := at.Frames[bestStart : bestStart+frameCountQuery]
		total := (frameCountQuery-1)*r.FrameShift + r.FrameLen
		startSample := qFrames[0].StartSample
		querySamples := append([]int16(nil), asset.Samples[startSample:startSample+total]...)
		payload := audiofp.EncodePCM(querySamples)
		sum := sha256.Sum256(payload)
		queryID := fmt.Sprintf("b%03d-q%d", batch, assetIndex)
		rel := filepath.ToSlash(filepath.Join("queries", queryID+".pcm"))
		if err := os.WriteFile(filepath.Join(base, rel), payload, 0o644); err != nil {
			return nil, err
		}
		queries = append(queries, audiofp.QueryTruth{
			QueryID: queryID, AssetID: asset.ID, File: rel, SampleRate: asset.SampleRate,
			TotalSamples: len(querySamples), SHA256: hex.EncodeToString(sum[:]),
			StartSample: startSample, FrameCount: frameCountQuery,
			Threshold: r.SimilarityThreshold, CandidateLimit: r.CandidateLimit,
			Expected: best,
		})
		if len(queries) >= 2 {
			break
		}
	}
	return queries, nil
}
