package pipeline

import (
	"fmt"
	"sort"

	"texturekernel"
)

type PathSnapshot struct {
	Name           string
	Final          *texture.Image
	StageHashes    []StageDigest
	CommitSequence []CommitRef
}

type CompareReport struct {
	Paths          []string      `json:"paths"`
	FinalHash      string        `json:"final_hash"`
	StageHashes    []StageDigest `json:"stage_hashes"`
	CommitSequence []CommitRef   `json:"commit_sequence"`
}

// ComparePaths requires every path to agree pairwise. No single path is
// designated as the reference: the first difference reports all observed values.
func ComparePaths(plan *texture.Plan, paths ...PathSnapshot) (*CompareReport, error) {
	if len(paths) < 3 {
		return nil, errf(texture.CodePara, "$.paths", "paths", "at least three paths are required")
	}
	if plan == nil {
		return nil, errf(texture.CodePara, "$.plan", "plan", "plan is required")
	}
	report := &CompareReport{Paths: make([]string, len(paths))}
	for i, path := range paths {
		report.Paths[i] = path.Name
		if path.Final == nil {
			return nil, diffError(&texture.DiffInfo{EntryIndex: -1, Field: "final", Reason: fmt.Sprintf("%s has no final image", path.Name)})
		}
	}
	base := paths[0]
	report.FinalHash = base.Final.Hash()
	for i := 1; i < len(paths); i++ {
		if base.Final.Width != paths[i].Final.Width || base.Final.Height != paths[i].Final.Height ||
			base.Final.BitDepth != paths[i].Final.BitDepth ||
			texture.LayoutString(base.Final.Layout) != texture.LayoutString(paths[i].Final.Layout) {
			return nil, diffError(&texture.DiffInfo{
				X: 0, Y: 0, EntryIndex: len(plan.Entries) - 1, Field: "image_metadata",
				Reason: "final image metadata differs",
				Values: map[string]string{
					paths[0].Name: fmt.Sprintf("%dx%d/%d/%s", base.Final.Width, base.Final.Height, base.Final.BitDepth, texture.LayoutString(base.Final.Layout)),
					paths[i].Name: fmt.Sprintf("%dx%d/%d/%s", paths[i].Final.Width, paths[i].Final.Height, paths[i].Final.BitDepth, texture.LayoutString(paths[i].Final.Layout)),
				},
			})
		}
	}

	for y := 0; y < base.Final.Height; y++ {
		for x := 0; x < base.Final.Width; x++ {
			for c, ch := range base.Final.Layout {
				expected := base.Final.At(x, y, c)
				allEqual := true
				values := make(map[string]string, len(paths))
				values[paths[0].Name] = fmt.Sprintf("%d", expected)
				for i := 1; i < len(paths); i++ {
					actual := paths[i].Final.At(x, y, c)
					values[paths[i].Name] = fmt.Sprintf("%d", actual)
					if actual != expected {
						allEqual = false
					}
				}
				if !allEqual {
					entry := entryForPixel(plan, x, y)
					return nil, diffError(&texture.DiffInfo{
						X: x, Y: y, EntryIndex: entry, Field: fmt.Sprintf("image.%s", ch),
						Reason: "final pixel differs between execution paths", Values: values,
					})
				}
			}
		}
	}
	firstEntryDiff := compareStageHashes(paths)
	if firstEntryDiff >= 0 {
		report.StageHashes = base.StageHashes
		return nil, diffError(&texture.DiffInfo{
			X: 0, Y: 0, EntryIndex: firstEntryDiff, Field: "stage_digest",
			Reason: "per-entry intermediate digests differ",
			Values: stageHashValues(paths, firstEntryDiff),
		})
	}
	if err := compareCommits(paths); err != nil {
		return nil, err
	}
	report.StageHashes = base.StageHashes
	report.CommitSequence = base.CommitSequence
	return report, nil
}

func compareStageHashes(paths []PathSnapshot) int {
	count := len(paths[0].StageHashes)
	for _, path := range paths[1:] {
		if len(path.StageHashes) < count {
			count = len(path.StageHashes)
		}
	}
	for i := 0; i < count; i++ {
		ref := paths[0].StageHashes[i]
		for _, path := range paths[1:] {
			if path.StageHashes[i].Digest != ref.Digest || path.StageHashes[i].EntryIndex != ref.EntryIndex {
				return ref.EntryIndex
			}
		}
	}
	for _, path := range paths {
		if len(path.StageHashes) != len(paths[0].StageHashes) {
			return maxInt(0, count-1)
		}
	}
	return -1
}

func stageHashValues(paths []PathSnapshot, entry int) map[string]string {
	out := make(map[string]string, len(paths))
	for _, path := range paths {
		if entry >= 0 && entry < len(path.StageHashes) {
			out[path.Name] = path.StageHashes[entry].Digest
		}
	}
	return out
}

func compareCommits(paths []PathSnapshot) error {
	ref := paths[0].CommitSequence
	for _, path := range paths[1:] {
		if len(path.CommitSequence) != len(ref) {
			return diffError(&texture.DiffInfo{EntryIndex: -1, Field: "commit_sequence", Reason: "commit count differs"})
		}
		for i := range ref {
			if ref[i] != path.CommitSequence[i] {
				return diffError(&texture.DiffInfo{
					EntryIndex: -1, Field: "commit_sequence",
					Reason: fmt.Sprintf("commit %d differs: %+v vs %+v", i, ref[i], path.CommitSequence[i]),
				})
			}
		}
	}
	return nil
}

func entryForPixel(plan *texture.Plan, x, y int) int {
	for i := len(plan.Entries) - 1; i >= 0; i-- {
		r := plan.Entries[i].Coverage
		if x >= r.X && x < r.Right() && y >= r.Y && y < r.Bottom() {
			return i
		}
	}
	return -1
}

func diffError(diff *texture.DiffInfo) *texture.KernelError {
	return &texture.KernelError{Code: texture.CodeDiff, Path: "$", Message: "execution paths differ", Diff: diff}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func SortCommitRefs(refs []CommitRef) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].CommitSeq < refs[j].CommitSeq })
}
