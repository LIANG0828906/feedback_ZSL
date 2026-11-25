package workbench

import (
	"fmt"
	"sort"

	"ballistic/internal/kernel"
)

type VersionChange struct {
	Component string `json:"component"`
	Before    int64  `json:"before"`
	After     int64  `json:"after"`
}

type InvalidationPlan struct {
	Edit             string              `json:"edit"`
	SegmentID        string              `json:"segment_id,omitempty"`
	Changes          []VersionChange     `json:"changes"`
	DirectTiles      []string            `json:"direct_tiles"`
	DownstreamTiles  []string            `json:"downstream_tiles"`
	ChangedHashTiles []string            `json:"changed_hash_tiles"`
	InvalidTiles     []string            `json:"invalid_tiles"`
	ReusedTiles      []string            `json:"reused_tiles"`
	AttributionIDs   []string            `json:"attribution_ids"`
	Reasons          map[string][]string `json:"reasons"`
}

func (w *Workbench) EditEndpointWorld(segmentID string, newA, newB *kernel.Vec2) (*InvalidationPlan, error) {
	index, _, oldMapping, oldReport, before, err := w.beginSegmentEdit(segmentID)
	if err != nil {
		return nil, err
	}
	if newA != nil {
		a, err := scaleEditCoord(*newA, "$.segments."+segmentID+".endpoint_a", segmentID, w.Scene.SchemaVersion)
		if err != nil {
			return nil, err
		}
		w.Scene.Segments[index].A = a
	}
	if newB != nil {
		b, err := scaleEditCoord(*newB, "$.segments."+segmentID+".endpoint_b", segmentID, w.Scene.SchemaVersion)
		if err != nil {
			return nil, err
		}
		w.Scene.Segments[index].B = b
	}
	w.Scene.Versions.Endpoints++
	if err := w.Rebuild(); err != nil {
		return nil, err
	}
	plan := w.buildPlan("edit_endpoint", segmentID, before, oldMapping, oldReport, true)
	w.LastPlan = plan
	return plan, nil
}

func (w *Workbench) EditRestitution(segmentID string, restitution int64) (*InvalidationPlan, error) {
	index, _, oldMapping, oldReport, before, err := w.beginSegmentEdit(segmentID)
	if err != nil {
		return nil, err
	}
	if restitution < 0 || restitution > kernel.CoeffScale {
		return nil, &kernel.Error{Issue: kernel.Issue{
			Code:          kernel.CodePara,
			Field:         "$.segments." + segmentID + ".restitution",
			ID:            segmentID,
			SourceVersion: w.Scene.SchemaVersion,
			Message:       "restitution out of declared fixed-point range",
			Operands:      []string{itoa64(restitution), "0", itoa64(kernel.CoeffScale)},
		}}
	}
	w.Scene.Segments[index].Restitution = restitution
	w.Scene.Versions.Restitutions++
	if err := w.Rebuild(); err != nil {
		return nil, err
	}
	plan := w.buildPlan("edit_restitution", segmentID, before, oldMapping, oldReport, false)
	w.LastPlan = plan
	return plan, nil
}

func (w *Workbench) beginSegmentEdit(segmentID string) (int, kernel.Segment, *kernel.Mapping, *kernel.PredictionReport, kernel.SemanticVersions, error) {
	index := -1
	var old kernel.Segment
	for i, seg := range w.Scene.Segments {
		if seg.ID == segmentID {
			index = i
			old = seg
			break
		}
	}
	if index < 0 {
		return -1, old, nil, nil, kernel.SemanticVersions{}, &kernel.Error{Issue: kernel.Issue{
			Code:    kernel.CodeSchema,
			Field:   "$.segments",
			ID:      segmentID,
			Message: "segment not found",
		}}
	}
	return index, old, w.Mapping, w.Report, w.Scene.Versions, nil
}

func scaleEditCoord(world kernel.Vec2, field, id string, version int) (kernel.Vec2, error) {
	x, err := kernel.ScaleWorld(world.X, field+".x", id, version)
	if err != nil {
		return kernel.Vec2{}, err
	}
	y, err := kernel.ScaleWorld(world.Y, field+".y", id, version)
	if err != nil {
		return kernel.Vec2{}, err
	}
	return kernel.Vec2{X: x, Y: y}, nil
}

func (w *Workbench) buildPlan(edit, segmentID string, before kernel.SemanticVersions,
	oldMapping *kernel.Mapping, oldReport *kernel.PredictionReport, endpointGeometry bool) *InvalidationPlan {
	plan := &InvalidationPlan{
		Edit:      edit,
		SegmentID: segmentID,
		Changes:   diffVersions(before, w.Scene.Versions),
		Reasons:   map[string][]string{},
	}

	direct := map[string]bool{}
	for _, tileID := range oldMapping.SegmentTiles[segmentID] {
		direct[tileID] = true
		plan.DirectTiles = append(plan.DirectTiles, tileID)
		w.addReason(plan, tileID, "edited_segment_old_projection")
	}
	for _, tileID := range w.Mapping.SegmentTiles[segmentID] {
		if !direct[tileID] {
			plan.DirectTiles = append(plan.DirectTiles, tileID)
		}
		direct[tileID] = true
		w.addReason(plan, tileID, "edited_segment_new_projection")
	}
	sort.Strings(plan.DirectTiles)

	changed := map[string]bool{}
	for _, tileID := range w.Mapping.TileOrder {
		oldHash := oldMapping.TileHash(tileID)
		newHash := w.Mapping.TileHash(tileID)
		if oldHash != newHash {
			changed[tileID] = true
			plan.ChangedHashTiles = append(plan.ChangedHashTiles, tileID)
			w.addReason(plan, tileID, "content_hash_changed")
		}
	}

	affectedRequests := map[string]bool{}
	addHitRequests(oldReport, segmentID, affectedRequests)
	addHitRequests(w.Report, segmentID, affectedRequests)
	addChangedAttributions(oldReport, w.Report, affectedRequests)

	for requestID := range affectedRequests {
		plan.AttributionIDs = append(plan.AttributionIDs, requestID)
		downstream := w.downstreamTiles(requestID, direct, segmentID)
		for _, tileID := range downstream {
			if !direct[tileID] {
				plan.DownstreamTiles = append(plan.DownstreamTiles, tileID)
			}
			w.addReason(plan, tileID, "downstream_hit_attribution:"+requestID)
		}
		w.addReason(plan, "$attribution:"+requestID, "hit_attribution_invalidated")
	}
	sort.Strings(plan.AttributionIDs)
	sort.Strings(plan.DownstreamTiles)
	plan.DownstreamTiles = uniqueSorted(plan.DownstreamTiles)

	invalid := map[string]bool{}
	for id := range direct {
		invalid[id] = true
	}
	for _, id := range plan.DownstreamTiles {
		invalid[id] = true
	}
	for id := range changed {
		invalid[id] = true
	}
	for _, tileID := range w.Mapping.TileOrder {
		if invalid[tileID] {
			plan.InvalidTiles = append(plan.InvalidTiles, tileID)
		} else {
			plan.ReusedTiles = append(plan.ReusedTiles, tileID)
		}
	}
	return plan
}

func (w *Workbench) addReason(plan *InvalidationPlan, tileID, reason string) {
	for _, existing := range plan.Reasons[tileID] {
		if existing == reason {
			return
		}
	}
	plan.Reasons[tileID] = append(plan.Reasons[tileID], reason)
}

func addHitRequests(report *kernel.PredictionReport, segmentID string, target map[string]bool) {
	if report == nil {
		return
	}
	for _, traj := range report.Trajectories {
		if traj.Attribution.FirstHitSegmentID == segmentID {
			target[traj.RequestID] = true
			continue
		}
		for _, hit := range traj.Attribution.HitSegmentIDs {
			if hit == segmentID {
				target[traj.RequestID] = true
				break
			}
		}
	}
}

func addChangedAttributions(oldReport, newReport *kernel.PredictionReport, target map[string]bool) {
	if oldReport == nil || newReport == nil {
		return
	}
	oldByID := map[string]kernel.Attribution{}
	for _, traj := range oldReport.Trajectories {
		oldByID[traj.RequestID] = traj.Attribution
	}
	for _, traj := range newReport.Trajectories {
		old, ok := oldByID[traj.RequestID]
		if !ok || !attributionsEqual(old, traj.Attribution) {
			target[traj.RequestID] = true
		}
	}
}

func attributionsEqual(a, b kernel.Attribution) bool {
	if a.FirstHitSegmentID != b.FirstHitSegmentID ||
		a.BounceCount != b.BounceCount ||
		a.Escaped != b.Escaped ||
		a.Outcome != b.Outcome ||
		len(a.HitSegmentIDs) != len(b.HitSegmentIDs) {
		return false
	}
	for i := range a.HitSegmentIDs {
		if a.HitSegmentIDs[i] != b.HitSegmentIDs[i] {
			return false
		}
	}
	return true
}

func (w *Workbench) downstreamTiles(requestID string, direct map[string]bool, segmentID string) []string {
	var trajectory *kernel.Trajectory
	for i := range w.Report.Trajectories {
		if w.Report.Trajectories[i].RequestID == requestID {
			trajectory = &w.Report.Trajectories[i]
			break
		}
	}
	if trajectory == nil {
		return nil
	}
	pixels := make([]kernel.Pixel, 0, len(trajectory.Samples))
	for _, sample := range trajectory.Samples {
		pixel, err := w.Camera.Project(sample.Position)
		if err != nil {
			continue
		}
		pixels = append(pixels, pixel)
	}

	start := -1
	for _, bounce := range trajectory.Bounces {
		if bounce.SegmentID == segmentID {
			start = bounce.Frame
			break
		}
	}
	if start < 0 {
		for i, pixel := range pixels {
			if direct[coordID(w.Camera.TileAt(pixel))] {
				start = i
				break
			}
		}
	}
	if start < 0 {
		for i := 0; i+1 < len(pixels); i++ {
			touched := false
			kernel.RasterLine(pixels[i], pixels[i+1], func(p kernel.Pixel) {
				if direct[coordID(w.Camera.TileAt(p))] {
					touched = true
				}
			})
			if touched {
				start = i
				break
			}
		}
	}
	if start < 0 {
		return append([]string{}, w.Mapping.RequestTiles[requestID]...)
	}

	set := map[string]bool{}
	addPixel := func(p kernel.Pixel) {
		id := coordID(w.Camera.TileAt(p))
		if w.Mapping.Tiles[id] != nil {
			set[id] = true
		}
	}
	if start < len(pixels) {
		addPixel(pixels[start])
	}
	for i := start; i+1 < len(pixels); i++ {
		kernel.RasterLine(pixels[i], pixels[i+1], addPixel)
	}
	for _, bounce := range trajectory.Bounces {
		pixel, err := w.Camera.Project(bounce.Position)
		if err == nil {
			addPixel(pixel)
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func coordID(coord kernel.TileCoord) string {
	return fmt.Sprintf("tx%02d_ty%02d", coord.X, coord.Y)
}

func diffVersions(before, after kernel.SemanticVersions) []VersionChange {
	changes := []VersionChange{}
	add := func(component string, b, a int64) {
		if b != a {
			changes = append(changes, VersionChange{Component: component, Before: b, After: a})
		}
	}
	add("endpoints", before.Endpoints, after.Endpoints)
	add("normals", before.Normals, after.Normals)
	add("restitutions", before.Restitutions, after.Restitutions)
	add("gravity", before.Gravity, after.Gravity)
	add("time_step", before.TimeStep, after.TimeStep)
	add("materials", before.Materials, after.Materials)
	return changes
}

func uniqueSorted(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		set[value] = true
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func itoa(i int) string {
	return itoa64(int64(i))
}

func itoa64(i int64) string {
	if i == 0 {
		return "0"
	}
	negative := i < 0
	if negative {
		i = -i
	}
	var buf [24]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if negative {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
