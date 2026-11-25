package maskqc

import (
	"context"
	"fmt"
	"image"
	"sort"
)

// Params are output-affecting raster/topology parameters plus scheduler budget.
type Params struct {
	ChunkSize      int               `json:"chunk_size"`
	AlphaThreshold uint8             `json:"alpha_threshold"`
	Tolerance      TopologyTolerance `json:"tolerance"`
	Scheduler      SchedulerOptions  `json:"scheduler"`
}

func DefaultParams() Params {
	return Params{
		ChunkSize:      16,
		AlphaThreshold: 128,
		Tolerance:      DefaultTopologyTolerance(),
		Scheduler:      DefaultSchedulerOptions(),
	}
}

func (p Params) normalized() Params {
	if p.ChunkSize <= 0 {
		p.ChunkSize = 16
	}
	if p.AlphaThreshold == 0 {
		p.AlphaThreshold = 128
	}
	if p.Tolerance == (TopologyTolerance{}) {
		p.Tolerance = DefaultTopologyTolerance()
	}
	p.Scheduler = p.Scheduler.normalized(0)
	return p
}

// ParamsKey hashes only parameters that can change pixels or topology.
func (p Params) ParamsKey() string {
	p = p.normalized()
	return hashJSON("params:"+AlgorithmVersion, map[string]any{
		"chunk_size":      p.ChunkSize,
		"alpha_threshold": p.AlphaThreshold,
		"tolerance":       p.Tolerance,
		"algorithm":       AlgorithmVersion,
	})
}

// PipelineResult is the common output compared across execution paths.
type PipelineResult struct {
	Path           string              `json:"path"`
	Params         Params              `json:"params"`
	Mask           *image.Gray         `json:"-"`
	Chunks         []ChunkResult       `json:"chunks"`
	OrderedStrokes []Stroke            `json:"ordered_strokes"`
	OrderTrail     []TransformRecord   `json:"order_trail"`
	OrderVersion   string              `json:"order_version"`
	Topology       []DomainMetric      `json:"topology"`
	TopologyError  *Error              `json:"topology_error,omitempty"`
	Graph          *Graph              `json:"graph,omitempty"`
	StrokeCommits  []ComponentRecord   `json:"stroke_commits"`
	WaitRecords    []WaitRecord        `json:"wait_records,omitempty"`
	Invalidation   *InvalidationReport `json:"invalidation,omitempty"`
	RootHash       string              `json:"root_hash"`
	Identity       SnapshotIdentity    `json:"identity"`
}

// ChunkGrid returns deterministic tile coordinates in row-major order.
func ChunkGrid(width, height, size int) []ChunkCoord {
	if size <= 0 {
		size = 16
	}
	var coords []ChunkCoord
	for ty := 0; ty*size < height; ty++ {
		for tx := 0; tx*size < width; tx++ {
			coords = append(coords, ChunkCoord{X: tx, Y: ty})
		}
	}
	return coords
}

// ChunkRect returns the clipped pixel rectangle for a tile.
func ChunkRect(c ChunkCoord, width, height, size int) Rect {
	return Rect{
		MinX: c.X * size,
		MinY: c.Y * size,
		MaxX: minInt((c.X+1)*size, width),
		MaxY: minInt((c.Y+1)*size, height),
	}
}

// ChunksForRect returns row-major chunk coords intersecting r.
func ChunksForRect(r Rect, width, height, size int) []ChunkCoord {
	if size <= 0 {
		size = 16
	}
	minTX := maxInt(0, r.MinX/size)
	maxTX := minInt((width-1)/size, (r.MaxX-1)/size)
	minTY := maxInt(0, r.MinY/size)
	maxTY := minInt((height-1)/size, (r.MaxY-1)/size)
	var out []ChunkCoord
	for ty := minTY; ty <= maxTY; ty++ {
		for tx := minTX; tx <= maxTX; tx++ {
			c := ChunkCoord{X: tx, Y: ty}
			if !ChunkRect(c, width, height, size).Intersect(r).Empty() {
				out = append(out, c)
			}
		}
	}
	return out
}

// ProjectContentHash hashes candidate pixels and all construction fields.
func ProjectContentHash(p *Project) string {
	candidate := []byte(nil)
	if p.Candidate != nil {
		candidate = p.Candidate.Pix
	}
	return hashJSON("project-content-v1", map[string]any{
		"id":        p.ID,
		"seed":      p.Seed,
		"width":     p.Width,
		"height":    p.Height,
		"candidate": hashBytes(candidate),
		"strokes":   p.Strokes,
		"channels":  p.ChannelCoverage,
	})
}

// CompositionOrderVersion returns a content hash for adjudicated order.
func CompositionOrderVersion(ordered []Stroke) string {
	type item struct {
		Seq      int `json:"seq"`
		Priority int `json:"priority"`
	}
	items := make([]item, 0, len(ordered))
	for _, s := range ordered {
		items = append(items, item{Seq: s.Seq, Priority: s.OrderPriority})
	}
	return hashJSON("composition-order-v1:"+OrderRulesVersion, items)
}

// RunPipeline executes rasterization, checkpointing, chunk compose and topology.
func RunPipeline(ctx context.Context, p *Project, params Params, pathName string, sink CheckpointSink) (*PipelineResult, error) {
	return RunPipelineWithRecords(ctx, p, params, pathName, sink, nil)
}

// RunPipelineWithRecords reuses durable stroke components before recomputing.
func RunPipelineWithRecords(ctx context.Context, p *Project, params Params, pathName string, sink CheckpointSink, existing []ComponentRecord) (*PipelineResult, error) {
	if p == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "nil project"}
	}
	params = params.normalized()
	if p.Candidate == nil {
		p.Candidate = image.NewGray(image.Rect(0, 0, p.Width, p.Height))
	}
	ordered, orderTrail := AdjudicateOrder(p.Strokes)
	orderVersion := CompositionOrderVersion(ordered)
	paramsKey := params.ParamsKey()
	rootHash := ProjectContentHash(p)
	coords := ChunkGrid(p.Width, p.Height, params.ChunkSize)

	componentByChunk := map[string]ComponentRecord{}
	allCommits := []ComponentRecord{}
	waits := []WaitRecord{}

	for orderIndex, stroke := range ordered {
		if len(existing) > 0 {
			reused := recordsForStroke(existing, stroke.Seq)
			if len(reused) > 0 {
				for _, rec := range reused {
					rec.OrderIndex = orderIndex
					componentByChunk[strokeChunkKey(rec.StrokeSeq, rec.Chunk)] = rec
					allCommits = append(allCommits, rec)
				}
				continue
			}
		}
		bounds := StrokeBounds(stroke, p.Width, p.Height)
		affected := ChunksForRect(bounds, p.Width, p.Height, params.ChunkSize)
		jobs := make([]Job, 0, len(affected))
		rendered := make([][]byte, len(affected))
		regions := make([]Rect, len(affected))
		for ji, coord := range affected {
			region := ChunkRect(coord, p.Width, p.Height, params.ChunkSize).Intersect(bounds)
			regions[ji] = region
			ji := ji
			jobs = append(jobs, Job{
				ID:     fmt.Sprintf("raster/%d/%s", stroke.Seq, coord),
				Chunk:  coord,
				Pixels: region.Width() * region.Height(),
				Run: func(buffer []byte) error {
					if err := RasterizeStrokeInto(stroke, region, buffer[:region.Width()*region.Height()]); err != nil {
						return err
					}
					rendered[ji] = append([]byte(nil), buffer[:region.Width()*region.Height()]...)
					return nil
				},
			})
		}
		results := RunJobs(ctx, jobs, params.Scheduler)
		for _, jr := range results {
			waits = append(waits, jr.Waits...)
		}
		if err := AllJobsFailed(results); err != nil {
			return nil, err
		}

		components := make([]ComponentRecord, 0, len(affected))
		_, residual, _ := ConvertScale(stroke.WeightPermille, WeightMaxInternal, WeightMaxV2, fmt.Sprintf("stroke/%d/weight", stroke.Seq))
		residualDigest := ResidualDigest(stroke.Seq, "weight->v2", residual)
		for ji, coord := range affected {
			alpha := rendered[ji]
			contentHash := hashBytes([]byte(fmt.Sprintf("component:%s:%d:%s", AlgorithmVersion, stroke.Seq, coord.String())), alpha)
			cacheKey := hashJSON("component-cache-key-v1", map[string]any{
				"project": p.ID, "stroke": stroke.Seq, "chunk": coord,
				"bounds": regions[ji], "content": contentHash,
				"params": paramsKey, "residual": residualDigest,
				"algorithm": AlgorithmVersion,
			})
			rec := ComponentRecord{
				ProjectID: p.ID, OrderIndex: orderIndex, StrokeSeq: stroke.Seq,
				Chunk: coord, Bounds: regions[ji], Alpha: alpha,
				CacheKey: cacheKey, ContentHash: contentHash,
				ResidualDigest: residualDigest, OrderVersion: OrderRulesVersion,
				AlgorithmVersion: AlgorithmVersion,
			}
			components = append(components, rec)
			componentByChunk[strokeChunkKey(stroke.Seq, coord)] = rec
		}
		cp := StrokeCheckpoint{
			ProjectID: p.ID, SchemaVersion: p.SourceVersion, OrderIndex: orderIndex, StrokeSeq: stroke.Seq,
			RootContentHash: rootHash, OrderVersion: OrderRulesVersion, ParamsVersion: paramsKey,
			Components: components,
		}
		if sink != nil {
			if _, err := sink.CommitStrokeCheckpoint(ctx, cp); err != nil {
				return nil, err
			}
		}
		allCommits = append(allCommits, components...)
	}

	chunkResults := make([]ChunkResult, len(coords))
	chunkAlphas := make([][]byte, len(coords))
	jobs := make([]Job, 0, len(coords))
	for i, coord := range coords {
		i, coord := i, coord
		rect := ChunkRect(coord, p.Width, p.Height, params.ChunkSize)
		pixels := rect.Width() * rect.Height()
		jobs = append(jobs, Job{
			ID: fmt.Sprintf("compose/%s", coord), Chunk: coord, Pixels: pixels,
			Run: func(buffer []byte) error {
				alpha := make([]byte, pixels)
				for y := rect.MinY; y < rect.MaxY; y++ {
					srcStart := y*p.Width + rect.MinX
					dstStart := (y - rect.MinY) * rect.Width()
					copy(alpha[dstStart:dstStart+rect.Width()], p.Candidate.Pix[srcStart:srcStart+rect.Width()])
				}
				chunkRect := rect
				seqs := []int{}
				upstream := []string{}
				for _, stroke := range ordered {
					key := strokeChunkKey(stroke.Seq, coord)
					rec, ok := componentByChunk[key]
					if !ok {
						continue
					}
					seqs = append(seqs, stroke.Seq)
					upstream = append(upstream, rec.CacheKey)
					ComposeRegionOver(alpha, chunkRect, rec.Bounds, rec.Alpha)
				}
				chunkAlphas[i] = alpha
				inputHash := hashBytes(p.Candidate.Pix[rect.MinY*p.Width+rect.MinX : rect.MinY*p.Width+rect.MinX+pixels])
				localOrderKey := hashJSON("chunk-order-v1", map[string]any{"rules": OrderRulesVersion, "seqs": seqs})
				cacheKey := hashJSON("chunk-cache-key-v1", map[string]any{
					"input": inputHash, "chunk": coord, "seqs": seqs,
					"order_version": OrderRulesVersion, "order_key": localOrderKey,
					"params": paramsKey, "upstream": upstream,
				})
				chunkResults[i] = ChunkResult{
					Coord: coord, Bounds: rect, InputHash: inputHash,
					OrderKey: localOrderKey, ParamsKey: paramsKey,
					UpstreamKeys: upstream, CacheKey: cacheKey,
					OutputHash: hashBytes([]byte("chunk-output"), alpha),
				}
				return nil
			},
		})
	}
	composeResults := RunJobs(ctx, jobs, params.Scheduler)
	for _, jr := range composeResults {
		waits = append(waits, jr.Waits...)
	}
	if err := AllJobsFailed(composeResults); err != nil {
		return nil, err
	}
	for i := range chunkResults {
		chunkResults[i].Alpha = chunkAlphas[i]
	}

	mask := image.NewGray(image.Rect(0, 0, p.Width, p.Height))
	for i, cr := range chunkResults {
		rect := cr.Bounds
		for y := rect.MinY; y < rect.MaxY; y++ {
			dst := y*p.Width + rect.MinX
			src := (y - rect.MinY) * rect.Width()
			copy(mask.Pix[dst:dst+rect.Width()], chunkAlphas[i][src:src+rect.Width()])
		}
	}

	topo, err := AnalyzeGrayMask(mask, params.AlphaThreshold)
	if err != nil {
		return nil, &Error{Code: CodeState, Field: "mask", Message: err.Error()}
	}
	var topoErr *Error
	if p.Truth != nil {
		topoErr = VerifyTopology(topo, p.Truth.Domains, params.Tolerance)
	}
	graph := BuildGraph(p, coords, ordered, chunkResults, topo)
	identity := SnapshotIdentity{
		ProjectID: p.ID, RootContentHash: rootHash, OrderVersion: OrderRulesVersion,
		ParamsVersion: paramsKey, AlgorithmVersion: AlgorithmVersion,
		SchemaVersion: p.SourceVersion,
	}
	result := &PipelineResult{
		Path: pathName, Params: params, Mask: mask, Chunks: chunkResults,
		OrderedStrokes: ordered, OrderTrail: orderTrail, OrderVersion: orderVersion,
		Topology: topo, TopologyError: topoErr, Graph: graph,
		StrokeCommits: allCommits, WaitRecords: waits, RootHash: rootHash, Identity: identity,
	}
	if topoErr != nil {
		return result, topoErr
	}
	return result, nil
}

// BuildGraph explicitly links strokes, order, chunks and topology checks.
func BuildGraph(p *Project, coords []ChunkCoord, ordered []Stroke, chunks []ChunkResult, domains []DomainMetric) *Graph {
	g := &Graph{}
	g.Nodes = append(g.Nodes, GraphNode{ID: "order:adjudication", Kind: "order", Label: "priority then seq", Metadata: map[string]any{"rules": OrderRulesVersion, "order": strokeSeqs(ordered)}})
	for _, s := range ordered {
		g.Nodes = append(g.Nodes, GraphNode{ID: fmt.Sprintf("stroke:%d", s.Seq), Kind: "stroke", Label: fmt.Sprintf("stroke %d", s.Seq), Metadata: map[string]any{"priority": s.OrderPriority}})
		g.Edges = append(g.Edges, GraphEdge{From: "order:adjudication", To: fmt.Sprintf("stroke:%d", s.Seq), Reason: "adjudicated position", StrokeSeq: s.Seq})
		affected := ChunksForRect(StrokeBounds(s, p.Width, p.Height), p.Width, p.Height, 16)
		for _, c := range affected {
			id := chunkNodeID(c)
			g.Edges = append(g.Edges, GraphEdge{From: fmt.Sprintf("stroke:%d", s.Seq), To: id, Reason: "raster coverage dependency", StrokeSeq: s.Seq})
		}
	}
	for _, c := range coords {
		g.Nodes = append(g.Nodes, GraphNode{ID: chunkNodeID(c), Kind: "chunk", Label: "chunk " + c.String(), Metadata: map[string]any{"coord": c}})
	}
	for _, d := range domains {
		id := fmt.Sprintf("topology:%d", d.DomainID)
		g.Nodes = append(g.Nodes, GraphNode{ID: id, Kind: "topology", Label: fmt.Sprintf("domain %d verification", d.DomainID), Metadata: map[string]any{"holes": d.Holes, "angle": d.AngleDeg}})
		for _, c := range coords {
			cr := ChunkRect(c, p.Width, p.Height, 16)
			if !cr.Intersect(d.Bounds).Empty() {
				g.Edges = append(g.Edges, GraphEdge{From: chunkNodeID(c), To: id, Reason: "chunk pixels feed topology domain"})
			}
		}
	}
	return g
}

func chunkNodeID(c ChunkCoord) string { return fmt.Sprintf("chunk:%d:%d", c.X, c.Y) }

func strokeChunkKey(seq int, c ChunkCoord) string {
	return fmt.Sprintf("%d@%d,%d", seq, c.X, c.Y)
}

func recordsForStroke(records []ComponentRecord, seq int) []ComponentRecord {
	var out []ComponentRecord
	for _, rec := range records {
		if rec.StrokeSeq == seq {
			out = append(out, rec)
		}
	}
	return out
}

func strokeSeqs(strokes []Stroke) []int {
	out := make([]int, 0, len(strokes))
	for _, s := range strokes {
		out = append(out, s.Seq)
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// SortedChunkResults returns chunks in stable tile order.
func SortedChunkResults(chunks []ChunkResult) []ChunkResult {
	out := append([]ChunkResult(nil), chunks...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Coord.Y != out[j].Coord.Y {
			return out[i].Coord.Y < out[j].Coord.Y
		}
		return out[i].Coord.X < out[j].Coord.X
	})
	return out
}
