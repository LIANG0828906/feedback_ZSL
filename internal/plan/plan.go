package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"texlab/internal/apperr"
	"texlab/internal/model"
)

func Load(path string, im model.Image) (model.Plan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return model.Plan{}, &apperr.Error{Code: apperr.EIO, Message: "read plan", Path: path, Details: err.Error()}
	}
	var p model.Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return model.Plan{}, &apperr.Error{Code: apperr.EPlan, Message: "decode plan JSON", Field: "plan", Details: err.Error()}
	}
	if err := Validate(p, im); err != nil {
		return model.Plan{}, err
	}
	return p, nil
}

func Validate(p model.Plan, im model.Image) error {
	if im.Width <= 0 || im.Height <= 0 {
		return apperr.At(apperr.EPlan, 0, "source", "source image has invalid dimensions")
	}
	if p.Segments.Cols <= 0 || p.Segments.Rows <= 0 {
		return apperr.At(apperr.EPlan, 0, "segments", "segment grid must have positive cols and rows")
	}
	if p.Segments.Cols > 16 || p.Segments.Rows > 16 || p.Segments.Cols*p.Segments.Rows > 256 {
		return apperr.At(apperr.EPlan, 0, "segments", "segment grid exceeds 16x16 / 256 cells")
	}
	if im.Width < p.Segments.Cols || im.Height < p.Segments.Rows {
		return apperr.At(apperr.EPlan, 0, "segments", "segment grid is larger than image")
	}
	if p.Revision < 0 {
		return apperr.At(apperr.EPlan, 0, "revision", "revision must be non-negative")
	}
	regions := map[string]model.Rect{}
	for i, r := range p.Regions {
		field := fmt.Sprintf("regions[%d]", i)
		if strings.TrimSpace(r.ID) == "" {
			return apperr.At(apperr.EPlan, 0, field+".id", "region id is required")
		}
		if _, exists := regions[r.ID]; exists {
			return apperr.At(apperr.EPlan, 0, field+".id", "duplicate region id %q", r.ID)
		}
		if r.W <= 0 || r.H <= 0 {
			return apperr.At(apperr.EPlan, 0, field+".w", "region %q has non-positive size", r.ID)
		}
		if r.X < 0 || r.Y < 0 || r.X+r.W > im.Width || r.Y+r.H > im.Height {
			return apperr.At(apperr.EPlan, 0, field, "region %q lies outside %dx%d image", r.ID, im.Width, im.Height)
		}
		regions[r.ID] = r
	}
	if len(p.Entries) == 0 {
		return apperr.At(apperr.EPlan, 0, "entries", "at least one conversion entry is required")
	}
	ids := map[string]bool{}
	sourceChannels := im.Channels
	if sourceChannels <= 0 || sourceChannels > 4 {
		return apperr.At(apperr.EPlan, 0, "source.channels", "source channel count must be between 1 and 4")
	}
	for i := range p.Entries {
		e := &p.Entries[i]
		entryNo := i + 1
		if e.Index != entryNo {
			return apperr.At(apperr.EPlan, entryNo, "index", "entry sequence must be continuous; got %d, want %d", e.Index, entryNo)
		}
		if strings.TrimSpace(e.ID) == "" {
			return apperr.At(apperr.EPlan, entryNo, "id", "entry id is required")
		}
		if ids[e.ID] {
			return apperr.At(apperr.EPlan, entryNo, "id", "duplicate entry id %q", e.ID)
		}
		ids[e.ID] = true
		if e.TargetDepth != 8 && e.TargetDepth != 16 {
			return apperr.At(apperr.EPlan, entryNo, "target_depth", "target depth must be 8 or 16")
		}
		if e.TargetChannels < 1 || e.TargetChannels > 4 {
			return apperr.At(apperr.EPlan, entryNo, "target_channels", "target channels must be between 1 and 4")
		}
		if e.Alpha != "straight" && e.Alpha != "premultiplied" {
			return apperr.At(apperr.EPlan, entryNo, "alpha", "alpha convention must be straight or premultiplied")
		}
		if e.TargetChannels < 4 && e.Alpha == "premultiplied" {
			return apperr.At(apperr.EPlan, entryNo, "alpha", "premultiplied output requires an alpha channel")
		}
		if e.Quantize != "floor" && e.Quantize != "ceil" && e.Quantize != "round" {
			return apperr.At(apperr.EPlan, entryNo, "quantize", "quantize must be floor, ceil, or round")
		}
		if len(e.Regions) == 0 {
			return apperr.At(apperr.EPlan, entryNo, "regions", "entry must declare at least one region")
		}
		seenRegions := map[string]bool{}
		for j, id := range e.Regions {
			if _, ok := regions[id]; !ok {
				return apperr.At(apperr.EPlan, entryNo, fmt.Sprintf("regions[%d]", j), "unknown region %q", id)
			}
			if seenRegions[id] {
				return apperr.At(apperr.EPlan, entryNo, fmt.Sprintf("regions[%d]", j), "duplicate region %q", id)
			}
			seenRegions[id] = true
		}
		if len(e.Permutation) != e.TargetChannels {
			return apperr.At(apperr.EPlan, entryNo, "permutation", "permutation length %d must equal target_channels %d", len(e.Permutation), e.TargetChannels)
		}
		used := map[int]bool{}
		fillAt := map[int]model.FillSpec{}
		for _, f := range e.Fill {
			if f.TargetIndex < 0 || f.TargetIndex >= e.TargetChannels {
				return apperr.At(apperr.EPlan, entryNo, "fill.target_index", "fill target index %d is out of range", f.TargetIndex)
			}
			if _, ok := fillAt[f.TargetIndex]; ok {
				return apperr.At(apperr.EPlan, entryNo, "fill.target_index", "duplicate fill for target index %d", f.TargetIndex)
			}
			switch f.Source {
			case "constant":
				maxValue := 255
				if e.TargetDepth == 16 {
					maxValue = 65535
				}
				if f.Value < 0 || f.Value > maxValue {
					return apperr.At(apperr.EPlan, entryNo, "fill.value", "constant fill %d outside 0..%d", f.Value, maxValue)
				}
			case "zero", "one", "alpha":
			case "channel":
				if f.SourceChannel < 0 || f.SourceChannel >= sourceChannels {
					return apperr.At(apperr.EPlan, entryNo, "fill.source_channel", "fill source channel %d outside source channel count %d", f.SourceChannel, sourceChannels)
				}
			default:
				return apperr.At(apperr.EPlan, entryNo, "fill.source", "unknown fill source %q", f.Source)
			}
			if len(f.Channel) != 1 || !strings.Contains("RGBA", f.Channel) {
				return apperr.At(apperr.EPlan, entryNo, "fill.channel", "fill channel must be one of R,G,B,A")
			}
			fillAt[f.TargetIndex] = f
		}
		for slot, src := range e.Permutation {
			if src < -1 || src >= sourceChannels {
				return apperr.At(apperr.EPlan, entryNo, fmt.Sprintf("permutation[%d]", slot), "channel index %d outside source channel count %d", src, sourceChannels)
			}
			if src == -1 {
				if _, ok := fillAt[slot]; !ok {
					return apperr.At(apperr.EPlan, entryNo, fmt.Sprintf("permutation[%d]", slot), "missing fill declaration for target slot %d", slot)
				}
				continue
			}
			if used[src] {
				return apperr.At(apperr.EPlan, entryNo, fmt.Sprintf("permutation[%d]", slot), "source channel %d is used more than once", src)
			}
			used[src] = true
			if _, ok := fillAt[slot]; ok {
				return apperr.At(apperr.EPlan, entryNo, "fill", "target slot %d has a mapped channel and a fill declaration", slot)
			}
		}
		if e.TargetChannels >= sourceChannels {
			for src := 0; src < sourceChannels; src++ {
				if !used[src] {
					return apperr.At(apperr.EPlan, entryNo, "permutation", "source channel %d is missing from channel arrangement", src)
				}
			}
			if len(e.Drop) != 0 {
				return apperr.At(apperr.EPlan, entryNo, "drop", "drop must be empty when channel count does not decrease")
			}
			if len(fillAt) != e.TargetChannels-sourceChannels {
				return apperr.At(apperr.EPlan, entryNo, "fill", "expected %d fill declarations, got %d", e.TargetChannels-sourceChannels, len(fillAt))
			}
		} else {
			if len(fillAt) != 0 {
				return apperr.At(apperr.EPlan, entryNo, "fill", "fill must be empty when channel count decreases")
			}
			var expectedDrop []int
			for src := 0; src < sourceChannels; src++ {
				if !used[src] {
					expectedDrop = append(expectedDrop, src)
				}
			}
			gotDrop := append([]int(nil), e.Drop...)
			sort.Ints(gotDrop)
			if !equalInts(gotDrop, expectedDrop) {
				return apperr.At(apperr.EPlan, entryNo, "drop", "declared drop channels %v do not match unused channels %v", gotDrop, expectedDrop)
			}
		}
		for _, d := range e.Drop {
			if d < 0 || d >= sourceChannels {
				return apperr.At(apperr.EPlan, entryNo, "drop", "drop channel %d outside source channel count %d", d, sourceChannels)
			}
		}
		labels := make(map[string]bool)
		for slot, src := range e.Permutation {
			label := ""
			if src >= 0 {
				label = string("RGBA"[src])
			} else {
				label = fillAt[slot].Channel
			}
			if labels[label] {
				return apperr.At(apperr.EPlan, entryNo, "permutation", "target channel label %q is duplicated", label)
			}
			labels[label] = true
		}
		sourceChannels = e.TargetChannels
	}
	return nil
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func Reindex(p *model.Plan) {
	for i := range p.Entries {
		p.Entries[i].Index = i + 1
	}
}

func FindEntry(p model.Plan, id string) (int, bool) {
	for i := range p.Entries {
		if p.Entries[i].ID == id {
			return i, true
		}
	}
	return -1, false
}
