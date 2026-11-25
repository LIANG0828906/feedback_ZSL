package engine

import (
	"fmt"
	"math"
	"sort"

	"ms-workbench/internal/apperr"
	"ms-workbench/internal/scene"
	"ms-workbench/internal/store"
)

type invalidationStage struct {
	fusion  bool
	chroma  bool
	reasons []string
}

func (e *Engine) SetSettings(next scene.RenderSettings) (InvalidationReport, error) {
	if err := next.Validate(); err != nil {
		return InvalidationReport{}, apperr.New(apperr.EPARAM, "%v", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return InvalidationReport{}, apperr.New(apperr.ESTATE, "pause and wait for the running pass before editing parameters")
	}
	old := e.scene.Settings
	if old == next {
		return e.invalidationReportLocked(nil), nil
	}
	changed := make([]string, 0, 4)
	var stage invalidationStage
	addFusion := func(path string) {
		stage.fusion = true
		stage.chroma = true
		stage.reasons = appendUnique(stage.reasons, path, "chroma-downstream")
		changed = append(changed, path)
	}
	addChroma := func(path string) {
		stage.chroma = true
		stage.reasons = appendUnique(stage.reasons, path)
		changed = append(changed, path)
	}
	if old.InterpolationOrder != next.InterpolationOrder {
		addFusion("render.interpolationOrder")
	}
	if old.MaxRegistrationOffset != next.MaxRegistrationOffset {
		addFusion("render.maxRegistrationOffset")
	}
	if old.FusionPower != next.FusionPower {
		addFusion("render.fusionPower")
	}
	if old.FusionStrength != next.FusionStrength {
		addFusion("render.fusionStrength")
	}
	if old.DetailGain != next.DetailGain {
		addFusion("render.detailGain")
	}
	if old.GamutIntent != next.GamutIntent {
		addChroma("render.gamutIntent")
	}
	if old.TargetGamut != next.TargetGamut {
		addChroma("render.targetGamut")
	}
	sort.Strings(changed)
	newScene := e.scene.Clone()
	newScene.Settings = next
	if err := newScene.Settings.Validate(); err != nil {
		return InvalidationReport{}, apperr.New(apperr.EPARAM, "%v", err)
	}
	stages := map[int]invalidationStage{}
	for _, seg := range e.segments {
		stages[seg.ID] = stage
	}
	return e.commitEditLocked(newScene, changed, stages)
}

// Dependencies exposes the explicit parameter/band-to-segment graph used by
// invalidation.
func (e *Engine) Dependencies() []Dependency {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Dependency
	addAll := func(parameter, stage, description string) {
		for _, seg := range e.segments {
			out = append(out, Dependency{
				Parameter: parameter, Stage: stage, SegmentID: seg.ID, Description: description,
			})
		}
	}
	addAll("render.interpolationOrder", "fusion+chroma", "raw code interpolation changes fused radiance and downstream color")
	addAll("render.maxRegistrationOffset", "fusion+chroma", "registration gate changes alignment")
	addAll("render.fusionPower", "fusion+chroma", "exposure weight curve changes fusion")
	addAll("render.fusionStrength", "fusion+chroma", "exposure weight curve changes fusion")
	addAll("render.detailGain", "fusion+chroma", "noise detail enters fused radiance")
	addAll("render.gamutIntent", "chroma", "target RGB mapping changes only")
	addAll("render.targetGamut", "chroma", "target RGB mapping only")
	for _, b := range e.scene.Bands {
		addAll("band."+b.ID+".response", "chroma", "spectral response feeds XYZ integration")
		addAll("band."+b.ID+".registration", "fusion+chroma", "band offset changes aligned samples")
		addAll("band."+b.ID+".exposures", "fusion+chroma", "exposure timing changes linearization")
		for _, seg := range e.segments {
			out = append(out, Dependency{
				Parameter:   "band." + b.ID + ".image",
				Stage:       "fusion+chroma",
				SegmentID:   seg.ID,
				Description: "only segments intersecting changed source pixels are invalidated",
			})
		}
	}
	return out
}

func (e *Engine) ReplaceBand(bandID string, replacement scene.Band) (InvalidationReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return InvalidationReport{}, apperr.New(apperr.ESTATE, "pause and wait for the running pass before replacing a band")
	}
	index := e.scene.BandIndex(bandID)
	if index < 0 {
		return InvalidationReport{}, apperr.New(apperr.EPARAM, "unknown band %q", bandID)
	}
	if replacement.ID != bandID {
		return InvalidationReport{}, apperr.New(apperr.EPARAM, "replacement band id %q does not match %q", replacement.ID, bandID)
	}
	oldBand := e.scene.Bands[index]
	newScene := e.scene.Clone()
	newScene.Bands[index] = replacement.Clone()
	if err := newScene.Validate(); err != nil {
		return InvalidationReport{}, apperr.Schema(e.scene.SourceVersion, fmt.Sprintf("bands[%d]", index), "%v", err)
	}

	changed := []string{"band." + bandID}
	stages := map[int]invalidationStage{}
	var global invalidationStage
	responseChanged := oldBand.ResponseXYZ != replacement.ResponseXYZ
	offsetChanged := oldBand.OffsetX != replacement.OffsetX || oldBand.OffsetY != replacement.OffsetY
	timingChanged := exposureTimingChanged(oldBand, replacement)
	if responseChanged {
		global.chroma = true
		global.reasons = appendUnique(global.reasons, "band-response:"+bandID)
		changed = append(changed, "band."+bandID+".response")
	}
	if offsetChanged {
		global.fusion = true
		global.chroma = true
		global.reasons = appendUnique(global.reasons, "registration:"+bandID, "chroma-downstream")
		changed = append(changed, "band."+bandID+".registration")
	}
	if timingChanged {
		global.fusion = true
		global.chroma = true
		global.reasons = appendUnique(global.reasons, "exposure-timing:"+bandID, "chroma-downstream")
		changed = append(changed, "band."+bandID+".exposures")
	}
	for _, seg := range e.segments {
		stages[seg.ID] = global
	}
	region, imageChanged, imageAll := bandImageDiff(oldBand, replacement, e.scene.Width, e.scene.Height)
	if imageChanged {
		changed = append(changed, "band."+bandID+".image")
		expand := int(math.Ceil(math.Max(math.Abs(oldBand.OffsetX), math.Abs(oldBand.OffsetY))))
		expand = maxInt(expand, int(math.Ceil(math.Abs(replacement.OffsetX))))
		expand = maxInt(expand, int(math.Ceil(math.Abs(replacement.OffsetY))))
		expand = maxInt(expand, e.scene.Settings.InterpolationOrder/2+2)
		region = region.Expand(expand)
		for _, seg := range e.segments {
			if !imageAll && !seg.Rect.Intersects(region) {
				continue
			}
			stage := stages[seg.ID]
			stage.fusion = true
			stage.chroma = true
			stage.reasons = appendUnique(stage.reasons, "band-image:"+bandID, "chroma-downstream")
			stages[seg.ID] = stage
		}
	}
	sort.Strings(changed)
	return e.commitEditLocked(newScene, changed, stages)
}

func (e *Engine) commitEditLocked(newScene *scene.Scene, changed []string, stages map[int]invalidationStage) (InvalidationReport, error) {
	key := storeKeyFor(newScene, e.cfg)
	var deleteIDs []int
	for id, stage := range stages {
		if stage.fusion || stage.chroma {
			deleteIDs = append(deleteIDs, id)
		}
	}
	sort.Ints(deleteIDs)
	if err := e.cp.ApplyEdit(key, deleteIDs); err != nil {
		return InvalidationReport{}, err
	}
	e.scene = newScene
	report := InvalidationReport{
		ChangedFields:    append([]string(nil), changed...),
		ParamVersion:     newScene.Settings.Version(),
		BandVersion:      newScene.BandMetadataVersion(),
		SceneFingerprint: newScene.Fingerprint(),
	}
	for _, seg := range e.segments {
		stage := stages[seg.ID]
		entry := SegmentInvalidation{
			SegmentID: seg.ID,
			Reused:    seg.State == Committed && !stage.fusion && !stage.chroma,
			Reasons:   append([]string(nil), stage.reasons...),
		}
		switch {
		case stage.fusion:
			entry.Stage = "fusion+chroma"
			seg.FusionDirty = true
			seg.ChromaDirty = true
			seg.Result = nil
			seg.Ready = false
			seg.Reasons = appendUnique(seg.Reasons, stage.reasons...)
			if seg.State == Committed {
				if err := transitionLocked(&seg.State, Pending, seg.ID); err != nil {
					return report, err
				}
			} else if seg.State == Failed {
				if err := transitionLocked(&seg.State, Pending, seg.ID); err != nil {
					return report, err
				}
			}
		case stage.chroma:
			entry.Stage = "chroma"
			seg.ChromaDirty = true
			seg.Ready = false
			seg.Reasons = appendUnique(seg.Reasons, stage.reasons...)
			if seg.State == Committed {
				if err := transitionLocked(&seg.State, Pending, seg.ID); err != nil {
					return report, err
				}
			} else if seg.State == Failed {
				if err := transitionLocked(&seg.State, Pending, seg.ID); err != nil {
					return report, err
				}
			}
		default:
			entry.Stage = "none"
			entry.Reasons = nil
		}
		seg.Reused = entry.Reused
		report.Segments = append(report.Segments, entry)
	}
	e.lastInvalidation = &report
	return report, nil
}

func (e *Engine) invalidationReportLocked(_ map[int]invalidationStage) InvalidationReport {
	report := InvalidationReport{
		ParamVersion:     e.scene.Settings.Version(),
		BandVersion:      e.scene.BandMetadataVersion(),
		SceneFingerprint: e.scene.Fingerprint(),
	}
	for _, seg := range e.segments {
		reused := seg.State == Committed
		report.Segments = append(report.Segments, SegmentInvalidation{
			SegmentID: seg.ID,
			Stage:     "none",
			Reused:    reused,
		})
		seg.Reused = reused
	}
	return report
}

func storeKeyFor(s *scene.Scene, cfg Config) store.Key {
	return store.Key{
		SceneID:          s.SceneID,
		SceneFingerprint: s.Fingerprint(),
		ParamVersion:     s.Settings.Version(),
		BandVersion:      s.BandMetadataVersion(),
		AlgorithmVersion: cfg.AlgorithmVersion,
		Width:            s.Width,
		Height:           s.Height,
		BandCount:        len(s.Bands),
		ChunkW:           cfg.ChunkWidth,
		ChunkH:           cfg.ChunkHeight,
	}
}

func appendUnique(values []string, additions ...string) []string {
	for _, add := range additions {
		found := false
		for _, v := range values {
			if v == add {
				found = true
				break
			}
		}
		if !found {
			values = append(values, add)
		}
	}
	return values
}

func exposureTimingChanged(a, b scene.Band) bool {
	if len(a.Exposures) != len(b.Exposures) {
		return true
	}
	for i := range a.Exposures {
		if a.Exposures[i].DurationUS != b.Exposures[i].DurationUS {
			return true
		}
	}
	return false
}

func bandImageDiff(a, b scene.Band, width, height int) (scene.Rect, bool, bool) {
	minX, minY := width, height
	maxX, maxY := -1, -1
	if len(a.Exposures) != len(b.Exposures) || len(a.Detail) != len(b.Detail) {
		return scene.Rect{}, true, true
	}
	for i := range a.Exposures {
		if len(a.Exposures[i].Codes) != len(b.Exposures[i].Codes) {
			return scene.Rect{}, true, true
		}
		for idx, av := range a.Exposures[i].Codes {
			if av != b.Exposures[i].Codes[idx] {
				x, y := idx%width, idx/width
				if x < minX {
					minX = x
				}
				if y < minY {
					minY = y
				}
				if x > maxX {
					maxX = x
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	for idx, av := range a.Detail {
		if math.Float64bits(av) != math.Float64bits(b.Detail[idx]) {
			x, y := idx%width, idx/width
			if x < minX {
				minX = x
			}
			if y < minY {
				minY = y
			}
			if x > maxX {
				maxX = x
			}
			if y > maxY {
				maxY = y
			}
		}
	}
	if maxX < 0 {
		return scene.Rect{}, false, false
	}
	return scene.Rect{X: minX, Y: minY, W: maxX - minX + 1, H: maxY - minY + 1}, true, false
}
