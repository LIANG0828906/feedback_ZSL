package iconmask

import (
	"fmt"
	"math"
	"sort"
)

type RepairConfig struct {
	Radius            RadiusConfig `json:"radius"`
	Shape             string       `json:"shape"`
	MaxRounds         int          `json:"maxRounds"`
	BlockSize         int          `json:"blockSize"`
	ConvergencePixels int          `json:"convergencePixels"`
}

func DefaultRepairConfig() RepairConfig {
	return RepairConfig{
		Radius: RadiusConfig{
			Min:            2,
			Max:            4,
			CurvatureBands: 4,
		},
		Shape:             ShapeSquare,
		MaxRounds:         8,
		BlockSize:         32,
		ConvergencePixels: 0,
	}
}

type RepairRoundLog struct {
	Round        int `json:"round"`
	CloseChanged int `json:"closeChangedPixels"`
	OpenChanged  int `json:"openChangedPixels"`
	Changed      int `json:"changedPixels"`
	Residual     int `json:"residualPixels"`
}

type RepairResult struct {
	ImageID        string           `json:"imageId"`
	Mask           Mask             `json:"mask"`
	EdgeBand       Mask             `json:"edgeBand"`
	EdgeEmpty      bool             `json:"edgeEmpty"`
	Status         string           `json:"status"`
	Success        bool             `json:"success"`
	RadiusTable    *RadiusTable     `json:"radiusTable,omitempty"`
	Rounds         []RepairRoundLog `json:"rounds"`
	Converged      bool             `json:"converged"`
	OutsideChanged int              `json:"outsideChangedPixels"`
	Stats          RegionStats      `json:"stats"`
	Accuracy       AccuracyReport   `json:"accuracy"`
	RoundMasks     []Mask           `json:"-"`
	BlockSchedule  *SchedulerReport `json:"blockSchedule,omitempty"`
}

func RefineSample(sample ImageSample, cfg RepairConfig) (*RepairResult, *Error) {
	return Refine(
		sample.ID,
		sample.InitialMask,
		sample.Truth.ReferenceMask,
		sample.Truth.Segments,
		cfg,
	)
}

func Refine(imageID string, initial, reference Mask, segments []BoundarySegment, cfg RepairConfig) (*RepairResult, *Error) {
	if initial.Empty() || !initial.Binary() {
		return nil, schemaError(imageID, "initialMask", "initial mask must be a non-empty binary mask")
	}
	if initial.Width != reference.Width || initial.Height != reference.Height {
		return nil, schemaError(imageID, "initialMask.width", "initial mask and reference mask dimensions conflict")
	}
	if cfg.Shape == "" {
		cfg.Shape = ShapeSquare
	}
	if !legalShape(cfg.Shape) {
		return nil, parameterError(imageID, "shape", "unsupported structuring element shape", LegalShapes...)
	}
	if cfg.MaxRounds <= 0 {
		return nil, parameterError(imageID, "maxRounds", "max rounds must be a positive integer", "positive integer")
	}
	if cfg.BlockSize <= 0 {
		return nil, parameterError(imageID, "blockSize", "block size must be a positive integer", "positive integer")
	}
	if cfg.ConvergencePixels < 0 {
		return nil, parameterError(imageID, "convergencePixels", "convergence pixels must be non-negative", "non-negative integer")
	}
	table, err := ResolveRadii(imageID, segments, cfg.Radius)
	if err != nil {
		return nil, err
	}
	edge, err := LocateEdgeBand(imageID, initial, segments, EdgeConfig{
		Shape:         cfg.Shape,
		RadiusBandMin: cfg.Radius.Min,
	})
	if err != nil {
		return nil, err
	}
	result := &RepairResult{
		ImageID:     imageID,
		Mask:        initial.Clone(),
		EdgeBand:    edge.Band,
		EdgeEmpty:   edge.Empty,
		RadiusTable: table,
		Status:      "ready",
	}
	if edge.Empty {
		result.Status = "empty_edge_band"
		result.Success = false
		result.Stats = AnalyzeMask(result.Mask)
		result.Accuracy = ComputeAccuracy(reference, result.Mask)
		return result, nil
	}
	assignment := assignBandToSegments(edge.Band, segments)
	mask := initial.Clone()
	converged := false
	for round := 0; round < cfg.MaxRounds; round++ {
		roundStart := mask.Clone()
		closeResult := mask.Clone()
		closeChanged, err := applySegmentedMorphology(imageID, closeResult, mask, edge.Band, assignment, segments, table, cfg.Shape, true)
		if err != nil {
			return nil, err
		}
		openResult := closeResult.Clone()
		openChanged, err := applySegmentedMorphology(imageID, openResult, closeResult, edge.Band, assignment, segments, table, cfg.Shape, false)
		if err != nil {
			return nil, err
		}
		changed := roundStart.DiffPixels(openResult)
		logEntry := RepairRoundLog{
			Round:        round,
			CloseChanged: closeChanged,
			OpenChanged:  openChanged,
			Changed:      changed,
			Residual:     changed,
		}
		result.Rounds = append(result.Rounds, logEntry)
		mask = openResult
		result.RoundMasks = append(result.RoundMasks, mask.Clone())
		if changed <= cfg.ConvergencePixels {
			converged = true
			break
		}
	}
	result.Mask = mask
	result.Converged = converged
	result.Success = converged
	result.Status = "converged"
	if !converged {
		last := result.Rounds[len(result.Rounds)-1]
		round := last.Round
		result.Status = "iteration_budget_exhausted"
		result.Stats = AnalyzeMask(result.Mask)
		result.Accuracy = ComputeAccuracy(reference, result.Mask)
		result.OutsideChanged = outsideBandChanges(initial, result.Mask, edge.Band)
		return result, &Error{
			Code:      CodeIteration,
			ImageID:   imageID,
			Field:     "maxRounds",
			Message:   "iteration budget exhausted before hole/overflow convergence",
			Round:     &round,
			Remaining: last.Residual,
			Details: map[string]any{
				"rounds":    len(result.Rounds),
				"maxRounds": cfg.MaxRounds,
			},
		}
	}
	result.OutsideChanged = outsideBandChanges(initial, result.Mask, edge.Band)
	if result.OutsideChanged != 0 {
		result.Status = "outside_band_mutation"
		return result, &Error{
			Code:      CodeState,
			ImageID:   imageID,
			Field:     "edgeBand",
			Message:   "repair changed pixels outside the declared edge band",
			Remaining: result.OutsideChanged,
		}
	}
	result.Stats = AnalyzeMask(result.Mask)
	result.Accuracy = ComputeAccuracy(reference, result.Mask)
	return result, nil
}

func applySegmentedMorphology(
	imageID string,
	out, source, band Mask,
	assignment []int,
	segments []BoundarySegment,
	table *RadiusTable,
	shape string,
	closeOperation bool,
) (int, *Error) {
	byRadius := make(map[int][]int)
	for segmentIndex, segment := range segments {
		entry, ok := table.Lookup(segment.ID)
		if !ok {
			return 0, radiusError(imageID, "radiusTable", "missing radius for segment "+segment.ID, map[string]any{
				"segmentId": segment.ID,
			})
		}
		radius := entry.ErosionRadius
		if !closeOperation {
			radius = entry.DilationRadius
		}
		byRadius[radius] = append(byRadius[radius], segmentIndex)
	}
	radii := make([]int, 0, len(byRadius))
	for radius := range byRadius {
		radii = append(radii, radius)
	}
	sort.Ints(radii)
	changed := 0
	for _, radius := range radii {
		element, err := NewStructuringElement(imageID, shape, radius)
		if err != nil {
			return 0, err
		}
		var morphed Mask
		if closeOperation {
			morphed = Close(source, element)
		} else {
			morphed = Open(source, element)
		}
		group := byRadius[radius]
		inGroup := make(map[int]bool, len(group))
		for _, segmentIndex := range group {
			inGroup[segmentIndex] = true
		}
		for idx := range out.Data {
			if band.Data[idx] == 0 || !inGroup[assignment[idx]] {
				continue
			}
			value := morphed.Data[idx]
			if out.Data[idx] != value {
				out.Data[idx] = value
				changed++
			}
		}
	}
	return changed, nil
}

func assignBandToSegments(band Mask, segments []BoundarySegment) []int {
	assignment := make([]int, len(band.Data))
	for i := range assignment {
		assignment[i] = -1
	}
	points := make([]Point, 0)
	owners := make([]int, 0)
	for segmentIndex, segment := range segments {
		for _, point := range segment.Points {
			points = append(points, point)
			owners = append(owners, segmentIndex)
		}
	}
	if len(points) == 0 {
		return assignment
	}
	for y := 0; y < band.Height; y++ {
		for x := 0; x < band.Width; x++ {
			idx := y*band.Width + x
			if band.Data[idx] == 0 {
				continue
			}
			px, py := float64(x)+0.5, float64(y)+0.5
			bestDistance := math.Inf(1)
			bestOwner := -1
			for pointIndex, point := range points {
				dx, dy := px-point.X, py-point.Y
				distance := dx*dx + dy*dy
				if distance < bestDistance {
					bestDistance = distance
					bestOwner = owners[pointIndex]
				}
			}
			assignment[idx] = bestOwner
		}
	}
	return assignment
}

func outsideBandChanges(before, after, band Mask) int {
	if before.Width != after.Width || before.Height != after.Height {
		return maxInt(before.Width*before.Height, after.Width*after.Height)
	}
	changed := 0
	for i := range before.Data {
		if band.Data[i] == 0 && before.Data[i] != after.Data[i] {
			changed++
		}
	}
	return changed
}

func (result *RepairResult) Summary() string {
	if result == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s status=%s rounds=%d converged=%t regions=%d iou=%.4f",
		result.ImageID, result.Status, len(result.Rounds), result.Converged,
		result.Stats.Count, result.Accuracy.IoU)
}
