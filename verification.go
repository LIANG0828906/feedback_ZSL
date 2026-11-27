package terrainbake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

type PathRun struct {
	Name           string      `json:"name"`
	CheckpointDir  string      `json:"checkpoint_dir"`
	Result         *BakeResult `json:"-"`
	Fingerprint    string      `json:"fingerprint"`
	ReusedCount    int         `json:"reused_count"`
	WrittenCount   int         `json:"written_count"`
	CommitSequence []string    `json:"commit_sequence"`
	EditsApplied   int         `json:"edits_applied"`
}

type PathComparison struct {
	Left  string `json:"left"`
	Right string `json:"right"`
	Equal bool   `json:"equal"`
	Error *Error `json:"error,omitempty"`
}

type ThreePathReport struct {
	Equal       bool             `json:"equal"`
	Paths       []PathRun        `json:"paths"`
	Comparisons []PathComparison `json:"comparisons"`
	Message     string           `json:"message"`
}

// VerifyThreePaths executes the same edits through:
//  1. draft incremental bakes,
//  2. checkpoint restart between edits (resume),
//  3. one full rebuild from an empty cache.
//
// It compares vertices, triangles, seam folds, attributions and the canonical
// commit sequence. Any mismatch returns E_DIFF at the first differing block.
func VerifyThreePaths(ctx context.Context, p *Project, cfg EngineConfig, edits []Edit, rootDir string) (*ThreePathReport, error) {
	if p == nil {
		return nil, errSchema("project", "nil project")
	}
	if rootDir == "" {
		dir, err := os.MkdirTemp("", "terrain-verify-*")
		if err != nil {
			return nil, &Error{Code: CodeBlock, Field: "workdir", Message: err.Error()}
		}
		rootDir = dir
	} else if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, &Error{Code: CodeBlock, Field: "workdir", Message: err.Error()}
	}
	report := &ThreePathReport{}

	draftDir := filepath.Join(rootDir, "draft")
	draft, err := NewEngine(p, cfg, draftDir)
	if err != nil {
		return nil, err
	}
	baseDraft, err := draft.Bake(ctx)
	if err != nil {
		return nil, err
	}
	draftResult := baseDraft
	for _, edit := range edits {
		if _, err := draft.ApplyEdit(edit); err != nil {
			return nil, err
		}
		draftResult, err = draft.Bake(ctx)
		if err != nil {
			return nil, err
		}
	}
	draftRun := makePathRun("draft_incremental", draftDir, draftResult, len(edits))

	resumeDir := filepath.Join(rootDir, "resume")
	resumeEngine, err := NewEngine(p, cfg, resumeDir)
	if err != nil {
		return nil, err
	}
	if _, err := resumeEngine.Bake(ctx); err != nil {
		return nil, err
	}
	var resumeResult *BakeResult
	for _, edit := range edits {
		// Reopen before every edit to prove that all invalidation-relevant
		// state is recovered from the atomic checkpoint.
		resumeEngine, err = NewEngine(p, cfg, resumeDir)
		if err != nil {
			return nil, err
		}
		if _, err := resumeEngine.ApplyEdit(edit); err != nil {
			return nil, err
		}
		resumeResult, err = resumeEngine.Bake(ctx)
		if err != nil {
			return nil, err
		}
	}
	if resumeResult == nil {
		resumeEngine, err = NewEngine(p, cfg, resumeDir)
		if err != nil {
			return nil, err
		}
		resumeResult, err = resumeEngine.Bake(ctx)
		if err != nil {
			return nil, err
		}
	}
	resumeRun := makePathRun("checkpoint_resume", resumeDir, resumeResult, len(edits))

	fullDir := filepath.Join(rootDir, "full")
	fullEngine, err := NewEngine(p, cfg, fullDir)
	if err != nil {
		return nil, err
	}
	for _, edit := range edits {
		if _, err := fullEngine.ApplyEdit(edit); err != nil {
			return nil, err
		}
	}
	fullResult, err := fullEngine.BakeFull(ctx)
	if err != nil {
		return nil, err
	}
	fullRun := makePathRun("full_rebuild", fullDir, fullResult, len(edits))

	report.Paths = []PathRun{draftRun, resumeRun, fullRun}
	pairs := [][2]int{{0, 1}, {1, 2}, {0, 2}}
	for _, pair := range pairs {
		left, right := report.Paths[pair[0]], report.Paths[pair[1]]
		cmp := PathComparison{Left: left.Name, Right: right.Name}
		if err := CompareBakeResults(left.Result, right.Result); err != nil {
			cmp.Error = asError(err)
			report.Comparisons = append(report.Comparisons, cmp)
			report.Equal = false
			report.Message = fmt.Sprintf("%s != %s", left.Name, right.Name)
			return report, cmp.Error
		}
		cmp.Equal = true
		report.Comparisons = append(report.Comparisons, cmp)
	}
	report.Equal = true
	report.Message = "draft incremental, checkpoint resume and full rebuild are item-by-item equal"
	return report, nil
}

func makePathRun(name, dir string, result *BakeResult, edits int) PathRun {
	run := PathRun{
		Name: name, CheckpointDir: dir, Result: result,
		ReusedCount: result.ReusedCount, WrittenCount: len(result.WrittenBlocks),
		CommitSequence: append([]string(nil), result.CommitSequence...),
		EditsApplied:   edits,
	}
	run.Fingerprint = BakeFingerprint(result)
	return run
}

func BakeFingerprint(result *BakeResult) string {
	if result == nil {
		return ""
	}
	type summary struct {
		Commit []string
		Blocks []struct {
			ID     string
			Digest string
			Seams  []SeamReport
		}
	}
	var s summary
	s.Commit = result.CommitSequence
	for _, id := range result.CommitSequence {
		m := result.Meshes[id]
		entry := struct {
			ID     string
			Digest string
			Seams  []SeamReport
		}{ID: id, Seams: seamsForBlock(result.Seams, id)}
		if m != nil {
			entry.Digest = DigestMesh(m)
		}
		s.Blocks = append(s.Blocks, entry)
	}
	return digestJSON(s)
}
