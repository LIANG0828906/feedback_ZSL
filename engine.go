package maskqa

import (
	"fmt"
	"image"
	"image/color"
)

// Edit is a supported precise mutation.
type Edit struct {
	StrokeID        string `json:"stroke_id"`
	Start           *Point `json:"start,omitempty"`
	End             *Point `json:"end,omitempty"`
	WeightPermyriad *int   `json:"weight_permyriad,omitempty"`
	Priority        *int   `json:"priority,omitempty"`
}

// TileComputation is the result of one chunk.
type TileComputation struct {
	TileResult
	records []StrokeCommitRecord
}

// TileResult contains cache and participation metadata for one chunk.
type TileResult struct {
	Coord         TileCoord `json:"coord"`
	CacheKey      string    `json:"cache_key"`
	InputHash     string    `json:"input_hash"`
	ContentHash   string    `json:"content_hash"`
	StrokeIDs     []string  `json:"stroke_ids"`
	OrderBasis    []string  `json:"order_basis"`
	Pixels        []byte    `json:"-"`
	Reused        bool      `json:"reused"`
	StrokeCommits int       `json:"stroke_commits"`
}

// CommitIdentity is the stable logical chunk commit sequence entry.
type CommitIdentity struct {
	LogicalSequence int       `json:"logical_sequence"`
	Tile            TileCoord `json:"tile"`
	CacheKey        string    `json:"cache_key"`
	ContentHash     string    `json:"content_hash"`
	StrokeCommits   int       `json:"stroke_commits"`
}

// ComputeResult is the complete mask, tile and verification output.
type ComputeResult struct {
	ProjectID       string              `json:"project_id"`
	Width           int                 `json:"width"`
	Height          int                 `json:"height"`
	TileSize        int                 `json:"tile_size"`
	Accumulated     *image.Gray         `json:"-"`
	Tiles           []TileResult        `json:"tiles"`
	CommitSequence  []CommitIdentity    `json:"commit_sequence"`
	OrderedStrokes  []string            `json:"ordered_strokes"`
	Report          *VerificationReport `json:"report,omitempty"`
	Budget          BudgetMetrics       `json:"budget"`
	RecomputedTiles []TileCoord         `json:"recomputed_tiles"`
	ReusedTiles     []TileCoord         `json:"reused_tiles"`
	Invalidation    *InvalidationPlan   `json:"invalidation,omitempty"`
	FirstError      *Error              `json:"first_error,omitempty"`
}

// ComputeFull recomputes every chunk and optionally journals every stroke.
func ComputeFull(project *Project, options SchedulerOptions, store *CheckpointStore) (*ComputeResult, error) {
	return computeTiles(project, options, store, nil, nil)
}

func computeTiles(
	project *Project,
	options SchedulerOptions,
	store *CheckpointStore,
	invalid map[TileCoord]bool,
	previous []TileResult,
) (*ComputeResult, error) {
	if project == nil {
		return nil, decodeError("project", "nil project")
	}
	ordered := OrderStrokes(project.Strokes)
	for i, stroke := range project.Strokes {
		if err := stroke.ValidateParameters(fmt.Sprintf("strokes[%d]", i)); err != nil {
			return nil, err
		}
	}
	tiles := Tiles(project.Width, project.Height, project.tileSize())
	previousByCoord := map[TileCoord]TileResult{}
	for _, tile := range previous {
		previousByCoord[tile.Coord] = tile
	}

	computations := make([]*TileComputation, len(tiles))
	jobs := make([]Job, 0, len(tiles))
	jobIndexes := make([]int, 0, len(tiles))
	for i, tile := range tiles {
		if invalid != nil && !invalid[tile.Coord] {
			if prior, ok := previousByCoord[tile.Coord]; ok && len(prior.Pixels) == tile.PixelCount {
				prior.Reused = true
				computations[i] = &TileComputation{TileResult: prior}
				continue
			}
			// If persistent prior is unavailable, fall through and recompute.
		}
		idx := i
		tile := tile
		jobs = append(jobs, Job{
			ID:     "tile:" + tile.Coord.String(),
			Pixels: tile.PixelCount,
			Run: func(buffer []byte) error {
				computation, err := computeTile(project, tile, ordered, buffer)
				if err != nil {
					return err
				}
				computations[idx] = computation
				return nil
			},
		})
		jobIndexes = append(jobIndexes, i)
	}

	scheduler := NewScheduler(options)
	jobResults := scheduler.Run(jobs)
	var firstError *Error
	for _, result := range jobResults {
		if result.Err == nil {
			continue
		}
		if business, ok := AsError(result.Err); ok && firstError == nil {
			firstError = business
		} else if firstError == nil {
			firstError = &Error{Code: "E_WORKER", Field: result.ID, Message: result.Err.Error()}
		}
	}
	if firstError != nil {
		return nil, firstError
	}

	result := &ComputeResult{
		ProjectID: project.ID, Width: project.Width, Height: project.Height,
		TileSize: project.tileSize(), Budget: scheduler.Snapshot(),
		OrderedStrokes: make([]string, 0, len(ordered)),
	}
	for _, stroke := range ordered {
		result.OrderedStrokes = append(result.OrderedStrokes, stroke.ID)
	}

	mask := image.NewGray(image.Rect(0, 0, project.Width, project.Height))
	for i, tile := range tiles {
		computation := computations[i]
		if computation == nil {
			return nil, &Error{Code: "E_WORKER", Field: "tile:" + tile.Coord.String(), Message: "tile produced no result"}
		}
		tileResult := computation.TileResult
		result.Tiles = append(result.Tiles, tileResult)
		if tileResult.Reused {
			result.ReusedTiles = append(result.ReusedTiles, tile.Coord)
		} else {
			result.RecomputedTiles = append(result.RecomputedTiles, tile.Coord)
		}
		copyTileToMask(mask, tile, tileResult.Pixels)

		if store != nil && !tileResult.Reused {
			for recordIndex := range computation.records {
				committed, err := store.CommitStroke(computation.records[recordIndex])
				if err != nil {
					return nil, err
				}
				computation.records[recordIndex] = committed
			}
		}
		result.CommitSequence = append(result.CommitSequence, CommitIdentity{
			LogicalSequence: i + 1, Tile: tile.Coord,
			CacheKey: tileResult.CacheKey, ContentHash: tileResult.ContentHash,
			StrokeCommits: tileResult.StrokeCommits,
		})
	}
	result.Accumulated = mask
	report, verifyErr := VerifyMask(project, mask)
	result.Report = report
	if verifyErr != nil {
		if business, ok := AsError(verifyErr); ok {
			result.FirstError = business
		}
		return result, verifyErr
	}
	return result, nil
}

func computeTile(project *Project, tile Tile, ordered []OrderedStroke, componentBuffer []byte) (*TileComputation, error) {
	acc := make([]byte, tile.PixelCount)
	tileWidth := tile.Rect.Width()
	for y := tile.MinY; y < tile.MaxY; y++ {
		for x := tile.MinX; x < tile.MaxX; x++ {
			local := (y-tile.MinY)*tileWidth + (x - tile.MinX)
			acc[local] = project.Candidate.GrayAt(x, y).Y
		}
	}
	inputHash := ContentDigest(acc)
	upstreamKey := "genesis"
	strokeIDs := []string{}
	basis := []string{}
	records := []StrokeCommitRecord{}
	participating := 0

	for _, orderedStroke := range ordered {
		stroke := orderedStroke.Stroke
		if StrokeBounds(stroke).Intersect(tile.Rect).Empty() {
			continue
		}
		if err := RasterizeStrokeTile(stroke, tile, componentBuffer); err != nil {
			return nil, err
		}
		componentCopy := append([]byte(nil), componentBuffer[:tile.PixelCount]...)
		paramsHash := ContentDigest([]byte(fmt.Sprintf(
			"id=%s seq=%d priority=%d tile_order=%d start=%d,%d end=%d,%d width=%d hardness=%d weight=%d",
			stroke.ID, stroke.Seq, stroke.Priority, participating,
			stroke.Start.X, stroke.Start.Y, stroke.End.X, stroke.End.Y,
			stroke.WidthPx, stroke.HardnessPermyriad, stroke.WeightPermyriad,
		)))
		cacheKey := ContentDigest(
			[]byte("tile="+tile.Coord.String()),
			[]byte("input_hash="+inputHash),
			[]byte("stroke_id="+stroke.ID),
			[]byte("stroke_params_hash="+paramsHash),
			[]byte("order_version="+OrderVersion),
			[]byte("conversion_version="+ConversionVersion),
			[]byte("upstream_key="+upstreamKey),
		)
		CompositeTileOver(acc, componentCopy)
		upstreamKey = cacheKey
		strokeIDs = append(strokeIDs, stroke.ID)
		basis = append(basis, orderedStroke.Reason)
		participating++
		records = append(records, StrokeCommitRecord{
			ProjectID: project.ID, StrokeID: stroke.ID, StrokeSeq: stroke.Seq,
			OrderIndex: orderedStroke.OrderIndex, TileX: tile.Coord.TX, TileY: tile.Coord.TY,
			Component: componentCopy, TileAfter: append([]byte(nil), acc...),
			CacheKey: cacheKey, ContentHash: ContentDigest(componentCopy),
			InputHash: inputHash, ResidualDigest: strokeResidualDigest(stroke),
			OrderVersion: OrderVersion, ConversionVersion: ConversionVersion,
			AlgorithmVersion: AlgorithmVersion,
		})
	}

	finalKey := ContentDigest(
		[]byte("tile_final"), []byte("tile="+tile.Coord.String()),
		[]byte("input_hash="+inputHash), []byte("last_upstream_key="+upstreamKey),
		[]byte("order_version="+OrderVersion),
		[]byte("conversion_version="+ConversionVersion),
	)
	for i := range records {
		records[i].TileCacheKey = finalKey
	}
	return &TileComputation{
		TileResult: TileResult{
			Coord: tile.Coord, CacheKey: finalKey, InputHash: inputHash,
			ContentHash: ContentDigest(acc), StrokeIDs: strokeIDs, OrderBasis: basis,
			Pixels: acc, Reused: false, StrokeCommits: participating,
		},
		records: records,
	}, nil
}

func strokeResidualDigest(stroke Stroke) string {
	records := []TruncationRecord{}
	_, _, _ = ScalePermyriadToPermille(int64(stroke.WeightPermyriad),
		"weight_permyriad", fmt.Sprintf("stroke:%s.weight", stroke.ID), &records)
	originX, _ := SequenceOrigin(stroke.Seq, 0, 100000)
	originY, _ := SequenceOrigin(stroke.Seq, 1, 100000)
	records = append(records, TruncationRecord{
		Field: "coordinate_offset_origin", Operation: "sequence_origin_integer_mod",
		OperandA: int64(originX), OperandB: int64(originY), Result: int64(stroke.Seq),
		Rounding: "mod_positive_exact",
	})
	return ResidualDigest(records)
}

func copyTileToMask(mask *image.Gray, tile Tile, pixels []byte) {
	tileWidth := tile.Rect.Width()
	for y := tile.MinY; y < tile.MaxY; y++ {
		for x := tile.MinX; x < tile.MaxX; x++ {
			local := (y-tile.MinY)*tileWidth + (x - tile.MinX)
			mask.SetGray(x, y, color.Gray{Y: pixels[local]})
		}
	}
}
