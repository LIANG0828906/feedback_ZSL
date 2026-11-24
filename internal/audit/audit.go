// Package audit independently verifies a persisted proof snapshot using only
// SQLite records, output pixels and the generator truth fingerprint. It never
// imports or calls the conversion package.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"hdrproof/internal/apperr"
	"hdrproof/internal/model"
	"hdrproof/internal/sample"
	"hdrproof/internal/snapshot"
)

const truthTolerance = 0.02

type Result struct {
	Passed           bool   `json:"passed"`
	TruthFingerprint string `json:"truth_fingerprint"`
	RoutesAudited    int    `json:"routes_audited"`
	SegmentsAudited  int    `json:"segments_audited"`
	FramesAudited    int    `json:"frames_audited"`
	RecordsAudited   int    `json:"records_audited"`
}

func Audit(ctx context.Context, datasetDir, dbPath string) (*Result, error) {
	ds, err := sample.LoadMetadata(datasetDir)
	if err != nil {
		return nil, err
	}
	store, err := snapshot.OpenReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	meta, err := store.Meta(ctx)
	if err != nil {
		return nil, err
	}
	if meta["truth_fingerprint"] != ds.TruthFingerprint {
		return nil, apperr.New(apperr.CodeAudit, "", -1, -1, "truth_fingerprint",
			ds.TruthFingerprint, fmt.Sprintf("snapshot has %q", meta["truth_fingerprint"]), nil)
	}
	counts, err := store.FrameCounts(ctx)
	if err != nil {
		return nil, err
	}
	for key, count := range counts {
		if count > 1 {
			return nil, apperr.New(apperr.CodeAudit, key.RouteID, -1, key.Frame,
				"records.duplicate", "exactly one proof record", fmt.Sprintf("%d records found", count), nil)
		}
	}
	segments, frames, err := store.ReadAll(ctx)
	if err != nil {
		return nil, err
	}
	result := &Result{TruthFingerprint: ds.TruthFingerprint}
	seenFrames := make(map[snapshot.FrameKey]bool)
	seenSegments := make(map[snapshot.SegmentKey]bool)
	for _, rd := range ds.Routes {
		route := rd.Route
		result.RoutesAudited++
		for _, truthFrame := range rd.Truth.Frames {
			key := snapshot.FrameKey{RouteID: route.ID, Frame: truthFrame.FrameIndex}
			frame, ok := frames[key]
			if !ok {
				return nil, apperr.New(apperr.CodeAudit, route.ID,
					model.SegmentOf(route, truthFrame.TimestampMS), truthFrame.FrameIndex,
					"records.missing", "exactly one proof record", "proof record is missing", nil)
			}
			if seenFrames[key] {
				return nil, apperr.New(apperr.CodeAudit, route.ID, frame.Segment, truthFrame.FrameIndex,
					"records.duplicate", "exactly one proof record", "duplicate proof record", nil)
			}
			seenFrames[key] = true
			if err := auditFrame(route, truthFrame, frame); err != nil {
				return nil, err
			}
			result.FramesAudited++
			result.RecordsAudited++
		}
	}
	if len(frames) != result.FramesAudited {
		for key := range frames {
			if !seenFrames[key] {
				return nil, apperr.New(apperr.CodeAudit, key.RouteID, -1, key.Frame,
					"records.extra", "only manifest truth frames", "unexpected proof record", nil)
			}
		}
	}
	for _, rd := range ds.Routes {
		count := model.SegmentCount(rd.Route)
		for seg := 0; seg < count; seg++ {
			indices := model.FrameIndicesForSegment(rd.Route, seg)
			if len(indices) == 0 {
				continue
			}
			key := snapshot.SegmentKey{RouteID: rd.Route.ID, Segment: seg}
			stored, ok := segments[key]
			if !ok {
				return nil, apperr.New(apperr.CodeAudit, rd.Route.ID, seg, -1,
					"segments.missing", "committed segment checkpoint", "segment checkpoint is missing", nil)
			}
			if seenSegments[key] {
				return nil, apperr.New(apperr.CodeAudit, rd.Route.ID, seg, -1,
					"segments.duplicate", "exactly one segment checkpoint", "duplicate segment checkpoint", nil)
			}
			seenSegments[key] = true
			committedFrames := make([]model.FrameResult, 0, len(indices))
			for _, index := range indices {
				frame := frames[snapshot.FrameKey{RouteID: rd.Route.ID, Frame: index}]
				var proof model.Proof
				if err := json.Unmarshal(frame.ProofJSON, &proof); err != nil {
					return nil, apperr.New(apperr.CodeAudit, rd.Route.ID, seg, index,
						"proof_json", "valid JSON", err.Error(), nil)
				}
				committedFrames = append(committedFrames, model.FrameResult{
					RouteID: frame.RouteID, Frame: frame.FrameIndex, Segment: frame.Segment,
					Pixels: frame.Pixels, Proof: proof, ProofJSON: frame.ProofJSON,
				})
			}
			plan := snapshot.SegmentPlan{
				RouteID: rd.Route.ID, Segment: seg, FrameStart: indices[0], FrameEnd: indices[len(indices)-1],
				CommitOrder: stored.CommitOrder,
			}
			computed := snapshot.BuildSegmentCommit(plan, committedFrames, stored.ParamVersion,
				stored.AlgorithmVersion, stored.TruthFingerprint)
			if computed.ContentHash != stored.ContentHash || computed.OutputHash != stored.OutputHash ||
				computed.ProofHash != stored.ProofHash || computed.SourceHash != stored.SourceHash {
				return nil, apperr.New(apperr.CodeAudit, rd.Route.ID, seg, -1,
					"segment.content_hash", stored.ContentHash, "segment hashes do not reproduce", nil)
			}
			result.SegmentsAudited++
		}
	}
	if len(segments) != result.SegmentsAudited {
		return nil, apperr.New(apperr.CodeAudit, "", -1, -1, "segments.extra",
			fmt.Sprintf("%d segments", result.SegmentsAudited), fmt.Sprintf("found %d", len(segments)), nil)
	}
	result.Passed = true
	return result, nil
}

func auditFrame(route model.Route, truth model.FrameTruth, frame snapshot.StoredFrame) error {
	segment := model.SegmentOf(route, truth.TimestampMS)
	if frame.FrameIndex != truth.FrameIndex || frame.TimestampMS != truth.TimestampMS || frame.Segment != segment {
		return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
			"frame.identity", "matching frame index, timestamp and segment", "stored frame identity differs", nil)
	}
	if model.SHA256Hex(frame.Pixels) != frame.PixelHash {
		return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
			"pixels.sha256", frame.PixelHash, "pixel hash mismatch", nil)
	}
	var proof model.Proof
	if err := json.Unmarshal(frame.ProofJSON, &proof); err != nil {
		return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
			"proof_json", "valid JSON", err.Error(), nil)
	}
	if proof.RecordSHA256 != frame.RecordHash || model.ComputeRecordHash(proof) != proof.RecordSHA256 {
		return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
			"proof.record_sha256", frame.RecordHash, "record hash does not reproduce", nil)
	}
	if proof.RouteID != route.ID || proof.FrameIndex != truth.FrameIndex ||
		proof.TimestampMS != truth.TimestampMS || proof.Segment != segment {
		return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
			"proof.identity", "matching route/frame/timestamp/segment", "proof identity differs", nil)
	}
	if proof.TruthFingerprint != frame.TruthFingerprint {
		return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
			"proof.truth_fingerprint", frame.TruthFingerprint, "proof truth fingerprint differs", nil)
	}
	if proof.AlgorithmVersion != frame.AlgorithmVersion || proof.ParamVersion != frame.ParamVersion {
		return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
			"proof.versions", "stored parameter and algorithm versions", "proof versions differ", nil)
	}
	width, height := proof.OutputStats.Width, proof.OutputStats.Height
	if width != route.Width || height != route.Height || len(frame.Pixels) != width*height*3 {
		return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
			"pixels.dimensions", fmt.Sprintf("%dx%dx3", route.Width, route.Height),
			fmt.Sprintf("stored %dx%dx%d", width, height, len(frame.Pixels)), nil)
	}
	stats, err := independentStats(frame.Pixels, width, height, proof.Params.TargetPeakNits)
	if err != nil {
		return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
			"pixels.encoding", "complete RGB8 frame", err.Error(), nil)
	}
	if stats.MinLinear != proof.OutputStats.MinLinear || stats.MaxLinear != proof.OutputStats.MaxLinear ||
		stats.MeanLinear != proof.OutputStats.MeanLinear || stats.PixelSHA256 != proof.OutputStats.PixelSHA256 ||
		!closeEnough(stats.PeakNits, proof.OutputStats.PeakNits, 1e-12) {
		return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
			"output_stats", "recomputed from persisted pixels", "output statistics mismatch", nil)
	}
	if len(proof.Patches) != len(truth.Patches) {
		return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
			"proof.patches", fmt.Sprintf("%d truth patches", len(truth.Patches)),
			fmt.Sprintf("%d proof patches", len(proof.Patches)), nil)
	}
	for i, patch := range truth.Patches {
		pp := proof.Patches[i]
		if pp.Name != patch.Name || pp.X != patch.X || pp.Y != patch.Y ||
			pp.Width != patch.Width || pp.Height != patch.Height {
			return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
				"proof.patches.geometry", patch.Name, "patch geometry differs", nil)
		}
		for c := 0; c < 3; c++ {
			if !closeEnough(pp.ExpectedLinearRGB[c], patch.ExpectedLinearRGB[c], 1e-12) {
				return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
					"proof.patches.expected_linear_rgb", "generator truth value",
					fmt.Sprintf("patch %s channel %d differs", patch.Name, c), nil)
			}
		}
		actual, err := patchAverage(frame.Pixels, width, patch.X, patch.Y, patch.Width, patch.Height)
		if err != nil {
			return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
				"proof.patches.actual_linear_rgb", "patch inside frame", err.Error(), nil)
		}
		for c := 0; c < 3; c++ {
			if !closeEnough(actual[c], pp.ActualLinearRGB[c], 1e-12) {
				return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
					"proof.patches.actual_linear_rgb", "recomputed patch average",
					fmt.Sprintf("patch %s channel %d differs", patch.Name, c), nil)
			}
			if math.Abs(actual[c]-patch.ExpectedLinearRGB[c]) > truthTolerance {
				return apperr.New(apperr.CodeAudit, route.ID, segment, truth.FrameIndex,
					"truth.delta", fmt.Sprintf("<= %.4f linear", truthTolerance),
					fmt.Sprintf("patch %s channel %d delta %.6f", patch.Name, c, actual[c]-patch.ExpectedLinearRGB[c]), nil)
			}
		}
	}
	return nil
}

func independentStats(pixels []byte, width, height int, targetPeak float64) (model.Stats, error) {
	if len(pixels) != width*height*3 {
		return model.Stats{}, fmt.Errorf("pixel length %d", len(pixels))
	}
	stats := model.Stats{
		Width: width, Height: height, Channels: 3,
		MinLinear:   [3]float64{math.Inf(1), math.Inf(1), math.Inf(1)},
		MaxLinear:   [3]float64{math.Inf(-1), math.Inf(-1), math.Inf(-1)},
		PixelSHA256: model.SHA256Hex(pixels),
	}
	var sums [3]float64
	for i := 0; i < width*height; i++ {
		for c := 0; c < 3; c++ {
			v := srgbToLinear(pixels[i*3+c])
			if v < stats.MinLinear[c] {
				stats.MinLinear[c] = v
			}
			if v > stats.MaxLinear[c] {
				stats.MaxLinear[c] = v
			}
			sums[c] += v
		}
	}
	n := float64(width * height)
	for c := 0; c < 3; c++ {
		stats.MeanLinear[c] = sums[c] / n
	}
	stats.PeakNits = math.Max(stats.MaxLinear[0], math.Max(stats.MaxLinear[1], stats.MaxLinear[2])) * targetPeak
	return stats, nil
}

func patchAverage(pixels []byte, width, x, y, patchWidth, patchHeight int) ([3]float64, error) {
	var sums [3]float64
	for yy := y; yy < y+patchHeight; yy++ {
		for xx := x; xx < x+patchWidth; xx++ {
			idx := (yy*width + xx) * 3
			if idx+2 >= len(pixels) {
				return [3]float64{}, fmt.Errorf("patch exceeds pixel buffer")
			}
			for c := 0; c < 3; c++ {
				sums[c] += srgbToLinear(pixels[idx+c])
			}
		}
	}
	n := float64(patchWidth * patchHeight)
	return [3]float64{sums[0] / n, sums[1] / n, sums[2] / n}, nil
}

func srgbToLinear(v byte) float64 {
	s := float64(v) / 255
	if s <= 0.04045 {
		return s / 12.92
	}
	return math.Pow((s+0.055)/1.055, 2.4)
}

func closeEnough(a, b, tolerance float64) bool {
	return math.Abs(a-b) <= tolerance
}

func SortedKeys(m map[snapshot.FrameKey]snapshot.StoredFrame) []snapshot.FrameKey {
	keys := make([]snapshot.FrameKey, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].RouteID == keys[j].RouteID {
			return keys[i].Frame < keys[j].Frame
		}
		return keys[i].RouteID < keys[j].RouteID
	})
	return keys
}
