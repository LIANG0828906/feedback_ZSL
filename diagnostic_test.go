package strokelab

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnosticArtifactsAndSampleLoader(t *testing.T) {
	suite, err := GenerateSuite(2323)
	if err != nil {
		t.Fatal(err)
	}
	sample := suite.Samples[2]
	root := t.TempDir()
	sampleDir := filepath.Join(root, "sample")
	if _, err := WriteSampleDir(sampleDir, sample); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSampleDir(sampleDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Project.ID != sample.Project.ID || loaded.Icon == nil {
		t.Fatalf("sample reload mismatch")
	}
	engine, _ := NewEngine(loaded.Project)
	eval, err := engine.Run(context.Background(), RunOptions{Mode: RunFull})
	if err != nil {
		t.Fatal(err)
	}
	weight := loaded.Project.Strokes[0].Weight - 400
	edit := Edit{StrokeID: loaded.Project.Strokes[0].ID, Weight: &weight}
	edited, _ := ApplyEdit(loaded.Project, edit)
	plan, _ := PlanInvalidation(loaded.Project, edited, edit)
	reportPath, report, err := WriteEvaluationDiagnostics(filepath.Join(root, "report"),
		loaded, eval, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report == nil || report.Topology == nil || !report.Topology.Passed {
		t.Fatalf("diagnostic topology missing")
	}
	for _, key := range []string{"candidate", "composite", "reference", "diff", "blocks"} {
		if _, ok := report.Assets[key]; !ok {
			t.Fatalf("missing asset key %s", key)
		}
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	for _, needle := range []string{"逐笔贡献", "连通拓扑", "失效传播", "检查点状态", "调度与排队明细"} {
		if !strings.Contains(html, needle) {
			t.Fatalf("report missing section %q", needle)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "report", "evaluation.json")); err != nil {
		t.Fatal(err)
	}
}

func TestCompareEvaluationsLocatesBlockAndStroke(t *testing.T) {
	suite, err := GenerateSuite(3434)
	if err != nil {
		t.Fatal(err)
	}
	engine, _ := NewEngine(suite.Samples[2].Project)
	base, err := engine.Run(context.Background(), RunOptions{Mode: RunFull})
	if err != nil {
		t.Fatal(err)
	}
	altered := cloneEval(base)
	blockIndex := -1
	for i := range altered.Blocks {
		if len(altered.Blocks[i].ComponentDigests) > 0 {
			blockIndex = i
			break
		}
	}
	if blockIndex < 0 {
		t.Fatal("no block has stroke components")
	}
	block := &altered.Blocks[blockIndex]
	block.Mask[0] ^= 0xff
	for strokeID := range block.ComponentDigests {
		block.ComponentDigests[strokeID] = "forced-mismatch"
		break
	}
	cmp := CompareEvaluations(base, altered)
	if cmp.Equal || len(cmp.Mismatches) == 0 {
		t.Fatalf("expected mismatch")
	}
	found := false
	for _, m := range cmp.Mismatches {
		if m.Kind == "block_mask" {
			if m.BlockID != block.BlockID || m.StrokeID == "" || m.StrokeOrdinal <= 0 {
				t.Fatalf("mismatch location incomplete: %+v", m)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("block mismatch not located: %+v", cmp.Mismatches)
	}
}

func cloneEval(in *Evaluation) *Evaluation {
	out := *in
	out.Composite = append([]uint8(nil), in.Composite...)
	out.ReusedBlocks = append([]int(nil), in.ReusedBlocks...)
	out.RecomputedBlocks = append([]int(nil), in.RecomputedBlocks...)
	out.Blocks = make([]BlockResult, len(in.Blocks))
	for i, b := range in.Blocks {
		out.Blocks[i] = b
		out.Blocks[i].Mask = append([]uint8(nil), b.Mask...)
		out.Blocks[i].ComponentDigests = make(map[string]string, len(b.ComponentDigests))
		for k, v := range b.ComponentDigests {
			out.Blocks[i].ComponentDigests[k] = v
		}
	}
	return &out
}
